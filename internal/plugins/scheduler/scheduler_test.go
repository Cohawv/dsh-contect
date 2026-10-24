package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// ---- 测试替身 ----

// stubPub 是 plugin.Publisher 的内存实现。
type stubPub struct {
	mu   sync.Mutex
	sent []protocol.Envelope
}

func (s *stubPub) Send(env protocol.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, env)
	return nil
}

func (s *stubPub) Broadcast(from, text string) error { return nil }

func (s *stubPub) last() protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[len(s.sent)-1]
}

// all 返回已发送信封的副本。
//
// 加锁 + 拷贝而不是让测试直接读 s.sent：调度循环在自己的协程里追加，
// 测试侧无锁读是数据竞争（-race 下会直接报，本机 386 跑不了 -race 不等于没问题）。
func (s *stubPub) all() []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Envelope, len(s.sent))
	copy(out, s.sent)
	return out
}

// probePlugin 是带一个端点的探针插件（call-api 与 plugin-action 两类任务的靶子）。
type probePlugin struct{}

func (p *probePlugin) Meta() plugin.Meta {
	return plugin.Meta{Name: "probe", Version: "1.0.0", Description: "test probe", DefaultOn: true}
}

func (p *probePlugin) Init(ctx *plugin.Context) error {
	return ctx.RegisterHTTP("/probe/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"pong": true})
	})
}

func (p *probePlugin) Start() error { return nil }
func (p *probePlugin) Stop() error  { return nil }

// newScheduler 构造一个不经过 Manager 的调度器（单测直连，真实时钟 + 小 tick）。
func newScheduler(t *testing.T, tickMs int) *Scheduler {
	t.Helper()
	p := &Scheduler{
		pub:   &stubPub{},
		now:   func() int64 { return time.Now().UnixMilli() },
		tick:  time.Duration(tickMs) * time.Millisecond,
		tasks: map[string]*task{},
	}
	return p
}

// waitFor 轮询等待条件成立（超时失败）——调度测试不用 sleep 猜时间。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("超时未满足: %s", what)
}

func snapshotTask(p *Scheduler, id string) task {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return *p.tasks[id]
}

// ---- 配置解析与校验 ----

// TestDefToTaskValidation：三类任务的校验规则。
func TestDefToTaskValidation(t *testing.T) {
	now := int64(1_700_000_000_000)
	cases := []struct {
		name    string
		def     taskDef
		wantErr string
	}{
		{"ok-at", taskDef{ID: "a", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now + 1000}, ""},
		{"ok-after", taskDef{ID: "b", Type: typeCallAPI, Payload: map[string]any{"path": "/x"}, After: 100}, ""},
		{"ok-interval", taskDef{ID: "c", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, Interval: 1000}, ""},
		{"ok-autoid", taskDef{Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now + 1}, ""},
		{"bad-type", taskDef{ID: "a", Type: "nope", Payload: map[string]any{}, At: now}, "未知任务类型"},
		{"no-schedule", taskDef{ID: "a", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}}, "at / after / interval"},
		{"bad-id", taskDef{ID: "a b", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now}, "非法字符"},
		{"send-no-text", taskDef{ID: "a", Type: typeSendMessage, Payload: map[string]any{}, At: now}, "非空 text"},
		{"api-no-path", taskDef{ID: "a", Type: typeCallAPI, Payload: map[string]any{}, At: now}, "path"},
		{"api-bad-path", taskDef{ID: "a", Type: typeCallAPI, Payload: map[string]any{"path": "api/x"}, At: now}, "path"},
		{"pa-no-plugin", taskDef{ID: "a", Type: typePluginAction, Payload: map[string]any{"action": "enable"}, At: now}, "plugin"},
		{"pa-bad-action", taskDef{ID: "a", Type: typePluginAction, Payload: map[string]any{"plugin": "p", "action": "restart"}, At: now}, "action"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk, err := defToTask(c.def, now)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误 = %v, want 含 %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if tk.ID == "" || tk.NextDue == 0 {
				t.Fatalf("任务未正确起算: %+v", tk)
			}
			if !tk.Enabled {
				t.Fatal("enabled 缺省应为 true")
			}
		})
	}
}

// TestEnabledFlagExplicitFalse：enabled:false 应被尊重。
func TestEnabledFlagExplicitFalse(t *testing.T) {
	off := false
	tk, err := defToTask(taskDef{ID: "a", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: 1, Enabled: &off}, 1)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if tk.Enabled {
		t.Fatal("enabled:false 未被尊重")
	}
}

// ---- 调度执行 ----

// TestOneShotRunsAndAutoDisables：at 一次性任务到点执行后自动熄灭。
func TestOneShotRunsAndAutoDisables(t *testing.T) {
	p := newScheduler(t, 5)
	now := time.Now().UnixMilli()
	// 直接装载（绕过 Init 的配置解析路径无关紧要，这里测调度器）。
	tk, err := defToTask(taskDef{ID: "once", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now + 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	p.tasks["once"] = tk

	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()

	waitFor(t, "once 执行", func() bool { return snapshotTask(p, "once").RunCount == 1 })
	tk2 := snapshotTask(p, "once")
	if tk2.Enabled {
		t.Fatal("一次性任务执行后应自动禁用")
	}
	if tk2.LastResult != "ok" {
		t.Fatalf("LastResult 应为 ok，实得 %q", tk2.LastResult)
	}
	if tk2.LastRun == 0 {
		t.Fatal("LastRun 应被记录")
	}
	// 信封经 Publisher 正确发出
	env := p.pub.(*stubPub).last()
	if env.From != "scheduler" || env.To != "" {
		t.Fatalf("信封 from/to 不正确: %+v", env)
	}
	if !strings.Contains(string(env.Content), "hi") {
		t.Fatalf("正文丢失: %s", env.Content)
	}
	// 再等一会，确认不会重复执行
	time.Sleep(80 * time.Millisecond)
	if got := snapshotTask(p, "once").RunCount; got != 1 {
		t.Fatalf("一次性任务不应重复执行，RunCount=%d", got)
	}
}

// TestIntervalReschedules：周期任务按间隔重复执行且保持启用。
func TestIntervalReschedules(t *testing.T) {
	p := newScheduler(t, 5)
	now := time.Now().UnixMilli()
	tk, _ := defToTask(taskDef{ID: "tick", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, Interval: 40}, now)
	p.tasks["tick"] = tk

	p.Start()
	defer p.Stop()

	waitFor(t, "tick 至少执行 2 次", func() bool { return snapshotTask(p, "tick").RunCount >= 2 })
	tk2 := snapshotTask(p, "tick")
	if !tk2.Enabled {
		t.Fatal("周期任务应保持启用")
	}
	if tk2.NextDue == 0 {
		t.Fatal("周期任务应持续排程")
	}
}

// TestStartIdempotent：重复 Start 不起第二个循环（回归：双循环会双跑任务，
// 且旧循环的 ctx 无法取消导致 Stop 的 wg.Wait 永久挂住）。
func TestStartIdempotent(t *testing.T) {
	p := newScheduler(t, 5)
	now := time.Now().UnixMilli()
	tk, err := defToTask(taskDef{ID: "once", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now + 60}, now)
	if err != nil {
		t.Fatal(err)
	}
	p.tasks["once"] = tk

	p.Start()
	p.Start() // 第二次应被忽略
	defer p.Stop()

	waitFor(t, "once 执行", func() bool { return snapshotTask(p, "once").RunCount == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := snapshotTask(p, "once").RunCount; got != 1 {
		t.Fatalf("双 Start 导致任务重复执行，RunCount=%d", got)
	}
}

// TestDisabledTaskDoesNotRun：禁用的任务不执行。
func TestDisabledTaskDoesNotRun(t *testing.T) {
	p := newScheduler(t, 5)
	now := time.Now().UnixMilli()
	off := false
	tk, _ := defToTask(taskDef{ID: "off", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, At: now + 30, Enabled: &off}, now)
	p.tasks["off"] = tk

	p.Start()
	defer p.Stop()

	time.Sleep(120 * time.Millisecond)
	if got := snapshotTask(p, "off").RunCount; got != 0 {
		t.Fatalf("禁用任务不应执行，RunCount=%d", got)
	}
}

// TestSendMessageToAndBroadcast：to/room/broadcast 语义。
func TestSendMessageToAndBroadcast(t *testing.T) {
	p := newScheduler(t, 5)
	now := time.Now().UnixMilli()
	t1, _ := defToTask(taskDef{ID: "dm", Type: typeSendMessage, Payload: map[string]any{"text": "hi", "to": "agent-x"}, At: now + 20}, now)
	t2, _ := defToTask(taskDef{ID: "bc", Type: typeSendMessage, Payload: map[string]any{"text": "all", "to": "ignored", "broadcast": true}, At: now + 20}, now)
	p.tasks["dm"], p.tasks["bc"] = t1, t2

	p.Start()
	defer p.Stop()

	waitFor(t, "两条都执行", func() bool {
		return snapshotTask(p, "dm").RunCount == 1 && snapshotTask(p, "bc").RunCount == 1
	})
	// dm：点对点；bc：广播（to 被清空）
	//
	// 刻意不按 sent[0]/sent[1] 下标取：dm 与 bc 的到期时刻相同（都是 now+20），
	// 调度循环用 `for id, t := range p.tasks` 遍历到期任务，而 Go 的 map 迭代
	// 顺序是随机的——按下标取会随机把广播当成点对点。这不是产品缺陷，
	// 是测试对"执行顺序"的错误假设：实测 -count=6 有 2 次红。
	// 因此只断言语义存在性（各恰好一条），不引入任何顺序要求。
	sent := p.pub.(*stubPub).all()
	if len(sent) != 2 {
		t.Fatalf("应发出 2 条信封，实得 %d 条：%+v", len(sent), sent)
	}
	var dm, bc int
	for i := range sent {
		switch sent[i].To {
		case "agent-x":
			dm++
		case "":
			bc++
		}
	}
	if dm != 1 || bc != 1 {
		t.Fatalf("应各有一条点对点(to=agent-x)与一条广播(to 空)，实得 dm=%d bc=%d：%+v", dm, bc, sent)
	}
}

// TestCallAPIInProcess：call-api 经共享 mux 进程内调用（rig：probe 插件提供端点）。
// rig 已随 StartAll 启动循环（t.Cleanup 负责停），此处不再显式 Start。
func TestCallAPIInProcess(t *testing.T) {
	p, _ := schedulerRig(t, 5, map[string]any{
		"tasks": fmt.Sprintf(`[{"id":"ping","type":"call-api","payload":{"path":"/probe/ping"},"at":%d}]`,
			time.Now().UnixMilli()+50),
	})

	waitFor(t, "ping 执行", func() bool { return snapshotTask(p, "ping").RunCount == 1 })
	if got := snapshotTask(p, "ping").LastResult; got != "http 200" {
		t.Fatalf("call-api 结果应为 http 200，实得 %q", got)
	}
}

// TestPluginActionWithManager：plugin-action 经管理器停用目标插件。
// rig 已启动循环，不再显式 Start。
func TestPluginActionWithManager(t *testing.T) {
	now := time.Now().UnixMilli()
	p, mgr := schedulerRig(t, 5, map[string]any{
		"tasks": fmt.Sprintf(`[{"id":"mute","type":"plugin-action","payload":{"plugin":"probe","action":"disable"},"at":%d}]`, now+50),
	})

	// 前置：probe 启用中
	if !pluginEnabled(mgr, "probe") {
		t.Fatal("前置条件不成立：probe 应启用")
	}
	waitFor(t, "mute 执行", func() bool { return snapshotTask(p, "mute").RunCount == 1 })
	if got := snapshotTask(p, "mute").LastResult; got != "ok" {
		t.Fatalf("plugin-action 结果应为 ok，实得 %q", got)
	}
	if pluginEnabled(mgr, "probe") {
		t.Fatal("probe 应被任务停用")
	}
}

// TestPluginActionManagerUnavailableSkipped：无管理器时记 skipped 不报错。
func TestPluginActionManagerUnavailableSkipped(t *testing.T) {
	p := newScheduler(t, 5)
	if got := p.execPluginAction(map[string]any{"plugin": "x", "action": "disable"}); !strings.HasPrefix(got, "skipped:") {
		t.Fatalf("应记 skipped，实得 %q", got)
	}
}

// TestPluginActionUnknownPluginSkipped：目标插件不存在记 skipped（设计：挂起不报错）。
func TestPluginActionUnknownPluginSkipped(t *testing.T) {
	_, mgr := schedulerRig(t, 5, nil)
	p := &Scheduler{mgr: mgr}
	if got := p.execPluginAction(map[string]any{"plugin": "no-such", "action": "disable"}); !strings.HasPrefix(got, "skipped:") {
		t.Fatalf("应记 skipped，实得 %q", got)
	}
}

// ---- HTTP 端点 ----

// TestTaskAPIEndpoints：创建/列表/立即执行/启用/禁用/删除全链路。
func TestTaskAPIEndpoints(t *testing.T) {
	p, _ := schedulerRig(t, 1000, nil) // 大 tick：不被循环干扰

	// 创建（id 缺省自动生成）
	rec := doJSON(t, p.handleTasks, http.MethodPost, "/api/scheduler/tasks",
		`{"type":"send-message","payload":{"text":"hi"},"after":3600000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var created task
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解码创建响应: %v", err)
	}
	if created.ID == "" || created.Kind != kindOneShot {
		t.Fatalf("创建响应不正确: %+v", created)
	}

	// 重复 id → 409
	rec = doJSON(t, p.handleTasks, http.MethodPost, "/api/scheduler/tasks",
		`{"id":"`+created.ID+`","type":"send-message","payload":{"text":"hi"},"after":3600000}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复 id 应 409，实得 %d", rec.Code)
	}

	// 列表
	rec = doJSON(t, p.handleTasks, http.MethodGet, "/api/scheduler/tasks", "")
	var list struct {
		Tasks []task `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解码列表: %v", err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != created.ID {
		t.Fatalf("列表不正确: %+v", list.Tasks)
	}

	// 立即执行（不改排程）
	rec = doJSON(t, p.handleTaskOp, http.MethodPost, "/api/scheduler/tasks/"+created.ID+"/run", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("run 应 200，实得 %d", rec.Code)
	}
	var ran task
	_ = json.Unmarshal(rec.Body.Bytes(), &ran)
	if ran.RunCount != 1 || ran.LastResult != "ok" || !ran.Enabled {
		t.Fatalf("run 后状态不正确: %+v", ran)
	}

	// 禁用 → 启用（重新武装）
	doJSON(t, p.handleTaskOp, http.MethodPost, "/api/scheduler/tasks/"+created.ID+"/disable", "")
	if snapshotTask(p, created.ID).Enabled {
		t.Fatal("disable 后应 Enabled=false")
	}
	rec = doJSON(t, p.handleTaskOp, http.MethodPost, "/api/scheduler/tasks/"+created.ID+"/enable", "")
	var armed task
	_ = json.Unmarshal(rec.Body.Bytes(), &armed)
	if !armed.Enabled || armed.NextDue == 0 {
		t.Fatalf("enable 后应重新武装: %+v", armed)
	}

	// 删除 + 重复删除 404
	rec = doJSON(t, p.handleTaskOp, http.MethodDelete, "/api/scheduler/tasks/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，实得 %d", rec.Code)
	}
	rec = doJSON(t, p.handleTaskOp, http.MethodDelete, "/api/scheduler/tasks/"+created.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实得 %d", rec.Code)
	}
}

// TestCreateTaskBadRequest：非法定义 400（含未知类型）。
func TestCreateTaskBadRequest(t *testing.T) {
	p, _ := schedulerRig(t, 1000, nil)
	for _, body := range []string{
		`{"id":"x","type":"nope","at":1}`,
		`{"id":"x","type":"send-message","payload":{},"at":1}`,
		`{"id":"x","type":"send-message","payload":{"text":"hi"}}`,
		`{"id":"x/y","type":"send-message","payload":{"text":"hi"},"at":1}`,
	} {
		rec := doJSON(t, p.handleTasks, http.MethodPost, "/api/scheduler/tasks", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s 应 400，实得 %d", body, rec.Code)
		}
	}
}

// TestApplyHotUpdateKeepsRuntimeTasks：热更新替换配置任务、保留运行期任务。
func TestApplyHotUpdateKeepsRuntimeTasks(t *testing.T) {
	now := time.Now().UnixMilli()
	p, _ := schedulerRig(t, 1000, map[string]any{
		"tasks": fmt.Sprintf(`[{"id":"cfg","type":"send-message","payload":{"text":"hi"},"after":3600000}]`),
	})
	// 运行期创建一个任务（fromConfig=false）
	rt, _ := defToTask(taskDef{ID: "rt", Type: typeSendMessage, Payload: map[string]any{"text": "hi"}, After: 3600000}, now)
	p.tasks["rt"] = rt

	if err := p.Apply(map[string]any{"tasks": `[{"id":"cfg2","type":"send-message","payload":{"text":"hi"},"after":3600000}]`}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := p.tasks["cfg"]; ok {
		t.Fatal("旧配置任务应被替换")
	}
	if _, ok := p.tasks["cfg2"]; !ok {
		t.Fatal("新配置任务应生效")
	}
	if _, ok := p.tasks["rt"]; !ok {
		t.Fatal("运行期任务应保留")
	}
	if err := p.Apply(map[string]any{"tasks": `bad`}); err == nil {
		t.Fatal("热更新非法 JSON 应报错")
	}
}

// ---- rig ----

// schedulerRig 装配 probe + scheduler（经 Manager，mux/管理器真实可用）。
// 自持 Scheduler 实例传入 Manager，从而能直接拿回它驱动断言。
func schedulerRig(t *testing.T, tickMs int, schedulerOptions map[string]any) (*Scheduler, *plugin.Manager) {
	t.Helper()
	sched := &Scheduler{}
	plugins := []plugin.Plugin{&probePlugin{}, sched}
	enabled := map[string]bool{"probe": true, "scheduler": true}
	schedOpts := map[string]any{"tickMs": tickMs}
	for k, v := range schedulerOptions {
		schedOpts[k] = v
	}
	opts := map[string]map[string]any{
		"probe":     {},
		"scheduler": schedOpts,
	}
	mgr := plugin.NewManagerWith(plugins)
	mgr.ApplyConfig(enabled, opts)
	if err := mgr.BuildContext(nil); err != nil { // hub 为 nil：scheduler 不依赖 hub
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Cleanup(mgr.StopAll)
	return sched, mgr
}

func pluginEnabled(mgr *plugin.Manager, name string) bool {
	for _, info := range mgr.Infos() {
		if info.Name == name {
			return info.State != plugin.StateDisabled
		}
	}
	return false
}

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
