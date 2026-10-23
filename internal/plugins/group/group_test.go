package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/hubbridge"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/plugins/httpgateway"
	"lan-agent-hub/internal/protocol"
)

// ---- 测试替身 ----

// stubView 是 plugin.HubView 的内存实现。
type stubView struct{ agents []plugin.AgentInfo }

func (s stubView) Agents() []plugin.AgentInfo { return s.agents }
func (s stubView) Rooms() []plugin.RoomInfo   { return nil }
func (s stubView) Stats() plugin.HubStats     { return plugin.HubStats{} }

// stubPub 是 plugin.Publisher 的内存实现（记录发送，可模拟离线失败）。
type stubPub struct {
	mu      sync.Mutex
	sent    []protocol.Envelope
	failFor map[string]bool
}

func (s *stubPub) Send(env protocol.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, env)
	if s.failFor[env.To] {
		return fmt.Errorf("stub: %s offline", env.To)
	}
	return nil
}

func (s *stubPub) Broadcast(from, text string) error { return nil }

// stubSession 是 core.Session 的内存实现（rig 集成测用）。
type stubSession struct {
	id  string
	mu  sync.Mutex
	got []protocol.Envelope
}

func (s *stubSession) ID() string { return s.id }

func (s *stubSession) Deliver(e protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}

func (s *stubSession) Close() error { return nil }

func (s *stubSession) received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// ---- 配置解析与校验 ----

// TestNormalizeDefValidation：组名与成员/caps 的校验规则。
func TestNormalizeDefValidation(t *testing.T) {
	cases := []struct {
		name    string
		members []string
		caps    []string
		mode    string
		wantErr string
	}{
		{"ok", []string{"a"}, nil, "", ""},
		{"", []string{"a"}, nil, "", "组名不能为空"},
		{"has space", []string{"a"}, nil, "", "空格或斜杠"},
		{"has/slash", []string{"a"}, nil, "", "空格或斜杠"},
		{"empty-members", nil, nil, "", "members"},
		{"blank-members", []string{"  "}, nil, "", "members"},
		{"dyn-default-mode", nil, []string{"python"}, "", ""},
		{"dyn-bad-mode", nil, []string{"python"}, "sometimes", "any|all"},
	}
	for _, c := range cases {
		g, err := normalizeDef(c.name, c.members, c.caps, c.mode, sourceRuntime)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("normalizeDef(%q) 错误 = %v, want 含 %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeDef(%q) 不应报错: %v", c.name, err)
		}
		if len(c.caps) > 0 {
			if g.Source != sourceRuntime || g.Mode == "" || g.Mode == "sometimes" {
				t.Fatalf("动态组归一化不正确: %+v", g)
			}
		}
	}
}

// TestNormalizeDefDedupSort：成员去重 + 排序 + 去空白。
func TestNormalizeDefDedupSort(t *testing.T) {
	g, err := normalizeDef("devs", []string{" b ", "a", "b", ""}, nil, "", sourceStatic)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !reflect.DeepEqual(g.Members, []string{"a", "b"}) {
		t.Fatalf("成员应去重排序为 [a b]，实得 %v", g.Members)
	}
}

// TestParseStaticDynamic：配置 JSON 解析（含错误用例）。
func TestParseStaticDynamic(t *testing.T) {
	static, err := parseStatic(`{"devs":["a","b"],"ops":["c"]}`)
	if err != nil {
		t.Fatalf("parseStatic: %v", err)
	}
	if len(static) != 2 || static[0].Name == "" || len(static[0].Members) == 0 {
		t.Fatalf("静态组解析不正确: %+v", static)
	}
	for _, g := range static {
		if g.Source != sourceStatic {
			t.Fatalf("source 应为 static: %+v", g)
		}
	}

	dynamic, err := parseDynamic(`{"py":{"caps":["python","go"],"mode":"all"}}`)
	if err != nil {
		t.Fatalf("parseDynamic: %v", err)
	}
	if len(dynamic) != 1 || dynamic[0].Mode != modeAll || len(dynamic[0].Caps) != 2 {
		t.Fatalf("动态组解析不正确: %+v", dynamic)
	}

	if _, err := parseStatic(`not-json`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := parseDynamic(`{"py":{"mode":"bad"}}`); err == nil {
		t.Fatal("mode 非法应报错")
	}
}

// ---- 能力匹配 ----

// TestCapsMatch：any/all 语义 + 大小写不敏感。
func TestCapsMatch(t *testing.T) {
	have := []string{"Python", "camera"}
	if !capsMatch(have, []string{"python"}, modeAny) {
		t.Fatal("any 模式应命中（大小写不敏感）")
	}
	if capsMatch(have, []string{"go"}, modeAny) {
		t.Fatal("any 模式不应命中未声明的能力")
	}
	if !capsMatch(have, []string{"python", "camera"}, modeAll) {
		t.Fatal("all 模式应全命中")
	}
	if capsMatch(have, []string{"python", "go"}, modeAll) {
		t.Fatal("all 模式缺一不应命中")
	}
	if capsMatch(have, nil, modeAny) {
		t.Fatal("空规则不应命中")
	}
}

// ---- 成员解析 ----

func viewOf(agents ...plugin.AgentInfo) stubView { return stubView{agents: agents} }

// TestEffectiveStatic：静态组有效成员 = 配置成员（去重排序）。
func TestEffectiveStatic(t *testing.T) {
	p := &Group{groups: map[string]*groupDef{
		"devs": {Name: "devs", Source: sourceStatic, Members: []string{"b", "a", "b"}},
	}}
	got := p.effective(p.groups["devs"])
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("实得 %v", got)
	}
}

// TestEffectiveDynamicFromView：动态组有效成员 = 当前在线且匹配的 Agent。
func TestEffectiveDynamicFromView(t *testing.T) {
	p := &Group{
		view: viewOf(
			plugin.AgentInfo{ID: "A", Caps: []string{"python"}},
			plugin.AgentInfo{ID: "B", Caps: []string{"go"}},
			plugin.AgentInfo{ID: "C", Caps: []string{"python", "go"}},
		),
		groups: map[string]*groupDef{
			"py": {Name: "py", Source: sourceDynamic, Caps: []string{"python"}, Mode: modeAny},
			"pg": {Name: "pg", Source: sourceDynamic, Caps: []string{"python", "go"}, Mode: modeAll},
		},
	}
	if got := p.effective(p.groups["py"]); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("py 组实得 %v", got)
	}
	if got := p.effective(p.groups["pg"]); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("pg 组实得 %v", got)
	}
}

// ---- HTTP 端点（替身驱动） ----

func doJSON(t *testing.T, h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// TestSendEmptyGroupAck：无在线成员 → action=empty-group 明确回执（设计验收项）。
func TestSendEmptyGroupAck(t *testing.T) {
	p := &Group{
		view:   viewOf(), // 空在线表
		pub:    &stubPub{},
		groups: map[string]*groupDef{"py": {Name: "py", Source: sourceDynamic, Caps: []string{"python"}, Mode: modeAny}},
	}
	rec := doJSON(t, p.handleGroupOp, http.MethodPost, "/api/groups/py/send", `{"text":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	if resp["action"] != actionEmptyGroup {
		t.Fatalf("空组应回 action=empty-group，实得 %v", resp)
	}
	if resp["delivered"].(float64) != 0 {
		t.Fatalf("delivered 应为 0，实得 %v", resp["delivered"])
	}
}

// TestSendStaticOnlyOnlineAndOfflineReported：静态组只发给在线成员，离线成员进 offline。
func TestSendStaticOnlyOnlineAndOfflineReported(t *testing.T) {
	p := &Group{
		view: viewOf(plugin.AgentInfo{ID: "A"}, plugin.AgentInfo{ID: "B"}),
		pub:  &stubPub{},
		groups: map[string]*groupDef{
			"devs": {Name: "devs", Source: sourceStatic, Members: []string{"A", "B", "C"}},
		},
	}
	rec := doJSON(t, p.handleGroupOp, http.MethodPost, "/api/groups/devs/send", `{"text":"hi"}`)
	var resp struct {
		Delivered int      `json:"delivered"`
		Missed    []string `json:"missed"`
		Offline   []string `json:"offline"`
		Action    string   `json:"action"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	if resp.Delivered != 2 {
		t.Fatalf("应投递 2（A/B 在线），实得 %d", resp.Delivered)
	}
	if !reflect.DeepEqual(resp.Offline, []string{"C"}) {
		t.Fatalf("离线成员应为 [C]，实得 %v", resp.Offline)
	}
	if resp.Action != "" {
		t.Fatalf("有在线成员时不应带 empty-group，实得 %q", resp.Action)
	}
	// 信封形态：From 带组名（接收方可见来源），To 为各成员。
	pub := p.pub.(*stubPub)
	if len(pub.sent) != 2 {
		t.Fatalf("应发 2 封，实得 %d", len(pub.sent))
	}
	for _, env := range pub.sent {
		if env.From != "group:devs" {
			t.Fatalf("From 应为 group:devs，实得 %q", env.From)
		}
		if env.To != "A" && env.To != "B" {
			t.Fatalf("To 应是在线成员，实得 %q", env.To)
		}
		if !strings.Contains(string(env.Content), "hi") {
			t.Fatalf("正文缺失: %s", env.Content)
		}
	}
}

// TestSendContentOverride：content 对象优先于 text。
func TestSendContentOverride(t *testing.T) {
	p := &Group{
		view:   viewOf(plugin.AgentInfo{ID: "A"}),
		pub:    &stubPub{},
		groups: map[string]*groupDef{"devs": {Name: "devs", Source: sourceStatic, Members: []string{"A"}}},
	}
	rec := doJSON(t, p.handleGroupOp, http.MethodPost, "/api/groups/devs/send",
		`{"text":"ignored","content":{"cmd":"reboot"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	env := p.pub.(*stubPub).sent[0]
	if !strings.Contains(string(env.Content), "reboot") || strings.Contains(string(env.Content), "ignored") {
		t.Fatalf("content 应优先于 text: %s", env.Content)
	}
}

// TestSendUnknownGroup404 与 TestDeleteUnknown404。
func TestSendUnknownGroup404(t *testing.T) {
	p := &Group{groups: map[string]*groupDef{}}
	rec := doJSON(t, p.handleGroupOp, http.MethodPost, "/api/groups/nope/send", `{"text":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知组发送应 404，实得 %d", rec.Code)
	}
}

// TestCreateGroupViaHTTP：POST 创建 + GET 列表 + upsert + DELETE。
func TestCreateGroupViaHTTP(t *testing.T) {
	p := &Group{hub: core.NewHub(), pub: &stubPub{}, view: viewOf(), groups: map[string]*groupDef{}}

	rec := doJSON(t, p.handleGroups, http.MethodPost, "/api/groups", `{"name":"ops","members":["a","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := p.groups["ops"]; !ok {
		t.Fatal("组未落入状态")
	}

	// 非法创建：空 members 无 caps → 400
	rec = doJSON(t, p.handleGroups, http.MethodPost, "/api/groups", `{"name":"bad"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无成员无 caps 应 400，实得 %d", rec.Code)
	}

	// 列表
	rec = doJSON(t, p.handleGroups, http.MethodGet, "/api/groups", "")
	var list struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	if len(list.Groups) != 1 || list.Groups[0]["name"] != "ops" {
		t.Fatalf("列表不正确: %v", list.Groups)
	}

	// 删除
	rec = doJSON(t, p.handleGroupOp, http.MethodDelete, "/api/groups/ops", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，实得 %d", rec.Code)
	}
	if _, ok := p.groups["ops"]; ok {
		t.Fatal("删除后组应不存在")
	}
	rec = doJSON(t, p.handleGroupOp, http.MethodDelete, "/api/groups/ops", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实得 %d", rec.Code)
	}
}

// TestGroupChangedEventOnCreateDelete：增删组必须发布 group.changed（死契约的发布方）。
func TestGroupChangedEventOnCreateDelete(t *testing.T) {
	hub := core.NewHub()
	var mu sync.Mutex
	var events []protocol.GroupChanged
	hub.Bus.Subscribe(protocol.TopicGroupChanged, func(_ string, payload any) {
		if gc, ok := payload.(protocol.GroupChanged); ok {
			mu.Lock()
			events = append(events, gc)
			mu.Unlock()
		}
	})
	p := &Group{hub: hub, pub: &stubPub{}, view: viewOf(), groups: map[string]*groupDef{}}

	doJSON(t, p.handleGroups, http.MethodPost, "/api/groups", `{"name":"ops","members":["a","b"]}`)
	doJSON(t, p.handleGroupOp, http.MethodDelete, "/api/groups/ops", "")

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("应发布 2 条 group.changed，实得 %d", len(events))
	}
	if events[0].Action != "created" || events[0].Name != "ops" || events[0].Source != sourceRuntime {
		t.Fatalf("created 事件不正确: %+v", events[0])
	}
	if events[1].Action != "deleted" || events[1].Name != "ops" {
		t.Fatalf("deleted 事件不正确: %+v", events[1])
	}
}

// TestApplyHotUpdate：热更新配置组（运行期组保留）。
func TestApplyHotUpdate(t *testing.T) {
	p := &Group{groups: map[string]*groupDef{
		"old": {Name: "old", Source: sourceStatic, Members: []string{"a"}},
		"rt":  {Name: "rt", Source: sourceRuntime, Members: []string{"b"}},
	}}
	if err := p.Apply(map[string]any{"groups": `{"new":["c","d"]}`}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := p.groups["old"]; ok {
		t.Fatal("旧配置组应被替换")
	}
	if _, ok := p.groups["new"]; !ok {
		t.Fatal("新配置组应生效")
	}
	if _, ok := p.groups["rt"]; !ok {
		t.Fatal("运行期组应在热更新中保留")
	}
	if err := p.Apply(map[string]any{"groups": `bad-json`}); err == nil {
		t.Fatal("热更新非法 JSON 应报错")
	}
}

// ---- 端到端（真实 hub + Manager + hubbridge） ----

// groupRig 按生产装配顺序启动：hub → 插件（httpgateway+group）→ SetHub 真实能力 →
// BuildContext → InitAll → StartAll。返回 hub 与 HTTP 基址。
func groupRig(t *testing.T, groupOptions map[string]any) (*core.Hub, string) {
	t.Helper()
	hub := core.NewHub()

	plugins := []plugin.Plugin{&httpgateway.HTTPGateway{}, &Group{}}
	enabled := map[string]bool{"httpgateway": true, "group": true}
	opts := map[string]map[string]any{
		"httpgateway": {"httpAddr": "127.0.0.1:0"},
		"group":       groupOptions,
	}
	mgr := plugin.NewManagerWith(plugins)
	mgr.ApplyConfig(enabled, opts)
	mgr.SetEventBus(hub.Bus)
	// 关键：接入真实 Publisher/View（生产由 cmd/hub 经 hubbridge 完成）。
	mgr.SetHub(hubbridge.NewView(hub), hubbridge.NewPublisher(hub))
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(mgr.Mux())
	t.Cleanup(func() { mgr.StopAll(); srv.Close() })
	return hub, srv.URL
}

// TestRigEndToEndDynamicGroupSend：核心验收——向 caps:python 动态组发送，
// 只有声明该能力的 Agent 收到；消息带 seq（说明走了内核路由机器）。
func TestRigEndToEndDynamicGroupSend(t *testing.T) {
	hub, base := groupRig(t, map[string]any{
		"dynamicGroups": `{"py":{"caps":["python"],"mode":"any"}}`,
	})

	pyAgent := &stubSession{id: "py-agent"}
	goAgent := &stubSession{id: "go-agent"}
	hub.Registry.Bind("py-agent", "Py", []string{"python"}, protocol.ProtoCurrent, pyAgent)
	hub.Registry.Bind("go-agent", "Go", []string{"go"}, protocol.ProtoCurrent, goAgent)

	resp, err := http.Post(base+"/api/groups/py/send", "application/json",
		strings.NewReader(`{"text":"hello pythoneers"}`))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Delivered int      `json:"delivered"`
		Missed    []string `json:"missed"`
		Action    string   `json:"action"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if body.Delivered != 1 {
		t.Fatalf("应投递 1（仅 py-agent），实得 %d", body.Delivered)
	}

	if pyAgent.received() != 1 {
		t.Fatalf("py-agent 应收到 1 条，实得 %d", pyAgent.received())
	}
	if goAgent.received() != 0 {
		t.Fatalf("go-agent 不应收到，实得 %d", goAgent.received())
	}
	// 信封经内核路由：必须有 seq、From 带组名。
	pyAgent.mu.Lock()
	env := pyAgent.got[0]
	pyAgent.mu.Unlock()
	if env.Seq == 0 {
		t.Fatal("组消息必须经内核分配 seq（否则 store/回放对不上）")
	}
	if env.From != "group:py" {
		t.Fatalf("From 应为 group:py，实得 %q", env.From)
	}
	if !strings.Contains(string(env.Content), "hello pythoneers") {
		t.Fatalf("正文丢失: %s", env.Content)
	}
}

// TestRigConfigGroupsListed：配置声明的静态+动态组在 GET /api/groups 可见（混用验收）。
func TestRigConfigGroupsListed(t *testing.T) {
	hub, base := groupRig(t, map[string]any{
		"groups":         `{"devs":["a","b"]}`,
		"dynamicGroups": `{"py":{"caps":["python"],"mode":"any"}}`,
	})
	hub.Registry.Bind("a", "A", []string{"python"}, protocol.ProtoCurrent, &stubSession{id: "a"})

	resp, err := http.Get(base + "/api/groups")
	if err != nil {
		t.Fatalf("GET groups: %v", err)
	}
	defer resp.Body.Close()
	var list struct {
		Groups []struct {
			Name    string   `json:"name"`
			Source  string   `json:"source"`
			Members []string `json:"members"`
			Caps    []string `json:"caps"`
			Mode    string   `json:"mode"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	byName := map[string]int{}
	for i, g := range list.Groups {
		byName[g.Name] = i
	}
	devs, ok := byName["devs"]
	if !ok || list.Groups[devs].Source != sourceStatic || len(list.Groups[devs].Members) != 2 {
		t.Fatalf("静态组 devs 展示不正确: %+v", list.Groups)
	}
	py, ok := byName["py"]
	if !ok || list.Groups[py].Source != sourceDynamic || list.Groups[py].Mode != modeAny {
		t.Fatalf("动态组 py 展示不正确: %+v", list.Groups)
	}
	if !reflect.DeepEqual(list.Groups[py].Members, []string{"a"}) {
		t.Fatalf("动态组 py 的有效成员应为在线匹配者 [a]，实得 %v", list.Groups[py].Members)
	}
}

// TestRigEmptyDynamicGroupAck：动态组无匹配在线 Agent → empty-group 回执（端到端）。
func TestRigEmptyDynamicGroupAck(t *testing.T) {
	_, base := groupRig(t, map[string]any{
		"dynamicGroups": `{"py":{"caps":["python"],"mode":"any"}}`,
	})
	resp, err := http.Post(base+"/api/groups/py/send", "application/json",
		strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if body.Action != actionEmptyGroup {
		t.Fatalf("无匹配成员应回 empty-group，实得 %q", body.Action)
	}
}

// TestRigRuntimeCapsGroupWorks：缺陷 #1 回归——运行期 POST 创建的 caps 动态组
// 必须能解析成员、能发送（此前按 Source 判定类型导致其落入静态空成员分支，
// 创建返回 200 但组永远为空）。
func TestRigRuntimeCapsGroupWorks(t *testing.T) {
	hub, base := groupRig(t, nil) // 无配置组，全靠运行期 API

	pyAgent := &stubSession{id: "py-agent"}
	goAgent := &stubSession{id: "go-agent"}
	hub.Registry.Bind("py-agent", "Py", []string{"python"}, protocol.ProtoCurrent, pyAgent)
	hub.Registry.Bind("go-agent", "Go", []string{"go"}, protocol.ProtoCurrent, goAgent)

	// 1) 运行期创建 caps 组
	resp, err := http.Post(base+"/api/groups", "application/json",
		strings.NewReader(`{"name":"rt-py","caps":["python"]}`))
	if err != nil {
		t.Fatalf("POST create: %v", err)
	}
	var created struct {
		Name    string   `json:"name"`
		Source  string   `json:"source"`
		Caps    []string `json:"caps"`
		Mode    string   `json:"mode"`
		Members []string `json:"members"`
		Action  string   `json:"action"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		t.Fatalf("解码创建响应: %v", err)
	}
	resp.Body.Close()
	if created.Source != sourceRuntime {
		t.Fatalf("运行期创建 source 应为 runtime，实得 %q", created.Source)
	}
	if len(created.Caps) != 1 || created.Mode != modeAny {
		t.Fatalf("caps/mode 应随响应返回: %+v", created)
	}
	if !reflect.DeepEqual(created.Members, []string{"py-agent"}) {
		t.Fatalf("有效成员应为在线匹配者 [py-agent]，实得 %v", created.Members)
	}
	if created.Action != "created" {
		t.Fatalf("响应应带 action=created，实得 %q", created.Action)
	}

	// 2) GET 列表里同样按动态组展示
	listResp, err := http.Get(base + "/api/groups")
	if err != nil {
		t.Fatalf("GET groups: %v", err)
	}
	var list struct {
		Groups []struct {
			Name    string   `json:"name"`
			Source  string   `json:"source"`
			Caps    []string `json:"caps"`
			Members []string `json:"members"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		listResp.Body.Close()
		t.Fatalf("解码列表: %v", err)
	}
	listResp.Body.Close()
	if len(list.Groups) != 1 || list.Groups[0].Name != "rt-py" || len(list.Groups[0].Caps) != 1 {
		t.Fatalf("运行期 caps 组未按动态组展示: %+v", list.Groups)
	}

	// 3) 发送：只有声明 python 的 Agent 收到
	sendResp, err := http.Post(base+"/api/groups/rt-py/send", "application/json",
		strings.NewReader(`{"text":"hello rt"}`))
	if err != nil {
		t.Fatalf("POST send: %v", err)
	}
	var sent struct {
		Delivered int `json:"delivered"`
	}
	if err := json.NewDecoder(sendResp.Body).Decode(&sent); err != nil {
		sendResp.Body.Close()
		t.Fatalf("解码发送响应: %v", err)
	}
	sendResp.Body.Close()
	if sent.Delivered != 1 {
		t.Fatalf("运行期 caps 组应投递 1，实得 %d", sent.Delivered)
	}
	if pyAgent.received() != 1 || goAgent.received() != 0 {
		t.Fatalf("只有 py-agent 应收到：py=%d go=%d", pyAgent.received(), goAgent.received())
	}
}

// TestRuntimeCapsGroupSurvivesApply：运行期 caps 组不得被配置热更新误删
//（类型判定改按 caps 后，replaceConfigLocked 仍只替换同 Source 的配置组）。
func TestRuntimeCapsGroupSurvivesApply(t *testing.T) {
	p := &Group{groups: map[string]*groupDef{}}
	g, err := normalizeDef("rt-py", nil, []string{"python"}, "", sourceRuntime)
	if err != nil {
		t.Fatalf("normalizeDef: %v", err)
	}
	p.groups["rt-py"] = g

	// 热更新：新的动态配置组进来
	if err := p.Apply(map[string]any{"dynamicGroups": `{"cfg-py":{"caps":["go"]}}`}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := p.groups["rt-py"]; !ok {
		t.Fatal("运行期 caps 组被热更新误删")
	}
	if _, ok := p.groups["cfg-py"]; !ok {
		t.Fatal("新配置动态组应生效")
	}
	// 且运行期组仍按动态语义解析
	if !isDynamic(p.groups["rt-py"]) {
		t.Fatal("运行期 caps 组应判定为动态组")
	}
}

// TestCreateGroupUpdatedAction：重复创建同名组返回 action=updated（缺陷 #2 回归）。
func TestCreateGroupUpdatedAction(t *testing.T) {
	p := &Group{hub: core.NewHub(), pub: &stubPub{}, view: viewOf(), groups: map[string]*groupDef{}}
	doJSON(t, p.handleGroups, http.MethodPost, "/api/groups", `{"name":"ops","members":["a"]}`)
	rec := doJSON(t, p.handleGroups, http.MethodPost, "/api/groups", `{"name":"ops","members":["a","b"]}`)
	var resp struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if resp.Action != "updated" {
		t.Fatalf("二次创建应回 action=updated，实得 %q", resp.Action)
	}
}

// TestSendEmptyPayload400：无 text 无 content 的发送请求应 400（不投递空消息）。
func TestSendEmptyPayload400(t *testing.T) {
	p := &Group{
		view:   viewOf(plugin.AgentInfo{ID: "A"}),
		pub:    &stubPub{},
		groups: map[string]*groupDef{"devs": {Name: "devs", Source: sourceStatic, Members: []string{"A"}}},
	}
	for _, body := range []string{`{}`, ``, `{"text":""}`} {
		rec := doJSON(t, p.handleGroupOp, http.MethodPost, "/api/groups/devs/send", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q 应 400，实得 %d", body, rec.Code)
		}
	}
	if len(p.pub.(*stubPub).sent) != 0 {
		t.Fatal("空 payload 不应产生任何投递")
	}
}
