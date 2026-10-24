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
	// TopicMsgContent 一条已路由消息的正文（opt-in：刻意不在默认订阅集合里）。
	//
	// 与 msg.routed 分离的设计意图：
	//   - msg.routed 只计数量（kind/delivered/seq），供 metrics 计数，任何订阅者都拿不到内容；
	//   - msg.content 才带正文（from/to/roomId/text/ts/seq），但**只有显式订阅它的客户端**才会收到。
	// 网页控制台借此成为 Hub 的一等参与者：能收发、能看历史（会话视图）。
	// 隐私权衡：内容只在客户端主动订阅该 topic 时才下发，不在默认集合、不进日志流。
	TopicMsgContent = "msg.content"
	// TopicGroupChanged 分组集合发生变化（V6 ② group 插件发布：组的创建/删除/成员变更）。
	//
	// 与 room.changed 同构：组的增删是低频管理动作，载荷带组名与解析后的成员快照，
	// 供控制台实时刷新组视图；不订阅的客户端零感知。
	TopicGroupChanged = "group.changed"
	// TopicHTTPRequest 一次 HTTP 请求进入共享 mux（探针端点除外）。
	TopicHTTPRequest = "http.request"
	// TopicMemHigh 进程堆占用跨越告警阈值（V8 ④ pprof 插件发布）。
	//
	// 只在**跨越**阈值时发一次（带 90% 迟滞），不按采样周期重复——
	// 订阅方拿到的是「事件」而不是「心跳」，可以放心地弹一次横幅。
	// 与 msg.content 同样属于 opt-in：不在 realtime 的默认订阅集合里。
	TopicMemHigh = "mem.high"
)

// MemHigh 是 protocol.TopicMemHigh 的事件载荷（V8 ④ 内存阈值告警）。
//
// 只带判定所需的最小数字：运维关心的是「多高、上限多少、多少个 goroutine」，
// 不需要也不应该从事件里拿走整个堆内容（那要走 pprof 快照）。
type MemHigh struct {
	HeapAllocBytes uint64 `json:"heapAllocBytes"`
	LimitBytes     uint64 `json:"limitBytes"`
	Goroutines     int    `json:"goroutines"`
	HeapObjects    uint64 `json:"heapObjects"`
	NumGC          uint32 `json:"numGC"`
	Count          int64  `json:"count"` // 本次是第几次进入超阈值状态
	TS             int64  `json:"ts"`    // 采样时刻（毫秒）
}

// MsgContent 是 protocol.TopicMsgContent 的事件载荷（聊天正文，供控制台展示）。
//
// 只暴露展示所需的最小字段；正文 text 由路由侧从信封 Content 中尽力提取
// （Content 形如 {"text":"..."} 时提取，否则为空串），不把整个信封塞进事件。
type MsgContent struct {
	Type   string `json:"type"`
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	RoomID string `json:"roomId,omitempty"`
	Text   string `json:"text,omitempty"`
	TS     int64  `json:"ts"`
	Seq    int64  `json:"seq"`
}

// GroupChanged 是 protocol.TopicGroupChanged 的事件载荷（V6 ② group 插件发布）。
//
// action 取值：created / deleted / updated（成员或定义变更）。
// Members 是「解析后」的成员快照（静态组为配置成员，动态组为当前在线的匹配 Agent），
// 订阅方无需再回查一次组定义。
type GroupChanged struct {
	Action  string   `json:"action"` // created | deleted | updated
	Name    string   `json:"name"`
	Source  string   `json:"source"` // static | dynamic | runtime
	Members []string `json:"members"`
}

// AllTopics 返回本包声明的全部事件 topic（供死契约检查遍历）。
// 新增 topic 常量时**必须**同步加到这里，否则死契约检查看不到它。
func AllTopics() []string {
	return []string{
		TopicAgentOnline,
		TopicAgentOffline,
		TopicRoomChanged,
		TopicMsgRouted,
		TopicMsgContent,
		TopicGroupChanged,
		TopicHTTPRequest,
		TopicMemHigh,
	}
}

// 保活参数的默认值。
// 真实值由服务端在注册回执里下发（能力协商），客户端不应硬编码。
const (
	DefaultHeartbeatMs = 15_000
	DefaultTimeoutMs   = 60_000
)
