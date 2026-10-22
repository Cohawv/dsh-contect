package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// 运行期管理相关的哨兵错误：便于插件层（plugininfo）把它们映射成明确的 HTTP 状态码，
// 而不是靠匹配错误文案。
var (
	// ErrUnknownPlugin 指定的插件不存在（→ 400）。
	ErrUnknownPlugin = errors.New("unknown plugin")
	// ErrAlreadyEnabled 插件已处于启用状态（→ 409）。
	ErrAlreadyEnabled = errors.New("plugin already enabled")
	// ErrAlreadyDisabled 插件已处于停用状态（→ 409）。
	ErrAlreadyDisabled = errors.New("plugin already disabled")
	// ErrRequiredPlugin 必需插件不可停用（→ 409）。
	ErrRequiredPlugin = errors.New("required plugin cannot be disabled")
	// ErrDependedOn 被其它运行中插件依赖，不可停用（→ 409）。
	ErrDependedOn = errors.New("plugin is depended on by other plugins")
	// ErrReadOnly 只读模式（-config-readonly）下禁止写配置（→ 400）。
	ErrReadOnly = errors.New("config is read-only")
	// ErrNoConfigPath 未配置配置文件路径（→ 400）。
	ErrNoConfigPath = errors.New("config path is not set")
	// ErrInvalidConfig 配置校验失败（未知键 / 类型不符 / 越界）（→ 400）。
	ErrInvalidConfig = errors.New("invalid config")
	// ErrConflict 配置版本冲突（乐观锁，审查 M4）（→ 409）。
	ErrConflict = errors.New("config version conflict")
)

// sharedKeyManager 是管理器在共享 KV 中的键。
//
// plugininfo 这类「管理类插件」通过 plugin.ManagerFromContext(ctx) 取到管理器，
// 从而既能读快照、也能执行启停——而框架本身不必知道有 plugininfo 这个插件存在。
const sharedKeyManager = "plugin.manager"

// Result 记录单个插件在 StartAll / Reload 阶段的结果（有序输出，可直接打印成启动表）。
type Result struct {
	Name     string        // 插件名
	State    State         // 最终状态
	Duration time.Duration // 启动耗时
	Error    string        // 失败原因（成功时为空）
	Skipped  bool          // 是否被跳过（未真正重载），原因见 Reason
	Reason   string        // 跳过原因（Skipped=true 时非空）
}

// ReloadSkipper 是可选的「拒绝参与全量重载」的接口。
//
// 为什么需要它：plugininfo 的 /api/plugins/reload 会逐个 Stop→Init→Start，
// 而 httpgateway 本身就是承载 /api/* 的 HTTP 服务——在它的请求处理过程中 Stop 自己，
// 会把「正在等待响应」的那条连接一起掐断（客户端表现为 reload 请求超时）。
// 让它显式声明「别重载我」比在框架里硬编码插件名更诚实、也更可扩展。
type ReloadSkipper interface {
	SkipReload() (skip bool, reason string)
}

// RouteInfo 描述一条已注册路由及其归属插件（V3-③ 路由冲突预检的副产品）。
type RouteInfo struct {
	Pattern string `json:"pattern"`
	Plugin  string `json:"plugin"`
}

// StateEvent 是一次插件状态变更记录。
// 每个插件保留最近 stateHistorySize 条（环形缓冲），供单插件详情页排障。
type StateEvent struct {
	At    int64  `json:"at"`              // 变更时间戳（毫秒）
	From  State  `json:"from"`            // 变更前状态
	To    State  `json:"to"`              // 变更后状态
	Error string `json:"error,omitempty"` // 同时发生的错误（成功时为空）
	// RolledBack 标记「这次迁移已被回滚」：历史保留（排障要看得到尝试过什么），
	// 但必须标明它不代表当前状态，避免排障时把中间态误读成现状。
	RolledBack bool `json:"rolledBack,omitempty"`
}

// stateHistorySize 是每个插件保留的状态变更历史条数。
const stateHistorySize = 20

// Manager 负责插件的装配与生命周期编排。
//
// V3：全量装配（InitAll）+ 一次性启动（StartAll）+ 逆序停止（StopAll）。
// V4：新增运行期管理——Snapshot/Enable/Disable/Reload/Routes 与配置读写，
// 所有启停操作经 opMu 全局串行化（风险 V4-1），失败回滚到操作前状态。
type Manager struct {
	plugins []Plugin          // 按 Name 升序
	byName  map[string]Plugin // Name → 插件

	mu      sync.RWMutex              // 保护 infos/enabled/options/history（读多写少）
	infos   map[string]*Info          // Name → 运行时信息
	enabled map[string]bool           // Name → 是否启用（缺省视为启用）
	options map[string]map[string]any // Name → 合并后的配置项
	history map[string][]StateEvent   // Name → 状态变更历史（最近 20 条）
	gates   map[string]*gate          // Name → 路由闸门

	// opMu 串行化运行期启停/重载，避免「半启状态」的交叉污染（风险 V4-1）。
	opMu sync.Mutex

	view   HubView   // 注入给插件的只读视图
	pub    Publisher // 注入给插件的发布能力
	mux    *http.ServeMux
	routes *routeTable
	shared *sync.Map
	hub    any // 业务内核实体（*core.Hub），框架侧为 any

	cfgPath     string // 配置文件路径（"" 表示不落盘）
	cfgReadOnly bool   // 只读模式：禁止写盘
	store       *Store // 配置文件存储（懒建）

	// blocked 记录「Stop 超时后拒绝再次装配」的插件（F5）。
	// 超时的 Stop 协程仍在跑，若此时又 Init 同一个实例（进程级单例），
	// 两边会并发操作同一份 listener/协程状态。唯一的解锁方式：显式 Reload。
	blocked map[string]string // Name → 阻断原因

	// sink 是事件发布出口（V5）：由 SetEventBus 在装配期注入，
	// 用于发布 plugin.state（启停）与 http.request（访问计数）。
	// 为 nil 时静默不发布，框架行为与 V4 完全一致。
	sink EventSink

	// logFan 是接管的 log 输出分发器（V5）：仅当存在实现 LogSink 的插件时才装上。
	// 为 nil 表示未接管 log（行为与 V4 逐字节一致）。
	logFan *logFanout
}

// NewManager 从全局注册表构建管理器（生产用法）。
func NewManager() *Manager {
	return newManagerFrom(All())
}

// NewManagerWith 以**显式插件列表**构建管理器（V5，供测试与定制部署）。
//
// 与 NewManager 的唯一差别是插件集合的来源：不读全局注册表。
// 这让测试可以只装配「自己关心的那几个插件」，
// 而不必为了一个端点把整个二进制的能力清单都拉起来。
func NewManagerWith(plugins []Plugin) *Manager {
	return newManagerFrom(plugins)
}

// SetEventBus 注入事件总线（V5）。传 nil 表示不发布任何框架事件。
//
// 参数类型是窄接口 EventSink 而非 *core.EventBus：框架绝不能 import internal/core，
// 而 *core.EventBus 恰好满足 Publish(topic string, payload any)。
// 应在 InitAll 之前调用（生产由 cmd/hub/main.go 完成）。
func (m *Manager) SetEventBus(sink EventSink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
}

// eventSink 返回事件出口（未注入时为 nil）。
func (m *Manager) eventSink() EventSink {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sink
}

// publish 发布一条框架事件；未注入总线时静默（与 V4 行为一致）。
//
// 调用点必须保证**不在持有 m.mu 时**调用（订阅方可能回头读管理器快照）。
func (m *Manager) publish(topic string, payload any) {
	if sink := m.eventSink(); sink != nil {
		sink.Publish(topic, payload)
	}
}

// newManagerFrom 以显式插件列表构建管理器，供测试注入（避免依赖全局注册表状态）。
func newManagerFrom(plugins []Plugin) *Manager {
	m := &Manager{
		byName:  make(map[string]Plugin, len(plugins)),
		infos:   make(map[string]*Info, len(plugins)),
		enabled: make(map[string]bool),
		options: make(map[string]map[string]any),
		history: make(map[string][]StateEvent, len(plugins)),
		gates:   make(map[string]*gate, len(plugins)),
		blocked: make(map[string]string),
	}
	// 按 Name 升序排列，保证后续一切顺序的确定性。
	sorted := make([]Plugin, len(plugins))
	copy(sorted, plugins)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Meta().Name < sorted[j].Meta().Name
	})
	now := time.Now().UnixMilli()
	for _, p := range sorted {
		name := p.Meta().Name
		m.plugins = append(m.plugins, p)
		m.byName[name] = p
		m.gates[name] = newGate()
		m.infos[name] = &Info{Meta: p.Meta(), State: StateRegistered, StateChangedAt: now}
		m.history[name] = nil
	}
	return m
}

// ManagerFromContext 取出框架管理器（供 plugininfo 等管理类插件使用）。
func ManagerFromContext(c *Context) (*Manager, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.GetShared(sharedKeyManager)
	if !ok {
		return nil, false
	}
	m, ok := v.(*Manager)
	return m, ok
}

// SetEnabled 显式设置某插件的启用状态（覆盖默认）。不做生命周期动作，仅用于装配前配置。
func (m *Manager) SetEnabled(name string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled[name] = on
}

// SetHub 注入内核能力（HubView / Publisher）。传 nil 则保留默认空实现。
func (m *Manager) SetHub(view HubView, pub Publisher) {
	if view != nil {
		m.view = view
	}
	if pub != nil {
		m.pub = pub
	}
}

// ApplyConfig 应用配置加载的结果（启用状态 + 每插件配置项）。
func (m *Manager) ApplyConfig(enabled map[string]bool, options map[string]map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range enabled {
		m.enabled[k] = v
	}
	for k, v := range options {
		m.options[k] = v
	}
}

// BuildContext 建立共享 mux、影子路由表、跨插件 KV 与路由闸门，并注入业务内核实体。
// 必须在 InitAll 之前调用；hub 通常为 *core.Hub，框架以 any 持有。
// 重复调用安全：复用同一套共享结构，新 hub 覆盖旧值。
func (m *Manager) BuildContext(hub any) error {
	if m.mux == nil {
		m.mux = http.NewServeMux()
	}
	if m.routes == nil {
		m.routes = newRouteTable()
	}
	if m.shared == nil {
		m.shared = &sync.Map{}
	}
	m.hub = hub
	// 把管理器自身放进共享 KV：plugininfo 通过 ManagerFromContext 取用。
	// 框架仍然不知道 plugininfo 的存在（依赖方向保持 plugin ← plugins）。
	m.shared.Store(sharedKeyManager, m)
	// V5：有插件实现了 LogSink 就接管 log 输出（额外副本，原落盘行为不变）。
	m.installLogFanout()
	return nil
}

// Routes 导出当前已注册的路由清单（按 Pattern 升序），含归属插件名。
// 供控制台的插件视图与排障使用。
func (m *Manager) Routes() []RouteInfo {
	return m.routes.list()
}

// Mux 返回插件共享的 *http.ServeMux（已注册全部插件路由，含启停闸门）。
// 主要供测试与自定义服务器装配使用；生产由 httpgateway 插件持有并对外服务。
// BuildContext 之前调用会返回 nil。
func (m *Manager) Mux() *http.ServeMux {
	return m.mux
}

// routesOf 返回某插件注册的路由 pattern（按字典序）。
func (m *Manager) routesOf(name string) []string {
	return m.routes.ofOwner(name)
}

// isEnabled 返回插件是否启用：显式设置优先，否则默认启用（兼容 V2「全量启」语义）。
func (m *Manager) isEnabled(p Plugin) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.enabled[p.Meta().Name]; ok {
		return v
	}
	return true
}

// optionsFor 返回某插件的合并后配置项（缺省为空 map）。
func (m *Manager) optionsFor(p Plugin) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if o, ok := m.options[p.Meta().Name]; ok {
		return o
	}
	return map[string]any{}
}

// OptionsOf 返回某插件当前生效的配置项副本（供设置端点展示）。
func (m *Manager) OptionsOf(name string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]any)
	for k, v := range m.options[name] {
		out[k] = v
	}
	return out
}

// SetOptions 覆盖某插件的内存配置项（不触发插件回调）。
func (m *Manager) SetOptions(name string, options map[string]any) {
	cp := make(map[string]any, len(options))
	for k, v := range options {
		cp[k] = v
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.options[name] = cp
}

// ApplyOptions 更新内存配置，并对实现了 Applier 的插件立即热应用。
// 返回 nil 表示已热生效；返回 error 表示该插件不支持热更新或热更新失败（调用方据此标注 pendingRestart）。
func (m *Manager) ApplyOptions(name string, options map[string]any) error {
	p, ok := m.byName[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
	}
	m.SetOptions(name, options)
	applier, ok := p.(Applier)
	if !ok {
		return fmt.Errorf("plugin %q does not implement plugin.Applier", name)
	}
	return applier.Apply(options)
}

// Schemas 返回所有声明了 Options() 的插件的配置声明（Name → []OptionDef）。
// 控制台的设置视图完全由此驱动——新增插件零前端改动即可出现表单。
func (m *Manager) Schemas() map[string][]OptionDef {
	out := make(map[string][]OptionDef, len(m.plugins))
	for _, p := range m.plugins {
		if o, ok := p.(Optioner); ok {
			defs := o.Options()
			if len(defs) > 0 {
				out[p.Meta().Name] = append([]OptionDef(nil), defs...)
			}
		}
	}
	return out
}

// DefaultOptions 返回各插件的默认选项（用于设置页的「恢复默认」与模板生成）。
func (m *Manager) DefaultOptions() map[string]map[string]any {
	out := make(map[string]map[string]any)
	for _, p := range m.plugins {
		o, ok := p.(Optioner)
		if !ok {
			continue
		}
		opts := map[string]any{}
		for _, d := range o.Options() {
			if d.Default != nil {
				opts[d.Key] = d.Default
			}
		}
		if len(opts) > 0 {
			out[p.Meta().Name] = opts
		}
	}
	return out
}

// names 返回全部插件名（升序）。
func (m *Manager) names() []string {
	out := make([]string, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.Meta().Name)
	}
	return out
}

// stateOf 返回插件当前状态（不存在时返回 StateRegistered）。
func (m *Manager) stateOf(name string) State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if in, ok := m.infos[name]; ok {
		return in.State
	}
	return StateRegistered
}

// errorOf 返回插件当前的错误文本。
func (m *Manager) errorOf(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if in, ok := m.infos[name]; ok {
		return in.Error
	}
	return ""
}

// snapshotInfo 返回单个插件的运行时信息副本。
func (m *Manager) snapshotInfo(name string) Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if in, ok := m.infos[name]; ok {
		return *in
	}
	return Info{}
}

// setState 变更插件状态：更新状态、错误、时间戳、状态历史，并同步路由闸门。
// 这是唯一的「状态写入口」，保证历史与闸门永不与状态脱节。
func (m *Manager) setState(name string, st State, errMsg string) {
	now := time.Now().UnixMilli()
	m.mu.Lock()
	in, ok := m.infos[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	from := in.State
	in.State = st
	in.Error = errMsg
	in.StateChangedAt = now
	h := append(m.history[name], StateEvent{At: now, From: from, To: st, Error: errMsg})
	if len(h) > stateHistorySize {
		h = h[len(h)-stateHistorySize:]
	}
	m.history[name] = h
	g := m.gates[name]
	m.mu.Unlock()

	// 闸门与状态同步：装配完成（initialized）或运行中才放行路由。
	g.set(stateGateOn(st))

	// V5：发布 plugin.state（在解锁之后发布，订阅方可以自由回读管理器快照）。
	// 状态机是插件生命周期的唯一事实来源，因此这里是**唯一**的发布点——
	// 启停 / 重载 / 失败 / 回滚都会自动带上事件，不会漏。
	ev := StateEventPayload{Name: name, State: st, Prev: from, Error: errMsg, At: now}
	m.publish(TopicState, ev)
	// 细分 topic 与聚合 topic 同时发布：TopicState 覆盖每一次迁移（控制台用），
	// TopicStarted/Stopped/Failed 只表达"进入某态"（给需要精确语义的订阅者用）。
	// 两者都不改变任何既有行为，只是新增事件源——死契约检查因此可以是空黑名单。
	switch st {
	case StateRunning:
		m.publish(TopicStarted, ev)
	case StateStopped:
		m.publish(TopicStopped, ev)
	case StateFailed:
		m.publish(TopicFailed, ev)
	}
}

// markEnabledAt 记录「启用时间」（Init 成功即刻）。
func (m *Manager) markEnabledAt(name string, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.infos[name]; ok {
		in.EnabledAt = at
	}
}

// markStartedAt 记录「启动时间」。
func (m *Manager) markStartedAt(name string, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.infos[name]; ok {
		in.StartedAt = at
	}
}

// markStoppedAt 记录「停止时间」。
func (m *Manager) markStoppedAt(name string, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.infos[name]; ok {
		in.StoppedAt = at
	}
}

// bumpRestart 递增启停次数并刷新状态变更时间（成功的启用/停用/重载各计一次）。
func (m *Manager) bumpRestart(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.infos[name]; ok {
		in.RestartCount++
		in.StateChangedAt = time.Now().UnixMilli()
	}
}

// setGate 切换某插件的路由闸门。
func (m *Manager) setGate(name string, on bool) {
	m.mu.RLock()
	g := m.gates[name]
	m.mu.RUnlock()
	g.set(on)
}

// History 返回某插件的状态变更历史（时间正序，最多 20 条）。
func (m *Manager) History(name string) []StateEvent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.history[name]
	out := make([]StateEvent, len(src))
	copy(out, src)
	return out
}

// Dependents 返回声明依赖 name 的全部插件名（无论是否启用，按字典序）。
func (m *Manager) Dependents(name string) []string {
	var out []string
	for _, p := range m.plugins {
		q := p.Meta().Name
		if q == name {
			continue
		}
		for _, d := range p.Meta().Dependencies {
			if d == name {
				out = append(out, q)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// activeDependents 返回「当前仍在使用 name」的依赖方（启用中且已装配/运行/失败待修）。
func (m *Manager) activeDependents(name string) []string {
	var out []string
	for _, p := range m.plugins {
		q := p.Meta().Name
		if q == name || !m.isEnabled(p) {
			continue
		}
		depends := false
		for _, d := range p.Meta().Dependencies {
			if d == name {
				depends = true
				break
			}
		}
		if !depends {
			continue
		}
		switch m.stateOf(q) {
		case StateInitialized, StateRunning, StateFailed:
			out = append(out, q)
		}
	}
	sort.Strings(out)
	return out
}

// order 计算拓扑排序结果。
// 返回：
//   - ordered：依赖就绪、可正常装配的插件 Name（按拓扑序，同层按 Name 字典序，确定性）；
//   - bad：无法装配的插件 Name → 人可读错误（缺失依赖 / 循环依赖 / 自依赖）。
//
// 依赖被记为「边 dep → plugin」：被依赖者先于依赖者处理（Kahn 入度为 0 者先出）。
func (m *Manager) order() (ordered []string, bad map[string]string) {
	bad = make(map[string]string)

	inDeg := make(map[string]int, len(m.plugins))
	dependents := make(map[string][]string) // dep Name → 依赖它的插件 Name
	for _, p := range m.plugins {
		inDeg[p.Meta().Name] = 0
	}

	// 建图：统计入度，识别缺失依赖与自依赖。
	for _, p := range m.plugins {
		name := p.Meta().Name
		for _, d := range p.Meta().Dependencies {
			if d == name {
				if bad[name] == "" {
					bad[name] = "depends on itself"
				}
				continue
			}
			if _, ok := m.byName[d]; !ok {
				if bad[name] == "" {
					bad[name] = fmt.Sprintf("depends on missing plugin %q", d)
				}
				continue
			}
			dependents[d] = append(dependents[d], name)
			inDeg[name]++
		}
	}

	// 把「依赖了不可满足插件」的插件也标记为 bad（级联），使错误定位更准。
	changed := true
	for changed {
		changed = false
		for _, p := range m.plugins {
			name := p.Meta().Name
			if bad[name] != "" {
				continue
			}
			for _, d := range p.Meta().Dependencies {
				if bad[d] != "" {
					bad[name] = fmt.Sprintf("dependency %q is unavailable", d)
					changed = true
					break
				}
			}
		}
	}

	// Kahn：入度为 0 且非 bad 的节点入队；同层按 Name 字典序，保证确定性。
	var ready []string
	for _, p := range m.plugins {
		if inDeg[p.Meta().Name] == 0 && bad[p.Meta().Name] == "" {
			ready = append(ready, p.Meta().Name)
		}
	}
	sort.Strings(ready)

	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		ordered = append(ordered, n)
		var next []string
		for _, dep := range dependents[n] {
			inDeg[dep]--
			if inDeg[dep] == 0 && bad[dep] == "" {
				next = append(next, dep)
			}
		}
		sort.Strings(next)
		ready = append(ready, next...)
		sort.Strings(ready)
	}

	// 剩余入度 > 0（且非 bad）的节点即处于循环依赖中。
	for _, p := range m.plugins {
		if bad[p.Meta().Name] == "" && inDeg[p.Meta().Name] > 0 {
			bad[p.Meta().Name] = "circular dependency detected"
		}
	}
	return ordered, bad
}

// InitAll 纯装配：按拓扑序调用各插件 Init，不起协程。
// 缺失依赖 / 循环依赖 / 依赖失败的插件直接标记为 Failed，不影响其它插件。
func (m *Manager) InitAll() {
	ordered, bad := m.order()
	for _, name := range ordered {
		p := m.byName[name]
		if !m.isEnabled(p) {
			m.setState(name, StateDisabled, "")
			continue
		}
		if bad[name] != "" {
			m.setState(name, StateFailed, bad[name])
			continue
		}
		// 依赖已就绪（拓扑序保证先处理），但若某依赖因 panic/错误而 Failed，则本插件也 Failed。
		if failedDep := m.firstFailedDependency(p); failedDep != "" {
			m.setState(name, StateFailed, fmt.Sprintf("dependency %q failed or unavailable", failedDep))
			continue
		}
		m.safeInit(p)
	}
	// 未被拓扑序覆盖的坏插件同样标记为 Failed。
	for _, p := range m.plugins {
		if bad[p.Meta().Name] != "" {
			m.setState(p.Meta().Name, StateFailed, bad[p.Meta().Name])
		}
	}
}

// firstFailedDependency 返回 p 的第一个处于 Failed 状态的依赖（拓扑序下依赖已先处理）。
func (m *Manager) firstFailedDependency(p Plugin) string {
	for _, d := range p.Meta().Dependencies {
		m.mu.RLock()
		di, ok := m.infos[d]
		failed := !ok || di.State == StateFailed
		m.mu.RUnlock()
		if failed {
			return d
		}
	}
	return ""
}

// StartAll 一次性启动全部插件，返回有序结果表。
// 若某个 Required 插件被禁用（白名单/CLI 排除），整体启动失败并返回明确错误。
func (m *Manager) StartAll() ([]Result, error) {
	for _, p := range m.plugins {
		if p.Meta().Required && !m.isEnabled(p) {
			reason := p.Meta().RequiredReason
			if reason == "" {
				reason = "它是必需插件，必须启用"
			}
			return nil, fmt.Errorf("缺少必需插件 %q，%s", p.Meta().Name, reason)
		}
	}

	ordered, bad := m.order()
	var results []Result

	for _, name := range ordered {
		p := m.byName[name]
		if !m.isEnabled(p) {
			m.setState(name, StateDisabled, "")
			continue
		}
		if bad[name] != "" {
			m.setState(name, StateFailed, bad[name])
			results = append(results, Result{Name: name, State: StateFailed, Error: m.errorOf(name)})
			continue
		}
		if failedDep := m.firstFailedDependency(p); failedDep != "" {
			msg := fmt.Sprintf("dependency %q failed or unavailable", failedDep)
			m.setState(name, StateFailed, msg)
			results = append(results, Result{Name: name, State: StateFailed, Error: msg})
			continue
		}
		// Init 阶段已失败的插件不再 Start：否则 safeStart 会把 StateFailed 覆盖为 Running，
		// 掩盖「它其实没装配好」（典型场景：路由冲突导致 Init 返回 error）。
		if st := m.stateOf(name); st == StateFailed {
			results = append(results, Result{Name: name, State: StateFailed, Error: m.errorOf(name)})
			continue
		}
		start := time.Now()
		m.safeStart(p)
		in := m.snapshotInfo(name)
		results = append(results, Result{
			Name:     name,
			State:    in.State,
			Duration: time.Since(start),
			Error:    in.Error,
		})
	}

	// 坏插件（缺失/循环依赖）补充进结果表，便于一张表看清全貌（按 Name 排序追加）。
	var broken []string
	for _, p := range m.plugins {
		if bad[p.Meta().Name] != "" {
			broken = append(broken, p.Meta().Name)
		}
	}
	sort.Strings(broken)
	for _, name := range broken {
		results = append(results, Result{Name: name, State: StateFailed, Error: m.errorOf(name)})
	}

	// 必需插件即使「被启用」也可能 Start 失败（典型：端口被占）。此时整体启动必须失败：
	// 否则会打印"启动完成"、继续等信号，而实际没有任何连接入口（静默僵尸启动）。
	for _, p := range m.plugins {
		if !p.Meta().Required || !m.isEnabled(p) {
			continue
		}
		name := p.Meta().Name
		if st := m.stateOf(name); st != StateRunning {
			return results, fmt.Errorf("必需插件 %q 启动失败（state=%s）: %s",
				name, st.String(), m.errorOf(name))
		}
	}

	return results, nil
}

// StopAll 逆序停止所有处于 Running 的插件（panic 同样被隔离）。
func (m *Manager) StopAll() {
	ordered, _ := m.order()
	for i := len(ordered) - 1; i >= 0; i-- {
		name := ordered[i]
		p := m.byName[name]
		if !m.isEnabled(p) || m.stateOf(name) != StateRunning {
			continue
		}
		m.safeStop(p)
	}
}

// Reload 逐个插件重新装配（Stop → Init → Start，不重启进程），返回每个插件的结果。
// 用于「改了配置但插件没有实现 Applier」的场景：一次调用让所有插件读到新配置。
func (m *Manager) Reload() []Result {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	ordered, bad := m.order()
	results := make([]Result, 0, len(ordered))
	for _, name := range ordered {
		p := m.byName[name]
		if !m.isEnabled(p) {
			continue
		}
		if bad[name] != "" {
			m.setState(name, StateFailed, bad[name])
			results = append(results, Result{Name: name, State: StateFailed, Error: bad[name]})
			continue
		}
		start := time.Now()
		// 显式 Reload 是解除「Stop 超时阻断」的唯一入口（F5）：
		// 用户/运维主动要求重新装配时，才允许再碰那个可能仍有协程在跑的实例。
		m.clearBlocked(name)
		// 承载对外服务的插件（如 httpgateway）拒绝在请求处理过程中自我重启。
		if skipper, ok := p.(ReloadSkipper); ok {
			if skip, reason := skipper.SkipReload(); skip {
				in := m.snapshotInfo(name)
				results = append(results, Result{Name: name, State: in.State, Skipped: true, Reason: reason})
				continue
			}
		}
		if m.stateOf(name) == StateRunning {
			m.setGate(name, false) // 重载期间端点暂不可用，避免访问到半装配的 handler
			m.safeStop(p)
		}
		m.safeInit(p)
		if m.stateOf(name) == StateInitialized {
			m.safeStart(p)
		}
		m.bumpRestart(name)
		in := m.snapshotInfo(name)
		results = append(results, Result{
			Name:     name,
			State:    in.State,
			Duration: time.Since(start),
			Error:    in.Error,
		})
	}
	return results
}

// ---------------------------------------------------------------------------
// 运行期启停（V4 ①）：依赖保护 / Required 保护 / 顺序保证 / 失败回滚
// ---------------------------------------------------------------------------

// managerSnapshot 是启停操作前的状态快照，用于失败回滚。
//
// 除 State/Error 外还保存四个时间戳与 RestartCount（F4c）：
// 只回滚状态而不回滚这些字段，会让「失败的启用」在 /api/plugins 上表现为
// 「刚刚启用过一次」（enabledAt/restartCount 被推进），与「什么都没发生」自相矛盾。
type managerSnapshot struct {
	at      int64           // 快照时刻（毫秒）：回滚时用它标记「本次操作新增的历史条目」
	enabled map[string]bool // Name → 启用状态
	infos   map[string]Info // Name → 完整运行时信息副本
}

// takeSnapshot 记录当前启用状态与各插件运行时信息。
func (m *Manager) takeSnapshot() managerSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := managerSnapshot{
		at:      time.Now().UnixMilli(),
		enabled: make(map[string]bool, len(m.enabled)),
		infos:   make(map[string]Info, len(m.infos)),
	}
	for k, v := range m.enabled {
		s.enabled[k] = v
	}
	for k, in := range m.infos {
		s.infos[k] = *in
	}
	return s
}

// rollback 恢复到操作前的状态。
//
// 参数 attempted 是「本次操作中**曾进入 Init** 的插件」（按执行顺序）。
// 逆序对它们逐个 safeStop：包括 Start 失败、甚至 Init 失败的那个——
// 它的 Init 可能已经开过 socket / 建过协程，只改状态等于泄漏（F4a）。
// 回滚过程中的 Stop 失败只记日志，绝不阻断回滚（否则一次坏插件会拖住整次操作）。
func (m *Manager) rollback(snap managerSnapshot, attempted []string) {
	for i := len(attempted) - 1; i >= 0; i-- {
		name := attempted[i]
		p, ok := m.byName[name]
		if !ok {
			continue
		}
		switch m.stateOf(name) {
		case StateRunning, StateInitialized, StateFailed:
			m.setGate(name, false)
			m.safeStop(p)
		}
	}

	m.mu.Lock()
	for name, on := range snap.enabled {
		m.enabled[name] = on
	}
	for name, old := range snap.infos {
		in, ok := m.infos[name]
		if !ok {
			continue
		}
		// 状态回滚：State/Error 与四个时间戳、启停次数一并还原（F4c）。
		in.State = old.State
		in.Error = old.Error
		in.StateChangedAt = old.StateChangedAt
		in.EnabledAt = old.EnabledAt
		in.StartedAt = old.StartedAt
		in.StoppedAt = old.StoppedAt
		in.RestartCount = old.RestartCount
		// 历史保留但打上 rolledBack 标记：排障能看到"尝试过什么"，
		// 又不会把中间态误读成当前状态。
		hist := m.history[name]
		for i := range hist {
			if hist[i].At >= snap.at {
				hist[i].RolledBack = true
			}
		}
		m.history[name] = hist
		m.gates[name].set(stateGateOn(old.State))
	}
	m.mu.Unlock()
}

// setErrorKeepState 只更新错误文案（不改状态），并追加一条 From=To 的历史条目。
//
// 用途（F4b）：Disable 的 Stop 失败后状态要回滚为 running，但"回滚"不等于"没出过事"——
// 错误必须留在 Error 里，否则 /api/plugins 会显示一个健康的 running 插件，
// 而它的 listener 其实已经关掉了（用户拿不到任何线索）。
func (m *Manager) setErrorKeepState(name, msg string) {
	now := time.Now().UnixMilli()
	m.mu.Lock()
	in, ok := m.infos[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	// 只写 Error，**不**动 StateChangedAt：状态并没有变（From=To），
	// 推进"状态变更时间"会让回滚看起来像一次真实的迁移（F4c 要求该字段保持操作前的值）。
	in.Error = msg
	h := append(m.history[name], StateEvent{At: now, From: in.State, To: in.State, Error: msg, RolledBack: true})
	if len(h) > stateHistorySize {
		h = h[len(h)-stateHistorySize:]
	}
	m.history[name] = h
	m.mu.Unlock()
}

// ready 判断插件是否「已启用且已装配就绪」。
func (m *Manager) ready(name string) bool {
	p, ok := m.byName[name]
	if !ok || !m.isEnabled(p) {
		return false
	}
	st := m.stateOf(name)
	return st == StateInitialized || st == StateRunning
}

// enablePlan 计算「启用 name」需要按顺序动工的最小插件集合（正拓扑序，含未就绪的依赖）。
func (m *Manager) enablePlan(name string) ([]string, error) {
	var plan []string
	seen := make(map[string]bool, len(m.plugins))

	var visit func(n string) error
	visit = func(n string) error {
		if seen[n] {
			return nil
		}
		p, ok := m.byName[n]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownPlugin, n)
		}
		seen[n] = true
		deps := append([]string(nil), p.Meta().Dependencies...)
		sort.Strings(deps)
		for _, d := range deps {
			if d == n {
				return fmt.Errorf("插件 %q 依赖了自身，无法启用", n)
			}
			if err := visit(d); err != nil {
				return err
			}
		}
		if !m.ready(n) {
			plan = append(plan, n)
		}
		return nil
	}
	if err := visit(name); err != nil {
		return nil, err
	}
	return plan, nil
}

// initAndStart 对单个插件执行「启用 → Init → Start」，任一步失败即返回错误。
func (m *Manager) initAndStart(name string) error {
	p := m.byName[name]
	m.SetEnabled(name, true)
	m.safeInit(p)
	if st := m.stateOf(name); st != StateInitialized {
		return fmt.Errorf("插件 %q 启用失败（Init，state=%s）: %s", name, st.String(), m.errorOf(name))
	}
	m.safeStart(p)
	if st := m.stateOf(name); st != StateRunning {
		return fmt.Errorf("插件 %q 启用失败（Start，state=%s）: %s", name, st.String(), m.errorOf(name))
	}
	return nil
}

// Enable 运行期启用插件。
//
// 顺序保证：按正拓扑序先补齐依赖（依赖未启用/未就绪则一并启用）；
// 失败回滚：任一步失败即把本次启动的插件停掉并恢复到操作前状态（风险 V4-1）。
func (m *Manager) Enable(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if _, ok := m.byName[name]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
	}
	if m.ready(name) {
		return fmt.Errorf("%w: %q", ErrAlreadyEnabled, name)
	}
	if _, bad := m.order(); bad[name] != "" {
		return fmt.Errorf("插件 %q 无法装配：%s", name, bad[name])
	}
	plan, err := m.enablePlan(name)
	if err != nil {
		return err
	}

	snap := m.takeSnapshot()
	// attempted 记录「本次操作中凡进入过 Init 的插件」：回滚时对它们逐个 safeStop，
	// 包括最后那个 Start 失败的（它的 Init 可能已占用 socket/协程，只改状态等于泄漏）。
	attempted := make([]string, 0, len(plan))
	for _, n := range plan {
		attempted = append(attempted, n)
		if err := m.initAndStart(n); err != nil {
			m.rollback(snap, attempted)
			// 回滚把状态还原了，但"为什么失败"必须留在 Error 里——否则 /api/plugins
			// 会显示一个"什么都没发生"的插件，运维只能去翻日志。
			m.setErrorKeepState(n, fmt.Sprintf("启用失败（已回滚到操作前状态）: %s", err.Error()))
			return err
		}
	}
	for _, n := range plan {
		m.bumpRestart(n)
	}
	m.logRuntime("enable", name, plan)
	return nil
}

// Disable 运行期停用插件。
//
// 三条保护缺一不可：Required 不可停用；被运行中插件依赖则拒绝并点名依赖方；
// 停用后其路由立即 404（闸门），但插件实例与路由注册保留，以便随时再启用。
func (m *Manager) Disable(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	p, ok := m.byName[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPlugin, name)
	}
	if p.Meta().Required {
		reason := p.Meta().RequiredReason
		if reason == "" {
			reason = "它是必需插件，必须启用"
		}
		return fmt.Errorf("%w: 插件 %q 是必需插件，不可停用（%s）", ErrRequiredPlugin, name, reason)
	}
	if !m.isEnabled(p) {
		return fmt.Errorf("%w: %q", ErrAlreadyDisabled, name)
	}
	if deps := m.activeDependents(name); len(deps) > 0 {
		return fmt.Errorf("%w: 插件 %q 正被 %s 依赖，请先停用它们",
			ErrDependedOn, name, strings.Join(deps, "、"))
	}

	snap := m.takeSnapshot()
	m.setGate(name, false) // 先让端点 404，再停止插件：避免关闭过程中仍有请求打进来
	// F4d：StateInitialized 也要 Stop——Init 阶段可能已经占了资源（监听口、协程、临时文件）。
	switch m.stateOf(name) {
	case StateRunning, StateInitialized:
		m.safeStop(p)
		if m.stateOf(name) == StateFailed {
			stopErr := m.errorOf(name)
			// Stop **超时**是硬故障：那个 Stop 协程还在跑，回滚成 running 会撒谎
			// （"运行中"但端口可能已关，且下一次 Init 会与它并发操作同一实例）。
			// 因此不回滚状态，保持 failed 并依赖 F5 的阻断闸，等显式 Reload 解锁。
			if _, wedged := m.blockedReason(name); wedged {
				m.setErrorKeepState(name, fmt.Sprintf(
					"Stop 超时，插件已被阻断（执行 Reload 可解锁）: %s", stopErr))
				return fmt.Errorf("插件 %q 停用超时，已阻断重新装配（执行 Reload 可解锁）: %s", name, stopErr)
			}
			// 普通 Stop 失败：状态回滚为操作前（通常是 running），但错误必须保留——
			// 否则 /api/plugins 会显示 running 而端口已死，用户拿不到任何线索（F4b）。
			// attempted 传 nil：Stop 刚刚才失败过，回滚时**不要**再调一次 Stop。
			m.rollback(snap, nil)
			m.setErrorKeepState(name, fmt.Sprintf(
				"Stop 失败（状态已回滚为 %s，但资源可能未完全释放）: %s",
				snap.infos[name].State.String(), stopErr))
			return fmt.Errorf("插件 %q 停用失败（Stop）: %s", name, stopErr)
		}
	}
	m.SetEnabled(name, false)
	m.setState(name, StateDisabled, "")
	m.bumpRestart(name)
	m.logRuntime("disable", name, nil)
	return nil
}

// Infos 返回当前所有插件的运行时信息（按 Name 升序）。
func (m *Manager) Infos() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.plugins))
	for _, p := range m.plugins {
		if in, ok := m.infos[p.Meta().Name]; ok {
			out = append(out, *in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Snapshot 返回全部插件的运行时快照（含 Meta + State + Error + 时间戳 + RestartCount）。
// 与 Infos 同源，但语义上专供管理 API 使用（V4 ①）。
func (m *Manager) Snapshot() []Info {
	return m.Infos()
}

// Summary 返回停机摘要字符串：每行一个插件，含状态与耗时（已停止用 StoppedAt-StartedAt，
// 仍在运行用 time.Since(StartedAt)）。便于「双击 EXE / 终端」场景下快速排障。
func (m *Manager) Summary() string {
	infos := m.Infos()
	if len(infos) == 0 {
		return "(无插件)"
	}
	var b strings.Builder
	for i, info := range infos {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('[')
		b.WriteString(info.Name)
		b.WriteByte(']')
		b.WriteByte('=')
		b.WriteString(info.State.String())
		// 耗时
		var d time.Duration
		if info.StartedAt > 0 {
			if info.StoppedAt > 0 {
				d = time.Duration(info.StoppedAt-info.StartedAt) * time.Millisecond
			} else if info.State == StateRunning {
				d = time.Since(time.UnixMilli(info.StartedAt))
			}
		}
		if d > 0 {
			b.WriteByte('(')
			b.WriteString(d.Round(time.Millisecond).String())
			b.WriteByte(')')
		}
		if info.Error != "" {
			b.WriteByte(':')
			b.WriteString(info.Error)
		}
	}
	return b.String()
}

// newContextFor 为插件构造带共享 mux/路由表/闸门/KV/hub 的 Context。
// 若 BuildContext 未被调用（如单测），这里兜底建一套默认结构，保证 Context 可用。
func (m *Manager) newContextFor(p Plugin) *Context {
	if m.mux == nil {
		m.mux = http.NewServeMux()
	}
	if m.routes == nil {
		m.routes = newRouteTable()
	}
	if m.shared == nil {
		m.shared = &sync.Map{}
	}
	name := p.Meta().Name
	m.mu.RLock()
	g := m.gates[name]
	sink := m.sink
	m.mu.RUnlock()
	c := newContext(m.view, m.pub, m.optionsFor(p), name, m.mux, m.routes, m.shared, m.hub, g)
	c.sink = sink // 只读字段：BuildContext 后不再变，无需再加锁
	return c
}

// markBlocked 记录「该插件在 Stop 超时后不得再被装配」（F5）。
func (m *Manager) markBlocked(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blocked == nil {
		m.blocked = make(map[string]string)
	}
	m.blocked[name] = reason
}

// clearBlocked 解除阻断（仅显式 Reload 调用）。
func (m *Manager) clearBlocked(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blocked, name)
}

// blockedReason 返回阻断原因（未阻断时 ok=false）。
func (m *Manager) blockedReason(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.blocked[name]
	return r, ok
}

// safeInit 调用 Init 并隔离 panic；成功置 StateInitialized，失败置 StateFailed。
func (m *Manager) safeInit(p Plugin) {
	name := p.Meta().Name
	if reason, blocked := m.blockedReason(name); blocked {
		m.setState(name, StateFailed, fmt.Sprintf(
			"已阻断重新装配(%s)；请先执行一次 Reload(POST /api/plugins/reload) 再重试", reason))
		return
	}
	ctx := m.newContextFor(p)

	var initErr error
	var panicVal any
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicVal = r
			}
		}()
		initErr = p.Init(ctx)
	}()

	switch {
	case panicVal != nil:
		m.setState(name, StateFailed, fmt.Sprintf("Init panicked: %v", panicVal))
	case initErr != nil:
		m.setState(name, StateFailed, "Init failed: "+initErr.Error())
	default:
		m.setState(name, StateInitialized, "")
		m.markEnabledAt(name, time.Now().UnixMilli())
	}
}

// safeStart 调用 Start 并隔离 panic；成功置 StateRunning，失败置 StateFailed。
func (m *Manager) safeStart(p Plugin) {
	name := p.Meta().Name
	if reason, blocked := m.blockedReason(name); blocked {
		m.setState(name, StateFailed, fmt.Sprintf(
			"已阻断启动(%s)；请先执行一次 Reload(POST /api/plugins/reload) 再重试", reason))
		return
	}
	m.markStartedAt(name, time.Now().UnixMilli())

	var startErr error
	var panicVal any
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicVal = r
			}
		}()
		startErr = p.Start()
	}()

	switch {
	case panicVal != nil:
		m.setState(name, StateFailed, fmt.Sprintf("Start panicked: %v", panicVal))
	case startErr != nil:
		m.setState(name, StateFailed, "Start failed: "+startErr.Error())
	default:
		m.setState(name, StateRunning, "")
	}
}

// stopTimeout 是「单个插件 Stop」的硬超时：超时记录为 Failed，但绝不阻塞整体停机。
// 内置插件的 Stop 自身也已内建 5s 超时（tcpgateway 等待 Accept 退出、sweeper 等待循环退出、
// httpgateway 的 Shutdown），这里是框架层的兜底，防止第三方插件写出永不返回的 Stop。
// 声明为变量仅为便于单测缩短（生产不修改）。
var stopTimeout = 5 * time.Second

// safeStop 调用 Stop 并隔离 panic / 卡死；成功置 StateStopped，失败或超时置 StateFailed。
func (m *Manager) safeStop(p Plugin) {
	name := p.Meta().Name
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("Stop panicked: %v", r)
			}
		}()
		done <- p.Stop()
	}()
	select {
	case err := <-done:
		if err != nil {
			m.setState(name, StateFailed, "Stop failed: "+err.Error())
			return
		}
		m.setState(name, StateStopped, "")
		m.markStoppedAt(name, time.Now().UnixMilli())
	case <-time.After(stopTimeout):
		// 超时不只是"记一笔"：那个 Stop 协程还在跑，若马上又 Init 同一个实例，
		// 两边会并发操作同一份 listener/协程状态（进程级单例）——必须阻断后续装配（F5）。
		m.setState(name, StateFailed,
			fmt.Sprintf("Stop timed out after %s（已跳过，未阻塞整体停机）", stopTimeout))
		m.markBlocked(name, fmt.Sprintf("上次 Stop 在 %s 内未返回", stopTimeout))
	}
}
