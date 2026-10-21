package protocol

// 能力声明（capability tokens）。
//
// 约定：
//   - 服务端把能提供的特征列在 ServerCaps（注册回执下发）；
//   - 客户端在 register 时声明自己关心的 caps，服务端据此「门控」部分回执字段；
//   - 双方都不认识对方新声明的 cap —— 直接忽略（JSON 天然宽容），绝不让连接失败；
//   - 新增 cap 永远是「增量」，老客户端不声明就不会收到对应字段。
const (
	// structured-ack：系统回执带结构化 action 字段（v2 起即支持）。
	CapStructuredAck = "structured-ack"
	// heartbeat-negotiation：心跳/超时参数由服务端在注册回执下发。
	CapHeartbeatNeg = "heartbeat-negotiation"
	// discovery：客户端支持局域网自动发现（广播 / 单播探测）。
	CapDiscovery = "discovery"
	// agent-caps：在线列表回执里携带每个 agent 的 caps / proto。
	CapAgentCaps = "agent-caps"
	// endpoints：注册回执下发服务端 tcp/http 端点信息。
	CapEndpoints = "endpoints"
)

// CapSet 是能力集合的便捷封装。
type CapSet []string

// Has 判断集合是否包含某能力。
func (cs CapSet) Has(cap string) bool { return HasCap(cs, cap) }

// Sorted 返回排序后的副本（用于生成稳定指纹 / 日志）。
func (cs CapSet) Sorted() []string {
	out := append([]string(nil), cs...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// NegotiatedFeatures 描述服务端对一个会话实际启用的特征。
//
// 关键设计：所有「会改变客户端行为」或「新增语义」的字段都收拢到 features，
// 且 features 仅在 proto >= 3 时随注册回执下发；v1/v2 客户端完全看不到它，
// 自然回到历史行为。feature 在 proto>=3 前提下，再逐个受 cap 门控：
//   - structuredAck 只要 proto>=2 即 true（历史已存在的增量字段，不依赖 cap）；
//   - 其余 feature 必须由客户端显式声明对应 cap 才启用。
type NegotiatedFeatures struct {
	StructuredAck bool `json:"structuredAck"`
	Heartbeat     bool `json:"heartbeat"`
	Discovery     bool `json:"discovery"`
	AgentCaps     bool `json:"agentCaps"`
	Endpoints     bool `json:"endpoints"`
}

// ComputeFeatures 根据协商版本与客户端能力集计算启用特征。
// 服务端「永远迁就客户端」：版本不够或没声明 cap，对应 feature 直接 false。
func ComputeFeatures(proto int, clientCaps []string) NegotiatedFeatures {
	cs := CapSet(clientCaps)
	f := NegotiatedFeatures{}
	if proto >= 2 {
		f.StructuredAck = true // structured-ack 自 v2 起即稳定，无需额外 cap
	}
	if proto < 3 {
		return f // v1/v2 客户端不接收 features 对象，所有新特征按 false 处理
	}
	f.Heartbeat = cs.Has(CapHeartbeatNeg)
	f.Discovery = cs.Has(CapDiscovery)
	f.AgentCaps = cs.Has(CapAgentCaps)
	f.Endpoints = cs.Has(CapEndpoints) || cs.Has(CapDiscovery) // discovery 隐含需要 endpoints
	return f
}

// Endpoints 随注册回执下发的服务端端点信息（cap: endpoints）。
type Endpoints struct {
	TCPPort  int `json:"tcpPort"`
	HTTPPort int `json:"httpPort"`
}
