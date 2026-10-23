package protocol

// 协议版本。
//
// 规则：
//   - 客户端不声明 v（或 v=0）时按 ProtoLegacy 处理，行为与历史版本完全一致；
//   - 服务端永不因版本过低而拒绝连接，只降级到对端能理解的行为；
//   - 版本号只用于「启用新行为」，不用于「拒绝服务」。
const (
	ProtoLegacy       = 1 // 历史版本：无能力协商，回执靠 text 承载语义
	ProtoV2           = 2 // v2：能力协商 + 结构化回执
	ProtoCurrent      = 3 // 当前版本：能力协商增强（features / 自动发现 / 端点信息）
	ProtoMinSupported = 1
)

// Negotiate 计算本次会话采用的协议版本：取「客户端声明」与「服务端支持」的较小值。
// 服务端永远迁就客户端，这是「服务端可独立升级」的前提。
func Negotiate(clientV int) int {
	if clientV <= 0 {
		return ProtoLegacy
	}
	if clientV > ProtoCurrent {
		return ProtoCurrent
	}
	if clientV < ProtoMinSupported {
		return ProtoLegacy
	}
	return clientV
}

// ServerCaps 是服务端当前开放的能力集，在注册回执里下发给客户端。
// 客户端应忽略不认识的 cap，而不是报错——这样服务端新增能力不会打断老客户端。
var ServerCaps = []string{
	"structured-ack",        // 回执带结构化 action 字段
	"heartbeat-negotiation", // 心跳参数由服务端下发
	"discovery",             // 局域网自动发现（UDP 广播 + 单播探测）
	"agent-caps",            // list 回执携带各 agent 的 caps/proto
	"endpoints",             // 注册回执下发 tcp/http 端点
	"replay",                // 离线消息回放（声明后注册回执带 lastSeq、可携带 sinceSeq 续传）
}

// HasCap 判断能力集中是否包含某个能力。
func HasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// RegisteredAck 构造注册回执。
//
// 兼容性约定：
//   - text 恒定输出（老客户端的唯一生命线，长期保留）；
//   - action / agentId 是纯增量字段，老客户端会忽略，因此无条件输出；
//   - proto / serverCaps / heartbeatMs / timeoutMs 会改变客户端行为，
//     因此仅在协商版本 >= 2 时下发（版本门控）；
//   - features / clientCaps / endpoints 是 v3 新增，仅在 proto >= 3 时下发，
//     且 features 内部仍按客户端声明的 cap 逐位门控。
//   - lastSeq 仅在客户端声明 replay cap 时下发（V6 ①），告知客户端「服务端已持久化的
//     最大消息序号」，便于客户端携带 sinceSeq 做离线回放；老客户端与不声明 replay 的
//     客户端永远看不到该字段。
func RegisteredAck(agentID string, clientCaps []string, proto int, hb EndpointsCfg, lastSeq int64) Envelope {
	content := map[string]any{
		"text":    "registered as " + agentID,
		"action":  ActionRegistered,
		"agentId": agentID,
	}
	if proto >= 2 {
		content["proto"] = proto
		content["serverCaps"] = ServerCaps
		content["heartbeatMs"] = hb.HeartbeatMs
		content["timeoutMs"] = hb.TimeoutMs
	}
	if proto >= 3 {
		content["features"] = ComputeFeatures(proto, clientCaps)
		// 回显客户端声明的能力集，便于排障（尤其自动发现是否生效）
		content["clientCaps"] = CapSet(clientCaps).Sorted()
		if ComputeFeatures(proto, clientCaps).Endpoints {
			content["endpoints"] = Endpoints{TCPPort: hb.TCPPort, HTTPPort: hb.HTTPPort}
		}
	}
	// replay cap 门控：仅声明 replay 的客户端收到 lastSeq（V6 ①）。
	if HasCap(clientCaps, CapReplay) {
		content["lastSeq"] = lastSeq
	}
	return NewEnvelope(TypeSystem, "hub", agentID, "", content)
}

// EndpointsCfg 携带服务端保活参数与端口信息，供 RegisteredAck 渲染。
type EndpointsCfg struct {
	HeartbeatMs int64
	TimeoutMs   int64
	TCPPort     int
	HTTPPort    int
}
