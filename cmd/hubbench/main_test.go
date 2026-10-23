package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"

	_ "lan-agent-hub/cmd/hub/plugins" // 触发全部内置插件注册（与生产装配同一份清单）
)

/* ================= 测试脚手架 ================= */

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("释放临时监听失败: %v", err)
	}
	return port
}

// startTestHub 起一个真实 Hub（真实 TCP/HTTP 端口），供压测端到端验证。
//
// 关闭 autobrowser/discovery：前者会开浏览器、后者会往局域网广播 UDP，
// 测试里两个都是副作用。其余插件全开，保证 tcpgateway/httpgateway/metrics 可用。
func startTestHub(t *testing.T) (tcpAddr, httpAddr string) {
	t.Helper()
	tcpPort, httpPort := freePort(t), freePort(t)
	tcpAddr = fmt.Sprintf("127.0.0.1:%d", tcpPort)
	httpAddr = fmt.Sprintf("127.0.0.1:%d", httpPort)

	hub := core.NewHub()
	hub.TCPPort, hub.HTTPPort = tcpPort, httpPort

	off := map[string]bool{"autobrowser": true, "discovery": true}
	enabled := map[string]bool{}
	for _, p := range plugin.All() {
		name := p.Meta().Name
		enabled[name] = !off[name]
	}
	options := map[string]map[string]any{
		"tcpgateway":  {"addr": tcpAddr},
		"httpgateway": {"httpAddr": httpAddr},
		// store 默认写 ./store、file 默认写 ./data/files，都是相对 cwd 的。
		// 测试 cwd 是包目录，不重定向就会在源码树里拉出这些运行态目录——
		// 测试不该污染工作区。
		"store": {"path": t.TempDir()},
		"file":  {"dataDir": t.TempDir()},
	}
	mgr := plugin.NewManager()
	mgr.ApplyConfig(enabled, options)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Cleanup(mgr.StopAll)
	return tcpAddr, httpAddr
}

func testConfig(tcpAddr, httpAddr string) *Config {
	return &Config{
		Addr:        tcpAddr,
		HTTPAddr:    httpAddr,
		Connections: 4,
		Duration:    600 * time.Millisecond,
		RatePerSec:  50,
		Scenario:    "chat",
		Warmup:      100 * time.Millisecond,
		Timeout:     3 * time.Second,
		FlapRate:    20,
	}
}

/* ================= 参数解析 ================= */

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"10", 10},
		{"10/s", 10},
		{"10/s/conn", 10},
		{"2.5", 2.5},
		{"0", 0},
		{"  20  ", 20},
	}
	for _, c := range cases {
		got, err := parseRate(c.in)
		if err != nil {
			t.Errorf("parseRate(%q) 返回错误: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseRate(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"abc", "", "-1", "1/2"} {
		if _, err := parseRate(bad); err == nil {
			t.Errorf("parseRate(%q) 应当报错，但通过了", bad)
		}
	}
}

func TestParseArgsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"连接数为 0", []string{"-c", "0"}},
		{"时长为 0", []string{"-d", "0s"}},
		{"未知场景", []string{"-scenario", "bogus"}},
		{"速率为负", []string{"-r", "-5"}},
		{"空地址", []string{"-addr", ""}},
		{"多余参数", []string{"extra"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseArgs(c.args, devNull{})
			if err == nil {
				t.Fatalf("期望参数错误，实际通过")
			}
			var ue *usageError
			if !asUsage(err, &ue) {
				t.Fatalf("期望 *usageError，实际 %T: %v", err, err)
			}
		})
	}
}

func TestParseArgsAcceptsDefaults(t *testing.T) {
	cfg, err := parseArgs(nil, devNull{})
	if err != nil {
		t.Fatalf("默认参数应当合法: %v", err)
	}
	if cfg.Scenario != "chat" || cfg.Connections != 100 || cfg.RatePerSec != 10 {
		t.Fatalf("默认值不符: %+v", cfg)
	}
}

func TestRealMainUsageExitCode(t *testing.T) {
	var out, errOut strings.Builder
	if code := realMain([]string{"-c", "0"}, &out, &errOut); code != 2 {
		t.Fatalf("参数错误应返回退出码 2，实际 %d", code)
	}
	if !strings.Contains(errOut.String(), "参数错误") {
		t.Fatalf("stderr 应说明参数错误，实际 %q", errOut.String())
	}
}

func TestRealMainUnreachableHubExitCode(t *testing.T) {
	// 拿一个已关闭的端口：连接必然被拒，realMain 应返回 1（压测失败）而不是 2（参数错误）。
	port := freePort(t)
	var out, errOut strings.Builder
	code := realMain([]string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-c", "2", "-d", "100ms", "-warmup", "0s", "-timeout", "300ms",
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("连不上 Hub 应返回退出码 1，实际 %d（stderr=%s）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "压测失败") {
		t.Fatalf("stderr 应包含失败原因，实际 %q", errOut.String())
	}
}

/* ================= 直方图 ================= */

func TestHistogramQuantiles(t *testing.T) {
	h := newHistogram()
	for i := 1; i <= 100; i++ {
		h.Observe(time.Duration(i) * time.Millisecond)
	}
	if h.Total() != 100 {
		t.Fatalf("样本数 = %d，期望 100", h.Total())
	}
	s := h.Snapshot(false)
	if s.Count != 100 {
		t.Fatalf("快照 count = %d", s.Count)
	}
	// 桶内插值：1..100ms 均匀分布时各分位数应贴近真值，而不是被顶到桶上界。
	// 这正是「分桶但可插值」的意义——否则 p50 只能读到档位刻度。
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"p50", s.P50Ms, 50},
		{"p95", s.P95Ms, 95},
		{"p99", s.P99Ms, 99},
	} {
		if math.Abs(c.got-c.want) > c.want*0.15 {
			t.Errorf("%s = %.2fms，期望 %.0fms（允许 ±15%%）", c.name, c.got, c.want)
		}
	}
	// 单调性 + 不得超过实测 max：粗桶时代会出现 p50=10ms 而 max=9ms，一眼假。
	if !(s.MinMs <= s.P50Ms && s.P50Ms <= s.P95Ms && s.P95Ms <= s.P99Ms && s.P99Ms <= s.MaxMs) {
		t.Errorf("分位数必须单调且不超过 max，实际 min=%.3f p50=%.3f p95=%.3f p99=%.3f max=%.3f",
			s.MinMs, s.P50Ms, s.P95Ms, s.P99Ms, s.MaxMs)
	}
	if s.MaxMs != 100 {
		t.Errorf("max = %vms，期望 100", s.MaxMs)
	}
	if s.MinMs != 1 {
		t.Errorf("min = %vms，期望 1", s.MinMs)
	}
	if s.MeanMs < 49 || s.MeanMs > 52 {
		t.Errorf("mean = %vms，期望约 50.5", s.MeanMs)
	}
	// 分桶明细只在 includeBuckets=true 时输出（报告要它，人读表格不要）。
	if len(s.HistBucket) != 0 {
		t.Errorf("Snapshot(false) 不应带分桶明细，实际 %d 个", len(s.HistBucket))
	}
	if full := h.Snapshot(true); len(full.HistBucket) != len(histBounds) {
		t.Errorf("Snapshot(true) 分桶数 = %d，期望 %d", len(full.HistBucket), len(histBounds))
	}
}

func TestHistogramOverflowAndEdgeCases(t *testing.T) {
	h := newHistogram()
	if got := h.Quantile(0.5); got != 0 {
		t.Errorf("空直方图分位数应为 0，实际 %v", got)
	}
	h.Observe(30 * time.Second)      // 溢出桶
	h.Observe(-5 * time.Millisecond) // 负数按 0 处理，不污染分位数
	if h.Total() != 2 {
		t.Fatalf("样本数 = %d", h.Total())
	}
	if got := h.Quantile(1.0); got != 30*time.Second {
		t.Errorf("q=1 应返回 max 30s，实际 %v", got)
	}
	if got := h.Quantile(0.99); got != 30*time.Second {
		t.Errorf("溢出桶内分布未知，p99 应返回实测 max 30s，实际 %v", got)
	}
	if h.Quantile(0) != 0 {
		t.Errorf("q=0 应返回 min（负数被夹到 0），实际 %v", h.Quantile(0))
	}
}

func TestHistogramMerge(t *testing.T) {
	a, b := newHistogram(), newHistogram()
	a.Observe(2 * time.Millisecond)
	b.Observe(3 * time.Millisecond)
	b.Observe(40 * time.Millisecond)
	a.Merge(b)
	if a.Total() != 3 {
		t.Fatalf("合并后样本数 = %d，期望 3", a.Total())
	}
	if a.Snapshot(false).MaxMs != 40 {
		t.Errorf("合并后 max = %v，期望 40", a.Snapshot(false).MaxMs)
	}
	// 合并空直方图不应改变任何东西（避免 nil/零值污染）
	a.Merge(newHistogram())
	if a.Total() != 3 {
		t.Errorf("并入空直方图后样本数被改成 %d", a.Total())
	}
}

func TestSlowBufKeepsTopN(t *testing.T) {
	s := newSlowBuf(3)
	for i := 1; i <= 10; i++ {
		s.add("c", fmt.Sprint(i), time.Duration(i)*time.Millisecond)
	}
	rows := s.snapshot()
	if len(rows) != 3 {
		t.Fatalf("保留条数 = %d，期望 3", len(rows))
	}
	if rows[0].Ms != 10 || rows[1].Ms != 9 || rows[2].Ms != 8 {
		t.Fatalf("应保留最慢 3 条且降序，实际 %+v", rows)
	}
}

/* ================= 协议辅助 ================= */

func TestSplitRoles(t *testing.T) {
	cases := []struct{ total, senders, others int }{
		{1, 1, 0},
		{2, 1, 1},
		{4, 2, 2},
		{5, 2, 3},
		{100, 50, 50},
	}
	for _, c := range cases {
		s, o := splitRoles(c.total)
		if s != c.senders || o != c.others {
			t.Errorf("splitRoles(%d) = (%d,%d)，期望 (%d,%d)", c.total, s, o, c.senders, c.others)
		}
		if s+o != c.total {
			t.Errorf("splitRoles(%d) 两侧之和 %d != 总数", c.total, s+o)
		}
	}
}

func TestPendingMapSweep(t *testing.T) {
	p := newPendingMap()
	p.put("old")
	p.put("new")
	// 直接把 old 的发出时刻改到过去，模拟超时
	p.mu.Lock()
	p.m["old"] = time.Now().Add(-10 * time.Second)
	p.mu.Unlock()

	if n := p.sweep(time.Now(), 5*time.Second); n != 1 {
		t.Fatalf("清扫数量 = %d，期望 1", n)
	}
	if _, ok := p.take("new"); !ok {
		t.Fatal("未超时的等待项被误删")
	}
	if _, ok := p.take("new"); ok {
		t.Fatal("take 应当删除已取出的等待项（否则内存无界增长）")
	}
}

func TestIsRegisteredAck(t *testing.T) {
	actionAck := protocol.NewEnvelope(protocol.TypeSystem, "hub", "a1", "", map[string]any{
		"action": protocol.ActionRegistered, "text": "registered as a1",
	})
	if !isRegisteredAck(actionAck) {
		t.Error("带 action=registered 的回执应被识别")
	}
	legacyAck := protocol.NewEnvelope(protocol.TypeSystem, "hub", "a1", "", map[string]string{
		"text": "registered as a1",
	})
	if !isRegisteredAck(legacyAck) {
		t.Error("只有旧 text 的回执也应被识别（过渡期兼容）")
	}
	notAck := protocol.NewEnvelope(protocol.TypeSystem, "hub", "a1", "", map[string]string{"text": "hello"})
	if isRegisteredAck(notAck) {
		t.Error("非注册回执被误判为注册成功")
	}
	if isRegisteredAck(protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", nil)) {
		t.Error("chat 信封被误判为注册回执")
	}
}

func TestPayloadOf(t *testing.T) {
	env := newBenchChat("a", "b", "n1", false)
	p, ok := payloadOf(env)
	if !ok || p.Nonce != "n1" || p.Echo || !p.Bench {
		t.Fatalf("压测负载解析错误: ok=%v payload=%+v", ok, p)
	}
	if _, ok := payloadOf(protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"text": "hi"})); ok {
		t.Error("普通消息不应被识别为压测流量")
	}
	if _, ok := payloadOf(protocol.Envelope{}); ok {
		t.Error("空 content 不应被识别为压测流量")
	}
}

/* ================= 端到端场景 ================= */

func TestChatScenarioEndToEnd(t *testing.T) {
	tcpAddr, httpAddr := startTestHub(t)
	cfg := testConfig(tcpAddr, httpAddr)
	rep, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("压测失败: %v", err)
	}
	if rep.Conn.Established != 4 {
		t.Fatalf("建立连接数 = %d，期望 4（失败 %d，注册失败 %d）",
			rep.Conn.Established, rep.Conn.Failed, rep.Conn.RegisterFailed)
	}
	if rep.Traffic.Sent == 0 {
		t.Fatal("发送数为 0：压测根本没跑起来")
	}
	if rep.Traffic.Delivered == 0 {
		t.Fatal("送达数为 0：回程没回来，往返链路不通")
	}
	if rep.Latency.Count == 0 {
		t.Fatal("延迟样本为 0：无法给出 p99")
	}
	if rep.Latency.P99Ms <= 0 || rep.Latency.P99Ms > 3000 {
		t.Fatalf("p99 = %vms，超出合理范围", rep.Latency.P99Ms)
	}
	if !rep.OK {
		t.Fatalf("报告判定为 FAIL（错误率 %.4f，错误 %v）", rep.Traffic.ErrorRate, rep.Errors)
	}
	if rep.Scenario != "chat" || rep.Tool != "hubbench" || rep.Version == "" {
		t.Fatalf("报告元信息不完整: %+v", rep)
	}
	if rep.Env.NumCPU == 0 || rep.Env.GOOS == "" || rep.Env.Connections != 4 {
		t.Fatalf("环境指纹不完整: %+v", rep.Env)
	}
}

func TestRoomScenarioFanout(t *testing.T) {
	tcpAddr, httpAddr := startTestHub(t)
	cfg := testConfig(tcpAddr, httpAddr)
	cfg.Scenario = "room"
	rep, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("压测失败: %v", err)
	}
	if rep.Traffic.Sent == 0 {
		t.Fatal("房间场景发送数为 0")
	}
	// 扇出：一条房间消息会被多个成员各收一次，送达数必然大于发送数。
	if rep.Traffic.Delivered <= rep.Traffic.Sent {
		t.Fatalf("送达 %d 不大于发送 %d，扇出没有发生", rep.Traffic.Delivered, rep.Traffic.Sent)
	}
	if rep.Latency.Count == 0 {
		t.Fatal("房间场景也应有往返延迟样本（由回声成员回程）")
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "房间") {
		t.Fatalf("房间场景应记录房间信息，实际 %v", rep.Notes)
	}
	if !rep.OK {
		t.Fatalf("报告判定为 FAIL（错误率 %.4f）", rep.Traffic.ErrorRate)
	}
}

func TestFlapScenarioRegistryConsistent(t *testing.T) {
	tcpAddr, httpAddr := startTestHub(t)
	cfg := testConfig(tcpAddr, httpAddr)
	cfg.Scenario = "flap"
	cfg.Duration = 800 * time.Millisecond
	cfg.Warmup = 0
	cfg.FlapRate = 20
	rep, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("压测失败: %v", err)
	}
	if rep.Conn.Attempted == 0 || rep.Conn.Established == 0 {
		t.Fatalf("抖动期间没有任何成功连接: %+v", rep.Conn)
	}
	if rep.Conn.Failed != 0 {
		t.Fatalf("抖动期间出现 %d 次连接失败（应为 0）", rep.Conn.Failed)
	}
	if rep.Hub == nil || !rep.Hub.Scraped {
		t.Fatalf("应采集到 Hub 指标，实际 %+v", rep.Hub)
	}
	if rep.Hub.AgentsOnline != 0 {
		t.Fatalf("抖动结束后 Hub 在线 Agent = %v，期望 0（注册表未收敛/会话泄漏）", rep.Hub.AgentsOnline)
	}
	if !rep.OK {
		t.Fatalf("抖动报告应判定为 PASS，实际 FAIL（错误 %v）", rep.Errors)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	tcpAddr, httpAddr := startTestHub(t)
	cfg := testConfig(tcpAddr, httpAddr)
	cfg.Duration = 300 * time.Millisecond
	rep, err := Execute(context.Background(), cfg)
	if err != nil {
		t.Fatalf("压测失败: %v", err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("报告无法序列化: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("报告无法反序列化: %v", err)
	}
	for _, k := range []string{
		"tool", "version", "scenario", "startedAt", "durationMs", "env",
		"connections", "traffic", "latency", "errors", "hub", "ok",
	} {
		if _, ok := back[k]; !ok {
			t.Errorf("报告 JSON 缺少字段 %q", k)
		}
	}
	lat, _ := back["latency"].(map[string]any)
	for _, k := range []string{"count", "p50Ms", "p95Ms", "p99Ms", "maxMs", "histogram"} {
		if _, ok := lat[k]; !ok {
			t.Errorf("latency 缺少字段 %q", k)
		}
	}
}

/* ================= 基线对比（-compare） ================= */

func TestParseArgsCompareMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	_, err := parseArgs([]string{"-compare", missing}, devNull{})
	if err == nil {
		t.Fatalf("基线文件不存在应报错，实际通过")
	}
	var ue *usageError
	if !asUsage(err, &ue) {
		t.Fatalf("基线文件不存在应算参数错误（退出码 2），实际 %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "-compare") {
		t.Fatalf("错误信息应指向 -compare，实际 %q", err.Error())
	}
}

func TestParseArgsCompareAcceptsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base.json")
	if err := os.WriteFile(path, []byte(`{"tool":"hubbench","scenario":"chat"}`), 0o644); err != nil {
		t.Fatalf("写基线文件失败: %v", err)
	}
	cfg, err := parseArgs([]string{"-compare", path}, devNull{})
	if err != nil {
		t.Fatalf("基线文件存在时不应报错: %v", err)
	}
	if cfg.Compare != path {
		t.Fatalf("Compare 未记录，实际 %q", cfg.Compare)
	}
}

// mkReport 造一份可比较的报告：只关心参与对比的字段。
func mkReport(p50, p95, p99, max, sendRate, deliverRate, errRate float64) *Report {
	return &Report{
		Tool:      "hubbench",
		Version:   benchVersion,
		Scenario:  "chat",
		StartedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		Env:       Environment{GOOS: "windows", GOARCH: "amd64", NumCPU: 16, Connections: 100},
		Traffic: TrafficStats{
			SendPerSec:    sendRate,
			DeliverPerSec: deliverRate,
			ErrorRate:     errRate,
		},
		Latency: Stats{P50Ms: p50, P95Ms: p95, P99Ms: p99, MaxMs: max},
	}
}

// lineOf 取出含 needle 的那一行（对比表结论只看具体行，不能全表扫——
// 否则任何一行打了勾都会让断言假通过）。
func lineOf(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}

func TestRenderCompareLatencyImprovementMarksGreen(t *testing.T) {
	base := mkReport(1.0, 2.0, 10.0, 20.0, 500, 500, 0.001)
	cur := mkReport(1.0, 2.0, 5.0, 20.0, 500, 500, 0.001) // 只有 p99 降 50%
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	out := sb.String()
	if !strings.Contains(out, "p99 延迟") {
		t.Fatalf("对比表应包含 p99 行，实际：\n%s", out)
	}
	line := lineOf(out, "p99 延迟")
	if !strings.Contains(line, "-50.0%") {
		t.Fatalf("p99 应显示 -50.0%%，实际行：%q", line)
	}
	// 延迟降幅 >5% 是好事 → 该行打勾。
	if !strings.Contains(line, "✅") {
		t.Fatalf("p99 明显下降应标记 ✅，实际行：%q", line)
	}
}

func TestRenderCompareLatencyRegressionMarksWarning(t *testing.T) {
	base := mkReport(1.0, 2.0, 5.0, 20.0, 500, 500, 0.001)
	cur := mkReport(1.0, 2.0, 10.0, 20.0, 500, 500, 0.001) // p99 翻倍
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	out := sb.String()
	line := lineOf(out, "p99 延迟")
	if !strings.Contains(line, "+100.0%") {
		t.Fatalf("p99 应显示 +100.0%%，实际行：%q", line)
	}
	if !strings.Contains(line, "⚠") {
		t.Fatalf("p99 明显上升应标记 ⚠，实际行：%q", line)
	}
}

func TestRenderCompareThroughputHigherIsBetter(t *testing.T) {
	// 送达速率上升 20% → 对吞吐而言是好事，应打勾；这是与延迟相反的判定方向。
	base := mkReport(1, 1, 1, 1, 500, 1000, 0.001)
	cur := mkReport(1, 1, 1, 1, 500, 1200, 0.001)
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	line := lineOf(sb.String(), "送达速率")
	if !strings.Contains(line, "+20.0%") {
		t.Fatalf("送达速率应显示 +20.0%%，实际行：%q", line)
	}
	if !strings.Contains(line, "✅") {
		t.Fatalf("吞吐上升应标记 ✅，实际行：%q", line)
	}
}

func TestRenderCompareSmallDeltaHasNoMark(t *testing.T) {
	// ±5% 以内属于噪声，不该打勾或警告——否则每次压测都「有结论」，等于没有结论。
	base := mkReport(1.0, 2.0, 10.0, 20.0, 500, 500, 0.001)
	cur := mkReport(1.0, 2.0, 10.2, 20.0, 500, 500, 0.001) // +2%
	line := lineOf(renderToString(base, cur), "p99 延迟")
	if strings.Contains(line, "✅") || strings.Contains(line, "⚠") {
		t.Fatalf("±5%% 内不应标记，实际行：%q", line)
	}
}

func renderToString(base, cur *Report) string {
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	return sb.String()
}

func TestRenderCompareZeroBaselineShowsDash(t *testing.T) {
	// 基线为 0 时无法算百分比，必须显示「—」而不是 NaN 或 +Inf。
	base := mkReport(0, 0, 0, 0, 500, 500, 0)
	cur := mkReport(1, 1, 1, 1, 500, 500, 0)
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	out := sb.String()
	if strings.Contains(out, "NaN") || strings.Contains(out, "+Inf") || strings.Contains(out, "Inf%") {
		t.Fatalf("基线为 0 不应出现 NaN/Inf，实际：\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("无法比较时应显示 —，实际：\n%s", out)
	}
}

func TestRenderCompareWarnsOnEnvMismatch(t *testing.T) {
	base := mkReport(1, 1, 1, 1, 500, 500, 0)
	cur := mkReport(1, 1, 1, 1, 500, 500, 0)
	cur.Env.NumCPU = 8 // 换了机器
	var sb strings.Builder
	renderCompare(&sb, base, cur)
	if !strings.Contains(sb.String(), "环境指纹不同") {
		t.Fatalf("核数不同应提示环境指纹不同，实际：\n%s", sb.String())
	}

	// 同机器同核数不应提示，避免狼来了。
	var sb2 strings.Builder
	cur2 := mkReport(1, 1, 1, 1, 500, 500, 0)
	renderCompare(&sb2, base, cur2)
	if strings.Contains(sb2.String(), "环境指纹不同") {
		t.Fatalf("指纹一致时不应提示，实际：\n%s", sb2.String())
	}
}

func TestLoadReportRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := LoadReport(bad); err == nil {
		t.Fatalf("非 JSON 基线应报错")
	}
	if _, err := LoadReport(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatalf("不存在的基线应报错")
	}
}

func TestRealMainCompareMissingBaselineExitCode(t *testing.T) {
	// 基线不存在属于参数错误：必须在建连之前就退出 2，而不是跑完再报。
	var out, errOut strings.Builder
	code := realMain([]string{
		"-addr", "127.0.0.1:1",
		"-c", "2", "-d", "100ms", "-warmup", "0s",
		"-compare", filepath.Join(t.TempDir(), "absent.json"),
	}, &out, &errOut)
	if code != 2 {
		t.Fatalf("基线缺失应返回退出码 2，实际 %d（stderr=%s）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "参数错误") {
		t.Fatalf("stderr 应说明参数错误，实际 %q", errOut.String())
	}
}

func TestRenderCompareBothZeroMeansNoChange(t *testing.T) {
	// 基线 0 → 本次 0 是「没变化」，不是「无法比较」。压测里错误率常年是 0，
	// 若显示「—」会让人误以为这项没测到。
	base := mkReport(0, 0, 0, 0, 500, 500, 0)
	cur := mkReport(0, 0, 0, 0, 500, 500, 0)
	out := renderToString(base, cur)
	line := lineOf(out, "错误率")
	if !strings.Contains(line, "+0.0%") {
		t.Fatalf("双零应显示 +0.0%%，实际行：%q", line)
	}
	if strings.Contains(line, "—") {
		t.Fatalf("双零不应显示 —，实际行：%q", line)
	}
}

func TestRenderCompareMaxIsNotFlagged(t *testing.T) {
	// max 是单次极值，抖一次就 +150%；给它打 ⚠ 会让每次压测都「有回归」。
	base := mkReport(1, 1, 1, 8, 500, 500, 0.001)
	cur := mkReport(1, 1, 1, 20, 500, 500, 0.001)
	line := lineOf(renderToString(base, cur), "max 延迟")
	if !strings.Contains(line, "+150.0%") {
		t.Fatalf("max 应显示 +150.0%%，实际行：%q", line)
	}
	if strings.Contains(line, "⚠") || strings.Contains(line, "✅") {
		t.Fatalf("max 不应打结论标记，实际行：%q", line)
	}
	// 其他行照常判定：确认「不打标记」只针对 max，不是整表失效。
	if !strings.Contains(renderToString(base, cur), "判定阈值") {
		t.Fatalf("应输出阈值说明")
	}
}

func TestRenderCompareNewMetricFromZeroShowsDash(t *testing.T) {
	// 基线为 0、本次非 0：百分比无意义，显示 —。
	base := mkReport(1, 1, 1, 1, 500, 0, 0)
	cur := mkReport(1, 1, 1, 1, 500, 100, 0)
	line := lineOf(renderToString(base, cur), "送达速率")
	if !strings.Contains(line, "—") {
		t.Fatalf("基线 0 本次非 0 应显示 —，实际行：%q", line)
	}
}

/* ================= 小工具 ================= */

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// asUsage 是 errors.As 的薄包装（避免在测试里反复写 import errors 的样板）。
func asUsage(err error, target **usageError) bool {
	ue, ok := err.(*usageError)
	if !ok {
		return false
	}
	*target = ue
	return true
}
