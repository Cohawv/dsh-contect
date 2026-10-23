package store

import (
	"fmt"
	"net"
	"testing"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/plugins/httpgateway"
)

// TestInitTwiceClosesPreviousEngine：双 Init 时旧引擎必须已关闭。
//
// 回归：V6 ⑧ 插件契约测试（test/plugincontract）对全部已注册插件跑双 Init 时抓出——
// Init 曾直接 `p.engine.Store(eng)` 覆盖，第一个引擎的 messages.log 句柄从不关闭。
// 修复：替换前 `if old := p.engine.Swap(eng); old != nil { _ = old.Close() }`。
//
// 证据方式：对已关闭的 *os.File 再次 Close 必定返回错误；若旧引擎未被关闭，
// 这里的 eng1.Close() 会返回 nil——即证明泄漏仍在。
func TestInitTwiceClosesPreviousEngine(t *testing.T) {
	dir := t.TempDir()
	hub := core.NewHub()
	st := &Store{}

	// store 依赖 httpgateway（复用共享 mux），单独装配会被判 Failed、Init 根本不跑。
	port := freeTCPPort(t)
	mgr := plugin.NewManagerWith([]plugin.Plugin{&httpgateway.HTTPGateway{}, st})
	mgr.ApplyConfig(map[string]bool{"httpgateway": true, "store": true},
		map[string]map[string]any{
			"httpgateway": {"httpAddr": fmt.Sprintf("127.0.0.1:%d", port)},
			"store":       {"path": dir},
		})
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	mgr.InitAll()
	eng1 := st.engine.Load()
	if eng1 == nil {
		t.Fatal("首次 Init 未建立引擎")
	}

	mgr.InitAll() // 第二次 Init：修复前会直接覆盖，旧句柄泄漏
	eng2 := st.engine.Load()
	if eng2 == nil {
		t.Fatal("第二次 Init 未建立引擎")
	}
	if eng1 == eng2 {
		t.Fatal("第二次 Init 未替换引擎（应新建并关闭旧的）")
	}

	if err := eng1.Close(); err == nil {
		t.Fatal("旧引擎未被关闭（对其 Close 应返回错误）——双 Init 泄漏 messages.log 句柄")
	}
	if err := eng2.Close(); err != nil {
		t.Fatalf("当前引擎应处于打开状态，Close 实得: %v", err)
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
