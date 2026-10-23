// Package main 实现 hubctl —— LAN Agent Hub 的命令行客户端。
//
// 定位（V7 ②）：hubctl 是**独立二进制**，不属于服务端插件体系，
// 只通过 REST + SSE 契约与 Hub 通信——「如果只有浏览器能用这些 API，说明 API 设计错了」。
//
// 约束：
//   - 零外部依赖，只用标准库（flag/net/http/encoding/json/...）；
//   - 退出码契约：0 成功 / 1 用法错误 / 2 网络或服务端错误，CI 可直接依赖；
//   - 输出目标、是否 TTY、时间源均可注入，故单测能断言字节级结果。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// 退出码契约（CI 可依赖）。
const (
	exitOK    = 0
	exitUsage = 1
	exitNet   = 2
)

// envHubAddr 是 Hub 地址的环境变量名（免除每次敲 -addr）。
const envHubAddr = "HUB_ADDR"

// version 由构建时 -ldflags "-X main.version=..." 注入。
var version = "dev"

// command 描述一个子命令。fn 为 nil 时由分发逻辑特殊处理（help）。
type command struct {
	name  string
	usage string
	brief string
	fn    func(a *app, args []string) error
}

// commands 是唯一的子命令事实源：帮助文本与补全脚本的一致性都由它派生。
//
// 放在 init 里赋值而非直接初始化：cmdFmt → runCommand → commands 构成运行期环，
// 用 var 直接初始化会被编译器判为初始化循环。
var commands []command

func init() {
	commands = []command{
		{"status", "hubctl status", "服务状态总览（GET /api/status）", (*app).cmdStatus},
		{"agents", "hubctl agents [-w]", "Agent 表格；-w 监听模式（GET /api/agents + SSE /api/stream）", (*app).cmdAgents},
		{"rooms", "hubctl rooms", "房间列表（GET /api/rooms）", (*app).cmdRooms},
		{"send", `hubctl send --to A --text "hi"`, "点对点发送（POST /api/send）", (*app).cmdSend},
		{"say", `hubctl say --room R --text "hi"`, "房间发送（POST /api/room）", (*app).cmdSay},
		{"bcast", `hubctl bcast --text "hi"`, "广播（POST /api/broadcast）", (*app).cmdBcast},
		{"plugins", "hubctl plugins [start|stop NAME]", "插件列表与启停（GET /api/plugins、POST /api/plugins/toggle）", (*app).cmdPlugins},
		{"logs", "hubctl logs [-f] [-n N] [-level L]", "日志查询与跟随（GET /api/logs、SSE /api/logs/stream）", (*app).cmdLogs},
		{"messages", "hubctl messages [--since SEQ]", "历史消息（GET /api/messages）", (*app).cmdMessages},
		{"watch", "hubctl watch [-level L]", "一键跟随事件流（等价于 agents -w + logs -f 合并）", (*app).cmdWatch},
		{"fmt", "hubctl fmt [json|table] [子命令]", "查看或指定输出格式（等价于全局 --fmt）", (*app).cmdFmt},
		{"completion", "hubctl completion bash|zsh|powershell", "生成 shell 补全脚本", (*app).cmdCompletion},
		{"help", "hubctl help", "显示帮助", nil},
	}
}

// commandNames 返回全部子命令名，供补全一致性检查使用。
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.name)
	}
	return names
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// globals 是必须出现在子命令之前的全局参数。
type globals struct {
	addr    string
	format  string
	color   string
	help    bool
	version bool
}

// parseGlobalFlags 手工解析前置的全局参数，返回剩余参数。
//
// 不用 flag 包先解析一遍：flag 遇到第一个非 flag 参数就停，而子命令后面还有
// 自己的 --to/--text 等参数，两段必须用各自的 FlagSet。
func parseGlobalFlags(args []string) (globals, []string, error) {
	g := globals{color: "auto"}
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		name := strings.TrimLeft(arg, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		switch name {
		case "h", "help":
			g.help = true
			i++
		case "version", "v":
			g.version = true
			i++
		case "addr":
			v, next, err := takeValue(args, i, "-addr")
			if err != nil {
				return g, nil, err
			}
			g.addr, i = v, next
		case "fmt":
			v, next, err := takeValue(args, i, "-fmt")
			if err != nil {
				return g, nil, err
			}
			g.format, i = v, next
		case "color":
			v, next, err := takeValue(args, i, "-color")
			if err != nil {
				return g, nil, err
			}
			g.color, i = v, next
		default:
			return g, nil, fmt.Errorf("未知的全局参数 %q（全局参数必须写在子命令之前）", arg)
		}
	}
	return g, args[i:], nil
}

// takeValue 取一个全局参数的取值，支持 "-addr X" 与 "-addr=X" 两种写法。
func takeValue(args []string, i int, flag string) (string, int, error) {
	arg := args[i]
	if eq := strings.IndexByte(arg, '='); eq >= 0 {
		return arg[eq+1:], i + 1, nil
	}
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("%s 缺少取值", flag)
	}
	return args[i+1], i + 2, nil
}

// run 执行一次 CLI 调用并返回退出码——真正的 main 只负责 os.Exit，
// 这样测试可以直接驱动它并断言退出码与 stdout/stderr。
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	g, rest, err := parseGlobalFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "hubctl: 用法错误: %v\n\n", err)
		printUsage(stderr)
		return exitUsage
	}
	if g.version {
		fmt.Fprintf(stdout, "hubctl %s\n", version)
		return exitOK
	}
	if g.help {
		printUsage(stdout)
		return exitOK
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "hubctl: 缺少子命令")
		printUsage(stderr)
		return exitUsage
	}
	if g.format != "" && g.format != formatJSON && g.format != formatTable {
		fmt.Fprintf(stderr, "hubctl: 用法错误: -fmt 只能是 json 或 table，收到 %q\n", g.format)
		return exitUsage
	}

	addr := g.addr
	if strings.TrimSpace(addr) == "" {
		addr = os.Getenv(envHubAddr)
	}
	cl := newClient(addr)

	term := isTerminal(stdout)
	out := newRenderer(stdout, resolveFormat(g.format, term), resolveColor(g.color, term))
	a := &app{
		out:  out,
		errw: stderr,
		cl:   cl,
		now:  time.Now,
		ctx:  ctx,
	}

	if err := runCommand(a, rest[0], rest[1:]); err != nil {
		return reportError(stderr, err, cl.baseURL)
	}
	return exitOK
}

// reportError 把错误翻译成人读信息 + 退出码：用法错误 1，网络/服务端错误 2。
func reportError(stderr io.Writer, err error, baseURL string) int {
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(stderr, "hubctl: 用法错误: %s\n", ue.msg)
		return exitUsage
	}
	fmt.Fprintf(stderr, "hubctl: %v\n", err)
	// 连不上服务端是最常见的一类失败，给出可直接复制的启动命令，
	// 而不是让使用者对着 dial 错误猜。
	var ce *connError
	if errors.As(err, &ce) {
		fmt.Fprintf(stderr, "  ↳ 连不上 Hub（%s），请确认服务端已启动，例如：\n", baseURL)
		fmt.Fprintf(stderr, "      go run ./cmd/hub            # 默认 HTTP 端口 8081\n")
		fmt.Fprintf(stderr, "      ./hub -http :8081\n")
		fmt.Fprintf(stderr, "  也可用 -addr 或 HUB_ADDR 环境变量指向其它地址。\n")
	}
	return exitNet
}

// runCommand 分发到子命令；未知子命令是用法错误（退出码 1）。
func runCommand(a *app, name string, args []string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, c := range commands {
		if c.name != name {
			continue
		}
		if c.fn == nil { // help
			printUsage(a.out.w)
			return nil
		}
		return c.fn(a, args)
	}
	return usageErr("未知子命令 %q（可用：%s）", name, strings.Join(commandNames(), ", "))
}

// printUsage 输出帮助文本。
func printUsage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

// usageText 由 commandListText 拼装（含变量，故不能是 const）。
var usageText = `hubctl —— LAN Agent Hub 命令行客户端（REST + SSE 契约，零外部依赖）

用法：
  hubctl [全局参数] <子命令> [参数...]

全局参数（必须写在子命令之前）：
  -addr URL       Hub REST 地址（默认 ` + defaultAddr + `；也可用 ` + envHubAddr + ` 环境变量）
  -fmt FMT        输出格式：json | table（默认 table；非 TTY/管道时自动 json）
  -color MODE     彩色输出：auto | always | never（默认 auto，非 TTY 自动关闭）
  -h, --help      显示本帮助
  --version       显示版本

子命令：
` + commandListText + `
示例：
  hubctl status                      # 服务状态
  hubctl agents -w                   # 实时监听 Agent 上下线
  hubctl send --to A --text "hi"     # 点对点
  hubctl messages --since 100        # 序号 100 之后的历史消息
  hubctl --fmt json agents | jq .    # 管道场景自动 JSON
  source <(hubctl completion bash)   # 启用 bash 补全

退出码：
  0 成功   1 用法错误   2 网络或服务端错误
`

// commandListText 由 commands 派生，避免帮助文本与实际命令表漂移。
var commandListText = buildCommandList()

func buildCommandList() string {
	var sb strings.Builder
	for _, c := range commands {
		fmt.Fprintf(&sb, "  %-36s %s\n", c.usage, c.brief)
	}
	return sb.String()
}
