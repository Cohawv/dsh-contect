package core

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"lan-agent-hub/internal/protocol"
)

// mockSession 是 core.Session 的内存实现：
// 业务内核的测试完全不需要起端口、不需要网络。
type mockSession struct {
	mu  sync.Mutex
	id  string
	got []protocol.Envelope
}

func (m *mockSession) ID() string { return m.id }

func (m *mockSession) Deliver(e protocol.Envelope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, e)
}

func (m *mockSession) Close() error { return nil }

func (m *mockSession) find(pred func(protocol.Envelope) bool) (protocol.Envelope, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.got {
		if pred(e) {
			return e, true
		}
	}
	return protocol.Envelope{}, false
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func contentOf(t *testing.T, e protocol.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Content, &m); err != nil {
		t.Fatalf("unmarshal content %q: %v", string(e.Content), err)
	}
	return m
}

// 契约测试：注册回执必须保留旧 text 字段，否则老客户端立即失效。
func TestRegisterAckKeepsLegacyText(t *testing.T) {
	hub := NewHub()
	s := &mockSession{id: "A"}
	hub.HandleEnvelope(s, &protocol.Envelope{
		Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`),
	})

	env, ok := s.find(func(e protocol.Envelope) bool { return e.Type == protocol.TypeSystem })
	if !ok {
		t.Fatal("no system ack after register")
	}
	c := contentOf(t, env)
	if c["text"] != "registered as A" {
		t.Fatalf("legacy text contract broken: %v", c["text"])
	}
	if c["action"] != protocol.ActionRegistered {
		t.Fatalf("structured action missing: %v", c)
	}
}

// v1 客户端（不声明 v）不应收到会改变其行为的新字段。
func TestLegacyClientGetsNoNegotiatedFields(t *testing.T) {
	hub := NewHub()
	s := &mockSession{id: "A"}
	hub.HandleEnvelope(s, &protocol.Envelope{
		Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`),
	})
	env, _ := s.find(func(e protocol.Envelope) bool { return e.Type == protocol.TypeSystem })
	c := contentOf(t, env)
	for _, k := range []string{"proto", "serverCaps", "heartbeatMs", "timeoutMs"} {
		if _, ok := c[k]; ok {
			t.Fatalf("v1 client should not receive %q", k)
		}
	}
}

// v2 客户端应收到完整协商结果。
func TestModernClientGetsNegotiatedFields(t *testing.T) {
	hub := NewHub()
	s := &mockSession{id: "A"}
	hub.HandleEnvelope(s, &protocol.Envelope{
		V: 2, Type: protocol.TypeRegister, From: "A",
		Content: raw(`{"name":"Alice","caps":["structured-ack"]}`),
	})
	env, _ := s.find(func(e protocol.Envelope) bool { return e.Type == protocol.TypeSystem })
	c := contentOf(t, env)
	if c["proto"] != float64(2) {
		t.Fatalf("proto = %v, want 2", c["proto"])
	}
	if c["heartbeatMs"] == nil || c["timeoutMs"] == nil {
		t.Fatalf("heartbeat negotiation fields missing: %v", c)
	}
}

// 端到端业务场景（迁移自原根目录 hub_test.go，改用内存 session）
func TestPointToPointRoomAndOffline(t *testing.T) {
	hub := NewHub()
	a := &mockSession{id: "A"}
	b := &mockSession{id: "B"}

	hub.HandleEnvelope(a, &protocol.Envelope{Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`)})
	hub.HandleEnvelope(b, &protocol.Envelope{Type: protocol.TypeRegister, From: "B", Content: raw(`{"name":"Bob"}`)})

	// 1) 私聊
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeChat, From: "A", To: "B", Content: raw(`{"text":"hello B"}`),
	})
	env, ok := b.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeChat && strings.Contains(string(e.Content), "hello B")
	})
	if !ok {
		t.Fatal("B should receive private message")
	}
	if env.Seq == 0 {
		t.Fatal("routed message must carry a seq")
	}

	// 2) 建房间并把 B 拉进来
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeCreateRoom, From: "A", Content: raw(`{"name":"general"}`),
	})
	created, ok := a.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeSystem && strings.Contains(string(e.Content), `"created"`)
	})
	if !ok {
		t.Fatal("no created ack")
	}
	roomID := contentOf(t, created)["roomId"].(string)
	if roomID == "" {
		t.Fatal("empty roomId")
	}
	hub.HandleEnvelope(b, &protocol.Envelope{
		Type: protocol.TypeJoin, From: "B", Content: raw(`{"roomId":"` + roomID + `"}`),
	})

	// 3) 房间消息
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeRoom, From: "A", RoomID: roomID, Content: raw(`{"text":"room hi"}`),
	})
	if _, ok := b.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeRoom && strings.Contains(string(e.Content), "room hi")
	}); !ok {
		t.Fatal("B should receive room message")
	}

	// 4) 目标离线提示
	hub.HandleEnvelope(a, &protocol.Envelope{
		Type: protocol.TypeChat, From: "A", To: "ghost", Content: raw(`{"text":"x"}`),
	})
	ack, ok := a.find(func(e protocol.Envelope) bool {
		return e.Type == protocol.TypeSystem && strings.Contains(string(e.Content), "target offline")
	})
	if !ok {
		t.Fatal("should notify target offline")
	}
	c := contentOf(t, ack)
	if c["action"] != protocol.ActionUndelivered || c["code"] != protocol.CodeTargetOffline {
		t.Fatalf("undelivered ack should carry action/code: %v", c)
	}
}

// 下线必须清理在线表与房间成员，并发布 offline 事件。
func TestUnregisterDropsFromRoomsAndPublishesEvent(t *testing.T) {
	hub := NewHub()
	a := &mockSession{id: "A"}
	hub.HandleEnvelope(a, &protocol.Envelope{Type: protocol.TypeRegister, From: "A", Content: raw(`{"name":"Alice"}`)})
	roomID := hub.Rooms.Create("r", []string{"A"})

	var offline string
	hub.Bus.Subscribe(protocol.TopicAgentOffline, func(_ string, payload any) {
		offline, _ = payload.(string)
	})

	hub.Unregister("A")

	if offline != "A" {
		t.Fatalf("offline event = %q, want A", offline)
	}
	if _, ok := hub.Registry.Get("A"); ok {
		t.Fatal("A should be removed from registry")
	}
	if m := hub.Rooms.Members(roomID); len(m) != 0 {
		t.Fatalf("A should be removed from room, got %v", m)
	}
}
