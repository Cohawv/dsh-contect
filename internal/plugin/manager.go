package plugin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result 记录单个插件在 StartAll 阶段的结果（有序输出，可直接打印成启动表）。
type Result struct {
	Name     string        // 插件名
	State    State         // 最终状态
	Duration time.Duration // 启动耗时
	Error    string        // 失败原因（成功时为空）
}

// Manager 负责插件的装配与生命周期编排。
//
// 本版本仅支持「全量启」：InitAll 纯装配（禁起协程），StartAll 一次性启动全部，
// StopAll 逆序停止。不支持运行期单独启停某个插件。
type Manager struct {
	plugins []Plugin            // 按 Name 升序
	byName  map[string]Plugin   // Name → 插件
	infos   map[string]*Info    // Name → 运行时信息
	enabled map[string]bool     // Name → 是否启用（缺省视为启用，即「全量启」）
	view    HubView             // 注入给插件的只读视图（V2 默认空实现）
	pub     Publisher           // 注入给插件的发布能力（V2 默认空实现）
	options map[string]map[string]any // Name → 合并后的配置项
	mux     *http.ServeMux      // 共享 mux：BuildContext 时建立
	routes  *sync.Map           // 影子路由表：pattern → 插件名（冲突预检）
	shared  *sync.Map           // 跨插件共享 KV
	hub     any                 // 业务内核实体（*core.Hub），框架侧为 any
}

// NewManager 从全局注册表构建管理器（生产用法）。
func NewManager() *Manager {
	return newManagerFrom(All())
}

// newManagerFrom 以显式插件列表构建管理器，供测试注入（避免依赖全局注册表状态）。
func newManagerFrom(plugins []Plugin) *Manager {
	m := &Manager{
		byName:  make(map[string]Plugin, len(plugins)),
		infos:   make(map[string]*Info, len(plugins)),
		enabled: make(map[string]bool),
		options: make(map[string]map[string]any),
	}
	// 按 Name 升序排列，保证后续一切顺序的确定性。
	sorted := make([]Plugin, len(plugins))
	copy(sorted, plugins)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Meta().Name < sorted[j].Meta().Name
	})
	for _, p := range sorted {
		m.plugins = append(m.plugins, p)
		m.byName[p.Meta().Name] = p
		m.infos[p.Meta().Name] = &Info{Meta: p.Meta(), State: StateRegistered}
	}
	return m
}

// SetEnabled 显式设置某插件的启用状态（覆盖默认）。
func (m *Manager) SetEnabled(name string, on bool) {
	m.enabled[name] = on
}

// SetHub 注入内核能力（HubView / Publisher）。传 nil 则保留默认空实现。
// V2 未接入真实内核，调用方通常省略；V3 由装配代码传入真实实现。
func (m *Manager) SetHub(view HubView, pub Publisher) {
	if view != nil {
		m.view = view
	}
	if pub != nil {
		m.pub = pub
	}
}

// ApplyConfig 应用 LoadConfig 的结果（启用状态 + 每插件配置项）。
func (m *Manager) ApplyConfig(enabled map[string]bool, options map[string]map[string]any) {
	for k, v := range enabled {
		m.enabled[k] = v
	}
	for k, v := range options {
		m.options[k] = v
	}
}

// BuildContext 建立共享 mux、影子路由表与跨插件 KV，并注入业务内核实体。
// 必须在 InitAll 之前调用；hub 通常为 *core.Hub，框架以 any 持有。
// 重复调用安全：复用同一套共享结构，新 hub 覆盖旧值。
func (m *Manager) BuildContext(hub any) error {
	if m.mux == nil {
		m.mux = http.NewServeMux()
	}
	if m.routes == nil {
		m.routes = &sync.Map{}
	}
	if m.shared == nil {
		m.shared = &sync.Map{}
	}
	m.hub = hub
	return nil
}

// Routes 导出当前已注册的路由清单（pattern → 拥有者插件名），
// 供 V4 的插件视图 / 排障使用。
func (m *Manager) Routes() map[string]string {
	out := make(map[string]string)
	if m.routes != nil {
		m.routes.Range(func(k, v any) bool {
			if ks, ok := k.(string); ok {
				if vs, ok := v.(string); ok {
					out[ks] = vs
				}
			}
			return true
		})
	}
	return out
}

// isEnabled 返回插件是否启用：显式设置优先，否则默认启用（V2「全量启」）。
func (m *Manager) isEnabled(p Plugin) bool {
	if v, ok := m.enabled[p.Meta().Name]; ok {
		return v
	}
	return true
}

// optionsFor 返回某插件的合并后配置项（缺省为空 map）。
func (m *Manager) optionsFor(p Plugin) map[string]any {
	if o, ok := m.options[p.Meta().Name]; ok {
		return o
	}
	return map[string]any{}
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
		info := m.infos[name]
		if !m.isEnabled(p) {
			info.State = StateDisabled
			continue
		}
		if bad[name] != "" {
			info.State = StateFailed
			info.Error = bad[name]
			continue
		}
		// 依赖已就绪（拓扑序保证先处理），但若某依赖因 panic/错误而 Failed，则本插件也 Failed。
		if failedDep := m.firstFailedDependency(p); failedDep != "" {
			info.State = StateFailed
			info.Error = fmt.Sprintf("dependency %q failed or unavailable", failedDep)
			continue
		}
		m.safeInit(p)
	}
	// 未被拓扑序覆盖的坏插件同样标记为 Failed。
	for _, p := range m.plugins {
		if bad[p.Meta().Name] != "" {
			info := m.infos[p.Meta().Name]
			info.State = StateFailed
			info.Error = bad[p.Meta().Name]
		}
	}
}

// firstFailedDependency 返回 p 的第一个处于 Failed 状态的依赖（拓扑序下依赖已先处理）。
func (m *Manager) firstFailedDependency(p Plugin) string {
	for _, d := range p.Meta().Dependencies {
		di, ok := m.infos[d]
		if !ok || di.State == StateFailed {
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
		info := m.infos[name]
		if !m.isEnabled(p) {
			info.State = StateDisabled
			continue
		}
		if bad[name] != "" {
			info.State = StateFailed
			info.Error = bad[name]
			results = append(results, Result{Name: name, State: StateFailed, Error: info.Error})
			continue
		}
		if failedDep := m.firstFailedDependency(p); failedDep != "" {
			info.State = StateFailed
			info.Error = fmt.Sprintf("dependency %q failed or unavailable", failedDep)
			results = append(results, Result{Name: name, State: StateFailed, Error: info.Error})
			continue
		}
		// Init 阶段已失败的插件不再 Start：否则 safeStart 会把 StateFailed 覆盖为 Running，
		// 掩盖「它其实没装配好」（典型场景：路由冲突导致 Init 返回 error）。
		if info.State == StateFailed {
			results = append(results, Result{Name: name, State: StateFailed, Error: info.Error})
			continue
		}
		start := time.Now()
		m.safeStart(p)
		results = append(results, Result{
			Name:     name,
			State:    info.State,
			Duration: time.Since(start),
			Error:    info.Error,
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
		info := m.infos[name]
		results = append(results, Result{Name: name, State: StateFailed, Error: info.Error})
	}

	// 必需插件即使「被启用」也可能 Start 失败（典型：端口被占）。此时整体启动必须失败：
	// 否则会打印"启动完成"、继续等信号，而实际没有任何连接入口（静默僵尸启动）。
	// V2 是直接 log.Fatalf 退出，这里保持等价语义。
	for _, p := range m.plugins {
		if !p.Meta().Required || !m.isEnabled(p) {
			continue
		}
		info := m.infos[p.Meta().Name]
		if info.State != StateRunning {
			return results, fmt.Errorf("必需插件 %q 启动失败（state=%s）: %s",
				p.Meta().Name, info.State.String(), info.Error)
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
		info := m.infos[name]
		if !m.isEnabled(p) || info.State != StateRunning {
			continue
		}
		m.safeStop(p)
	}
}

// safeInit 调用 Init 并隔离 panic；成功置 StateInitialized，失败置 StateFailed。
func (m *Manager) safeInit(p Plugin) {
	info := m.infos[p.Meta().Name]
	ctx := m.newContextFor(p)
	defer func() {
		if r := recover(); r != nil {
			info.State = StateFailed
			info.Error = fmt.Sprintf("Init panicked: %v", r)
		}
	}()
	if err := p.Init(ctx); err != nil {
		info.State = StateFailed
		info.Error = "Init failed: " + err.Error()
		return
	}
	info.State = StateInitialized
	info.EnabledAt = time.Now().UnixMilli()
}

// newContextFor 为插件构造带共享 mux/路由表/KV/hub 的 Context。
// 若 BuildContext 未被调用（如单测），这里兜底建一套默认结构，保证 Context 可用。
func (m *Manager) newContextFor(p Plugin) *Context {
	if m.mux == nil {
		m.mux = http.NewServeMux()
	}
	if m.routes == nil {
		m.routes = &sync.Map{}
	}
	if m.shared == nil {
		m.shared = &sync.Map{}
	}
	return newContext(m.view, m.pub, m.optionsFor(p), p.Meta().Name, m.mux, m.routes, m.shared, m.hub)
}

// safeStart 调用 Start 并隔离 panic；成功置 StateRunning，失败置 StateFailed。
func (m *Manager) safeStart(p Plugin) {
	info := m.infos[p.Meta().Name]
	info.StartedAt = time.Now().UnixMilli()
	defer func() {
		if r := recover(); r != nil {
			info.State = StateFailed
			info.Error = fmt.Sprintf("Start panicked: %v", r)
		}
	}()
	if err := p.Start(); err != nil {
		info.State = StateFailed
		info.Error = "Start failed: " + err.Error()
		return
	}
	info.State = StateRunning
}

// stopTimeout 是「单个插件 Stop」的硬超时：超时记录为 Failed，但绝不阻塞整体停机。
// 内置插件的 Stop 自身也已内建 5s 超时（tcpgateway 等待 Accept 退出、sweeper 等待循环退出、
// httpgateway 的 Shutdown），这里是框架层的兜底，防止第三方插件写出永不返回的 Stop。
// 声明为变量仅为便于单测缩短（生产不修改）。
var stopTimeout = 5 * time.Second

// safeStop 调用 Stop 并隔离 panic / 卡死；成功置 StateStopped，失败或超时置 StateFailed。
func (m *Manager) safeStop(p Plugin) {
	info := m.infos[p.Meta().Name]
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
			info.State = StateFailed
			info.Error = "Stop failed: " + err.Error()
			return
		}
		info.State = StateStopped
		info.StoppedAt = time.Now().UnixMilli()
	case <-time.After(stopTimeout):
		info.State = StateFailed
		info.Error = fmt.Sprintf("Stop timed out after %s（已跳过，未阻塞整体停机）", stopTimeout)
	}
}

// Infos 返回当前所有插件的运行时信息（按 Name 升序）。
func (m *Manager) Infos() []Info {
	out := make([]Info, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, *m.infos[p.Meta().Name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
