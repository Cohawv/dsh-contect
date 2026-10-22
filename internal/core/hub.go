package core

import (
	"context"
	"sync"
	"time"

	"lan-agent-hub/internal/protocol"
)

// Hub 是业务内核门面：装配各 service，并对接入层暴露 HandleEnvelope。
//
// 接入层（adapter）只需要：拿到一帧 → 调 HandleEnvelope → 用 Session 下发。
// 它不知道房间怎么管、seq 怎么分配、presence 怎么广播。
type Hub struct {
	Registry    *Registry
	Rooms       *RoomService
	Router      *Router
	PresenceSvc *Presence
	Bus         *EventBus

	// 保活参数，注册时通过能力协商下发给客户端
	HeartbeatMs int64
	TimeoutMs   int64
	// 服务端暴露的端口，注册回执在 cap: endpoints 下发给客户端（支持自动发现后的二次确认）
	TCPPort  int
	HTTPPort int

	// 清扫观测字段（V3 新增，供 V5 metrics 使用）。
	muSweep     sync.RWMutex
	LastSweepAt int64 // 上次 SweepLoop 实际执行清理的时间戳（毫秒）
}

func NewHub() *Hub {
	reg := NewRegistry()
	rooms := NewRoomService()
	router := NewRouter(reg, rooms)
	bus := NewEventBus()
	pres := NewPresence(reg, router)

	// 事件接线：上下线事件 → presence 广播。发布方无需知道谁在消费。
	bus.Subscribe(protocol.TopicAgentOnline, func(_ string, payload any) {
		if id, ok := payload.(string); ok {
			pres.Broadcast("online", id)
		}
	})
	bus.Subscribe(protocol.TopicAgentOffline, func(_ string, payload any) {
		if id, ok := payload.(string); ok {
			pres.Broadcast("offline", id)
		}
	})

	return &Hub{
		Registry:    reg,
		Rooms:       rooms,
		Router:      router,
		PresenceSvc: pres,
		Bus:         bus,
		HeartbeatMs: protocol.DefaultHeartbeatMs,
		TimeoutMs:   protocol.DefaultTimeoutMs,
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// HandleEnvelope 处理一条来自 sess 的信封（纯业务分发，不感知传输与编码）。
func (h *Hub) HandleEnvelope(sess Session, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeRegister:
		var p struct {
			Name string   `json:"name"`
			Caps []string `json:"caps"`
		}
		_ = protocol.UnmarshalContent(env.Content, &p)
		// 版本协商：服务端迁就客户端
		proto := protocol.Negotiate(env.V)
		a := h.Registry.Bind(env.From, p.Name, p.Caps, proto, sess)
		a.Deliver(protocol.RegisteredAck(env.From, p.Caps, proto, h.ackCfg()))
		h.Bus.Publish(protocol.TopicAgentOnline, env.From)

	case protocol.TypeHeartbeat:
		h.Registry.Touch(env.From)

	case protocol.TypeChat:
		// 点对点：目标离线时给发送方回执（与历史行为一致）
		if missed := h.Router.Route(*env); missed != "" {
			h.notifyUndelivered(env.From, missed)
		}
	case protocol.TypeRoom, protocol.TypeBroadcast:
		h.Router.Route(*env)

	case protocol.TypeCreateRoom:
		var p struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}
		_ = protocol.UnmarshalContent(env.Content, &p)
		id := h.Rooms.Create(p.Name, p.Members)
		sess.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", env.From, "",
			map[string]any{"action": protocol.ActionCreated, "roomId": id, "name": p.Name}))

	case protocol.TypeJoin:
		var p struct {
			RoomID string `json:"roomId"`
		}
		_ = protocol.UnmarshalContent(env.Content, &p)
		if p.RoomID != "" {
			h.Rooms.Join(p.RoomID, env.From)
		}

	case protocol.TypeLeave:
		var p struct {
			RoomID string `json:"roomId"`
		}
		_ = protocol.UnmarshalContent(env.Content, &p)
		if p.RoomID != "" {
			h.Rooms.Leave(p.RoomID, env.From)
		}

	case protocol.TypeList:
		var lp struct {
			Caps []string `json:"caps"`
		}
		_ = protocol.UnmarshalContent(env.Content, &lp)
		sess.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", env.From, "",
			map[string]any{"action": protocol.ActionList, "agents": h.ListAgents(lp.Caps)}))

	default:
		sess.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", env.From, "", map[string]any{
			"text":   "unknown type: " + env.Type, // 旧契约，保留
			"action": protocol.ActionError,
			"code":   protocol.CodeUnknownType,
		}))
	}
}

// notifyUndelivered 给发送方回一条未送达回执。
func (h *Hub) notifyUndelivered(from, target string) {
	if sender, ok := h.Registry.Get(from); ok {
		sender.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", from, "", map[string]any{
			"text":   "target offline: " + target, // 旧契约，保留
			"action": protocol.ActionUndelivered,
			"code":   protocol.CodeTargetOffline,
			"target": target,
		}))
	}
}

// Unregister 处理连接断开：摘除在线记录、退出所有房间、发布下线事件。
func (h *Hub) Unregister(id string) {
	if id == "" {
		return
	}
	h.Registry.Unbind(id)
	h.Rooms.Drop(id)
	h.Bus.Publish(protocol.TopicAgentOffline, id)
}

// SweepLoop 定期清理心跳超时的连接，可被外部通过 ctx 取消（V3 插件化开的可控口子）。
// 调用方（sweeper 插件）应在 Stop 时 cancel 并等待本循环退出。
func (h *Hub) SweepLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepOnce()
		}
	}
}

// sweepOnce 执行一次超时清理（抽出便于单测；core 的最小增量，不违反 C1）。
func (h *Hub) sweepOnce() {
	h.muSweep.Lock()
	h.LastSweepAt = time.Now().UnixMilli()
	h.muSweep.Unlock()
	for _, a := range h.Registry.Sweep(h.TimeoutMs) {
		h.Rooms.Drop(a.ID)
		h.Bus.Publish(protocol.TopicAgentOffline, a.ID)
		if a.Sess != nil {
			go func(s Session) { _ = s.Close() }(a.Sess)
		}
	}
}

// ListAgents 返回在线列表。是否携带各 agent 的 caps/proto 由请求者声明的
// cap: agent-caps 门控——不声明的客户端拿到的就是最轻量的 id/name 列表。
func (h *Hub) ListAgents(viewerCaps []string) []map[string]any {
	if protocol.CapSet(viewerCaps).Has(protocol.CapAgentCaps) {
		return h.Registry.ListAgentsDetailed()
	}
	return h.Registry.ListAgents()
}

// ListAgentsDetailed 总是返回带 caps/proto 的明细（HTTP 调试网关用）。
func (h *Hub) ListAgentsDetailed() []map[string]any {
	return h.Registry.ListAgentsDetailed()
}

// ackCfg 拼装注册回执所需的端点 / 保活配置。
func (h *Hub) ackCfg() protocol.EndpointsCfg {
	return protocol.EndpointsCfg{
		HeartbeatMs: h.HeartbeatMs,
		TimeoutMs:   h.TimeoutMs,
		TCPPort:     h.TCPPort,
		HTTPPort:    h.HTTPPort,
	}
}
