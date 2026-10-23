// Package store 的插件包装：生命周期、配置、HTTP 端点与回放逻辑。
//
// 依赖 httpgateway（复用其共享 mux，不自建端口）。默认启用（DefaultOn=true）——
// 持久化是 LAN 场景的高频痛点（离线消息），开箱即用；可通过 hub.json 关闭。
package store

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径。
const (
	pathMessages = "/api/messages"
	pathExport   = "/api/store/export"
	pathVacuum   = "/api/store/vacuum"
)

// 配置默认值。
const (
	defaultPath          = "store"
	defaultMaxReplay     = 500
	defaultFlushEvery    = 1000
	defaultRetentionDays = 0
)

// Store 是 store 插件实体。
type Store struct {
	hub *core.Hub

	path          string
	maxReplay     int64
	flushEvery    int
	retentionDays int

	engine atomic.Pointer[Engine] // 当前存储引擎；Stop 后指向 nil，读写安全
}

// Meta 声明插件元信息。依赖 httpgateway（复用共享 mux）。
func (p *Store) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "store",
		Version:      "1.0.0",
		Description:  "消息持久化与离线回放（纯标准库：追加日志+内存索引+崩溃安全截断；零外部依赖）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项。
func (p *Store) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "path",
			Type:        plugin.OptionString,
			Default:     defaultPath,
			Description: "消息日志存储目录（相对 exe 或绝对）；默认 store/",
		},
		{
			Key:         "maxReplay",
			Type:        plugin.OptionInt,
			Default:     defaultMaxReplay,
			Min:         1,
			Max:         5000,
			Description: "单次离线回放最多补发的消息条数（防止惊群）",
		},
		{
			Key:         "flushEvery",
			Type:        plugin.OptionInt,
			Default:     defaultFlushEvery,
			Min:         1,
			Max:         100000,
			Description: "每追加多少条消息做一次 fsync（批量落盘，平衡耐久与性能）",
		},
		{
			Key:         "retentionDays",
			Type:        plugin.OptionInt,
			Default:     defaultRetentionDays,
			Min:         0,
			Max:         3650,
			Description: "消息保留天数（0 = 永久保留）；Vacuum 时按此淘汰过期消息",
		},
	}
}

// Init 打开存储、挂接核心钩子并注册端点。
func (p *Store) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("store: missing *core.Hub in context")
	}
	p.hub = hub

	p.path = ctx.String("path", defaultPath)
	p.maxReplay = int64(clampInt(ctx.Int("maxReplay", defaultMaxReplay), 1, 5000))
	p.flushEvery = clampInt(ctx.Int("flushEvery", defaultFlushEvery), 1, 100000)
	p.retentionDays = clampInt(ctx.Int("retentionDays", defaultRetentionDays), 0, 3650)

	eng, err := Open(p.path, p.flushEvery, p.retentionDays)
	if err != nil {
		return fmt.Errorf("store: 打开存储失败: %w", err)
	}
	p.engine.Store(eng)

	// 挂接核心钩子（store import core，依赖方向单向；core 不 import store）。
	hub.Router.SetMessageSink(p)
	hub.SetReplayProvider(p.replay)
	hub.SetLastSeqProvider(func() int64 { return eng.LastSeq() })

	// 注册端点（走共享 mux，受运行期启停闸门保护）。
	if err := ctx.RegisterHTTP(pathMessages, p.handleMessages); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := ctx.RegisterHTTP(pathExport, p.handleExport); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := ctx.RegisterHTTP(pathVacuum, p.handleVacuum); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Append 实现 core.MessageSink：把已路由完成的信封持久化。
// 由 core.Router 在消息热路径上同步调用——必须非阻塞、不 panic。
func (p *Store) Append(env protocol.Envelope) {
	eng := p.engine.Load()
	if eng == nil {
		return // 已停用：静默跳过，行为与无 store 一致
	}
	_ = eng.Append(Record{
		Seq:      env.Seq,
		TS:       env.TS,
		Type:     env.Type,
		From:     env.From,
		To:       env.To,
		RoomID:   env.RoomID,
		Content:  string(env.Content),
		Delivered: 0,
	})
}

// replay 实现 core.ReplayProvider：把 seq > sinceSeq 且与 from 相关的消息补发给该会话。
func (p *Store) replay(sess core.Session, from string, sinceSeq int64) {
	eng := p.engine.Load()
	if eng == nil || sess == nil {
		return
	}
	recs := eng.Since(sinceSeq, p.maxReplay)
	if len(recs) == 0 {
		return
	}
	// 成员资格缓存：避免每条消息都查一次 Rooms.Members。
	roomMembers := map[string][]string{}
	memberOf := func(roomID string) []string {
		if m, ok := roomMembers[roomID]; ok {
			return m
		}
		m := p.hub.Rooms.Members(roomID)
		roomMembers[roomID] = m
		return m
	}
	contains := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}

	for _, r := range recs {
		switch {
		case r.RoomID != "": // 房间消息：仅当重连客户端是成员才补发（必须优先于广播判定，
			// 因为房间消息的 To 也是空串，否则会被误判为广播发给所有人）
			if !contains(memberOf(r.RoomID), from) {
				continue
			}
		case r.To == from: // 点对点消息，接收方正是重连的客户端
		case r.To == "": // 广播：离线期间没收到，补发
		default: // 点对点消息且接收方是别人：跳过
			continue
		}
		sess.Deliver(protocol.Envelope{
			Type:    r.Type,
			From:    r.From,
			To:      r.To,
			RoomID:  r.RoomID,
			Content: json.RawMessage(r.Content),
			Seq:     r.Seq,
			TS:      r.TS,
		})
	}
}

// Start 无需后台任务（flush 在 Append 内联完成），返回 nil。
func (p *Store) Start() error {
	if p.engine.Load() == nil {
		return fmt.Errorf("store: engine not initialized")
	}
	return nil
}

// Stop 卸载核心钩子并关闭存储（下次启用会重新 Init 并打开文件）。
func (p *Store) Stop() error {
	if p.hub != nil {
		p.hub.Router.SetMessageSink(nil)
		p.hub.SetReplayProvider(nil)
		p.hub.SetLastSeqProvider(nil)
	}
	if eng := p.engine.Swap(nil); eng != nil {
		return eng.Close()
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新 maxReplay / retentionDays（path / flushEvery 需重启生效）。
func (p *Store) Apply(options map[string]any) error {
	if v, ok := options["maxReplay"]; ok {
		n := clampInt(toInt(v, int(p.maxReplay)), 1, 5000)
		p.maxReplay = int64(n)
	}
	if v, ok := options["retentionDays"]; ok {
		n := clampInt(toInt(v, p.retentionDays), 0, 3650)
		p.retentionDays = n
		if eng := p.engine.Load(); eng != nil {
			eng.retentionDays = n
		}
	}
	return nil
}

// handleMessages 处理 GET /api/messages：查询历史或离线续传。
func (p *Store) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	eng := p.engine.Load()
	if eng == nil {
		http.Error(w, "store not ready", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	limit := parseInt(q.Get("limit"), DefaultLimit)

	var recs []Record
	if since := parseInt64(q.Get("since"), 0); since > 0 {
		recs = eng.Since(since, int64(limit))
	} else {
		recs = eng.Query(Filter{
			From:   q.Get("from"),
			To:     q.Get("to"),
			RoomID: q.Get("room"),
			Limit:  limit,
		})
	}
	writeJSON(w, map[string]any{
		"messages": recs,
		"count":    len(recs),
		"lastSeq":  eng.LastSeq(),
	})
}

// handleExport 处理 GET /api/store/export：导出为 JSONL（默认）或 JSON 数组。
func (p *Store) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	eng := p.engine.Load()
	if eng == nil {
		http.Error(w, "store not ready", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Query().Get("format") == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
		if err := eng.ExportJSONL(w); err != nil {
			// 头已发出，只能记日志（std 输出由 logview 接管）。
			fmt.Fprintf(w, "\n// export error: %v\n", err)
		}
		return
	}
	// 默认：JSON 数组（便于脚本消费）。用 make 而非 nil 切片，保证空时序列化为 [] 而非 null。
	all := make([]Record, 0, len(eng.recs))
	eng.mu.RLock()
	all = append(all, eng.recs...)
	eng.mu.RUnlock()
	writeJSON(w, map[string]any{"messages": all, "count": len(all)})
}

// handleVacuum 处理 POST /api/store/vacuum：压缩存储（可选淘汰过期）。
func (p *Store) handleVacuum(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	eng := p.engine.Load()
	if eng == nil {
		http.Error(w, "store not ready", http.StatusServiceUnavailable)
		return
	}
	res, err := eng.Vacuum()
	if err != nil {
		http.Error(w, "vacuum failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

// ---- 内部工具 ----

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

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

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Store{})
}
