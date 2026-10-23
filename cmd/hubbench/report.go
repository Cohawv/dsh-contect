package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// benchVersion 是压测工具自身的版本（报告里带，便于跨时间对比）。
const benchVersion = "1.0.0"

// 错误分类：报告里按类计数，避免只看到一个「错误率 3%」却不知道该修哪一侧。
const (
	errDial       = "dial"
	errRegister   = "register"
	errSend       = "send"
	errRecv       = "recv"
	errTimeout    = "timeout"
	errUnexpected = "unexpected"
)

// collector 汇总所有连接的数据。
//
// 计数器用 atomic（高频、无锁）；直方图每连接各持一个，结束时合并 ——
// 让全局锁远离热路径，否则压测工具自己会先成为瓶颈。
type collector struct {
	lat  *Histogram
	slow *slowBuf

	// recording 是「采样开关」：预热期间为 false，预热结束才打开。
	// 连接层计数（connAttempted 等）**不受它影响** —— 建连发生在预热之前，
	// 那部分数据本来就该统计。
	recording atomic.Bool

	connAttempted   atomic.Int64
	connEstablished atomic.Int64
	connFailed      atomic.Int64
	registerFailed  atomic.Int64
	sent            atomic.Int64
	delivered       atomic.Int64
	errors          atomic.Int64

	mu     sync.Mutex
	errMap map[string]int64
}

func newCollector() *collector {
	return &collector{lat: newHistogram(), slow: newSlowBuf(10), errMap: map[string]int64{}}
}

// addLatency 记录一次往返延迟（仅采样期生效）。
func (c *collector) addLatency(d time.Duration, client, nonce string) {
	if !c.recording.Load() {
		return
	}
	c.lat.Observe(d)
	c.slow.add(client, nonce, d)
}

func (c *collector) countSent() {
	if c.recording.Load() {
		c.sent.Add(1)
	}
}

func (c *collector) countDelivered() {
	if c.recording.Load() {
		c.delivered.Add(1)
	}
}

// err 记录一条错误（仅采样期生效）。
func (c *collector) err(kind string) {
	if !c.recording.Load() {
		return
	}
	c.errors.Add(1)
	c.mu.Lock()
	c.errMap[kind]++
	c.mu.Unlock()
}

// connErr 记录连接层错误（不受采样开关影响：建连在预热之前）。
func (c *collector) connErr(err error) {
	kind := errDial
	msg := err.Error()
	switch {
	case strings.Contains(msg, "register"):
		kind = errRegister
	case strings.Contains(msg, "recv"):
		kind = errRecv
	}
	c.errors.Add(1)
	c.mu.Lock()
	c.errMap[kind]++
	c.mu.Unlock()
}

func (c *collector) RegisterFailed() { c.registerFailed.Add(1) }

func (c *collector) errSnapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errMap) == 0 {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(c.errMap))
	for k, v := range c.errMap {
		out[k] = v
	}
	return out
}

// Environment 是环境指纹：没有它，两台机器上的数字无法比较，
// 「这次比上次慢了」也可能只是换了台笔记本。
type Environment struct {
	GOOS        string `json:"goos"`
	GOARCH      string `json:"goarch"`
	GoVersion   string `json:"goVersion"`
	NumCPU      int    `json:"numCPU"`
	GOMAXPROCS  int    `json:"gomaxprocs"`
	Addr        string `json:"addr"`
	HTTPAddr    string `json:"httpAddr,omitempty"`
	Connections int    `json:"connections"`
	Senders     int    `json:"senders"`
	Echoers     int    `json:"echoers"`
	RatePerSec  string `json:"ratePerSenderPerSec"`
	WarmupMs    int64  `json:"warmupMs"`
	// FlapPerSec 仅 flap 场景有值：上下线抖动每秒次数（此时 Connections/RatePerSec 无意义）。
	FlapPerSec int `json:"flapPerSec,omitempty"`
}

// ConnectionStats 是连接层统计。
type ConnectionStats struct {
	Attempted      int64 `json:"attempted"`
	Established    int64 `json:"established"`
	Failed         int64 `json:"failed"`
	RegisterFailed int64 `json:"registerFailed"`
}

// TrafficStats 是流量层统计。
type TrafficStats struct {
	Sent          int64   `json:"sent"`
	Delivered     int64   `json:"delivered"`
	SendPerSec    float64 `json:"sendPerSec"`
	DeliverPerSec float64 `json:"deliverPerSec"`
	ErrorRate     float64 `json:"errorRate"` // errors / (sent + errors)
}

// HubStats 是 Hub 侧的观测值（来自 /metrics，V5 就绪）。
type HubStats struct {
	Scraped            bool    `json:"scraped"`
	AgentsOnline       float64 `json:"agentsOnline"`
	Rooms              float64 `json:"rooms"`
	MessagesTotal      float64 `json:"messagesTotal"`
	AgentsOnlineBefore float64 `json:"agentsOnlineBefore,omitempty"`
	MessagesBefore     float64 `json:"messagesTotalBefore,omitempty"`
	Error              string  `json:"error,omitempty"`
}

// Report 是一次压测的完整结果（`-out` 直接落这个结构）。
type Report struct {
	Tool       string           `json:"tool"`
	Version    string           `json:"version"`
	Scenario   string           `json:"scenario"`
	StartedAt  time.Time        `json:"startedAt"`
	DurationMs int64            `json:"durationMs"`
	Env        Environment      `json:"env"`
	Conn       ConnectionStats  `json:"connections"`
	Traffic    TrafficStats     `json:"traffic"`
	Latency    Stats            `json:"latency"`
	Errors     map[string]int64 `json:"errors"`
	Hub        *HubStats        `json:"hub,omitempty"`
	Slowest    []SlowSample     `json:"slowest,omitempty"`
	Notes      []string         `json:"notes,omitempty"`
	OK         bool             `json:"ok"`
}

// scrapeMetrics 读 Hub 的 /metrics 并取出报告需要的几个量。
// 解析用最朴素的 `name value` 行匹配：只依赖 Prometheus 文本格式的公共形状，
// 不引入解析库（零依赖铁律）。
func scrapeMetrics(httpAddr string, timeout time.Duration) (map[string]float64, error) {
	if httpAddr == "" {
		return nil, fmt.Errorf("未指定 -http，跳过 Hub 指标采集")
	}
	url := "http://" + httpAddr + "/metrics"
	cli := &http.Client{Timeout: timeout}
	resp, err := cli.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/metrics 返回 %d", resp.StatusCode)
	}
	want := map[string]bool{
		"hub_agents_online":  true,
		"hub_rooms":          true,
		"hub_messages_total": true,
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !want[fields[0]] {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err == nil {
			out[fields[0]] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// renderTable 把报告渲染成人读的表（默认输出；`-json` 时只输出 JSON）。
func renderTable(w io.Writer, r *Report) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("hubbench %s · 场景 %s · 时长 %.1fs\n", r.Version, r.Scenario, float64(r.DurationMs)/1000)
	p("────────────────────────────────────────────────────────\n")
	p("环境    %s/%s · %d 核 · GOMAXPROCS=%d · %s\n",
		r.Env.GOOS, r.Env.GOARCH, r.Env.NumCPU, r.Env.GOMAXPROCS, r.Env.GoVersion)
	if r.Env.FlapPerSec > 0 {
		p("目标    tcp://%s   上下线抖动 %d 次/秒（不发送业务消息）\n", r.Env.Addr, r.Env.FlapPerSec)
	} else {
		p("目标    tcp://%s   连接 %d（发送方 %d / 回声方 %d）  速率 %s/秒/发送方\n",
			r.Env.Addr, r.Env.Connections, r.Env.Senders, r.Env.Echoers, r.Env.RatePerSec)
	}
	p("────────────────────────────────────────────────────────\n")
	p("连接    尝试 %d · 成功 %d · 失败 %d（注册失败 %d）\n",
		r.Conn.Attempted, r.Conn.Established, r.Conn.Failed, r.Conn.RegisterFailed)
	p("流量    发送 %d 条（%.1f/秒） · 送达 %d 条（%.1f/秒）\n",
		r.Traffic.Sent, r.Traffic.SendPerSec, r.Traffic.Delivered, r.Traffic.DeliverPerSec)
	p("错误    总计 %d 条（错误率 %.3f%%）\n", r.Errors["_total"], r.Traffic.ErrorRate*100)
	if len(r.Errors) > 0 {
		keys := make([]string, 0, len(r.Errors))
		for k := range r.Errors {
			if k == "_total" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p("          · %-12s %d\n", k, r.Errors[k])
		}
	}
	if r.Latency.Count > 0 {
		p("延迟    p50 %.3fms · p95 %.3fms · p99 %.3fms · max %.3fms · 均值 %.3fms\n",
			r.Latency.P50Ms, r.Latency.P95Ms, r.Latency.P99Ms, r.Latency.MaxMs, r.Latency.MeanMs)
	} else {
		p("延迟    无样本（该场景不测量往返延迟）\n")
	}
	if r.Hub != nil && r.Hub.Scraped {
		p("Hub     在线 Agent %.0f · 房间 %.0f · 累计消息 %.0f\n",
			r.Hub.AgentsOnline, r.Hub.Rooms, r.Hub.MessagesTotal)
		if r.Hub.MessagesBefore > 0 {
			p("        （压测期间 Hub 侧新增路由消息 %.0f 条）\n", r.Hub.MessagesTotal-r.Hub.MessagesBefore)
		}
	} else if r.Hub != nil && r.Hub.Error != "" {
		p("Hub     未采集：%s\n", r.Hub.Error)
	}
	if len(r.Slowest) > 0 {
		p("最慢    前 %d 条：", len(r.Slowest))
		for i, s := range r.Slowest {
			if i > 0 {
				p("、")
			}
			p("%.3fms", s.Ms)
		}
		p("\n")
	}
	for _, n := range r.Notes {
		p("备注    %s\n", n)
	}
	verdict := "PASS"
	if !r.OK {
		verdict = "FAIL"
	}
	p("────────────────────────────────────────────────────────\n")
	p("结论    %s\n", verdict)
}

// fingerprint 组装环境指纹。
func fingerprint(addr, httpAddr string, senders, echoers int, rate string, warmup time.Duration) Environment {
	return Environment{
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		GoVersion:   runtime.Version(),
		NumCPU:      runtime.NumCPU(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		Addr:        addr,
		HTTPAddr:    httpAddr,
		Connections: senders + echoers,
		Senders:     senders,
		Echoers:     echoers,
		RatePerSec:  rate,
		WarmupMs:    warmup.Milliseconds(),
	}
}

// LoadReport 读入一份历史报告（`-compare` 的基线）。
func LoadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析基线报告 %s 失败: %w", path, err)
	}
	return &r, nil
}

// delta 计算相对变化百分比；基线为 0 时返回 NaN 表示无法比较。
func delta(base, cur float64) float64 {
	if base == 0 {
		return math.NaN()
	}
	return (cur - base) / base * 100
}

// renderCompare 打印「基线 vs 本次」对比表。
//
// 为什么内置而不是交给 jq：本项目零外部依赖，而对比表是压测的核心产出之一
// （「优化有没有效果」全靠它）。放在工具里，脚本只负责跑与存档。
func renderCompare(w io.Writer, base, cur *Report) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	envWarn := ""
	if base.Env.NumCPU != cur.Env.NumCPU || base.Env.GOOS != cur.Env.GOOS || base.Env.Connections != cur.Env.Connections {
		envWarn = fmt.Sprintf("  ⚠ 环境指纹不同（基线 %s/%s %d 核 %d 连接 → 本次 %s/%s %d 核 %d 连接），数字仅供参考",
			base.Env.GOOS, base.Env.GOARCH, base.Env.NumCPU, base.Env.Connections,
			cur.Env.GOOS, cur.Env.GOARCH, cur.Env.NumCPU, cur.Env.Connections)
	}
	p("\n基线对比（%s · 场景 %s）\n", base.StartedAt.Format("2006-01-02 15:04"), base.Scenario)
	p("%-14s %12s %12s %10s\n", "指标", "基线", "本次", "变化")
	p("%s\n", strings.Repeat("─", 52))
	// rowMark：allowMark=false 时只列数字不打结论标记。
	rowMark := func(name, unit string, b, c float64, lowerBetter, allowMark bool) {
		// 基线 0 → 0 不是「无法比较」，是「没变化」；只有 0 → 非 0 才算无法算百分比。
		dtxt := "—"
		mark := ""
		switch {
		case b == 0 && c == 0:
			dtxt = "+0.0%"
		case b != 0:
			d := delta(b, c)
			dtxt = fmt.Sprintf("%+.1f%%", d)
			if allowMark {
				if lowerBetter {
					if d < -5 {
						mark = "✅"
					} else if d > 5 {
						mark = "⚠"
					}
				} else {
					if d > 5 {
						mark = "✅"
					} else if d < -5 {
						mark = "⚠"
					}
				}
			}
		}
		p("%-14s %10.3f%s %10.3f%s %8s %s\n", name, b, unit, c, unit, dtxt, mark)
	}
	row := func(name, unit string, b, c float64, lowerBetter bool) { rowMark(name, unit, b, c, lowerBetter, true) }
	row("p50 延迟", "ms", base.Latency.P50Ms, cur.Latency.P50Ms, true)
	row("p95 延迟", "ms", base.Latency.P95Ms, cur.Latency.P95Ms, true)
	row("p99 延迟", "ms", base.Latency.P99Ms, cur.Latency.P99Ms, true)
	// max 是单个极值样本，天生抖；用它下结论会造成「每次压测都有回归」的误报。
	rowMark("max 延迟", "ms", base.Latency.MaxMs, cur.Latency.MaxMs, true, false)
	row("发送速率", "/s", base.Traffic.SendPerSec, cur.Traffic.SendPerSec, false)
	row("送达速率", "/s", base.Traffic.DeliverPerSec, cur.Traffic.DeliverPerSec, false)
	row("错误率", "%", base.Traffic.ErrorRate*100, cur.Traffic.ErrorRate*100, true)
	p("%s\n", strings.Repeat("─", 52))
	p("  判定阈值 ±5%%；max 为单次极值样本，仅列数字不作结论。\n")
	if envWarn != "" {
		p("%s\n", envWarn)
	}
}
