// Package healthz 是「健康与就绪探针」插件（V5 ④）。
//
// 三个端点语义分明（沿用 Kubernetes 约定，外部监控可直接对接）：
//
//	GET /healthz             200 {"status":"ok"}   —— 进程活着吗（**极轻**：不读插件状态、不加锁）
//	GET /readyz              200 / 503             —— 能对外服务吗（必需插件全部 running）
//	GET /api/health/detail   200                   —— 诊断详情（各插件状态 + 依赖 + 计数 + 版本）
//
// 探针纪律：
//   - **不记访问日志**：本包不记录任何请求日志（探针高频，记了会把日志缓冲刷爆）；
//   - **不参与访问计数**：三个端点都用 RegisterHTTPUnobserved 注册，
//     避免监控自身把 hub_http_requests_total 刷成噪声；
//   - **不拨号自检**：就绪检查读的是"配置 + 内核可读"，不做 TCP 自连
//     （自连会引入超时抖动，且探针本身成了负载）。
package healthz

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径。
const (
	pathHealthz = "/healthz"
	pathReadyz  = "/readyz"
	pathDetail  = "/api/health/detail"
)

// Healthz 是 healthz 插件实体。
type Healthz struct {
	hub       *core.Hub
	mgr       *plugin.Manager
	startedAt int64 // 本插件启动时间（毫秒），用于 uptimeMs
}

// Meta 声明插件元信息。依赖 httpgateway（复用共享 mux）。
func (p *Healthz) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "healthz",
		Version:      "1.0.0",
		Description:  "健康/就绪探针（/healthz、/readyz）与健康详情（/api/health/detail）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Init 取内核与管理器并注册三个探针端点。
func (p *Healthz) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("healthz: missing *core.Hub in context")
	}
	p.hub = hub
	if mgr, ok := plugin.ManagerFromContext(ctx); ok {
		p.mgr = mgr
	}
	// 探针端点一律「不计访问」：它们会被外部监控高频打，计进去只会污染指标。
	if err := ctx.RegisterHTTPUnobserved(pathHealthz, p.handleHealthz); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	if err := ctx.RegisterHTTPUnobserved(pathReadyz, p.handleReadyz); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	if err := ctx.RegisterHTTPUnobserved(pathDetail, p.handleDetail); err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	return nil
}

// Start 只记录启动时刻（uptimeMs 用）。
func (p *Healthz) Start() error {
	p.startedAt = time.Now().UnixMilli()
	return nil
}

// Stop 无副作用（无自有端口/协程）。
func (p *Healthz) Stop() error { return nil }

// handleHealthz 处理 GET /healthz。
//
// **极轻**实现：不读插件状态、不加锁、不做任何 IO，只拼一个常量 JSON。
// 这样即使某个插件卡住（甚至死锁），存活探针照样 200 —— 存活探针失效会引发
// 无意义的重启风暴，比"探针什么也没查"危险得多。
// ?verbose=1 时才输出明细（明确区分"轻探针"与"诊断"）。
func (p *Healthz) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.URL.Query().Get("verbose"); v == "1" || v == "true" {
		writeJSON(w, http.StatusOK, p.buildDetail())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleReadyz 处理 GET /readyz：必需插件有非 running → 503。
func (p *Healthz) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	checks := p.checks()
	plugins, blockers := p.pluginStates()

	code := http.StatusOK
	status := "ready"
	if len(blockers) > 0 {
		code = http.StatusServiceUnavailable
		status = "not ready"
	}
	// 核心服务不可用同样判为未就绪（HTTP 端口未配置 / 内核不可读）。
	for _, name := range []string{"httpPort", "agents", "rooms"} {
		if c, ok := checks[name]; ok && !c.OK {
			code = http.StatusServiceUnavailable
			status = "not ready"
			break
		}
	}

	writeJSON(w, code, map[string]any{
		"status":   status,
		"checks":   checks,
		"plugins":  plugins,
		"blockers": blockers,
	})
}

// handleDetail 处理 GET /api/health/detail。
func (p *Healthz) handleDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, p.buildDetail())
}

// checkResult 是单项检查结果。
type checkResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// pluginHealth 是单个插件的健康条目。
type pluginHealth struct {
	Name         string   `json:"name"`
	State        string   `json:"state"`
	Required     bool     `json:"required"`
	Error        string   `json:"error,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// detailResponse 是 /api/health/detail（与 /healthz?verbose=1）的响应体。
type detailResponse struct {
	Status   string                 `json:"status"` // ok | degraded
	Plugins  []pluginHealth         `json:"plugins"`
	Checks   map[string]checkResult `json:"checks"`
	UptimeMs int64                  `json:"uptimeMs"`
	Version  string                 `json:"version"`
	Blockers []string               `json:"blockers,omitempty"`
}

// buildDetail 汇总诊断信息。
//
// 状态判定：必需插件全部 running 且无插件 failed → ok；否则 degraded。
// 注意"可选插件失败"也算 degraded（不是不可用）——这让排障能一眼看到
// "某个可选能力挂了，但服务本身还能用"。
func (p *Healthz) buildDetail() detailResponse {
	plugins, blockers := p.pluginStates()
	status := "ok"
	if len(blockers) > 0 {
		status = "degraded"
	} else {
		for _, ph := range plugins {
			if ph.State == plugin.StateFailed.String() {
				status = "degraded"
				break
			}
		}
	}
	var uptime int64
	if p.startedAt > 0 {
		uptime = time.Now().UnixMilli() - p.startedAt
	}
	return detailResponse{
		Status:   status,
		Plugins:  plugins,
		Checks:   p.checks(),
		UptimeMs: uptime,
		Version:  plugin.Version(),
		Blockers: blockers,
	}
}

// pluginStates 返回各插件健康条目，以及「必需但非 running」的插件名（阻塞项）。
func (p *Healthz) pluginStates() ([]pluginHealth, []string) {
	out := make([]pluginHealth, 0, 8)
	var blockers []string
	if p.mgr == nil {
		// 无管理器（单测直连）：返回空清单而不是 panic。
		return out, blockers
	}
	for _, info := range p.mgr.Snapshot() {
		out = append(out, pluginHealth{
			Name:         info.Name,
			State:        info.State.String(),
			Required:     info.Required,
			Error:        info.Error,
			Dependencies: info.Dependencies,
		})
		if info.Required && info.State != plugin.StateRunning {
			blockers = append(blockers, info.Name)
		}
	}
	return out, blockers
}

// checks 返回核心检查项：httpPort / agents / rooms（就绪判定至少含这三项）。
func (p *Healthz) checks() map[string]checkResult {
	checks := make(map[string]checkResult, 4)
	if p.hub == nil {
		checks["httpPort"] = checkResult{OK: false, Detail: "hub 未注入"}
		checks["agents"] = checkResult{OK: false, Detail: "hub 未注入"}
		checks["rooms"] = checkResult{OK: false, Detail: "hub 未注入"}
		return checks
	}

	// HTTP 端口：只判"是否配置了端口"，不做 TCP 自连（见包注释）。
	if port := p.hub.HTTPPort; port > 0 {
		checks["httpPort"] = checkResult{OK: true, Detail: fmt.Sprintf(":%d configured", port)}
	} else {
		checks["httpPort"] = checkResult{OK: false, Detail: "HTTP port not configured"}
	}

	agents := len(p.hub.ListAgentsDetailed())
	checks["agents"] = checkResult{OK: true, Detail: fmt.Sprintf("%d online", agents)}

	rooms := len(p.hub.Rooms.List())
	checks["rooms"] = checkResult{OK: true, Detail: fmt.Sprintf("%d rooms", rooms)}

	return checks
}

// writeJSON 以指定状态码输出 JSON（风格与 httpgateway / plugininfo 一致）。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Healthz{})
}
