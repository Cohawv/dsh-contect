// Package realtime 是「SSE 实时推送」插件（V5 ②）。
//
// 传输选择：**Server-Sent Events，不写 WebSocket**。
// 本项目只需要「服务端 → 控制台」单向推数据，SSE 与需求完全一致：
//   - 实现只需标准库（text/event-stream + http.Flusher），无手写帧/掩码/分片风险面；
//   - 浏览器 EventSource 自带断线重连与 Last-Event-ID，前端零依赖；
//   - 心跳用注释行 `:ping` 即可，不需要 ping/pong 协议。
//
// 可靠性设计（对应 V5 风险 RK2/RK4/RK5）：
//   - 连接上限 maxClients（默认 64），超限返回 503（RK2）；
//   - 每连接有界队列（64 事件），满则丢弃并计数，**绝不阻塞发布方**（RK3）；
//   - 服务端保留最近 100 条事件，客户端带 Last-Event-ID 时补发后续事件；
//   - 25s 心跳注释行 + X-Accel-Buffering: no，防代理缓冲/掐断空闲连接（RK5）；
//   - r.Context().Done() 到达即退订释放，无 goroutine 泄漏。
package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径。
const (
	pathStream      = "/api/stream"
	pathStreamStats = "/api/stream/stats"
)

// 默认与边界取值。
const (
	defaultMaxClients = 64
	maxClientsLimit   = 512

	defaultHeartbeat     = 25 * time.Second
	minHeartbeat         = 100 * time.Millisecond
	defaultStatsInterval = 5 * time.Second
	minStatsInterval     = 100 * time.Millisecond

	// historySize 是补发环形缓冲的条数（Last-Event-ID 只能补最近这么多条）。
	historySize = 100
	// clientQueue 是每连接的事件队列长度：慢客户端满则丢弃，绝不阻塞发布方。
	clientQueue = 64
	// busQueue 是总线异步订阅队列长度：本插件处理不过来时由总线侧丢弃并计数。
	busQueue = 256
)

// statsTickTopic 是「低频兜底」统计事件，由本插件自己按 statsInterval 产生。
// 前端用它滚动更新概览数字（即使没有 agent 上下线，数字也不会长期不动）。
const statsTickTopic = "stats.tick"

// defaultTopics 是客户端不指定 topics 时的订阅集合（不含 msg.routed / http.request：
// 它们频率高、对界面无意义，仅用于 metrics 计数）。
var defaultTopics = []string{
	protocol.TopicAgentOnline,
	protocol.TopicAgentOffline,
	protocol.TopicRoomChanged,
	plugin.TopicState,
	statsTickTopic,
}

// event 是一条待推送的事件（id 即 SSE 的 id: 段，用于 Last-Event-ID 补发）。
type event struct {
	Seq   int64           `json:"seq"`
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
}

// client 是一条 SSE 连接。
type client struct {
	id      int
	ch      chan event
	filter  topicFilter
	dropped atomic.Int64
}

// Realtime 是 realtime 插件实体。
//
// 三个可热更新的配置项都用 atomic 持有：handleStream 在每条请求上读取它们，
// 用普通字段 + 写锁会在 -race 下暴露竞争（而且热更新不该让每条连接抢锁）。
type Realtime struct {
	hub *core.Hub

	maxClients  atomic.Int64 // 连接上限
	heartbeatNs atomic.Int64 // 心跳间隔（纳秒）
	statsNs     atomic.Int64 // stats.tick 间隔（纳秒）

	mu           sync.Mutex
	seq          int64
	history      []event // 定长环形（最近 historySize 条）
	historyStart int
	clients      map[int]*client
	nextClientID int

	active  atomic.Int64
	sent    atomic.Int64
	dropped atomic.Int64 // 慢客户端丢弃总数

	closed chan struct{}
	cancel context.CancelFunc
	unsubs []func()
}

// Meta 声明插件元信息。依赖 httpgateway（复用其共享 mux，本插件不自建端口）。
func (p *Realtime) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "realtime",
		Version:      "1.0.0",
		Description:  "SSE 实时推送（/api/stream，含心跳/补发/连接上限/慢客户端隔离）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项。
func (p *Realtime) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "maxClients",
			Type:        plugin.OptionInt,
			Default:     defaultMaxClients,
			Description: "最大并发 SSE 连接数，超限返回 503；支持热更新",
			Min:         1,
			Max:         maxClientsLimit,
		},
		{
			Key:         "heartbeat",
			Type:        plugin.OptionDuration,
			Default:     defaultHeartbeat.String(),
			Description: "心跳注释行（:ping）间隔，用于防止中间设备掐断空闲长连接；支持热更新",
		},
		{
			Key:         "statsInterval",
			Type:        plugin.OptionDuration,
			Default:     defaultStatsInterval.String(),
			Description: "stats.tick 事件间隔（在线数/房间数低频兜底推送）；支持热更新",
		},
	}
}

// Init 取内核、解析配置并注册两个端点。
func (p *Realtime) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("realtime: missing *core.Hub in context")
	}
	p.hub = hub
	clients := ctx.Int("maxClients", defaultMaxClients)
	if clients < 1 {
		clients = 1
	}
	if clients > maxClientsLimit {
		clients = maxClientsLimit
	}
	p.maxClients.Store(int64(clients))

	heartbeat := ctx.Duration("heartbeat", defaultHeartbeat)
	if heartbeat < minHeartbeat {
		heartbeat = minHeartbeat
	}
	p.heartbeatNs.Store(int64(heartbeat))

	statsEvery := ctx.Duration("statsInterval", defaultStatsInterval)
	if statsEvery < minStatsInterval {
		statsEvery = minStatsInterval
	}
	p.statsNs.Store(int64(statsEvery))

	p.mu.Lock()
	if p.clients == nil {
		p.clients = make(map[int]*client)
	}
	p.mu.Unlock()

	if err := ctx.RegisterHTTP(pathStream, p.handleStream); err != nil {
		return fmt.Errorf("realtime: %w", err)
	}
	if err := ctx.RegisterHTTP(pathStreamStats, p.handleStats); err != nil {
		return fmt.Errorf("realtime: %w", err)
	}
	// 把 SSE 统计放进共享 KV，供 metrics 插件读取（metrics 因此无需 import 本包）。
	ctx.SetShared(SharedKey, p.sseCounters)
	return nil
}

// SharedKey 是 realtime 在跨插件共享 KV 中的键。
//
// 值是 func() (clients int, dropped int64) —— 刻意用**函数值**而不是 *Realtime：
//   - metrics 插件断言一个纯函数签名即可，不必 import 本包（插件之间零耦合，
//     删掉 realtime 这行 import 不会让 metrics 编译不过）；
//   - 共享的是"只读计数"，不是插件实例，接口面极小。
const SharedKey = "realtime.sse"

// sseCounters 返回 (当前 SSE 连接数, 累计丢弃事件数)。
func (p *Realtime) sseCounters() (int, int64) {
	return int(p.active.Load()), p.dropped.Load()
}

// Start 订阅事件总线并启动 stats.tick 定时器。
func (p *Realtime) Start() error {
	if p.hub == nil {
		return fmt.Errorf("realtime: hub not initialized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	closed := make(chan struct{})
	p.mu.Lock()
	p.closed = closed
	p.mu.Unlock()

	// 全部走 SubscribeAsync：慢消费（HTTP 写阻塞）绝不能反压到消息热路径。
	for _, topic := range p.busTopics() {
		topic := topic
		p.unsubs = append(p.unsubs, p.hub.Bus.SubscribeAsync(topic,
			func(t string, payload any) { p.broadcast(t, payload) }, busQueue))
	}

	go p.statsLoop(ctx, closed)
	return nil
}

// statsLoop 周期产生 stats.tick；每轮重新读取间隔，使热更新立即生效。
func (p *Realtime) statsLoop(ctx context.Context, closed <-chan struct{}) {
	for {
		timer := time.NewTimer(p.statsInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-closed:
			timer.Stop()
			return
		case <-timer.C:
			p.broadcast(statsTickTopic, p.statsPayload())
		}
	}
}

// statsInterval 返回 stats.tick 间隔（热更新安全）。
func (p *Realtime) statsInterval() time.Duration {
	d := time.Duration(p.statsNs.Load())
	if d < minStatsInterval {
		return defaultStatsInterval
	}
	return d
}

// heartbeatInterval 返回心跳间隔（热更新安全）。
func (p *Realtime) heartbeatInterval() time.Duration {
	d := time.Duration(p.heartbeatNs.Load())
	if d < minHeartbeat {
		return defaultHeartbeat
	}
	return d
}

// Stop 取消订阅、关闭所有在途连接并等待定时器退出。
func (p *Realtime) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
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

// Apply 实现 plugin.Applier：热更新 maxClients / heartbeat / statsInterval。
//
// 说明：心跳间隔在**每条连接建立时**读取（见 handleStream），因此热更新只影响新连接；
// 已建立的连接继续用旧节奏心跳——这比"为了改心跳重连所有客户端"温和得多。
func (p *Realtime) Apply(options map[string]any) error {
	if v, ok := options["maxClients"]; ok {
		n := toInt(v, int(p.maxClients.Load()))
		if n < 1 || n > maxClientsLimit {
			return fmt.Errorf("realtime: maxClients 越界（%d，允许 1..%d）", n, maxClientsLimit)
		}
		p.maxClients.Store(int64(n))
	}
	if v, ok := options["heartbeat"]; ok {
		d, err := toDuration(v, p.heartbeatInterval())
		if err != nil || d < minHeartbeat {
			return fmt.Errorf("realtime: heartbeat 不是合法正时长")
		}
		p.heartbeatNs.Store(int64(d))
	}
	if v, ok := options["statsInterval"]; ok {
		d, err := toDuration(v, p.statsInterval())
		if err != nil || d < minStatsInterval {
			return fmt.Errorf("realtime: statsInterval 不是合法正时长")
		}
		p.statsNs.Store(int64(d))
	}
	return nil
}

// busTopics 返回需要订阅的总线 topic（不含本插件自产的 stats.tick）。
//
// 注意：msg.content 已加入订阅列表，但**不在 defaultTopics** 里——
// 只有显式带 ?topics=msg.content 的客户端才会收到聊天正文，默认订阅集合不含它（隐私默认关）。
func (p *Realtime) busTopics() []string {
	return []string{
		protocol.TopicAgentOnline,
		protocol.TopicAgentOffline,
		protocol.TopicRoomChanged,
		protocol.TopicMsgContent,
		plugin.TopicState,
	}
}

// broadcast 把一条总线事件转成 SSE 事件并分发给匹配的客户端。
//
// 全程持锁：分配 seq、写历史环、投递给所有客户端必须是原子的，
// 否则 Last-Event-ID 补发的边界会与在线推送交叉导致重复或漏发。
// 投递是非阻塞 select，所以持锁时间极短。
func (p *Realtime) broadcast(topic string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = json.RawMessage("null")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.seq++
	ev := event{Seq: p.seq, Topic: topic, Data: data}
	p.pushHistoryLocked(ev)

	for _, c := range p.clients {
		if !c.filter.match(topic) {
			continue
		}
		select {
		case c.ch <- ev:
			p.sent.Add(1)
		default:
			// 慢客户端：丢弃这一条并计数（绝不阻塞发布方——这是 RK3 的核心对策）。
			c.dropped.Add(1)
			p.dropped.Add(1)
		}
	}
}

// pushHistoryLocked 把事件写入补发环形缓冲（调用方需持 p.mu）。
func (p *Realtime) pushHistoryLocked(ev event) {
	if len(p.history) < historySize {
		p.history = append(p.history, ev)
		return
	}
	p.history[p.historyStart] = ev
	p.historyStart = (p.historyStart + 1) % historySize
}

// historySinceLocked 返回 seq > lastID 的事件（按 seq 正序）。
func (p *Realtime) historySinceLocked(lastID int64) []event {
	out := make([]event, 0, len(p.history))
	for i := 0; i < len(p.history); i++ {
		idx := i
		if len(p.history) == historySize {
			idx = (p.historyStart + i) % historySize
		}
		if p.history[idx].Seq > lastID {
			out = append(out, p.history[idx])
		}
	}
	return out
}

// Stats 是本插件对外暴露的运行统计（供 /api/stream/stats 与 metrics）。
type Stats struct {
	Clients     int   `json:"clients"`
	MaxClients  int   `json:"maxClients"`
	Sent        int64 `json:"sent"`
	Dropped     int64 `json:"dropped"`
	LastSeq     int64 `json:"lastSeq"`
	HistorySize int   `json:"historySize"`
	HistoryCap  int   `json:"historyCapacity"`
	HeartbeatMs int64 `json:"heartbeatMs"`
	StatsTickMs int64 `json:"statsIntervalMs"`
}

// Stats 返回当前统计（实现需与 handleStats 输出一致）。
func (p *Realtime) Stats() Stats {
	p.mu.Lock()
	lastSeq := p.seq
	histLen := len(p.history)
	clients := len(p.clients)
	p.mu.Unlock()
	return Stats{
		Clients:     clients,
		MaxClients:  int(p.maxClients.Load()),
		Sent:        p.sent.Load(),
		Dropped:     p.dropped.Load(),
		LastSeq:     lastSeq,
		HistorySize: histLen,
		HistoryCap:  historySize,
		HeartbeatMs: p.heartbeatInterval().Milliseconds(),
		StatsTickMs: p.statsInterval().Milliseconds(),
	}
}

// statsPayload 是 stats.tick 事件的载荷（在线数/房间数，供概览兜底刷新）。
func (p *Realtime) statsPayload() map[string]any {
	agents, rooms := 0, 0
	if p.hub != nil {
		agents = len(p.hub.ListAgentsDetailed())
		rooms = len(p.hub.Rooms.List())
	}
	return map[string]any{
		"agentCount": agents,
		"roomCount":  rooms,
		"serverTime": time.Now().UnixMilli(),
	}
}

// handleStats 处理 GET /api/stream/stats（控制台顶栏展示当前连接数）。
func (p *Realtime) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, p.Stats())
}

// handleStream 处理 GET /api/stream?topics=a,b,c → text/event-stream。
func (p *Realtime) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// 连接上限：先占坑再写任何 SSE 内容，超限时能干净地返回 503（RK2）。
	if !p.acquire() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many realtime clients", http.StatusServiceUnavailable)
		return
	}
	defer p.release()

	filter := parseTopics(r.URL.Query().Get("topics"))
	heartbeat := p.heartbeatInterval()
	lastID := lastEventID(r)

	// 注册客户端与会话内历史快照必须在同一临界区完成，
	// 否则「注册 → 补发」之间产生的事件会既不在队列里也不在补发列表里（丢事件）。
	c, replay, closedCh := p.addClient(filter, lastID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // 立刻把响应头发出去：客户端 onopen 不必等第一个事件

	defer p.removeClient(c)

	for _, ev := range replay {
		if !writeEvent(w, ev) {
			return
		}
	}
	if len(replay) > 0 {
		flusher.Flush()
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done(): // 客户端断开：退订并释放（无 goroutine 泄漏）
			return
		case <-closedCh: // 插件停用
			return
		case ev := <-c.ch:
			if !writeEvent(w, ev) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// 心跳用 SSE 注释行：浏览器 EventSource 会忽略它，
			// 但代理/网关看到有字节流动就不会判定空闲超时（RK5）。
			if _, err := w.Write([]byte(":ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// addClient 注册一个客户端，并返回注册瞬间的历史快照（seq > lastID）。
func (p *Realtime) addClient(filter topicFilter, lastID int64) (*client, []event, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := p.nextClientID
	p.nextClientID++
	c := &client{id: id, ch: make(chan event, clientQueue), filter: filter}
	if p.clients == nil {
		p.clients = make(map[int]*client)
	}
	p.clients[id] = c

	var replay []event
	if lastID > 0 {
		for _, ev := range p.historySinceLocked(lastID) {
			if filter.match(ev.Topic) {
				replay = append(replay, ev)
			}
		}
	}
	closed := p.closed
	if closed == nil {
		closed = neverClosed
	}
	return c, replay, closed
}

// removeClient 注销客户端（幂等）。
func (p *Realtime) removeClient(c *client) {
	if c == nil {
		return
	}
	p.mu.Lock()
	delete(p.clients, c.id)
	p.mu.Unlock()
}

// acquire 尝试占用一个连接名额（超限返回 false）。
func (p *Realtime) acquire() bool {
	max := p.maxClients.Load()
	for {
		cur := p.active.Load()
		if cur >= max {
			return false
		}
		if p.active.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// release 归还一个连接名额。
func (p *Realtime) release() { p.active.Add(-1) }

// writeEvent 写一帧 SSE 事件：event / data / id 三段 + 空行结束。
func writeEvent(w http.ResponseWriter, ev event) bool {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\nid: %d\n\n", ev.Topic, ev.Data, ev.Seq)
	return err == nil
}

// neverClosed 是「插件未启动」时的兜底通道。
var neverClosed = make(chan struct{})

// topicFilter 是客户端订阅的 topic 过滤器，支持精确名与前缀通配（agent.* / *）。
type topicFilter struct {
	all      bool
	patterns []string
}

// parseTopics 解析 ?topics=a,b,c；空值时用 defaultTopics。
func parseTopics(raw string) topicFilter {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return topicFilter{patterns: append([]string(nil), defaultTopics...)}
	}
	f := topicFilter{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			f.all = true
			continue
		}
		f.patterns = append(f.patterns, part)
	}
	if !f.all && len(f.patterns) == 0 {
		f.patterns = append(f.patterns, defaultTopics...)
	}
	return f
}

// match 判断某 topic 是否命中过滤器。
func (f topicFilter) match(topic string) bool {
	if f.all {
		return true
	}
	for _, pat := range f.patterns {
		if pat == topic {
			return true
		}
		if strings.HasSuffix(pat, ".*") && strings.HasPrefix(topic, pat[:len(pat)-1]) {
			return true
		}
	}
	return false
}

// lastEventID 取 Last-Event-ID（浏览器 EventSource 断线重连时自动带上）；
// query 里的 lastEventId 供手工调试（curl）使用。
func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// toInt 把 any 转成 int（兼容 JSON 的 float64）。
func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

// toDuration 把 any 转成 time.Duration（数字按毫秒、字符串按 ParseDuration）。
func toDuration(v any, def time.Duration) (time.Duration, error) {
	switch n := v.(type) {
	case float64:
		return time.Duration(n) * time.Millisecond, nil
	case int:
		return time.Duration(n) * time.Millisecond, nil
	case int64:
		return time.Duration(n) * time.Millisecond, nil
	case string:
		return time.ParseDuration(n)
	default:
		return def, fmt.Errorf("invalid duration value %v", v)
	}
}

// writeJSON 输出 JSON（风格与 httpgateway / plugininfo 一致）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Realtime{})
}
