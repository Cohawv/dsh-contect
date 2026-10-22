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
	"lan-agent-hub/internal/plugins/plugininfo"

	_ "lan-agent-hub/cmd/hub/plugins" // 功能总清单：blank import 内置插件（含 plugininfo）
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入；默认值用于本地开发。
var version = "dev"

// main 只做装配：解析 flag → 加载配置 → 建 hub → 注册/构建插件 → 启动 → 等信号 → 优雅停机。
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
	configFlag := flag.String("config", "", "path to hub.json (default: next to the executable, else in the working directory)")
	configReadOnly := flag.Bool("config-readonly", false, "read-only mode: never write hub.json or its changelog")
	flag.Parse()

	// 让 /api/manifest 的版本与 -version 输出一致。
	plugininfo.SetHubVersion(version)

	if *showVersion {
		fmt.Printf("lan-agent-hub %s\n", version)
		fmt.Println("compiled plugins:")
		for _, p := range plugin.All() {
			m := p.Meta()
			fmt.Printf("  - %s %s\n", m.Name, m.Version)
		}
		return
	}

	// ① 配置文件：读取（缺失则视为全默认），非只读模式且首次运行时生成模板。
	cfgPath := resolveConfigPath(*configFlag)
	store := plugin.NewStore(cfgPath)
	fileCfg, existed, err := store.LoadOrTemplate()
	if err != nil {
		log.Fatalf("加载配置 %q 失败: %v", cfgPath, err)
	}
	if !existed {
		if *configReadOnly {
			log.Printf("配置文件 %q 不存在（只读模式，不生成）", cfgPath)
		} else if err := store.SaveWithVersion(fileCfg, 0); err != nil {
			log.Printf("生成默认配置 %q 失败（继续使用内存默认值）: %v", cfgPath, err)
		} else {
			log.Printf("已生成默认配置模板: %s", cfgPath)
		}
	}
	log.Printf("配置文件: %s (readOnly=%t)", cfgPath, *configReadOnly)

	hub := core.NewHub()
	hub.TCPPort, hub.HTTPPort = portOf(*addr), portOf(*httpAddr)

	// ② 合并配置：CLI 显式参数 > hub.json > Meta.DefaultOn/选项默认值。
	enabled, options := configToMaps(fileCfg)
	applyToggles(enabled, *white, *black)
	mergeOptions(options, map[string]map[string]any{
		"tcpgateway":  {"addr": *addr},
		"httpgateway": {"httpAddr": *httpAddr},
		"discovery":   {"bindAddr": *discoverAddr, "hubName": *hubName, "noDiscovery": *noDiscovery},
		"autobrowser": {"open": *openBrowserFlag},
	})

	mgr := plugin.NewManager()
	mgr.SetConfigPath(cfgPath, *configReadOnly)
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

// resolveConfigPath 决定 hub.json 的路径。
//
// 优先级：显式 -config > exe 同目录已存在的 hub.json > 工作目录已存在的 hub.json >
// exe 同目录（不存在则在首次运行时按此路径生成模板）。
func resolveConfigPath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	const name = "hub.json"
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	candidate := filepath.Join(filepath.Dir(exe), name)
	if _, statErr := os.Stat(candidate); statErr == nil {
		return candidate
	}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		inWd := filepath.Join(wd, name)
		if _, statErr := os.Stat(inWd); statErr == nil {
			return inWd
		}
	}
	return candidate
}

// configToMaps 把文件配置展开成「每插件启用状态 + 配置项」两张表。
// 未在文件中出现的插件走 Meta.DefaultOn 与空选项（再由 mergeOptions 补 CLI 值）。
func configToMaps(cfg plugin.Config) (enabled map[string]bool, options map[string]map[string]any) {
	enabled = make(map[string]bool, len(plugin.All()))
	options = make(map[string]map[string]any, len(plugin.All()))
	for _, p := range plugin.All() {
		name := p.Meta().Name
		enabled[name] = p.Meta().DefaultOn
		options[name] = map[string]any{}
	}
	for name, pc := range cfg.Plugins {
		if _, known := enabled[name]; !known {
			continue // 文件里有、二进制里没有的插件（例如手工删掉 import）：忽略
		}
		enabled[name] = pc.Enabled
		for k, v := range pc.Options {
			options[name][k] = v
		}
	}
	return enabled, options
}

// mergeOptions 把 CLI 提供的配置项合并进 options（CLI 优先），不引入新插件键。
func mergeOptions(options, override map[string]map[string]any) {
	for name, opts := range override {
		if _, ok := options[name]; !ok {
			options[name] = map[string]any{}
		}
		for k, v := range opts {
			options[name][k] = v
		}
	}
}

// applyToggles 根据白/黑名单就地修改启用状态（缺省走配置文件/Meta.DefaultOn）。
func applyToggles(enabled map[string]bool, white, black string) {
	whiteSet := splitList(white)
	blackSet := splitList(black)
	for _, p := range plugin.All() {
		name := p.Meta().Name
		on, ok := enabled[name]
		if !ok {
			on = p.Meta().DefaultOn
		}
		if len(whiteSet) > 0 {
			on = whiteSet[name]
		}
		if blackSet[name] {
			on = false
		}
		enabled[name] = on
	}
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
