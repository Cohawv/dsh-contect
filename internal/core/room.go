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

// RoomService 管理房间与成员。
type RoomService struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func NewRoomService() *RoomService {
	return &RoomService{rooms: make(map[string]*Room)}
}

// Create 创建房间并返回其 ID。ID 格式由服务端决定，客户端只存储不解析。
func (s *RoomService) Create(name string, members []string) string {
	id := "room-" + protocol.UUID()[:8]
	s.mu.Lock()
	defer s.mu.Unlock()
	room := &Room{ID: id, Name: name, Members: map[string]bool{}}
	for _, m := range members {
		room.Members[m] = true
	}
	s.rooms[id] = room
	return id
}

func (s *RoomService) Join(roomID, agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[roomID]; ok {
		r.Members[agentID] = true
	}
}

func (s *RoomService) Leave(roomID, agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[roomID]; ok {
		delete(r.Members, agentID)
	}
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
func (s *RoomService) Drop(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rooms {
		delete(r.Members, agentID)
	}
}
