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
