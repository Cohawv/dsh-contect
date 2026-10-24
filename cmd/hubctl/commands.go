package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 契约模型：字段与服务端响应逐字对应（读到的是真实 API，不是猜的）
//
//   GET  /api/status   → {proto,tcpPort,httpPort,agentCount,roomCount,discovery,serverTime}
//   GET  /api/agents   → {agents:[{id,name,lastSeen?,caps?,proto?}]}
//   GET  /api/rooms    → {rooms:[{id,name,memberCount,members}]}
//   POST /api/send     → {from,to,text}        → {status:"ok"}
//   POST /api/room     → {from,roomId,text}    → {status:"ok"}
//   POST /api/broadcast→ {from,text}           → {status:"ok"}
//   GET  /api/plugins  → {count,running,disabled,failed,plugins:[...],routes,generatedAt}
//   POST /api/plugins/toggle → {name,action}   → {ok,name,action,state,restartCount}
//   GET  /api/logs?level=&limit=&since= → {count,limit,level,capacity,total,dropped,entries:[{seq,ts,level,msg}]}
//   GET  /api/messages?since=&limit=&from=&to=&room= → {messages:[{seq,ts,type,from,to,roomId,content,delivered}],count,lastSeq}
// ---------------------------------------------------------------------------

type statusResponse struct {
	Proto      int   `json:"proto"`
	TCPPort    int   `json:"tcpPort"`
	HTTPPort   int   `json:"httpPort"`
	AgentCount int   `json:"agentCount"`
	RoomCount  int   `json:"roomCount"`
	Discovery  bool  `json:"discovery"`
	ServerTime int64 `json:"serverTime"`
}

type agentView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	LastSeen int64    `json:"lastSeen,omitempty"`
	Caps     []string `json:"caps,omitempty"`
	Proto    int      `json:"proto,omitempty"`
}

type agentsResponse struct {
	Agents []agentView `json:"agents"`
}

type roomView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	MemberCount int      `json:"memberCount"`
	Members     []string `json:"members"`
}

type roomsResponse struct {
	Rooms []roomView `json:"rooms"`
}

type pluginView struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	RouteCount  int      `json:"routeCount"`
	Routes      []string `json:"routes"`
}

type pluginsResponse struct {
	Count   int          `json:"count"`
	Running []string     `json:"running"`
	Plugins []pluginView `json:"plugins"`
}

type logEntry struct {
	Seq   int64  `json:"seq"`
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type logsResponse struct {
	Entries []logEntry `json:"entries"`
}

type messageRecord struct {
	Seq       int64  `json:"seq"`
	TS        int64  `json:"ts"`
	Type      string `json:"type"`
	From      string `json:"from"`
	To        string `json:"to"`
	RoomID    string `json:"roomId,omitempty"`
	Content   string `json:"content"`
	Delivered int    `json:"delivered"`
}

type messagesResponse struct {
	Messages []messageRecord `json:"messages"`
	Count    int             `json:"count"`
	LastSeq  int64           `json:"lastSeq"`
}

// ---------------------------------------------------------------------------
// app：一次 CLI 调用的全部可变状态
// ---------------------------------------------------------------------------

// app 承载一次调用的输出目标、客户端与时间源。
// out/errw/cl/now 全部可注入，所以测试不需要真的起服务或真的等时钟。
type app struct {
	out  *renderer
	errw io.Writer
	cl   *client
	now  func() time.Time
	ctx  context.Context
	mu   sync.Mutex // watch 同时跑两条 SSE，输出必须串行化

	// isTTY 记录 stdout 是否为字符设备。perf -w 据此决定「原地重绘」还是
	// 「一行一个快照」——ANSI 清屏码写进管道会把下游 jq 直接毒掉。
	isTTY bool

	// perfEvery 是 perf -w 的轮询间隔；0 表示用 defaultPerfInterval。
	// 单独开一个字段是为了让单测能把 2s 压到 1ms，而不是靠 sleep 等时钟。
	perfEvery time.Duration
}

// fetch 取回一个端点：raw 用于 json 模式原样透传（契约透明），typed 用于表格渲染。
func (a *app) fetch(ctx context.Context, path string, q url.Values, typed any) (any, error) {
	var raw any
	if err := a.cl.getJSON(ctx, path, q, &raw); err != nil {
		return nil, err
	}
	if typed != nil {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, typed)
		}
	}
	return raw, nil
}

// print 按当前格式输出：json 模式序列化 v，table 模式输出 header+rows。
func (a *app) print(v any, header []string, rows [][]string) error {
	if a.out.isJSON() {
		return a.out.json(v)
	}
	return a.out.table(header, rows)
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func (a *app) cmdStatus(args []string) error {
	fs := newFlagSet("status")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl status：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("status 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	var typed statusResponse
	raw, err := a.fetch(a.ctx, "/api/status", nil, &typed)
	if err != nil {
		return err
	}
	rows := [][]string{
		{"proto", strconv.Itoa(typed.Proto)},
		{"tcpPort", strconv.Itoa(typed.TCPPort)},
		{"httpPort", strconv.Itoa(typed.HTTPPort)},
		{"agentCount", strconv.Itoa(typed.AgentCount)},
		{"roomCount", strconv.Itoa(typed.RoomCount)},
		{"discovery", boolText(typed.Discovery)},
		{"serverTime", fmtTime(typed.ServerTime)},
	}
	return a.print(raw, []string{"KEY", "VALUE"}, rows)
}

// ---------------------------------------------------------------------------
// agents
// ---------------------------------------------------------------------------

func (a *app) cmdAgents(args []string) error {
	fs := newFlagSet("agents")
	watch := fs.Bool("w", false, "监听模式：消费 SSE /api/stream 实时刷新")
	fs.BoolVar(watch, "watch", false, "同 -w")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl agents [-w]：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("agents 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	var typed agentsResponse
	raw, err := a.fetch(a.ctx, "/api/agents", nil, &typed)
	if err != nil {
		return err
	}
	if err := a.print(raw, agentHeader(), agentRows(a, typed.Agents)); err != nil {
		return err
	}
	if !*watch {
		return nil
	}
	return a.followEvents(eventStreamAgents())
}

func agentHeader() []string { return []string{"ID", "NAME", "PROTO", "CAPS", "LASTSEEN"} }

func agentRows(a *app, agents []agentView) [][]string {
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	rows := make([][]string, 0, len(agents))
	for _, ag := range agents {
		proto := "-"
		if ag.Proto > 0 {
			proto = strconv.Itoa(ag.Proto)
		}
		rows = append(rows, []string{ag.ID, ag.Name, proto, joinList(ag.Caps), fmtAge(ag.LastSeen, a.now())})
	}
	return rows
}

// ---------------------------------------------------------------------------
// rooms
// ---------------------------------------------------------------------------

func (a *app) cmdRooms(args []string) error {
	fs := newFlagSet("rooms")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl rooms：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("rooms 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	var typed roomsResponse
	raw, err := a.fetch(a.ctx, "/api/rooms", nil, &typed)
	if err != nil {
		return err
	}
	sort.Slice(typed.Rooms, func(i, j int) bool { return typed.Rooms[i].ID < typed.Rooms[j].ID })
	rows := make([][]string, 0, len(typed.Rooms))
	for _, r := range typed.Rooms {
		rows = append(rows, []string{r.ID, r.Name, strconv.Itoa(r.MemberCount), joinList(r.Members)})
	}
	return a.print(raw, []string{"ID", "NAME", "MEMBERS", "MEMBER-LIST"}, rows)
}

// ---------------------------------------------------------------------------
// send / say / bcast
// ---------------------------------------------------------------------------

// defaultFrom 是 hubctl 作为发送方时使用的身份。
const defaultFrom = "hubctl"

func (a *app) cmdSend(args []string) error {
	fs := newFlagSet("send")
	from := fs.String("from", defaultFrom, "发送方身份")
	to := fs.String("to", "", "目标 Agent（必填）")
	text := fs.String("text", "", "消息正文（必填）")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl send --to A --text \"hi\"：%v", err)
	}
	if *to == "" || *text == "" {
		return usageErr("send 需要 --to 与 --text（当前 to=%q text=%q）", *to, *text)
	}
	return a.postMessage("/api/send", map[string]string{"from": *from, "to": *to, "text": *text})
}

func (a *app) cmdSay(args []string) error {
	fs := newFlagSet("say")
	from := fs.String("from", defaultFrom, "发送方身份")
	room := fs.String("room", "", "目标房间（必填）")
	text := fs.String("text", "", "消息正文（必填）")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl say --room R --text \"hi\"：%v", err)
	}
	if *room == "" || *text == "" {
		return usageErr("say 需要 --room 与 --text（当前 room=%q text=%q）", *room, *text)
	}
	return a.postMessage("/api/room", map[string]string{"from": *from, "roomId": *room, "text": *text})
}

func (a *app) cmdBcast(args []string) error {
	fs := newFlagSet("bcast")
	from := fs.String("from", defaultFrom, "发送方身份")
	text := fs.String("text", "", "消息正文（必填）")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl bcast --text \"hi\"：%v", err)
	}
	if *text == "" {
		return usageErr("bcast 需要 --text")
	}
	return a.postMessage("/api/broadcast", map[string]string{"from": *from, "text": *text})
}

// postMessage 发送一条消息并渲染回执（成功响应体是 {"status":"ok"}）。
func (a *app) postMessage(path string, body map[string]string) error {
	var raw map[string]any
	if err := a.cl.postJSON(a.ctx, path, body, &raw); err != nil {
		return err
	}
	status, _ := raw["status"].(string)
	if status == "" {
		status = "ok"
	}
	return a.print(raw, []string{"ENDPOINT", "STATUS"}, [][]string{{path, status}})
}

// ---------------------------------------------------------------------------
// plugins
// ---------------------------------------------------------------------------

func (a *app) cmdPlugins(args []string) error {
	fs := newFlagSet("plugins")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl plugins [start|stop NAME]：%v", err)
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return a.pluginsList()
	case len(rest) == 2 && (rest[0] == "start" || rest[0] == "stop"):
		action := "enable"
		if rest[0] == "stop" {
			action = "disable"
		}
		return a.pluginToggle(rest[1], action)
	default:
		return usageErr("plugins 用法：hubctl plugins | hubctl plugins start NAME | hubctl plugins stop NAME")
	}
}

func (a *app) pluginsList() error {
	var typed pluginsResponse
	raw, err := a.fetch(a.ctx, "/api/plugins", nil, &typed)
	if err != nil {
		return err
	}
	sort.Slice(typed.Plugins, func(i, j int) bool { return typed.Plugins[i].Name < typed.Plugins[j].Name })
	rows := make([][]string, 0, len(typed.Plugins))
	for _, p := range typed.Plugins {
		rows = append(rows, []string{
			p.Name,
			a.out.stateColor(p.State, p.State),
			p.Version,
			strconv.Itoa(p.RouteCount),
			oneLine(p.Description),
		})
	}
	return a.print(raw, []string{"NAME", "STATE", "VERSION", "ROUTES", "DESCRIPTION"}, rows)
}

// pluginToggle 调用 POST /api/plugins/toggle（action 只能是 enable/disable）。
func (a *app) pluginToggle(name, action string) error {
	var raw map[string]any
	if err := a.cl.postJSON(a.ctx, "/api/plugins/toggle", map[string]string{"name": name, "action": action}, &raw); err != nil {
		return err
	}
	state, _ := raw["state"].(string)
	restarts := 0
	switch v := raw["restartCount"].(type) {
	case float64:
		restarts = int(v)
	}
	return a.print(raw, []string{"NAME", "ACTION", "STATE", "RESTARTS"}, [][]string{
		{name, action, a.out.stateColor(state, state), strconv.Itoa(restarts)},
	})
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func (a *app) cmdLogs(args []string) error {
	fs := newFlagSet("logs")
	follow := fs.Bool("f", false, "跟随模式：消费 SSE /api/logs/stream")
	fs.BoolVar(follow, "follow", false, "同 -f")
	limit := fs.Int("n", 0, "最多返回条数（0 = 服务端默认）")
	fs.IntVar(limit, "limit", 0, "同 -n")
	level := fs.String("level", "", "级别过滤：info|warn|error")
	since := fs.Int64("since", 0, "只返回 ts > since 的条目（毫秒时间戳）")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl logs [-f] [-n N] [-level L] [-since MS]：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("logs 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	if *follow {
		q := url.Values{}
		if *level != "" {
			q.Set("level", *level)
		}
		return a.followLogs(q)
	}
	q := url.Values{}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *level != "" {
		q.Set("level", *level)
	}
	if *since > 0 {
		q.Set("since", strconv.FormatInt(*since, 10))
	}
	var typed logsResponse
	raw, err := a.fetch(a.ctx, "/api/logs", q, &typed)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(typed.Entries))
	for _, e := range typed.Entries {
		rows = append(rows, []string{
			strconv.FormatInt(e.Seq, 10),
			fmtTime(e.TS),
			a.out.levelColor(e.Level, e.Level),
			oneLine(e.Msg),
		})
	}
	return a.print(raw, []string{"SEQ", "TIME", "LEVEL", "MSG"}, rows)
}

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

func (a *app) cmdMessages(args []string) error {
	fs := newFlagSet("messages")
	since := fs.Int64("since", 0, "只返回 seq > since 的消息（服务端语义：序号游标）")
	limit := fs.Int("limit", 0, "最多返回条数（0 = 服务端默认 100）")
	from := fs.String("from", "", "按发送方过滤")
	to := fs.String("to", "", "按接收方过滤")
	room := fs.String("room", "", "按房间过滤")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl messages [--since SEQ] [--limit N]：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("messages 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	q := url.Values{}
	if *since > 0 {
		q.Set("since", strconv.FormatInt(*since, 10))
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *from != "" {
		q.Set("from", *from)
	}
	if *to != "" {
		q.Set("to", *to)
	}
	if *room != "" {
		q.Set("room", *room)
	}
	var typed messagesResponse
	raw, err := a.fetch(a.ctx, "/api/messages", q, &typed)
	if err != nil {
		return err
	}
	rows := make([][]string, 0, len(typed.Messages))
	for _, m := range typed.Messages {
		to := m.To
		if to == "" {
			to = "*"
		}
		rows = append(rows, []string{
			strconv.FormatInt(m.Seq, 10),
			fmtTime(m.TS),
			m.Type,
			m.From,
			to,
			oneLine(messageText(m.Content)),
		})
	}
	return a.print(raw, []string{"SEQ", "TIME", "TYPE", "FROM", "TO", "TEXT"}, rows)
}

// messageText 从消息正文里取出人读文本。
// store 记录里 content 是 Envelope.Content 的原文（如 {"text":"hi"}），
// 取不到 text 字段就原样展示，不做任何猜测。
func messageText(content string) string {
	var m struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &m); err == nil && m.Text != "" {
		return m.Text
	}
	return content
}

// ---------------------------------------------------------------------------
// perf
// ---------------------------------------------------------------------------

// perfPath 是性能快照端点（metrics 插件提供）。
const perfPath = "/api/metrics/summary"

// defaultPerfInterval 是 perf -w 的刷新周期。
// 2s 是折中：比采样间隔（intervalMs）长，保证每次都能拿到新样本；
// 又短到人眼能看出趋势，同时不会把终端刷到无法阅读。
const defaultPerfInterval = 2 * time.Second

// perfSummary 对应 GET /api/metrics/summary。
//
// 字段名与后端逐字一致。前 9 个是 V5 就有的，后 9 个是 V8「性能与规模」新增：
// 老服务端不返回它们时零值兜底，表格里 memLimitBytes=0 显示「未启用」——
// 「没配阈值」和「阈值是 0 字节」是两件事，运维排障时不能混为一谈。
//
// 故意不定义 samples：趋势点不在表格里展示，而 JSON 模式走 raw 原样透传，
// 所以这里少定义字段不会丢数据。
type perfSummary struct {
	UptimeMs          flexInt `json:"uptimeMs"`
	AgentsOnline      flexInt `json:"agentsOnline"`
	Rooms             flexInt `json:"rooms"`
	MessagesTotal     flexInt `json:"messagesTotal"`
	MessagesPerSecond float64 `json:"messagesPerSecond"`
	HTTPRequestsTotal flexInt `json:"httpRequestsTotal"`
	EventsDropped     flexInt `json:"eventsDropped"`
	SSEClients        flexInt `json:"sseClients"`
	IntervalMs        flexInt `json:"intervalMs"`

	// V8 新增：投递丢弃（拆两种成因）、内存、GC、阈值告警。
	DeliverDropped flexInt `json:"deliverDropped"`
	DeliverClosed  flexInt `json:"deliverClosed"`
	HeapAllocBytes flexInt `json:"heapAllocBytes"`
	HeapInuseBytes flexInt `json:"heapInuseBytes"`
	// NextGCBytes 是下一次 GC 的目标堆：heapAlloc/nextGC 才是真实内存压力比，
	// 只看 heapAlloc 无从判断「300 MiB 是轻松还是马上要 GC」。
	NextGCBytes   flexInt `json:"nextGCBytes"`
	Goroutines    flexInt `json:"goroutines"`
	GCTotal       flexInt `json:"gcTotal"`
	MemLimitBytes flexInt `json:"memLimitBytes"`
	MemHigh       bool    `json:"memHigh"`
	MemHighTotal  flexInt `json:"memHighTotal"`
}

// flexInt 是一个宽容的整数：接受 12、12.0、"12"、null。
//
// 为什么不用 int64：这些字段的 Go 类型在后端还没钉死（gcTotal 可能是 uint32，
// 也可能被写成浮点计数器）。整份摘要共用一次 json.Unmarshal，一个字段类型不符
// 就会让其余字段一起归零——那比少一个字段难查得多。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0 // 解析不出就当没有：单字段脏数据不该让整份快照失败
		return nil
	}
	*f = flexInt(int64(v))
	return nil
}

// dropTotal 是三类丢弃之和——CI 只关心「有没有丢」，拆分留给屏幕上的两行。
func (p perfSummary) dropTotal() int64 {
	return int64(p.DeliverDropped) + int64(p.DeliverClosed) + int64(p.EventsDropped)
}

func (a *app) cmdPerf(args []string) error {
	fs := newFlagSet("perf")
	watch := fs.Bool("w", false, "跟随模式：每 2 秒刷新一次快照")
	fs.BoolVar(watch, "watch", false, "同 -w")
	failOnDrop := fs.Bool("fail-on-drop", false, "检测到任何丢弃时以退出码 3 结束（CI 用）")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl perf [-w] [--fail-on-drop]：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("perf 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	if *watch {
		return a.perfWatch(*failOnDrop)
	}
	return a.perfOnce(*failOnDrop)
}

// perfOnce 取一次快照并渲染，随后按需做降级判定。
// 先输出再判定：即使要退出 3，也要让人看到「丢了多少」的证据。
func (a *app) perfOnce(failOnDrop bool) error {
	raw, typed, err := a.perfSnapshot()
	if err != nil {
		return err
	}
	if err := a.renderPerf(raw, typed); err != nil {
		return err
	}
	if failOnDrop && typed.dropTotal() > 0 {
		return degradedErr(typed)
	}
	return nil
}

// perfWatch 轮询快照：TTY 上原地重绘，非 TTY 输出 NDJSON。
//
// 用 a.ctx（由 signal.NotifyContext 派生）作为唯一退出信号：Ctrl-C 后
// 定时器随之作废，不残留 goroutine——本循环本身就在调用方 goroutine 里跑。
func (a *app) perfWatch(failOnDrop bool) error {
	interval := a.perfInterval()
	first := true
	for {
		raw, typed, err := a.perfSnapshot()
		if err != nil {
			return err
		}
		if err := a.redrawPerf(raw, typed, first); err != nil {
			return err
		}
		first = false
		// 跟随模式下判到即退：CI 等待的是「出现即失败」，不是「退出时才有结论」。
		if failOnDrop && typed.dropTotal() > 0 {
			return degradedErr(typed)
		}
		select {
		case <-a.ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// perfInterval 返回本次调用的刷新周期（测试会注入更短的值）。
func (a *app) perfInterval() time.Duration {
	if a.perfEvery > 0 {
		return a.perfEvery
	}
	return defaultPerfInterval
}

// perfSnapshot 取一次快照：raw 供 JSON 原样透传，typed 供表格渲染。
func (a *app) perfSnapshot() (any, perfSummary, error) {
	var typed perfSummary
	raw, err := a.fetch(a.ctx, perfPath, nil, &typed)
	if err != nil {
		return nil, perfSummary{}, err
	}
	return raw, typed, nil
}

// renderPerf 单次渲染：json 模式透传服务端响应（保留未知字段），table 模式走键值表。
func (a *app) renderPerf(raw any, typed perfSummary) error {
	return a.print(raw, []string{"KEY", "VALUE"}, a.perfRows(typed))
}

// redrawPerf 是跟随模式下的单帧输出。
//
// TTY：先清屏再整屏重绘，避免表格逐帧往下堆。
// 非 TTY：绝不出 ANSI——那里是 `hubctl perf -w | jq -c .` 的行流（NDJSON）。
func (a *app) redrawPerf(raw any, typed perfSummary, first bool) error {
	if a.out.isJSON() {
		b, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		return a.out.line(string(b))
	}
	if a.isTTY {
		if err := a.out.line(ansiClearHome); err != nil {
			return err
		}
	} else if !first {
		// 纯文本串流：用空行分帧，保持每帧仍是同一种表格形状。
		if err := a.out.line(""); err != nil {
			return err
		}
	}
	return a.renderPerf(raw, typed)
}

// perfRows 把快照摊平成键值行。
//
// 两类丢弃刻意分成两行：deliverDropped 是队列满（该加缓冲/降速，属背压），
// deliverClosed 是往已关闭的订阅写（属泄漏或竞态，要改代码）——处置动作完全不同，
// 合成一个总数就等于把故障线索扔掉。
//
// 是 app 的方法而非自由函数：堆压力那行需要 renderer 着色，而着色策略
// （TTY / -color）本来就挂在 renderer 上。
func (a *app) perfRows(p perfSummary) [][]string {
	rows := [][]string{
		{"uptime", fmtDuration(int64(p.UptimeMs))},
		{"agentsOnline", strconv.FormatInt(int64(p.AgentsOnline), 10)},
		{"rooms", strconv.FormatInt(int64(p.Rooms), 10)},
		{"messagesTotal", strconv.FormatInt(int64(p.MessagesTotal), 10)},
		{"messagesPerSecond", strconv.FormatFloat(p.MessagesPerSecond, 'f', 1, 64) + " msg/s"},
		// 速率是「最近一个采样窗口内」的增量，窗口多长直接决定这个数怎么读。
		{"sampleInterval", fmtDuration(int64(p.IntervalMs))},
		{"httpRequestsTotal", strconv.FormatInt(int64(p.HTTPRequestsTotal), 10)},
		{"sseClients", strconv.FormatInt(int64(p.SSEClients), 10)},
		{"eventsDropped", strconv.FormatInt(int64(p.EventsDropped), 10)},
		{"deliverDropped(队列满)", strconv.FormatInt(int64(p.DeliverDropped), 10)},
		{"deliverClosed(已关闭)", strconv.FormatInt(int64(p.DeliverClosed), 10)},
		{"heapAlloc", fmtBytes(int64(p.HeapAllocBytes))},
		{"heapInuse", fmtBytes(int64(p.HeapInuseBytes))},
		{"nextGC", nextGCText(p)},
		{"heapPressure(堆/GC 水位)", a.heapPressureText(p)},
		{"goroutines", strconv.FormatInt(int64(p.Goroutines), 10)},
		{"gcTotal", strconv.FormatInt(int64(p.GCTotal), 10)},
		{"memLimit", memLimitText(p)},
		{"memHigh", memHighText(p)},
	}
	return rows
}

// heapPressureCriticalPct 是「堆已逼近 GC 目标」的判定线。
// 90% 取自 Go 的 GC 触发阈值语义：到 100% 就触发 GC，90% 已在临界区内。
const heapPressureCriticalPct = 90.0

// heapPressurePct 返回 heapAlloc 占 nextGC 的百分比；nextGC 不可用时 ok=false。
func heapPressurePct(p perfSummary) (float64, bool) {
	next := int64(p.NextGCBytes)
	if next <= 0 {
		return 0, false
	}
	return float64(int64(p.HeapAllocBytes)) / float64(next) * 100, true
}

// nextGCText 渲染下一次 GC 的目标堆。
// 0 表示 runtime 尚未给出目标（或该服务端没有这项），故写「未设置」而不是 0 B。
func nextGCText(p perfSummary) string {
	if int64(p.NextGCBytes) <= 0 {
		return "未设置"
	}
	return fmtBytes(int64(p.NextGCBytes))
}

// heapPressureText 渲染堆/GC 水位。
//
// 超过临界线时同时给「颜色 + 文字」双重信号：颜色在管道、日志、无色终端里
// 全都会丢，只靠颜色等于没有告警。
func (a *app) heapPressureText(p perfSummary) string {
	pct, ok := heapPressurePct(p)
	if !ok {
		return "—" // 没有分母就不编造比例
	}
	s := strconv.FormatFloat(pct, 'f', 1, 64) + "%"
	if pct < heapPressureCriticalPct {
		return s
	}
	return a.out.paint(ansiRed, s+"（临界：堆已逼近 GC 目标）")
}

// memLimitText 渲染内存阈值：未配置时显式写「未启用」而不是 0 B。
func memLimitText(p perfSummary) string {
	if int64(p.MemLimitBytes) <= 0 {
		return "未启用"
	}
	return fmtBytes(int64(p.MemLimitBytes))
}

// memHighText 渲染阈值状态；累计次数一并给出，便于判断是刚越线还是长期越线。
func memHighText(p perfSummary) string {
	if int64(p.MemLimitBytes) <= 0 {
		return "未启用"
	}
	total := "累计 " + strconv.FormatInt(int64(p.MemHighTotal), 10) + " 次"
	if p.MemHigh {
		return "超阈值（" + total + "）"
	}
	return "正常（" + total + "）"
}

// degradedErr 构造降级退出（退出码 3）。
// 文案把三个计数拆开写：日志里也要能看出是哪一类丢弃触发的。
func degradedErr(p perfSummary) error {
	return &degradedError{msg: "检测到丢弃：" +
		"deliverDropped=" + strconv.FormatInt(int64(p.DeliverDropped), 10) +
		" deliverClosed=" + strconv.FormatInt(int64(p.DeliverClosed), 10) +
		" eventsDropped=" + strconv.FormatInt(int64(p.EventsDropped), 10) +
		"（合计 " + strconv.FormatInt(p.dropTotal(), 10) + "）"}
}

// ---------------------------------------------------------------------------
// fmt
// ---------------------------------------------------------------------------

// cmdFmt 既支持 `hubctl fmt json`（查看/指定格式），也支持
// `hubctl fmt json <子命令>`（等价于 `hubctl --fmt json <子命令>`）。
// 无子命令时打印当前生效格式。
func (a *app) cmdFmt(args []string) error {
	rest := args
	if len(rest) > 0 {
		switch strings.ToLower(rest[0]) {
		case formatJSON, formatTable:
			a.out.format = strings.ToLower(rest[0])
			rest = rest[1:]
		case "-h", "--help":
			return usageErr("hubctl fmt [json|table] [子命令 [参数...]]")
		}
	}
	if len(rest) == 0 {
		f := a.out.format
		return a.print(map[string]string{"format": f}, []string{"FORMAT"}, [][]string{{f}})
	}
	return runCommand(a, rest[0], rest[1:])
}

// newFlagSet 构造一个不自带输出的 FlagSet：解析失败由调用方转成 usageError，
// 避免 flag 包把用法打印到测试无法捕获的地方。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
