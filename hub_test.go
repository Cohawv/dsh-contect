package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

type testEnv struct {
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	RoomID  string          `json:"roomId"`
	Content json.RawMessage `json:"content"`
}

func tMustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func tWrite(t *testing.T, c net.Conn, e testEnv) {
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// tReadUntil 持续读取直到某行包含 substr 或超时
func tReadUntil(c net.Conn, substr string) (string, error) {
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1)
	var last string
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			if tmp[0] == '\n' {
				line := string(buf)
				if strings.Contains(line, substr) {
					return line, nil
				}
				last = line
				buf = buf[:0]
			} else {
				buf = append(buf, tmp[0])
			}
		}
		if err != nil {
			return last, err
		}
	}
}

func TestPointToPointAndRoom(t *testing.T) {
	hub := NewHub()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go hub.handleConn(c)
		}
	}()

	addr := ln.Addr().String()
	a, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	tWrite(t, a, testEnv{Type: "register", From: "A", Content: tMustJSON(map[string]string{"name": "Alice"})})
	tWrite(t, b, testEnv{Type: "register", From: "B", Content: tMustJSON(map[string]string{"name": "Bob"})})

	// 1) A 私聊 B
	tWrite(t, a, testEnv{Type: "chat", From: "A", To: "B", Content: tMustJSON(map[string]string{"text": "hello B"})})
	line, err := tReadUntil(b, "hello B")
	if err != nil || !strings.Contains(line, "hello B") {
		t.Fatalf("B should receive private msg, got: %q err=%v", line, err)
	}

	// 2) 创建房间并把 B 拉进来
	tWrite(t, a, testEnv{Type: "create_room", From: "A", Content: tMustJSON(map[string]string{"name": "general"})})
	roomLine, err := tReadUntil(a, "\"created\"")
	if err != nil {
		t.Fatalf("create room response: %v", err)
	}
	var created struct {
		Content struct {
			RoomID string `json:"roomId"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(roomLine), &created); err != nil || created.Content.RoomID == "" {
		t.Fatalf("parse roomId failed: %s", roomLine)
	}
	roomID := created.Content.RoomID
	tWrite(t, b, testEnv{Type: "join", From: "B", Content: tMustJSON(map[string]string{"roomId": roomID})})
	time.Sleep(50 * time.Millisecond)

	// 3) A 在房间里发消息，B 应收到
	tWrite(t, a, testEnv{Type: "room", From: "A", RoomID: roomID, Content: tMustJSON(map[string]string{"text": "room hi"})})
	rl, err := tReadUntil(b, "room hi")
	if err != nil || !strings.Contains(rl, "room hi") {
		t.Fatalf("B should receive room msg, got: %q err=%v", rl, err)
	}

	// 4) 离线投递提示：B 不存在时应提示发送方
	tWrite(t, a, testEnv{Type: "chat", From: "A", To: "ghost", Content: tMustJSON(map[string]string{"text": "x"})})
	ol, err := tReadUntil(a, "target offline")
	if err != nil || !strings.Contains(ol, "target offline") {
		t.Fatalf("should notify target offline, got: %q err=%v", ol, err)
	}
}
