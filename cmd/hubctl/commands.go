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
