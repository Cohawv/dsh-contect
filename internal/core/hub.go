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

	// replayProvider 是「离线消息回放」钩子（V6 ① store 注入）。为 nil 时（未挂载
	// store 或已停用）重连不补发，行为退化为历史一致。
	muReplay        sync.Mutex
	replayProvider  ReplayProvider
	lastSeqProvider func() int64 // 当前已持久化的最大 seq（store 注入；nil 时为 0）

	// deliveryTracker 是「投递确认与对账」钩子（V6 ② ack 注入）。为 nil 时（未挂载
	// ack 或已停用）TypeAck 退回 unknown type 语义，行为退化为历史一致。
	muAck          sync.Mutex
	deliveryTracker DeliveryTracker
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

	// V5：把总线注入两个发布方，使 room.changed / msg.routed 有真实来源。
	rooms.SetBus(bus)
	router.SetBus(bus)

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

// SetReplayProvider 注入「离线消息回放」钩子（由 store 插件在 Init 时调用）。
// 传 nil 即卸载（store 被运行期停用时调用）。仅由装配期 / 插件启停调用。
func (h *Hub) SetReplayProvider(f ReplayProvider) {
	h.muReplay.Lock()
	h.replayProvider = f
	h.muReplay.Unlock()
}

// replayProviderLocked 读取当前回放钩子（nil 安全）。
func (h *Hub) replayProviderLocked() ReplayProvider {
	h.muReplay.Lock()
	f := h.replayProvider
	h.muReplay.Unlock()
	return f
}

// SetDeliveryTracker 注入「投递确认与对账」钩子（由 ack 插件在 Init 时调用）。
// 传 nil 即卸载（ack 被运行期停用时调用）。仅由装配期 / 插件启停调用。
func (h *Hub) SetDeliveryTracker(t DeliveryTracker) {
	h.muAck.Lock()
	h.deliveryTracker = t
	h.muAck.Unlock()
}

// deliveryTrackerLocked 读取当前投递追踪钩子（nil 安全）。
func (h *Hub) deliveryTrackerLocked() DeliveryTracker {
	h.muAck.Lock()
	t := h.deliveryTracker
	h.muAck.Unlock()
	return t
}

// SetLastSeqProvider 注入「当前已持久化最大 seq」读取器（由 store 插件在 Init 时调用）。
// 传 nil 即卸载。
func (h *Hub) SetLastSeqProvider(f func() int64) {
	h.muReplay.Lock()
	h.lastSeqProvider = f
	h.muReplay.Unlock()
}

// LastSeq 返回当前已持久化的消息序号上限（无 store 时为 0）。
func (h *Hub) LastSeq() int64 {
	h.muReplay.Lock()
	defer h.muReplay.Unlock()
	if h.lastSeqProvider != nil {
		return h.lastSeqProvider()
	}
	return 0
}

// HandleEnvelope 处理一条来自 sess 的信封（纯业务分发，不感知传输与编码）。
func (h *Hub) HandleEnvelope(sess Session, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeRegister:
		var p struct {
			Name     string   `json:"name"`
			Caps     []string `json:"caps"`
			SinceSeq int64    `json:"sinceSeq"` // V6 ①：声明 replay cap 的客户端携带，用于离线消息回放
		}
		_ = protocol.UnmarshalContent(env.Content, &p)
		// 版本协商：服务端迁就客户端
		proto := protocol.Negotiate(env.V)
		a := h.Registry.Bind(env.From, p.Name, p.Caps, proto, sess)
		// V6 ①：声明 replay cap 的客户端，注册回执带 lastSeq（当前已持久化的最大 seq）。
		// 未声明的客户端拿不到该字段，行为退化为历史一致。
		var lastSeq int64
		if protocol.HasCap(p.Caps, protocol.CapReplay) {
			lastSeq = h.LastSeq()
		}
		a.Deliver(protocol.RegisteredAck(env.From, p.Caps, proto, h.ackCfg(), lastSeq))
		h.Bus.Publish(protocol.TopicAgentOnline, env.From)

		// V6 ①：声明 replay cap 且携带 sinceSeq 的客户端，补发离线期间（seq > sinceSeq）
		// 与其相关的消息。回放在 ack 之后异步进行（直接复用当前会话下发）。
		if protocol.HasCap(p.Caps, protocol.CapReplay) && p.SinceSeq > 0 {
			if replay := h.replayProviderLocked(); replay != nil {
				replay(sess, env.From, p.SinceSeq)
			}
		}

	case protocol.TypeHeartbeat:
		h.Registry.Touch(env.From)

	case protocol.TypeChat:
		// 点对点：目标离线时给发送方回执（与历史行为一致）
		if missed := h.Router.Route(*env); missed != "" {
			h.notifyUndelivered(env.From, missed)
			if t := h.deliveryTrackerLocked(); t != nil {
				t.Track(env.MsgID, false, missed)
			}
		} else if t := h.deliveryTrackerLocked(); t != nil {
			t.Track(env.MsgID, true, env.To)
		}
	case protocol.TypeRoom, protocol.TypeBroadcast:
		h.Router.Route(*env)
		if t := h.deliveryTrackerLocked(); t != nil {
			t.Track(env.MsgID, true, "")
		}

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

	case protocol.TypeAck:
		// V6 ②：投递确认回执。仅当挂载了 ack 插件（deliveryTracker 非 nil）时才识别——
		// 未挂载时退回 default 的 unknown type 语义，保证「无 ack 插件的部署」与历史完全一致。
		if t := h.deliveryTrackerLocked(); t != nil {
			var p struct {
				MsgID  string `json:"msgId"`
				Status string `json:"status"`
			}
			_ = protocol.UnmarshalContent(env.Content, &p)
			t.Ack(p.MsgID, p.Status, env.From)
			sess.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", env.From, "",
				map[string]any{"action": protocol.ActionAckReceived, "msgId": p.MsgID}))
		} else {
			h.unknownType(sess, env)
		}

	default:
		h.unknownType(sess, env)
	}
}

// unknownType 对不认识的类型回一条 unknown_type 系统回执（旧契约，长期保留）。
func (h *Hub) unknownType(sess Session, env *protocol.Envelope) {
	sess.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", env.From, "", map[string]any{
		"text":   "unknown type: " + env.Type, // 旧契约，保留
		"action": protocol.ActionError,
		"code":   protocol.CodeUnknownType,
	}))
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
