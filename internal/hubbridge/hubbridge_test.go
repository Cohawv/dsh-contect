package hubbridge

import (
	"strings"
	"testing"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/protocol"
)

// stubSession 是 core.Session 的内存实现（hubbridge 测试自用）。
type stubSession struct {
	id  string
	got []protocol.Envelope
}

func (s *stubSession) ID() string { return s.id }

func (s *stubSession) Deliver(e protocol.Envelope) { s.got = append(s.got, e) }

func (s *stubSession) Close() error { return nil }

func rigHub(t *testing.T) (*core.Hub, *stubSession, *stubSession) {
	t.Helper()
	hub := core.NewHub()
	a := &stubSession{id: "A"}
	b := &stubSession{id: "B"}
	hub.Registry.Bind("A", "Alice", []string{"python", "camera"}, protocol.ProtoCurrent, a)
	hub.Registry.Bind("B", "Bob", []string{"python"}, protocol.ProtoCurrent, b)
	return hub, a, b
}

// TestViewAgentsCarriesCaps：View.Agents 必须带 caps（capdir 动态分组的依据）。
func TestViewAgentsCarriesCaps(t *testing.T) {
	hub, _, _ := rigHub(t)
	agents := NewView(hub).Agents()
	if len(agents) != 2 {
		t.Fatalf("期望 2 个 agent，实得 %d", len(agents))
	}
	byID := map[string]string{}
	for _, a := range agents {
		byID[a.ID] = strings.Join(a.Caps, ",")
	}
	if byID["A"] != "python,camera" {
		t.Fatalf("A 的 caps 映射错误: %q", byID["A"])
	}
	if byID["B"] != "python" {
		t.Fatalf("B 的 caps 映射错误: %q", byID["B"])
	}
}

// TestViewRoomsAndStats：房间快照与计数。
func TestViewRoomsAndStats(t *testing.T) {
	hub, _, _ := rigHub(t)
	hub.Rooms.Create("general", []string{"A", "B"})

	v := NewView(hub)
	rooms := v.Rooms()
	if len(rooms) != 1 {
		t.Fatalf("期望 1 个房间，实得 %d", len(rooms))
	}
	if rooms[0].Name != "general" || rooms[0].MemberCount != 2 || len(rooms[0].Members) != 2 {
		t.Fatalf("房间快照不正确: %+v", rooms[0])
	}
	st := v.Stats()
	if st.AgentCount != 2 || st.RoomCount != 1 || st.ProtoVersion != protocol.ProtoCurrent {
		t.Fatalf("统计不正确: %+v", st)
	}
}

// TestPublisherSendAssignsSeqAndDelivers：Send 走 Router.Route——必须真的投递且分配 seq。
func TestPublisherSendAssignsSeqAndDelivers(t *testing.T) {
	hub, a, _ := rigHub(t)
	p := NewPublisher(hub)

	err := p.Send(protocol.NewEnvelope(protocol.TypeChat, "tester", "A", "", map[string]any{"text": "hi"}))
	if err != nil {
		t.Fatalf("在线目标 Send 不应报错: %v", err)
	}
	if len(a.got) != 1 {
		t.Fatalf("A 应收到 1 条消息，实得 %d", len(a.got))
	}
	if a.got[0].Seq == 0 {
		t.Fatal("Publisher.Send 必须经内核分配 seq（否则 store/回放对不上）")
	}
}

// TestPublisherSendOfflineReturnsError：目标离线必须返 error（调用方据此对账）。
func TestPublisherSendOfflineReturnsError(t *testing.T) {
	hub, _, _ := rigHub(t)
	p := NewPublisher(hub)

	err := p.Send(protocol.NewEnvelope(protocol.TypeChat, "tester", "ghost", "", map[string]any{"text": "hi"}))
	if err == nil {
		t.Fatal("离线目标 Send 应返回 error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error 应点名离线目标: %v", err)
	}
}

// TestPublisherBroadcastReachesAll：Broadcast 送达全部在线 Agent。
func TestPublisherBroadcastReachesAll(t *testing.T) {
	hub, a, b := rigHub(t)
	p := NewPublisher(hub)

	if err := p.Broadcast("tester", "hello all"); err != nil {
		t.Fatalf("Broadcast 不应报错: %v", err)
	}
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("广播应达 A/B 各 1 条，实得 A=%d B=%d", len(a.got), len(b.got))
	}
}
