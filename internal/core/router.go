package core

import (
	"sync/atomic"

	"lan-agent-hub/internal/protocol"
)

// Router 负责按目标转发信封：点对点 / 房间 / 广播，并分配全局单调 seq。
//
// 注意：只有经 Route 的消息才有 seq；系统回执与 presence 事件不分配 seq（保持历史行为）。
type Router struct {
	seq   int64
	reg   *Registry
	rooms *RoomService
}

func NewRouter(reg *Registry, rooms *RoomService) *Router {
	return &Router{reg: reg, rooms: rooms}
}

// Route 分配 seq 后按目标转发。
// 返回值：点对点场景下若目标离线，返回该目标 ID；其余场景返回空串。
func (rt *Router) Route(env protocol.Envelope) string {
	if env.MsgID == "" {
		env.MsgID = protocol.UUID()
	}
	env.Seq = atomic.AddInt64(&rt.seq, 1)
	env.TS = nowMillis()
	switch {
	case env.RoomID != "":
		rt.BroadcastRoom(env)
	case env.To != "":
		if !rt.DeliverTo(env.To, env) {
			return env.To
		}
	default:
		rt.BroadcastAll(env)
	}
	return ""
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

func (rt *Router) BroadcastRoom(env protocol.Envelope) {
	for _, m := range rt.rooms.Members(env.RoomID) {
		rt.DeliverTo(m, env)
	}
}

func (rt *Router) BroadcastAll(env protocol.Envelope) {
	for _, a := range rt.reg.All() {
		a.Deliver(env)
	}
}

// BroadcastRaw 广播一条不分配 seq 的信封（用于 presence 等系统事件）。
func (rt *Router) BroadcastRaw(env protocol.Envelope) {
	for _, a := range rt.reg.All() {
		a.Deliver(env)
	}
}
