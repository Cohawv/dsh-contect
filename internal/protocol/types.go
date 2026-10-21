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
	ActionError       = "error"       // 通用错误
)

// 错误码（配合 ActionError 使用）
const (
	CodeUnknownType = "unknown_type"
	CodeBadJSON     = "bad_json"
	CodeTargetOffline = "target_offline"
)

// 事件 topic（core 内部事件总线使用，属于稳定契约的一部分）
const (
	TopicAgentOnline  = "agent.online"
	TopicAgentOffline = "agent.offline"
	TopicRoomChanged  = "room.changed"
)

// 保活参数的默认值。
// 真实值由服务端在注册回执里下发（能力协商），客户端不应硬编码。
const (
	DefaultHeartbeatMs = 15_000
	DefaultTimeoutMs   = 60_000
)
