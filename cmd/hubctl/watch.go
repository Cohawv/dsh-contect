package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// SSE 跟随：agents -w / logs -f / watch
//
// 服务端两条 SSE 流的帧格式一致（realtime 与 logview 都用 event/data/id 三段）：
//   event: agent.online\ndata: "a1"\nid: 3\n\n
//   event: log\ndata: {"seq":7,"ts":...,"level":"info","msg":"hi"}\nid: 7\n\n
// ---------------------------------------------------------------------------

// eventStream 描述一条要跟随的 SSE 流。
type eventStream struct {
	path string
	q    url.Values
	src  string // 输出里的来源标记：stream（/api/stream）或 logs（/api/logs/stream）
}

// eventStreamAgents 是 agents -w 订阅的流：只关心 agent.* 前缀的上下线事件。
// realtime 的 topics 支持 "agent.*" 前缀通配（见 internal/plugins/realtime 的 match）。
func eventStreamAgents() eventStream {
	q := url.Values{}
	q.Set("topics", "agent.*")
	return eventStream{path: "/api/stream", q: q, src: "stream"}
}

// eventStreamLogs 是 logs -f 订阅的增量日志流。
func eventStreamLogs(level string) eventStream {
	q := url.Values{}
	if level != "" {
		q.Set("level", level)
	}
	return eventStream{path: "/api/logs/stream", q: q, src: "logs"}
}

// followEvents 跟随单条流（agents -w）。
func (a *app) followEvents(s eventStream) error { return a.followMany(s) }

// followLogs 跟随日志流（logs -f）。
func (a *app) followLogs(q url.Values) error {
	return a.followMany(eventStream{path: "/api/logs/stream", q: q, src: "logs"})
}

// followMany 并发跟随多条流，事件统一串行输出到同一渲染器。
// 任意一条流出错即返回该错误；全部正常结束（EOF / ctx 取消）返回 nil。
func (a *app) followMany(streams ...eventStream) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(streams))
	for _, s := range streams {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- a.cl.follow(a.ctx, s.path, s.q, func(ev sseEvent) error {
				return a.emit(ev, s.src)
			})
		}()
	}
	wg.Wait()
	close(errs)
	var first error
	for err := range errs {
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// streamLine 是流式输出的统一 JSON 形状（每行一个独立可解析的对象）。
type streamLine struct {
	Src   string          `json:"src,omitempty"`
	Seq   int64           `json:"seq,omitempty"`
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
}

// emit 输出一条事件。watch 会从两个 goroutine 调用它，故加 mutex 串行化。
func (a *app) emit(ev sseEvent, src string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.out.isJSON() {
		line := streamLine{Src: src, Seq: seqOf(ev.ID), Topic: ev.Event, Data: rawOrString(ev.Data)}
		b, err := json.Marshal(line)
		if err != nil {
			return err
		}
		return a.out.line(string(b))
	}

	// 表格模式：日志走专门的「级别 + 正文」排版，其余事件按 时间/来源/主题/序号/数据。
	if ev.Event == "log" {
		var e logEntry
		if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
			return a.out.line(strings.Join([]string{
				clockTime(a.now()),
				strings.ToUpper(e.Level),
				"#" + ev.ID,
				oneLine(e.Msg),
			}, "  "))
		}
	}
	return a.out.line(strings.Join([]string{
		clockTime(a.now()),
		src,
		ev.Event,
		"#" + ev.ID,
		oneLine(ev.Data),
	}, "  "))
}

// seqOf 把 SSE 的 id: 段解析成序号；解析不出返回 0（omitempty 会把它省略）。
func seqOf(id string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// rawOrString 让 data 保持原始 JSON 结构；不是合法 JSON 时退化成字符串，
// 保证每行输出始终可被 json.Unmarshal。
func rawOrString(data string) json.RawMessage {
	if json.Valid([]byte(data)) {
		return json.RawMessage(data)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// ---------------------------------------------------------------------------
// watch：agents -w + logs -f 合并
// ---------------------------------------------------------------------------

func (a *app) cmdWatch(args []string) error {
	fs := newFlagSet("watch")
	level := fs.String("level", "", "日志级别过滤：info|warn|error")
	noAgents := fs.Bool("no-agents", false, "不先打印 Agent 快照，只跟随事件")
	if err := fs.Parse(args); err != nil {
		return usageErr("hubctl watch [-level L]：%v", err)
	}
	if fs.NArg() > 0 {
		return usageErr("watch 不接受位置参数（多余：%s）", strings.Join(fs.Args(), " "))
	}
	if !*noAgents {
		var typed agentsResponse
		raw, err := a.fetch(a.ctx, "/api/agents", nil, &typed)
		if err != nil {
			return err
		}
		if err := a.print(raw, agentHeader(), agentRows(a, typed.Agents)); err != nil {
			return err
		}
	}
	return a.followMany(eventStreamAgents(), eventStreamLogs(*level))
}
