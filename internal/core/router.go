package core

import (
	"sync"
	"sync/atomic"

	"lan-agent-hub/internal/protocol"
)

// 消息路由 kind 取值（MsgRouted.Kind）。
const (
	RouteKindDirect    = "direct"
	RouteKindRoom      = "room"
	RouteKindBroadcast = "broadcast"
)

// MsgRouted 是 protocol.TopicMsgRouted 的事件载荷。
//
// 刻意只放「计数 + 分类 + seq」：消息速率统计只需要这些，
// 把整个信封塞进事件会让每个订阅者都拿到一份可变的业务数据（也更容易泄漏内容）。
type MsgRouted struct {
	Kind      string `json:"kind"`             // direct | room | broadcast
	RoomID    string `json:"roomId,omitempty"` // 仅 room 场景
	Delivered int    `json:"delivered"`        // 本次实际投递数（目标离线为 0）
	Seq       int64  `json:"seq"`              // 服务端分配的全局序号
}

// Router 负责按目标转发信封：点对点 / 房间 / 广播，并分配全局单调 seq。
//
// 注意：只有经 Route 的消息才有 seq；系统回执与 presence 事件不分配 seq（保持历史行为）。
//
// V5：Route 结束后发布 protocol.TopicMsgRouted（消息速率统计用）。
// 该发布点位于消息热路径上，因此依赖 EventBus.Publish 的「无订阅者快速返回」，
// 无订阅者时的额外成本仅为一次 RLock + map 查询（见 TestMsgRoutedHotPathCost）。
type Router struct {
	seq   int64
	reg   *Registry
	rooms *RoomService

	// bus 为 nil 时不发布（单测直连场景），行为与 V4 一致。
	bus *EventBus

	// sink 是「消息落盘」钩子（V6 ① store 注入）。为 nil 时（未挂载 store 或已停用）
	// 静默跳过，行为与历史完全一致。热路径只做一次指针读取，不阻塞。
	mu   sync.Mutex
	sink MessageSink
}

func NewRouter(reg *Registry, rooms *RoomService) *Router {
	return &Router{reg: reg, rooms: rooms}
}

// SetBus 注入事件总线。仅由 NewHub 在装配期调用一次（此后只读，无需加锁）。
func (rt *Router) SetBus(bus *EventBus) {
	rt.bus = bus
}

// SetMessageSink 注入「消息落盘」钩子（由 store 插件在 Init 时调用）。
// 传 nil 即卸载（store 被运行期停用时调用，使持久化立即停掉，行为与无 store 一致）。
// 仅由装配期 / 插件启停调用，热路径 Route 只读取，不在此加锁竞争。
func (rt *Router) SetMessageSink(sink MessageSink) {
	rt.mu.Lock()
	rt.sink = sink
	rt.mu.Unlock()
}

// messageSink 读取当前钩子（热路径用，nil 安全）。
func (rt *Router) messageSink() MessageSink {
	rt.mu.Lock()
	s := rt.sink
	rt.mu.Unlock()
	return s
}

// Route 分配 seq 后按目标转发。
// 返回值：点对点场景下若目标离线，返回该目标 ID；其余场景返回空串。
func (rt *Router) Route(env protocol.Envelope) string {
	if env.MsgID == "" {
		env.MsgID = protocol.UUID()
	}
	env.Seq = atomic.AddInt64(&rt.seq, 1)
	env.TS = nowMillis()

	kind := RouteKindBroadcast
	delivered := 0
	missed := ""
	switch {
	case env.RoomID != "":
		kind = RouteKindRoom
		delivered = rt.BroadcastRoom(env)
	case env.To != "":
		kind = RouteKindDirect
		if rt.DeliverTo(env.To, env) {
			delivered = 1
		} else {
			missed = env.To
		}
	default:
		delivered = rt.BroadcastAll(env)
	}

	rt.publishRouted(MsgRouted{
		Kind:      kind,
		RoomID:    env.RoomID,
		Delivered: delivered,
		Seq:       env.Seq,
	})

	// V6 ①：把已路由完成的信封交给消息落盘钩子（store）。
	// 必须在 seq / ts 分配之后调用，且调用方拿到的 env 是值拷贝（含 Seq / TS），
	// store 不会反向修改路由态。钩子为 nil 时静默跳过——热路径仅一次指针读取。
	if s := rt.messageSink(); s != nil {
		s.Append(env)
	}
	return missed
}

// publishRouted 发布一条 msg.routed 事件（bus 为 nil 时静默）。
func (rt *Router) publishRouted(ev MsgRouted) {
	if rt.bus == nil {
		return
	}
	rt.bus.Publish(protocol.TopicMsgRouted, ev)
}

// DeliverTo 点对点投递，返回是否送达（false 表示目标不在线）。
func (rt *Router) DeliverTo(id string, env protocol.Envelope) bool {
	a, ok := rt.reg.Get(id)
	if !ok || a == nil {
		return false
	}
	a.Deliver(env)
	return true
}

// BroadcastRoom 向房间成员投递，返回实际投递数。
func (rt *Router) BroadcastRoom(env protocol.Envelope) int {
	n := 0
	for _, m := range rt.rooms.Members(env.RoomID) {
		if rt.DeliverTo(m, env) {
			n++
		}
	}
	return n
}

// BroadcastAll 向所有在线 Agent 投递，返回实际投递数。
func (rt *Router) BroadcastAll(env protocol.Envelope) int {
	all := rt.reg.All()
	for _, a := range all {
		a.Deliver(env)
	}
	return len(all)
}

// BroadcastRaw 广播一条不分配 seq 的信封（用于 presence 等系统事件）。
func (rt *Router) BroadcastRaw(env protocol.Envelope) {
	for _, a := range rt.reg.All() {
		a.Deliver(env)
	}
}
