// V8 ④ 内存治理：metrics 侧的运行时指标。
//
// 这里刻意**不复用** pprof 插件的实例，而是分两类验证：
//  1. 没有 pprof 时的兜底（runtime 直读）——插件可关，指标不能缺；
//  2. 有 pprof 时走共享 KV 的同一份数字——「曲线」与「gauge」必须同源同刻。
package metrics

import (
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/plugin"
)

// runtimeFamilies 是 V8 ④ 新增的指标族与期望类型。
var runtimeFamilies = map[string]string{
	"hub_goroutines":             "gauge",
	"hub_heap_alloc_bytes":       "gauge",
	"hub_heap_objects":           "gauge",
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
