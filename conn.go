package main

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"time"
)

// writeLoop 单连接写协程：从 sendCh 取数据写入底层传输
func (a *Agent) writeLoop() {
	for data := range a.sendCh {
		if err := a.send(data); err != nil {
			return
		}
	}
}

// handleConn 处理单个 TCP 连接：每连接 1 读协程 + 1 写协程
func (h *Hub) handleConn(conn net.Conn) {
	a := &Agent{
		send:      func(b []byte) error { _, err := conn.Write(b); return err },
		closeConn: conn.Close,
		sendCh:    make(chan []byte, 256),
	}
	go a.writeLoop()
	defer func() {
		h.unregister(a)
		if a.closeConn != nil {
			_ = a.closeConn()
		}
		close(a.sendCh)
	}()

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			a.deliver(newSystem(a.ID, "bad json: "+err.Error()))
			continue
		}
		h.handleEnvelope(a, &env)
	}
}

// handleEnvelope 分发 Control / 消息类信封
func (h *Hub) handleEnvelope(a *Agent, env *Envelope) {
	switch env.Type {
	case TypeRegister:
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(env.Content, &p)
		h.register(a, env.From, p.Name)
	case TypeHeartbeat:
		a.lastSeen = time.Now().UnixMilli()
	case TypeChat, TypeRoom, TypeBroadcast:
		h.route(*env)
	case TypeCreateRoom:
		var p struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}
		_ = json.Unmarshal(env.Content, &p)
		id := h.createRoom(p.Name, p.Members)
		a.deliver(newEnvelope(TypeSystem, "hub", a.ID, "", map[string]string{"action": "created", "roomId": id}))
	case TypeJoin:
		var p struct {
			RoomID string `json:"roomId"`
		}
		_ = json.Unmarshal(env.Content, &p)
		if p.RoomID != "" {
			h.joinRoom(p.RoomID, a.ID)
		}
	case TypeLeave:
		var p struct {
			RoomID string `json:"roomId"`
		}
		_ = json.Unmarshal(env.Content, &p)
		if p.RoomID != "" {
			h.leaveRoom(p.RoomID, a.ID)
		}
	case TypeList:
		a.deliver(newEnvelope(TypeSystem, "hub", a.ID, "", map[string]any{"action": "list", "agents": h.listAgents()}))
	default:
		a.deliver(newSystem(a.ID, "unknown type: "+env.Type))
	}
}
