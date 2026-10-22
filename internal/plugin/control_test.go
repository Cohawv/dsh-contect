package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// richPlugin 是控制面测试替身：可注册路由、可声明 5 种类型的 Options、可选实现 Applier。
type richPlugin struct {
	meta     Meta
	patterns []string
	initErr  error
	startErr error
	stopErr  error

	mu       sync.Mutex
	starts   int
	stops    int
	applied  map[string]any
	applyErr error
	applies  int
}

func (p *richPlugin) Meta() Meta { return p.meta }

func (p *richPlugin) Init(ctx *Context) error {
	for _, pattern := range p.patterns {
		name := p.meta.Name
		h := func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(name))
		}
		if err := ctx.RegisterHTTP(pattern, h); err != nil {
			return err
		}
	}
	return p.initErr
}

func (p *richPlugin) Start() error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	return p.startErr
}

func (p *richPlugin) Stop() error {
	p.mu.Lock()
	p.stops++
	p.mu.Unlock()
	return p.stopErr
}

// Options 覆盖全部 5 种类型，用于验证「schema 自动出现在设置端点」。
func (p *richPlugin) Options() []OptionDef {
	return []OptionDef{
		{Key: "label", Type: OptionString, Default: "hello", Description: "文本项"},
		{Key: "count", Type: OptionInt, Default: 3, Min: 1, Max: 10, Description: "数字项"},
		{Key: "enabled", Type: OptionBool, Default: true, Description: "开关项"},
		{Key: "interval", Type: OptionDuration, Default: "30s", Description: "时长项"},
		{Key: "mode", Type: OptionSelect, Default: "fast", Choices: []string{"fast", "slow"}, Description: "下拉项"},
	}
}

func (p *richPlugin) Apply(options map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applies++
	p.applied = options
	return p.applyErr
}

// plainPlugin 只实现最小 Plugin 接口（不声明 Options、不支持热更新）。
type plainPlugin struct {
	meta     Meta
	patterns []string
}

func (p *plainPlugin) Meta() Meta              { return p.meta }
func (p *plainPlugin) Init(ctx *Context) error { return nil }
func (p *plainPlugin) Start() error            { return nil }
func (p *plainPlugin) Stop() error             { return nil }

// optionsPlugin 声明 Options 但**不**实现 Applier（用于验证 pendingRestart 的诚实标注）。
type optionsPlugin struct {
	meta Meta
}

func (p *optionsPlugin) Meta() Meta          { return p.meta }
func (p *optionsPlugin) Init(*Context) error { return nil }
func (p *optionsPlugin) Start() error        { return nil }
func (p *optionsPlugin) Stop() error         { return nil }
func (p *optionsPlugin) Options() []OptionDef {
	return []OptionDef{{Key: "addr", Type: OptionString, Default: ":0"}}
}

// TestRoutesExportedAsSortedSlice 验证 Routes() 返回按 Pattern 升序的有序切片（V4 起）。
func TestRoutesExportedAsSortedSlice(t *testing.T) {
	a := &richPlugin{meta: Meta{Name: "alpha"}, patterns: []string{"/z", "/a"}}
	b := &richPlugin{meta: Meta{Name: "bravo"}, patterns: []string{"/m"}}
	m := newManagerFrom([]Plugin{a, b})
	m.BuildContext(nil)
	m.InitAll()

	routes := m.Routes()
	want := []RouteInfo{
		{Pattern: "/a", Plugin: "alpha"},
		{Pattern: "/m", Plugin: "bravo"},
		{Pattern: "/z", Plugin: "alpha"},
	}
	if len(routes) != len(want) {
		t.Fatalf("routes len = %d want %d (%+v)", len(routes), len(want), routes)
	}
	for i := range want {
		if routes[i] != want[i] {
			t.Fatalf("routes[%d] = %+v want %+v", i, routes[i], want[i])
		}
	}
}

// TestSchemaExposedFromOptioner 验证声明了 Options() 的插件会自动出现在 schema 里
// （这是「控制台零前端改动就能出现设置表单」的服务端前提）。
func TestSchemaExposedFromOptioner(t *testing.T) {
	rich := &richPlugin{meta: Meta{Name: "rich"}}
	plain := &plainPlugin{meta: Meta{Name: "plain"}}
	m := newManagerFrom([]Plugin{rich, plain})

	schemas := m.Schemas()
	defs, ok := schemas["rich"]
	if !ok || len(defs) != 5 {
		t.Fatalf("schema[rich] = %+v (存在=%v) want 5 个声明", defs, ok)
	}
	if _, ok := schemas["plain"]; ok {
		t.Fatal("未声明 Options() 的插件不应出现在 schema 中")
	}
	// 五种类型全覆盖（V4-③ 风险：类型渲染必须都覆盖）。
	types := map[OptionType]bool{}
	for _, d := range defs {
		types[d.Type] = true
	}
	for _, want := range []OptionType{OptionString, OptionInt, OptionBool, OptionDuration, OptionSelect} {
		if !types[want] {
			t.Fatalf("schema 缺少类型 %s（当前 %+v）", want, defs)
		}
	}
}

// TestDisableRequiredRejected 验证必需插件不可停用，且错误里带「为什么不能停」。
func TestDisableRequiredRejected(t *testing.T) {
	req := &richPlugin{meta: Meta{Name: "req", Required: true, DefaultOn: true, RequiredReason: "它是连接入口"}}
	m := newManagerFrom([]Plugin{req})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	err := m.Disable("req")
	if err == nil {
		t.Fatal("必需插件应拒绝停用")
	}
	if !errors.Is(err, ErrRequiredPlugin) {
		t.Fatalf("错误应为 ErrRequiredPlugin，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "它是连接入口") {
		t.Fatalf("错误 %q 未说明必需原因", err.Error())
	}
	if st := m.stateOf("req"); st != StateRunning {
		t.Fatalf("被拒绝后状态应保持 running，实际 %v", st)
	}
}

// TestDisableDependedOnRejected 验证「被运行中插件依赖」时拒绝停用，并点名依赖方。
func TestDisableDependedOnRejected(t *testing.T) {
	base := &richPlugin{meta: Meta{Name: "base", DefaultOn: true}}
	user := &richPlugin{meta: Meta{Name: "user", DefaultOn: true, Dependencies: []string{"base"}}}
	m := newManagerFrom([]Plugin{base, user})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	err := m.Disable("base")
	if err == nil {
		t.Fatal("被依赖插件应拒绝停用")
	}
	if !errors.Is(err, ErrDependedOn) {
		t.Fatalf("错误应为 ErrDependedOn，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "user") {
		t.Fatalf("错误 %q 未列出依赖方 user", err.Error())
	}
	t.Logf("依赖保护报错（点名依赖方）: %v", err)

	// 先停依赖方，再停被依赖者应当成功（逆拓扑序）。
	if err := m.Disable("user"); err != nil {
		t.Fatalf("停用依赖方失败: %v", err)
	}
	if err := m.Disable("base"); err != nil {
		t.Fatalf("依赖方停用后应可停用被依赖者: %v", err)
	}
}

// TestEnablePullsDependenciesInOrder 验证启用时的正拓扑序：先补齐依赖，再启用自己。
func TestEnablePullsDependenciesInOrder(t *testing.T) {
	var log []string
	a := &orderPlugin{meta: Meta{Name: "aaa"}, log: &log}
	b := &orderPlugin{meta: Meta{Name: "bbb", Dependencies: []string{"aaa"}}, log: &log}
	m := newManagerFrom([]Plugin{a, b})
	m.BuildContext(nil)
	m.SetEnabled("aaa", false)
	m.SetEnabled("bbb", false)
	m.InitAll()

	log = nil
	if err := m.Enable("bbb"); err != nil {
		t.Fatalf("Enable(bbb): %v", err)
	}
	want := []string{"init:aaa", "start:aaa", "init:bbb", "start:bbb"}
	if !equalStrings(log, want) {
		t.Fatalf("启用顺序 = %v want %v（依赖必须先就绪）", log, want)
	}
}

// TestEnableDisableStateErrors 验证重复启用/停用返回明确的哨兵错误（供 API 映射 409）。
func TestEnableDisableStateErrors(t *testing.T) {
	p := &richPlugin{meta: Meta{Name: "p", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if err := m.Enable("p"); !errors.Is(err, ErrAlreadyEnabled) {
		t.Fatalf("重复启用应返回 ErrAlreadyEnabled，实际 %v", err)
	}
	if err := m.Disable("p"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := m.Disable("p"); !errors.Is(err, ErrAlreadyDisabled) {
		t.Fatalf("重复停用应返回 ErrAlreadyDisabled，实际 %v", err)
	}
	if err := m.Enable("ghost"); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("未知插件应返回 ErrUnknownPlugin，实际 %v", err)
	}
}

// TestToggleTenTimesRestartCount 验证反复启停 10 次：
//   - RestartCount 精确等于 10（每次成功变更计一次）；
//   - 没有残留监听/协程增长（用 starts/stops 计数核对）；
//   - 状态与历史一致（历史环形缓冲不超过 20 条）。
func TestToggleTenTimesRestartCount(t *testing.T) {
	p := &richPlugin{meta: Meta{Name: "flip", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			if err := m.Disable("flip"); err != nil {
				t.Fatalf("第 %d 次停用失败: %v", i+1, err)
			}
		} else {
			if err := m.Enable("flip"); err != nil {
				t.Fatalf("第 %d 次启用失败: %v", i+1, err)
			}
		}
	}

	info := m.snapshotInfo("flip")
	if info.RestartCount != 10 {
		t.Fatalf("RestartCount = %d want 10（10 次成功启停）", info.RestartCount)
	}
	// 每次启动都必须配有对应的停止：starts-stops 只应等于「当前是否在运行」(1/0)，
	// 多出来的差值就说明有启不停 -> 协程/监听泄漏。
	p.mu.Lock()
	starts, stops := p.starts, p.stops
	p.mu.Unlock()
	wantRunning := 0
	if info.State == StateRunning {
		wantRunning = 1
	}
	if starts-stops != wantRunning {
		t.Fatalf("Start=%d Stop=%d 差值为 %d，期望 %d（说明有启停不配对/泄漏）",
			starts, stops, starts-stops, wantRunning)
	}
	hist := m.History("flip")
	if len(hist) > stateHistorySize {
		t.Fatalf("状态历史 %d 条，超过环形缓冲上限 %d", len(hist), stateHistorySize)
	}
	if len(hist) == 0 || hist[len(hist)-1].To != StateRunning {
		t.Fatalf("历史最后一条应为 running，实际 %+v", hist[len(hist)-1])
	}
}

// TestEnableFailureRollsBack 验证启用失败会回滚到操作前状态（风险 V4-1）。
func TestEnableFailureRollsBack(t *testing.T) {
	dep := &richPlugin{meta: Meta{Name: "dep", DefaultOn: false}}
	main := &richPlugin{meta: Meta{Name: "main", DefaultOn: false, Dependencies: []string{"dep"}},
		startErr: errors.New("port in use")}
	m := newManagerFrom([]Plugin{dep, main})
	m.BuildContext(nil)
	m.SetEnabled("dep", false)
	m.SetEnabled("main", false)
	m.InitAll()

	err := m.Enable("main")
	if err == nil {
		t.Fatal("Start 失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "port in use") {
		t.Fatalf("错误 %q 应包含底层原因", err.Error())
	}
	// 回滚：自动启用的依赖必须回到停用态，不能留下「半启」。
	if st := m.stateOf("dep"); st != StateDisabled {
		t.Fatalf("回滚后 dep 状态 = %v want disabled", st)
	}
	if st := m.stateOf("main"); st != StateDisabled {
		t.Fatalf("回滚后 main 状态 = %v want disabled", st)
	}
	dep.mu.Lock()
	depStarts := dep.starts
	dep.mu.Unlock()
	if depStarts == 0 {
		t.Log("注意：依赖确实被尝试启动过（这是预期的中间态）")
	}
}

// TestRuntimeDisableGatesRoutes 验证「停用插件后它的端点立即 404，而其它插件的端点仍可用」——
// 这正是 V4 验收里「停用 webconsole 后 / 变 404 但 /api/plugins 仍 200」的机制。
func TestRuntimeDisableGatesRoutes(t *testing.T) {
	other := &richPlugin{meta: Meta{Name: "keeper", DefaultOn: true}, patterns: []string{"/api/keeper"}}
	victim := &richPlugin{meta: Meta{Name: "victim", DefaultOn: true}, patterns: []string{"/"}}
	m := newManagerFrom([]Plugin{other, victim})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	if code := getStatus(t, srv.URL+"/"); code != http.StatusOK {
		t.Fatalf("停用前 / = %d want 200", code)
	}
	if err := m.Disable("victim"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if code := getStatus(t, srv.URL+"/"); code != http.StatusNotFound {
		t.Fatalf("停用后 / = %d want 404（路由闸门未生效）", code)
	}
	if code := getStatus(t, srv.URL+"/api/keeper"); code != http.StatusOK {
		t.Fatalf("其它插件端点 /api/keeper = %d want 200（不应受影响）", code)
	}
	// 重新启用：端点应恢复（同插件重复注册不得 panic，也不得报冲突）。
	if err := m.Enable("victim"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if code := getStatus(t, srv.URL+"/"); code != http.StatusOK {
		t.Fatalf("重新启用后 / = %d want 200", code)
	}
}

// getStatus 发一次 GET 并返回状态码。
func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestApplyOptionsCallsApplier 验证实现 Applier 的插件会被真正回调（热更新的唯一凭据）。
func TestApplyOptionsCallsApplier(t *testing.T) {
	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}
	m := newManagerFrom([]Plugin{rich})
	m.BuildContext(nil)

	opts := map[string]any{"interval": "5s"}
	if err := m.ApplyOptions("rich", opts); err != nil {
		t.Fatalf("ApplyOptions: %v", err)
	}
	rich.mu.Lock()
	applies, applied := rich.applies, rich.applied
	rich.mu.Unlock()
	if applies != 1 {
		t.Fatalf("Apply 调用次数 = %d want 1", applies)
	}
	if fmt.Sprint(applied["interval"]) != "5s" {
		t.Fatalf("Apply 收到的选项 = %v want interval=5s", applied)
	}
	if got := m.OptionsOf("rich")["interval"]; fmt.Sprint(got) != "5s" {
		t.Fatalf("OptionsOf 未更新: %v", got)
	}

	// 未实现 Applier 的插件：明确报错（调用方据此标注 pendingRestart），不静默成功。
	plain := &optionsPlugin{meta: Meta{Name: "plain"}}
	m2 := newManagerFrom([]Plugin{plain})
	m2.BuildContext(nil)
	if err := m2.ApplyOptions("plain", map[string]any{"addr": ":1"}); err == nil {
		t.Fatal("未实现 Applier 时应返回错误")
	}
}

// TestSnapshotIncludesRestartAndStateChange 验证 Snapshot 带 RestartCount 与 StateChangedAt。
func TestSnapshotIncludesRestartAndStateChange(t *testing.T) {
	p := &richPlugin{meta: Meta{Name: "p", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	before := m.snapshotInfo("p")
	if before.StateChangedAt == 0 {
		t.Fatal("StateChangedAt 应被记录（排障需要）")
	}
	if err := m.Disable("p"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	after := m.snapshotInfo("p")
	if after.StateChangedAt < before.StateChangedAt {
		t.Fatal("StateChangedAt 应随状态变更前进")
	}
	if after.RestartCount != 1 {
		t.Fatalf("RestartCount = %d want 1", after.RestartCount)
	}
	if snap := m.Snapshot(); len(snap) != 1 || snap[0].Name != "p" {
		t.Fatalf("Snapshot = %+v", snap)
	}
}

// startFailPlugin 的 Init 成功、Start 失败（典型：端口被占），并统计各钩子调用次数。
// 用于验证 F4a：回滚必须回收「已经跑过 Init」的插件，而不只是「已经 Start 成功」的。
type startFailPlugin struct {
	meta Meta

	mu     sync.Mutex
	inits  int
	starts int
	stops  int
}

func (p *startFailPlugin) Meta() Meta { return p.meta }
func (p *startFailPlugin) Init(*Context) error {
	p.mu.Lock()
	p.inits++
	p.mu.Unlock()
	return nil
}
func (p *startFailPlugin) Start() error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	return errors.New("bind: address already in use")
}
func (p *startFailPlugin) Stop() error {
	p.mu.Lock()
	p.stops++
	p.mu.Unlock()
	return nil
}

func (p *startFailPlugin) counts() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inits, p.starts, p.stops
}

// TestEnableFailureStopsInitializedPlugin 覆盖 F4a + F4c：
//   - Start 失败的那个插件（不在 "已启动" 列表里）也必须被 Stop，否则 Init 占的资源泄漏；
//   - 回滚要把 RestartCount / EnabledAt / StateChangedAt 一并还原（否则表现为"刚启用过"）；
//   - 历史保留但标 rolledBack，避免把中间态误读成现状；
//   - Error 保留失败原因（不是空）。
func TestEnableFailureStopsInitializedPlugin(t *testing.T) {
	p := &startFailPlugin{meta: Meta{Name: "flaky", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.SetEnabled("flaky", false)
	m.InitAll()

	before := m.snapshotInfo("flaky")

	err := m.Enable("flaky")
	if err == nil {
		t.Fatal("Start 失败时 Enable 必须返回错误")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("错误应包含底层原因，实际: %v", err)
	}

	inits, starts, stops := p.counts()
	if inits != 1 || starts != 1 {
		t.Fatalf("Init/Start 调用次数 = %d/%d want 1/1", inits, starts)
	}
	if stops == 0 {
		t.Fatal("Init 成功但 Start 失败的插件也必须被 Stop（否则 Init 占用的 socket/协程泄漏）")
	}

	after := m.snapshotInfo("flaky")
	if after.State != before.State {
		t.Fatalf("回滚后状态 = %v want %v", after.State, before.State)
	}
	if after.RestartCount != before.RestartCount {
		t.Fatalf("回滚后 RestartCount = %d want %d（失败的操作不该计次）", after.RestartCount, before.RestartCount)
	}
	if after.EnabledAt != before.EnabledAt {
		t.Fatalf("回滚后 EnabledAt = %d want %d（否则看起来像刚启用过）", after.EnabledAt, before.EnabledAt)
	}
	if after.StateChangedAt != before.StateChangedAt {
		t.Fatalf("回滚后 StateChangedAt = %d want %d", after.StateChangedAt, before.StateChangedAt)
	}
	if after.Error == "" || !strings.Contains(after.Error, "已回滚") {
		t.Fatalf("回滚后 Error 应保留失败原因，实际 %q", after.Error)
	}

	var rolled int
	for _, e := range m.History("flaky") {
		if e.RolledBack {
			rolled++
		}
	}
	if rolled == 0 {
		t.Fatal("回滚后历史条目必须标记 rolledBack（历史可保留，但不能让排障误读成现状）")
	}
}

// stopFailPlugin 的 Stop 返回错误（典型：listener 已半关闭）。
type stopFailPlugin struct {
	meta Meta

	mu    sync.Mutex
	stops int
}

func (p *stopFailPlugin) Meta() Meta          { return p.meta }
func (p *stopFailPlugin) Init(*Context) error { return nil }
func (p *stopFailPlugin) Start() error        { return nil }
func (p *stopFailPlugin) Stop() error {
	p.mu.Lock()
	p.stops++
	p.mu.Unlock()
	return errors.New("listener close failed")
}

// TestDisableStopFailureKeepsError 覆盖 F4b：Disable 的 Stop 失败后状态回滚为 running，
// 但 Error 必须保留原文案——否则 /api/plugins 会显示一个健康的 running 插件，
// 而它的端口其实已经死了，用户拿不到任何线索。
func TestDisableStopFailureKeepsError(t *testing.T) {
	p := &stopFailPlugin{meta: Meta{Name: "half", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	err := m.Disable("half")
	if err == nil {
		t.Fatal("Stop 失败时 Disable 必须返回错误")
	}
	in := m.snapshotInfo("half")
	if in.State != StateRunning {
		t.Fatalf("回滚后状态 = %v want running", in.State)
	}
	if in.Error == "" {
		t.Fatal("回滚不能清空错误：必须保留 Stop 失败原因（含半拆卸提示）")
	}
	if !strings.Contains(in.Error, "Stop") || !strings.Contains(in.Error, "listener close failed") {
		t.Fatalf("Error 应含 Stop 与底层原因，实际 %q", in.Error)
	}
	if !strings.Contains(in.Error, "已回滚") {
		t.Fatalf("Error 应说明状态已回滚（避免误判为仍在正常服务），实际 %q", in.Error)
	}
	// 历史里保留这次失败迁移（并标 rolledBack）。
	var sawFailure bool
	for _, e := range m.History("half") {
		if e.To == StateFailed {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatal("历史里应保留 Stop 失败的迁移记录")
	}
}

// TestDisableInitializedPluginStops 覆盖 F4d：对 StateInitialized（Init 过但未 Start）
// 的插件停用，也必须走 Stop——Init 阶段可能已经占了资源（监听口、协程、临时文件）。
func TestDisableInitializedPluginStops(t *testing.T) {
	p := &startFailPlugin{meta: Meta{Name: "inited", DefaultOn: true}}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll() // 只 Init，不 Start → StateInitialized

	if st := m.stateOf("inited"); st != StateInitialized {
		t.Fatalf("前置状态 = %v want initialized", st)
	}
	// Start 会失败，但 Disable 不需要 Start；这里把 Start 置空以便复用同一个替身。
	if err := m.Disable("inited"); err != nil {
		t.Fatalf("停用 initialized 插件应成功: %v", err)
	}
	_, _, stops := p.counts()
	if stops != 1 {
		t.Fatalf("initialized 插件停用时 Stop 调用次数 = %d want 1（否则 Init 占的资源泄漏）", stops)
	}
	if st := m.stateOf("inited"); st != StateDisabled {
		t.Fatalf("停用后状态 = %v want disabled", st)
	}
}

// slowStopPlugin 的 Stop 会卡住（除非 release 被关闭），用于验证 F5 的 Stop 超时阻断。
type slowStopPlugin struct {
	meta    Meta
	release chan struct{}
}

func (p *slowStopPlugin) Meta() Meta          { return p.meta }
func (p *slowStopPlugin) Init(*Context) error { return nil }
func (p *slowStopPlugin) Start() error        { return nil }
func (p *slowStopPlugin) Stop() error {
	<-p.release
	return nil
}

// TestStopTimeoutBlocksReassemblyUntilReload 覆盖 F5：
// Stop 超过框架超时后，插件被标 failed 并**阻断**后续 Init/Start
// （那个 Stop 协程还在跑，再 Init 同一实例 = 并发操作同一份 listener/协程状态）；
// 只有显式 Reload 才解除阻断。
func TestStopTimeoutBlocksReassemblyUntilReload(t *testing.T) {
	old := stopTimeout
	stopTimeout = 20 * time.Millisecond
	defer func() { stopTimeout = old }()

	p := &slowStopPlugin{meta: Meta{Name: "stuck", DefaultOn: true}, release: make(chan struct{})}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	if err := m.Disable("stuck"); err == nil {
		t.Fatal("Stop 超时后 Disable 应返回错误")
	}
	if reason, blocked := m.blockedReason("stuck"); !blocked {
		t.Fatalf("Stop 超时后必须阻断重新装配，实际未阻断（reason=%q）", reason)
	}

	err := m.Enable("stuck")
	if err == nil {
		t.Fatal("被阻断时 Enable 必须失败")
	}
	if !strings.Contains(err.Error(), "Reload") && !strings.Contains(err.Error(), "阻断") {
		t.Fatalf("阻断错误应提示先执行 Reload，实际: %v", err)
	}

	// 放行卡住的 Stop 协程，再显式 Reload → 阻断解除、插件重新跑起来。
	close(p.release)
	results := m.Reload()
	for _, r := range results {
		if r.Name == "stuck" && r.State != StateRunning {
			t.Fatalf("Reload 后 stuck 状态 = %v want running（错误: %s）", r.State, r.Error)
		}
	}
	if _, blocked := m.blockedReason("stuck"); blocked {
		t.Fatal("显式 Reload 后应解除阻断")
	}
	if st := m.stateOf("stuck"); st != StateRunning {
		t.Fatalf("Reload 后状态 = %v want running", st)
	}
}

// TestReloadReassemblesPlugins 验证 Reload 逐个 Stop→Init→Start 且计入启停次数。
func TestReloadReassemblesPlugins(t *testing.T) {
	a := &richPlugin{meta: Meta{Name: "aaa", DefaultOn: true}}
	b := &richPlugin{meta: Meta{Name: "bbb", DefaultOn: true, Dependencies: []string{"aaa"}}}
	m := newManagerFrom([]Plugin{a, b})
	m.BuildContext(nil)
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	results := m.Reload()
	if len(results) != 2 {
		t.Fatalf("Reload 结果数 = %d want 2 (%+v)", len(results), results)
	}
	for _, res := range results {
		if res.State != StateRunning {
			t.Fatalf("重载后 %s 状态 = %v want running（错误: %s）", res.Name, res.State, res.Error)
		}
	}
	if info := m.snapshotInfo("aaa"); info.RestartCount != 1 {
		t.Fatalf("重载应计入 RestartCount，实际 %d", info.RestartCount)
	}
	a.mu.Lock()
	starts := a.starts
	a.mu.Unlock()
	if starts != 2 {
		t.Fatalf("aaa Start 次数 = %d want 2（首次启动 + 重载）", starts)
	}
}

// squatterPlugin 模拟「绕过影子表直接占位共享 mux」的插件：
// 它先注册一条**合法**路由 /squat-ok（这条必须被保留），
// 再直接 mux.HandleFunc 占住 /squat，然后对 /squat 走正常 RegisterHTTP。
// 影子表此时认为 /squat 空闲，于是 ServeMux 会 panic——
// 这正是 recover 必须「写回错误」且「不得连带撤销已成功路由」的场景。
type squatterPlugin struct {
	meta      Meta
	okPattern string
	pattern   string
	err       error
}

func (p *squatterPlugin) Meta() Meta { return p.meta }

func (p *squatterPlugin) Init(ctx *Context) error {
	if err := ctx.RegisterHTTP(p.okPattern, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("legit"))
	}); err != nil {
		return err
	}
	ctx.ServeMux().HandleFunc(p.pattern, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("squatter"))
	})
	p.err = ctx.RegisterHTTP(p.pattern, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ghost"))
	})
	return p.err
}

func (p *squatterPlugin) Start() error { return nil }
func (p *squatterPlugin) Stop() error  { return nil }

// TestRegisterHTTPRecoversPanicWithoutGhostRoute 是 QA 复核发现的真实缺陷的回归测试：
//
//  1. RegisterHTTP 必须用**命名返回值**，defer 里的 recover 才能把 panic 写回 err；
//     历史上用的是匿名返回值，panic 被吞、函数返回 nil，插件误以为注册成功。
//  2. routes.Store 必须在 mux.HandleFunc **之后**：panic 时不得在影子表里留记录，
//     否则 ① 后续合法插件注册同一 pattern 会被误报冲突；② Routes() 会报告一条
//     mux 上并不存在的「幽灵路由」，而 V4 的插件视图正是消费 Routes()。
//  3. panic 也不能靠「按 owner 全量撤销」来兜底——那会把该插件此前**已成功**注册的
//     路由一起抹掉，造成 Routes() 比 mux 少一条的另一种不一致。
func TestRegisterHTTPRecoversPanicWithoutGhostRoute(t *testing.T) {
	p := &squatterPlugin{
		meta:      Meta{Name: "squatter", DefaultOn: true},
		okPattern: "/squat-ok",
		pattern:   "/squat",
	}
	m := newManagerFrom([]Plugin{p})
	m.BuildContext(nil)
	m.InitAll()

	if p.err == nil {
		t.Fatal("mux 已被占位时 RegisterHTTP 应返回错误（recover 未写回命名返回值 → panic 被吞）")
	}
	if !strings.Contains(p.err.Error(), "panicked") {
		t.Fatalf("错误应带 panicked 前缀便于排障，实际: %v", p.err)
	}
	if !strings.Contains(p.err.Error(), "squatter") || !strings.Contains(p.err.Error(), "/squat") {
		t.Fatalf("错误应点名插件与 pattern，实际: %v", p.err)
	}

	var haveOK, haveGhost bool
	for _, r := range m.Routes() {
		switch r.Pattern {
		case p.pattern:
			haveGhost = true
		case p.okPattern:
			haveOK = r.Plugin == "squatter"
		}
	}
	if haveGhost {
		t.Fatalf("Routes() 不应包含幽灵路由 %q（mux 上并没有它），实际 %+v", p.pattern, m.Routes())
	}
	if !haveOK {
		t.Fatalf("panic 后不得撤销同插件**已成功**注册的路由 %q，实际 %+v", p.okPattern, m.Routes())
	}

	// panic 转成的错误必须让框架如实把插件标 failed（而不是 running）。
	if st := m.stateOf("squatter"); st != StateFailed {
		t.Fatalf("Init 失败后状态 = %v want failed", st)
	}
	if msg := m.errorOf("squatter"); !strings.Contains(msg, "panicked") {
		t.Fatalf("插件错误应保留 panic 原因，实际: %q", msg)
	}
}
