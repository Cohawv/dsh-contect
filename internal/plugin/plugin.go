// Package plugin 定义 V3 将要落地的插件框架骨架（V2 仅搭建最小内核，不接入 main.go）。
//
// 安全约束（来自 V2 审查）：
//   - 本包不得 import internal/core —— 插件可见 API 上绝不能出现 *core.Hub 实体；
//   - 插件只通过 Context 暴露的窄能力接口（HubView / Publisher）与内核交互，
//     会话句柄（Sess）等内部对象从类型层面被排除在插件可见范围之外；
//   - 本包允许 import internal/protocol（叶子包）。
package plugin

// State 是插件生命周期状态机节点。
type State int

const (
	// StateRegistered 已注册（尚未 Init）。
	StateRegistered State = iota
	// StateDisabled 被配置/白名单排除，不参与生命周期。
	StateDisabled
	// StateInitialized Init 成功，等待 Start。
	StateInitialized
	// StateRunning Start 成功，正在运行。
	StateRunning
	// StateStopped Stop 成功。
	StateStopped
	// StateFailed Init/Start/Stop 任一阶段失败或 panic 回收。
	StateFailed
)

// String 返回状态的可读名称（用于日志与状态展示）。
func (s State) String() string {
	switch s {
	case StateRegistered:
		return "registered"
	case StateDisabled:
		return "disabled"
	case StateInitialized:
		return "initialized"
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Meta 是插件的声明式元数据，在编译期随 Register 注册。
type Meta struct {
	Name         string   // 唯一标识，作为拓扑排序与启用开关的键
	Version      string   // 语义化版本
	Description  string   // 一句话说明
	Dependencies []string // 依赖的其它插件 Name（拓扑排序的边）
	DefaultOn    bool     // 未显式配置时的默认启用状态
	// Required 标记该插件为「必需」：一旦被白名单/CLI 排除（禁用），
	// 整个启动必须失败并明确报错（审查 M5 新增）。
	Required bool
}

// Info 是插件的运行时信息快照。
type Info struct {
	Meta
	State     State // 当前生命周期状态
	Error     string // 失败原因（StateFailed 时为人可读描述）
	EnabledAt int64  // 启用时间戳（毫秒），未启用为 0
}

// Plugin 是插件必须实现的接口。
// V2 仅做装配与单测，Init/Start/Stop 不要求真正起协程（本版本「全量启」，不支持运行期单独启停）。
type Plugin interface {
	Meta() Meta
	Init(ctx *Context) error
	Start() error
	Stop() error
}
