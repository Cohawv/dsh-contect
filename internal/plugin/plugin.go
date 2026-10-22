// Package plugin 定义插件框架：声明式元数据、生命周期编排与运行期管理能力。
//
// 安全约束（来自 V2/V3 审查，由 scripts/check-deps.sh 强制）：
//   - 本包不得 import internal/core —— 插件可见 API 上绝不能出现 *core.Hub 实体；
//   - 插件只通过 Context 暴露的窄能力接口（HubView / Publisher）与内核交互，
//     会话句柄（Sess）等内部对象从类型层面被排除在插件可见范围之外；
//   - 本包不得 import internal/plugins —— 框架与具体插件解耦；
//   - 本包允许 import internal/protocol（叶子包）。
package plugin

import "encoding/json"

// State 是插件生命周期状态机节点。
type State int

const (
	// StateRegistered 已注册（尚未 Init）。
	StateRegistered State = iota
	// StateDisabled 被配置/白名单排除/运行期停用，不参与生命周期。
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

// String 返回状态的可读名称（用于日志、状态展示与 JSON 输出）。
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

// MarshalJSON 让状态在 JSON 里以可读名称出现（管理 API 与状态历史需要人类可读）。
// 例：{"from":"registered","to":"running"} 而不是 {"from":0,"to":3}。
func (s State) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
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
	// RequiredReason 是 Required=true 时启动/停用失败给出的人可读原因，
	// 解释「为什么这个插件不能少」；为空则回退到通用说明。
	RequiredReason string
}

// Info 是插件的运行时信息快照。
type Info struct {
	Meta
	State     State  // 当前生命周期状态
	Error     string // 失败原因（StateFailed 时为人可读描述）
	EnabledAt int64  // 启用时间戳（毫秒），未启用为 0
	StartedAt int64  // 启动时间戳（毫秒），未启动为 0
	StoppedAt int64  // 停止时间戳（毫秒），未停止为 0
	// StateChangedAt 是最近一次状态变更时间戳（毫秒），V4 新增，排障很省事。
	StateChangedAt int64
	// RestartCount 是运行期成功启停次数（V4 新增）：每次成功的 Enable/Disable/Reload
	// 会给对应插件 +1，用于判断「这个插件被反复折腾过」。
	RestartCount int
}

// Plugin 是插件必须实现的接口（最小的三态生命周期）。
// 运行期启停由 Manager 负责编排，插件只需把 Init/Start/Stop 写成可重复调用安全的形态：
// 同一个插件实例会在运行期被多次 Init→Start→Stop→Init→Start……
type Plugin interface {
	Meta() Meta
	Init(ctx *Context) error
	Start() error
	Stop() error
}

// OptionType 是插件配置项的类型标签，驱动控制台**自动生成**表单控件（V4 ④）。
// 前端不写任何插件专属代码，只按本类型渲染。
type OptionType string

const (
	// OptionString 单行文本输入。
	OptionString OptionType = "string"
	// OptionInt 数字输入（配合 Min/Max 做范围校验）。
	OptionInt OptionType = "int"
	// OptionBool 开关。
	OptionBool OptionType = "bool"
	// OptionDuration 时长文本（如 "30s"、"1m"），服务端用 time.ParseDuration 校验。
	OptionDuration OptionType = "duration"
	// OptionSelect 下拉（取值必须落在 Choices 内）。
	OptionSelect OptionType = "select"
)

// OptionDef 是插件对单个配置项的声明。
//
// 语义约定（服务端按此校验，非法值一律 400 且不写盘）：
//   - string  → Default 为 string
//   - int     → Default 为 int；Min/Max 非 0 时做边界校验（0 表示不限制）
//   - bool    → Default 为 bool
//   - duration→ Default 为可被 time.ParseDuration 解析的字符串（如 "30s"）
//   - select  → Default 为 string，且必须出现在 Choices 中
type OptionDef struct {
	Key         string     `json:"key"`                   // 配置键（写入 hub.json 的 plugins.<name>.options）
	Type        OptionType `json:"type"`                  // 控件类型
	Default     any        `json:"default,omitempty"`     // 默认值
	Description string     `json:"description,omitempty"` // 人可读说明（显示在表单下方）
	Min         int        `json:"min,omitempty"`         // 仅 int 有效：最小值（0 = 不限制）
	Max         int        `json:"max,omitempty"`         // 仅 int 有效：最大值（0 = 不限制）
	Choices     []string   `json:"choices,omitempty"`     // 仅 select 有效：可选值
}

// Optioner 是「可声明配置项」的插件的**可选**接口。
//
// 刻意不并入 Plugin 主接口：并入会破坏全部既有插件与测试替身（编译期不兼容），
// 而「能否自动生成设置表单」本就不是每个插件的必要能力。
type Optioner interface {
	Options() []OptionDef
}

// Applier 是「支持配置热更新」的插件的**可选**接口（V4 补齐审查 M6 的语义缺口）。
//
// 实现它 ⇒ 保存设置后立即生效；未实现 ⇒ 保存成功但如实标注 pendingRestart（重启后生效）。
// 绝不假装热生效——那会让用户以为改了却什么也没变。
type Applier interface {
	Apply(options map[string]any) error
}
