// Package plugininfo 是「管理 API」插件：把插件框架的管理能力（清单 / 开关 / 重载 /
// 设置 / 部署清单）以 REST 端点暴露出来，供网页控制台的「插件」「设置」视图消费。
//
// 设计纪律（V4 ① 与 C2/C7）：
//   - 独立插件，不塞进 webconsole —— 这样停用 webconsole 后 /api/plugins 仍然可用，
//     用户不会因为关掉界面就彻底失去管理入口；
//   - 只通过 plugin.ManagerFromContext 使用框架的导出能力，不 import internal/core；
//   - 响应风格与 httpgateway 一致（Content-Type + X-Proto-Version），错误一律 JSON。
package plugininfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径常量（Go 1.21 的 ServeMux 不支持路径参数，故单插件详情走 query）。
const (
	pathPlugins      = "/api/plugins"
	pathPluginDetail = "/api/plugins/detail"
	pathPluginToggle = "/api/plugins/toggle"
	pathPluginReload = "/api/plugins/reload"
	pathManifest     = "/api/manifest"
	pathSettings     = "/api/settings"
)

// 开关动作取值。
const (
	actionEnable  = "enable"
	actionDisable = "disable"
)

// hubVersion 是二进制版本（由 cmd/hub 通过 SetHubVersion 注入；未注入时为 dev）。
var hubVersion = "dev"

// SetHubVersion 注入二进制版本号，使 /api/manifest 与 -version 输出保持一致。
func SetHubVersion(v string) {
	if strings.TrimSpace(v) != "" {
		hubVersion = v
	}
}

// PluginInfo 是 plugininfo 插件实体。
type PluginInfo struct {
	mu  sync.RWMutex
	mgr *plugin.Manager
}

// Meta 声明插件元信息。
// 依赖 httpgateway：本插件不自建监听端口，端点由 httpgateway 的共享 mux 对外提供服务。
func (p *PluginInfo) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "plugininfo",
		Version:      "1.0.0",
		Description:  "管理 API：插件清单/开关/重载、设置读写与部署清单",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Init 取到框架管理器并注册 6 个管理端点（经 ctx.RegisterHTTP，享受冲突预检与启停闸门）。
func (p *PluginInfo) Init(ctx *plugin.Context) error {
	mgr, ok := plugin.ManagerFromContext(ctx)
	if !ok {
		return errors.New("plugininfo: 共享 KV 中没有插件管理器（BuildContext 未执行？）")
	}
	p.mu.Lock()
	p.mgr = mgr
	p.mu.Unlock()

	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{pathPlugins, p.handlePlugins},
		{pathPluginDetail, p.handlePluginDetail},
		{pathPluginToggle, p.handlePluginToggle},
		{pathPluginReload, p.handlePluginReload},
		{pathManifest, p.handleManifest},
		{pathSettings, p.handleSettings},
	}
	for _, rt := range routes {
		if err := ctx.RegisterHTTP(rt.pattern, rt.handler); err != nil {
			return fmt.Errorf("plugininfo: %w", err)
		}
	}
	return nil
}

// Start 无副作用：端点由共享 mux 承载，本插件不持有端口/协程。
func (p *PluginInfo) Start() error { return nil }

// Stop 无副作用（无自有端口/协程需要回收）。
func (p *PluginInfo) Stop() error { return nil }

// manager 返回框架管理器（未注入时为 nil）。
func (p *PluginInfo) manager() *plugin.Manager {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mgr
}

func init() {
	plugin.Register(&PluginInfo{})
}

// ---------------------------------------------------------------------------
// 响应风格：与 httpgateway.writeJSON 一致（Content-Type + X-Proto-Version）
// ---------------------------------------------------------------------------

// writeJSON 以指定状态码输出 JSON，并带上协议版本头。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 输出统一格式的错误体：{"error":"..."}。
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// statusForErr 把框架错误映射成 HTTP 状态码。
//
// 语义：
//   - 400：请求本身不合法（未知插件名 / 非法配置 / 只读模式）；
//   - 409：状态冲突（已启用/已停用/必需插件/被依赖/版本冲突）；
//   - 500：请求合法但执行失败（如端口被占导致 Start 失败）。
func statusForErr(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, plugin.ErrUnknownPlugin),
		errors.Is(err, plugin.ErrInvalidConfig),
		errors.Is(err, plugin.ErrReadOnly),
		errors.Is(err, plugin.ErrNoConfigPath):
		return http.StatusBadRequest
	case errors.Is(err, plugin.ErrAlreadyEnabled),
		errors.Is(err, plugin.ErrAlreadyDisabled),
		errors.Is(err, plugin.ErrRequiredPlugin),
		errors.Is(err, plugin.ErrDependedOn),
		errors.Is(err, plugin.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// ---------------------------------------------------------------------------
// 视图模型
// ---------------------------------------------------------------------------

// pluginView 是 /api/plugins 里单个插件的展示结构。
type pluginView struct {
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Description    string   `json:"description"`
	Required       bool     `json:"required"`
	RequiredReason string   `json:"requiredReason,omitempty"`
	DefaultOn      bool     `json:"defaultOn"`
	Dependencies   []string `json:"dependencies"`
	Dependents     []string `json:"dependents"`
	State          string   `json:"state"`
	Error          string   `json:"error,omitempty"`
	RestartCount   int      `json:"restartCount"`
	StateChangedAt int64    `json:"stateChangedAt"`
	EnabledAt      int64    `json:"enabledAt,omitempty"`
	StartedAt      int64    `json:"startedAt,omitempty"`
	StoppedAt      int64    `json:"stoppedAt,omitempty"`
	RouteCount     int      `json:"routeCount"`
	Routes         []string `json:"routes"`
	HasOptions     bool     `json:"hasOptions"`
}

// pluginFailure 是失败插件的简短说明。
type pluginFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// pluginsResponse 是 GET /api/plugins 的响应体。
type pluginsResponse struct {
	Count       int                `json:"count"`
	Running     []string           `json:"running"`
	Disabled    []string           `json:"disabled"`
	Failed      []pluginFailure    `json:"failed"`
	Plugins     []pluginView       `json:"plugins"`
	Routes      []plugin.RouteInfo `json:"routes"`
	GeneratedAt int64              `json:"generatedAt"`
}

// optionView 是单插件详情里的「配置项 + 当前值」。
type optionView struct {
	plugin.OptionDef
	Value any `json:"value"`
}

// detailResponse 是 GET /api/plugins/detail 的响应体。
type detailResponse struct {
	pluginView
	Options []optionView        `json:"options"`
	History []plugin.StateEvent `json:"history"`
}

// toggleRequest 是 POST /api/plugins/toggle 的请求体。
type toggleRequest struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// toggleResponse 是 POST /api/plugins/toggle 的响应体。
type toggleResponse struct {
	OK           bool   `json:"ok"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	State        string `json:"state"`
	RestartCount int    `json:"restartCount"`
}

// reloadResult 是 POST /api/plugins/reload 里单个插件的结果。
type reloadResult struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// reloadResponse 是 POST /api/plugins/reload 的响应体。
type reloadResponse struct {
	OK      bool           `json:"ok"`
	Count   int            `json:"count"`
	Failed  int            `json:"failed"`
	Results []reloadResult `json:"results"`
}

// manifestEntry 是编译期插件清单里的一项。
type manifestEntry struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Required     bool     `json:"required"`
	DefaultOn    bool     `json:"defaultOn"`
	Dependencies []string `json:"dependencies"`
	State        string   `json:"state"`
}

// manifestResponse 是 GET /api/manifest 的响应体。
type manifestResponse struct {
	HubVersion string          `json:"hubVersion"`
	LastBuilt  string          `json:"lastBuilt"`
	Count      int             `json:"count"`
	Plugins    []manifestEntry `json:"plugins"`
}

// ---------------------------------------------------------------------------
// GET /api/plugins
// ---------------------------------------------------------------------------

func (p *PluginInfo) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	infos := mgr.Snapshot()
	schemas := mgr.Schemas()
	resp := pluginsResponse{
		Count:       len(infos),
		Running:     []string{},
		Disabled:    []string{},
		Failed:      []pluginFailure{},
		Plugins:     make([]pluginView, 0, len(infos)),
		Routes:      mgr.Routes(),
		GeneratedAt: time.Now().UnixMilli(),
	}
	if resp.Routes == nil {
		resp.Routes = []plugin.RouteInfo{}
	}

	for _, info := range infos {
		_, hasOptions := schemas[info.Name]
		view := newPluginView(mgr, info, hasOptions)
		switch info.State {
		case plugin.StateDisabled:
			resp.Disabled = append(resp.Disabled, info.Name)
		case plugin.StateFailed:
			resp.Failed = append(resp.Failed, pluginFailure{Name: info.Name, Error: info.Error})
			resp.Running = append(resp.Running, info.Name)
		default:
			resp.Running = append(resp.Running, info.Name)
		}
		resp.Plugins = append(resp.Plugins, view)
	}
	writeJSON(w, http.StatusOK, resp)
}

// newPluginView 由 Info 构造展示结构（依赖/路由/是否可配置一并算好，前端零逻辑）。
func newPluginView(mgr *plugin.Manager, info plugin.Info, hasOptions bool) pluginView {
	routes := mgr.Routes()
	owned := make([]string, 0, len(routes))
	for _, rt := range routes {
		if rt.Plugin == info.Name {
			owned = append(owned, rt.Pattern)
		}
	}
	sort.Strings(owned)
	deps := info.Dependencies
	if deps == nil {
		deps = []string{}
	}
	dependents := mgr.Dependents(info.Name)
	if dependents == nil {
		dependents = []string{}
	}
	return pluginView{
		Name:           info.Name,
		Version:        info.Version,
		Description:    info.Description,
		Required:       info.Required,
		RequiredReason: info.RequiredReason,
		DefaultOn:      info.DefaultOn,
		Dependencies:   deps,
		Dependents:     dependents,
		State:          info.State.String(),
		Error:          info.Error,
		RestartCount:   info.RestartCount,
		StateChangedAt: info.StateChangedAt,
		EnabledAt:      info.EnabledAt,
		StartedAt:      info.StartedAt,
		StoppedAt:      info.StoppedAt,
		RouteCount:     len(owned),
		Routes:         owned,
		HasOptions:     hasOptions,
	}
}

// ---------------------------------------------------------------------------
// GET /api/plugins/detail?name=xxx
// ---------------------------------------------------------------------------

func (p *PluginInfo) handlePluginDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "缺少 name 参数（示例：/api/plugins/detail?name=tcpgateway）")
		return
	}
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	var target *plugin.Info
	for _, info := range mgr.Snapshot() {
		if info.Name == name {
			cp := info
			target = &cp
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("插件 %q 不存在", name))
		return
	}

	options := make([]optionView, 0)
	for _, def := range mgr.Schemas()[name] {
		options = append(options, optionView{OptionDef: def, Value: mgr.OptionsOf(name)[def.Key]})
	}
	writeJSON(w, http.StatusOK, detailResponse{
		pluginView: newPluginView(mgr, *target, len(options) > 0),
		Options:    options,
		History:    mgr.History(name),
	})
}

// ---------------------------------------------------------------------------
// POST /api/plugins/toggle
// ---------------------------------------------------------------------------

func (p *PluginInfo) handlePluginToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	var req toggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	name := strings.TrimSpace(req.Name)
	action := strings.TrimSpace(req.Action)
	if name == "" {
		writeError(w, http.StatusBadRequest, "缺少 name")
		return
	}
	if action != actionEnable && action != actionDisable {
		writeError(w, http.StatusBadRequest, `action 只能是 "enable" 或 "disable"`)
		return
	}

	var err error
	if action == actionEnable {
		err = mgr.Enable(name)
	} else {
		err = mgr.Disable(name)
	}
	if err != nil {
		writeError(w, statusForErr(err), err.Error())
		return
	}

	var state plugin.State
	var restarts int
	for _, info := range mgr.Snapshot() {
		if info.Name == name {
			state = info.State
			restarts = info.RestartCount
			break
		}
	}
	writeJSON(w, http.StatusOK, toggleResponse{
		OK:           true,
		Name:         name,
		Action:       action,
		State:        state.String(),
		RestartCount: restarts,
	})
}

// ---------------------------------------------------------------------------
// POST /api/plugins/reload
// ---------------------------------------------------------------------------

func (p *PluginInfo) handlePluginReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	results := mgr.Reload()
	resp := reloadResponse{OK: true, Count: len(results), Results: make([]reloadResult, 0, len(results))}
	for _, res := range results {
		item := reloadResult{
			Name:       res.Name,
			State:      res.State.String(),
			DurationMs: res.Duration.Milliseconds(),
			Error:      res.Error,
			Skipped:    res.Skipped,
			Reason:     res.Reason,
		}
		if res.State == plugin.StateFailed {
			resp.Failed++
		}
		resp.Results = append(resp.Results, item)
	}
	resp.OK = resp.Failed == 0
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /api/manifest[?format=text]
// ---------------------------------------------------------------------------

func (p *PluginInfo) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	resp := manifestResponse{
		HubVersion: hubVersion,
		LastBuilt:  binaryBuiltAt(),
		Plugins:    make([]manifestEntry, 0),
	}
	for _, info := range mgr.Snapshot() {
		deps := info.Dependencies
		if deps == nil {
			deps = []string{}
		}
		resp.Plugins = append(resp.Plugins, manifestEntry{
			Name:         info.Name,
			Version:      info.Version,
			Description:  info.Description,
			Required:     info.Required,
			DefaultOn:    info.DefaultOn,
			Dependencies: deps,
			State:        info.State.String(),
		})
	}
	resp.Count = len(resp.Plugins)

	if strings.TrimSpace(r.URL.Query().Get("format")) == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
		_, _ = w.Write([]byte(manifestText(resp)))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// manifestText 渲染 curl 友好的纯文本清单。
func manifestText(m manifestResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lan-agent-hub %s (built %s)\n", m.HubVersion, m.LastBuilt)
	fmt.Fprintf(&b, "plugins: %d\n", m.Count)
	for _, e := range m.Plugins {
		kind := "optional"
		if e.Required {
			kind = "required"
		}
		line := fmt.Sprintf("  %-12s %-8s %-8s defaultOn=%-5t state=%-11s %s\n",
			e.Name, e.Version, kind, e.DefaultOn, e.State, e.Description)
		b.WriteString(line)
		if len(e.Dependencies) > 0 {
			b.WriteString("               dependencies=" + strings.Join(e.Dependencies, ",") + "\n")
		}
	}
	return b.String()
}

// binaryBuiltAt 用可执行文件的修改时间近似「构建时间」（零依赖、无需 ldflags 配合）。
func binaryBuiltAt() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return fi.ModTime().UTC().Format(time.RFC3339)
}
