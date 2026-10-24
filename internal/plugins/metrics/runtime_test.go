// V8 ④ 内存治理：metrics 侧的运行时指标。
//
// 这里刻意**不复用** pprof 插件的实例，而是分两类验证：
//  1. 没有 pprof 时的兜底（runtime 直读）——插件可关，指标不能缺；
//  2. 有 pprof 时走共享 KV 的同一份数字——「曲线」与「gauge」必须同源同刻。
package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/plugin"
)

// runtimeFamilies 是 V8 ④ 新增的指标族与期望类型。
var runtimeFamilies = map[string]string{
	"hub_goroutines":       "gauge",
	"hub_heap_alloc_bytes": "gauge",
	"hub_heap_objects":     "gauge",
	// heap_alloc 的参考线：没有它，读的人无法判断当前堆是"轻松"还是"快要 GC 了"。
	"hub_next_gc_bytes":          "gauge",
	"hub_gc_total":               "counter",
	"hub_gc_pause_seconds_total": "counter",
	"hub_memory_limit_bytes":     "gauge",
	"hub_mem_high_total":         "counter",
}

// TestRuntimeFamiliesWithoutPprof 断言：即使没有 pprof 插件，运行时指标一族也不缺。
//
// 这条用例守护的是一个很容易踩的坑——把指标挂在「某个可选插件」上，
// 结果那个插件被关掉后，恰恰在最需要看内存的部署里没有内存数字。
func TestRuntimeFamiliesWithoutPprof(t *testing.T) {
	rig := newMetricsRig(t, nil) // rig 只装 httpgateway 替身，没有 pprof

	body := rig.scrape(t)
	samples, helps, types := parseMetrics(t, body)
	for name, wantType := range runtimeFamilies {
		if !helps[name] {
			t.Errorf("缺少 # HELP %s", name)
		}
		if !types[name] {
			t.Errorf("缺少 # TYPE %s", name)
			continue
		}
		if _, ok := samples[name]; !ok {
			t.Errorf("缺少 %s 的样本值", name)
		}
		if got := typeOf(body, name); got != wantType {
			t.Errorf("%s 类型 = %q，期望 %q", name, got, wantType)
		}
	}

	if v := samples["hub_goroutines"]; v <= 0 {
		t.Errorf("hub_goroutines = %v，期望 > 0（兜底直读也必须拿到真数字）", v)
	}
	if v := samples["hub_heap_alloc_bytes"]; v <= 0 {
		t.Errorf("hub_heap_alloc_bytes = %v，期望 > 0", v)
	}
	// rig 没装 pprof：阈值与告警计数应如实为 0，而不是编一个默认值。
	if v := samples["hub_memory_limit_bytes"]; v != 0 {
		t.Errorf("hub_memory_limit_bytes = %v，期望 0（无 pprof 插件时未启用告警）", v)
	}
	if v := samples["hub_mem_high_total"]; v != 0 {
		t.Errorf("hub_mem_high_total = %v，期望 0", v)
	}
}

// TestRuntimeStatsComeFromSharedProvider 断言有提供者时走同一份数字（不各算一套）。
func TestRuntimeStatsComeFromSharedProvider(t *testing.T) {
	rig := newMetricsRig(t, map[string]any{"interval": "200ms"})

	want := plugin.RuntimeStats{
		HeapAllocBytes: 123456,
		HeapInuseBytes: 200000,
		HeapObjects:    789,
		Goroutines:     42,
		NumGC:          11,
		GCPauseTotalNs: 2_500_000, // 2.5ms
		MemLimitBytes:  2 << 20,
		MemHigh:        true,
		MemHighTotal:   3,
	}
	rig.mt.ctx.SetShared(plugin.RuntimeStatsKey, plugin.RuntimeStatsProvider(func() plugin.RuntimeStats {
		want.TS = time.Now().UnixMilli()
		return want
	}))

	samples, _, _ := parseMetrics(t, rig.scrape(t))
	checks := []struct {
		key  string
		want float64
	}{
		{"hub_heap_alloc_bytes", 123456},
		{"hub_heap_objects", 789},
		{"hub_goroutines", 42},
		{"hub_gc_total", 11},
		{"hub_gc_pause_seconds_total", 0.0025},
		{"hub_memory_limit_bytes", 2 << 20},
		{"hub_mem_high_total", 3},
	}
	for _, c := range checks {
		if got := samples[c.key]; got != c.want {
			t.Errorf("%s = %v，期望 %v（共享提供者的数字没有被采用）", c.key, got, c.want)
		}
	}

	// 曲线与 gauge 同源：采样点里的堆值也必须等于提供者的值。
	waitFor(t, "采样点带回运行时数字", func() bool {
		s := rig.summary(t)
		if len(s.Samples) == 0 {
			return false
		}
		last := s.Samples[len(s.Samples)-1]
		return last.HeapAllocBytes == want.HeapAllocBytes && last.Goroutines == want.Goroutines
	})
}

// TestSummarySamplesExposeRuntime 断言 /api/metrics/summary 的采样点带内存/协程字段。
func TestSummarySamplesExposeRuntime(t *testing.T) {
	rig := newMetricsRig(t, map[string]any{"interval": "200ms"})

	s := rig.summary(t)
	if len(s.Samples) == 0 {
		t.Fatal("没有采样点（Start 时应立即采一次，供曲线起点）")
	}
	last := s.Samples[len(s.Samples)-1]
	if last.HeapAllocBytes == 0 {
		t.Error("采样点 heapAllocBytes = 0，控制台画不出内存曲线")
	}
	if last.Goroutines <= 0 {
		t.Errorf("采样点 goroutines = %d，期望 > 0", last.Goroutines)
	}
	if last.TS == 0 {
		t.Error("采样点 ts = 0")
	}
}

// typeOf 从指标文本里取某个族的 TYPE。
func typeOf(body, name string) string {
	prefix := "# TYPE " + name + " "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// V8f：summary 补齐丢弃 / 内存 / GC 字段，且必须与 /metrics 同源
// ---------------------------------------------------------------------------

// summaryKeys 是 V8f 契约里新增的全部 JSON 键。
//
// 为什么逐个点名而不是只看结构体解码结果：解码进结构体时，缺失的键与「值为 0」
// 完全无法区分——字段名拼错（例如 heapAlloc 写成 heapAllocBytesBytes）在解码路径上
// 表现为「安静地是 0」，而这恰恰是契约破坏最难发现的一种形态。
var summaryKeys = []string{
	"deliverDropped", "deliverClosed",
	"heapAllocBytes", "heapInuseBytes", "goroutines", "gcTotal", "nextGCBytes",
	"memLimitBytes", "memHigh", "memHighTotal",
}

// TestSummaryExposesPerformanceFields 断言 summary JSON 含全部新键，且取的是真数字。
func TestSummaryExposesPerformanceFields(t *testing.T) {
	rig := newMetricsRig(t, nil) // 只装 httpgateway 替身，没有 pprof 提供者
	rig.hub.SetDropStats(func() (int64, int64) { return 99, 7 })

	raw, s := rig.summaryJSON(t)
	for _, k := range summaryKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("summary 缺少 JSON 键 %q（前端与 CLI 已按此契约开发）", k)
		}
	}

	// 非零值：证明这些字段不是零值占位。
	if s.Goroutines < 1 {
		t.Errorf("goroutines = %d，期望 >= 1（零值说明字段没接上数据源）", s.Goroutines)
	}
	if s.HeapAllocBytes == 0 {
		t.Error("heapAllocBytes = 0，期望 > 0")
	}
	if s.HeapInuseBytes == 0 {
		t.Error("heapInuseBytes = 0，期望 > 0")
	}
	// nextGCBytes 必须真的有值：Go 进程启动后 NextGC 由 GC 目标算出，恒为正。
	// 等于 0 只有两种可能——取了别的字段（例如 HeapObjects 在空堆上是 0），
	// 或者干脆没赋值。控制台要用它当"离下一次 GC 还有多远"的参考线，占位 0 会让曲线全废。
	if s.NextGCBytes == 0 {
		t.Error("nextGCBytes = 0，期望 > 0（取错字段或没赋值）")
	}
	// 丢弃侧：值必须如实来自 hub 的口子。
	if s.DeliverDropped != 99 {
		t.Errorf("deliverDropped = %d，期望 99", s.DeliverDropped)
	}
	if s.DeliverClosed != 7 {
		t.Errorf("deliverClosed = %d，期望 7", s.DeliverClosed)
	}
	// 无 pprof 提供者：阈值与告警状态如实为 0/false，不编默认阈值。
	if s.MemLimitBytes != 0 {
		t.Errorf("memLimitBytes = %d，期望 0（无 pprof 提供者）", s.MemLimitBytes)
	}
	if s.MemHigh {
		t.Error("memHigh = true，无 pprof 提供者时不可能处于超阈值状态")
	}
	if s.MemHighTotal != 0 {
		t.Errorf("memHighTotal = %d，期望 0", s.MemHighTotal)
	}
}

// TestSummaryAndMetricsShareRuntimeSource 断言 summary 的数字与 /metrics 的 gauge 同源。
//
// 用固定快照（共享 KV 里的 provider）而不是真实 MemStats：
// 真实 goroutine 数在两个 HTTP 请求之间必然变化，那样测出来的「不一致」
// 可能只是采样时差，无法区分「口径分家」与「时间差」——而这里要钉住的正是前者。
func TestSummaryAndMetricsShareRuntimeSource(t *testing.T) {
	rig := newMetricsRig(t, nil)

	fixed := plugin.RuntimeStats{
		Goroutines:     77,
		HeapAllocBytes: 4096,
		HeapInuseBytes: 8192,
		HeapObjects:    33,
		// NextGCBytes 刻意取一个与 heapAllocBytes **不同**的值：
		// 两者相等时，"把 NextGC 写成 HeapAlloc"这类取错字段在对照里看不出来。
		NextGCBytes:    65536,
		NumGC:          5,
		GCPauseTotalNs: 1_500_000,
		MemLimitBytes:  1 << 20,
		MemHigh:        true,
		MemHighTotal:   9,
	}
	rig.mt.ctx.SetShared(plugin.RuntimeStatsKey, plugin.RuntimeStatsProvider(func() plugin.RuntimeStats {
		fixed.TS = time.Now().UnixMilli()
		return fixed
	}))
	rig.hub.SetDropStats(func() (int64, int64) { return 4321, 17 })

	_, s := rig.summaryJSON(t)
	samples, _, _ := parseMetrics(t, rig.scrape(t))

	// 逐字段对照：summary 的 JSON 值 == /metrics 的 gauge 值。
	pairs := []struct {
		jsonKey string
		jsonVal float64
		family  string
	}{
		{"goroutines", float64(s.Goroutines), "hub_goroutines"},
		{"heapAllocBytes", float64(s.HeapAllocBytes), "hub_heap_alloc_bytes"},
		{"gcTotal", float64(s.GCTotal), "hub_gc_total"},
		// nextGCBytes 必须有同名 gauge：summary 暴露了而 Prometheus 侧没有，
		// 就是又一次口径分家（前端画参考线、抓取端看不到同一条线）。
		{"nextGCBytes", float64(s.NextGCBytes), "hub_next_gc_bytes"},
		{"memLimitBytes", float64(s.MemLimitBytes), "hub_memory_limit_bytes"},
		{"memHighTotal", float64(s.MemHighTotal), "hub_mem_high_total"},
		{"deliverDropped", float64(s.DeliverDropped), "hub_deliver_dropped_total"},
		{"deliverClosed", float64(s.DeliverClosed), "hub_deliver_closed_total"},
	}
	for _, p := range pairs {
		gauge, ok := samples[p.family]
		if !ok {
			t.Errorf("/metrics 缺少 %s", p.family)
			continue
		}
		if p.jsonVal != gauge {
			t.Errorf("%s=%v 与 /metrics 的 %s=%v 不一致（两个端点各算了一套口径）",
				p.jsonKey, p.jsonVal, p.family, gauge)
		}
	}

	// 且两者都真的采用了提供者的数字（否则「一致」可能只是两边都错成 0）。
	if s.Goroutines != fixed.Goroutines || s.MemHighTotal != fixed.MemHighTotal {
		t.Errorf("summary 未采用共享提供者的数字: %+v", s)
	}
	// memHigh 是布尔量，/metrics 里没有对应族，单独断言语义正确。
	if !s.MemHigh {
		t.Error("memHigh = false，提供者报告处于超阈值状态")
	}
}

// TestSummaryDegradesWithoutProvider 覆盖退化路径：没有 pprof 提供者也必须是真实值。
//
// 守护的是「为了省资源关掉插件」的部署——那正是最需要看内存的一类部署。
func TestSummaryDegradesWithoutProvider(t *testing.T) {
	rig := newMetricsRig(t, nil)

	_, s := rig.summaryJSON(t)
	if s.Goroutines < 1 {
		t.Errorf("无提供者时 goroutines = %d，期望 >= 1（应退化为 CachedRuntimeStats）", s.Goroutines)
	}
	if s.HeapAllocBytes == 0 {
		t.Error("无提供者时 heapAllocBytes = 0，退化路径没有拿到真实快照")
	}
	// 与 /metrics 一致：同样退化为缓存快照，趋势采样点也应有值。
	samples, _, _ := parseMetrics(t, rig.scrape(t))
	if samples["hub_goroutines"] < 1 {
		t.Errorf("无提供者时 /metrics hub_goroutines = %v，期望 >= 1", samples["hub_goroutines"])
	}
}

// summaryJSON 拉取 summary，同时返回原始键集合与解码后的结构体。
//
// 两样都要：键集合用来证明「键存在」（哪怕值是 false/0），结构体用来读数值。
func (r *metricsRig) summaryJSON(t *testing.T) (map[string]json.RawMessage, summaryResponse) {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + pathSummary)
	if err != nil {
		t.Fatalf("GET %s: %v", pathSummary, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读 body: %v", err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析 summary 键集合: %v（body=%s）", err, body)
	}
	var s summaryResponse
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("解析 summary: %v", err)
	}
	return raw, s
}
