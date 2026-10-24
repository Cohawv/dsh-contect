package pprof

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// stubGateway 满足 "pprof 依赖 httpgateway" 的拓扑约束（不自建端口）。
type stubGateway struct{}

func (stubGateway) Meta() plugin.Meta          { return plugin.Meta{Name: "httpgateway", DefaultOn: true} }
func (stubGateway) Init(*plugin.Context) error { return nil }
func (stubGateway) Start() error               { return nil }
func (stubGateway) Stop() error                { return nil }

// pprofRig 是装配好的 pprof 插件 + 真实 HTTP 服务器。
type pprofRig struct {
	hub *core.Hub
	p   *Pprof
	mgr *plugin.Manager
	srv *httptest.Server
	dir string
}

// newPprofRig 装配一套插件。
//
// 默认把告警阈值设为 0（关闭）、定时快照设为 0（关闭）：单测不该被
// 「测试进程堆有多大」这种环境因素影响，需要告警/快照的用例自己显式打开。
func newPprofRig(t *testing.T, options map[string]any) *pprofRig {
	t.Helper()
	dir := t.TempDir()
	opts := map[string]any{
		"memLimitMB":       0,
		"checkInterval":    "100ms",
		"snapshotInterval": 0,
		"snapshotDir":      dir,
		"snapshotKeep":     3,
	}
	for k, v := range options {
		opts[k] = v
	}

	hub := core.NewHub()
	p := &Pprof{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{p, stubGateway{}})
	mgr.ApplyConfig(
		map[string]bool{"pprof": true, "httpgateway": true},
		map[string]map[string]any{"pprof": opts},
	)
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(mgr.Mux())
	rig := &pprofRig{hub: hub, p: p, mgr: mgr, srv: srv, dir: dir}
	t.Cleanup(func() {
		mgr.StopAll()
		srv.Close()
	})
	return rig
}

// get 发起 GET 并返回状态码与响应体。
func (r *pprofRig) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// runtimeBody 抓取 /api/runtime 并解出响应体。
func (r *pprofRig) runtimeBody(t *testing.T) runtimeResponse {
	t.Helper()
	status, body := r.get(t, pathRuntime)
	if status != http.StatusOK {
		t.Fatalf("%s 状态码 = %d, body=%s", pathRuntime, status, body)
	}
	var out runtimeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 %s 失败: %v（body=%s）", pathRuntime, err, body)
	}
	return out
}

// TestRuntimeEndpointReportsRealNumbers 断言 /api/runtime 报的是真数字。
func TestRuntimeEndpointReportsRealNumbers(t *testing.T) {
	rig := newPprofRig(t, map[string]any{"memLimitMB": 1024})

	got := rig.runtimeBody(t)
	if got.Goroutines <= 0 {
		t.Errorf("goroutines = %d，期望 > 0（连测试进程自己都数不到，说明没读到 runtime）", got.Goroutines)
	}
	if got.HeapAllocBytes == 0 || got.HeapObjects == 0 {
		t.Errorf("heapAllocBytes=%d heapObjects=%d，期望均 > 0",
			got.HeapAllocBytes, got.HeapObjects)
	}
	// 1024MB = 1GiB = 1073741824 字节（按 1024 进制换算，不要和 10^9 混）。
	const wantLimit uint64 = 1024 * 1024 * 1024
	if got.MemLimitBytes != wantLimit {
		t.Errorf("memLimitBytes = %d，期望 %d（1024MB 换算）", got.MemLimitBytes, wantLimit)
	}
	if got.AllowRemote {
		t.Error("allowRemote 默认必须为 false（本系统无鉴权，默认只允许本机）")
	}
	if got.SnapshotIntervalMs != 0 {
		t.Errorf("snapshotIntervalMs = %d，期望 0（本 rig 显式关闭定时快照）", got.SnapshotIntervalMs)
	}
	if len(got.Snapshots) != 0 {
		t.Errorf("初始快照清单应为空，实际 %d 项", len(got.Snapshots))
	}
	// GC 一定发生过（进程起不来就不会跑到这里），停顿累计应为非负的有限值。
	if got.GCPauseTotalMs < 0 {
		t.Errorf("gcPauseTotalMs = %v，不应为负", got.GCPauseTotalMs)
	}
}

// TestRuntimeEndpointMethodNotAllowed 断言非 GET 被明确拒绝。
func TestRuntimeEndpointMethodNotAllowed(t *testing.T) {
	rig := newPprofRig(t, nil)
	resp, err := rig.srv.Client().Post(rig.srv.URL+pathRuntime, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST %s: %v", pathRuntime, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d，期望 405", resp.StatusCode)
	}
}

// TestSnapshotWritesPprofAndRotates 断言快照真的写成了 pprof 格式，且按份数轮转。
func TestSnapshotWritesPprofAndRotates(t *testing.T) {
	rig := newPprofRig(t, map[string]any{"snapshotKeep": 2})

	var names []string
	for i := 0; i < 3; i++ {
		status, body := rig.get(t, pathSnapshot)
		if status != http.StatusOK {
			t.Fatalf("第 %d 次抓快照状态码 = %d, body=%s", i+1, status, body)
		}
		var info snapshotInfo
		if err := json.Unmarshal([]byte(body), &info); err != nil {
			t.Fatalf("解析快照响应失败: %v", err)
		}
		if info.SizeBytes <= 0 {
			t.Fatalf("快照 sizeBytes = %d，期望 > 0", info.SizeBytes)
		}
		// pprof 的 WriteTo(debug=0) 是 gzip 压缩的 protobuf——magic 不对说明写了个空文件。
		f, err := os.Open(info.Path)
		if err != nil {
			t.Fatalf("打开快照失败: %v", err)
		}
		head := make([]byte, 2)
		_, _ = io.ReadFull(f, head)
		_ = f.Close()
		if head[0] != 0x1f || head[1] != 0x8b {
			t.Fatalf("快照不是 gzip/pprof 格式，头两字节 = %x %x", head[0], head[1])
		}
		names = append(names, info.Name)
	}

	snaps, err := listSnapshots(rig.dir)
	if err != nil {
		t.Fatalf("列快照失败: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("保留份数 = %d，期望 2（snapshotKeep=2 时必须删掉最旧的）", len(snaps))
	}
	// 留下的是「最新两份」：第一份必须已被删掉。
	for _, s := range snaps {
		if s.Name == names[0] {
			t.Errorf("最旧的快照 %s 应已被轮转删除", names[0])
		}
	}
	if snaps[len(snaps)-1].Name != names[2] {
		t.Errorf("清单最后一项 = %s，期望最新的 %s", snaps[len(snaps)-1].Name, names[2])
	}

	body := rig.runtimeBody(t)
	if body.SnapshotTotal != 3 {
		t.Errorf("snapshotTotal = %d，期望 3", body.SnapshotTotal)
	}
	if body.LastSnapshot == "" {
		t.Error("lastSnapshot 为空，期望指向最近一次快照")
	}
	if len(body.Snapshots) != 2 {
		t.Errorf("/api/runtime 的 snapshots 项数 = %d，期望 2", len(body.Snapshots))
	}
}

// TestRotateIgnoresForeignFiles 断言轮转只碰自己的文件。
func TestRotateIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreignTxt := filepath.Join(dir, "notes.txt")
	foreignBad := filepath.Join(dir, "heap-notanumber.pprof")
	for _, f := range []string{foreignTxt, foreignBad} {
		if err := os.WriteFile(f, []byte("keep me"), 0o644); err != nil {
			t.Fatalf("预置文件失败: %v", err)
		}
	}

	p := &Pprof{}
	p.snapDir.Store(dir)
	p.snapKeep.Store(1)
	for i := 0; i < 3; i++ {
		if _, err := p.snapshot("test"); err != nil {
			t.Fatalf("写快照失败: %v", err)
		}
	}

	snaps, err := listSnapshots(dir)
	if err != nil {
		t.Fatalf("列快照失败: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("自己的快照剩余 %d 份，期望 1", len(snaps))
	}
	for _, f := range []string{foreignTxt, foreignBad} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("轮转误删了非本插件文件 %s：%v", f, err)
		}
	}
}

// TestEvaluateHysteresis 断言阈值判定：只在跨越时告警，回落到 90% 以下才解除。
func TestEvaluateHysteresis(t *testing.T) {
	hub := core.NewHub()
	p := &Pprof{hub: hub}
	p.memLimit.Store(100) // 100 字节的「阈值」，让判定可以用整数讲清楚

	var mu sync.Mutex
	var events []protocol.MemHigh
	cancel := hub.Bus.Subscribe(protocol.TopicMemHigh, func(_ string, payload any) {
		mu.Lock()
		defer mu.Unlock()
		if ev, ok := payload.(protocol.MemHigh); ok {
			events = append(events, ev)
		}
	})
	defer cancel()

	countEvents := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(events)
	}

	steps := []struct {
		heap     uint64
		wantHigh bool
		wantEvts int
		why      string
	}{
		{99, false, 0, "阈值以下：不告警"},
		{100, true, 1, "刚好触达阈值：告警一次"},
		{150, true, 1, "持续超限：不重复告警（否则日志被刷成噪声）"},
		{95, true, 1, "95% 未回落到迟滞线以下：仍视为未恢复"},
		{89, false, 1, "跌到 90% 以下：解除"},
		{120, true, 2, "再次越线：第二次告警"},
	}
	for _, s := range steps {
		p.evaluate(plugin.RuntimeStats{
			HeapAllocBytes: s.heap,
			MemLimitBytes:  100,
			Goroutines:     7,
		})
		if got := p.memHigh.Load(); got != s.wantHigh {
			t.Errorf("heap=%d 时 memHigh = %t，期望 %t（%s）", s.heap, got, s.wantHigh, s.why)
		}
		if got := countEvents(); got != s.wantEvts {
			t.Errorf("heap=%d 时事件数 = %d，期望 %d（%s）", s.heap, got, s.wantEvts, s.why)
		}
	}

	if total := p.memHighTotal.Load(); total != 2 {
		t.Errorf("memHighTotal = %d，期望 2（进入超限状态两次）", total)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("收到 %d 个 mem.high 事件，期望 2", len(events))
	}
	if events[0].LimitBytes != 100 || events[0].Goroutines != 7 {
		t.Errorf("事件载荷不对：%+v", events[0])
	}
	if events[0].Count != 1 || events[1].Count != 2 {
		t.Errorf("事件序号 = %d,%d，期望 1,2", events[0].Count, events[1].Count)
	}
}

// TestZeroLimitDisablesAlert 断言 memLimitMB=0 是「关掉」，不是「阈值为零所以永远告警」。
func TestZeroLimitDisablesAlert(t *testing.T) {
	p := &Pprof{}
	p.memLimit.Store(0)
	p.evaluate(plugin.RuntimeStats{HeapAllocBytes: 1 << 40})
	if p.memHigh.Load() {
		t.Error("阈值 0 应表示关闭告警，实际进入了超限状态")
	}
	if p.memHighTotal.Load() != 0 {
		t.Error("阈值 0 时不应累计告警次数")
	}
}

// TestLoopbackGuard 断言 pprof 端点默认只对本机开放。
func TestLoopbackGuard(t *testing.T) {
	p := &Pprof{}
	h := p.guard(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		remote string
		want   int
	}{
		{"127.0.0.1:5000", http.StatusOK},
		{"127.0.0.2:5000", http.StatusOK},
		{"[::1]:5000", http.StatusOK},
		{"192.0.2.9:5000", http.StatusForbidden},
		{"198.51.100.1:5000", http.StatusForbidden},
		{"garbage", http.StatusForbidden}, // 解析失败 → 拒绝（失败即安全）
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, pathIndex, nil)
		req.RemoteAddr = c.remote
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("RemoteAddr=%q 状态码 = %d，期望 %d", c.remote, rec.Code, c.want)
		}
	}
}

// TestAllowRemoteBypassesGuard 断言显式开启后放行（配置是唯一开关）。
func TestAllowRemoteBypassesGuard(t *testing.T) {
	p := &Pprof{}
	p.allowRemote.Store(true)
	h := p.guard(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, pathIndex, nil)
	req.RemoteAddr = "192.0.2.9:5000"
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("allowRemote=true 时状态码 = %d，期望 200", rec.Code)
	}
}

// TestProfileSecondsCap 断言 CPU 采样时长有硬顶（防外部占住 CPU 采样器一小时）。
func TestProfileSecondsCap(t *testing.T) {
	rig := newPprofRig(t, nil)
	for _, s := range []string{"0", "-1", "999", "abc"} {
		status, _ := rig.get(t, pathProfile+"?seconds="+s)
		if status != http.StatusBadRequest {
			t.Errorf("seconds=%s 状态码 = %d，期望 400（必须拒绝越界/非法时长）", s, status)
		}
	}
}

// TestSharedStatsProvider 断言共享 KV 里的提供者可用（metrics 靠它对齐口径）。
func TestSharedStatsProvider(t *testing.T) {
	rig := newPprofRig(t, nil)
	v, ok := rig.p.ctx.GetShared(plugin.RuntimeStatsKey)
	if !ok {
		t.Fatalf("共享 KV 里没有 %s（metrics 的运行时指标会退化为兜底读数）", plugin.RuntimeStatsKey)
	}
	fn, isFn := v.(plugin.RuntimeStatsProvider)
	if !isFn || fn == nil {
		t.Fatalf("共享 KV 值类型 = %T，期望 plugin.RuntimeStatsProvider", v)
	}
	st := fn()
	if st.Goroutines <= 0 || st.HeapAllocBytes == 0 {
		t.Errorf("提供者返回 goroutines=%d heapAlloc=%d，期望均为真实正值",
			st.Goroutines, st.HeapAllocBytes)
	}
	if st.TS == 0 {
		t.Error("提供者返回 TS=0，期望毫秒时间戳")
	}
}

// TestStopLeavesNoGoroutines 断言停用后不留协程（定时快照的经典泄漏点）。
func TestStopLeavesNoGoroutines(t *testing.T) {
	hub := core.NewHub()
	p := &Pprof{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{p, stubGateway{}})
	// 1s 快照间隔（配置下限）：确保 Stop 时确有定时器在跑，测得到「关得掉」。
	mgr.ApplyConfig(
		map[string]bool{"pprof": true, "httpgateway": true},
		map[string]map[string]any{"pprof": {
			"memLimitMB":       0,
			"snapshotInterval": "1s",
			"snapshotDir":      t.TempDir(),
		}},
	)
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()

	// 基线取在 StartAll 之前：此时插件协程都还没起，差值就是插件自己带来的。
	runtime.GC()
	base := runtime.NumGoroutine()

	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	// 让它真的跑几轮（保证两个循环都已进入稳态，Stop 才有东西可停）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, total, _ := p.counters(); total > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, total, _ := p.counters(); total == 0 {
		t.Fatal("1s 间隔下 5 秒内没写出任何定时快照：定时循环没跑起来")
	}

	mgr.StopAll()

	// Stop 内部已 wg.Wait()；这里再等 goroutine 计数回落到基线附近（允许 +2 的调度噪声）。
	var got int
	for i := 0; i < 200; i++ {
		got = runtime.NumGoroutine()
		if got <= base+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Stop 后 goroutines = %d，启动前基线 = %d：定时协程没有退出（泄漏）", got, base)
}

// TestApplyHotUpdates 断言热更新改的是真配置，非法值一律拒绝。
func TestApplyHotUpdates(t *testing.T) {
	rig := newPprofRig(t, nil)

	if err := rig.p.Apply(map[string]any{"memLimitMB": float64(4096)}); err != nil {
		t.Fatalf("Apply memLimitMB: %v", err)
	}
	// 用 int64 常量：windows/386 上 int 是 32 位，4GB 字面量会溢出（这正是本机目标平台）。
	const wantLimit int64 = 4096 * 1024 * 1024
	if got := rig.p.memLimit.Load(); got != wantLimit {
		t.Errorf("memLimitMB 热更新后 = %d，期望 %d", got, wantLimit)
	}
	if got := rig.runtimeBody(t).MemLimitBytes; got != uint64(wantLimit) {
		t.Errorf("/api/runtime 的 memLimitBytes = %d，热更新未反映到端点", got)
	}

	if err := rig.p.Apply(map[string]any{"snapshotKeep": float64(5)}); err != nil {
		t.Fatalf("Apply snapshotKeep: %v", err)
	}
	if got := rig.p.snapKeep.Load(); got != 5 {
		t.Errorf("snapshotKeep 热更新后 = %d，期望 5", got)
	}

	bad := []map[string]any{
		{"snapshotKeep": float64(0)},
		{"snapshotKeep": float64(9999)},
		{"checkInterval": "1ms"},
		{"memLimitMB": "不是数字"},
	}
	for _, opts := range bad {
		if err := rig.p.Apply(opts); err == nil {
			t.Errorf("Apply(%v) 应返回错误（非法配置必须被拒绝，不能静默接受）", opts)
		}
	}
}
