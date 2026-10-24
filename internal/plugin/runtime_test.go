package plugin

import (
	"testing"
	"time"
)

// TestCachedStatsDoesNotReread 断言读取缓存不会重新读 MemStats。
//
// 这条契约很关键：runtime.ReadMemStats 会 STW，而 /metrics 的抓取频率由外部决定。
// 若「取」也走「读」，就会出现「监控抓得越勤、业务越卡」——
// 实测在 600 成员房间扇出下，抓取期间丢弃 1360 条，不抓取只丢 42 条。
// 缓存点的判定方式是 TS（采样时刻）：连续两次取缓存必须拿到同一时刻的值。
func TestCachedStatsDoesNotReread(t *testing.T) {
	first := RefreshRuntimeStats()
	if first.TS == 0 {
		t.Fatal("RefreshRuntimeStats 返回 TS=0，采样时刻必须填")
	}
	if first.HeapAllocBytes == 0 {
		t.Error("HeapAllocBytes = 0，未真正读到堆状态")
	}

	// 等一段时间后仍取缓存：只要中间没有刷新，采样时刻就不应变。
	time.Sleep(5 * time.Millisecond)
	again := CachedRuntimeStats()
	if again.TS != first.TS {
		t.Errorf("缓存采样时刻变了（%d → %d）：取缓存路径在重新读 MemStats（会 STW）",
			first.TS, again.TS)
	}

	// 刷新后必须换成新时刻（否则缓存等于冻结，曲线会变成一条直线）。
	time.Sleep(2 * time.Millisecond)
	refreshed := RefreshRuntimeStats()
	if refreshed.TS == first.TS {
		t.Errorf("刷新后采样时刻仍是 %d：缓存没有更新，指标会永远停在旧值", first.TS)
	}
	if got := CachedRuntimeStats(); got.TS != refreshed.TS {
		t.Errorf("刷新后取缓存 = %d，期望 %d", got.TS, refreshed.TS)
	}
}

// TestGoroutinesIsReported 断言 goroutine 数被如实报出（泄漏的第一信号）。
func TestGoroutinesIsReported(t *testing.T) {
	st := RefreshRuntimeStats()
	if st.Goroutines <= 0 {
		t.Errorf("Goroutines = %d，期望 > 0", st.Goroutines)
	}
	if st.SysBytes == 0 {
		t.Error("SysBytes = 0，期望 > 0")
	}
}
