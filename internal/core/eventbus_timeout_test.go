package core

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestSlowSubscriberDoesNotDelayOthers 是 V8 ⑤ 的核心验收。
//
// 注入一个「睡 300ms」的订阅者、守护时长 50ms，断言：
//   - 发布方只被拖住约 50ms（而不是 300ms）；
//   - 同一个 topic 上的其它订阅者照常收到事件（单点故障不扩散）；
//   - 慢订阅者被标记降级。
//
// 没有这条保护时，慢处理器跑在**发布方的协程**里：一个卡住的 webhook 转发
// 会让所有发消息的客户端一起卡住——这正是文档里「慢处理器拖垮 Hub 主线」的形态。
func TestSlowSubscriberDoesNotDelayOthers(t *testing.T) {
	bus := NewEventBus()
	release := make(chan struct{})

	bus.SubscribeWithTimeout("t", func(string, any) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-release:
		}
	}, 50*time.Millisecond)

	var fast int64
	bus.Subscribe("t", func(string, any) { atomic.AddInt64(&fast, 1) })

	start := time.Now()
	bus.Publish("t", nil)
	elapsed := time.Since(start)
	close(release)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("发布耗时 %s：慢订阅者仍拖住了发布方（守护时长 50ms）", elapsed)
	}
	if got := atomic.LoadInt64(&fast); got != 1 {
		t.Fatalf("快订阅者收到 %d 次，期望 1（被慢订阅者连累）", got)
	}
	if got := bus.DegradedCount(); got != 1 {
		t.Fatalf("降级计数 = %d，期望 1", got)
	}
}

// TestDegradedSubscriberIsSkipped 验证降级后不再投递。
//
// 为什么必须「暂停投递」而不是「继续尝试」：卡住的处理器通常是因为它依赖的
// 外部资源没了（目标不可达），继续投递等于每个事件都让发布方白等一个 timeout，
// 把单点故障放大成全体延迟。
func TestDegradedSubscriberIsSkipped(t *testing.T) {
	bus := NewEventBus()
	var calls int64
	release := make(chan struct{})

	bus.SubscribeWithTimeout("t", func(string, any) {
		atomic.AddInt64(&calls, 1)
		select {
		case <-time.After(200 * time.Millisecond):
		case <-release:
		}
	}, 30*time.Millisecond)

	bus.Publish("t", nil) // 第一次：超时 → 降级
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("首次发布后调用次数 = %d，期望 1", got)
	}

	bus.Publish("t", nil) // 第二次：已降级，应直接跳过
	bus.Publish("t", nil)
	time.Sleep(50 * time.Millisecond)
	close(release)

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("降级后仍被调用了 %d 次（期望停在 1）：暂停投递未生效", got)
	}
}

// TestSecondPublishAfterDegradeIsFast 验证降级后的发布零等待。
//
// 这条单独测，是因为「跳过」与「还等一个 timeout」在日志上几乎看不出区别，
// 但对热路径意味着每次发布白等 5 秒。
func TestSecondPublishAfterDegradeIsFast(t *testing.T) {
	bus := NewEventBus()
	release := make(chan struct{})
	bus.SubscribeWithTimeout("t", func(string, any) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-release:
		}
	}, 40*time.Millisecond)

	bus.Publish("t", nil) // 触发降级
	start := time.Now()
	bus.Publish("t", nil) // 应几乎立即返回
	elapsed := time.Since(start)
	close(release)

	if elapsed > 20*time.Millisecond {
		t.Fatalf("降级后发布耗时 %s：仍在为已降级的订阅者等超时", elapsed)
	}
}

// TestRecoverDegradedResumesDelivery 验证显式恢复能重新参与派发。
func TestRecoverDegradedResumesDelivery(t *testing.T) {
	bus := NewEventBus()
	var calls int64
	release := make(chan struct{})
	slow := int64(1) // 1=慢（触发降级），0=快

	bus.SubscribeWithTimeout("t", func(string, any) {
		atomic.AddInt64(&calls, 1)
		if atomic.LoadInt64(&slow) == 1 {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-release:
			}
		}
	}, 30*time.Millisecond)

	bus.Publish("t", nil)
	time.Sleep(50 * time.Millisecond)
	if bus.DegradedCount() != 1 {
		t.Fatalf("应已降级，实际 DegradedCount=%d", bus.DegradedCount())
	}

	atomic.StoreInt64(&slow, 0) // 处理器恢复正常速度
	bus.Publish("t", nil)       // 降级中：不投递
	time.Sleep(20 * time.Millisecond)
	before := atomic.LoadInt64(&calls)

	bus.RecoverDegraded()
	bus.Publish("t", nil)
	time.Sleep(50 * time.Millisecond)
	close(release)

	if got := atomic.LoadInt64(&calls); got != before+1 {
		t.Fatalf("恢复后调用次数 = %d，期望 %d（恢复未生效）", got, before+1)
	}
}

// TestFastSubscriberWithTimeoutIsNotDegraded 验证快处理器永远不会被误降级。
func TestFastSubscriberWithTimeoutIsNotDegraded(t *testing.T) {
	bus := NewEventBus()
	var n int64
	bus.SubscribeWithTimeout("t", func(string, any) { atomic.AddInt64(&n, 1) }, 50*time.Millisecond)

	for i := 0; i < 20; i++ {
		bus.Publish("t", nil)
	}
	if got := atomic.LoadInt64(&n); got != 20 {
		t.Fatalf("收到 %d 次，期望 20", got)
	}
	if got := bus.DegradedCount(); got != 0 {
		t.Fatalf("快处理器被误降级 %d 次", got)
	}
}

// TestGuardedSubscriberPanicIsolated 验证守护派发里的 panic 不会传出去。
func TestGuardedSubscriberPanicIsolated(t *testing.T) {
	bus := NewEventBus()
	bus.SubscribeWithTimeout("t", func(string, any) { panic("boom") }, 50*time.Millisecond)

	done := make(chan struct{})
	go func() {
		bus.Publish("t", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panic 的守护订阅者卡住了发布方")
	}
	time.Sleep(50 * time.Millisecond)
	if got := bus.DegradedCount(); got != 0 {
		t.Fatalf("panic 被 recover 后不该算降级，实得 %d", got)
	}
}

// TestSubscribeWithTimeoutCancel 验证注销后不再投递。
func TestSubscribeWithTimeoutCancel(t *testing.T) {
	bus := NewEventBus()
	var n int64
	cancel := bus.SubscribeWithTimeout("t", func(string, any) { atomic.AddInt64(&n, 1) }, 50*time.Millisecond)
	bus.Publish("t", nil)
	cancel()
	bus.Publish("t", nil)
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Fatalf("注销后仍收到事件，调用次数 = %d（期望 1）", got)
	}
}
