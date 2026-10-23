package core

import (
	"strings"
	"sync"
	"testing"

	"lan-agent-hub/internal/protocol"
)

// fakeTracker 记录 Hub 对 DeliveryTracker 的调用，供断言接线是否正确。
type fakeTracker struct {
	mu      sync.Mutex
	tracks  []trackCall
	acks    []ackCall
}

type trackCall struct {
	msgID     string
	delivered bool
	target    string
}

type ackCall struct {
	msgID  string
	status string
	from   string
}

func (f *fakeTracker) Track(msgID string, delivered bool, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracks = append(f.tracks, trackCall{msgID, delivered, target})
}

func (f *fakeTracker) Ack(msgID string, status string, from string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, ackCall{msgID, status, from})
}

func (f *fakeTracker) lastTrack() trackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tracks[len(f.tracks)-1]
}

func (f *fakeTracker) trackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tracks)
}

// TestDeliveryTrackerWiring：证明 V6 ② 的三条接线（在线 pending / 离线 failed / 回执 delivered）。
func TestDeliveryTrackerWiring(t *testing.T) {
	hub := NewHub()
	ft := &fakeTracker{}
	hub.SetDeliveryTracker(ft)

	a := &mockSession{id: "A"}
	b := &mockSession{id: "B"}
	hub.HandleEnvelope(a, &protocol.Envelope{Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`)})
	hub.HandleEnvelope(b, &protocol.Envelope{Type: protocol.TypeRegister, From: "B", Content: raw(`{"name":"Bob"}`)})

	// 1) 点对点在线 → Track(delivered=true, target=B)
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeChat, From: "A", To: "B", MsgID: "m-online", Content: raw(`{"text":"hi"}`),
	})
	if got := ft.lastTrack(); !got.delivered || got.msgID != "m-online" || got.target != "B" {
		t.Fatalf("在线投递应 Track(true, B)，实得 %+v", got)
	}

	// 2) 点对点离线 → Track(delivered=false, target=ghost)
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeChat, From: "A", To: "ghost", MsgID: "m-offline", Content: raw(`{"text":"hi"}`),
	})
	if got := ft.lastTrack(); got.delivered || got.msgID != "m-offline" || got.target != "ghost" {
		t.Fatalf("离线投递应 Track(false, ghost)，实得 %+v", got)
	}

	// 3) 广播 → Track(delivered=true, target="")
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeBroadcast, From: "A", MsgID: "m-bcast", Content: raw(`{"text":"all"}`),
	})
	if got := ft.lastTrack(); !got.delivered || got.msgID != "m-bcast" || got.target != "" {
		t.Fatalf("广播应 Track(true, \"\")，实得 %+v", got)
	}
	if ft.trackCount() != 3 {
		t.Fatalf("应共 3 次 Track，实得 %d", ft.trackCount())
	}

	// 4) TypeAck → Ack(msgID, status, from) + 发送方收到 ack_received 系统回执
	hub.HandleEnvelope(b, &protocol.Envelope{
		Type: protocol.TypeAck, From: "B", MsgID: "m-online",
		Content: raw(`{"msgId":"m-online","status":"delivered"}`),
	})
	if len(ft.acks) != 1 || ft.acks[0].msgID != "m-online" || ft.acks[0].status != "delivered" || ft.acks[0].from != "B" {
		t.Fatalf("TypeAck 应触发一次 Ack(m-online, delivered, B)，实得 %+v", ft.acks)
	}
	ackEnv, ok := b.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeSystem && strings.Contains(string(e.Content), "ack_received")
	})
	if !ok {
		t.Fatal("ack 发送方应收到 ack_received 系统回执")
	}
	if c := contentOf(t, ackEnv); c["msgId"] != "m-online" {
		t.Fatalf("ack_received 应回带 msgId，实得 %v", c)
	}
}

// TestTypeAckWithoutTrackerFallsBack：未挂载 ack 时 TypeAck 必须退回 unknown type 语义（历史行为）。
func TestTypeAckWithoutTrackerFallsBack(t *testing.T) {
	hub := NewHub() // 未 SetDeliveryTracker
	a := &mockSession{id: "A"}
	hub.HandleEnvelope(a, &protocol.Envelope{Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`)})

	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeAck, From: "A", Content: raw(`{"msgId":"m1","status":"delivered"}`),
	})
	env, ok := a.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeSystem && strings.Contains(string(e.Content), "unknown type")
	})
	if !ok {
		t.Fatal("无 tracker 时 TypeAck 应回 unknown type（历史契约）")
	}
	if c := contentOf(t, env); c["code"] != protocol.CodeUnknownType {
		t.Fatalf("unknown type 应带 CodeUnknownType，实得 %v", c)
	}
}

// TestDeliveryTrackerNilSafe：tracker 可被卸载（传 nil），之后路由不再调用。
func TestDeliveryTrackerNilSafe(t *testing.T) {
	hub := NewHub()
	ft := &fakeTracker{}
	hub.SetDeliveryTracker(ft)
	a := &mockSession{id: "A"}
	b := &mockSession{id: "B"}
	hub.HandleEnvelope(a, &protocol.Envelope{Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`)})
	hub.HandleEnvelope(b, &protocol.Envelope{Type: protocol.TypeRegister, From: "B", Content: raw(`{"name":"Bob"}`)})

	hub.SetDeliveryTracker(nil) // 模拟插件 Stop 卸载钩子
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeChat, From: "A", To: "B", MsgID: "m1", Content: raw(`{"text":"hi"}`),
	})
	if ft.trackCount() != 0 {
		t.Fatalf("卸载后不应再调用 Track，实得 %d 次", ft.trackCount())
	}
}
