package plugin

// 插件生命周期事件 topic。
//
// 这些常量供内部事件总线使用，描述插件的启动/停止/失败，不属于对外 protocol 契约，
// 因而放在本包而非 internal/protocol（避免污染客户端契约）。
const (
	// TopicStarted 插件成功启动。
	TopicStarted = "plugin.started"
	// TopicStopped 插件成功停止。
	TopicStopped = "plugin.stopped"
	// TopicFailed 插件在 Init/Start/Stop 任一阶段失败。
	TopicFailed = "plugin.failed"
)
