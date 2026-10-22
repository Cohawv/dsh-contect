package core

import (
	"sync"
	"testing"
	"time"
)

// TestEventBusSubscribeCancel 注册 10 个 handler，cancel 其中 3 个，Publish 只应触发 7 个。
func TestEventBusSubscribeCancel(t *testing.T) {
	b := NewEventBus()
	var mu sync.Mutex
	counts := make([]int, 10)
	cancels := make([]func(), 10)
	for i := 0; i < 10; i++ {
		i := i
		cancels[i] = b.Subscribe("t", func(string, any) {
			mu.Lock()
			counts[i]++
			mu.Unlock()
		})
	}
	// 注销第 0、3、7 个。
	cancels[0]()
	cancels[3]()
	cancels[7]()

	b.Publish("t", nil)

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for i, c := range counts {
		if (i == 0 || i == 3 || i == 7) && c != 0 {
			t.Fatalf("cancelled handler %d still fired (%d)", i, c)
		}
		total += c
	}
	if total != 7 {
		t.Fatalf("expected 7 firings, got %d", total)
	}
}

// TestEventBusSyncOrder 同步模式下派发顺序应保持注册序。
func TestEventBusSyncOrder(t *testing.T) {
	b := NewEventBus()
	var mu sync.Mutex
	var order []int
	for i := 0; i < 3; i++ {
		i := i
		b.Subscribe("t", func(string, any) {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}
	b.Publish("t", nil)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Fatalf("dispatch order wrong: %v", order)
	}
}

// TestEventBusCancelIdempotent cancel 幂等：多次调用只有第一次生效，且 handler 不再触发。
func TestEventBusCancelIdempotent(t *testing.T) {
	b := NewEventBus()
	fired := 0
	c := b.Subscribe("t", func(string, any) { fired++ })
	c()
	c()
	c()
	b.Publish("t", nil)
	if fired != 0 {
		t.Fatalf("handler fired after cancel: %d", fired)
	}
	// 注销后再 Publish 仍不应触发，且不应 panic。
	b.Publish("t", nil)
	if fired != 0 {
		t.Fatalf("handler fired after repeated cancel: %d", fired)
	}
}

// TestEventBusAsyncNonBlocking 慢 handler 在 SubscribeAsync 下不阻塞 Publish 返回。
func TestEventBusAsyncNonBlocking(t *testing.T) {
	b := NewEventBus()
	b.SubscribeAsync("t", func(string, any) {
		time.Sleep(100 * time.Millisecond)
	}, 4)

	start := time.Now()
	b.Publish("t", nil)
	elapsed := time.Since(start)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("Publish blocked on slow async handler: %v", elapsed)
	}
	if b.Dropped() != 0 {
		t.Fatalf("unexpected drops with spare queue: %d", b.Dropped())
	}
}

// TestEventBusAsyncDropCount 队列满时丢弃并计数，绝不阻塞发布方。
func TestEventBusAsyncDropCount(t *testing.T) {
	b := NewEventBus()
	b.SubscribeAsync("t", func(string, any) {
		time.Sleep(50 * time.Millisecond)
	}, 1)
	for i := 0; i < 20; i++ {
		b.Publish("t", nil)
	}
	if b.Dropped() == 0 {
		t.Fatalf("expected drops with full bounded queue, got 0")
	}
}

// TestEventBusSubscriberCount 订阅计数正确反映当前订阅者数量。
func TestEventBusSubscriberCount(t *testing.T) {
	b := NewEventBus()
	c1 := b.Subscribe("x", func(string, any) {})
	if b.SubscriberCount("x") != 1 {
		t.Fatalf("count = %d want 1", b.SubscriberCount("x"))
	}
	c2 := b.Subscribe("x", func(string, any) {})
	if b.SubscriberCount("x") != 2 {
		t.Fatalf("count = %d want 2", b.SubscriberCount("x"))
	}
	c1()
	if b.SubscriberCount("x") != 1 {
		t.Fatalf("count after cancel = %d want 1", b.SubscriberCount("x"))
	}
	c2()
	if b.SubscriberCount("x") != 0 {
		t.Fatalf("count after all cancel = %d want 0", b.SubscriberCount("x"))
	}
}
