package core

import (
	"sync"

	"lan-agent-hub/internal/protocol"
)

// Room 聊天室（领域对象，不含传输细节）
type Room struct {
	ID      string
	Name    string
	Members map[string]bool
}

// RoomSnapshot 是房间的纯值快照（room.changed 事件的 room 字段）。
// 与 RoomService.List 的响应字段一一对应，便于控制台直接复用渲染逻辑。
type RoomSnapshot struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	MemberCount int      `json:"memberCount"`
	Members     []string `json:"members"`
}

// RoomChanged 是 protocol.TopicRoomChanged 的事件载荷。
//
// 只放快照与小字段（动作 + 触发者），不放 *Room 或任何内部引用——
// 订阅者（SSE 推送 / 指标 / 将来的持久化）拿到的是不可变的值副本。
type RoomChanged struct {
	// Action 是触发动作：created | joined | left | dropped。
	Action string `json:"action"`
	// Actor 是触发者 agentId（created 时为空，dropped 时为下线者）。
	Actor string `json:"actor,omitempty"`
	// Room 是变更后的房间快照。
	Room RoomSnapshot `json:"room"`
}

// 房间变更动作取值（RoomChanged.Action）。
const (
	RoomActionCreated = "created"
	RoomActionJoined  = "joined"
	RoomActionLeft    = "left"
	RoomActionDropped = "dropped"
	// RoomActionDeleted / RoomActionRenamed 由房间管理端点（POST /api/rooms/*）触发。
	// 系统没有权限体系（前置决策排除），因此这两个动作的入口必须刻意设在
	// 「需要明确构造请求」的层面——控制台 UI 还要二次确认——把误删成本抬高到
	// 不可能手滑触发，这是无权限体系下仅存的防线。
	RoomActionDeleted = "deleted"
	RoomActionRenamed = "renamed"
)

// RoomService 管理房间与成员。
//
// V5：本服务是 protocol.TopicRoomChanged 的**发布方**。
// 该 topic 从 V1 起就定义在协议里，但一直没有发布方（死契约）——
// 补发布方既修掉了这个契约缺陷，也让「实时推送房间变化」成为可能。
//
// 发布纪律：**只在锁外发布**（先改状态、取快照、解锁，再 Publish），
// 避免订阅方回调时反向读房间表造成锁等待甚至死锁。
type RoomService struct {
	mu    sync.RWMutex
	rooms map[string]*Room

	// bus 为 nil 时（单测/无总线场景）静默不发布，行为与 V4 完全一致。
	bus *EventBus
}

func NewRoomService() *RoomService {
	return &RoomService{rooms: make(map[string]*Room)}
}

// SetBus 注入事件总线。仅由 NewHub 在装配期调用一次（此后只读，无需加锁）。
func (s *RoomService) SetBus(bus *EventBus) {
	s.bus = bus
}

// Create 创建房间并返回其 ID。ID 格式由服务端决定，客户端只存储不解析。
func (s *RoomService) Create(name string, members []string) string {
	id := "room-" + protocol.UUID()[:8]
	s.mu.Lock()
	room := &Room{ID: id, Name: name, Members: map[string]bool{}}
	for _, m := range members {
		room.Members[m] = true
	}
	s.rooms[id] = room
	snap := roomSnapshot(room)
	s.mu.Unlock()

	s.publish(RoomActionCreated, "", snap)
	return id
}

func (s *RoomService) Join(roomID, agentID string) {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return // 房间不存在：不产生变更，也就不发布（避免噪声事件）
	}
	r.Members[agentID] = true
	snap := roomSnapshot(r)
	s.mu.Unlock()

	// 重复加入也发布：载荷是幂等的完整快照，订阅方重复消费不会导致状态漂移，
	// 而"有人明确加入过"本身是控制台需要看到的事实。
	s.publish(RoomActionJoined, agentID, snap)
}

func (s *RoomService) Leave(roomID, agentID string) {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return
	}
	_, existed := r.Members[agentID]
	delete(r.Members, agentID)
	snap := roomSnapshot(r)
	s.mu.Unlock()

	if !existed {
		return // 本来就不在房间里：无变更，不必发布
	}
	s.publish(RoomActionLeft, agentID, snap)
}

// List 返回所有房间的快照（含成员），供 HTTP 网关 / 控制台展示用。
// 与 Members 一样返回副本，避免调用方持有内部 map。
func (s *RoomService) List() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.rooms))
	for _, r := range s.rooms {
		members := make([]string, 0, len(r.Members))
		for m := range r.Members {
			members = append(members, m)
		}
		out = append(out, map[string]any{
			"id":          r.ID,
			"name":        r.Name,
			"memberCount": len(members),
			"members":     members,
		})
	}
	return out
}

// Members 返回房间成员快照（副本，避免调用方持有内部 map）。
func (s *RoomService) Members(roomID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rooms[roomID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(r.Members))
	for m := range r.Members {
		out = append(out, m)
	}
	return out
}

// Drop 在 Agent 下线时把它从所有房间移除。
//
// 每个**确实发生变化**的房间各发一条 room.changed（动作 dropped，actor=下线者），
// 载荷是该房间变更后的完整快照——控制台据此逐个房间增量刷新。
func (s *RoomService) Drop(agentID string) {
	s.mu.Lock()
	var changed []RoomSnapshot
	for _, r := range s.rooms {
		if !r.Members[agentID] {
			continue
		}
		delete(r.Members, agentID)
		changed = append(changed, roomSnapshot(r))
	}
	s.mu.Unlock()

	for _, snap := range changed {
		s.publish(RoomActionDropped, agentID, snap)
	}
}

// Delete 删除整个房间并通知所有订阅方。
//
// 返回 false 表示房间不存在（幂等：对已删除的房间重复删除是 no-op 而不是错误——
// 并发下两个删除请求只有一个能删到，另一个不该报 500）。
//
// 事件载荷是**删除前**的最后快照：订阅方（控制台/SSE）需要 id 才知道删的是谁，
// 成员列表留作审计线索（"删的时候里面有谁"）。
func (s *RoomService) Delete(roomID string) bool {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	snap := roomSnapshot(r)
	delete(s.rooms, roomID)
	s.mu.Unlock()

	s.publish(RoomActionDeleted, "", snap)
	return true
}

// Rename 修改房间显示名。返回 false 表示房间不存在。
//
// 房间 ID 刻意不变：ID 是成员端的接入锚点（join/leave/say 都拿 ID），
// 改名只是显示层的事——如果连 ID 一起换，所有成员会话瞬间失效，
// 那就不是改名而是重建。
func (s *RoomService) Rename(roomID, newName string) bool {
	s.mu.Lock()
	r, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	if r.Name == newName {
		s.mu.Unlock()
		return true // 同名改名：无变更不发布（避免噪声事件，与 Leave 的纪律一致）
	}
	r.Name = newName
	snap := roomSnapshot(r)
	s.mu.Unlock()

	s.publish(RoomActionRenamed, "", snap)
	return true
}

// publish 发布一条房间变更事件（锁外调用；bus 为 nil 时静默）。
func (s *RoomService) publish(action, actor string, snap RoomSnapshot) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(protocol.TopicRoomChanged, RoomChanged{Action: action, Actor: actor, Room: snap})
}

// roomSnapshot 在持锁状态下构造房间快照（调用方负责持锁）。
func roomSnapshot(r *Room) RoomSnapshot {
	members := make([]string, 0, len(r.Members))
	for m := range r.Members {
		members = append(members, m)
	}
	return RoomSnapshot{
		ID:          r.ID,
		Name:        r.Name,
		MemberCount: len(members),
		Members:     members,
	}
}
