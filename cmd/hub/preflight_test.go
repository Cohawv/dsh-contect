package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOldHub 模拟一个占着端口的旧 Hub 实例：/healthz 正常应答，
// /api/shutdown 记录被调用（可配置为老版本行为=404）。
type fakeOldHub struct {
	srv        *httptest.Server
	shutdowned atomic.Bool
	oldVersion bool // true = /api/shutdown 返回 404（旧版本不支持远程关停）
}

func newFakeOldHub(t *testing.T, oldVersion bool) *fakeOldHub {
	f := &fakeOldHub{oldVersion: oldVersion}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if f.oldVersion {
			http.NotFound(w, r)
			return
		}
		f.shutdowned.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"shutting-down"}`))
	})
	// 非 Hub 程序的样子：任何路径都 404（没有 /healthz）。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestPreflightFreePortPasses(t *testing.T) {
	// 16999 端口大概率空闲；即便被占，测试也只是红——比 mock 整个 TCP 栈诚实。
	if err := preflightPorts(":16999", "127.0.0.1:16998", false); err != nil {
		t.Fatalf("空闲端口不应报错：%v", err)
	}
}

func TestPreflightHubOccupiedWithoutReplaceFails(t *testing.T) {
	old := newFakeOldHub(t, false)
	_, port := splitAddr(t, old.srv.URL)

	err := preflightPorts("127.0.0.1:"+port, "127.0.0.1:"+port, false)
	if err == nil {
		t.Fatal("旧 Hub 占用端口且未 -replace，预检必须失败")
	}
	for _, want := range []string{"LAN Agent Hub", "-replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案缺 %q：%v", want, err)
		}
	}
	if old.shutdowned.Load() {
		t.Error("未带 -replace 时绝不应请求旧实例关停")
	}
}

func TestPreflightReplaceTakesOverOldHub(t *testing.T) {
	old := newFakeOldHub(t, false)
	_, port := splitAddr(t, old.srv.URL)

	// 「等端口释放」注入为立即成功：真实旧 Hub 收到 shutdown 后会自行退出释放端口，
	// 而 httptest server 不会——注入点把「关停后续」与「接管判定」解耦。
	waitPortFree = func(string, int, time.Duration) error { return nil }
	defer func() { waitPortFree = defaultWaitPortFree }()

	if err := preflightPorts("127.0.0.1:"+port, "127.0.0.1:"+port, true); err != nil {
		t.Fatalf("-replace 接管旧 Hub 应成功：%v", err)
	}
	if !old.shutdowned.Load() {
		t.Error("-replace 必须请求旧实例优雅关停")
	}
}

func TestPreflightReplaceRefusesOldVersion(t *testing.T) {
	old := newFakeOldHub(t, true) // 老版本：/api/shutdown 是 404
	_, port := splitAddr(t, old.srv.URL)

	err := preflightPorts("127.0.0.1:"+port, "127.0.0.1:"+port, true)
	if err == nil {
		t.Fatal("旧版本不支持远程关停，接管必须失败")
	}
	if !strings.Contains(err.Error(), "过旧") {
		t.Errorf("错误文案应提示版本过旧：%v", err)
	}
	if old.shutdowned.Load() {
		t.Error("404 的 shutdown 不应被当作已关停")
	}
}

// TestPreflightNeverKillsForeignProcess 是安全边界的核心：
// 占端口的是无关程序（没有 /healthz 特征）时，-replace 也绝不代杀。
// 反向验证口径：isHub 判定改成无条件 true，本用例必须红。
func TestPreflightNeverKillsForeignProcess(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 任意 web 服务：没有 Hub 的健康特征
	}))
	defer foreign.Close()
	_, port := splitAddr(t, foreign.URL)

	err := preflightPorts("127.0.0.1:"+port, "127.0.0.1:"+port, true)
	if err == nil {
		t.Fatal("无关程序占用端口，预检必须失败（绝不代杀）")
	}
	if !strings.Contains(err.Error(), "其它程序") {
		t.Errorf("错误文案应指明是其它程序：%v", err)
	}
}

func splitAddr(t *testing.T, url string) (host, port string) {
	t.Helper()
	// httptest.Server.URL 形如 http://127.0.0.1:port
	rest := strings.TrimPrefix(url, "http://")
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		t.Fatalf("URL 没有端口：%q", url)
	}
	return rest[:i], rest[i+1:]
}
