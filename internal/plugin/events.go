package plugin

// 插件生命周期事件 topic。
//
// 这些常量供内部事件总线使用，描述插件的启动/停止/失败，不属于对外 protocol 契约，
// 因而放在本包而非 internal/protocol（避免污染客户端契约）。
//
// V5：新增 TopicState —— 插件状态**变更**的统一事件（启停/失败/重载都会发一条），
// 由 Manager.setState 发布，供实时推送（realtime 插件）与控制台徽标增量更新消费。
// 它与 TopicStarted/Stopped/Failed 的关系是「聚合 vs 细分」：TopicState 覆盖每一次迁移，
// 细分的三个常量保留给将来需要精确语义的订阅者。
const (
	// TopicStarted 插件成功启动。
	TopicStarted = "plugin.started"
	// TopicStopped 插件成功停止。
	TopicStopped = "plugin.stopped"
	// TopicFailed 插件在 Init/Start/Stop 任一阶段失败。
	TopicFailed = "plugin.failed"
	// TopicState 插件状态发生迁移（载荷为 StateEventPayload）。
	TopicState = "plugin.state"
)

// AllTopics 返回本包声明的全部插件类事件 topic（供死契约检查遍历）。
func AllTopics() []string {
	return []string{TopicStarted, TopicStopped, TopicFailed, TopicState}
}

// StateEventPayload 是 TopicState 的事件载荷。
//
// 只放可序列化的小字段，不放 Plugin / Context / Manager 等任何实例引用——
// 订阅者可能来自别的插件，绝不能借事件拿到框架内部对象。
type StateEventPayload struct {
	Name  string `json:"name"`            // 插件名
	State State  `json:"state"`           // 迁移后状态（JSON 里是可读字符串）
	Prev  State  `json:"prev"`            // 迁移前状态
	Error string `json:"error,omitempty"` // 同时发生的错误
	At    int64  `json:"at"`              // 迁移时间戳（毫秒）
}

// EventSink 是框架「发布事件」的窄接口。
//
// *core.EventBus 天然满足它（Publish(topic string, payload any)），
// 因此框架无需 import internal/core 就能把插件状态送进事件总线——
// 依赖方向（plugin 不依赖 core）保持不变。
type EventSink interface {
	Publish(topic string, payload any)
}

// HTTPRequest 是 protocol.TopicHTTPRequest 的事件载荷。
//
// 只放**低基数**标签（path 是路由 pattern 而不是带参数的完整 URL，
// 且 metrics 侧对未知 path 做了聚合上限），绝不放 agentId/userId 这类高基数维度，
// 否则 Prometheus 侧会基数爆炸。
type HTTPRequest struct {
	Path   string `json:"path"`
	Method string `json:"method"`
}
