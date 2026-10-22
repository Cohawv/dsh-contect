// Package autobrowser 在 Hub 启动后于默认浏览器打开网页控制台（"双击 EXE 即见 UI"）。
//
// HTTP 地址经共享 KV 从 httpgateway 交接（httpgateway 在 Init 用 SetShared 写入
// "dashboardURL"，本插件依赖 httpgateway 以保证 Init 顺序）；保留 400ms 延迟等端口绑定。
package autobrowser

import (
	"log"
	"os/exec"
	"runtime"
	"time"

	"lan-agent-hub/internal/plugin"
)

// AutoBrowser 是 autobrowser 插件实体。
type AutoBrowser struct {
	open bool
	url  string
}

// Meta 声明插件元信息；依赖 httpgateway 以保证 dashboardURL 交接顺序。
func (p *AutoBrowser) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "autobrowser",
		Version:      "1.0.0",
		Description:  "启动后在默认浏览器打开网页控制台",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Init 读取 open 开关与共享 KV 中的 dashboardURL。
func (p *AutoBrowser) Init(ctx *plugin.Context) error {
	p.open = ctx.Bool("open", true)
	// 插件实例是进程级单例，先清掉上一次装配的残留 URL。
	p.url = ""
	// GetShared 返回 (any, bool)：先判存在，再做类型断言（缺失时保持空串 → Start 静默跳过）。
	if v, ok := ctx.GetShared("dashboardURL"); ok {
		if u, ok := v.(string); ok {
			p.url = u
		}
	}
	return nil
}

// Start 延迟后在默认浏览器打开控制台。
func (p *AutoBrowser) Start() error {
	if !p.open || p.url == "" {
		return nil
	}
	url := p.url
	go func() {
		time.Sleep(400 * time.Millisecond) // 等 HTTP 端口绑定完成
		openBrowser(url)
	}()
	return nil
}

// Stop 无副作用。
func (p *AutoBrowser) Stop() error { return nil }

func init() {
	plugin.Register(&AutoBrowser{})
}

// openBrowser 在系统默认浏览器中打开 URL，零依赖、跨平台。
// 失败仅记日志，不影响服务本身（用户仍可手动访问）。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("自动打开浏览器失败（请手动访问 %s）：%v", url, err)
		return
	}
	log.Printf("已在默认浏览器打开控制台：%s", url)
}
