package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type env struct {
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	RoomID  string          `json:"roomId"`
	Content json.RawMessage `json:"content"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "hub address")
	id := flag.String("id", "", "agent id (required)")
	name := flag.String("name", "", "agent display name")
	flag.Parse()
	if *id == "" {
		log.Fatal("please provide -id")
	}

	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		log.Fatalf("dial %s failed: %v", *addr, err)
	}
	defer conn.Close()

	send(conn, env{Type: "register", From: *id, Content: mustJSON(map[string]string{"name": *name})})

	// 定时心跳，防止被服务端清理
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			send(conn, env{Type: "heartbeat", From: *id})
		}
	}()

	// 接收服务端消息
	go func() {
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				fmt.Println("[disconnected from hub]")
				os.Exit(0)
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fmt.Println("<<", line)
		}
	}()

	fmt.Printf("connected as %s. commands:\n", *id)
	fmt.Println("  @<id> msg        -> 私聊给某 agent")
	fmt.Println("  #<room> msg      -> 发到某聊天室")
	fmt.Println("  create:<name>    -> 创建聊天室")
	fmt.Println("  join:<roomId>    -> 加入聊天室")
	fmt.Println("  leave:<roomId>   -> 离开聊天室")
	fmt.Println("  who              -> 查看在线 agent")
	fmt.Println("  其它文字          -> 全员广播")

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "@"):
			parts := strings.SplitN(line[1:], " ", 2)
			if len(parts) == 2 {
				send(conn, env{Type: "chat", From: *id, To: parts[0], Content: mustJSON(map[string]string{"text": parts[1]})})
			}
		case strings.HasPrefix(line, "#"):
			parts := strings.SplitN(line[1:], " ", 2)
			if len(parts) == 2 {
				send(conn, env{Type: "room", From: *id, RoomID: parts[0], Content: mustJSON(map[string]string{"text": parts[1]})})
			}
		case strings.HasPrefix(line, "create:"):
			send(conn, env{Type: "create_room", From: *id, Content: mustJSON(map[string]string{"name": line[len("create:"):]})})
		case strings.HasPrefix(line, "join:"):
			send(conn, env{Type: "join", From: *id, Content: mustJSON(map[string]string{"roomId": line[len("join:"):]})})
		case strings.HasPrefix(line, "leave:"):
			send(conn, env{Type: "leave", From: *id, Content: mustJSON(map[string]string{"roomId": line[len("leave:"):]})})
		case line == "who":
			send(conn, env{Type: "list", From: *id})
		default:
			send(conn, env{Type: "broadcast", From: *id, Content: mustJSON(map[string]string{"text": line})})
		}
	}
}

func send(conn net.Conn, e env) {
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
