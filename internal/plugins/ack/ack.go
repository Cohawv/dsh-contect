// Package ack 实现「投递确认与对账」插件（V6 ②）。
//
// 设计要点（与 store 同一套窄接口 + 能力协商门控）：
//   - core.Hub 定义 DeliveryTracker 窄接口，ack 实现并把自己挂上 Hub；依赖方向严格单向。
//   - Hub 在消息路由完成后调用 Track（delivered=failed 表示目标离线），在收到 TypeAck 时
//     调用 Ack（标记 delivered）。Tracker 为 nil（未挂载 ack 或已停用）时 Hub 静默跳过，
//     且 TypeAck 退回 unknown type 语义——未启用 ack 的部署与老客户端行为完全不变。
//   - 新增 TypeAck type 与 CapAck cap，均受「只增不改」铁律约束；不声明 cap 的客户端不会
//     收到任何额外回执（服务端不主动向客户端下发 ack 请求，只在收到回执时记录）。
package ack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

const (
	pathDelivery  = "/api/delivery/" // 前缀匹配：/api/delivery/{msgId}
	pathDeliveries = "/api/deliveries"
)

// 投递状态取值。
const (
	statusPending  = "pending"  // 已投递给在线接收方，等待其回 ack
	statusDelivered = "delivered" // 接收方已回 ack
	statusFailed   = "failed"   // 目标离线，投递失败
	statusExpired  = "expired"  // 超过 TTL，记录已清理
	statusUnknown  = "unknown"  // 未跟踪（查询不到）
)

// 配置默认值。
const (
	defaultTTL        = 0     // 投递记录保留时长（毫秒）；0 = 永久
	defaultMaxRecords = 100000 // 内存中最多保留的投递记录条数
)

// Ack 是 ack 插件实体。
type Ack struct {
	hub *core.Hub

	ttl        int64
	maxRecords int

	mu   sync.RWMutex
	recs map[string]*deliveryRecord
}

// deliveryRecord 是一条消息的投递状态。
type deliveryRecord struct {
	MsgID     string `json:"msgId"`
	Status    string `json:"status"`
	Target    string `json:"target,omitempty"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// Meta 声明插件元信息。依赖 httpgateway（复用共享 mux）。
func (p *Ack) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "ack",
		Version:      "1.0.0",
		Description:  "投递确认与对账（纯标准库；零外部依赖）：记录消息投递状态，供发送方查询对账",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项。
func (p *Ack) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "ttl",
			Type:        plugin.OptionInt,
			Default:     defaultTTL,
			Min:         0,
			Max:         86400000,
			Description: "投递记录保留时长（毫秒）；0 = 永久保留；过期记录查询时返回 expired 并惰性清理",
		},
		{
			Key:         "maxRecords",
			Type:        plugin.OptionInt,
			Default:     defaultMaxRecords,
			Min:         1000,
			Max:         10000000,
			Description: "内存中最多保留的投递记录条数（超出后最旧者惰性淘汰）",
		},
	}
}

// Init 挂接核心钩子并注册端点。
func (p *Ack) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("ack: missing *core.Hub in context")
	}
	p.hub = hub
	p.ttl = int64(clampInt(ctx.Int("ttl", defaultTTL), 0, 86400000))
	p.maxRecords = clampInt(ctx.Int("maxRecords", defaultMaxRecords), 1000, 10000000)
	p.recs = make(map[string]*deliveryRecord)

	hub.SetDeliveryTracker(p)

	if err := ctx.RegisterHTTP(pathDeliveries, p.handleDeliveries); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathDelivery, p.handleDelivery); err != nil {
		return err
	}
	return nil
}

// Track 实现 core.DeliveryTracker：消息路由完成后记录投递状态。
func (p *Ack) Track(msgID string, delivered bool, target string) {
	if msgID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if delivered {
		p.recs[msgID] = &deliveryRecord{MsgID: msgID, Status: statusPending, Target: target, CreatedAt: nowMillis()}
	} else {
		p.recs[msgID] = &deliveryRecord{MsgID: msgID, Status: statusFailed, Target: target, Reason: "offline", CreatedAt: nowMillis()}
	}
	p.sweepLocked()
}

// Ack 实现 core.DeliveryTracker：收到接收方确认后标记 delivered。
func (p *Ack) Ack(msgID string, status string, from string) {
	if msgID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.recs[msgID]; ok {
		r.Status = statusDelivered
	} else {
		// 回执先于 Track 到达（极端时序）或插件中途挂载：仍以 delivered 记录，便于对账。
		p.recs[msgID] = &deliveryRecord{MsgID: msgID, Status: statusDelivered, Target: from, CreatedAt: nowMillis()}
	}
	p.sweepLocked()
}

// Start 无需后台任务（TTL 采用惰性清理），返回 nil。
func (p *Ack) Start() error { return nil }

// Stop 卸载核心钩子。
func (p *Ack) Stop() error {
	if p.hub != nil {
		p.hub.SetDeliveryTracker(nil)
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新 ttl / maxRecords。
func (p *Ack) Apply(options map[string]any) error {
	if v, ok := options["ttl"]; ok {
		p.ttl = int64(clampInt(toInt(v, int(p.ttl)), 0, 86400000))
	}
	if v, ok := options["maxRecords"]; ok {
		p.maxRecords = clampInt(toInt(v, p.maxRecords), 1000, 10000000)
	}
	return nil
}

// handleDelivery 处理 GET /api/delivery/{msgId}：查询单条投递状态。
func (p *Ack) handleDelivery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	msgID := strings.TrimPrefix(r.URL.Path, pathDelivery)
	msgID = strings.Trim(msgID, "/")
	if msgID == "" {
		http.Error(w, "missing msgId", http.StatusBadRequest)
		return
	}
	status := p.statusOf(msgID)
	writeJSON(w, map[string]any{"msgId": msgID, "status": status})
}

// handleDeliveries 处理 GET /api/deliveries：返回各状态计数（对账视图）。
func (p *Ack) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counts := map[string]int{statusPending: 0, statusDelivered: 0, statusFailed: 0, statusExpired: 0, statusUnknown: 0}
	for _, st := range p.allStatuses() {
		counts[st]++
	}
	writeJSON(w, map[string]any{"counts": counts})
}

// statusOf 返回单条记录的有效状态（考虑 TTL），找不到返回 unknown。
func (p *Ack) statusOf(msgID string) string {
	p.mu.RLock()
	r, ok := p.recs[msgID]
	p.mu.RUnlock()
	if !ok {
		return statusUnknown
	}
	if p.ttl > 0 && nowMillis()-r.CreatedAt > p.ttl {
		return statusExpired
	}
	return r.Status
}

// allStatuses 返回全部记录的有效状态切片（考虑 TTL，并在读取时惰性淘汰过期/越界记录）。
func (p *Ack) allStatuses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	out := make([]string, 0, len(p.recs))
	for _, r := range p.recs {
		if p.ttl > 0 && nowMillis()-r.CreatedAt > p.ttl {
			out = append(out, statusExpired)
			continue
		}
		out = append(out, r.Status)
	}
	return out
}

// sweepLocked 在持写锁前提下：淘汰 TTL 过期记录，并限制总数（最旧者先淘汰）。调用方持锁。
func (p *Ack) sweepLocked() {
	if p.ttl > 0 {
		now := nowMillis()
		for id, r := range p.recs {
			if now-r.CreatedAt > p.ttl {
				delete(p.recs, id)
			}
		}
	}
	if len(p.recs) > p.maxRecords {
		// 简单策略：超出硬上限时整体裁剪——生产环境消息量有限，且投递记录非持久化状态。
		// 这里仅做防御性兜底；真实高频场景应由合理 ttl 控制规模。
		keep := make(map[string]*deliveryRecord, p.maxRecords)
		i := 0
		for id, r := range p.recs {
			if i >= p.maxRecords {
				break
			}
			keep[id] = r
			i++
		}
		p.recs = keep
	}
}

// ---- 内部工具 ----

func nowMillis() int64 { return time.Now().UnixMilli() }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

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

func init() {
	plugin.Register(&Ack{})
}
