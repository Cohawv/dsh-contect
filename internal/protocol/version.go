package protocol

// 协议版本。
//
// 规则：
//   - 客户端不声明 v（或 v=0）时按 ProtoLegacy 处理，行为与历史版本完全一致；
//   - 服务端永不因版本过低而拒绝连接，只降级到对端能理解的行为；
//   - 版本号只用于「启用新行为」，不用于「拒绝服务」。
const (
	ProtoLegacy       = 1 // 历史版本：无能力协商，回执靠 text 承载语义
	ProtoCurrent      = 2 // 当前版本：能力协商 + 结构化回执
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
	"structured-ack",       // 回执带结构化 action 字段
	"heartbeat-negotiation", // 心跳参数由服务端下发
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
//     因此仅在协商版本 >= 2 时下发（版本门控）。
func RegisteredAck(agentID string, proto int, heartbeatMs, timeoutMs int64) Envelope {
	content := map[string]any{
		"text":    "registered as " + agentID,
		"action":  ActionRegistered,
		"agentId": agentID,
	}
	if proto >= 2 {
		content["proto"] = proto
		content["serverCaps"] = ServerCaps
		content["heartbeatMs"] = heartbeatMs
		content["timeoutMs"] = timeoutMs
	}
	return NewEnvelope(TypeSystem, "hub", agentID, "", content)
}
