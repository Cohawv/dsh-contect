package main

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// StartHTTP 启动 HTTP 控制网关（REST），复用同一套 Hub 逻辑。
// 这样 Python / Node / DSH / curl 等非 Go 客户端也能通过 HTTP 接入，
// 与 TCP 客户端共用统一的 Envelope 协议。
func (h *Hub) StartHTTP(addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"agents": h.listAgents()})
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
		h.route(newEnvelope(TypeChat, p.From, p.To, "", map[string]string{"text": p.Text}))
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
		h.route(newEnvelope(TypeRoom, p.From, "", p.RoomID, map[string]string{"text": p.Text}))
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
		id := h.createRoom(p.Name, p.Members)
		writeJSON(w, map[string]string{"roomId": id})
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
		h.joinRoom(p.RoomID, p.From)
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
		h.leaveRoom(p.RoomID, p.From)
		writeJSON(w, map[string]string{"status": "ok"})
	})

	return http.ListenAndServe(addr, mux)
}
