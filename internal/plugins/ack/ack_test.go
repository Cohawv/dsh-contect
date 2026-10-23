package ack

import (
	"fmt"
	"testing"
	"time"
)

// newTestAck 构造一个不挂核心钩子的 Ack 实例（仅测 tracker 逻辑）。
func newTestAck(t *testing.T) *Ack {
	t.Helper()
	p := &Ack{}
	p.ttl = 0
	p.maxRecords = 1000
	p.recs = make(map[string]*deliveryRecord)
	return p
}

// TestAckTrackFailed：目标离线 → failed / reason=offline。
func TestAckTrackFailed(t *testing.T) {
	p := newTestAck(t)
	p.Track("m1", false, "agentB")
	r, ok := p.recs["m1"]
	if !ok {
		t.Fatal("m1 未被记录")
	}
	if r.Status != statusFailed {
		t.Fatalf("期望 failed，实得 %s", r.Status)
	}
	if r.Reason != "offline" {
		t.Fatalf("期望 reason=offline，实得 %q", r.Reason)
	}
	if r.Target != "agentB" {
		t.Fatalf("期望 target=agentB，实得 %q", r.Target)
	}
}

// TestAckTrackPendingThenAck：在线投递 → pending，收到回执 → delivered。
func TestAckTrackPendingThenAck(t *testing.T) {
	p := newTestAck(t)
	p.Track("m1", true, "agentB")
	if p.recs["m1"].Status != statusPending {
		t.Fatalf("期望 pending，实得 %s", p.recs["m1"].Status)
	}
	p.Ack("m1", statusDelivered, "agentB")
	if p.recs["m1"].Status != statusDelivered {
		t.Fatalf("期望 delivered，实得 %s", p.recs["m1"].Status)
	}
}

// TestAckStatusOfUnknown：查不到返回 unknown。
func TestAckStatusOfUnknown(t *testing.T) {
	p := newTestAck(t)
	if got := p.statusOf("nope"); got != statusUnknown {
		t.Fatalf("期望 unknown，实得 %s", got)
	}
}

// TestAckAckBeforeTrack：回执先于 Track 到达仍以 delivered 记录，避免对账缺口。
func TestAckAckBeforeTrack(t *testing.T) {
	p := newTestAck(t)
	p.Ack("m1", statusDelivered, "agentB")
	if p.recs["m1"].Status != statusDelivered {
		t.Fatalf("期望 delivered，实得 %s", p.recs["m1"].Status)
	}
	if p.recs["m1"].Target != "agentB" {
		t.Fatalf("期望 target=agentB，实得 %q", p.recs["m1"].Target)
	}
}

// TestAckTTLExpiry：超过 TTL 后查询返回 expired 并惰性清理。
func TestAckTTLExpiry(t *testing.T) {
	p := newTestAck(t)
	p.ttl = 50 // 50ms
	p.Track("m1", true, "agentB")
	if got := p.statusOf("m1"); got != statusPending {
		t.Fatalf("TTL 内期望 pending，实得 %s", got)
	}
	time.Sleep(80 * time.Millisecond)
	if got := p.statusOf("m1"); got != statusExpired {
		t.Fatalf("TTL 外期望 expired，实得 %s", got)
	}
	// allStatuses 读取时触发惰性清理
	_ = p.allStatuses()
	p.mu.RLock()
	_, still := p.recs["m1"]
	p.mu.RUnlock()
	if still {
		t.Fatal("过期记录未被惰性清理")
	}
}

// TestAckEmptyMsgIDNoop：空 msgId 不写入（防御）。
func TestAckEmptyMsgIDNoop(t *testing.T) {
	p := newTestAck(t)
	p.Track("", false, "agentB")
	p.Ack("", statusDelivered, "agentB")
	if len(p.recs) != 0 {
		t.Fatalf("空 msgId 不应产生记录，实得 %d 条", len(p.recs))
	}
}

// TestAckMaxRecordsCap：超上限后整体裁剪到 maxRecords。
func TestAckMaxRecordsCap(t *testing.T) {
	p := newTestAck(t)
	p.maxRecords = 10
	for i := 0; i < 25; i++ {
		p.Track(fmt.Sprintf("m-%d", i), true, "x")
	}
	p.mu.RLock()
	n := len(p.recs)
	p.mu.RUnlock()
	if n > p.maxRecords {
		t.Fatalf("记录数 %d 超过上限 %d", n, p.maxRecords)
	}
}
