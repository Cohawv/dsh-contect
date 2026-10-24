package httpgateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
)

// TestGuardHandlerTimeout 验证单个 API handler 超过上限时返回 503 + JSON 体。
//
// 为什么必须返回 JSON：控制台的 fetch 一律按 JSON 解析，返回纯文本会先抛
// SyntaxError，把「服务端超时」伪装成「前端解析错误」——排查方向直接跑偏。
func TestGuardHandlerTimeout(t *testing.T) {
	p := &HTTPGateway{handlerTimeout: 50 * time.Millisecond}
	h := p.guard(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("too late"))
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != handlerTimeoutBody {
		t.Fatalf("响应体 = %q，期望 %q", got, handlerTimeoutBody)
	}
}

// TestGuardPassesThroughFastHandler 验证正常路径逐字节不变。
//
// 这是回归护栏：guard 是包在所有端点外面的一层，一旦它改写了 header 或状态码，
// 11 个端点的契约基线会成片变红，而根因只在中间件里。
func TestGuardPassesThroughFastHandler(t *testing.T) {
	p := &HTTPGateway{handlerTimeout: time.Second}
	h := p.guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Proto-Version", "3")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201", rec.Code)
	}
	if got := rec.Header().Get("X-Proto-Version"); got != "3" {
		t.Fatalf("X-Proto-Version = %q，期望 3（header 被中间件丢了）", got)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("响应体 = %q", got)
	}
}

// TestGuardZeroDisablesTimeout 验证 handlerTimeout=0 表示关闭保护。
func TestGuardZeroDisablesTimeout(t *testing.T) {
	p := &HTTPGateway{handlerTimeout: 0}
	h := p.guard(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write([]byte("slow but fine"))
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "slow but fine" {
		t.Fatalf("handlerTimeout=0 应不掐断，实得 %d %q", rec.Code, rec.Body.String())
	}
}

// TestStreamingEndpointNotGuarded 验证流式端点不会被 handler 超时误杀。
//
// 这是本阶段最危险的一处边界：TimeoutHandler 的语义是「到点返回 503 并丢弃后续写入」，
// 套在 SSE 上等于给一条设计上要挂几小时的长连接设了死刑。
// 因此 guard 只包本插件自己的 11 个短端点，流式端点由各自的插件直接挂到共享 mux。
// 这个用例按真实装配方式搭（自己的端点经 guard，别人的流式端点不经 guard），
// 若哪天有人图省事把 guard 提到共享 mux 上，这里会立刻变红。
func TestStreamingEndpointNotGuarded(t *testing.T) {
	p := &HTTPGateway{handlerTimeout: 50 * time.Millisecond}

	mux := http.NewServeMux()
	// 本插件的短端点：经 guard
	mux.HandleFunc("/api/status", p.guard(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	// 模拟 realtime 注册的 SSE 端点：直接挂 mux，总耗时 400ms 远超 50ms 上限
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		f, _ := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			_, _ = w.Write([]byte("data: tick\n\n"))
			if f != nil {
				f.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cli := srv.Client()
	cli.Timeout = 3 * time.Second
	resp, err := cli.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatalf("GET /api/stream: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读流: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("流式端点状态码 = %d（被 handler 超时掐了，SSE 会全程不可用）", resp.StatusCode)
	}
	if n := strings.Count(string(body), "data: tick"); n != 8 {
		t.Fatalf("收到 %d 条事件，期望 8 条（流被提前截断）body=%q", n, string(body))
	}
}

// TestRegisterRoutesStaysOffStreamPaths 钉住「本插件不注册任何流式端点」。
// 上一条用例验证的是装配切分的结果，这一条验证的是它的前提：
// 一旦 httpgateway 的端点列表里出现 /api/stream 这类路径，guard 就会真的套上去。
//
// 数量是 10：registerRoutes 只登记 10 个 /api 端点；更新通道的 2 个预留端点
// （manifest / artifact）由 attachUpdateRoutes 直接挂到 mux，不计入本表。
func TestRegisterRoutesStaysOffStreamPaths(t *testing.T) {
	var patterns []string
	registerRoutes(func(pattern string, _ http.HandlerFunc) {
		patterns = append(patterns, pattern)
	}, core.NewHub(), nil, nil)

	if len(patterns) != 10 {
		t.Fatalf("端点数量 = %d，期望 10（新增端点需同步更新本断言与契约基线）", len(patterns))
	}
	for _, p := range patterns {
		if strings.Contains(p, "stream") {
			t.Fatalf("本插件不应注册流式端点，发现 %q", p)
		}
	}
}

// TestReadHeaderTimeoutClosesSlowloris 是 V8 ⑤ 最初的那个问题：只发半个请求头的连接。
//
// 实测背景：没有 ReadHeaderTimeout 时，这样一条连接会一直挂着，
// 50 条就让 goroutine 从 21 涨到 71 并永不自愈（每条一协程一 fd）——
// 外部单点就能把服务端资源耗尽。
//
// 断言方式：服务端必须**主动关闭**连接（客户端 Read 拿到 EOF），
// 而不是等到客户端自己的 2 秒 deadline 才返回（那说明服务端根本没管）。
func TestReadHeaderTimeoutClosesSlowloris(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// 走 newServer 而不是自己拼 http.Server：这样断言的是「本插件的配置真的传导了」，
	// 而不是「Go 标准库的 ReadHeaderTimeout 能用」。
	p := &HTTPGateway{httpAddr: "127.0.0.1:0", readHeaderTimeout: 80 * time.Millisecond}
	srv := p.newServer(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 只发半个请求头（没有结尾空行），然后什么都不做。
	if _, err := conn.Write([]byte("GET /api/status HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatalf("写半个请求头: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("服务端返回了数据，不应该（请求头没读完）")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("2 秒内服务端没有关闭慢连接：ReadHeaderTimeout 未生效（Slowloris 可复现）")
	}
}
