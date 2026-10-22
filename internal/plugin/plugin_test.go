package plugin

import (
	"os"
	"strings"
	"testing"
)

// fakePlugin 是测试替身，不引入任何新依赖。
// 通过字段可控制 Init/Start/Stop 的返回值与panic行为，并用计数器记录调用。
type fakePlugin struct {
	meta     Meta
	initErr  error
	startErr error
	stopErr  error
	panicIn  string // "init" | "start" | "stop" 时对应阶段 panic
	initHit  *int
	startHit *int
	stopHit  *int
}

func (f *fakePlugin) Meta() Meta { return f.meta }

func (f *fakePlugin) Init(*Context) error {
	if f.initHit != nil {
		*f.initHit++
	}
	if f.panicIn == "init" {
		panic("boom-init")
	}
	return f.initErr
}

func (f *fakePlugin) Start() error {
	if f.startHit != nil {
		*f.startHit++
	}
	if f.panicIn == "start" {
		panic("boom-start")
	}
	return f.startErr
}

func (f *fakePlugin) Stop() error {
	if f.stopHit != nil {
		*f.stopHit++
	}
	if f.panicIn == "stop" {
		panic("boom-stop")
	}
	return f.stopErr
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestRegisterDuplicatePanic 验证重名注册直接 panic（编译期发现问题）。
func TestRegisterDuplicatePanic(t *testing.T) {
	resetRegistry()
	Register(&fakePlugin{meta: Meta{Name: "dup"}})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register(&fakePlugin{meta: Meta{Name: "dup"}})
}

// TestRegistryAllSorted 验证 All() 按 Name 升序返回副本。
func TestRegistryAllSorted(t *testing.T) {
	resetRegistry()
	Register(&fakePlugin{meta: Meta{Name: "charlie"}})
	Register(&fakePlugin{meta: Meta{Name: "alpha"}})
	Register(&fakePlugin{meta: Meta{Name: "bravo"}})
	got := All()
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != len(want) {
		t.Fatalf("len %d want %d", len(got), len(want))
	}
	for i, p := range got {
		if p.Meta().Name != want[i] {
			t.Fatalf("pos %d = %q want %q", i, p.Meta().Name, want[i])
		}
	}
}

// TestTopoSortDeterministic 验证拓扑排序确定性：依赖就绪者在前，同层按 Name 字典序。
func TestTopoSortDeterministic(t *testing.T) {
	a := &fakePlugin{meta: Meta{Name: "A"}}
	b := &fakePlugin{meta: Meta{Name: "B", Dependencies: []string{"A"}}}
	c := &fakePlugin{meta: Meta{Name: "C", Dependencies: []string{"A"}}}
	d := &fakePlugin{meta: Meta{Name: "D"}}

	// 故意乱序输入，结果应当一致。
	m := newManagerFrom([]Plugin{d, c, a, b})
	ordered, bad := m.order()
	if len(bad) != 0 {
		t.Fatalf("unexpected bad: %v", bad)
	}
	want := []string{"A", "B", "C", "D"}
	if !equalStrings(ordered, want) {
		t.Fatalf("order %v want %v", ordered, want)
	}
}

// TestTopoSortTieBreak 验证两个无依赖插件按 Name 字典序定序。
func TestTopoSortTieBreak(t *testing.T) {
	x := &fakePlugin{meta: Meta{Name: "Zeta"}}
	y := &fakePlugin{meta: Meta{Name: "Alpha"}}
	m := newManagerFrom([]Plugin{x, y})
	ordered, _ := m.order()
	if !equalStrings(ordered, []string{"Alpha", "Zeta"}) {
		t.Fatalf("tie-break order %v", ordered)
	}
}

// TestMissingDependency 验证依赖不存在时该插件 Failed 并给出人可读错误。
func TestMissingDependency(t *testing.T) {
	p := &fakePlugin{meta: Meta{Name: "P", Dependencies: []string{"ghost"}}}
	m := newManagerFrom([]Plugin{p})
	m.InitAll()
	info := m.infos["P"]
	if info.State != StateFailed {
		t.Fatalf("P state = %v want failed", info.State)
	}
	if !contains(info.Error, "ghost") {
		t.Fatalf("error %q should mention missing dependency", info.Error)
	}

	results, err := m.StartAll()
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Name == "P" && r.State == StateFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("P not reported failed in StartAll results: %v", results)
	}
}

// TestCircularDependency 验证循环依赖被检测为 Failed 并标注 circular。
func TestCircularDependency(t *testing.T) {
	a := &fakePlugin{meta: Meta{Name: "A", Dependencies: []string{"B"}}}
	b := &fakePlugin{meta: Meta{Name: "B", Dependencies: []string{"A"}}}
	m := newManagerFrom([]Plugin{a, b})
	m.InitAll()
	for _, name := range []string{"A", "B"} {
		info := m.infos[name]
		if info.State != StateFailed {
			t.Fatalf("%s state = %v want failed", name, info.State)
		}
		if !contains(info.Error, "circular") {
			t.Fatalf("%s error %q should mention circular", name, info.Error)
		}
	}
}

// TestPanicRecovered 验证 Init panic 被隔离为 Failed，不影响其它插件。
func TestPanicRecovered(t *testing.T) {
	good := &fakePlugin{meta: Meta{Name: "good"}}
	bad := &fakePlugin{meta: Meta{Name: "bad"}, panicIn: "init"}
	m := newManagerFrom([]Plugin{bad, good})
	m.InitAll()
	if m.infos["bad"].State != StateFailed {
		t.Fatalf("bad should be failed")
	}
	if !contains(m.infos["bad"].Error, "panicked") {
		t.Fatalf("bad error %q should mention panic", m.infos["bad"].Error)
	}
	if m.infos["good"].State != StateInitialized {
		t.Fatalf("good state = %v want initialized", m.infos["good"].State)
	}
}

// TestPanicInStartRecovered 验证 Start panic 同样被隔离，且不影响其它插件启动。
func TestPanicInStartRecovered(t *testing.T) {
	good := &fakePlugin{meta: Meta{Name: "good"}}
	bad := &fakePlugin{meta: Meta{Name: "bad"}, panicIn: "start"}
	m := newManagerFrom([]Plugin{bad, good})
	m.InitAll()
	results, err := m.StartAll()
	if err != nil {
		t.Fatalf("unexpected fatal: %v", err)
	}
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if byName["bad"].State != StateFailed {
		t.Fatalf("bad should be failed after start")
	}
	if !contains(byName["bad"].Error, "panicked") {
		t.Fatalf("bad error %q", byName["bad"].Error)
	}
	if byName["good"].State != StateRunning {
		t.Fatalf("good should be running, got %v", byName["good"].State)
	}
}

// TestRequiredMissingFailsStartup 验证 Required 插件被禁用时整体启动失败并报错。
func TestRequiredMissingFailsStartup(t *testing.T) {
	req := &fakePlugin{meta: Meta{Name: "req", Required: true, DefaultOn: true}}
	other := &fakePlugin{meta: Meta{Name: "other"}}
	m := newManagerFrom([]Plugin{req, other})
	m.SetEnabled("req", false) // 被白名单排除
	_, err := m.StartAll()
	if err == nil {
		t.Fatal("expected startup failure when required plugin disabled")
	}
	if !contains(err.Error(), "req") {
		t.Fatalf("error %q should mention required plugin", err.Error())
	}
}

// TestLifecycleHappyPath 验证正常全量启停：Init→Start→Stop 状态流转正确且顺序确定。
func TestLifecycleHappyPath(t *testing.T) {
	a := &fakePlugin{meta: Meta{Name: "A"}}
	b := &fakePlugin{meta: Meta{Name: "B", Dependencies: []string{"A"}}}
	m := newManagerFrom([]Plugin{a, b})

	m.InitAll()
	if m.infos["A"].State != StateInitialized || m.infos["B"].State != StateInitialized {
		t.Fatalf("init states: A=%v B=%v", m.infos["A"].State, m.infos["B"].State)
	}

	results, err := m.StartAll()
	if err != nil {
		t.Fatalf("start err: %v", err)
	}
	if m.infos["A"].State != StateRunning || m.infos["B"].State != StateRunning {
		t.Fatalf("run states: A=%v B=%v", m.infos["A"].State, m.infos["B"].State)
	}
	// 依赖顺序：A 先于 B。
	if len(results) != 2 || results[0].Name != "A" || results[1].Name != "B" {
		t.Fatalf("start order wrong: %v", results)
	}

	m.StopAll()
	if m.infos["B"].State != StateStopped || m.infos["A"].State != StateStopped {
		t.Fatalf("stop states: A=%v B=%v", m.infos["A"].State, m.infos["B"].State)
	}
}

// TestConfigLoadMerge 验证 LoadConfig 的合并优先级：CLI > 文件 > DefaultOn。
func TestConfigLoadMerge(t *testing.T) {
	resetRegistry()
	// 通过临时文件模拟 hub.json。
	dir := t.TempDir()
	path := dir + "/hub.json"
	content := `{"plugins":{"alpha":{"enabled":false,"options":{"port":8081,"name":"from-file"}}}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	Register(&fakePlugin{meta: Meta{Name: "alpha", DefaultOn: true}})
	Register(&fakePlugin{meta: Meta{Name: "beta", DefaultOn: false}})

	// CLI 强制开启 alpha，beta 未提（走文件/默认）。
	enabled, options, err := LoadConfig(path, map[string]bool{"alpha": true})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !enabled["alpha"] {
		t.Fatal("CLI override should enable alpha")
	}
	if enabled["beta"] {
		t.Fatal("beta default off should stay off")
	}
	if options["alpha"]["name"] != "from-file" {
		t.Fatalf("alpha options not merged: %v", options["alpha"])
	}
	if port, ok := options["alpha"]["port"].(float64); !ok || int(port) != 8081 {
		t.Fatalf("alpha port option wrong: %v", options["alpha"]["port"])
	}
}

// TestConfigMissingFileDefaults 验证文件缺失时全部走 DefaultOn，不报错。
func TestConfigMissingFileDefaults(t *testing.T) {
	resetRegistry()
	Register(&fakePlugin{meta: Meta{Name: "on", DefaultOn: true}})
	Register(&fakePlugin{meta: Meta{Name: "off", DefaultOn: false}})
	enabled, _, err := LoadConfig("/nonexistent/path/hub.json", nil)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if !enabled["on"] || enabled["off"] {
		t.Fatalf("defaults wrong: %v", enabled)
	}
}
