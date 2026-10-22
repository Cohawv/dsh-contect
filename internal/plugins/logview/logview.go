// Package logview 是「服务端日志环形缓冲 + 网页查看」插件（V5 ①）。
//
// 设计要点：
//   - 它是 plugin.LogSink 的实现者：框架把 log 输出**额外**分发给本插件，
//     lan-agent-hub.log 的落盘行为完全不变（EXE 无窗口场景仍能排障）；
//   - 缓冲是**定长环形**：写满后覆盖最旧的一条，内存不随运行时长增长；
//   - 端点只读：/api/logs 查询、/api/logs/stream 增量流（SSE），不做任何写操作；
//   - 容量可配（capacity，默认 1000）且支持热更新（实现 plugin.Applier）。
//
// 本包不 import internal/core：它只需要「接收日志 + 提供端点」两件事。
package logview

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径（Go 1.21 的 ServeMux 不支持路径参数，故全部走 query）。
const (
	pathLogs       = "/api/logs"
	pathLogsStream = "/api/logs/stream"
)

// 默认与边界取值。
const (
	defaultCapacity = 1000
	minCapacity     = 16
	maxCapacity     = 100_000

	defaultLimit = 100
	maxLimit     = 1000

	// subscriberQueue 是每个 SSE 订阅者的有界队列：满则丢弃并计数，绝不阻塞日志写入。
	subscriberQueue = 64
	// streamHeartbeat 是 SSE 心跳注释行间隔（防中间设备掐断空闲连接）。
	streamHeartbeat = 25 * time.Second
)

// Entry 是一条日志记录（对外 JSON 结构，控制台直接渲染）。
type Entry struct {
	Seq   int64  `json:"seq"`   // 单调递增序号（SSE 的 Last-Event-ID 游标）
	TS    int64  `json:"ts"`    // 毫秒时间戳
	Level string `json:"level"` // info | warn | error
	Msg   string `json:"msg"`   // 日志正文（已剥离 log 包时间前缀）
}

// subscriber 是一个 SSE 订阅连接。
//
// 用 done 通道而非关闭数据通道来退出：关闭慢消费者正在写入的 channel 会 panic，
// 而 done 广播是安全的（offer 侧 select 到 done 就跳过）。
type subscriber struct {
	ch      chan Entry
	done    chan struct{}
	dropped atomic.Int64
}

// LogView 是 logview 插件实体。
type LogView struct {
	mu       sync.RWMutex
	capacity int
	buf      []Entry // 定长环形：长度恒等于 capacity（首次写入后）
	head     int     // 下一次写入位置
	size     int     // 当前有效条数（<= capacity）
	seq      int64   // 已写入总条数（单调，不因覆盖而回退）

	subMu   sync.Mutex
	subs    map[int]*subscriber
	nextSub int
	closed  chan struct{} // Stop 时关闭：让所有在途 SSE handler 立即退出

	// droppedSub 累计「因订阅者队列满而丢弃」的条数（供 /api/logs 与 metrics 观测）。
	droppedSub atomic.Int64
}

// Meta 声明插件元信息。
// 依赖 httpgateway：本插件不自建监听端口，端点由共享 mux 对外服务。
func (p *LogView) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "logview",
		Version:      "1.0.0",
		Description:  "服务端日志环形缓冲与网页查看（/api/logs、/api/logs/stream）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
func (p *LogView) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "capacity",
			Type:        plugin.OptionInt,
			Default:     defaultCapacity,
			Description: "环形缓冲容量（条）。写满后覆盖最旧的一条，内存不随运行时长增长；支持热更新",
			Min:         minCapacity,
			Max:         maxCapacity,
		},
	}
}

// Init 读取容量配置并注册两个端点。
func (p *LogView) Init(ctx *plugin.Context) error {
	capacity := ctx.Int("capacity", defaultCapacity)
	if capacity < minCapacity {
		capacity = minCapacity
	}
	if capacity > maxCapacity {
		capacity = maxCapacity
	}

	p.mu.Lock()
	if p.buf == nil {
		p.buf = make([]Entry, capacity)
		p.capacity = capacity
		p.head = 0
		p.size = 0
	} else {
		p.resizeLocked(capacity)
	}
	p.mu.Unlock()

	p.subMu.Lock()
	if p.subs == nil {
		p.subs = make(map[int]*subscriber)
	}
	if p.closed == nil {
		p.closed = make(chan struct{})
	}
	p.subMu.Unlock()

	if err := ctx.RegisterHTTP(pathLogs, p.handleLogs); err != nil {
		return fmt.Errorf("logview: %w", err)
	}
	if err := ctx.RegisterHTTP(pathLogsStream, p.handleStream); err != nil {
		return fmt.Errorf("logview: %w", err)
	}
	return nil
}

// Start 无副作用：端点由共享 mux 承载，本插件不持有端口/协程。
func (p *LogView) Start() error { return nil }

// Stop 让所有在途 SSE 连接立即退出（否则插件停用后连接仍挂着，形成泄漏）。
// 缓冲区保留（重新启用后仍能看到停用前的日志，对排障更有用）。
func (p *LogView) Stop() error {
	p.subMu.Lock()
	if p.closed != nil {
		close(p.closed) // 只关一次：closed 置 nil 后再调用就是无操作
		p.closed = nil
	}
	p.subMu.Unlock()
	return nil
}

// Apply 实现 plugin.Applier：热更新容量（保留最新的若干条）。
func (p *LogView) Apply(options map[string]any) error {
	raw, ok := options["capacity"]
	if !ok {
		return nil
	}
	var capacity int
	switch v := raw.(type) {
	case float64:
		capacity = int(v)
	case int:
		capacity = v
	case int64:
		capacity = int(v)
	default:
		return errors.New("logview: capacity 选项不是数字")
	}
	if capacity < minCapacity || capacity > maxCapacity {
		return fmt.Errorf("logview: capacity 越界（%d，允许 %d..%d）", capacity, minCapacity, maxCapacity)
	}
	p.mu.Lock()
	p.resizeLocked(capacity)
	p.mu.Unlock()
	return nil
}

// Write 实现 plugin.LogSink：把一行日志写入环形缓冲，并推给在线订阅者。
//
// 非阻塞保证：订阅者投递一律 select+default，队列满就丢弃并计数。
// 即使 Init 尚未执行（容量为 0），这里也会按默认容量懒建缓冲区——
// 日志的第一行不该因为「装配顺序」而丢失。
func (p *LogView) Write(level, msg string, ts time.Time) {
	p.mu.Lock()
	if p.capacity <= 0 || len(p.buf) != p.capacity {
		p.capacity = defaultCapacity
		p.buf = make([]Entry, p.capacity)
		p.head = 0
		p.size = 0
	}
	p.seq++
	entry := Entry{Seq: p.seq, TS: ts.UnixMilli(), Level: level, Msg: msg}
	p.buf[p.head] = entry
	p.head = (p.head + 1) % p.capacity
	if p.size < p.capacity {
		p.size++
	}
	p.mu.Unlock()

	// 投递在锁外做：订阅者 offer 是非阻塞的，但仍避免长时间持锁。
	p.fanout(entry)
}

// fanout 把一条新日志推给所有订阅者（非阻塞）。
func (p *LogView) fanout(entry Entry) {
	p.subMu.Lock()
	defer p.subMu.Unlock()
	for _, s := range p.subs {
		select {
		case s.ch <- entry:
		case <-s.done:
			// 已注销：跳过
		default:
			s.dropped.Add(1)
			p.droppedSub.Add(1)
		}
	}
}

// QueryOptions 是 /api/logs 的查询条件（导出便于单测）。
type QueryOptions struct {
	Level string // "" / info / warn / error；warn 表示 warn 及以上
	Limit int
	Since int64 // 毫秒时间戳；>0 时只返回 ts > since 的条目
}

// Query 返回按时间正序排列的日志副本（最多 limit 条，取**最新**的那批）。
func (p *LogView) Query(opt QueryOptions) []Entry {
	minRank := levelRank(opt.Level)
	limit := opt.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.capacity <= 0 || p.size == 0 {
		return []Entry{}
	}

	// 从旧到新遍历环形，收集满足条件的条目；数量到 limit 后滚动覆盖（保留最新的 limit 条）。
	out := make([]Entry, 0, limit)
	for i := 0; i < p.size; i++ {
		idx := (p.head - p.size + i + p.capacity) % p.capacity
		e := p.buf[idx]
		if levelRank(e.Level) < minRank {
			continue
		}
		if opt.Since > 0 && e.TS <= opt.Since {
			continue
		}
		if len(out) < limit {
			out = append(out, e)
			continue
		}
		copy(out, out[1:])
		out[limit-1] = e
	}
	return out
}

// Stats 返回缓冲区统计（供 /api/logs 响应与 metrics 使用）。
func (p *LogView) Stats() (capacity, size int, total int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.capacity, p.size, p.seq
}

// Dropped 返回因订阅者队列满而丢弃的日志条数。
func (p *LogView) Dropped() int64 { return p.droppedSub.Load() }

// resizeLocked 重建缓冲区（保留最新 min(旧条数, 新容量) 条），调用方需持 p.mu。
func (p *LogView) resizeLocked(capacity int) {
	if capacity < minCapacity {
		capacity = minCapacity
	}
	if capacity > maxCapacity {
		capacity = maxCapacity
	}
	keep := p.size
	if keep > capacity {
		keep = capacity
	}
	next := make([]Entry, capacity)
	if p.capacity > 0 && len(p.buf) == p.capacity {
		for i := 0; i < keep; i++ {
			// 取最近 keep 条（从旧到新填充到新环的 0..keep-1）。
			srcIdx := (p.head - keep + i + p.capacity) % p.capacity
			next[i] = p.buf[srcIdx]
		}
	}
	p.buf = next
	p.capacity = capacity
	p.head = keep % capacity
	p.size = keep
}

// ---------------------------------------------------------------------------
// HTTP 端点
// ---------------------------------------------------------------------------

// logsResponse 是 /api/logs 的响应体。
type logsResponse struct {
	Count    int     `json:"count"`
	Limit    int     `json:"limit"`
	Level    string  `json:"level"`
	Capacity int     `json:"capacity"`
	Total    int64   `json:"total"`
	Dropped  int64   `json:"dropped"`
	Entries  []Entry `json:"entries"`
}

// handleLogs 处理 GET /api/logs?level=&limit=&since=。
func (p *LogView) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	level := strings.ToLower(strings.TrimSpace(q.Get("level")))
	if level != "" && levelRank(level) == 0 {
		http.Error(w, "level must be one of info|warn|error", http.StatusBadRequest)
		return
	}
	limit := defaultLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "limit must be an integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	var since int64
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "since must be a millisecond timestamp", http.StatusBadRequest)
			return
		}
		since = n
	}

	entries := p.Query(QueryOptions{Level: level, Limit: limit, Since: since})
	capacity, _, total := p.Stats()
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	writeJSON(w, logsResponse{
		Count:    len(entries),
		Limit:    limit,
		Level:    level,
		Capacity: capacity,
		Total:    total,
		Dropped:  p.Dropped(),
		Entries:  entries,
	})
}

// handleStream 处理 GET /api/logs/stream：SSE 增量日志流。
//
// 帧格式与 realtime 插件保持一致：event/data/id 三段 + 心跳注释行。
// Last-Event-ID（浏览器 EventSource 断线重连时自动带上）用于补发其后的日志。
func (p *LogView) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	minRank := levelRank(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level"))))
	lastID := lastEventID(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub, replay, closed := p.subscribe(lastID, minRank)

	// 先补发：replay 与「订阅已生效后到达的实时日志」按 Seq 去重合并后下发，
	// 消除注册→快照窗口内写入日志被重复下发的帧（P2-1）。d 跨补发与实时两阶段共享。
	d := &seqDedup{last: lastID}
	merged := p.mergeReplayChannel(sub, replay, d)
	for _, e := range merged {
		if !writeLogFrame(w, e) {
			p.unsubscribe(sub)
			return
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(streamHeartbeat)
	defer ticker.Stop()
	defer p.unsubscribe(sub)

	for {
		select {
		case <-r.Context().Done(): // 客户端断开
			return
		case <-closed: // 插件停用
			return
		case e, ok := <-sub.ch:
			if !ok {
				return
			}
			if levelRank(e.Level) < minRank {
				continue
			}
			// 去重：replay 与通道可能含同一 Seq（窗口内写入），只下发一次。
			if !d.accept(e.Seq) {
				continue
			}
			if !writeLogFrame(w, e) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := w.Write([]byte(":ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// subscribe 注册订阅者，并快照需要补发的历史日志（Last-Event-ID 游标之后）。
//
// 注意：注册（持 subMu）与快照（持 mu）是**两个独立临界区**——刻意不合并，
// 因为若持 mu 横跨注册，会与热路径 Write 的 mu.Lock 互斥、阻塞日志写入。
// 代价是「注册 → 快照」之间存在一个窗口：窗口内新写入的日志会**同时**进入
// replay（它在 buf 里，Seq>lastID）与 sub.ch（订阅已生效），造成**重复帧**而非丢行。
// 去重交由接收侧按 Seq 完成（见 mergeReplayChannel / handleStream 的 seqDedup）。
func (p *LogView) subscribe(lastID int64, minRank int) (*subscriber, []Entry, <-chan struct{}) {
	sub := &subscriber{ch: make(chan Entry, subscriberQueue), done: make(chan struct{})}

	p.subMu.Lock()
	if p.subs == nil {
		p.subs = make(map[int]*subscriber)
	}
	id := p.nextSub
	p.nextSub++
	p.subs[id] = sub
	p.subMu.Unlock()

	var replay []Entry
	if lastID > 0 {
		p.mu.RLock()
		for i := 0; i < p.size; i++ {
			idx := (p.head - p.size + i + p.capacity) % p.capacity
			e := p.buf[idx]
			if e.Seq > lastID && levelRank(e.Level) >= minRank {
				replay = append(replay, e)
			}
		}
		p.mu.RUnlock()
	}

	closed := p.closedCh()
	return sub, replay, closed
}

// seqDedup 是按 Seq 去重的下发游标。
//
// accept 仅在 seq 严格大于「已发送最大值」时返回 true，并把它记为新的最大值。
// 用于 SSE 下发，消除「replay 与实时通道重复同一条日志」造成的重复帧（P2-1）。
type seqDedup struct {
	last int64
}

func (s *seqDedup) accept(seq int64) bool {
	if seq <= s.last {
		return false
	}
	s.last = seq
	return true
}

// mergeReplayChannel 把「补发快照 replay」与「订阅者通道中已到达的实时日志」按 Seq 去重合并。
//
// d 是跨「补发阶段 → 实时阶段」共享的去重游标（初始 last=lastID）。replay 与 sub.ch
// 中可能同时含同一 Seq（注册→快照窗口内写入的日志会同时进入两者），必须去重。
// 通道仅在本次调用内非阻塞排空一次；之后的实时帧由 handleStream 的 select 循环继续读取。
func (p *LogView) mergeReplayChannel(sub *subscriber, replay []Entry, d *seqDedup) []Entry {
	out := make([]Entry, 0, len(replay)+len(sub.ch))
	for _, e := range replay {
		if d.accept(e.Seq) {
			out = append(out, e)
		}
	}
	for {
		select {
		case e := <-sub.ch:
			if d.accept(e.Seq) {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// unsubscribe 注销订阅者（幂等，可安全重复调用）。
func (p *LogView) unsubscribe(sub *subscriber) {
	if sub == nil {
		return
	}
	p.subMu.Lock()
	for id, s := range p.subs {
		if s == sub {
			delete(p.subs, id)
			break
		}
	}
	p.subMu.Unlock()
	select {
	case <-sub.done:
	default:
		close(sub.done)
	}
}

// closedCh 返回停止信号通道（未初始化时返回一个永不关闭的通道）。
func (p *LogView) closedCh() <-chan struct{} {
	p.subMu.Lock()
	defer p.subMu.Unlock()
	if p.closed == nil {
		return neverClosed
	}
	return p.closed
}

// neverClosed 是一个永不关闭的通道（插件尚未 Start/Stop 时的兜底）。
var neverClosed = make(chan struct{})

// writeLogFrame 写一帧 SSE 日志事件，返回是否仍然可写。
func writeLogFrame(w http.ResponseWriter, e Entry) bool {
	data, err := json.Marshal(e)
	if err != nil {
		return true // 序列化失败只跳过这一条，不断开连接
	}
	_, werr := fmt.Fprintf(w, "event: log\ndata: %s\nid: %d\n\n", data, e.Seq)
	return werr == nil
}

// lastEventID 取 Last-Event-ID（优先 header，其次 query 便于手工调试）。
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

// levelRank 把级别映射成可比较的序号：info=1 < warn=2 < error=3，未知=0。
func levelRank(level string) int {
	switch level {
	case plugin.LevelInfo:
		return 1
	case plugin.LevelWarn:
		return 2
	case plugin.LevelError:
		return 3
	default:
		return 0
	}
}

// writeJSON 输出 JSON（风格与 httpgateway / plugininfo 一致：带协议版本头）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&LogView{})
}
