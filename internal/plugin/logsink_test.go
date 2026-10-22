package plugin

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// sinkPlugin 是实现了 LogSink 的测试插件。
type sinkPlugin struct {
	mu     sync.Mutex
	msgs   []string
	levels []string
	delay  time.Duration
	writes int
}

func (p *sinkPlugin) Meta() Meta          { return Meta{Name: "fakesink", DefaultOn: true} }
func (p *sinkPlugin) Init(*Context) error { return nil }
func (p *sinkPlugin) Start() error        { return nil }
func (p *sinkPlugin) Stop() error         { return nil }

func (p *sinkPlugin) Write(level, msg string, ts time.Time) {
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes++
	p.msgs = append(p.msgs, msg)
	p.levels = append(p.levels, level)
}

func (p *sinkPlugin) snapshot() ([]string, []string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.msgs...), append([]string(nil), p.levels...), p.writes
}

// TestLogFanoutKeepsOriginalOutputAndFeedsSink 同时断言两件事：
//  1. 原输出（模拟 lan-agent-hub.log 的文件 writer）**行为不变**，仍拿到完整一行；
//  2. LogSink 拿到剥掉时间前缀的正文与推断出的级别。
func TestLogFanoutKeepsOriginalOutputAndFeedsSink(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)

	var fileBuf bytes.Buffer // 模拟 -H windowsgui 场景下的日志文件
	log.SetOutput(&fileBuf)

	sink := &sinkPlugin{}
	m := newManagerFrom([]Plugin{sink})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	log.Printf("tcpgateway failed to bind :8080")

	if !strings.Contains(fileBuf.String(), "tcpgateway failed to bind :8080") {
		t.Fatalf("原输出丢失（lan-agent-hub.log 行为必须不变）: %q", fileBuf.String())
	}

	msgs, levels, _ := waitForWrites(t, sink, 1)
	if msgs[0] != "tcpgateway failed to bind :8080" {
		t.Errorf("sink 收到的正文 = %q，期望剥掉时间前缀的原始消息", msgs[0])
	}
	if levels[0] != LevelError {
		t.Errorf("级别 = %q，期望 %q（含 failed 关键字）", levels[0], LevelError)
	}
	if n := m.LogDropped(); n != 0 {
		t.Errorf("不应有丢弃，实际 %d", n)
	}
}

// TestLogFanoutNotInstalledWithoutSink：没有 LogSink 时不得接管 log（V4 等价）。
func TestLogFanoutNotInstalledWithoutSink(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)

	var buf bytes.Buffer
	log.SetOutput(&buf)

	m := newManagerFrom([]Plugin{&plainPlugin{meta: Meta{Name: "aplain"}}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	if log.Writer() != io.Writer(&buf) {
		t.Fatal("无 LogSink 时不应改写 log 输出")
	}
	log.Printf("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatal("日志未照常写入原输出")
	}
	if m.LogDropped() != 0 {
		t.Fatal("未接管时丢弃计数应为 0")
	}
}

// TestLogFanoutIsIdempotent：BuildContext 重复调用不得让同一行被投递多次。
func TestLogFanoutIsIdempotent(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)
	log.SetOutput(io.Discard)

	sink := &sinkPlugin{}
	m := newManagerFrom([]Plugin{sink})
	for i := 0; i < 3; i++ {
		if err := m.BuildContext(nil); err != nil {
			t.Fatalf("BuildContext: %v", err)
		}
	}
	log.Printf("once")
	msgs, _, writes := waitForWrites(t, sink, 1)
	// 给潜在的重复投递一点时间暴露出来。
	time.Sleep(30 * time.Millisecond)
	_, _, writesAfter := sink.snapshot()
	if writesAfter != writes {
		t.Fatalf("重复投递：%d → %d 次", writes, writesAfter)
	}
	if len(msgs) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条: %v", len(msgs), msgs)
	}
}

// TestLogFanoutDoesNotBlockProducer：慢 sink 让队列写满后必须丢弃并计数，
// 绝不反过来阻塞 log.Printf（日志不能拖慢业务线程）。
func TestLogFanoutDoesNotBlockProducer(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)
	log.SetOutput(io.Discard)

	sink := &sinkPlugin{delay: 5 * time.Millisecond}
	m := newManagerFrom([]Plugin{sink})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	const lines = 2000
	start := time.Now()
	for i := 0; i < lines; i++ {
		log.Printf("burst line %d", i)
	}
	elapsed := time.Since(start)

	// 若阻塞：2000 行 × 5ms 至少 10s。给足余量的上限是 1s。
	if elapsed > time.Second {
		t.Fatalf("log 写入被 sink 阻塞：%d 行耗时 %s（队列必须满即丢弃）", lines, elapsed)
	}
	if m.LogDropped() == 0 {
		t.Fatalf("慢 sink 场景应产生丢弃计数，实际 0（队列容量 %d，写入 %d 行）", logFanoutQueue, lines)
	}
	t.Logf("实测：%d 行日志耗时 %s，丢弃 %d 行（队列容量 %d）", lines, elapsed, m.LogDropped(), logFanoutQueue)
}

// TestLogFanoutSurvivesSinkPanic：单个 sink panic 不能拖垮日志派发。
func TestLogFanoutSurvivesSinkPanic(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)
	log.SetOutput(io.Discard)

	bad := &panicSink{}
	good := &sinkPlugin{}
	m := newManagerFrom([]Plugin{bad, good})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	log.Printf("after panic")
	msgs, _, _ := waitForWrites(t, good, 1)
	if len(msgs) != 1 {
		t.Fatalf("panic 的 sink 之后的 sink 应仍收到日志，实际 %d 条", len(msgs))
	}
}

// panicSink 是 Write 必 panic 的 sink（名字排序靠前，保证先被调用）。
type panicSink struct{}

func (*panicSink) Meta() Meta                      { return Meta{Name: "apanic", DefaultOn: true} }
func (*panicSink) Init(*Context) error             { return nil }
func (*panicSink) Start() error                    { return nil }
func (*panicSink) Stop() error                     { return nil }
func (*panicSink) Write(string, string, time.Time) { panic("sink boom") }

// TestInferLogLevel 覆盖级别推断规则（不改任何日志文案，只做关键字归类）。
func TestInferLogLevel(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"httpgateway listen :8081: bind failed", LevelError},
		{"plugin discovery error: timeout", LevelError},
		{"recovered from panic in handler", LevelError},
		{"deprecated flag -ws is ignored", LevelWarn},
		{"sweeper: interval too small, using warning default", LevelWarn},
		{"LAN Agent Hub 启动完成 (tcp=:8080 http=:8081)", LevelInfo},
		{"enabled=[webconsole] disabled=[]", LevelInfo},
	}
	for _, c := range cases {
		if got := InferLogLevel(c.msg); got != c.want {
			t.Errorf("InferLogLevel(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// TestSplitLogLine 断言默认 log 前缀被正确剥离（时间戳则从前缀解析）。
func TestSplitLogLine(t *testing.T) {
	msg, ts := splitLogLine("2026/10/22 13:18:46 LAN Agent Hub 启动完成")
	if msg != "LAN Agent Hub 启动完成" {
		t.Errorf("msg = %q", msg)
	}
	if ts.IsZero() || ts.Year() != 2026 {
		t.Errorf("ts = %v，期望从前缀解析出 2026 年的时间", ts)
	}
	// 无前缀：原样返回正文，时间用当前时间兜底（绝不给零值）。
	msg, ts = splitLogLine("no prefix here")
	if msg != "no prefix here" {
		t.Errorf("msg = %q", msg)
	}
	if time.Since(ts) > time.Minute {
		t.Errorf("无前缀时应回退到当前时间，实际 %v", ts)
	}
}

// waitForWrites 轮询等待 sink 至少收到 n 条日志（派发是异步的）。
func waitForWrites(t *testing.T, sink *sinkPlugin, n int) ([]string, []string, int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs, levels, writes := sink.snapshot()
		if writes >= n {
			return msgs, levels, writes
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 sink 收到 %d 条日志超时（实际 %d）", n, writes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
