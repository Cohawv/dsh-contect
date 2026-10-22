package plugininfo

import (
	"encoding/json"
	"errors"
	"net/http"

	"lan-agent-hub/internal/plugin"
)

// settingsPostRequest 是 POST /api/settings 的请求体。
//
// Version 用指针：必须能区分「没带 version」（400 拒绝）与「version=0」（首次写盘）。
// 这是乐观锁（审查 M4）的硬要求——没有版本号就无法防止丢更新。
//
// Plugins 用 PluginPatch（Enabled 为 *bool）：没提交 enabled 表示「本次不改启用状态」，
// 只提交某个 options 键时不会把插件静默停用，也不会抹掉同插件其它 option。
type settingsPostRequest struct {
	Version *int                          `json:"version"`
	Plugins map[string]plugin.PluginPatch `json:"plugins"`
}

// settingsPostResponse 是 POST /api/settings 成功时的响应体。
//
// UnknownPlugins / StaleOptions 在成功响应里**必然为空**（本次写盘已把它们清掉），
// 但仍显式下发：前端保存后据此就地清空 stale 提示，不必再发一次 GET（N4）。
type settingsPostResponse struct {
	OK             bool                           `json:"ok"`
	Version        int                            `json:"version"`
	Plugins        map[string]plugin.PluginConfig `json:"plugins"`
	PendingRestart []string                       `json:"pendingRestart"`
	UnknownPlugins []string                       `json:"unknownPlugins"`
	StaleOptions   []plugin.StaleOption           `json:"staleOptions"`
}

// settingsConflictResponse 是 POST /api/settings 版本冲突（409）时的响应体：
// 同时返回磁盘当前值，前端据此提示「他人已修改」并提供「查看最新值」。
type settingsConflictResponse struct {
	Error   string                         `json:"error"`
	Version int                            `json:"version"`
	Plugins map[string]plugin.PluginConfig `json:"plugins"`
}

// handleSettings 处理 /api/settings：
//   - GET  → 返回版本 + 各插件配置 + schema（前端据此自动生成表单）+ 配置文件路径
//   - POST → 乐观锁保存：版本不一致 409、非法配置 400（且不写盘）、成功 200 + 新版本
//
// 诚实原则：实现了 plugin.Applier 的插件立即热生效；未实现的在 pendingRestart 里点名，
// 让 UI 明确显示「重启后生效」，绝不假装全部热生效。
func (p *PluginInfo) handleSettings(w http.ResponseWriter, r *http.Request) {
	mgr := p.manager()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "插件管理器不可用")
		return
	}

	switch r.Method {
	case http.MethodGet:
		snap, err := mgr.Settings()
		if err != nil {
			writeError(w, statusForErr(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, snap)

	case http.MethodPost:
		p.saveSettings(w, r, mgr)

	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// saveSettings 执行一次配置保存。
func (p *PluginInfo) saveSettings(w http.ResponseWriter, r *http.Request, mgr *plugin.Manager) {
	var req settingsPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Version == nil {
		writeError(w, http.StatusBadRequest,
			"缺少 version 字段（乐观锁必需）：请先 GET /api/settings 取当前版本")
		return
	}
	if req.Plugins == nil {
		writeError(w, http.StatusBadRequest, "缺少 plugins 字段")
		return
	}

	snap, pending, err := mgr.SaveSettings(plugin.SettingsRequest{
		Version: *req.Version,
		Plugins: req.Plugins,
	})
	if err != nil {
		code := statusForErr(err)
		if errors.Is(err, plugin.ErrConflict) {
			// 409：把磁盘当前值一并返回，前端提示「他人已修改」并可一键查看最新值。
			writeJSON(w, code, settingsConflictResponse{
				Error:   err.Error(),
				Version: snap.Version,
				Plugins: snap.Plugins,
			})
			return
		}
		writeError(w, code, err.Error())
		return
	}

	if pending == nil {
		pending = []string{}
	}
	// 保存成功后磁盘上已无残留（保存时清理过），因此显式下发空数组而不是省略字段：
	// 前端据此立刻清掉「保存一次即会被清理」的提示，避免提示在骗人（N4）。
	unknown := snap.UnknownPlugins
	if unknown == nil {
		unknown = []string{}
	}
	stale := snap.StaleOptions
	if stale == nil {
		stale = []plugin.StaleOption{}
	}
	writeJSON(w, http.StatusOK, settingsPostResponse{
		OK:             true,
		Version:        snap.Version,
		Plugins:        snap.Plugins,
		PendingRestart: pending,
		UnknownPlugins: unknown,
		StaleOptions:   stale,
	})
}
