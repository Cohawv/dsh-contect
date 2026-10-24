package httpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
)

// shutdownRoutes 装配一个最小路由表：只把 /api/shutdown 挂出来，
// exit 函数换成可观察的 no-op（否则 handler 会把测试进程一起带走）。
func shutdownRoutes(t *testing.T, exitCalled *atomic.Bool) (http.Handler, func()) {
	t.Helper()
	handlers := map[string]http.HandlerFunc{}
	exitCh := make(chan struct{})
	registerRoutes(func(pattern string, h http.HandlerFunc) {
		handlers[pattern] = h
	}, core.NewHub(), nil, nil, func(int) {
		exitCalled.Store(true)
		close(exitCh) // 通知测试「退出点已到达」
	})
	srvDone := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hh, ok := handlers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hh(w, r)
	})
	return h, func() {
		select {
		case <-exitCh:
		case <-time.After(3 * time.Second):
			t.Errorf("3s 内未走到退出点（异步停机 goroutine 未执行）")
		}
		close(srvDone)
	}
}

func TestShutdownRequiresPost(t *testing.T) {
	exitCalled := &atomic.Bool{}
	h, waitExit := shutdownRoutes(t, exitCalled)
	_ = waitExit

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/shutdown", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/shutdown 状态码 = %d，期望 405", rec.Code)
	}
}

// TestShutdownRejectsRemote 是安全边界的核心断言：非 loopback 来源必须 403。
// 反向验证口径：删掉 handler 里的 IsLoopback 校验，本用例必须红。
func TestShutdownRejectsRemote(t *testing.T) {
	exitCalled := &atomic.Bool{}
	h, waitExit := shutdownRoutes(t, exitCalled)
	_ = waitExit

	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	req.RemoteAddr = "192.0.2.50:52341" // 局域网来源
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 loopback 来源状态码 = %d，期望 403（局域网任何人都能关 Hub 是不可接受的）", rec.Code)
	}
}

func TestShutdownFromLoopbackIsGraceful(t *testing.T) {
	exitCalled := &atomic.Bool{}
	h, waitExit := shutdownRoutes(t, exitCalled)

	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:51000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("本机 shutdown 状态码 = %d，期望 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%q）", err, rec.Body.String())
	}
	if body["status"] != "shutting-down" {
		t.Errorf("响应 status = %q，期望 shutting-down", body["status"])
	}
	// 响应已回、异步停机必然发生——等到退出点被调用为止。
	waitExit()
	if !exitCalled.Load() {
		t.Fatalf("退出函数未被调用")
	}
}
