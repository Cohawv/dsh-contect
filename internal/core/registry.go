package core

import (
	"sort"
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

// BindReplace 同 Bind，但同时返回被它顶掉的旧 Agent（没有则 nil）。
//
// 同 id 重连时旧连接不会自己消失：它要等到读循环读到 EOF/错误才退出，期间
// 仍在占用 socket 与写协程。调用方拿到旧 Agent 后应主动关掉它的会话，
// 否则上下线风暴下会堆积大批「已不在表里、却还活着」的僵尸连接。
func (r *Registry) BindReplace(id, name string, caps []string, proto int, s Session) (*Agent, *Agent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.agents[id]
	if prev != nil && prev.Sess == s {
		// 同一会话重复注册（重连同一 socket、或客户端重发 register）：不当作替换，
		// 否则调用方会把自己的连接关掉。
		prev.Name, prev.Caps, prev.Proto = name, caps, proto
		prev.LastSeen = time.Now().UnixMilli()
		return prev, nil
	}
	a := &Agent{
		ID:       id,
		Name:     name,
		Caps:     caps,
		Proto:    proto,
		LastSeen: time.Now().UnixMilli(),
		Sess:     s,
	}
	r.agents[id] = a
	return a, prev
}

// Unbind 强制按 id 摘除（管理面意图：不管现在是谁，都要下线）。
func (r *Registry) Unbind(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agents, id)
}

// UnbindSession 只在「当前登记的会话正是 sess」时摘除，返回是否真的摘除。
//
// 为什么必须校验会话身份：同 id 重连时，新会话先被 Bind 写进表，旧会话的读循环
// 随后读到 EOF 调到这里。若按 id 无条件删除，**旧会话会把新会话摘掉**——
// 连接还活着，却再收不到任何消息，房间成员身份也一并丢失，且会误发一条
// agent.offline。压测里表现为「连接全成功、延迟很低、但一部分消息凭空消失」。
func (r *Registry) UnbindSession(id string, sess Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return false
	}
	if sess != nil && a.Sess != sess {
		return false // 表里已经是别人的会话了，旧会话无权摘除
	}
	delete(r.agents, id)
	return true
}

// Get 返回在线 Agent 的快照副本。
// 值拷贝 + Caps 切片深拷贝一层，因此调用方持有/修改副本不会影响在线表。
// 注意：Sess 字段保留在副本中——core 内部（notifyUndelivered、Router 的 DeliverTo/
// BroadcastAll）仍依赖它来实际下发；对外暴露由 plugin.AgentInfo（不含 Sess）负责。
func (r *Registry) Get(id string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[id]
	if !ok {
		return nil, false
	}
	return copyAgent(a), true
}

// All 返回所有在线 Agent 的副本切片，按 ID 升序排列（稳定，便于断言与控制台展示）。
// 每个副本同样深拷贝 Caps，避免调用方改动影响在线表。
func (r *Registry) All() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, copyAgent(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// copyAgent 深拷贝一个 Agent（值拷贝 + Caps 切片一层深拷贝）。
// Sess 接口值原样保留（接口内部引用不变），因此副本仍可 Deliver。
func copyAgent(a *Agent) *Agent {
	cp := *a
	cp.Caps = append([]string(nil), a.Caps...)
	return &cp
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

// ListAgents 输出在线列表（默认：仅 id/name，最轻量，老客户端友好）。
func (r *Registry) ListAgents() []map[string]any {
	return r.listAgents(false)
}

// ListAgentsDetailed 输出带 caps/proto 的明细列表（受 cap: agent-caps 门控）。
func (r *Registry) ListAgentsDetailed() []map[string]any {
	return r.listAgents(true)
}

// listAgents 按 detailed 决定是否下发每个 agent 的 caps/proto 等增量字段。
// 增量字段对不声明对应 cap 的客户端无意义，下发反而是噪声，故默认不下发。
func (r *Registry) listAgents(detailed bool) []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.agents))
	for _, a := range r.agents {
		item := map[string]any{"id": a.ID, "name": a.Name}
		if detailed {
			if a.LastSeen > 0 {
				item["lastSeen"] = a.LastSeen
			}
			if len(a.Caps) > 0 {
				item["caps"] = a.Caps
			}
			if a.Proto > 0 {
				item["proto"] = a.Proto
			}
		}
		out = append(out, item)
	}
	return out
}
