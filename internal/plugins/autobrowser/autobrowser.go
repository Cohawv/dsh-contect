// Package autobrowser 在 Hub 启动后于默认浏览器打开网页控制台（"双击 EXE 即见 UI"）。
//
// HTTP 地址经共享 KV 从 httpgateway 交接（httpgateway 在 Init 用 SetShared 写入
// "dashboardURL"，本插件依赖 httpgateway 以保证 Init 顺序）；保留 400ms 延迟等端口绑定。
package autobrowser

import (
	"errors"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"lan-agent-hub/internal/plugin"
)

// AutoBrowser 是 autobrowser 插件实体。
//
// 为什么要加锁（F6）：StopAll（进程停机）**不持** Manager.opMu，而运行期的
// Disable/Apply 持 opMu——两者可以同时进到本插件，出现「Stop 关 done」与
// 「Start 重建 done」并发写同一字段。加一把小锁把 open/url/done 三个字段的读写收口。
type AutoBrowser struct {
	mu     sync.Mutex
	open   bool
	url    string
	done   chan struct{}      // Stop 时关闭：取消「延迟 400ms 打开浏览器」的协程
	openFn func(url string)   // 可注入：默认 openBrowser（单测借此断言「是否真的打开」）
	delay  time.Duration      // 可注入：默认 openDelay（单测借此免等 400ms）
}

// Meta 声明插件元信息；依赖 httpgateway 以保证 dashboardURL 交接顺序。
func (p *AutoBrowser) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "autobrowser",
		Version:      "1.1.0",
		Description:  "启动后在默认浏览器打开网页控制台",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
func (p *AutoBrowser) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "open",
			Type:        plugin.OptionBool,
			Default:     true,
			Description: "启动时自动打开浏览器（关闭后仍需手动访问控制台地址）",
		},
	}
}

// Apply 实现 plugin.Applier：open 只是一个「下次启动是否打开浏览器」的开关，
// 本插件不持有 socket / 协程，因此热更新无需任何重建动作，语义上立即生效。
func (p *AutoBrowser) Apply(options map[string]any) error {
	if v, ok := options["open"]; ok {
		open, isBool := v.(bool)
		if !isBool {
			return errors.New("autobrowser: open 选项应为布尔值")
		}
		p.mu.Lock()
		p.open = open
		p.mu.Unlock()
	}
	return nil
}

// Init 读取 open 开关与共享 KV 中的 dashboardURL。
func (p *AutoBrowser) Init(ctx *plugin.Context) error {
	// GetShared 返回 (any, bool)：先判存在，再做类型断言（缺失时保持空串 → Start 静默跳过）。
	url := ""
	if v, ok := ctx.GetShared("dashboardURL"); ok {
		if u, ok := v.(string); ok {
			url = u
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.open = ctx.Bool("open", true)
	// 注入点默认值（生产路径为 openBrowser + openDelay；单测可覆盖以便断言）。
	if p.openFn == nil {
		p.openFn = openBrowser
	}
	if p.delay <= 0 {
		p.delay = openDelay
	}
	// 插件实例是进程级单例：先清掉上一次装配的残留。
	p.url = url
	// 双 Init 幂等：上一轮 Start 起的 goroutine 还拿着旧 done，先关掉再换新的
	// （否则旧 goroutine 的取消信道失效，Stop 关的永远是新 done——契约测试抓出）。
	if p.done != nil {
		close(p.done)
	}
	// 每次装配换一个全新的取消信道——上一轮 Stop 关掉的那个不能复用。
	p.done = make(chan struct{})
	return nil
}

// Start 延迟后在默认浏览器打开控制台。
// 延迟期间若插件被 Stop（done 关闭）则取消打开动作，避免「刚停服又弹出浏览器」。
func (p *AutoBrowser) Start() error {
	p.mu.Lock()
	open, url, done := p.open, p.url, p.done
	openFn, delay := p.openFn, p.delay
	if openFn == nil { // 防御：未经 Init 直接被 Start（单测直连场景）
		openFn = openBrowser
	}
	if delay <= 0 {
		delay = openDelay
	}
	if done == nil {
		done = make(chan struct{})
		p.done = done
	}
	p.mu.Unlock()

	if !open || url == "" {
		return nil
	}
	go func() {
		select {
		case <-time.After(delay): // 等 HTTP 端口绑定完成
			openFn(url)
		case <-done:
			return
		}
	}()
	return nil
}

// Stop 取消尚未触发的「打开浏览器」动作。
func (p *AutoBrowser) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done != nil {
		close(p.done)
		p.done = nil
	}
	return nil
}

func init() {
	plugin.Register(&AutoBrowser{})
}

// openDelay 是「等 HTTP 端口绑定完成」的延迟（可注入字段 delay 的默认值）。
const openDelay = 400 * time.Millisecond

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
