package autobrowser

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/plugins/httpgateway"
)

// TestInitTwiceCancelsStaleOpen：双 Init 时上一轮 Start 起的「延迟打开浏览器」
// 协程必须被取消——否则它拿着的旧 done 永不关闭，400ms 后会真的弹出浏览器。
//
// 回归：V6 ⑧ 插件契约测试抓出——Init 曾无条件 `p.done = make(...)` 换掉信道，
// 旧协程的取消信道失效（Stop 关的永远是新的那个）。
// 修复：Init 里 `if p.done != nil { close(p.done) }` 先关旧的再建新的。
//
// 证据方式：注入 openFn 计数器与短 delay；若旧协程未被取消，它会真的调用
// openFn（计数 +1）；修复后计数应恒为 0。
func TestInitTwiceCancelsStaleOpen(t *testing.T) {
	hub := core.NewHub()
	ab := &AutoBrowser{}
	var opened int32
	ab.openFn = func(string) { atomic.AddInt32(&opened, 1) }
	ab.delay = 80 * time.Millisecond

	port := freeTCPPort(t)
	mgr := plugin.NewManagerWith([]plugin.Plugin{&httpgateway.HTTPGateway{}, ab})
	mgr.ApplyConfig(map[string]bool{"httpgateway": true, "autobrowser": true},
		map[string]map[string]any{
			"httpgateway": {"httpAddr": fmt.Sprintf("127.0.0.1:%d", port)},
			"autobrowser": {"open": true},
		})
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()

	// dashboardURL 由 httpgateway 经共享 KV 交接；本用例不启动 HTTP 服务，
	// 直接给一个占位地址（Start 只做「延迟后打开」，不校验可达性）。
	ab.mu.Lock()
	if ab.url == "" {
		ab.url = "http://127.0.0.1:1/dashboard"
	}
	ab.mu.Unlock()

	if err := ab.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mgr.InitAll() // 第二次 Init：修复前会丢弃旧 done，旧协程取消不了

	time.Sleep(300 * time.Millisecond) // 远大于 delay(80ms)
	if n := atomic.LoadInt32(&opened); n != 0 {
		t.Fatalf("旧协程未被取消（打开浏览器 %d 次）——双 Init 泄漏 done 信道", n)
	}

	// 收尾：Stop 幂等，且当前这一轮也不应再打开
	if err := ab.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := ab.Stop(); err != nil {
		t.Fatalf("重复 Stop 应安全，实得: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(&opened); n != 0 {
		t.Fatalf("Stop 后仍打开了浏览器 %d 次", n)
	}
}

// freeTCPPort 取一个当前空闲的 TCP 端口（httpgateway 需要合法地址，测试不启动监听）。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测端口失败: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
