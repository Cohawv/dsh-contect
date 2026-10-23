// Package group 实现「分组与定向路由」插件（V6 ②）。
//
// 把路由词汇从「点对点 / 房间 / 广播」扩展到「按组群发」：
//
//	chat(to=X) / room(roomId) / broadcast(all)   —— 既有三种，零变化
//	POST /api/groups/{name}/send                 —— 新增：发给组内全部在线成员
//
// 设计约束（V6 设计文档 C1）：GroupService 在 core.Router 之外**旁挂**，
// 经框架的 Publisher 能力发消息（internal/hubbridge 生产接线），**不改 core**。
// 每个成员的投递都走内核 Router.Route——seq 分配、store 落盘、msg.routed 事件
// 与客户端消息完全同路，不搞第二套投递逻辑。
//
// 组的两种定义来源：
//   - 静态组：配置声明显式成员（hub.json → group.groups，JSON 字符串选项）；
//   - 动态组：caps 规则（hub.json → group.dynamicGroups），有效成员 =
//     当前在线且 caps 匹配的 Agent——匹配时经框架只读视图读取在线表，
//     与 capdir 插件共用同一份数据源（内核在线表），实时派生、无缓存。
// 运行期还可经 POST /api/groups 增改组、DELETE /api/groups/{name} 删组；
// 每次集合变更发布 protocol.TopicGroupChanged（控制台可实时刷新）。
package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

const (
	// pathGroups 组集合端点：GET 列表 / POST 创建或更新。
	pathGroups = "/api/groups"
	// pathGroupPrefix 组操作前缀：POST /api/groups/{name}/send、DELETE /api/groups/{name}。
	pathGroupPrefix = "/api/groups/"

	// sendSuffix 发送动作后缀（POST {pathGroupPrefix}{name}/send）。
	sendSuffix = "/send"

	// maxNameLen 组名长度上限（防滥用；组名会进 URL 路径）。
	maxNameLen = 64
)

// 组来源取值。
const (
	sourceStatic  = "static"  // 配置声明的静态组
	sourceDynamic = "dynamic" // 配置声明的 caps 规则组
	sourceRuntime = "runtime" // 运行期 API 创建/更新的组
)

// 动态组匹配模式。
const (
	modeAny = "any" // 命中任意一个声明能力即入组
	modeAll = "all" // 须命中全部声明能力才入组
)

// 空组回执 action（设计验收项：组内无在线成员时返回明确回执）。
const actionEmptyGroup = "empty-group"

// groupDef 是一个组的定义。
type groupDef struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`            // static | dynamic | runtime
	Members []string `json:"members,omitempty"` // static/runtime 的显式成员
	Caps    []string `json:"caps,omitempty"`    // dynamic 的能力规则
	Mode    string   `json:"mode,omitempty"`    // dynamic：any | all
}

// Group 是 group 插件实体。
type Group struct {
	hub  *core.Hub
	pub  plugin.Publisher
	view plugin.HubView

	mu     sync.RWMutex
	groups map[string]*groupDef // name → 定义（静态+动态+运行期合并视图）
}

// Meta 声明插件元信息。
// 无硬依赖：Publisher/View 由 cmd/hub 经 hubbridge 接线；未接线时本插件
// 降级为「空目录 + 空发送」（不影响其它任何行为）。
func (p *Group) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "group",
		Version:     "1.0.0",
		Description: "分组与定向路由（纯标准库；零外部依赖）：静态组 + caps 动态组，群发经内核路由，不改 core",
		DefaultOn:   true,
	}
}

// Options 声明配置项：两组 JSON 字符串（控制台渲染为文本框）。
func (p *Group) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "groups",
			Type:        plugin.OptionString,
			Default:     "{}",
			Description: "静态分组定义（JSON 对象）：{\"devs\":[\"agent-a\",\"agent-b\"]}；成员为 agentId 列表",
		},
		{
			Key:         "dynamicGroups",
			Type:        plugin.OptionString,
			Default:     "{}",
			Description: "动态分组规则（JSON 对象）：{\"py\":{\"caps\":[\"python\"],\"mode\":\"any\"}}；mode 取值 any|all",
		},
	}
}

// Init 挂接框架能力、解析配置组、注册端点。
func (p *Group) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("group: missing *core.Hub in context")
	}
	p.hub = hub
	p.pub = ctx.Publish()
	p.view = ctx.View()
	p.groups = make(map[string]*groupDef)

	static, err := parseStatic(ctx.String("groups", "{}"))
	if err != nil {
		return fmt.Errorf("group: 解析 groups 配置失败: %w", err)
	}
	dynamic, err := parseDynamic(ctx.String("dynamicGroups", "{}"))
	if err != nil {
		return fmt.Errorf("group: 解析 dynamicGroups 配置失败: %w", err)
	}
	for _, g := range static {
		p.groups[g.Name] = g
	}
	for _, g := range dynamic {
		p.groups[g.Name] = g
	}

	if err := ctx.RegisterHTTP(pathGroups, p.handleGroups); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathGroupPrefix, p.handleGroupOp); err != nil {
		return err
	}
	return nil
}

// Start / Stop 无后台任务、无核心钩子（Publisher 是框架侧能力，无需卸载）。
func (p *Group) Start() error { return nil }
func (p *Group) Stop() error  { return nil }

// Apply 实现 plugin.Applier：热更新静态/动态组配置（运行期创建的组保留）。
func (p *Group) Apply(options map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := options["groups"]; ok {
		if s, ok := v.(string); ok {
			static, err := parseStatic(s)
			if err != nil {
				return fmt.Errorf("group: 热更新 groups 失败: %w", err)
			}
			p.replaceConfigLocked(sourceStatic, static)
		}
	}
	if v, ok := options["dynamicGroups"]; ok {
		if s, ok := v.(string); ok {
			dynamic, err := parseDynamic(s)
			if err != nil {
				return fmt.Errorf("group: 热更新 dynamicGroups 失败: %w", err)
			}
			p.replaceConfigLocked(sourceDynamic, dynamic)
		}
	}
	return nil
}

// replaceConfigLocked 用新配置组替换旧配置组（同 source），运行期组不动。调用方持写锁。
func (p *Group) replaceConfigLocked(source string, fresh []*groupDef) {
	for name, g := range p.groups {
		if g.Source == source {
			delete(p.groups, name)
		}
	}
	for _, g := range fresh {
		p.groups[g.Name] = g
	}
}

// ---- 端点 ----

// handleGroups 处理 GET /api/groups（列表）与 POST /api/groups（创建/更新）。
func (p *Group) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.listGroups(w)
	case http.MethodPost:
		p.createGroup(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listGroups 返回全部组（含解析后的有效成员），按组名升序。
func (p *Group) listGroups(w http.ResponseWriter) {
	p.mu.RLock()
	defs := make([]*groupDef, 0, len(p.groups))
	for _, g := range p.groups {
		defs = append(defs, g)
	}
	p.mu.RUnlock()
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	out := make([]map[string]any, 0, len(defs))
	for _, g := range defs {
		item := map[string]any{"name": g.Name, "source": g.Source, "members": p.effective(g)}
		if g.Source == sourceDynamic {
			item["caps"] = g.Caps
			item["mode"] = g.Mode
		}
		out = append(out, item)
	}
	writeJSON(w, map[string]any{"groups": out})
}

// createGroup 处理 POST /api/groups：创建或更新一个组（upsert）。
// 请求体：{"name":"x","members":[...]}（静态语义）或 {"name":"y","caps":[...],"mode":"any"}（动态语义）。
func (p *Group) createGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		Caps    []string `json:"caps"`
		Mode    string   `json:"mode"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, err := normalizeDef(body.Name, body.Members, body.Caps, body.Mode, sourceRuntime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, existed := p.snapshot(g.Name)

	p.mu.Lock()
	p.groups[g.Name] = g
	p.mu.Unlock()

	action := "created"
	if existed {
		action = "updated"
	}
	p.publishChanged(action, g)

	item := map[string]any{"name": g.Name, "source": g.Source, "members": p.effective(g)}
	if g.Source == sourceDynamic {
		item["caps"] = g.Caps
		item["mode"] = g.Mode
	}
	writeJSON(w, item)
}

// handleGroupOp 处理 {pathGroupPrefix} 余量：
//   - POST /api/groups/{name}/send —— 群发
//   - DELETE /api/groups/{name}   —— 删除
func (p *Group) handleGroupOp(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, pathGroupPrefix), "/")
	if rest == "" {
		http.Error(w, "missing group name", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(rest, sendSuffix) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.sendToGroup(w, r, strings.TrimSuffix(rest, sendSuffix))
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.deleteGroup(w, rest)
}

// sendToGroup 处理 POST /api/groups/{name}/send：把消息投递给组内全部在线成员。
// 每个成员一次内核路由（独立 seq、独立落盘）；组内无在线成员时返回
// action=empty-group 的明确回执（设计验收项）。
func (p *Group) sendToGroup(w http.ResponseWriter, r *http.Request, name string) {
	g, ok := p.snapshot(name)
	if !ok {
		http.Error(w, "unknown group: "+name, http.StatusNotFound)
		return
	}
	var body struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	content := any(body.Content)
	if len(body.Content) == 0 {
		content = map[string]any{"text": body.Text}
	}

	targets := p.effective(g)
	online := p.onlineSubset(targets)

	resp := map[string]any{"group": name, "from": "group:" + name}
	if len(online) == 0 {
		// 无在线成员：明确回执，不静默成功（运维需要能区分「发了」和「没发」）。
		resp["action"] = actionEmptyGroup
		resp["delivered"] = 0
		resp["missed"] = []string{}
		if len(targets) > 0 {
			resp["offline"] = targets
		}
		writeJSON(w, resp)
		return
	}

	delivered := 0
	var missed []string
	for _, id := range online {
		env := protocol.NewEnvelope(protocol.TypeChat, "group:"+name, id, "", content)
		if err := p.pub.Send(env); err != nil {
			missed = append(missed, id)
			continue
		}
		delivered++
	}
	resp["delivered"] = delivered
	if missed == nil {
		missed = []string{}
	}
	resp["missed"] = missed
	// 静态组里不在线的成员单独报告（发送方可见「谁没收到」）。
	var offline []string
	if len(online) < len(targets) {
		onlineSet := make(map[string]bool, len(online))
		for _, id := range online {
			onlineSet[id] = true
		}
		for _, id := range targets {
			if !onlineSet[id] {
				offline = append(offline, id)
			}
		}
	}
	if len(offline) > 0 {
		resp["offline"] = offline
	}
	writeJSON(w, resp)
}

// deleteGroup 处理 DELETE /api/groups/{name}。
func (p *Group) deleteGroup(w http.ResponseWriter, name string) {
	p.mu.Lock()
	g, ok := p.groups[name]
	if ok {
		delete(p.groups, name)
	}
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown group: "+name, http.StatusNotFound)
		return
	}
	p.publishChanged("deleted", g)
	writeJSON(w, map[string]any{"deleted": name})
}

// ---- 成员解析 ----

// snapshot 取组定义的浅拷贝（持读锁，避免调用方持有内部指针）。
func (p *Group) snapshot(name string) (*groupDef, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	g, ok := p.groups[name]
	if !ok {
		return nil, false
	}
	cp := *g
	cp.Members = append([]string(nil), g.Members...)
	cp.Caps = append([]string(nil), g.Caps...)
	return &cp, true
}

// effective 返回组的有效成员（定义层）：
//   - 静态/运行组：配置成员（去重、排序）；
//   - 动态组：当前在线且 caps 匹配的 Agent（按 ID 排序；无匹配时返回空切片而非 nil，
//     保证 JSON 序列化为 [] —— 客户端解析形状稳定）。
func (p *Group) effective(g *groupDef) []string {
	if g.Source != sourceDynamic {
		return sortedUnique(g.Members)
	}
	out := make([]string, 0, 4)
	for _, a := range p.onlineAgents() {
		if capsMatch(a.Caps, g.Caps, g.Mode) {
			out = append(out, a.ID)
		}
	}
	sort.Strings(out)
	return out
}

// onlineSubset 从 targets 中过滤出当前在线的成员（保序）。
func (p *Group) onlineSubset(targets []string) []string {
	online := make(map[string]bool)
	for _, a := range p.onlineAgents() {
		online[a.ID] = true
	}
	out := make([]string, 0, len(targets))
	for _, id := range targets {
		if online[id] {
			out = append(out, id)
		}
	}
	return out
}

// onlineAgents 取在线 Agent 快照（view 未接线时为空——动态组随之解析为空，安全降级）。
func (p *Group) onlineAgents() []plugin.AgentInfo {
	if p.view == nil {
		return nil
	}
	return p.view.Agents()
}

// capsMatch 判断 have 是否满足 want 规则（规范化后比较，大小写不敏感）。
func capsMatch(have, want []string, mode string) bool {
	if len(want) == 0 {
		return false
	}
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[strings.ToLower(strings.TrimSpace(c))] = true
	}
	if mode == modeAll {
		for _, c := range want {
			if !set[strings.ToLower(strings.TrimSpace(c))] {
				return false
			}
		}
		return true
	}
	for _, c := range want { // modeAny（默认）
		if set[strings.ToLower(strings.TrimSpace(c))] {
			return true
		}
	}
	return false
}

// ---- 事件 ----

// publishChanged 发布一条 group.changed（bus 为 nil 时静默）。
func (p *Group) publishChanged(action string, g *groupDef) {
	if p.hub == nil || p.hub.Bus == nil {
		return
	}
	p.hub.Bus.Publish(protocol.TopicGroupChanged, protocol.GroupChanged{
		Action:  action,
		Name:    g.Name,
		Source:  g.Source,
		Members: p.effective(g),
	})
}

// ---- 配置解析与校验 ----

// parseStatic 解析静态组配置 JSON：{"name":["a","b"]}。
func parseStatic(s string) ([]*groupDef, error) {
	var raw map[string][]string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make([]*groupDef, 0, len(raw))
	for name, members := range raw {
		g, err := normalizeDef(name, members, nil, "", sourceStatic)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// parseDynamic 解析动态组配置 JSON：{"name":{"caps":["python"],"mode":"any"}}。
func parseDynamic(s string) ([]*groupDef, error) {
	var raw map[string]struct {
		Caps []string `json:"caps"`
		Mode string   `json:"mode"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make([]*groupDef, 0, len(raw))
	for name, rule := range raw {
		g, err := normalizeDef(name, nil, rule.Caps, rule.Mode, sourceDynamic)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// normalizeDef 校验并归一化一个组定义。
// caps 非空 → 动态组（mode 默认 any，只接受 any/all）；否则 → 显式成员组（成员不可为空）。
func normalizeDef(name string, members, caps []string, mode, source string) (*groupDef, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("组名不能为空")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("组名长度超过 %d", maxNameLen)
	}
	if strings.ContainsAny(name, "/ \t") {
		return nil, fmt.Errorf("组名不可包含空格或斜杠（会进 URL 路径）")
	}
	if len(caps) > 0 {
		m := strings.ToLower(strings.TrimSpace(mode))
		if m == "" {
			m = modeAny
		}
		if m != modeAny && m != modeAll {
			return nil, fmt.Errorf("mode 只支持 any|all，实得 %q", mode)
		}
		return &groupDef{Name: name, Source: source, Caps: sortedUnique(caps), Mode: m}, nil
	}
	if len(sortedUnique(members)) == 0 {
		return nil, fmt.Errorf("组 %q 需要非空 members（静态组）或非空 caps（动态组）", name)
	}
	return &groupDef{Name: name, Source: source, Members: sortedUnique(members)}, nil
}

// ---- 内部工具 ----

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeJSON 解析请求体（空 body 视为空对象，不报错——发送接口允许只带 text 或缺省）。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil // 空 body：保持零值
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// 与 httpgateway/store/ack/capdir 一致：客户端可据此判断服务端协议版本。
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Group{})
}
