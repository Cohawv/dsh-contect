package adapter

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/protocol"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// 便于观测：客户端可据此判断服务端支持的协议版本
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

// NewMux 构造 REST 网关的路由表（*http.ServeMux），注册全部 /api/* 端点与内置控制台。
//
// 抽离自 StartHTTP，便于契约测试用 httptest 直接挂载真实 handler 而不真正监听端口。
// 端点路径、响应字段、回执文案与历史版本完全一致（本版本零行为变更）。
// disco 可为 nil（关闭自动发现时），此时 /api/discover 返回 404。
func NewMux(hub *core.Hub, disco *discovery.Service) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/discover", func(w http.ResponseWriter, r *http.Request) {
		if disco == nil {
			http.Error(w, "discovery disabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(disco.BeaconJSON())
	})

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"agents": hub.ListAgentsDetailed()})
	})

	// 控制台状态总览：协议版本、暴露端口、在线/房间计数、发现服务开关。
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{
			"proto":      protocol.ProtoCurrent,
			"tcpPort":    hub.TCPPort,
			"httpPort":   hub.HTTPPort,
			"agentCount": len(hub.ListAgentsDetailed()),
			"roomCount":  len(hub.Rooms.List()),
			"discovery":  disco != nil,
			"serverTime": time.Now().UnixMilli(),
		})
	})

	// 房间列表（含成员），供控制台房间视图展示。
	mux.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"rooms": hub.Rooms.List()})
	})

	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.To == "" || p.Text == "" {
			http.Error(w, "need from/to/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeChat, p.From, p.To, "",
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/room", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" || p.Text == "" {
			http.Error(w, "need from/roomId/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeRoom, p.From, "", p.RoomID,
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/broadcast", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From string `json:"from"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Text == "" {
			http.Error(w, "need from/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeBroadcast, p.From, "", "",
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/create_room", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From    string   `json:"from"`
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"roomId": hub.Rooms.Create(p.Name, p.Members)})
	})

	mux.HandleFunc("/api/join", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" {
			http.Error(w, "need roomId", http.StatusBadRequest)
			return
		}
		hub.Rooms.Join(p.RoomID, p.From)
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/leave", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" {
			http.Error(w, "need roomId", http.StatusBadRequest)
			return
		}
		hub.Rooms.Leave(p.RoomID, p.From)
		writeJSON(w, map[string]string{"status": "ok"})
	})

	// 挂载内置控制台（静态资源，由 web.go 通过 //go:embed 嵌入，零外部依赖）。
	registerWeb(mux)

	return mux
}

// StartHTTP 启动 REST 网关，复用同一套 core 逻辑。
// 注意：HTTP 是无连接的请求-响应式接入，没有 Session 概念，
// 因此这里直接调用内核方法；离线提示等需要回执的能力仅在长连接形态下提供。
// disco 可为 nil（关闭自动发现时），此时 /api/discover 返回 404。
func StartHTTP(addr string, hub *core.Hub, disco *discovery.Service) error {
	return http.ListenAndServe(addr, NewMux(hub, disco))
}
