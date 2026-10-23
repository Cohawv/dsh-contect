// Package capdir 实现「能力目录」插件（V6 ②）。
//
// 把 caps 从「客户端自述的字符串数组」升级为可检索的服务目录：
// 维护 capability → agents 的反向索引，供按能力选人（如把任务派给
// "有摄像头的机器"）。
//
// 设计决策——索引是「查询期派生」而非「缓存 + 事件维护」：
//   - 数据源只有一份：内核在线表（Registry）。capdir 经框架的只读视图
//     （plugin.HubView.Agents，由 internal/hubbridge 在生产接线）读取带 caps 的
//     在线 Agent 快照，每次查询时当场派生索引。
//   - 因此「Agent 注册/下线后索引实时更新」是恒真的——不存在缓存与真实状态
//     不一致的窗口，也不需要订阅上下线事件做增量维护（少一类时序 bug）。
//   - group 插件的动态组成员规则读取同一份视图，天然与 capdir 共用同一索引。
//
// 依赖方向：capdir → plugin（框架）。零外部依赖，不 import core。
package capdir

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

const (
	// pathCapabilities 汇总视图：GET /api/capabilities。
	pathCapabilities = "/api/capabilities"
	// pathCapabilityPrefix 单能力视图（前缀模式）：GET /api/capabilities/{cap}。
	// Go 1.21 的 ServeMux 不支持 {wildcard}，按 ack 插件的先例用前缀匹配 + 余量解析。
	pathCapabilityPrefix = "/api/capabilities/"
)

// CapDir 是 capdir 插件实体（无配置项、无内部可变状态——索引每次查询时派生）。
type CapDir struct {
	// view 是框架注入的只读内核视图（Init 时从 Context 取出）。
	// 运行期「停用 → 启用」会重新走 Init，因此取到的是新 Context 的新视图，不会过期。
	view plugin.HubView
}

// Meta 声明插件元信息。无硬依赖：只读视图由 cmd/hub 装配期经 hubbridge 接线
// （未接线时退化为空视图——本插件返回空目录，不影响其它任何行为）。
func (p *CapDir) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "capdir",
		Version:     "1.0.0",
		Description: "能力目录与按能力选人（纯标准库；零外部依赖）：capability→agents 反向索引，实时派生自在线表",
		DefaultOn:   true,
	}
}

// Init 取出只读视图并注册端点（无钩子、无可变状态）。
func (p *CapDir) Init(ctx *plugin.Context) error {
	p.view = ctx.View()
	if err := ctx.RegisterHTTP(pathCapabilities, p.handleCapabilities); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathCapabilityPrefix, p.handleCapability); err != nil {
		return err
	}
	return nil
}

// Start / Stop 无后台任务与核心钩子。
func (p *CapDir) Start() error { return nil }
func (p *CapDir) Stop() error  { return nil }

// handleCapabilities 处理 GET /api/capabilities：返回全部能力的汇总索引。
// 响应形状：{"capabilities": {"<cap>": ["agentId", ...]}}（成员按 ID 升序）。
func (p *CapDir) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"capabilities": buildIndex(p.agents())})
}

// handleCapability 处理 GET /api/capabilities/{cap}：返回声明某能力的 Agent 列表。
// 匹配大小写不敏感；能力不存在时返回 200 + 空列表（目录查询语义，与
// ack 的 /api/delivery/{unknown} 返回 unknown 一致——查不到不是服务端错误）。
func (p *CapDir) handleCapability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	want := normalizeCap(strings.Trim(strings.TrimPrefix(r.URL.Path, pathCapabilityPrefix), "/"))
	if want == "" {
		http.Error(w, "missing capability", http.StatusBadRequest)
		return
	}
	agents := buildIndex(p.agents())[want]
	if agents == nil {
		agents = []string{} // 归一化为 [] 而非 null：客户端解析形状稳定
	}
	writeJSON(w, map[string]any{"capability": want, "agents": agents})
}

// agents 取当前在线 Agent 快照（view 为 nil 时返回空——单测直连场景）。
func (p *CapDir) agents() []plugin.AgentInfo {
	if p.view == nil {
		return nil
	}
	return p.view.Agents()
}

// ---- 索引派生（纯函数，可单测） ----

// buildIndex 把在线 Agent 快照派生为 capability → agentIds 索引。
// 键为规范化（小写）能力名，值为按 ID 升序的 agentId 列表。
// 同一能力的不同大小写写法（"Python" / "python"）归并到同一键。
func buildIndex(agents []plugin.AgentInfo) map[string][]string {
	idx := make(map[string][]string)
	for _, a := range agents {
		for _, c := range a.Caps {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			key := normalizeCap(c)
			idx[key] = append(idx[key], a.ID)
		}
	}
	for k := range idx {
		sort.Strings(idx[k])
	}
	return idx
}

// normalizeCap 规范化能力名用于匹配（去首尾空白 + 小写）。
func normalizeCap(c string) string { return strings.ToLower(strings.TrimSpace(c)) }

// ---- 内部工具 ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// 与 httpgateway/store/ack 一致：客户端可据此判断服务端协议版本。
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&CapDir{})
}
