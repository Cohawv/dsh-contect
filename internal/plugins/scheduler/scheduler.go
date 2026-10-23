// Package scheduler 实现「定时与延迟任务」插件（V6 ③）。
//
// 把"未来某时刻自动发消息 / 调自己的 REST 端点 / 启停插件"变成可配置能力，
// 替代散落的一次性 goroutine——这正是插件化要消灭的模式。
//
// 任务模型（UTC 毫秒时间戳，与既有 nowMillis 一致）：
//
//	{"id":"daily","type":"send-message","payload":{"text":"..."},"at":1730000000000,"enabled":true}
//	{"id":"poll","type":"call-api","payload":{"method":"GET","path":"/api/status"},"interval":60000}
//	{"id":"mute","type":"plugin-action","payload":{"plugin":"metrics","action":"disable"},"after":5000}
//
// 三种调度语义：
//   - at：一次性，到点执行后自动禁用（enabled=false）；
//   - after：一次性，延迟 N 毫秒（自创建/启动起算）后执行，随后自动禁用；
//   - interval：周期，每 N 毫秒执行；逾期不补跑（停用期间错过的周期直接跳过，
//     从当前时刻起按新周期续跑——对齐设计文档"重启后继续按新周期运行"）。
//
// 三种任务类型：
//   - send-message：经框架 Publisher 发消息（V6b hubbridge 接线；未接线时
//     记录错误结果，不影响其它任务）；
//   - call-api：进程内调用自身共享 mux（httptest 思路：ServeHTTP(recorder, req)），
//     无需知道自己的监听地址；
//   - plugin-action：经 plugin.ManagerFromContext 取管理器启停目标插件；
//     管理器不可用或目标插件不存在时记 skipped，不报错（设计验收：
//     "停用插件后，指向该插件的任务自动挂起（不报错）"）。
//
// 持久化：配置态任务走 `tasks` JSON 选项（落在 hub.json，重启恢复）；
// 运行期经 API 创建的任务驻留内存，重启后需由配置重建（与 group 插件同策略）。
// 执行记录（LastRun/LastResult/RunCount）为内存态，供控制台查看。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

const (
	pathTasks      = "/api/scheduler/tasks"
	pathTaskPrefix = "/api/scheduler/tasks/"

	// 任务类型。
	typeSendMessage  = "send-message"
	typeCallAPI      = "call-api"
	typePluginAction = "plugin-action"

	// plugin-action 的动作。
	actionEnable  = "enable"
	actionDisable = "disable"

	// 调度语义。
	kindOneShot  = "one-shot" // at / after
	kindInterval = "interval"

	// id 允许字符（防路径注入：id 会进 URL）。
	idCharset = "abcdefghijijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."

	// tickMs 默认/范围。
	defaultTickMs = 200
	minTickMs     = 10
	maxTickMs     = 60000
)

// taskDef 是任务的配置/API 形态（JSON）。
type taskDef struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Payload  map[string]any `json:"payload"`
	At       int64          `json:"at"`       // 一次性：到点 UTC 毫秒
	After    int64          `json:"after"`    // 一次性：延迟毫秒（自启动起算）
	Interval int64          `json:"interval"` // 周期：毫秒
	Enabled  *bool          `json:"enabled"`  // 缺省 true
}

// task 是运行态任务（含调度与执行记录）。
type task struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Payload  map[string]any `json:"payload,omitempty"`
	Kind     string         `json:"kind"` // one-shot | interval
	At       int64          `json:"at,omitempty"`
	After    int64          `json:"after,omitempty"`
	Interval int64          `json:"interval,omitempty"`
	Enabled  bool           `json:"enabled"`

	NextDue    int64  `json:"nextDue,omitempty"` // 下次执行时刻（0=未排程/已熄灭）
	LastRun    int64  `json:"lastRun,omitempty"`
	LastResult string `json:"lastResult,omitempty"`
	RunCount   int64  `json:"runCount"`

	fromConfig bool `json:"-"` // 来源：配置（Apply 热更新时替换）vs 运行期 API（保留）
}

// Scheduler 是 scheduler 插件实体。
type Scheduler struct {
	pub  plugin.Publisher
	mux  *http.ServeMux // 自身共享 mux（call-api 进程内调用）
	mgr  *plugin.Manager
	now  func() int64 // 可注入（测试）；缺省 time.Now().UnixMilli

	tick time.Duration

	mu    sync.RWMutex
	tasks map[string]*task

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Meta 声明插件元信息。无硬依赖：Publisher/mux 由框架注入，Manager 经共享 KV 取用。
func (p *Scheduler) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "scheduler",
		Version:     "1.0.0",
		Description: "定时与延迟任务（纯标准库；零外部依赖）：定时群发 / 定时调 REST / 定时启停插件，执行记录可查",
		DefaultOn:   true,
	}
}

// Options 声明配置项。
func (p *Scheduler) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:     "tasks",
			Type:    plugin.OptionString,
			Default: "[]",
			Description: "任务定义（JSON 数组）：[{\"id\",\"type\":\"send-message|call-api|plugin-action\"," +
				"\"payload\",\"at|\"after\"|\"interval\",\"enabled\"}]；at/after/interval 均为毫秒",
		},
		{
			Key:         "tickMs",
			Type:        plugin.OptionInt,
			Default:     defaultTickMs,
			Min:         minTickMs,
			Max:         maxTickMs,
			Description: "调度循环 tick 间隔（毫秒）：决定任务执行的时间精度下限",
		},
	}
}

// Init 解析配置任务、注入能力、注册端点（不启动循环——Start 才启动）。
func (p *Scheduler) Init(ctx *plugin.Context) error {
	p.pub = ctx.Publish()
	p.mux = ctx.ServeMux()
	if mgr, ok := plugin.ManagerFromContext(ctx); ok {
		p.mgr = mgr
	}
	// 注意：必须用闭包。`p.now = time.Now().UnixMilli` 会把方法值绑定到
	// **Init 那一刻的固定 Time**（Go 方法值陷阱），之后 now() 永远返回同一时刻，
	// 调度循环的「现在」不再前进——任务永远不到点。
	p.now = func() int64 { return time.Now().UnixMilli() }
	p.tick = time.Duration(clampInt(ctx.Int("tickMs", defaultTickMs), minTickMs, maxTickMs)) * time.Millisecond
	p.tasks = make(map[string]*task)

	defs, err := parseTasks(ctx.String("tasks", "[]"))
	if err != nil {
		return fmt.Errorf("scheduler: 解析 tasks 配置失败: %w", err)
	}
	if err := ctx.RegisterHTTP(pathTasks, p.handleTasks); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathTaskPrefix, p.handleTaskOp); err != nil {
		return err
	}

	// 配置任务装载（NextDue 按当前时刻起算）。
	now := p.now()
	for _, d := range defs {
		t, err := defToTask(d, now)
		if err != nil {
			return fmt.Errorf("scheduler: 任务 %q 定义非法: %w", d.ID, err)
		}
		t.fromConfig = true
		p.tasks[t.ID] = t
	}
	return nil
}

// Start 启动调度循环。幂等：已在运行时重复调用直接返回。
//
// 为什么必须幂等：双 Start 会起两个循环——到点任务被重复执行（runCount 双计），
// 且后一次 Start 覆盖 p.cancel 后，前一个循环的 ctx 再无取消途径，
// Stop 的 wg.Wait 永久挂住（框架 safeStop 有 5s 超时兜底，但协程已泄漏）。
func (p *Scheduler) Start() error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()
	p.wg.Add(1)
	go p.loop(ctx)
	return nil
}

// Stop 停止调度循环（等在飞任务结束，避免停机后半截执行）。幂等。
func (p *Scheduler) Stop() error {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新配置任务（运行期创建的任务保留，同 group 策略）。
func (p *Scheduler) Apply(options map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := options["tasks"]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("scheduler: tasks 选项必须是 JSON 字符串")
	}
	defs, err := parseTasks(s)
	if err != nil {
		return fmt.Errorf("scheduler: 热更新 tasks 失败: %w", err)
	}
	// 替换旧配置任务（NextDue 已排程过的按新定义重置），运行期任务不动。
	for id, t := range p.tasks {
		if t.fromConfig {
			delete(p.tasks, id)
		}
	}
	now := p.now()
	for _, d := range defs {
		t, err := defToTask(d, now)
		if err != nil {
			return fmt.Errorf("scheduler: 热更新任务 %q 非法: %w", d.ID, err)
		}
		p.tasks[t.ID] = t
	}
	return nil
}

// ---- 调度循环 ----

func (p *Scheduler) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runDue()
		}
	}
}

// runDue 执行所有到点任务（持锁收集 → 锁外执行，避免执行期间长时间占锁）。
func (p *Scheduler) runDue() {
	now := p.now()
	p.mu.RLock()
	due := make([]*task, 0, 4)
	for _, t := range p.tasks {
		if !t.Enabled || t.NextDue == 0 || t.NextDue > now {
			continue
		}
		due = append(due, t)
	}
	p.mu.RUnlock()
	for _, t := range due {
		p.runTask(t)
	}
}

// runTask 执行一个任务并更新调度与记录。
func (p *Scheduler) runTask(t *task) {
	result := p.execute(t)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	t.LastRun = now
	t.LastResult = result
	t.RunCount++
	if t.Kind == kindOneShot {
		t.Enabled = false // 一次性任务执行后自动熄灭
		t.NextDue = 0
		return
	}
	// 周期任务：按原排程点递推；若已逾期（停机错过），从当前起新周期，不补跑。
	t.NextDue = t.NextDue + t.Interval
	if t.NextDue <= now {
		t.NextDue = now + t.Interval
	}
}

// execute 按类型执行任务，返回结果串（"ok..." / "error: ..." / "skipped: ..."）。
func (p *Scheduler) execute(t *task) string {
	switch t.Type {
	case typeSendMessage:
		return p.execSendMessage(t.Payload)
	case typeCallAPI:
		return p.execCallAPI(t.Payload)
	case typePluginAction:
		return p.execPluginAction(t.Payload)
	default:
		return "error: unknown type " + t.Type
	}
}

// execSendMessage 经 Publisher 发一条消息。
// payload：{"text":"...","to":"agent-x"?,"room":"r1"?,"broadcast":true?}
func (p *Scheduler) execSendMessage(payload map[string]any) string {
	text, _ := payload["text"].(string)
	if text == "" {
		return "error: send-message 需要非空 text"
	}
	to, _ := payload["to"].(string)
	room, _ := payload["room"].(string)
	bcast, _ := payload["broadcast"].(bool)
	if bcast {
		to, room = "", ""
	}
	env := protocol.NewEnvelope(protocol.TypeChat, "scheduler", to, room,
		map[string]any{"text": text})
	if p.pub == nil {
		return "error: Publisher 未接线（hubbridge 缺失）"
	}
	if err := p.pub.Send(env); err != nil {
		return "error: " + err.Error()
	}
	return "ok"
}

// execCallAPI 进程内调用自身共享 mux（无需知道监听地址）。
// payload：{"method":"GET"?,"path":"/api/status","body":"..."?}
func (p *Scheduler) execCallAPI(payload map[string]any) string {
	path, _ := payload["path"].(string)
	if path == "" {
		return "error: call-api 需要非空 path"
	}
	method, _ := payload["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	if p.mux == nil {
		return "error: 共享 mux 不可用"
	}
	var body io.Reader
	if s, ok := payload["body"].(string); ok && s != "" {
		body = strings.NewReader(s)
	}
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return "error: " + err.Error()
	}
	rec := httptest.NewRecorder()
	p.mux.ServeHTTP(rec, req)
	return fmt.Sprintf("http %d", rec.Code)
}

// execPluginAction 经管理器启停插件；管理器缺失或目标不存在记 skipped（不报错）。
// payload：{"plugin":"metrics","action":"enable|disable"}
func (p *Scheduler) execPluginAction(payload map[string]any) string {
	name, _ := payload["plugin"].(string)
	act, _ := payload["action"].(string)
	if name == "" || (act != actionEnable && act != actionDisable) {
		return "error: plugin-action 需要 plugin 与 action(enable|disable)"
	}
	if p.mgr == nil {
		return "skipped: 管理器不可用"
	}
	var err error
	if act == actionEnable {
		err = p.mgr.Enable(name)
	} else {
		err = p.mgr.Disable(name)
	}
	if err != nil {
		// 目标插件不存在/被依赖挡住等：按设计"自动挂起不报错"。
		return "skipped: " + err.Error()
	}
	return "ok"
}

// ---- 端点 ----

// handleTasks 处理 GET /api/scheduler/tasks（列表）与 POST（创建）。
func (p *Scheduler) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.listTasks(w)
	case http.MethodPost:
		p.createTask(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listTasks 返回全部任务（含调度与执行记录），按 ID 升序。
func (p *Scheduler) listTasks(w http.ResponseWriter) {
	p.mu.RLock()
	out := make([]*task, 0, len(p.tasks))
	for _, t := range p.tasks {
		cp := *t
		out = append(out, &cp)
	}
	p.mu.RUnlock()
	sortTasks(out)
	writeJSON(w, map[string]any{"tasks": out})
}

// createTask 处理 POST /api/scheduler/tasks：创建任务（id 已存在 → 409）。
func (p *Scheduler) createTask(w http.ResponseWriter, r *http.Request) {
	var d taskDef
	if err := decodeJSON(r, &d); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	t, err := defToTask(d, p.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if _, dup := p.tasks[t.ID]; dup {
		p.mu.Unlock()
		http.Error(w, "task id already exists: "+t.ID, http.StatusConflict)
		return
	}
	p.tasks[t.ID] = t
	p.mu.Unlock()
	writeJSON(w, t)
}

// handleTaskOp 处理 {pathTaskPrefix} 余量：
//   - POST   /api/scheduler/tasks/{id}/run     —— 立即执行（不改排程）
//   - POST   /api/scheduler/tasks/{id}/enable  —— 启用并重新武装
//   - POST   /api/scheduler/tasks/{id}/disable —— 禁用
//   - DELETE /api/scheduler/tasks/{id}         —— 删除
func (p *Scheduler) handleTaskOp(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, pathTaskPrefix), "/")
	if rest == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return
	}
	for _, suffix := range []string{"/run", "/enable", "/disable"} {
		if strings.HasSuffix(rest, suffix) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			p.taskAction(w, strings.TrimSuffix(rest, suffix), strings.TrimPrefix(suffix, "/"))
			return
		}
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.deleteTask(w, rest)
}

// taskAction 执行 run/enable/disable。
func (p *Scheduler) taskAction(w http.ResponseWriter, id, action string) {
	if !validID(id) {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	p.mu.RLock()
	t, ok := p.tasks[id]
	p.mu.RUnlock()
	if !ok {
		http.Error(w, "unknown task: "+id, http.StatusNotFound)
		return
	}
	switch action {
	case "run":
		// 立即执行并记录，但不改动 enabled/NextDue（排程照常）。
		result := p.execute(t)
		now := p.now()
		p.mu.Lock()
		t.LastRun = now
		t.LastResult = result
		t.RunCount++
		cp := *t
		p.mu.Unlock()
		writeJSON(w, &cp)
	case "enable":
		p.mu.Lock()
		t.Enabled = true
		t.rearm(p.now()) // 重新武装：一次性任务到点/立即再跑，周期任务进新周期
		cp := *t
		p.mu.Unlock()
		writeJSON(w, &cp)
	case "disable":
		p.mu.Lock()
		t.Enabled = false
		cp := *t
		p.mu.Unlock()
		writeJSON(w, &cp)
	}
}

// deleteTask 处理 DELETE。
func (p *Scheduler) deleteTask(w http.ResponseWriter, id string) {
	if !validID(id) {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	_, ok := p.tasks[id]
	if ok {
		delete(p.tasks, id)
	}
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown task: "+id, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"deleted": id})
}

// rearm 重新武装排程（调用方持锁）。
func (t *task) rearm(now int64) {
	switch t.Kind {
	case kindOneShot:
		if t.At > 0 {
			t.NextDue = t.At // 到点（可能立即触发）
		} else {
			t.NextDue = now + t.After
		}
	case kindInterval:
		t.NextDue = now + t.Interval
	}
}

// ---- 配置解析 ----

// parseTasks 解析 tasks 配置 JSON 数组。
func parseTasks(s string) ([]taskDef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var defs []taskDef
	if err := json.Unmarshal([]byte(s), &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// defToTask 把配置/API 定义转成运行态任务（校验 + 起算 NextDue）。
func defToTask(d taskDef, now int64) (*task, error) {
	if d.ID == "" {
		d.ID = protocol.UUID() // API 创建时 id 可缺省
	}
	if !validID(d.ID) {
		return nil, fmt.Errorf("id %q 含非法字符（允许字母数字 . _ -）", d.ID)
	}
	if d.Type != typeSendMessage && d.Type != typeCallAPI && d.Type != typePluginAction {
		return nil, fmt.Errorf("未知任务类型 %q（支持 send-message/call-api/plugin-action）", d.Type)
	}
	if err := validatePayload(d.Type, d.Payload); err != nil {
		return nil, err
	}
	enabled := true
	if d.Enabled != nil {
		enabled = *d.Enabled
	}
	t := &task{
		ID: d.ID, Type: d.Type, Payload: d.Payload, Enabled: enabled,
		At: d.At, After: d.After, Interval: d.Interval,
	}
	switch {
	case d.Interval > 0:
		t.Kind = kindInterval
		t.NextDue = now + d.Interval // 首个周期后执行；停机不错过、不补跑
	case d.At > 0 || d.After > 0:
		t.Kind = kindOneShot
		if d.At > 0 {
			t.NextDue = d.At // 历史到点任务启动即补跑一次（对齐 at 语义）
		} else {
			t.NextDue = now + d.After
		}
	default:
		return nil, fmt.Errorf("任务 %q 需要 at / after / interval 之一", d.ID)
	}
	return t, nil
}

// validatePayload 按类型校验 payload 必填项。
func validatePayload(typ string, payload map[string]any) error {
	switch typ {
	case typeSendMessage:
		if s, _ := payload["text"].(string); s == "" {
			return fmt.Errorf("send-message 需要非空 text")
		}
	case typeCallAPI:
		path, _ := payload["path"].(string)
		if path == "" || !strings.HasPrefix(path, "/") {
			return fmt.Errorf("call-api 需要以 / 开头的 path")
		}
	case typePluginAction:
		name, _ := payload["plugin"].(string)
		act, _ := payload["action"].(string)
		if name == "" {
			return fmt.Errorf("plugin-action 需要 plugin")
		}
		if act != actionEnable && act != actionDisable {
			return fmt.Errorf("plugin-action 的 action 只支持 enable|disable")
		}
	}
	return nil
}

// ---- 内部工具 ----

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune(idCharset, c) {
			return false
		}
	}
	return true
}

func sortTasks(ts []*task) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].ID < ts[j-1].ID; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
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

// decodeJSON 解析请求体（空 body 视为空对象）。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func init() {
	plugin.Register(&Scheduler{})
}
