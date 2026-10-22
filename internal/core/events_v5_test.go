package core

import (
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/protocol"
)

// TestRoomChangedPublished 验证 room.changed 不再是死契约：
// Create / Join / Leave / Drop 四个动作都会发布，且载荷是变更后的房间快照。
func TestRoomChangedPublished(t *testing.T) {
	hub := NewHub()

	var mu sync.Mutex
	var got []RoomChanged
	hub.Bus.Subscribe(protocol.TopicRoomChanged, func(_ string, payload any) {
		ev, ok := payload.(RoomChanged)
		if !ok {
			t.Errorf("payload 类型 = %T，期望 core.RoomChanged", payload)
			return
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	take := func() []RoomChanged {
		mu.Lock()
		defer mu.Unlock()
		out := append([]RoomChanged(nil), got...)
		got = nil
		return out
	}

	// ① Create
	id := hub.Rooms.Create("运维值班", []string{"A"})
	evs := take()
	if len(evs) != 1 {
		t.Fatalf("Create 后事件数 = %d，期望 1", len(evs))
	}
	if evs[0].Action != RoomActionCreated {
		t.Errorf("action = %q，期望 %q", evs[0].Action, RoomActionCreated)
	}
	if evs[0].Room.ID != id || evs[0].Room.Name != "运维值班" {
		t.Errorf("房间快照 = %+v", evs[0].Room)
	}
	if evs[0].Room.MemberCount != 1 || len(evs[0].Room.Members) != 1 {
		t.Errorf("成员快照 = %+v", evs[0].Room)
	}

	// ② Join
	hub.Rooms.Join(id, "B")
	evs = take()
	if len(evs) != 1 || evs[0].Action != RoomActionJoined || evs[0].Actor != "B" {
		t.Fatalf("Join 事件 = %+v", evs)
	}
	if evs[0].Room.MemberCount != 2 {
		t.Errorf("Join 后成员数 = %d，期望 2", evs[0].Room.MemberCount)
	}

	// ③ Leave
	hub.Rooms.Leave(id, "B")
	evs = take()
	if len(evs) != 1 || evs[0].Action != RoomActionLeft || evs[0].Actor != "B" {
		t.Fatalf("Leave 事件 = %+v", evs)
	}

	// ④ Drop（下线时从所有房间移除）
	hub.Rooms.Join(id, "C")
	take() // 丢掉上面 Join 的事件
	hub.Rooms.Drop("A")
	evs = take()
	if len(evs) != 1 {
		t.Fatalf("Drop 后事件数 = %d，期望 1（A 只在一个房间）", len(evs))
	}
	if evs[0].Action != RoomActionDropped || evs[0].Actor != "A" {
		t.Fatalf("Drop 事件 = %+v", evs)
	}
	if evs[0].Room.MemberCount != 1 {
		t.Errorf("Drop 后成员数 = %d，期望 1（只剩 C）", evs[0].Room.MemberCount)
	}

	// ⑤ 无变更不发布：对不存在的房间操作不应制造噪声事件。
	hub.Rooms.Join("room-nope", "A")
	hub.Rooms.Leave("room-nope", "A")
	if evs := take(); len(evs) != 0 {
		t.Fatalf("对不存在房间的操作不应发布事件，实际 %+v", evs)
	}
}

// TestMsgRoutedPublished 验证 Route 之后会发布 msg.routed，且载荷只含计数与分类。
func TestMsgRoutedPublished(t *testing.T) {
	hub := NewHub()
	var mu sync.Mutex
	var got []MsgRouted
	hub.Bus.Subscribe(protocol.TopicMsgRouted, func(_ string, payload any) {
		ev, ok := payload.(MsgRouted)
		if !ok {
			t.Errorf("payload 类型 = %T，期望 core.MsgRouted", payload)
			return
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	sess := &fakeSession{}
	hub.Registry.Bind("A", "agentA", nil, protocol.ProtoCurrent, sess)
	hub.Registry.Bind("B", "agentB", nil, protocol.ProtoCurrent, sess)

	// 点对点：送达 → delivered=1
	hub.Router.Route(protocol.NewEnvelope(protocol.TypeChat, "A", "B", "", map[string]string{"text": "hi"}))
	// 点对点：目标离线 → delivered=0
	hub.Router.Route(protocol.NewEnvelope(protocol.TypeChat, "A", "ghost", "", map[string]string{"text": "hi"}))
	// 广播：两个在线 → delivered=2
	hub.Router.Route(protocol.NewEnvelope(protocol.TypeBroadcast, "A", "", "", map[string]string{"text": "yo"}))

	mu.Lock()
	events := append([]MsgRouted(nil), got...)
	mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("msg.routed 事件数 = %d，期望 3", len(events))
	}
	if events[0].Kind != RouteKindDirect || events[0].Delivered != 1 {
		t.Errorf("第 1 条 = %+v", events[0])
	}
	if events[1].Kind != RouteKindDirect || events[1].Delivered != 0 {
		t.Errorf("第 2 条（目标离线）= %+v", events[1])
	}
	if events[2].Kind != RouteKindBroadcast || events[2].Delivered != 2 {
		t.Errorf("第 3 条（广播）= %+v", events[2])
	}
	if events[0].Seq == 0 || events[2].Seq <= events[0].Seq {
		t.Errorf("seq 应单调递增：%+v", events)
	}
}

// TestRoomChangedPayloadIsSmall 断言事件载荷不含内部引用（只有快照值）。
// 这条测试防的是「有人图省事把 *Room 塞进事件」，那会让订阅方持有可变共享状态。
func TestRoomChangedPayloadIsSmall(t *testing.T) {
	var ev any
	bus := NewEventBus()
	bus.Subscribe(protocol.TopicRoomChanged, func(_ string, payload any) { ev = payload })
	rooms := NewRoomService()
	rooms.SetBus(bus)
	rooms.Create("x", nil)

	if _, ok := ev.(RoomChanged); !ok {
		t.Fatalf("payload = %T，期望值类型 core.RoomChanged", ev)
	}
}

// TestMsgRoutedHotPathCost 对比「有订阅者 / 无订阅者」两种情况下 1000 条消息的耗时。
//
// 这是 V5 的风险 RK3 的量化闸门：msg.routed 位于消息热路径，
// 只要裸发布（无订阅者）没有数量级劣化，就可以保留这个发布点。
func TestMsgRoutedHotPathCost(t *testing.T) {
	const messages = 1000
	sess := &fakeSession{}

	measure := func(withSubscriber bool) time.Duration {
		hub := NewHub()
		hub.Registry.Bind("A", "agentA", nil, protocol.ProtoCurrent, sess)
		hub.Registry.Bind("B", "agentB", nil, protocol.ProtoCurrent, sess)
		var cancel func()
		if withSubscriber {
			cancel = hub.Bus.SubscribeAsync(protocol.TopicMsgRouted, func(string, any) {}, 256)
			defer cancel()
		}
		env := protocol.NewEnvelope(protocol.TypeChat, "A", "B", "", map[string]string{"text": "x"})
		start := time.Now()
		for i := 0; i < messages; i++ {
			hub.Router.Route(env)
		}
		return time.Since(start)
	}

	// 预热（避免首次分配主导测量）。
	measure(false)
	measure(true)

	without := measure(false)
	with := measure(true)

	perWithout := float64(without.Nanoseconds()) / messages
	perWith := float64(with.Nanoseconds()) / messages
	t.Logf("实测 %d 条消息：无订阅者 %s（%.0f ns/条），有异步订阅者 %s（%.0f ns/条）",
		messages, without, perWithout, with, perWith)

	// 闸门：单条消息的总耗时必须仍在微秒级（远低于一次 TCP 写）。
	// 绝对阈值比相对倍数更抗抖动（异步订阅者会在另一个 goroutine 里消费，测量噪声大）。
	const budgetPerMessage = 20 * time.Microsecond
	if time.Duration(perWithout) > budgetPerMessage {
		t.Fatalf("无订阅者时单条 %v 超过预算 %v（msg.routed 发布点过重）",
			time.Duration(perWithout), budgetPerMessage)
	}
	if time.Duration(perWith) > budgetPerMessage {
		t.Fatalf("有订阅者时单条 %v 超过预算 %v（异步派发不应阻塞发布方）",
			time.Duration(perWith), budgetPerMessage)
	}
}

// fakeSession 是只记录投递的 Session 替身。
type fakeSession struct {
	mu   sync.Mutex
	envs []protocol.Envelope
}

func (s *fakeSession) Deliver(env protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs = append(s.envs, env)
}

func (s *fakeSession) ID() string { return "fake" }

func (s *fakeSession) Close() error { return nil }

func (s *fakeSession) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}
