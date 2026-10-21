package core

import "lan-agent-hub/internal/protocol"

// Presence 订阅上下线事件，并向所有在线 Agent 广播 presence 信封。
//
// 它只依赖 Registry 与 Router，不关心是谁发布的事件 —— 新增「房间变更通知」这类
// 需求时，只需再加一个订阅者，Registry 与 Router 都不用改。
type Presence struct {
	reg    *Registry
	router *Router
}

func NewPresence(reg *Registry, router *Router) *Presence {
	return &Presence{reg: reg, router: router}
}

// Broadcast 广播上下线事件。
// 使用 BroadcastRaw：不分配 seq —— 与历史行为一致（presence 的 seq 恒为 0）。
func (p *Presence) Broadcast(status, id string) {
	env := protocol.NewEnvelope(protocol.TypePresence, "hub", "", "",
		map[string]string{"id": id, "status": status})
	p.router.BroadcastRaw(env)
}
