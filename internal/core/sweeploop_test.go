package core

import (
	"context"
	"testing"
	"time"

	"lan-agent-hub/internal/protocol"
)

// lastSweepAt 读一次 LastSweepAt（并发安全），供本文件断言用。
func lastSweepAt(h *Hub) int64 {
	h.muSweep.RLock()
	defer h.muSweep.RUnlock()
	return h.LastSweepAt
}

// TestSweepLoopStopsOnCancel 是 V3-② 的验收测试：
// SweepLoop 必须能被外部 cancel 干净终止（历史实现用 `for range ticker.C` 永不返回）。
func TestSweepLoopStopsOnCancel(t *testing.T) {
	hub := NewHub()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.SweepLoop(ctx, 10*time.Millisecond)
	}()

	// 先证明循环确实在跑：等到至少发生一次清扫。
	deadline := time.Now().Add(2 * time.Second)
	for lastSweepAt(hub) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lastSweepAt(hub) == 0 {
		t.Fatal("SweepLoop 在 2s 内没有执行任何一次清扫")
	}

	// cancel 后必须在 100ms 内返回（V3 验收标准：cancel() 后 100ms 内退出）。
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancel 后 100ms 内 SweepLoop 未退出（循环不可控）")
	}

	// 退出后不得再有清扫发生。
	after := lastSweepAt(hub)
	time.Sleep(50 * time.Millisecond)
	if got := lastSweepAt(hub); got != after {
		t.Fatalf("SweepLoop 退出后仍在清扫: LastSweepAt %d → %d", after, got)
	}
}

// TestSweepLoopSweepsStaleAgentOnTick 验证循环在 tick 时真的清理超时连接，
// 并发布 offline 事件（心跳超时行为与历史一致）。
func TestSweepLoopSweepsStaleAgentOnTick(t *testing.T) {
	hub := NewHub()
	hub.TimeoutMs = 1 // 1ms 超时：tick 到达时该 agent 必定已超时

	offline := make(chan string, 4)
	hub.Bus.Subscribe(protocol.TopicAgentOffline, func(_ string, payload any) {
		if id, ok := payload.(string); ok {
			select {
			case offline <- id:
			default:
			}
		}
	})

	sess := &mockSession{id: "stale-1"}
	hub.Registry.Bind("stale-1", "Stale", nil, protocol.ProtoCurrent, sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.SweepLoop(ctx, 10*time.Millisecond)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SweepLoop 未在 1s 内退出")
		}
	}()

	select {
	case id := <-offline:
		if id != "stale-1" {
			t.Fatalf("offline 事件 id = %q want stale-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SweepLoop 未在 2s 内清理超时连接并发布 offline 事件")
	}

	if _, ok := hub.Registry.Get("stale-1"); ok {
		t.Fatal("超时 agent 仍在在线表中")
	}
}
