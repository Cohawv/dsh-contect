// Package metrics 是「指标导出 + 概览」插件（V5 ③）。
//
// 端点：
//   - GET /metrics              Prometheus 文本格式（Content-Type: text/plain; version=0.0.4）
//   - GET /api/metrics/summary  最近 15 个采样点 + 消息速率 + 运行时长（控制台概览用）
//
// 设计纪律：
//   - **零外部依赖**：Prometheus 文本格式手写（每族三行：HELP / TYPE / 值），
//     不引 prometheus/client_golang（那会拖进一整棵依赖树）；
//   - 计数器用 atomic.Int64（无锁、跨平台）；
//   - 与 /api/status **同源计算**：在线数/房间数直接读内核快照，不另立统计口径，
//     因此两个端点永远自洽；
//   - **排除高基数字段**：标签只到 path / plugin 这一级，绝不出现 agentId 列表；
//     path 维度还有聚合上限（超出归入 other），防基数爆炸。
package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// sseSharedKey 是 realtime 插件在共享 KV 里登记的键。
//
// 刻意**重复声明字面量**而不 import realtime 包：插件之间零编译依赖，
// 任何一方被从二进制里移除都不会拖累另一方（签名的契约由 sseCounters 类型保证）。
const sseSharedKey = "realtime.sse"

// 端点路径。
const (
	pathMetrics = "/metrics"
	pathSummary = "/api/metrics/summary"
)

// 默认与边界取值。
const (
	defaultInterval = 5 * time.Second
	minInterval     = 200 * time.Millisecond
	// sampleCount 是趋势数据保留的采样点数（15 个 × 默认 5s ≈ 75 秒窗口）。
	sampleCount = 15
	// maxLabelPaths 是 hub_http_requests_total 的 path 标签基数上限。
	// 超出后统一归入 "other"：宁可少一个维度，也不要让 Prometheus 侧基数爆炸。
	maxLabelPaths = 64
	// busQueue 是总线异步订阅队列长度（metrics 处理不过来时由总线丢弃并计数）。
	busQueue = 512
)

// Sample 是一个趋势采样点。
type Sample struct {
	TS            int64 `json:"ts"`            // 采样时刻（毫秒）
	AgentsOnline  int   `json:"agentsOnline"`  // 在线 Agent 数
	Rooms         int   `json:"rooms"`         // 房间数
	MessagesTotal int64 `json:"messagesTotal"` // 累计路由消息数
}

// Metrics 是 metrics 插件实体。
type Metrics struct {
	hub *core.Hub
	mgr *plugin.Manager
	ctx *plugin.Context

	intervalNs atomic.Int64
	startedAt  atomic.Int64 // 插件启动时间（毫秒）

	// 计数器
	msgTotal  atomic.Int64 // 累计 msg.routed 事件数
	httpTotal atomic.Int64 // 累计 HTTP 请求数（观测范围内的路由）

	mu         sync.Mutex
	httpByPath map[string]int64
	overflow   int64 // 超出 maxLabelPaths 后的聚合计数
	samples    []Sample

	unsubs []func()
	closed chan struct{}
}

// Meta 声明插件元信息。依赖 httpgateway（复用共享 mux）。
func (p *Metrics) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "metrics",
		Version:      "1.0.0",
		Description:  "Prometheus 指标导出（/metrics）与概览趋势（/api/metrics/summary）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项。
func (p *Metrics) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "interval",
			Type:        plugin.OptionDuration,
			Default:     defaultInterval.String(),
			Description: "趋势采样间隔（控制台概览的折线粒度）；支持热更新",
		},
	}
}

// Init 取内核与管理器、解析采样间隔并注册两个端点。
func (p *Metrics) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("metrics: missing *core.Hub in context")
	}
	p.hub = hub
	p.ctx = ctx
	if mgr, ok := plugin.ManagerFromContext(ctx); ok {
		p.mgr = mgr
	}

	interval := ctx.Duration("interval", defaultInterval)
	if interval < minInterval {
		interval = minInterval
	}
	p.intervalNs.Store(int64(interval))

	p.mu.Lock()
	if p.httpByPath == nil {
		p.httpByPath = make(map[string]int64)
	}
	p.mu.Unlock()

	if err := ctx.RegisterHTTPUnobserved(pathMetrics, p.handleMetrics); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	// 概览端点自己也排除：它是控制台的高频轮询目标，计进去会把趋势曲线变成自画像。
	if err := ctx.RegisterHTTPUnobserved(pathSummary, p.handleSummary); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	return nil
}

// Start 订阅事件总线并把采样协程拉起来。
func (p *Metrics) Start() error {
	if p.hub == nil {
		return fmt.Errorf("metrics: hub not initialized")
	}
	p.startedAt.Store(time.Now().UnixMilli())
	closed := make(chan struct{})
	p.mu.Lock()
	p.closed = closed
	p.mu.Unlock()

	// 一律 SubscribeAsync：计数绝不能反压到消息热路径 / HTTP 服务路径。
	p.unsubs = append(p.unsubs,
		p.hub.Bus.SubscribeAsync(protocol.TopicMsgRouted, func(_ string, payload any) {
			p.msgTotal.Add(1)
		}, busQueue),
		p.hub.Bus.SubscribeAsync(protocol.TopicHTTPRequest, func(_ string, payload any) {
			p.countHTTP(payload)
		}, busQueue),
	)

	// 先取一个初始采样点，使控制台打开时就有曲线起点（而不是空图）。
	p.sample()
	go p.sampleLoop(closed)
	return nil
}

// Stop 退订并停掉采样协程。
func (p *Metrics) Stop() error {
	for _, unsub := range p.unsubs {
		unsub()
	}
	p.unsubs = nil
	p.mu.Lock()
	if p.closed != nil {
		close(p.closed)
		p.closed = nil
	}
	p.mu.Unlock()
	return nil
}

// Apply 实现 plugin.Applier：热更新采样间隔。
func (p *Metrics) Apply(options map[string]any) error {
	raw, ok := options["interval"]
	if !ok {
		return nil
	}
	var d time.Duration
	switch v := raw.(type) {
	case float64:
		d = time.Duration(v) * time.Millisecond
	case int:
		d = time.Duration(v) * time.Millisecond
	case int64:
		d = time.Duration(v) * time.Millisecond
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("metrics: interval 不是合法时长: %s", v)
		}
		d = parsed
	default:
		return fmt.Errorf("metrics: interval 类型不支持")
	}
	if d < minInterval {
		return fmt.Errorf("metrics: interval 过小（%s，最小 %s）", d, minInterval)
	}
	p.intervalNs.Store(int64(d))
	return nil
}

// interval 返回采样间隔（热更新安全）。
func (p *Metrics) interval() time.Duration {
	d := time.Duration(p.intervalNs.Load())
	if d < minInterval {
		return defaultInterval
	}
	return d
}

// countHTTP 记录一次 HTTP 请求（按 path 标签聚合，带基数上限）。
func (p *Metrics) countHTTP(payload any) {
	req, ok := payload.(plugin.HTTPRequest)
	if !ok {
		return
	}
	path := req.Path
	if path == "" {
		path = "unknown"
	}
	p.httpTotal.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.httpByPath == nil {
		p.httpByPath = make(map[string]int64)
	}
	if _, known := p.httpByPath[path]; !known && len(p.httpByPath) >= maxLabelPaths {
		p.overflow++
		return
	}
	p.httpByPath[path]++
}

// sampleLoop 周期采样（每轮重新读取间隔，热更新立即生效）。
func (p *Metrics) sampleLoop(closed <-chan struct{}) {
	for {
		timer := time.NewTimer(p.interval())
		select {
		case <-closed:
			timer.Stop()
			return
		case <-timer.C:
			p.sample()
		}
	}
}

// sample 记录一个采样点（定长环形，最多 sampleCount 个）。
func (p *Metrics) sample() {
	if p.hub == nil {
		return
	}
	s := Sample{
		TS:            time.Now().UnixMilli(),
		AgentsOnline:  len(p.hub.ListAgentsDetailed()),
		Rooms:         len(p.hub.Rooms.List()),
		MessagesTotal: p.msgTotal.Load(),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples = append(p.samples, s)
	if len(p.samples) > sampleCount {
		p.samples = append([]Sample(nil), p.samples[len(p.samples)-sampleCount:]...)
	}
}

// snapshotSamples 返回采样点副本（时间正序）。
func (p *Metrics) snapshotSamples() []Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Sample, len(p.samples))
	copy(out, p.samples)
	return out
}

// uptime 返回插件运行时长（毫秒）。
func (p *Metrics) uptime() int64 {
	start := p.startedAt.Load()
	if start == 0 {
		return 0
	}
	return time.Now().UnixMilli() - start
}

// ---------------------------------------------------------------------------
// /metrics
// ---------------------------------------------------------------------------

// handleMetrics 输出 Prometheus 文本格式。
func (p *Metrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(p.render()))
}

// render 拼装指标文本。
//
// 每一族严格三行（# HELP / # TYPE / 值行），便于 Prometheus 与「逐行解析」测试；
// 多值族的标签按字典序输出，保证同一份数据每次渲染逐字节一致（可做 golden）。
func (p *Metrics) render() string {
	var b strings.Builder

	agentsOnline, rooms := 0, 0
	eventsDropped := int64(0)
	if p.hub != nil {
		agentsOnline = len(p.hub.ListAgentsDetailed())
		rooms = len(p.hub.Rooms.List())
		eventsDropped = p.hub.Bus.Dropped()
	}

	writeGauge(&b, "hub_agents_online", "当前在线 Agent 数", float64(agentsOnline))
	writeGauge(&b, "hub_rooms", "房间数", float64(rooms))
	writeCounter(&b, "hub_messages_total", "累计路由消息数", float64(p.msgTotal.Load()))

	// HTTP 请求数（按 path 标签）：基数有上限，超出归入 other。
	p.mu.Lock()
	paths := make([]string, 0, len(p.httpByPath))
	for k := range p.httpByPath {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	byPath := make(map[string]int64, len(p.httpByPath))
	for k, v := range p.httpByPath {
		byPath[k] = v
	}
	overflow := p.overflow
	p.mu.Unlock()

	b.WriteString("# HELP hub_http_requests_total HTTP 请求数（按路由 pattern 标签）\n")
	b.WriteString("# TYPE hub_http_requests_total counter\n")
	for _, path := range paths {
		fmt.Fprintf(&b, "hub_http_requests_total{path=%q} %d\n", path, byPath[path])
	}
	if overflow > 0 {
		fmt.Fprintf(&b, "hub_http_requests_total{path=%q} %d\n", "other", overflow)
	}
	if len(paths) == 0 && overflow == 0 {
		// 没有样本时也输出一行 0（带聚合标签），使「HELP/TYPE/值」三段齐全，
		// 抓取端与测试都能稳定看到这一族存在。
		fmt.Fprintf(&b, "hub_http_requests_total{path=%q} %d\n", "other", 0)
	}

	// 插件运行时长：用 StartedAt（running）或 StoppedAt-StartedAt（已停止）。
	b.WriteString("# HELP hub_plugin_uptime_seconds 插件运行时长（秒）\n")
	b.WriteString("# TYPE hub_plugin_uptime_seconds gauge\n")
	if p.mgr != nil {
		now := time.Now().UnixMilli()
		for _, info := range p.mgr.Snapshot() {
			if info.StartedAt <= 0 {
				continue
			}
			var ms int64
			if info.StoppedAt > 0 {
				ms = info.StoppedAt - info.StartedAt
			} else {
				ms = now - info.StartedAt
			}
			if ms < 0 {
				ms = 0
			}
			fmt.Fprintf(&b, "hub_plugin_uptime_seconds{plugin=%q} %.3f\n", info.Name, float64(ms)/1000)
		}
	}

	writeCounter(&b, "hub_events_dropped_total", "因异步队列满被丢弃的事件数", float64(eventsDropped))

	sseClients, sseDropped := 0, int64(0)
	if c, d, ok := p.sseStats(); ok {
		sseClients = c
		sseDropped = d
	}
	writeGauge(&b, "hub_sse_clients", "当前 SSE 连接数", float64(sseClients))
	writeCounter(&b, "hub_sse_events_dropped_total", "因慢客户端被丢弃的 SSE 事件数", float64(sseDropped))

	return b.String()
}

// sseStats 从共享 KV 读取 realtime 插件的统计（未启用 realtime 时 ok=false）。
func (p *Metrics) sseStats() (clients int, dropped int64, ok bool) {
	if p.ctx == nil {
		return 0, 0, false
	}
	v, found := p.ctx.GetShared(sseSharedKey)
	if !found {
		return 0, 0, false
	}
	fn, isFn := v.(func() (int, int64))
	if !isFn || fn == nil {
		return 0, 0, false
	}
	c, d := fn()
	return c, d, true
}

// writeGauge 写一个 gauge 族（# HELP / # TYPE / 值）。
func writeGauge(b *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
	fmt.Fprintf(b, "%s %s\n", name, formatFloat(value))
}

// writeCounter 写一个 counter 族（# HELP / # TYPE / 值）。
func writeCounter(b *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s %s\n", name, formatFloat(value))
}

// formatFloat 以最少字符输出数值（整数不带小数点，便于人读与逐行解析）。
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// ---------------------------------------------------------------------------
// /api/metrics/summary
// ---------------------------------------------------------------------------

// summaryResponse 是 /api/metrics/summary 的响应体。
type summaryResponse struct {
	UptimeMs          int64    `json:"uptimeMs"`
	AgentsOnline      int      `json:"agentsOnline"`
	Rooms             int      `json:"rooms"`
	MessagesTotal     int64    `json:"messagesTotal"`
	MessagesPerSecond float64  `json:"messagesPerSecond"`
	HTTPRequestsTotal int64    `json:"httpRequestsTotal"`
	EventsDropped     int64    `json:"eventsDropped"`
	SSEClients        int      `json:"sseClients"`
	IntervalMs        int64    `json:"intervalMs"`
	Samples           []Sample `json:"samples"`
}

// handleSummary 输出概览用的趋势与速率。
func (p *Metrics) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := summaryResponse{
		UptimeMs:          p.uptime(),
		MessagesTotal:     p.msgTotal.Load(),
		HTTPRequestsTotal: p.httpTotal.Load(),
		IntervalMs:        p.interval().Milliseconds(),
		Samples:           p.snapshotSamples(),
	}
	if p.hub != nil {
		resp.AgentsOnline = len(p.hub.ListAgentsDetailed())
		resp.Rooms = len(p.hub.Rooms.List())
		resp.EventsDropped = p.hub.Bus.Dropped()
	}
	if c, _, ok := p.sseStats(); ok {
		resp.SSEClients = c
	}
	resp.MessagesPerSecond = ratePerSecond(resp.Samples)
	writeJSON(w, resp)
}

// ratePerSecond 由采样点计算消息速率（消息数增量 / 时间增量）。
// 少于两个采样点时返回 0（而不是拿总量除以运行时长——那会把历史平均当当前速率）。
func ratePerSecond(samples []Sample) float64 {
	if len(samples) < 2 {
		return 0
	}
	first, last := samples[0], samples[len(samples)-1]
	dtMs := last.TS - first.TS
	if dtMs <= 0 {
		return 0
	}
	delta := last.MessagesTotal - first.MessagesTotal
	if delta < 0 {
		return 0
	}
	return float64(delta) / (float64(dtMs) / 1000.0)
}

// writeJSON 输出 JSON（风格与 httpgateway / plugininfo 一致）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Metrics{})
}
