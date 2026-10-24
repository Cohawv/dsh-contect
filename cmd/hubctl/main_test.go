package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 假服务端：cli 的输入输出全部可注入，所以测试不需要真的起 Hub
// ---------------------------------------------------------------------------

// recorded 是一次被记录下来的请求（用于断言"hubctl 真的打了这个端点、带了这些字段"）。
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

type fakeHub struct {
	t    *testing.T
	mux  *http.ServeMux
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{t: t, mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.Query(), string(body)})
		f.mu.Unlock()
		// 请求体已被读掉，塞回去给真正的 handler。
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// handle 注册一个端点（未注册的端点会返回 404，正好用来暴露"打错 API"）。
func (f *fakeHub) handle(pattern string, fn http.HandlerFunc) {
	f.mux.HandleFunc(pattern, fn)
}

// json 注册一个返回固定 JSON 的端点。
func (f *fakeHub) json(pattern, body string) {
	f.handle(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
}

// sse 注册一个 SSE 端点：发完给定帧就结束（客户端读到 EOF 即正常退出）。
func (f *fakeHub) sse(pattern string, frames ...string) {
	f.handle(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr)
			if fl != nil {
				fl.Flush()
			}
		}
	})
}

func (f *fakeHub) addr() string { return f.srv.URL }

// run 驱动一次 CLI 调用，返回（退出码, stdout, stderr）。
func (f *fakeHub) run(args ...string) (int, string, string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"-addr", f.addr()}, args...)
	code := run(context.Background(), full, &out, &errb)
	return code, out.String(), errb.String()
}

// reqsFor 返回打到某个路径的全部请求。
func (f *fakeHub) reqsFor(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 断言小工具
// ---------------------------------------------------------------------------

func assertExit(t *testing.T, got, want int, stderr string) {
	t.Helper()
	if got != want {
		t.Fatalf("退出码 = %d，期望 %d（stderr=%q）", got, want, stderr)
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("输出不是合法 JSON：%v\n输出=%q", err, s)
	}
	return v
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("输出缺少 %q\n完整输出=%q", sub, s)
		}
	}
}

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Fatalf("输出不应包含 %q\n完整输出=%q", sub, s)
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func TestStatusTableHasHeaderAndRows(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{"proto":3,"tcpPort":8080,"httpPort":8081,"agentCount":2,"roomCount":1,"discovery":true,"serverTime":1700000000000}`)

	code, out, errb := f.run("-fmt", "table", "status")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "KEY", "VALUE", "proto", "8081", "agentCount")
	mustContain(t, out, "2")
	if strings.Contains(out, "\x1b[") {
		t.Errorf("非 TTY 下不应输出颜色，实际=%q", out)
	}
}

func TestStatusJSONIsUnmarshalable(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{"proto":3,"tcpPort":8080,"httpPort":8081,"agentCount":0,"roomCount":0,"discovery":false,"serverTime":1700000000000}`)

	code, out, errb := f.run("-fmt", "json", "status")
	assertExit(t, code, exitOK, errb)
	v := mustJSON(t, out)
	if v["httpPort"] != float64(8081) {
		t.Errorf("httpPort = %v，期望 8081", v["httpPort"])
	}
	if v["agentCount"] != float64(0) {
		t.Errorf("agentCount = %v，期望 0", v["agentCount"])
	}
}

// ---------------------------------------------------------------------------
// agents
// ---------------------------------------------------------------------------

func TestAgentsTableAndJSON(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/agents", `{"agents":[{"id":"a1","name":"Alpha","proto":3,"caps":["chat"],"lastSeen":1700000000000},{"id":"a2","name":"Beta"}]}`)

	code, out, errb := f.run("-fmt", "table", "agents")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ID", "NAME", "PROTO", "CAPS", "LASTSEEN", "a1", "Alpha", "chat", "a2", "Beta")

	code, out, errb = f.run("-fmt", "json", "agents")
	assertExit(t, code, exitOK, errb)
	v := mustJSON(t, out)
	agents, ok := v["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("agents 字段应为 2 项数组，实际=%v", v["agents"])
	}
}

func TestAgentsWatchConsumesSSE(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/agents", `{"agents":[{"id":"a1","name":"Alpha"}]}`)
	f.sse("/api/stream",
		"event: agent.online\ndata: \"a2\"\nid: 7\n\n",
		":ping\n\n",
		"event: agent.offline\ndata: \"a1\"\nid: 8\n\n",
	)

	code, out, errb := f.run("-fmt", "table", "agents", "-w")
	assertExit(t, code, exitOK, errb)
	// 先快照，再事件。
	mustContain(t, out, "a1", "Alpha", "agent.online", "a2", "agent.offline")
	mustNotContain(t, out, "ping") // 心跳注释行不得泄漏给用户

	reqs := f.reqsFor("/api/stream")
	if len(reqs) != 1 {
		t.Fatalf("应订阅 1 次 /api/stream，实际 %d", len(reqs))
	}
	if got := reqs[0].Query.Get("topics"); got != "agent.*" {
		t.Errorf("topics = %q，期望 agent.*", got)
	}
}

// ---------------------------------------------------------------------------
// rooms
// ---------------------------------------------------------------------------

func TestRoomsTableAndJSON(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/rooms", `{"rooms":[{"id":"r1","name":"Lobby","memberCount":2,"members":["a1","a2"]}]}`)

	code, out, errb := f.run("-fmt", "table", "rooms")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ID", "NAME", "MEMBERS", "r1", "Lobby", "a1,a2")

	code, out, errb = f.run("-fmt", "json", "rooms")
	assertExit(t, code, exitOK, errb)
	v := mustJSON(t, out)
	if _, ok := v["rooms"]; !ok {
		t.Errorf("json 输出缺少 rooms 字段：%v", v)
	}
}

// ---------------------------------------------------------------------------
// room 管理子动作（create / drop / rename / kick）
// ---------------------------------------------------------------------------

func TestRoomCreatePostsNameAndMembers(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/create_room", `{"roomId":"r-new-1"}`)

	// flag 包语义：--members 必须写在位置参数之前（遇到第一个位置参数即停止解析）。
	code, out, errb := f.run("-fmt", "table", "room", "create", "--members=a1, a2 ,,", "作战室")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ROOM-ID", "NAME", "r-new-1", "作战室")

	// 无 --members 的最常见用法（仅位置参数）也必须工作。
	code, out, errb = f.run("-fmt", "table", "room", "create", "仅我")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ROOM-ID", "NAME", "仅我")

	reqs := f.reqsFor("/api/create_room")
	if len(reqs) != 2 {
		t.Fatalf("应 POST /api/create_room 两次（带/不带 members），实际 %d", len(reqs))
	}
	var body struct {
		From    string   `json:"from"`
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v（%q）", err, reqs[0].Body)
	}
	// --members 的语义：逗号分隔 + 容忍空片段和空白（" a2,," 只留 a1/a2）。
	if body.Name != "作战室" || len(body.Members) != 2 || body.Members[0] != "a1" || body.Members[1] != "a2" {
		t.Errorf("请求体字段不符：%+v", body)
	}
	if body.From == "" {
		t.Errorf("create_room 请求体缺 from 身份")
	}
	// 无 --members 时 members 应是空数组而非 null（服务端好处理）。
	var body2 struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := json.Unmarshal([]byte(reqs[1].Body), &body2); err != nil {
		t.Fatalf("第二笔请求体不是合法 JSON：%v（%q）", err, reqs[1].Body)
	}
	if body2.Name != "仅我" || len(body2.Members) != 0 {
		t.Errorf("无 members 的请求体字段不符：%+v", body2)
	}
}

func TestRoomDropPostsRoomID(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/delete_room", `{"status":"ok"}`)

	code, out, errb := f.run("-fmt", "table", "room", "drop", "r-old")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "/api/delete_room", "r-old 已删除")

	reqs := f.reqsFor("/api/delete_room")
	if len(reqs) != 1 {
		t.Fatalf("应 POST /api/delete_room 一次，实际 %d", len(reqs))
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(reqs[0].Body), &body)
	if body["roomId"] != "r-old" {
		t.Errorf("请求体字段不符：%v", body)
	}
}

// TestRoomDropTranslates404 必须用全新的 fakeHub：上面的成功用例注册过
// 200 端点，复用同一实例的话 404 场景会静默落到旧端点上（首跑就栽过）。
func TestRoomDropTranslates404(t *testing.T) {
	f := newFakeHub(t) // 一个端点都不注册 → mux 自然返回 404

	// 404 必须翻译成「房间不存在」的用法错误（退出码 1），
	// 而不是透传 HTTP 细节让用户猜（那样是退出码 2）。
	code, _, errb := f.run("room", "drop", "r-ghost")
	assertExit(t, code, exitUsage, errb)
	mustContain(t, errb, "房间不存在")
}

func TestRoomRenamePostsRoomIDAndName(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/rename_room", `{"status":"ok"}`)

	code, out, errb := f.run("-fmt", "table", "room", "rename", "r1", "新名字")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "/api/rename_room", "r1 → 新名字")

	var body map[string]string
	_ = json.Unmarshal([]byte(f.reqsFor("/api/rename_room")[0].Body), &body)
	if body["roomId"] != "r1" || body["name"] != "新名字" {
		t.Errorf("请求体字段不符：%v", body)
	}
}

func TestRoomKickPostsRoomIDAndAgent(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/kick", `{"status":"ok"}`)

	code, out, errb := f.run("-fmt", "table", "room", "kick", "r1", "a2")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "/api/kick", "a2 ← r1")

	var body map[string]string
	_ = json.Unmarshal([]byte(f.reqsFor("/api/kick")[0].Body), &body)
	if body["roomId"] != "r1" || body["agent"] != "a2" {
		t.Errorf("请求体字段不符：%v", body)
	}
}

func TestRoomUsageErrorsExit1(t *testing.T) {
	f := newFakeHub(t)
	// 用法错误在发请求之前就该拦下——fakeHub 一个端点都没注册，
	// 如果这些用例真的打了网络，会以退出码 2 收场，正好被断言抓住。
	cases := []struct {
		name string
		args []string
		hint string
	}{
		{"无子动作", []string{"room"}, "需要子动作"},
		{"未知子动作", []string{"room", "purge"}, "未知 room 子动作"},
		{"create 缺名称", []string{"room", "create"}, "用法：hubctl room create"},
		{"drop 缺房间ID", []string{"room", "drop"}, "用法：hubctl room drop"},
		{"rename 缺新名称", []string{"room", "rename", "r1"}, "用法：hubctl room rename"},
		{"kick 缺成员", []string{"room", "kick", "r1"}, "用法：hubctl room kick"},
	}
	for _, tc := range cases {
		code, _, errb := f.run(tc.args...)
		if code != exitUsage {
			t.Errorf("%s：退出码 = %d，期望 %d（stderr=%q）", tc.name, code, exitUsage, errb)
		}
		if !strings.Contains(errb, tc.hint) {
			t.Errorf("%s：stderr 缺提示 %q，实际 %q", tc.name, tc.hint, errb)
		}
	}
	if n := len(f.reqsFor("/api/create_room")) + len(f.reqsFor("/api/delete_room")) +
		len(f.reqsFor("/api/rename_room")) + len(f.reqsFor("/api/kick")); n != 0 {
		t.Errorf("用法错误不应发出任何网络请求，实际发出 %d 个", n)
	}
}

// ---------------------------------------------------------------------------
// send / say / bcast
// ---------------------------------------------------------------------------

func TestSendPostsExpectedBody(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/send", `{"status":"ok"}`)

	code, out, errb := f.run("-fmt", "table", "send", "--to", "a1", "--text", "hi")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "/api/send", "ok")

	reqs := f.reqsFor("/api/send")
	if len(reqs) != 1 {
		t.Fatalf("应 POST /api/send 一次，实际 %d", len(reqs))
	}
	if reqs[0].Method != http.MethodPost {
		t.Errorf("方法 = %s，期望 POST", reqs[0].Method)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v（%q）", err, reqs[0].Body)
	}
	if body["to"] != "a1" || body["text"] != "hi" || body["from"] == "" {
		t.Errorf("请求体字段不符：%v", body)
	}
}

func TestSayPostsRoomID(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/room", `{"status":"ok"}`)

	code, out, errb := f.run("-fmt", "json", "say", "--room", "r1", "--text", "hi")
	assertExit(t, code, exitOK, errb)
	mustJSON(t, out)

	reqs := f.reqsFor("/api/room")
	if len(reqs) != 1 {
		t.Fatalf("应 POST /api/room 一次，实际 %d", len(reqs))
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(reqs[0].Body), &body)
	if body["roomId"] != "r1" || body["text"] != "hi" {
		t.Errorf("请求体字段不符：%v", body)
	}
}

func TestBroadcastPostsText(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/broadcast", `{"status":"ok"}`)

	code, _, errb := f.run("-fmt", "json", "bcast", "--text", "hello all")
	assertExit(t, code, exitOK, errb)

	reqs := f.reqsFor("/api/broadcast")
	if len(reqs) != 1 {
		t.Fatalf("应 POST /api/broadcast 一次，实际 %d", len(reqs))
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(reqs[0].Body), &body)
	if body["text"] != "hello all" {
		t.Errorf("请求体 text 不符：%v", body)
	}
}

// ---------------------------------------------------------------------------
// plugins
// ---------------------------------------------------------------------------

func TestPluginsListAndToggle(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/plugins", `{"count":2,"running":["a"],"disabled":["b"],"failed":[],"plugins":[{"name":"tcpgateway","version":"1.0.0","description":"TCP","state":"running","routeCount":1,"routes":["/api/x"]},{"name":"sweeper","version":"1.0.0","description":"清理","state":"disabled","routeCount":0,"routes":[]}],"routes":[],"generatedAt":1}`)
	f.json("/api/plugins/toggle", `{"ok":true,"name":"sweeper","action":"enable","state":"running","restartCount":3}`)

	code, out, errb := f.run("-fmt", "table", "plugins")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "NAME", "STATE", "VERSION", "tcpgateway", "running", "sweeper", "disabled")

	code, out, errb = f.run("-fmt", "table", "plugins", "start", "sweeper")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "sweeper", "enable", "running")

	reqs := f.reqsFor("/api/plugins/toggle")
	if len(reqs) != 1 {
		t.Fatalf("应 POST /api/plugins/toggle 一次，实际 %d", len(reqs))
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(reqs[0].Body), &body)
	if body["name"] != "sweeper" || body["action"] != "enable" {
		t.Errorf("toggle 请求体不符：%v", body)
	}
}

func TestPluginsStopUsesDisable(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/plugins/toggle", `{"ok":true,"name":"sweeper","action":"disable","state":"disabled","restartCount":0}`)

	code, _, errb := f.run("-fmt", "json", "plugins", "stop", "sweeper")
	assertExit(t, code, exitOK, errb)

	reqs := f.reqsFor("/api/plugins/toggle")
	if len(reqs) != 1 {
		t.Fatalf("应 POST 一次，实际 %d", len(reqs))
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(reqs[0].Body), &body)
	if body["action"] != "disable" {
		t.Errorf("stop 应转成 disable，实际=%v", body)
	}
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func TestLogsTableAndJSON(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/logs", `{"count":2,"limit":100,"level":"","capacity":500,"total":10,"dropped":0,"entries":[{"seq":1,"ts":1700000000000,"level":"info","msg":"started"},{"seq":2,"ts":1700000001000,"level":"error","msg":"boom"}]}`)

	code, out, errb := f.run("-fmt", "table", "logs")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "SEQ", "TIME", "LEVEL", "MSG", "started", "boom", "error")

	code, out, errb = f.run("-fmt", "json", "logs", "-n", "10", "-level", "warn")
	assertExit(t, code, exitOK, errb)
	mustJSON(t, out)
	reqs := f.reqsFor("/api/logs")
	if len(reqs) != 2 {
		t.Fatalf("应 GET /api/logs 两次，实际 %d", len(reqs))
	}
	if reqs[1].Query.Get("limit") != "10" || reqs[1].Query.Get("level") != "warn" {
		t.Errorf("查询参数不符：%v", reqs[1].Query)
	}
}

func TestLogsFollowSSE(t *testing.T) {
	f := newFakeHub(t)
	f.sse("/api/logs/stream",
		"event: log\ndata: {\"seq\":1,\"ts\":1700000000000,\"level\":\"info\",\"msg\":\"hello\"}\nid: 1\n\n",
		":ping\n\n",
		"event: log\ndata: {\"seq\":2,\"ts\":1700000001000,\"level\":\"error\",\"msg\":\"boom\"}\nid: 2\n\n",
	)

	code, out, errb := f.run("-fmt", "table", "logs", "-f")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "hello", "boom", "ERROR")
	mustNotContain(t, out, "ping")

	// json 模式下每一行都必须能被单独 json.Unmarshal（流式输出的关键约束）。
	code, out, errb = f.run("-fmt", "json", "logs", "-f")
	assertExit(t, code, exitOK, errb)
	lines := nonEmptyLines(out)
	if len(lines) != 2 {
		t.Fatalf("应有 2 行事件，实际 %d：%q", len(lines), out)
	}
	for i, ln := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON：%v（%q）", i+1, err, ln)
		}
		if v["topic"] != "log" {
			t.Errorf("第 %d 行 topic = %v，期望 log", i+1, v["topic"])
		}
	}
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

func TestWatchMergesAgentAndLogStreams(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/agents", `{"agents":[{"id":"a1","name":"Alpha"}]}`)
	f.sse("/api/stream", "event: agent.online\ndata: \"a9\"\nid: 5\n\n")
	f.sse("/api/logs/stream", "event: log\ndata: {\"seq\":9,\"ts\":1700000000000,\"level\":\"warn\",\"msg\":\"watchme\"}\nid: 9\n\n")

	code, out, errb := f.run("-fmt", "table", "watch")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ID", "a1", "Alpha", "agent.online", "a9", "watchme", "WARN")

	if len(f.reqsFor("/api/stream")) != 1 {
		t.Errorf("应订阅 /api/stream 一次")
	}
	if len(f.reqsFor("/api/logs/stream")) != 1 {
		t.Errorf("应订阅 /api/logs/stream 一次")
	}
}

func TestWatchJSONLinesAreIndividuallyValid(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/agents", `{"agents":[]}`)
	f.sse("/api/stream", "event: agent.online\ndata: \"a9\"\nid: 5\n\n")
	f.sse("/api/logs/stream", "event: log\ndata: {\"seq\":9,\"ts\":1,\"level\":\"info\",\"msg\":\"m\"}\nid: 9\n\n")

	code, out, errb := f.run("-fmt", "json", "watch", "--no-agents")
	assertExit(t, code, exitOK, errb)
	lines := nonEmptyLines(out)
	if len(lines) != 2 {
		t.Fatalf("应有 2 行事件，实际 %d：%q", len(lines), out)
	}
	for _, ln := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("行不是合法 JSON：%v（%q）", err, ln)
		}
		if _, ok := v["src"]; !ok {
			t.Errorf("行缺少 src 字段：%q", ln)
		}
	}
}

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

func TestMessagesSinceSeq(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/messages", `{"messages":[{"seq":101,"ts":1700000000000,"type":"chat","from":"A","to":"B","content":"{\"text\":\"hi\"}","delivered":1}],"count":1,"lastSeq":101}`)

	code, out, errb := f.run("-fmt", "table", "messages", "--since", "100")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "SEQ", "FROM", "TO", "TEXT", "101", "A", "B", "hi")

	reqs := f.reqsFor("/api/messages")
	if len(reqs) != 1 {
		t.Fatalf("应 GET /api/messages 一次，实际 %d", len(reqs))
	}
	if reqs[0].Query.Get("since") != "100" {
		t.Errorf("since 查询参数 = %q，期望 100", reqs[0].Query.Get("since"))
	}

	code, out, errb = f.run("-fmt", "json", "messages", "--since", "100")
	assertExit(t, code, exitOK, errb)
	v := mustJSON(t, out)
	if v["lastSeq"] != float64(101) {
		t.Errorf("lastSeq = %v，期望 101", v["lastSeq"])
	}
}

// ---------------------------------------------------------------------------
// perf
// ---------------------------------------------------------------------------

// perfFixture 是固定的一份 /api/metrics/summary 响应。
//
// 两个丢弃数刻意取不同的值（7 / 2）：如果实现把它们相加成 9，下面的单元格断言会立刻红。
// memLimitBytes=0 而堆用量非零：保证输出里唯一可能出现的 "0 B" 只能来自「未启用阈值」，
// 于是 mustNotContain(out, "0 B") 才是对「0 与没配是两件事」的有效约束。
const perfFixture = `{"uptimeMs":3723000,"agentsOnline":2,"rooms":1,"messagesTotal":1234,` +
	`"messagesPerSecond":3.5,"httpRequestsTotal":88,"eventsDropped":0,"sseClients":4,"intervalMs":5000,` +
	`"deliverDropped":7,"deliverClosed":2,"heapAllocBytes":12582912,"heapInuseBytes":20971520,` +
	`"goroutines":42,"gcTotal":17,"memLimitBytes":0,"memHigh":false,"memHighTotal":0,"samples":[]}`

// perfCell 从键值表输出里取出某个键的取值单元格。
// tabwriter 用 ≥2 个空格分列，所以「键 + 空格」之后的部分就是取值。
func perfCell(t *testing.T, out, key string) string {
	t.Helper()
	for _, ln := range nonEmptyLines(out) {
		if !strings.HasPrefix(ln, key) {
			continue
		}
		rest := ln[len(key):]
		if !strings.HasPrefix(rest, "  ") { // 前缀撞车（如 memHigh/memHighTotal）不算命中
			continue
		}
		return strings.TrimSpace(rest)
	}
	t.Fatalf("输出里找不到键 %q 的行：\n%s", key, out)
	return ""
}

func TestPerfTableSeparatesDropCausesAndUnsetLimit(t *testing.T) {
	f := newFakeHub(t)
	f.json(perfPath, perfFixture)

	code, out, errb := f.run("-fmt", "table", "perf")
	assertExit(t, code, exitOK, errb)

	mustContain(t, out, "KEY", "VALUE", "deliverDropped(队列满)", "deliverClosed(已关闭)", "未启用")
	if got := perfCell(t, out, "deliverDropped(队列满)"); got != "7" {
		t.Errorf("队列满丢弃 = %q，期望 7（不得与 deliverClosed 相加）", got)
	}
	if got := perfCell(t, out, "deliverClosed(已关闭)"); got != "2" {
		t.Errorf("已关闭丢弃 = %q，期望 2", got)
	}
	if got := perfCell(t, out, "messagesPerSecond"); got != "3.5 msg/s" {
		t.Errorf("当前速率 = %q，期望 3.5 msg/s（保留 1 位小数）", got)
	}
	if got := perfCell(t, out, "uptime"); got != "1h02m" {
		t.Errorf("运行时长 = %q，期望 1h02m（两级单位）", got)
	}
	if got := perfCell(t, out, "heapAlloc"); got != "12.0 MiB" {
		t.Errorf("heapAlloc = %q，期望 12.0 MiB", got)
	}
	if got := perfCell(t, out, "goroutines"); got != "42" {
		t.Errorf("goroutines = %q，期望 42", got)
	}
	// memLimitBytes=0：阈值与告警状态都必须是「未启用」，且绝不能渲染成 "0 B"。
	if n := strings.Count(out, "未启用"); n != 2 {
		t.Errorf("「未启用」应出现 2 次（memLimit 与 memHigh 各一次），实际 %d 次：\n%s", n, out)
	}
	mustNotContain(t, out, "0 B")
}

func TestPerfMemHighOverThreshold(t *testing.T) {
	f := newFakeHub(t)
	// 64 MiB 阈值 + 已越线 + 累计 3 次。
	f.json(perfPath, `{"uptimeMs":60000,"memLimitBytes":67108864,"memHigh":true,"memHighTotal":3}`)

	code, out, errb := f.run("-fmt", "table", "perf")
	assertExit(t, code, exitOK, errb)

	mustContain(t, out, "超阈值")
	mustNotContain(t, out, "未启用")
	if got := perfCell(t, out, "memLimit"); got != "64.0 MiB" {
		t.Errorf("memLimit = %q，期望 64.0 MiB", got)
	}
	if got := perfCell(t, out, "memHigh"); got != "超阈值（累计 3 次）" {
		t.Errorf("memHigh = %q，期望 超阈值（累计 3 次）", got)
	}
}

func TestPerfNextGCAndHeapPressure(t *testing.T) {
	f := newFakeHub(t)
	// 已用 3 MiB / 目标 4 MiB → 75.0%
	f.json(perfPath, `{"uptimeMs":60000,"heapAllocBytes":3145728,"heapInuseBytes":4194304,"nextGCBytes":4194304}`)

	code, out, errb := f.run("-fmt", "table", "perf")
	assertExit(t, code, exitOK, errb)

	mustContain(t, out, "75.0%", "4.0 MiB")
	if got := perfCell(t, out, "nextGC"); got != "4.0 MiB" {
		t.Errorf("nextGC = %q，期望 4.0 MiB（复用 fmtBytes 的 MiB 一位小数）", got)
	}
	if got := perfCell(t, out, "heapPressure(堆/GC 水位)"); got != "75.0%" {
		t.Errorf("heapPressure = %q，期望 75.0%%", got)
	}
	// 75% 还没到临界线，不该喊临界（否则告警一响就没人看了）。
	mustNotContain(t, out, "临界")
}

func TestPerfHeapPressureCriticalHasTextSignal(t *testing.T) {
	f := newFakeHub(t)
	// 4089446 / 4194304 ≈ 97.5%，已进临界区。
	f.json(perfPath, `{"uptimeMs":60000,"heapAllocBytes":4089446,"heapInuseBytes":4194304,"nextGCBytes":4194304}`)

	// 无色模式（管道/日志/NO_COLOR）：文字信号必须独立成立——颜色不能是唯一信号。
	code, out, errb := f.run("-fmt", "table", "-color", "never", "perf")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "97.5%", "临界")
	mustNotContain(t, out, "\x1b[")

	// 有色模式：颜色与文字同时出现。
	code, out, errb = f.run("-fmt", "table", "-color", "always", "perf")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "\x1b[", "97.5%", "临界")
}

func TestPerfNextGCUnsetShowsNotSetAndDash(t *testing.T) {
	f := newFakeHub(t)
	// nextGCBytes=0 与 memLimitBytes=0：两处都不能渲染成 "0 B"。
	f.json(perfPath, `{"uptimeMs":60000,"heapAllocBytes":3145728,"heapInuseBytes":4194304,"nextGCBytes":0,"memLimitBytes":0}`)

	code, out, errb := f.run("-fmt", "table", "perf")
	assertExit(t, code, exitOK, errb)

	if got := perfCell(t, out, "nextGC"); got != "未设置" {
		t.Errorf("nextGC = %q，期望 未设置", got)
	}
	if got := perfCell(t, out, "heapPressure(堆/GC 水位)"); got != "—" {
		t.Errorf("heapPressure = %q，期望 —（没有分母就不编造比例）", got)
	}
	mustContain(t, out, "未设置", "—")
	mustNotContain(t, out, "0 B")
	mustNotContain(t, out, "0.0%") // 0 也不能当成「压力是 0%」
}

func TestPerfJSONPassesThroughUnknownFields(t *testing.T) {
	f := newFakeHub(t)
	// futureField / 未定义的 samples 子结构都必须活着出现在输出里：
	// 这证明 json 模式是原样透传，而不是重新构造结构体（那会静默丢字段）。
	// nextGCBytes 也是同一逻辑：后端加字段时客户端 zero-change 就有。
	f.json(perfPath, `{"uptimeMs":5000,"deliverDropped":7,"nextGCBytes":4194304,"futureField":1,"samples":[{"ts":1,"messagesTotal":2}]}`)

	code, out, errb := f.run("-fmt", "json", "perf")
	assertExit(t, code, exitOK, errb)

	v := mustJSON(t, out)
	if v["futureField"] != float64(1) {
		t.Errorf("futureField = %v，期望 1（未知字段被丢弃说明不是透传）", v["futureField"])
	}
	if v["deliverDropped"] != float64(7) {
		t.Errorf("deliverDropped = %v，期望 7", v["deliverDropped"])
	}
	if v["nextGCBytes"] != float64(4194304) {
		t.Errorf("nextGCBytes = %v，期望 4194304（新字段应随透传自动出现）", v["nextGCBytes"])
	}
	mustContain(t, out, "futureField", "samples", "nextGCBytes")
}

func TestPerfFailOnDropExitCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"无丢弃", `{"deliverDropped":0,"deliverClosed":0,"eventsDropped":0}`, exitOK},
		{"队列满丢弃", `{"deliverDropped":3,"deliverClosed":0,"eventsDropped":0}`, exitDegraded},
		{"已关闭丢弃", `{"deliverDropped":0,"deliverClosed":1,"eventsDropped":0}`, exitDegraded},
		{"事件丢弃", `{"deliverDropped":0,"deliverClosed":0,"eventsDropped":2}`, exitDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHub(t)
			f.json(perfPath, tc.body)
			code, out, errb := f.run("-fmt", "json", "perf", "--fail-on-drop")
			assertExit(t, code, tc.want, errb)
			// 退出 3 时 stdout 仍是完整可用快照（CI 要能在同一份输出上定位问题）。
			mustJSON(t, out)
			if tc.want == exitDegraded {
				mustContain(t, errb, "降级")
			} else if errb != "" {
				t.Errorf("无丢弃时 stderr 应为空，实际=%q", errb)
			}
		})
	}

	// 不带 --fail-on-drop：同样的响应必须是 0（默认语义不变）。
	f := newFakeHub(t)
	f.json(perfPath, `{"deliverDropped":5,"deliverClosed":5,"eventsDropped":5}`)
	code, _, errb := f.run("-fmt", "json", "perf")
	assertExit(t, code, exitOK, errb)
}

func TestPerfUsageErrorsExit1(t *testing.T) {
	f := newFakeHub(t)
	f.json(perfPath, perfFixture)

	cases := []struct {
		name string
		args []string
	}{
		{"未知标志", []string{"perf", "--bogus"}},
		{"--fail-on-drop 拼错", []string{"perf", "--fail-on-drops"}},
		{"位置参数", []string{"perf", "now"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errb := f.run(tc.args...)
			assertExit(t, code, exitUsage, errb)
			mustContain(t, errb, "用法错误")
		})
	}
}

// captureWriter 在缓冲区里数到 marker 出现 need 次时调用 cancel。
// 这样 perf -w 的轮询在"断言够用的帧数"之后自然收尾：既不用 sleep 等时钟，
// 也不会因为取消 httptest 响应读取而把请求搞成 connError。
type captureWriter struct {
	buf    bytes.Buffer
	marker string
	need   int
	seen   int
	cancel context.CancelFunc
}

func (w *captureWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if w.cancel != nil {
		w.seen += strings.Count(string(p), w.marker)
		if w.seen >= w.need {
			w.cancel()
			w.cancel = nil
		}
	}
	return n, err
}

func (w *captureWriter) String() string { return w.buf.String() }

// newPerfApp 构造一个只跑 perf 的最小 app：间隔压到 1ms，输出目标可注入。
func newPerfApp(f *fakeHub, out io.Writer, format string, isTTY bool, ctx context.Context) *app {
	return &app{
		out:       newRenderer(out, format, false),
		errw:      io.Discard,
		cl:        newClient(f.addr()),
		now:       time.Now,
		ctx:       ctx,
		isTTY:     isTTY,
		perfEvery: time.Millisecond,
	}
}

func TestPerfWatchNonTTYEmitsNDJSON(t *testing.T) {
	f := newFakeHub(t)
	f.json(perfPath, perfFixture)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 两帧即停：第二行写完时取消 ctx，select 立刻走退出分支。
	lw := &captureWriter{marker: "\n", need: 2, cancel: cancel}
	a := newPerfApp(f, lw, formatJSON, false, ctx)

	if err := a.cmdPerf([]string{"-w"}); err != nil {
		t.Fatalf("perf -w 应正常结束，实际错误：%v", err)
	}
	lines := nonEmptyLines(lw.String())
	if len(lines) != 2 {
		t.Fatalf("非 TTY 下 -w 应输出 2 行 NDJSON，实际 %d 行：%q", len(lines), lw.String())
	}
	for i, ln := range lines {
		v := mustJSON(t, ln) // 每行必须能独立解析，才能 | jq -c 逐行消费
		if _, ok := v["uptimeMs"]; !ok {
			t.Errorf("第 %d 行缺少 uptimeMs：%q", i+1, ln)
		}
	}
	mustNotContain(t, lw.String(), "\x1b[") // 管道里绝不能出现 ANSI 控制符
}

func TestPerfWatchTTYRedrawsInPlace(t *testing.T) {
	f := newFakeHub(t)
	f.json(perfPath, perfFixture)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 清屏码出现 2 次即停：一帧一个清屏，说明是「同一屏重绘」而不是往下追加。
	lw := &captureWriter{marker: ansiClearHome, need: 2, cancel: cancel}
	a := newPerfApp(f, lw, formatTable, true, ctx)

	if err := a.cmdPerf([]string{"-w"}); err != nil {
		t.Fatalf("perf -w 应正常结束，实际错误：%v", err)
	}
	if n := strings.Count(lw.String(), ansiClearHome); n != 2 {
		t.Fatalf("TTY 下每帧应先清屏再重绘，清屏次数=%d，期望 2", n)
	}
	// 每帧都是完整的同一张表（2 帧 → 两个丢弃行各 2 次）。
	if n := strings.Count(lw.String(), "deliverDropped(队列满)"); n != 2 {
		t.Errorf("deliverDropped 行应出现 2 次（每帧一次），实际 %d 次", n)
	}
}

func TestPerfServerErrorAndBadJSONExit2(t *testing.T) {
	// 非 200：走既有 statusError 路径（退出码 2 + 状态码进 stderr）。
	f := newFakeHub(t)
	f.handle(perfPath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	code, out, errb := f.run("perf")
	assertExit(t, code, exitNet, errb)
	mustContain(t, errb, "500")
	if out != "" {
		t.Errorf("失败时 stdout 应为空，实际=%q", out)
	}

	// 坏 JSON：解析失败也必须退出 2，不能当成「全零快照」静默返回 0。
	f2 := newFakeHub(t)
	f2.json(perfPath, `{"uptimeMs": 12,`)
	code, _, errb = f2.run("perf")
	assertExit(t, code, exitNet, errb)
	mustContain(t, errb, "解析失败")
}

func TestFmtDurationAndBytes(t *testing.T) {
	durations := map[int64]string{
		0:        "0s",
		42000:    "42s",
		3723000:  "1h02m",
		90061000: "1d01h",
		3599000:  "59m59s",
	}
	for ms, want := range durations {
		if got := fmtDuration(ms); got != want {
			t.Errorf("fmtDuration(%d) = %q，期望 %q", ms, got, want)
		}
	}

	sizes := map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1024:       "1.0 KiB",
		12582912:   "12.0 MiB",
		1073741824: "1.00 GiB",
	}
	for n, want := range sizes {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%d) = %q，期望 %q", n, got, want)
		}
	}
}

func TestPerfHelpLineAndExitCodeDoc(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &out, &errb)
	assertExit(t, code, exitOK, errb.String())
	// 帮助文本与退出码段都来自 main.go，不另写文案；这里钉住 perf 行与退出码 3。
	mustContain(t, out.String(),
		"hubctl perf [-w] [--fail-on-drop]",
		"3 降级（与 --fail-on-drop 连用时检测到丢弃）")
}

// ---------------------------------------------------------------------------
// 错误路径：连接失败 / 服务端错误 / 用法错误
// ---------------------------------------------------------------------------

func TestConnectionRefusedExit2WithHint(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{}`)
	dead := f.addr()
	f.srv.Close() // 故意让服务端消失

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"-addr", dead, "status"}, &out, &errb)
	assertExit(t, code, exitNet, errb.String())
	mustContain(t, errb.String(), "连不上 Hub", "go run ./cmd/hub")
	if out.Len() != 0 {
		t.Errorf("失败时 stdout 应为空，实际=%q", out.String())
	}
}

func TestServerErrorExit2(t *testing.T) {
	f := newFakeHub(t)
	f.handle("/api/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	code, _, errb := f.run("status")
	assertExit(t, code, exitNet, errb)
	mustContain(t, errb, "500")
}

func TestUsageErrorsExit1(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/send", `{"status":"ok"}`)
	f.json("/api/room", `{"status":"ok"}`)
	f.json("/api/broadcast", `{"status":"ok"}`)

	cases := []struct {
		name string
		args []string
	}{
		{"send 缺 --to", []string{"send", "--text", "hi"}},
		{"send 缺 --text", []string{"send", "--to", "a1"}},
		{"say 缺 --room", []string{"say", "--text", "hi"}},
		{"bcast 缺 --text", []string{"bcast"}},
		{"未知子命令", []string{"nope"}},
		{"plugins 参数个数不对", []string{"plugins", "start"}},
		{"plugins 非法动作", []string{"plugins", "restart", "x"}},
		{"completion 缺 shell", []string{"completion"}},
		{"completion 非法 shell", []string{"completion", "fish"}},
		{"未知全局参数", []string{"-bogus", "status"}},
		{"-addr 缺取值", []string{"-addr"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errb := f.run(tc.args...)
			assertExit(t, code, exitUsage, errb)
			mustContain(t, errb, "用法错误")
		})
	}
}

func TestNoSubcommandExit1(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), nil, &out, &errb)
	assertExit(t, code, exitUsage, errb.String())
	mustContain(t, errb.String(), "缺少子命令")
}

// ---------------------------------------------------------------------------
// 输出格式
// ---------------------------------------------------------------------------

func TestFormatSwitchBetweenJSONAndTable(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/rooms", `{"rooms":[{"id":"r1","name":"Lobby","memberCount":1,"members":["a1"]}]}`)

	code, out, errb := f.run("-fmt", "json", "rooms")
	assertExit(t, code, exitOK, errb)
	mustJSON(t, out)

	code, out, errb = f.run("-fmt", "table", "rooms")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ID", "r1")
}

func TestAutoJSONWhenNotATerminal(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{"proto":3}`)

	// 不传 -fmt：stdout 是 bytes.Buffer（非 *os.File → 非 TTY），应自动退化成 json。
	code, out, errb := f.run("status")
	assertExit(t, code, exitOK, errb)
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("管道场景应自动输出 JSON，实际=%q", out)
	}
	mustJSON(t, out)
}

func TestFmtSubcommandAppliesToNextCommand(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/rooms", `{"rooms":[{"id":"r1","name":"Lobby","memberCount":1,"members":["a1"]}]}`)

	code, out, errb := f.run("fmt", "table", "rooms")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "ID", "NAME", "r1")

	code, out, errb = f.run("fmt", "json", "rooms")
	assertExit(t, code, exitOK, errb)
	mustJSON(t, out)

	// 不带子命令时打印当前生效格式。
	code, out, errb = f.run("-fmt", "json", "fmt")
	assertExit(t, code, exitOK, errb)
	if v := mustJSON(t, out); v["format"] != "json" {
		t.Errorf("format = %v，期望 json", v["format"])
	}

	code, _, errb = f.run("fmt", "xml", "rooms")
	assertExit(t, code, exitUsage, errb)
}

// ---------------------------------------------------------------------------
// 补全
// ---------------------------------------------------------------------------

func TestCompletionScriptsAreNonEmptyAndCoverCommands(t *testing.T) {
	f := newFakeHub(t)
	for _, shell := range []string{"bash", "zsh", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			code, out, errb := f.run("completion", shell)
			assertExit(t, code, exitOK, errb)
			if strings.TrimSpace(out) == "" {
				t.Fatalf("%s 补全脚本为空", shell)
			}
			if missing := missingFromScript(out); len(missing) > 0 {
				t.Errorf("%s 补全脚本缺少子命令：%v", shell, missing)
			}
		})
	}
	// 补全脚本是"代码"不是"数据"：-fmt json 也必须原样给出可 source 的脚本。
	code, out, errb := f.run("-fmt", "json", "completion", "bash")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "complete -F _hubctl hubctl")
}

// ---------------------------------------------------------------------------
// 彩色 / 地址 / 环境
// ---------------------------------------------------------------------------

func TestColorDetection(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/logs", `{"entries":[{"seq":1,"ts":1700000000000,"level":"error","msg":"boom"}]}`)

	code, out, errb := f.run("-fmt", "table", "-color", "always", "logs")
	assertExit(t, code, exitOK, errb)
	mustContain(t, out, "\x1b[")

	code, out, errb = f.run("-fmt", "table", "-color", "never", "logs")
	assertExit(t, code, exitOK, errb)
	mustNotContain(t, out, "\x1b[")
}

func TestNoColorEnvDisablesColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	f := newFakeHub(t)
	f.json("/api/logs", `{"entries":[{"seq":1,"ts":1,"level":"error","msg":"boom"}]}`)

	code, out, errb := f.run("-fmt", "table", "logs") // auto：非 TTY 已经关色，NO_COLOR 是第二道保险
	assertExit(t, code, exitOK, errb)
	mustNotContain(t, out, "\x1b[")
}

func TestHUBAddrEnvVar(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{"proto":3}`)
	t.Setenv(envHubAddr, f.addr())

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"-fmt", "json", "status"}, &out, &errb) // 不传 -addr
	assertExit(t, code, exitOK, errb.String())
	mustJSON(t, out.String())
}

func TestAddrFlagOverridesEnv(t *testing.T) {
	f := newFakeHub(t)
	f.json("/api/status", `{"proto":3}`)
	t.Setenv(envHubAddr, "http://127.0.0.1:1") // 指到一个必死的地址

	code, out, errb := f.run("-fmt", "json", "status")
	assertExit(t, code, exitOK, errb)
	mustJSON(t, out)
}

// ---------------------------------------------------------------------------
// 纯函数单测
// ---------------------------------------------------------------------------

func TestReadSSEParsesFrames(t *testing.T) {
	raw := strings.Join([]string{
		":ping\n\n",
		"event: log\n",
		"data: {\"msg\":\"a\"}\n",
		"id: 3\n\n",
		"event: log\ndata: {\"msg\":\"b\"}\nid: 4\n\n",
	}, "")
	r := newSSEReader(raw)

	ev, err := r()
	if err != nil {
		t.Fatalf("第一个事件解析失败：%v", err)
	}
	if ev.Event != "log" || ev.ID != "3" || ev.Data != `{"msg":"a"}` {
		t.Errorf("第一个事件 = %+v", ev)
	}
	ev, err = r()
	if err != nil {
		t.Fatalf("第二个事件解析失败：%v", err)
	}
	if ev.ID != "4" || ev.Data != `{"msg":"b"}` {
		t.Errorf("第二个事件 = %+v", ev)
	}
	if _, err := r(); err == nil {
		t.Error("流结束后应返回 EOF")
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"":                      defaultAddr,
		"localhost:8081":        "http://localhost:8081",
		"http://127.0.0.1:9000": "http://127.0.0.1:9000",
		"http://h:8081/":        "http://h:8081",
	}
	for in, want := range cases {
		if got := normalizeAddr(in); got != want {
			t.Errorf("normalizeAddr(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestResolveFormatAndColor(t *testing.T) {
	if got := resolveFormat("", true); got != formatTable {
		t.Errorf("TTY 默认应为 table，实际 %q", got)
	}
	if got := resolveFormat("", false); got != formatJSON {
		t.Errorf("非 TTY 默认应为 json，实际 %q", got)
	}
	if got := resolveFormat("json", true); got != formatJSON {
		t.Errorf("显式 json 应胜出，实际 %q", got)
	}
	if !resolveColor("auto", true) {
		t.Error("auto + TTY 应着色")
	}
	if resolveColor("auto", false) {
		t.Error("auto + 非 TTY 不应着色")
	}
	if !resolveColor("always", false) {
		t.Error("always 应强制着色")
	}
	if resolveColor("never", true) {
		t.Error("never 应强制不色")
	}
}

func TestIsTerminalFalseForNonFileWriters(t *testing.T) {
	var buf bytes.Buffer
	if isTerminal(&buf) {
		t.Error("bytes.Buffer 不是 TTY")
	}
	if isTerminal(io.Discard) {
		t.Error("io.Discard 不是 TTY")
	}
}

func TestHelpAndVersionExit0(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &out, &errb); code != exitOK {
		t.Fatalf("--help 退出码 = %d，期望 0", code)
	}
	mustContain(t, out.String(), "hubctl", "status", "agents", "退出码")

	out.Reset()
	if code := run(context.Background(), []string{"help"}, &out, &errb); code != exitOK {
		t.Fatalf("help 退出码 = %d，期望 0", code)
	}
	mustContain(t, out.String(), "子命令")

	out.Reset()
	if code := run(context.Background(), []string{"--version"}, &out, &errb); code != exitOK {
		t.Fatalf("--version 退出码 = %d，期望 0", code)
	}
	mustContain(t, out.String(), "hubctl")
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// newSSEReader 返回一个逐事件读取器，便于单测直接喂帧。
func newSSEReader(s string) func() (*sseEvent, error) {
	br := bufio.NewReader(strings.NewReader(s))
	return func() (*sseEvent, error) { return readSSE(br) }
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}
