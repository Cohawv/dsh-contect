package httpgateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lan-agent-hub/internal/core"
)

// newRoomRoutes 装配一个真实内核 + 全部 /api 路由的测试靶场。
// 返回顶层 handler 与内核（用例直接往 Rooms 里铺数据，再从 HTTP 侧验证行为）。
func newRoomRoutes(t *testing.T) (http.Handler, *core.Hub) {
	t.Helper()
	hub := core.NewHub()
	handlers := map[string]http.HandlerFunc{}
	registerRoutes(func(pattern string, h http.HandlerFunc) {
		handlers[pattern] = h
	}, hub, nil, nil, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}), hub
}

// post 向指定端点发 JSON POST 并真实执行请求。
func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRoomDeleteEndpoint 覆盖删除房间的四条路径：成功 / 重复删 404 / 缺参 400 / 方法错 405。
func TestRoomDeleteEndpoint(t *testing.T) {
	h, hub := newRoomRoutes(t)

	id := hub.Rooms.Create("待删房间", []string{"A"})

	if rec := post(h, "/api/delete_room", `{"roomId":"`+id+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("删除存在房间 = %d，期望 200（body=%s）", rec.Code, rec.Body)
	}
	// 已删：再删必须 404——"删的时候已经没了"更可能是拿错 ID，静默成功不利排查
	if rec := post(h, "/api/delete_room", `{"roomId":"`+id+`"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除 = %d，期望 404", rec.Code)
	}
	for _, body := range []string{"{bad json", `{}`, `{"roomId":""}`} {
		if rec := post(h, "/api/delete_room", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("delete_room(%q) = %d，期望 400", body, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/delete_room", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET delete_room = %d，期望 405（破坏性操作不接受隐式触发）", rec.Code)
	}
}

// TestRoomRenameEndpoint：改名生效到列表、不存在 404、缺 name 400。
func TestRoomRenameEndpoint(t *testing.T) {
	h, hub := newRoomRoutes(t)

	id := hub.Rooms.Create("旧名", nil)
	if rec := post(h, "/api/rename_room", `{"roomId":"`+id+`","name":"新名"}`); rec.Code != http.StatusOK {
		t.Fatalf("改名 = %d，期望 200（body=%s）", rec.Code, rec.Body)
	}
	var name string
	for _, r := range hub.Rooms.List() {
		if r["id"] == id {
			name, _ = r["name"].(string)
		}
	}
	if name != "新名" {
		t.Fatalf("改名后 List.name = %q，期望 新名", name)
	}
	if rec := post(h, "/api/rename_room", `{"roomId":"room-nope","name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("改不存在的房间 = %d，期望 404", rec.Code)
	}
	if rec := post(h, "/api/rename_room", `{"roomId":"`+id+`","name":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 name = %d，期望 400（改名成空串等于把房间名抹掉）", rec.Code)
	}
}

// TestRoomKickEndpoint：踢人复用 Leave，被踢后成员列表里不再有他。
func TestRoomKickEndpoint(t *testing.T) {
	h, hub := newRoomRoutes(t)

	id := hub.Rooms.Create("踢人靶场", []string{"keeper"})
	hub.Rooms.Join(id, "victim")

	if rec := post(h, "/api/kick", `{"roomId":"`+id+`","agent":"victim"}`); rec.Code != http.StatusOK {
		t.Fatalf("踢人 = %d，期望 200（body=%s）", rec.Code, rec.Body)
	}
	for _, m := range hub.Rooms.Members(id) {
		if m == "victim" {
			t.Fatal("被踢成员仍在房间成员列表里")
		}
	}
	// 踢一个不在房间里的人：无变更，仍 200（与 /api/leave 的幂等语义一致）
	if rec := post(h, "/api/kick", `{"roomId":"`+id+`","agent":"ghost"}`); rec.Code != http.StatusOK {
		t.Fatalf("踢不存在成员 = %d，期望 200（幂等）", rec.Code)
	}
	if rec := post(h, "/api/kick", `{"roomId":"`+id+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 agent = %d，期望 400", rec.Code)
	}
}
