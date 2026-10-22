package plugin

import (
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogSink 是「可接收服务端日志」的插件的**可选**接口（V5 ①）。
//
// 框架在 BuildContext 时扫描一遍插件清单：只要有一个插件实现了它，
// 就把 log 的输出**额外**分发给这些 sink（原输出保持不变）。
// 没有任何插件实现时完全不接管 log —— 行为与 V4 逐字节一致。
//
// 契约（实现方必须遵守）：
//  1. **绝不能阻塞**：Write 会被日志派发协程逐个调用，实现内部必须自己再入队/丢弃；
//  2. **绝不能写日志**：Write 里再调 log.Printf 会形成「日志→sink→日志」的自激回路
//     （框架侧的队列是有界的，不会死锁，但会持续丢日志并让排障变噪声）；
//  3. Write 可能被并发调用，实现必须自己加锁；
//  4. level 取值固定为 "info" / "warn" / "error"（由框架从日志文本推断，见 InferLogLevel）。
type LogSink interface {
	Write(level, msg string, ts time.Time)
}

// 日志派发队列容量：日志是低频高价值数据，512 条足以吸收突发；
// 队列满即丢弃并计数（绝不反过来阻塞 log.Printf，否则会拖住业务线程）。
const logFanoutQueue = 512

// 日志级别取值（LogSink.Write 的 level 参数）。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// logFanout 是替换给 log 包的 io.Writer：
// 先写原输出（文件 / 终端），再非阻塞地把这一行投递给各 LogSink。
type logFanout struct {
	out     io.Writer
	ch      chan string
	mu      sync.RWMutex
	sinks   []LogSink
	dropped atomic.Int64
}

// newLogFanout 构造 fanout（不启动派发协程，由调用方 go f.run()）。
func newLogFanout(out io.Writer, sinks []LogSink) *logFanout {
	return &logFanout{
		out:   out,
		ch:    make(chan string, logFanoutQueue),
		sinks: sinks,
	}
}

// Write 实现 io.Writer：**先**写原输出（保证 lan-agent-hub.log 与终端行为不变），
// 再非阻塞入队。原输出出错时把错误原样返回给 log 包（与没有 fanout 时一致）。
func (f *logFanout) Write(p []byte) (int, error) {
	n, err := f.out.Write(p)
	line := strings.TrimRight(string(p), "\r\n")
	select {
	case f.ch <- line:
	default:
		// 队列满：丢弃这一行并计数。绝不阻塞——日志不能拖慢业务线程。
		f.dropped.Add(1)
	}
	return n, err
}

// run 是唯一的日志派发协程：解析时间戳/级别，逐个投递给 sink。
func (f *logFanout) run() {
	for line := range f.ch {
		msg, ts := splitLogLine(line)
		level := InferLogLevel(msg)
		f.mu.RLock()
		sinks := f.sinks
		f.mu.RUnlock()
		for _, s := range sinks {
			writeSinkSafe(s, level, msg, ts)
		}
	}
}

// setSinks 更新 sink 清单（BuildContext 被重复调用时复用同一 fanout，避免重复包裹 log）。
func (f *logFanout) setSinks(sinks []LogSink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinks = sinks
}

// writeSinkSafe 隔离单个 sink 的 panic：一个坏 sink 不能拖垮日志派发。
func writeSinkSafe(s LogSink, level, msg string, ts time.Time) {
	defer func() { _ = recover() }()
	s.Write(level, msg, ts)
}

// InferLogLevel 从日志文本推断级别。
//
// 设计取舍：现有日志文案一律**不改**（它们是排障时的肌肉记忆，也是历史上被引用的证据），
// 因此级别靠关键字推断：failed/error/panic → error；warn/deprecated → warn；其余 info。
// 推断规则集中在这一处，任何日志行都得到同一套判定。
func InferLogLevel(msg string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "failed"), strings.Contains(low, "error"),
		strings.Contains(low, "panic"):
		return LevelError
	case strings.Contains(low, "warn"), strings.Contains(low, "deprecated"):
		return LevelWarn
	default:
		return LevelInfo
	}
}

// splitLogLine 拆掉 log 包默认前缀（"2006/01/02 15:04:05 "），得到纯消息与时间戳。
//
// 前缀存在与否由 log.SetFlags 决定（本仓库用默认 LstdFlags）；不带前缀时
// 用当前时间兜底，保证任何写入方式都不会让 sink 拿到零值时间。
func splitLogLine(line string) (msg string, ts time.Time) {
	const prefixLen = len("2006/01/02 15:04:05 ")
	if len(line) > prefixLen && line[4] == '/' && line[7] == '/' && line[10] == ' ' &&
		line[13] == ':' && line[16] == ':' && line[19] == ' ' {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:prefixLen-1], time.Local); err == nil {
			return line[prefixLen:], t
		}
	}
	return line, time.Now()
}

// installLogFanout 扫描实现了 LogSink 的插件并接管 log 输出（幂等）。
//
// 幂等性很重要：BuildContext 可以重复调用（测试里常见），
// 重复包裹 log.Writer() 会让同一行日志被投递多次。
func (m *Manager) installLogFanout() {
	sinks := make([]LogSink, 0, 4)
	for _, p := range m.plugins {
		if s, ok := p.(LogSink); ok {
			sinks = append(sinks, s)
		}
	}
	if len(sinks) == 0 {
		return
	}

	m.mu.Lock()
	if m.logFan != nil {
		fan := m.logFan
		m.mu.Unlock()
		fan.setSinks(sinks)
		return
	}
	fan := newLogFanout(log.Writer(), sinks)
	m.logFan = fan
	m.mu.Unlock()

	go fan.run()
	log.SetOutput(fan)
}

// LogDropped 返回累计因队列满被丢弃的日志行数（供 metrics 观测）。
// 未启用 fanout 时恒为 0。
func (m *Manager) LogDropped() int64 {
	m.mu.RLock()
	fan := m.logFan
	m.mu.RUnlock()
	if fan == nil {
		return 0
	}
	return fan.dropped.Load()
}
