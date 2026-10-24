package core

import (
	"sync"
	"testing"

	"lan-agent-hub/internal/protocol"
)

// TestDeleteRoom 删除房间：从列表消失、发布 action=deleted、重复删除幂等返回 false。
func TestDeleteRoom(t *testing.T) {
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

	id := hub.Rooms.Create("临时作战室", []string{"A", "B"})
	take() // 清掉 created 事件

	if !hub.Rooms.Delete(id) {
		t.Fatal("Delete 应返回 true（房间存在）")
	}
	evs := take()
	if len(evs) != 1 || evs[0].Action != RoomActionDeleted {
		t.Fatalf("Delete 事件 = %+v，期望恰好一条 %q", evs, RoomActionDeleted)
	}
	// 快照是删除前的最后状态：id 与成员要留作审计线索
	if evs[0].Room.ID != id || evs[0].Room.MemberCount != 2 {
		t.Errorf("删除事件快照 = %+v，期望含 id 与删除前的 2 个成员", evs[0].Room)
	}

	for _, r := range hub.Rooms.List() {
		if r["id"] == id {
			t.Fatalf("删除后列表仍含房间 %v", r)
		}
	}
	if hub.Rooms.Delete(id) {
		t.Fatal("对已删除房间重复 Delete 应返回 false（幂等 no-op）")
	}
	if len(take()) != 0 {
		t.Fatal("重复删除不应再发布事件")
	}
	if hub.Rooms.Delete("room-nope") {
		t.Fatal("删除不存在的房间应返回 false")
	}
}

// TestRenameRoom 改名：列表反映新名、ID 不变、发布 action=renamed、同名改名无事件。
func TestRenameRoom(t *testing.T) {
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

	id := hub.Rooms.Create("旧名字", []string{"A"})
	take()

	if !hub.Rooms.Rename(id, "新名字") {
		t.Fatal("Rename 应返回 true（房间存在）")
	}
	evs := take()
	if len(evs) != 1 || evs[0].Action != RoomActionRenamed {
		t.Fatalf("Rename 事件 = %+v，期望恰好一条 %q", evs, RoomActionRenamed)
	}
	if evs[0].Room.Name != "新名字" {
		t.Errorf("改名后快照名 = %q，期望 新名字", evs[0].Room.Name)
	}
	found := false
	for _, r := range hub.Rooms.List() {
		if r["id"] == id {
			found = true
			if r["name"] != "新名字" {
				t.Errorf("List 中 name = %v，期望 新名字", r["name"])
			}
		}
	}
	if !found {
		t.Fatal("改名后列表中找不到该房间（ID 不应因改名丢失）")
	}

	// 同名改名：无变更不发布（与 Leave 的"无变更不发布"纪律一致）
	if !hub.Rooms.Rename(id, "新名字") {
		t.Fatal("同名改名应返回 true（房间在，操作成功但无事件）")
	}
	if len(take()) != 0 {
		t.Fatal("同名改名不应发布事件（噪声）")
	}
	if hub.Rooms.Rename("room-nope", "任意") {
		t.Fatal("对不存在的房间 Rename 应返回 false")
	}
}
