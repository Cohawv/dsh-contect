package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/hubbridge"
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
	// V8f：超时预设档。留空 = 完全不干预（一律走插件自己声明的默认值）。
	// 注意语义是「只填补未显式配置的项」，见 timeoutPresetOptions 上方的说明。
	timeoutsFlag := flag.String("timeouts", "",
		"HTTP timeout preset: strict|relaxed|off (empty = leave plugin defaults); only fills options you did not set explicitly")
	// V7d：-replace 接管。仅当端口占用者被 /healthz 确认是旧 Hub 实例时生效；
	// 无关程序绝不代杀（见 preflight.go 顶部说明）。
	replaceFlag := flag.Bool("replace", false, "take over from an existing hub instance listening on the same ports (asks it to shut down gracefully; never touches non-hub processes)")
	flag.Parse()

	// 让 /api/manifest 的版本与 -version 输出一致。
	plugininfo.SetHubVersion(version)
	// V5：健康详情（/api/health/detail）报的 version 来自框架侧副本，
	// 与上面是同一个字符串（两处都不允许自己去猜版本号）。
	plugin.SetVersion(version)

	if *showVersion {
		fmt.Printf("lan-agent-hub %s\n", version)
		fmt.Println("compiled plugins:")
		for _, p := range plugin.All() {
			m := p.Meta()
			fmt.Printf("  - %s %s\n", m.Name, m.Version)
		}
		return
	}

	// V8f：超时预设档先解析。放在读配置之前是刻意的——参数拼错必须**立刻**报错退出，
	// 不能先把 hub.json 模板生成出来再退出：一次手误就在用户目录里留下一个
	// 看起来正常的配置文件，反而掩盖了真正的问题（参数错了）。
	timeoutFill, timeoutErr := timeoutPresetOptions(*timeoutsFlag)
	if timeoutErr != nil {
		log.Fatalf("参数错误: %v", timeoutErr)
	}

	// V7d：端口预检放在一切耗时工作之前——占端口的错误越早暴露越好，
	// 文案见 preflightOne：三条出路（关旧实例 / -replace / 换端口）写进错误里。
	if err := preflightPorts(*addr, *httpAddr, *replaceFlag); err != nil {
		log.Fatalf("端口预检失败: %v", err)
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

	// V8f：超时预设档只填「用户还没表态」的项（判定规则见 fillPresetOptions）。
	// 顺序很重要：必须在 mergeOptions 之后（否则文件/CLI 的值会被当成"缺席"），
	// 且在 ApplyConfig / InitAll 之前（否则 httpgateway 已经按默认值解析完了）。
	if len(timeoutFill) > 0 {
		applied := fillPresetOptions(options, httpgatewayPlugin,
			declaredDefaults(httpgatewayPlugin), timeoutFill)
		if len(applied) > 0 {
			// 日志只报**实际应用**的项，不报整个预设档：
			// 用户看到 readHeaderTimeout=5s 却明明配了 30s 时，第一反应就是"被覆盖了"，
			// 而真相是那一项根本没被碰过。日志必须能自证这一点。
			log.Printf("超时预设档 -timeouts=%s 已应用: %s（未列出的项已被显式配置，保持原值）",
				strings.ToLower(strings.TrimSpace(*timeoutsFlag)), describeOptions(applied))
		} else {
			log.Printf("超时预设档 -timeouts=%s 未应用任何项：httpgateway 的超时项均为显式配置",
				strings.ToLower(strings.TrimSpace(*timeoutsFlag)))
		}
	}

	mgr := plugin.NewManager()
	mgr.SetConfigPath(cfgPath, *configReadOnly)
	mgr.ApplyConfig(enabled, options)
	// V5：把事件总线交给框架。此后插件启停会发布 plugin.state、
	// 每次 HTTP 请求会发布 http.request —— 实时推送与指标因此有数据源。
	// 必须在 InitAll 之前注入，否则启动阶段的状态迁移不会产生事件。
	mgr.SetEventBus(hub.Bus)

	if err := mgr.BuildContext(hub); err != nil {
		log.Fatalf("build context: %v", err)
	}
	// V6 ②：把内核能力（只读视图 + 发布能力）接进框架。此前 Publisher/HubView
	// 一直是空实现——声明了接口却没有生产消费方。group 插件（V6b）是第一个
	// 真实使用者：分组路由经 Publisher.Send 发消息，动态组成员经 View.Agents 解析。
	mgr.SetHub(hubbridge.NewView(hub), hubbridge.NewPublisher(hub))
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

// httpgatewayPlugin 是 httpgateway 插件名（超时预设档的唯一作用目标）。
//
// 用常量而不是各处写字面量：插件名拼错的症状是「预设档静默不生效」，
// 而启动日志里看起来一切正常——这类失效最难发现。
const httpgatewayPlugin = "httpgateway"

// timeoutPresetOptions 返回 -timeouts 预设档对应的配置项集合（nil = 不干预）。
//
// 三档的语义是「面向什么客户端」，不是三组魔法数字：
//   - strict：公网 / 不可信客户端。任何人连上来都能用「只发半个请求头」占一个协程一个 fd
//     （Slowloris，V8e 实测 50 条这样的连接就把 goroutine 从 21 顶到 71 且永不自愈），
//     因此读头与 handler 都收紧（5s / 15s），空闲连接 30s 就收割；
//   - relaxed：内网大扇出。控制台轮询、SSE、批量调用都在内网，读头放宽到 10s，
//     空闲 120s（与插件默认一致），handler 60s —— 给大房间扇出的慢端点留余量，
//     否则一个 600 人社群的 broadcast 就可能撞上 15s 上限；
//   - off：全部填 0（关闭）。专用于排查「是不是超时把连接掐了」——
//     此时要的是一个不加任何超时干预的基线，数字难看也没关系。
//
// **刻意不携带 writeTimeout / readIdleTimeout（更不含全局 ReadTimeout / WriteTimeout）**：
// WriteTimeout 从读完请求头开始计时、覆盖整个响应写出过程，SSE 挂到第 N 秒就会被硬切；
// ReadTimeout 到期会给连接打上永久的读截止时间，长连接的后续读全部失败。
// V8e 已经为这两个字段踩过坑（它们是历史行为里"故意没有"的东西，不是遗漏），
// 因此预设档永远不带这两个键——将来若真要为它们加档位，必须先改这段注释与测试。
//
// 另注：httpgateway 目前只声明了 readHeaderTimeout / idleTimeout / handlerTimeout 三个
// 超时选项。此处只写它真正声明的键，绝不顺手塞一个"看起来该有"的键：
// 未声明的键会被 Context 静默忽略——预设档写了却没有效果，比没写更糟。
func timeoutPresetOptions(name string) (map[string]any, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		// 留空 = 不干预：连空 map 都不返回，调用方据此完全跳过这一步。
		return nil, nil
	case "strict":
		return map[string]any{
			"readHeaderTimeout": "5s",
			"idleTimeout":       "30s",
			"handlerTimeout":    "15s",
		}, nil
	case "relaxed":
		return map[string]any{
			"readHeaderTimeout": "10s",
			"idleTimeout":       "120s",
			"handlerTimeout":    "60s",
		}, nil
	case "off":
		// 显式 0：httpgateway 侧把 <=0 视为关闭（不设 ReadHeaderTimeout/IdleTimeout、
		// 不套 TimeoutHandler）。用 "0s" 而不是数字 0，是为了在 hub.json 与日志里
		// 保持"时长"的写法一致（其它档位也是 "5s" 这种形态）。
		return map[string]any{
			"readHeaderTimeout": "0s",
			"idleTimeout":       "0s",
			"handlerTimeout":    "0s",
		}, nil
	default:
		return nil, fmt.Errorf("-timeouts=%q 不认识：只接受 strict / relaxed / off（留空表示不干预，走插件默认值）", name)
	}
}

// declaredDefaults 返回某个插件声明的配置项默认值（键 → OptionDef.Default）。
//
// 预设档需要它来区分「用户表过态的项」与「只是被默认值补齐的项」——
// 见 fillPresetOptions 上方的说明。插件不存在时返回 nil。
func declaredDefaults(pluginName string) map[string]any {
	for _, p := range plugin.All() {
		if p.Meta().Name != pluginName {
			continue
		}
		o, ok := p.(plugin.Optioner)
		if !ok {
			return nil
		}
		defs := o.Options()
		out := make(map[string]any, len(defs))
		for _, d := range defs {
			out[d.Key] = d.Default
		}
		return out
	}
	return nil
}

// fillPresetOptions 把预设档的值填进 options[pluginName]，返回**本次真正应用**的项。
//
// 判定「这一项要不要填」的标准有两条，缺一不可：
//  1. 键不存在 → 填（最简单的缺席情形）；
//  2. 键存在但值 == 插件声明的默认值 → 也填。
//
// 第 2 条是本任务真正的难点，也是踩过的坑：main 读配置走的是
// Store.LoadOrTemplate()，它内部会 MergeDefaults——**每个已声明选项都会被补齐**
// （值来自 OptionDef.Default）。所以 options 里永远"存在"这些键：
// 按键存在性判断的话，预设档一次都不会生效，而且失效方式是**静默的**
// （功能看起来实现了，跑起来毫无作用；首次运行生成的 hub.json 模板本身也写全了默认值）。
// 用「值 == 默认值」来判定"用户还没对这项表态"，正好把这两种来源都认成"未表态"。
//
// 反过来，只要值与默认值不同（用户写了 readHeaderTimeout: 30s，或显式写 0 表示关闭），
// 就一律认为是用户的显式意图，**预设档不碰**。
// 失效现场是运行期的连接行为，根因却在启动参数里——「我明明设了 30s 却被改成 5s」
// 是排查成本最高的一类问题，因此这条优先级绝不能倒过来：
// 预设档是**默认值集合**，不是策略强制。
//
// 已知边界（无法区分意图的客观限制）：用户显式写了与默认值完全相同的值
// （例如把 readHeaderTimeout 写成插件默认的 10s）会被当成"未表态"从而被改写。
// 需要绝对控制时的办法是 -timeouts=off（彻底不干预）或改成一个与默认值不同的数。
//
// defaults 为 nil（插件未注册/不声明选项）时退化为「只看键是否存在」。
func fillPresetOptions(options map[string]map[string]any, pluginName string, defaults, fill map[string]any) map[string]any {
	applied := make(map[string]any, len(fill))
	if len(fill) == 0 {
		return applied
	}
	if _, ok := options[pluginName]; !ok {
		options[pluginName] = map[string]any{}
	}
	for k, v := range fill {
		cur, exists := options[pluginName][k]
		if exists {
			// 插件没声明过这个键 → 无法判断默认值 → 保守起见视为用户配置，不覆盖。
			def, declared := defaults[k]
			if !declared || !sameOptionValue(cur, def) {
				continue
			}
		}
		options[pluginName][k] = v
		applied[k] = v
	}
	return applied
}

// sameOptionValue 比较两个配置值是否等价。
//
// 走 fmt.Sprint 而不是 ==：== 对 any 会在底层类型不同时直接 false，
// 而 JSON 数字解析成 float64、默认值可能是 int——同一个 100 会被判成"不同"，
// 于是把本该被预设档接管的默认项误判成用户配置（预设档静默失效）。
// 这里只用于默认值比较（类型可控），但保持宽松更稳妥。
func sameOptionValue(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// describeOptions 把配置项拼成稳定的日志文案（键名升序）。
//
// 排序是必要的：map 遍历顺序随机，不排序的话同一份配置每次启动的日志都不一样，
// 事后对照日志与 hub.json 时会被顺序差异干扰。
func describeOptions(opts map[string]any) string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, opts[k]))
	}
	return strings.Join(parts, " ")
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
