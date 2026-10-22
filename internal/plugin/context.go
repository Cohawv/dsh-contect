package plugin

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"lan-agent-hub/internal/protocol"
)

// HubView 是插件看到的「只读内核视图」。
// 它只暴露快照与计数，不提供任何写入/状态变更能力——插件无法借此改动在线表或路由。
type HubView interface {
	Agents() []AgentInfo // 在线 Agent 快照（纯值类型，不含 Sess）
	Rooms() []RoomInfo   // 房间快照（纯值类型）
	Stats() HubStats     // 计数类统计
}

// Publisher 是插件看到的「发布能力」。
// 插件只能发消息，不能直接触碰任何 Agent 的会话句柄（Sess 被排除在类型之外）。
// 需要投递到特定 Agent 的插件走 Send，由内核分配 seq 与路由。
type Publisher interface {
	Send(env protocol.Envelope) error
	Broadcast(from, text string) error
}

// AgentInfo 是在线 Agent 的纯值快照，与 /api/agents 的明细响应字段一一对应。
// 刻意不含 Sess 字段——从类型上杜绝会话句柄外泄给插件。
type AgentInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Caps     []string `json:"caps,omitempty"`
	Proto    int      `json:"proto,omitempty"`
	LastSeen int64    `json:"lastSeen,omitempty"`
}

// RoomInfo 是房间的纯值快照，与 /api/rooms 响应字段一一对应。
// 同样不含任何内部引用。
type RoomInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	MemberCount int      `json:"memberCount"`
	Members     []string `json:"members"`
}

// HubStats 是内核计数类统计，供控制台/插件读取。
type HubStats struct {
	AgentCount   int `json:"agentCount"`
	RoomCount    int `json:"roomCount"`
	ProtoVersion int `json:"protoVersion"`
}

// Context 是插件在 Init/Start 期间能拿到的全部上下文。
//
// V3 扩展点：共享 *http.ServeMux + 影子路由表（冲突预检）+ 跨插件共享 KV + 内核实体。
// V4 扩展点：路由闸门（gate），使运行期启停能立即改变端点可达性，而无需注销路由。
//
// 关键安全点：本包（框架）不得 import internal/core——Context 只以 any 持有 hub，
// 真正取出 *core.Hub 的逻辑在插件侧，框架本身永远看不到该具体类型。
type Context struct {
	view    HubView
	pub     Publisher
	options map[string]any // 本插件的合并后配置项
	mux     *http.ServeMux // 共享 mux：所有 HTTP 插件注册到它
	routes  *routeTable    // 影子路由表 pattern → 归属插件（冲突预检 + 归属查询）
	shared  *sync.Map      // 跨插件共享 KV：key(string) → any
	hub     any            // 业务内核实体（*core.Hub），框架侧为 any
	owner   string         // 当前插件名（用于冲突报错点名）
	gate    *gate          // 本插件的路由闸门（运行期停用后其端点立即 404）
	// sink 是框架的事件发布出口（BuildContext 后只读）。
	// 为 nil 时（单测直连）观测类事件静默丢弃，路由本身照常工作。
	sink EventSink
}

// newContext 构造 Context；view/pub 为 nil 时退化为空实现；
// routes/gate 为 nil 时按「无闸门、无路由表」降级（单测直连场景）。
func newContext(view HubView, pub Publisher, options map[string]any, owner string,
	mux *http.ServeMux, routes *routeTable, shared *sync.Map, hub any, g *gate) *Context {
	if view == nil {
		view = emptyView{}
	}
	if pub == nil {
		pub = emptyPublisher{}
	}
	if options == nil {
		options = map[string]any{}
	}
	return &Context{
		view: view, pub: pub, options: options, owner: owner,
		mux: mux, routes: routes, shared: shared, hub: hub, gate: g,
	}
}

// View 返回只读内核视图。
func (c *Context) View() HubView { return c.view }

// Publish 返回发布能力。
func (c *Context) Publish() Publisher { return c.pub }

// Hub 返回业务内核实体（注入时为 *core.Hub）。
// 框架侧以 any 持有，内置插件（在 internal/plugins/* 中）自行类型断言。
func (c *Context) Hub() any { return c.hub }

// ServeMux 返回插件共享的 *http.ServeMux（所有 HTTP 端点注册到此）。
func (c *Context) ServeMux() *http.ServeMux { return c.mux }

// Owner 返回当前插件的名字（用于日志与错误点名）。
func (c *Context) Owner() string { return c.owner }

// RegisterHTTP 把 pattern 注册到共享 mux，并做冲突预检 + 挂上运行期启停闸门。
//
// 规则：
//  1. 若 pattern 已被**别的**插件占用 → 返回 error 并点名冲突双方（不依赖 ServeMux 的 panic）；
//  2. 若 pattern 已被**自己**占用（运行期停用后重新 Init）→ 仅刷新影子表记录，不重复注册
//     （ServeMux 无法重复注册同一 pattern，否则 panic）；
//  3. 新注册的 handler 会套一层闸门：插件被停用/失败时直接 404，放行时行为与原 handler 完全一致；
//  4. ServeMux 仍可能因为**绕过影子表**的占位（例如别处直接调用 ctx.ServeMux().HandleFunc）
//     而 panic：此时 recover 把错误写回命名返回值，并且**不**在影子表里记账，
//     避免留下「Routes() 报告了一条 mux 上并不存在的路由」这种幽灵记录。
//
// 注意：必须使用命名返回值 err，否则 defer 里的 recover 只能吞掉 panic 而无法回传错误，
// 调用方会误以为注册成功（历史上正是这个写法导致 panic 被静默）。
//
// V5：注册成功的路由默认参与访问观测（发布 protocol.TopicHTTPRequest，供 metrics 计数）。
// 探针类端点用 RegisterHTTPUnobserved 注册，避免高频探针把请求计数刷成噪声。
func (c *Context) RegisterHTTP(pattern string, h http.HandlerFunc) (err error) {
	return c.register(pattern, h, true)
}

// RegisterHTTPUnobserved 注册一条**不参与访问观测**的路由（探针 /healthz、/readyz 等）。
//
// 语义与 RegisterHTTP 完全一致（冲突预检、归属记账、启停闸门），
// 唯一区别是请求进来时不发布 http.request 事件——
// 让 /metrics 里的 hub_http_requests_total 反映真实业务流量，而不是监控自身的噪声。
func (c *Context) RegisterHTTPUnobserved(pattern string, h http.HandlerFunc) (err error) {
	return c.register(pattern, h, false)
}

// register 是 RegisterHTTP / RegisterHTTPUnobserved 的共同实现。
func (c *Context) register(pattern string, h http.HandlerFunc, observed bool) (err error) {
	if c.routes == nil || c.mux == nil {
		return fmt.Errorf("plugin %q: context has no shared mux (BuildContext not called?)", c.owner)
	}
	if h == nil {
		return fmt.Errorf("plugin %q: nil handler for route %q", c.owner, pattern)
	}
	entry := &routeEntry{owner: c.owner, pattern: pattern, gate: c.gate}
	if prev, dup := c.routes.get(pattern); dup {
		if prev.owner != c.owner {
			return fmt.Errorf("route %q already registered by plugin %q (conflicts with %q)", pattern, prev.owner, c.owner)
		}
		// 同插件重复注册：运行期「停用 → 启用」会重新走一遍 Init，
		// ServeMux 不允许重复 Handle 同一 pattern，因此只刷新归属记录。
		c.routes.set(pattern, entry)
		return nil
	}

	// 兜底 ServeMux 的意外 panic，转为 Init 错误由 Manager 隔离。
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin %q: RegisterHTTP(%q) panicked: %v", c.owner, pattern, r)
		}
	}()

	g := c.gate
	sink := c.sink
	c.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if g != nil && !g.enabled() {
			http.NotFound(w, r)
			return
		}
		// 观测在 handler 之前发布：不包装 ResponseWriter，因此 SSE 的
		// http.Flusher 断言与长连接行为完全不受影响。
		if observed && sink != nil {
			sink.Publish(protocol.TopicHTTPRequest, HTTPRequest{Path: pattern, Method: r.Method})
		}
		h(w, r)
	})
	// 注册成功后才记账：panic 时走不到这里，影子表不会留下幽灵路由。
	c.routes.set(pattern, entry)
	return nil
}

// GetShared 读取跨插件共享 KV（首次读取返回 (nil, false)）。
func (c *Context) GetShared(key string) (any, bool) {
	if c.shared == nil {
		return nil, false
	}
	return c.shared.Load(key)
}

// SetShared 写入跨插件共享 KV。
func (c *Context) SetShared(key string, v any) {
	if c.shared == nil {
		return
	}
	c.shared.Store(key, v)
}

// String 读取字符串型配置项，缺失或非字符串时返回默认值。
func (c *Context) String(key, def string) string {
	if v, ok := c.options[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// Int 读取整型配置项（兼容 JSON 反序列化的 float64 与字面 int），缺失或类型不符返回默认值。
func (c *Context) Int(key string, def int) int {
	if v, ok := c.options[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return def
}

// Bool 读取布尔型配置项，缺失或类型不符返回默认值。
func (c *Context) Bool(key string, def bool) bool {
	if v, ok := c.options[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// Duration 读取时间型配置项：
//   - 数值按毫秒解释（JSON 数字反序列化为 float64）；
//   - 字符串按 time.ParseDuration 解析；
//   - 否则返回默认值。
func (c *Context) Duration(key string, def time.Duration) time.Duration {
	if v, ok := c.options[key]; ok {
		switch n := v.(type) {
		case float64:
			return time.Duration(n) * time.Millisecond
		case int:
			return time.Duration(n) * time.Millisecond
		case int64:
			return time.Duration(n) * time.Millisecond
		case string:
			if d, err := time.ParseDuration(n); err == nil {
				return d
			}
		}
	}
	return def
}

// emptyView 是 HubView 的空实现（未接入真实内核时返回空快照）。
type emptyView struct{}

func (emptyView) Agents() []AgentInfo { return nil }
func (emptyView) Rooms() []RoomInfo   { return nil }
func (emptyView) Stats() HubStats     { return HubStats{} }

// emptyPublisher 是 Publisher 的空实现（未接入真实内核时静默成功）。
type emptyPublisher struct{}

func (emptyPublisher) Send(protocol.Envelope) error   { return nil }
func (emptyPublisher) Broadcast(string, string) error { return nil }
