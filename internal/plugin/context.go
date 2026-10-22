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
// V3 扩展点（相对 V2）：
//   - 持有共享 *http.ServeMux 与影子路由表，插件经 RegisterHTTP 注册端点，
//     由框架统一做冲突预检（不依赖 ServeMux 的 panic 行为）；
//   - 持有共享 KV（跨插件交接运行时对象，如 discovery 服务、dashboard URL）；
//   - 持有内核实体（any 形式），内置插件在 internal/plugins/* 自行类型断言取 *core.Hub。
//
// 关键安全点：本包（框架）不得 import internal/core——Context 只以 any 持有 hub，
// 真正取出 *core.Hub 的逻辑在插件侧，框架本身永远看不到该具体类型。
type Context struct {
	view        HubView
	pub         Publisher
	options     map[string]any    // 本插件的合并后配置项
	mux         *http.ServeMux    // 共享 mux：所有 HTTP 插件注册到它
	routes      *sync.Map         // 影子路由表 pattern → 拥有者插件名（冲突预检）
	shared      *sync.Map         // 跨插件共享 KV：key(string) → any
	hub         any               // 业务内核实体（*core.Hub），框架侧为 any
	owner       string            // 当前插件名（用于冲突报错点名）
}

// newContext 构造 Context；view/pub 为 nil 时退化为空实现（V2 未接入真实内核时可用）。
func newContext(view HubView, pub Publisher, options map[string]any, owner string, mux *http.ServeMux, routes, shared *sync.Map, hub any) *Context {
	if view == nil {
		view = emptyView{}
	}
	if pub == nil {
		pub = emptyPublisher{}
	}
	if options == nil {
		options = map[string]any{}
	}
	return &Context{view: view, pub: pub, options: options, owner: owner, mux: mux, routes: routes, shared: shared, hub: hub}
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

// RegisterHTTP 把 pattern 注册到共享 mux，并做冲突预检。
//
// 先用影子表（routes）查重：若 pattern 已被占用，返回 error 并**点名冲突双方插件名**，
// 绝不依赖 *http.ServeMux 对重复 pattern 的 panic 行为；
// 再用 recover 兜底 ServeMux 的其它（意外）panic，转为 Init 错误由 Manager 隔离。
func (c *Context) RegisterHTTP(pattern string, h http.HandlerFunc) error {
	if c.routes == nil || c.mux == nil {
		return fmt.Errorf("plugin %q: context has no shared mux (BuildContext not called?)", c.owner)
	}
	if prev, dup := c.routes.Load(pattern); dup {
		return fmt.Errorf("route %q already registered by plugin %q (conflicts with %q)", pattern, prev.(string), c.owner)
	}
	c.routes.Store(pattern, c.owner)
	defer func() {
		if r := recover(); r != nil {
			// ServeMux 意外的 panic（理论上不会，因为已用影子表挡住重复 pattern）：转为错误。
			_ = r
		}
	}()
	c.mux.HandleFunc(pattern, h)
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

// emptyView 是 HubView 的空实现（V2 未接入真实内核时返回空快照）。
type emptyView struct{}

func (emptyView) Agents() []AgentInfo { return nil }
func (emptyView) Rooms() []RoomInfo   { return nil }
func (emptyView) Stats() HubStats     { return HubStats{} }

// emptyPublisher 是 Publisher 的空实现（V2 未接入真实内核时静默成功）。
type emptyPublisher struct{}

func (emptyPublisher) Send(protocol.Envelope) error    { return nil }
func (emptyPublisher) Broadcast(string, string) error { return nil }
