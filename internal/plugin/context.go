package plugin

import (
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
// 关键安全点：Context 不持有 *core.Hub 字段，也不提供 RegisterHTTP / mux 接入
// （那属于 V3）。插件只能通过 View() / Publish() 的窄接口与内核交互，
// 通过 String/Int/Bool/Duration 读取自身被合并后的配置项。
type Context struct {
	view    HubView
	pub     Publisher
	options map[string]any // 本插件的合并后配置项
}

// newContext 构造 Context；view/pub 为 nil 时退化为空实现（V2 未接入真实内核时可用）。
func newContext(view HubView, pub Publisher, options map[string]any) *Context {
	if view == nil {
		view = emptyView{}
	}
	if pub == nil {
		pub = emptyPublisher{}
	}
	if options == nil {
		options = map[string]any{}
	}
	return &Context{view: view, pub: pub, options: options}
}

// View 返回只读内核视图。
func (c *Context) View() HubView { return c.view }

// Publish 返回发布能力。
func (c *Context) Publish() Publisher { return c.pub }

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

func (emptyPublisher) Send(protocol.Envelope) error      { return nil }
func (emptyPublisher) Broadcast(string, string) error   { return nil }
