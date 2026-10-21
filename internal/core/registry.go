package core

import (
	"sync"
	"time"

	"lan-agent-hub/internal/protocol"
)

// Agent 是在线 Agent 的领域对象：只保存业务信息，不含任何传输细节。
// 发送能力通过 Sess（Session 接口）持有，实现由 adapter 注入。
type Agent struct {
	ID       string
	Name     string
	Caps     []string // 客户端声明的能力集
	Proto    int      // 协商后的协议版本
	LastSeen int64    // 最近心跳时间（毫秒）
	Sess     Session
}

// Deliver 转发到实际会话；无会话时静默丢弃。
func (a *Agent) Deliver(env protocol.Envelope) {
	if a == nil || a.Sess == nil {
		return
	}
	a.Sess.Deliver(env)
}

// Registry 维护 agentId → Agent 的在线表。
type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
}

func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent)}
}

// Bind 登记（或覆盖）一个在线 Agent，返回其领域对象。
func (r *Registry) Bind(id, name string, caps []string, proto int, s Session) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := &Agent{
		ID:       id,
		Name:     name,
		Caps:     caps,
		Proto:    proto,
		LastSeen: time.Now().UnixMilli(),
		Sess:     s,
	}
	r.agents[id] = a
	return a
}

func (r *Registry) Unbind(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agents, id)
}

func (r *Registry) Get(id string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[id]
	return a, ok
}

func (r *Registry) All() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out
}

// Touch 刷新心跳时间。
func (r *Registry) Touch(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.agents[id]; ok {
		a.LastSeen = time.Now().UnixMilli()
	}
}

// Sweep 清理心跳超时的 Agent，返回被清理者（供上层发布下线事件）。
func (r *Registry) Sweep(timeoutMs int64) []*Agent {
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	var dropped []*Agent
	for id, a := range r.agents {
		if now-a.LastSeen > timeoutMs {
			delete(r.agents, id)
			dropped = append(dropped, a)
		}
	}
	return dropped
}

// ListAgents 输出在线列表（对外契约的一部分：id/name 恒定，caps 为增量字段）。
func (r *Registry) ListAgents() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.agents))
	for _, a := range r.agents {
		item := map[string]any{"id": a.ID, "name": a.Name}
		if len(a.Caps) > 0 {
			item["caps"] = a.Caps
		}
		out = append(out, item)
	}
	return out
}
