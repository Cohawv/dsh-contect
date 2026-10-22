package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"

	_ "lan-agent-hub/cmd/hub/plugins" // 功能总清单：blank import 6 个内置插件
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入；默认值用于本地开发。
var version = "dev"

// main 只做装配：解析 flag → 建 hub → 注册/构建插件 → 启动 → 等信号 → 优雅停机。
func main() {
	initLogging()
	addr := flag.String("addr", ":8080", "hub TCP listen address, e.g. :8080 or 0.0.0.0:8080")
	httpAddr := flag.String("http", ":8081", "hub HTTP gateway address (REST), e.g. :8081")
	discoverAddr := flag.String("discover", ":8082", "UDP discovery bind address; set empty to disable")
	noDiscovery := flag.Bool("no-discovery", false, "disable LAN auto-discovery beacon")
	hubName := flag.String("name", "", "hub display name (default: hostname)")
	showVersion := flag.Bool("version", false, "print version and exit")
	openBrowserFlag := flag.Bool("open", true, "auto-open the web dashboard in the default browser on start")
	white := flag.String("plugin", "", "comma-separated plugin whitelist; empty = all enabled")
	black := flag.String("disable-plugin", "", "comma-separated plugin blacklist")
	flag.Parse()

	if *showVersion {
		fmt.Printf("lan-agent-hub %s\n", version)
		fmt.Println("compiled plugins:")
		for _, p := range plugin.All() {
			m := p.Meta()
			fmt.Printf("  - %s %s\n", m.Name, m.Version)
		}
		return
	}

	hub := core.NewHub()
	hub.TCPPort, hub.HTTPPort = portOf(*addr), portOf(*httpAddr)

	mgr := plugin.NewManager()
	enabled := applyToggles(*white, *black)
	options := map[string]map[string]any{
		"tcpgateway":  {"addr": *addr},
		"httpgateway": {"httpAddr": *httpAddr},
		"discovery":   {"bindAddr": *discoverAddr, "hubName": *hubName, "noDiscovery": *noDiscovery},
		"autobrowser": {"open": *openBrowserFlag},
	}
	mgr.ApplyConfig(enabled, options)

	if err := mgr.BuildContext(hub); err != nil {
		log.Fatalf("build context: %v", err)
	}
	mgr.InitAll()
	// StartAll 返回启动结果表 + 致命错误（必需插件被禁用，或必需插件 Start 失败，如端口被占）。
	// 失败时先把每个失败插件的原因打出来，再退出——否则「双击两次 EXE / 端口被占」会变成
	// 打印"启动完成"却没有监听口的静默僵尸进程。
	results, err := mgr.StartAll()
	if err != nil {
		for _, r := range results {
			if r.State == plugin.StateFailed {
				log.Printf("插件 %s 启动失败: %s", r.Name, r.Error)
			}
		}
		log.Fatalf("启动失败: %v", err)
	}

	enabledList, disabledList := pluginStates(mgr)
	log.Printf("enabled=[%s] disabled=[%s]", strings.Join(enabledList, ","), strings.Join(disabledList, ","))
	log.Printf("LAN Agent Hub 启动完成 (tcp=%s http=%s)", *addr, *httpAddr)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	log.Printf("收到 %v，开始停机...", sig)

	mgr.StopAll()
	log.Printf("停机摘要: %s", mgr.Summary())
}

// applyToggles 根据白/黑名单计算每个插件的启用状态（缺省走 Meta.DefaultOn）。
func applyToggles(white, black string) map[string]bool {
	enabled := map[string]bool{}
	whiteSet := splitList(white)
	blackSet := splitList(black)
	for _, p := range plugin.All() {
		name := p.Meta().Name
		on := p.Meta().DefaultOn
		if len(whiteSet) > 0 {
			on = whiteSet[name]
		}
		if blackSet[name] {
			on = false
		}
		enabled[name] = on
	}
	return enabled
}

// splitList 把逗号分隔的列表解析为集合（忽略空白项）。
func splitList(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// pluginStates 返回当前启用的与禁用的插件名（按 Info 状态划分）。
func pluginStates(mgr *plugin.Manager) (enabled, disabled []string) {
	for _, info := range mgr.Infos() {
		if info.State == plugin.StateDisabled {
			disabled = append(disabled, info.Name)
		} else {
			enabled = append(enabled, info.Name)
		}
	}
	return
}

// portOf 从 ":8080" / "0.0.0.0:8080" 中提取端口号。
func portOf(addr string) int {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		return 0
	}
	return p
}

// initLogging 决定日志去向：若标准输出是终端（控制台编译/在终端运行）就照常打到控制台；
// 若不是（如用 -H=windowsgui 编译的无窗口版本，stdout 无效；或被管道/服务托管运行），
// 则写入 exe 同目录的日志文件，这样无窗口运行也能排障。零依赖、跨平台。
//
// V3 收尾：非终端场景下额外把日志并写到 stderr（文件在前，保证落盘优先）。
// 目的：被管道/CI 托管运行时，启动失败原因（如端口被占）仍能从 stderr 看到，
// 不会只剩一个非零退出码；无窗口 GUI 版本没有有效 stderr，写入失败被忽略，行为不变。
func initLogging() {
	if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		return // 终端：保持默认（控制台）
	}
	if exe, err := os.Executable(); err == nil {
		logPath := filepath.Join(filepath.Dir(exe), "lan-agent-hub.log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			log.SetOutput(io.MultiWriter(f, os.Stderr))
		}
	}
}
