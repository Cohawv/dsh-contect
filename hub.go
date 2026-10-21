package main

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Agent 代表一个已接入的客户端（TCP / HTTP / WS 等任意传输）
type Agent struct {
	ID        string
	Name      string
	send      func([]byte) error // 底层写方法，由具体传输注入
	closeConn func() error       // 底层关闭方法
	sendCh    chan []byte        // 发送缓冲，避免写阻塞
	lastSeen  int64              // 最近心跳时间（毫秒）
}

// Room 聊天室
type Room struct {
	ID      string
	Name    string
	Members map[string]bool
}

// Hub 中心服务器核心：注册表 + 路由 + 房间 + 广播
type Hub struct {
	mu     sync.RWMutex
	agents map[string]*Agent
	rooms  map[string]*Room
	seq    int64 // 全局单调序号
}

func NewHub() *Hub {
	return &Hub{
		agents: make(map[string]*Agent),
		rooms:  make(map[string]*Room),
	}
}

// deliver 非阻塞发送：信道满则丢弃，保证不阻塞、不拥堵（MVP 取舍）
func (a *Agent) deliver(env Envelope) {
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	b = append(b, '\n')
	defer func() { _ = recover() }() // 防止向已关闭信道发送导致 panic
	select {
	case a.sendCh <- b:
	default:
	}
}

func (h *Hub) register(a *Agent, id, name string) {
	h.mu.Lock()
	a.ID = id
	a.Name = name
	h.agents[id] = a
	h.mu.Unlock()
	a.lastSeen = time.Now().UnixMilli()
	a.deliver(newSystem(id, "registered as "+id))
	h.presence(a.ID, "online")
}

func (h *Hub) unregister(a *Agent) {
	if a.ID == "" {
		return
	}
	h.mu.Lock()
	delete(h.agents, a.ID)
	for _, room := range h.rooms {
		delete(room.Members, a.ID)
	}
	h.mu.Unlock()
	h.presence(a.ID, "offline")
}

func (h *Hub) presence(id, status string) {
	env := newEnvelope(TypePresence, "hub", "", "", map[string]string{"id": id, "status": status})
	h.broadcastAll(env)
}

// route 分配全局单调序号后按目标转发
func (h *Hub) route(env Envelope) {
	if env.MsgID == "" {
		env.MsgID = uuid()
	}
	env.Seq = atomic.AddInt64(&h.seq, 1)
	env.TS = time.Now().UnixMilli()
	switch {
	case env.RoomID != "":
		h.broadcastRoom(env)
	case env.To != "":
		h.deliverTo(env.To, env)
	default:
		h.broadcastAll(env)
	}
}

func (h *Hub) deliverTo(id string, env Envelope) {
	h.mu.RLock()
	a, ok := h.agents[id]
	h.mu.RUnlock()
	if !ok {
		if s, ok2 := h.agents[env.From]; ok2 {
			s.deliver(newSystem(env.From, "target offline: "+id))
		}
		return
	}
	a.deliver(env)
}

func (h *Hub) broadcastRoom(env Envelope) {
	h.mu.RLock()
	room, ok := h.rooms[env.RoomID]
	agents := h.agents
	members := make([]string, 0, len(room.Members))
	if ok {
		for m := range room.Members {
			members = append(members, m)
		}
	}
	h.mu.RUnlock()
	for _, m := range members {
		if a, ok := agents[m]; ok {
			a.deliver(env)
		}
	}
}

func (h *Hub) broadcastAll(env Envelope) {
	h.mu.RLock()
	agents := make([]*Agent, 0, len(h.agents))
	for _, a := range h.agents {
		agents = append(agents, a)
	}
	h.mu.RUnlock()
	for _, a := range agents {
		a.deliver(env)
	}
}

func (h *Hub) createRoom(name string, members []string) string {
	id := "room-" + uuid()[:8]
	h.mu.Lock()
	room := &Room{ID: id, Name: name, Members: map[string]bool{}}
	for _, m := range members {
		room.Members[m] = true
	}
	h.rooms[id] = room
	h.mu.Unlock()
	return id
}

func (h *Hub) joinRoom(roomID, agentID string) {
	h.mu.Lock()
	if room, ok := h.rooms[roomID]; ok {
		room.Members[agentID] = true
	}
	h.mu.Unlock()
}

func (h *Hub) leaveRoom(roomID, agentID string) {
	h.mu.Lock()
	if room, ok := h.rooms[roomID]; ok {
		delete(room.Members, agentID)
	}
	h.mu.Unlock()
}

func (h *Hub) listAgents() []map[string]string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]map[string]string, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, map[string]string{"id": a.ID, "name": a.Name})
	}
	return out
}

// cleanupLoop 定期清理心跳超时的连接
func (h *Hub) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now().UnixMilli()
		h.mu.Lock()
		for id, a := range h.agents {
			if now-a.lastSeen > 60000 {
				delete(h.agents, id)
				for _, room := range h.rooms {
					delete(room.Members, id)
				}
				if a.closeConn != nil {
					go a.closeConn()
				}
			}
		}
		h.mu.Unlock()
	}
}
