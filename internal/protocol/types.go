package protocol

// system 回执的 action 取值（结构化契约）。
//
// 背景：早期版本把语义承载在 content.text 的英文文案上（如 "registered as A"），
// 客户端靠字符串前缀匹配识别 —— 服务端改一个词就会导致客户端静默超时。
// 结构化 action 是替代方案；过渡期两者并存（双写），老客户端只读 text。
const (
	ActionRegistered  = "registered"  // 注册成功
	ActionCreated     = "created"     // 房间已创建
	ActionList        = "list"        // 在线列表
	ActionUndelivered = "undelivered" // 消息未送达（如目标离线）
	ActionAckReceived = "ack_received" // 服务端已收到投递确认（V6 ②）
	ActionError       = "error"       // 通用错误
)

// 错误码（配合 ActionError 使用）
const (
	CodeUnknownType   = "unknown_type"
	CodeBadJSON       = "bad_json"
	CodeTargetOffline = "target_offline"
)

// 事件 topic（core 内部事件总线使用，属于稳定契约的一部分）
//
// 命名约定 domain.entity，前端按前缀订阅（如 agent.* 同时匹配 agent.online/agent.offline）。
//
// V5 变更（只增不改）：
//   - room.changed 补上了真实发布方（V1 起一直是死契约，见 RoomService）；
//   - 新增 msg.routed / http.request 两个可观测性 topic（供 metrics 计数）。
//
// 铁律：本清单里出现的每个 topic **都必须有发布方**，
// 由 test/contract/topics_test.go 的死契约检查强制（新增 topic 必须同时补发布点与测试）。
const (
	TopicAgentOnline  = "agent.online"
	TopicAgentOffline = "agent.offline"
	TopicRoomChanged  = "room.changed"
	// TopicMsgRouted 一条消息完成路由（载荷只放计数与 kind，不放整个信封）。
	TopicMsgRouted = "msg.routed"
	// TopicHTTPRequest 一次 HTTP 请求进入共享 mux（探针端点除外）。
	TopicHTTPRequest = "http.request"
)

// AllTopics 返回本包声明的全部事件 topic（供死契约检查遍历）。
// 新增 topic 常量时**必须**同步加到这里，否则死契约检查看不到它。
func AllTopics() []string {
	return []string{
		TopicAgentOnline,
		TopicAgentOffline,
		TopicRoomChanged,
		TopicMsgRouted,
		TopicHTTPRequest,
	}
}

// 保活参数的默认值。
// 真实值由服务端在注册回执里下发（能力协商），客户端不应硬编码。
const (
	DefaultHeartbeatMs = 15_000
	DefaultTimeoutMs   = 60_000
)
