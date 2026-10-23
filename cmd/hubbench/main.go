// Command hubbench 是 LAN Agent Hub 的压测客户端（V8 ①）。
//
// 设计约束：
//   - **独立二进制**，不进主 EXE；零外部依赖（只用标准库）。
//   - 走**真实协议**：internal/protocol 的信封 + NDJSON 分帧，与生产客户端同一条
//     代码路径。绝不为压测开旁路——否则测出来的数字不代表真实客户端。
//   - 报告含**环境指纹**（GOOS/GOARCH/核数/连接数/速率），保证跨机器可比，
//     否则「这次比上次慢」可能只是换了台机器。
//   - 退出码：0 通过 / 1 未通过（有错误或注册表不一致）/ 2 参数错误。
//
// 用法示例：
//
//	hubbench -addr 127.0.0.1:8080 -http 127.0.0.1:8081 -c 100 -d 60s -r 10
//	hubbench -scenario room -c 50 -d 30s -r 5
//	hubbench -scenario flap -d 30s -flap-rate 20
//	hubbench -c 100 -d 60s -r 10 -out report.json -json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const usageText = `hubbench —— LAN Agent Hub 压测客户端（真实协议，零依赖）

用法：hubbench [选项]

连接与负载
  -addr string        Hub 的 TCP 地址（默认 127.0.0.1:8080）
  -http string        Hub 的 HTTP 地址，用于采集 /metrics（默认空=不采集）
  -c int              连接总数（默认 100；chat/room 场景一半发送一半回声）
  -d duration         压测时长，不含预热（默认 60s）
  -r string           每个发送方每秒消息数，支持 "10" 或 "10/s"（默认 10；0=不节流）
  -warmup duration    预热时长，期间的样本不计入报告（默认 2s）
  -timeout duration   单次读写超时（默认 5s）

场景
  -scenario string    chat（点对点往返，默认）/ room（房间扇出）/ flap（上下线抖动）
  -flap-rate int      flap 场景每秒上下线次数（默认 20）

输出
  -out string         把 JSON 报告写到该文件（默认不写）
  -json               只输出 JSON（默认输出人读表格）
  -compare string     与历史报告对比（基线 JSON 路径），表格后追加增减表

示例
  hubbench -addr 127.0.0.1:8080 -http 127.0.0.1:8081 -c 100 -d 60s -r 10
  hubbench -scenario flap -d 30s -flap-rate 20 -http 127.0.0.1:8081
  hubbench -c 100 -d 60s -r 10 -out new.json -compare baseline/v8a-chat.json
`

// Config 是一次压测的全部参数。
type Config struct {
	Addr        string
	HTTPAddr    string
	Connections int
	Duration    time.Duration
	RatePerSec  float64
	Scenario    string
	Out         string
	Compare     string
	Warmup      time.Duration
	Timeout     time.Duration
	FlapRate    int
	JSONOnly    bool

	// 运行时状态（不来自命令行）
	measureStart time.Time
	baseline     *HubStats
}

// usageError 表示「用户参数写错了」，与「压测本身失败」区分开（退出码 2 vs 1）。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain 是可测入口：返回退出码，所有输出走传入的 writer。
func realMain(args []string, out, errOut io.Writer) int {
	cfg, err := parseArgs(args, out)
	if err != nil {
		var ue *usageError
		if errors.As(err, &ue) {
			_, _ = fmt.Fprintln(errOut, "参数错误："+ue.Error())
			_, _ = fmt.Fprint(errOut, usageText)
			return 2
		}
		_, _ = fmt.Fprintln(errOut, "错误："+err.Error())
		return 1
	}
	rep, err := Execute(context.Background(), cfg)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "压测失败："+err.Error())
		return 1
	}
	if cfg.JSONOnly {
		b, _ := json.MarshalIndent(rep, "", "  ")
		_, _ = fmt.Fprintln(out, string(b))
	} else {
		renderTable(out, rep)
		if cfg.Compare != "" {
			base, err := LoadReport(cfg.Compare)
			if err != nil {
				// 对比失败不该让整次压测判死：数字已经测出来了，用户要的是参考，不是阻断。
				_, _ = fmt.Fprintf(errOut, "对比基线读取失败（忽略对比）：%v\n", err)
			} else {
				renderCompare(out, base, rep)
			}
		}
	}
	if cfg.Out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintln(errOut, "报告序列化失败："+err.Error())
			return 1
		}
		if err := os.WriteFile(cfg.Out, append(b, '\n'), 0o644); err != nil {
			_, _ = fmt.Fprintln(errOut, "报告写入失败："+err.Error())
			return 1
		}
		if !cfg.JSONOnly {
			_, _ = fmt.Fprintf(out, "报告已写入 %s\n", cfg.Out)
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// parseArgs 解析命令行。返回值里的 error 若是 *usageError 则应打印用法。
func parseArgs(args []string, out io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("hubbench", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 用法由 realMain 统一打印，避免重复
	cfg := &Config{}
	var rateStr string
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1:8080", "Hub TCP 地址")
	fs.StringVar(&cfg.HTTPAddr, "http", "", "Hub HTTP 地址（采集 /metrics）")
	fs.IntVar(&cfg.Connections, "c", 100, "连接总数")
	fs.DurationVar(&cfg.Duration, "d", 60*time.Second, "压测时长（不含预热）")
	fs.StringVar(&rateStr, "r", "10", "每个发送方每秒消息数（0=不节流）")
	fs.StringVar(&cfg.Scenario, "scenario", "chat", "场景：chat|room|flap")
	fs.StringVar(&cfg.Out, "out", "", "JSON 报告输出路径")
	fs.StringVar(&cfg.Compare, "compare", "", "与历史报告对比（基线 JSON 路径）")
	fs.DurationVar(&cfg.Warmup, "warmup", 2*time.Second, "预热时长（样本不计入报告）")
	fs.DurationVar(&cfg.Timeout, "timeout", 5*time.Second, "单次读写超时")
	fs.IntVar(&cfg.FlapRate, "flap-rate", 20, "flap 场景每秒上下线次数")
	fs.BoolVar(&cfg.JSONOnly, "json", false, "只输出 JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(out, usageText)
			return nil, &usageError{msg: "已显示用法"}
		}
		return nil, usageErrorf("%v", err)
	}
	if fs.NArg() > 0 {
		return nil, usageErrorf("不认识的参数：%s", strings.Join(fs.Args(), " "))
	}

	rate, err := parseRate(rateStr)
	if err != nil {
		return nil, err
	}
	cfg.RatePerSec = rate

	if cfg.Addr == "" {
		return nil, usageErrorf("-addr 不能为空")
	}
	if cfg.Connections <= 0 {
		return nil, usageErrorf("-c 必须大于 0，当前 %d", cfg.Connections)
	}
	if cfg.Duration <= 0 {
		return nil, usageErrorf("-d 必须大于 0，当前 %s", cfg.Duration)
	}
	if cfg.Warmup < 0 {
		return nil, usageErrorf("-warmup 不能为负")
	}
	if cfg.Timeout <= 0 {
		return nil, usageErrorf("-timeout 必须大于 0")
	}
	switch cfg.Scenario {
	case "chat", "room", "flap":
	default:
		return nil, usageErrorf("未知场景 %q（可选 chat / room / flap）", cfg.Scenario)
	}
	if cfg.Scenario == "flap" && cfg.FlapRate <= 0 {
		return nil, usageErrorf("-flap-rate 必须大于 0，当前 %d", cfg.FlapRate)
	}
	// -compare 提前校验：跑完 60s 才发现基线文件不存在，体验很差。
	if cfg.Compare != "" {
		if _, err := os.Stat(cfg.Compare); err != nil {
			return nil, usageErrorf("-compare 指向的基线报告不可读：%v", err)
		}
	}
	return cfg, nil
}

// parseRate 解析速率：接受 "10"、"10/s"、"10/s/conn"。
// 后一种是文档里常见的写法（每秒每连接），本质与 "10/s" 同义，一并接受。
func parseRate(s string) (float64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	t = strings.TrimSuffix(t, "/conn")
	t = strings.TrimSuffix(t, "/s")
	t = strings.TrimSuffix(t, "per sec")
	t = strings.TrimSpace(t)
	if t == "" {
		return 0, usageErrorf("-r 不能为空")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, usageErrorf("-r 不是合法数字：%q", s)
	}
	if v < 0 {
		return 0, usageErrorf("-r 不能为负：%q", s)
	}
	return v, nil
}

// Execute 跑一次完整压测并返回报告（预热 + 计时 + 采集 Hub 指标）。
func Execute(ctx context.Context, cfg *Config) (*Report, error) {
	cfg.baseline = cfg.scrape() // 压测前基线，用于算「期间新增了多少」
	total := cfg.Warmup + cfg.Duration
	runCtx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	var (
		rep *Report
		err error
	)
	switch cfg.Scenario {
	case "chat":
		rep, err = runChat(runCtx, cfg)
	case "room":
		rep, err = runRoom(runCtx, cfg)
	case "flap":
		rep, err = runFlap(runCtx, cfg)
	default:
		return nil, usageErrorf("未知场景 %q", cfg.Scenario)
	}
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// connect 建连并注册，同时维护连接层计数（连接成败始终统计，与预热无关）。
func (c *Config) connect(col *collector, id string) (*benchClient, error) {
	col.connAttempted.Add(1)
	cli, err := dialRegister(c.Addr, id, c.Timeout)
	if err != nil {
		if strings.Contains(err.Error(), "register") || strings.Contains(err.Error(), "registered") {
			col.RegisterFailed()
		} else {
			col.connFailed.Add(1)
		}
		col.connErr(err)
		return nil, err
	}
	col.connEstablished.Add(1)
	return cli, nil
}

// warmupAndRecord 阻塞到预热结束，然后打开采样开关并记录计时起点。
//
// 为什么预热要单独一段：建连、TCP 慢启动、服务端注册表与 GC 首次分配都会让前
// 几百毫秒明显偏慢；把这段算进分位数，p99 反映的是「启动成本」而不是稳态性能。
func (c *Config) warmupAndRecord(col *collector) {
	if c.Warmup > 0 {
		time.Sleep(c.Warmup)
	}
	c.measureStart = time.Now()
	col.recording.Store(true)
}

// report 把采集到的数据组装成报告（含 Hub 侧指标）。
func (c *Config) report(scenario string, col *collector, hub *HubStats, notes []string) *Report {
	if hub == nil {
		hub = c.scrape()
	}
	start := c.measureStart
	if start.IsZero() {
		start = time.Now()
	}
	dur := time.Since(start)
	senders, echoers := 0, 0
	if scenario != "flap" {
		senders, echoers = splitRoles(c.Connections)
	}
	rep := &Report{
		Tool:       "hubbench",
		Version:    benchVersion,
		Scenario:   scenario,
		StartedAt:  start,
		DurationMs: dur.Milliseconds(),
		Env:        fingerprint(c.Addr, c.HTTPAddr, senders, echoers, formatRate(c.RatePerSec), c.Warmup),
		Conn: ConnectionStats{
			Attempted:      col.connAttempted.Load(),
			Established:    col.connEstablished.Load(),
			Failed:         col.connFailed.Load(),
			RegisterFailed: col.registerFailed.Load(),
		},
		Latency: col.lat.Snapshot(true),
		Errors:  col.errSnapshot(),
		Hub:     hub,
		Slowest: col.slow.snapshot(),
		Notes:   notes,
	}
	if scenario == "flap" {
		rep.Env.FlapPerSec = c.FlapRate
	}
	sent, delivered := col.sent.Load(), col.delivered.Load()
	rep.Traffic = TrafficStats{Sent: sent, Delivered: delivered}
	if secs := dur.Seconds(); secs > 0 {
		rep.Traffic.SendPerSec = float64(sent) / secs
		rep.Traffic.DeliverPerSec = float64(delivered) / secs
	}
	totalErr := col.errors.Load()
	if sent+totalErr > 0 {
		rep.Traffic.ErrorRate = float64(totalErr) / float64(sent+totalErr)
	}
	errs := rep.Errors
	errs["_total"] = totalErr
	// 判定：无残留会话（若采到）、错误率 < 1%、且有连接建立成功。
	rep.OK = col.connEstablished.Load() > 0 && rep.Traffic.ErrorRate < 0.01
	if hub != nil && hub.Scraped && scenario == "flap" && hub.AgentsOnline > 0 {
		rep.OK = false
	}
	return rep
}

// scrape 抓一次 Hub 指标（-http 未配置或抓取失败时返回带 Error 的结构，不阻塞压测）。
func (c *Config) scrape() *HubStats {
	s := &HubStats{}
	if c.HTTPAddr == "" {
		s.Error = "未指定 -http"
		return s
	}
	vals, err := scrapeMetrics(c.HTTPAddr, c.Timeout)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Scraped = true
	s.AgentsOnline = vals["hub_agents_online"]
	s.Rooms = vals["hub_rooms"]
	s.MessagesTotal = vals["hub_messages_total"]
	if c.baseline != nil && c.baseline.Scraped {
		s.AgentsOnlineBefore = c.baseline.AgentsOnline
		s.MessagesBefore = c.baseline.MessagesTotal
	}
	return s
}

// formatRate 把速率格式化成环境指纹里的字符串（整数不带小数点，便于对比）。
func formatRate(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
