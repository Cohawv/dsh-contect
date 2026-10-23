package realtime

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// stubGateway 是满足依赖声明的占位插件（不注册任何路由）。
type stubGateway struct{}

func (stubGateway) Meta() plugin.Meta          { return plugin.Meta{Name: "httpgateway", DefaultOn: true} }
func (stubGateway) Init(*plugin.Context) error { return nil }
func (stubGateway) Start() error               { return nil }
func (stubGateway) Stop() error                { return nil }

// sseRig 是一套装配好的 realtime 插件 + 真实 HTTP 服务器。
type sseRig struct {
	hub *core.Hub
	rt  *Realtime
	mgr *plugin.Manager
	srv *httptest.Server
}

// newSSERig 装配 realtime 插件（可传选项）并启动 httptest 服务器。
func newSSERig(t *testing.T, options map[string]any) *sseRig {
	t.Helper()
	hub := core.NewHub()
	rt := &Realtime{}
	// stubGateway 只为满足 "realtime 依赖 httpgateway" 的拓扑约束：
	// 生产中共享 mux 由 httpgateway 插件提供，测试里则由 httptest 直接服务 mgr.Mux()。
	mgr := plugin.NewManagerWith([]plugin.Plugin{rt, stubGateway{}})
	mgr.ApplyConfig(
		map[string]bool{"realtime": true, "httpgateway": true},
		map[string]map[string]any{"realtime": options},
	)
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(mgr.Mux())
	return &sseRig{hub: hub, rt: rt, mgr: mgr, srv: srv}
}

// close 先停插件（让在途 SSE handler 返回），再关服务器（避免 Close 等活跃长连接）。
func (r *sseRig) close() {
	r.mgr.StopAll()
	r.srv.Close()
}

// realtime 返回被测插件实例。
func (r *sseRig) realtime() *Realtime { return r.rt }

// stream 打开一个 SSE 连接。
func (r *sseRig) stream(t *testing.T, query string, header map[string]string) *sseConn {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.srv.URL+pathStream+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", query, err)
	}
	return &sseConn{resp: resp, reader: bufio.NewReader(resp.Body)}
}

// sseConn 是一条已建立的 SSE 连接。
type sseConn struct {
	resp   *http.Response
	reader *bufio.Reader
}

func (c *sseConn) Close() { _ = c.resp.Body.Close() }

// frame 是一帧已解析的 SSE 数据。
type frame struct {
	Event string
	Data  string
	ID    string
	Ping  bool // 注释行（:ping）
}

// readFrame 读一帧（事件帧以空行结束；注释行单独成帧）。
func (c *sseConn) readFrame(t *testing.T) frame {
	t.Helper()
	var f frame
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读 SSE 帧失败: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.Event == "" && f.Data == "" && f.ID == "" && !f.Ping {
				continue // 前导空行
			}
			return f
		case strings.HasPrefix(line, ":"):
			f.Ping = true
			return f
		case strings.HasPrefix(line, "event: "):
			f.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.Data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "id: "):
			f.ID = strings.TrimPrefix(line, "id: ")
		}
	}
}

// TestStreamHeadersAndFrameFormat 断言响应头与帧格式（event / data / id 三段齐全）。
func TestStreamHeadersAndFrameFormat(t *testing.T) {
	rig := newSSERig(t, map[string]any{"heartbeat": "50ms", "statsInterval": "10s"})
	defer rig.close()

	conn := rig.stream(t, "?topics=agent.online", nil)
	defer conn.Close()

	if ct := conn.resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，期望 text/event-stream", ct)
	}
	if cc := conn.resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q，期望 no-cache", cc)
	}
	if cn := conn.resp.Header.Get("Connection"); cn != "keep-alive" {
		t.Errorf("Connection = %q，期望 keep-alive", cn)
	}
	if xa := conn.resp.Header.Get("X-Accel-Buffering"); xa != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 no（防代理缓冲）", xa)
	}

	// 触发一条 agent.online（走真实事件总线 → 异步订阅 → SSE）。
	rig.hub.Bus.Publish(protocol.TopicAgentOnline, "agent-1")

	f := conn.readFrame(t)
	if f.Ping {
		f = conn.readFrame(t) // 心跳可能先到（50ms 间隔），跳过
	}
	if f.Event != protocol.TopicAgentOnline {
		t.Fatalf("event = %q，期望 %q", f.Event, protocol.TopicAgentOnline)
	}
	if strings.TrimSpace(f.Data) != `"agent-1"` {
		t.Errorf("data = %q，期望 JSON 字符串 \"agent-1\"", f.Data)
	}
	if f.ID == "" || f.ID == "0" {
		t.Errorf("id = %q，期望非零的递增 seq", f.ID)
	}

	// 心跳：注释行（:ping）必须出现。
	gotPing := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fr := conn.readFrame(t)
		if fr.Ping {
			gotPing = true
			break
		}
	}
	if !gotPing {
		t.Fatal("未观察到心跳注释行 :ping")
	}
}

// TestLastEventIDReplay 断言带 Last-Event-ID 重连时能补发其后的事件。
func TestLastEventIDReplay(t *testing.T) {
	rig := newSSERig(t, map[string]any{"heartbeat": "10s", "statsInterval": "10s"})
	defer rig.close()

	const total = 120
	for i := 0; i < total; i++ {
		rig.hub.Bus.Publish(protocol.TopicAgentOnline, fmt.Sprintf("agent-%d", i))
	}
	lastSeq := waitForSeq(t, rig, total)

	// 断线游标：只补最后 5 条（在 100 条环形缓冲窗口内）。
	conn := rig.stream(t, "?topics=agent.online",
		map[string]string{"Last-Event-ID": fmt.Sprint(lastSeq - 5)})
	defer conn.Close()

	var ids []string
	for i := 0; i < 5; i++ {
		f := conn.readFrame(t)
		if f.Event != protocol.TopicAgentOnline {
			t.Fatalf("补发帧 event = %q", f.Event)
		}
		ids = append(ids, f.ID)
	}
	if len(ids) != 5 {
		t.Fatalf("补发条数 = %d，期望 5", len(ids))
	}
	if ids[0] != fmt.Sprint(lastSeq-4) || ids[4] != fmt.Sprint(lastSeq) {
		t.Fatalf("补发 id 序列 = %v，期望 %d..%d", ids, lastSeq-4, lastSeq)
	}
	// 补发结束后，新事件必须接着更大的 id（游标单调，不会重放旧事件）。
	rig.hub.Bus.Publish(protocol.TopicAgentOnline, "after-replay")
	live := conn.readFrame(t)
	for live.Ping {
		live = conn.readFrame(t)
	}
	if live.Event != protocol.TopicAgentOnline {
		t.Fatalf("实时帧 event = %q", live.Event)
	}
	if live.ID == "" || live.ID <= ids[len(ids)-1] {
		t.Fatalf("实时事件 id = %q，应大于补发最后一条 %q", live.ID, ids[len(ids)-1])
	}
}

// TestStreamStatsEndpoint 断言 /api/stream/stats 反映当前连接数。
func TestStreamStatsEndpoint(t *testing.T) {
	rig := newSSERig(t, map[string]any{"maxClients": 8, "heartbeat": "10s", "statsInterval": "10s"})
	defer rig.close()

	conn := rig.stream(t, "?topics=agent.*", nil)
	defer conn.Close()
	waitForClients(t, rig, 1)

	resp, err := rig.srv.Client().Get(rig.srv.URL + pathStreamStats)
	if err != nil {
		t.Fatalf("GET %s: %v", pathStreamStats, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	for _, want := range []string{`"clients":1`, `"maxClients":8`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("响应缺少 %s：%s", want, body)
		}
	}
}

// TestMaxClientsReturns503 断言超出连接上限时返回 503。
func TestMaxClientsReturns503(t *testing.T) {
	rig := newSSERig(t, map[string]any{"maxClients": 2, "heartbeat": "10s", "statsInterval": "10s"})
	defer rig.close()

	c1 := rig.stream(t, "?topics=agent.online", nil)
	defer c1.Close()
	c2 := rig.stream(t, "?topics=agent.online", nil)
	defer c2.Close()
	waitForClients(t, rig, 2)

	resp, err := rig.srv.Client().Get(rig.srv.URL + pathStream + "?topics=agent.online")
	if err != nil {
		t.Fatalf("第 3 个连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("第 3 个连接状态码 = %d，期望 503", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("503 应带 Retry-After 头，便于客户端退避")
	}
}

// TestNoGoroutineLeakWith100Clients 开 100 条并发 SSE 连接，断开后断言无 goroutine 泄漏。
func TestNoGoroutineLeakWith100Clients(t *testing.T) {
	rig := newSSERig(t, map[string]any{"maxClients": 200, "heartbeat": "10s", "statsInterval": "10s"})

	// 先打一次请求，让 HTTP 连接池与服务器的基础 goroutine 就位。
	before := settledGoroutines()

	const clients = 100
	conns := make([]*sseConn, 0, clients)
	for i := 0; i < clients; i++ {
		conns = append(conns, rig.stream(t, "?topics=agent.*", nil))
	}
	waitForClients(t, rig, clients)

	// 发一条事件，确认 100 条连接都能收到（不是"连上了但没人消费"）。
	rig.hub.Bus.Publish(protocol.TopicAgentOnline, "broadcast-check")
	okCount := 0
	for _, c := range conns {
		f := c.readFrame(t)
		if f.Ping {
			f = c.readFrame(t)
		}
		if f.Event == protocol.TopicAgentOnline {
			okCount++
		}
	}
	if okCount != clients {
		t.Fatalf("收到事件的连接数 = %d，期望 %d", okCount, clients)
	}

	for _, c := range conns {
		c.Close()
	}
	waitForClients(t, rig, 0)
	rig.close()

	after := settledGoroutines()
	// 允许小幅抖动（HTTP 连接池、GC 后台协程），但 100 条连接对应的 handler 必须全部回收。
	if after > before+20 {
		t.Fatalf("goroutine 泄漏：连接前 %d，全部断开后 %d（+%d）", before, after, after-before)
	}
	t.Logf("实测：100 条并发 SSE 连接前 %d 个 goroutine，断开并停机后 %d 个", before, after)
}

// TestSlowClientDoesNotBlockPublisher 断言慢客户端不会阻塞发布方，且丢弃被计数。
func TestSlowClientDoesNotBlockPublisher(t *testing.T) {
	rig := newSSERig(t, map[string]any{"maxClients": 4, "heartbeat": "10s", "statsInterval": "10s"})
	defer rig.close()

	// 只建立连接、**从不读取**：服务端写 socket 最终会被 TCP 窗口卡住，
	// 从而把每连接队列打满（这正是要验证的隔离点）。
	conn := rig.stream(t, "?topics=agent.online", nil)
	defer conn.Close()
	waitForClients(t, rig, 1)

	payload := strings.Repeat("x", 512)
	const events = 4000
	start := time.Now()
	for i := 0; i < events; i++ {
		rig.hub.Bus.Publish(protocol.TopicAgentOnline, payload)
	}
	elapsed := time.Since(start)

	// 若发布方被慢客户端阻塞，4000 次发布不可能在 2 秒内完成。
	if elapsed > 2*time.Second {
		t.Fatalf("%d 次 Publish 耗时 %s，慢客户端疑似阻塞了发布方", events, elapsed)
	}

	var dropped int64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		dropped = rig.realtime().Stats().Dropped
		if dropped > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dropped == 0 {
		t.Fatal("慢客户端场景应产生丢弃计数，实际为 0")
	}
	t.Logf("实测：%d 次发布耗时 %s，慢客户端丢弃 %d 条事件（每连接队列 %d）",
		events, elapsed, dropped, clientQueue)
}

// TestTopicFilterMatch 覆盖 topic 过滤器（精确名 + 前缀通配）。
func TestTopicFilterMatch(t *testing.T) {
	f := parseTopics("agent.*,plugin.state")
	cases := map[string]bool{
		"agent.online":  true,
		"agent.offline": true,
		"plugin.state":  true,
		"room.changed":  false,
		"stats.tick":    false,
	}
	for topic, want := range cases {
		if got := f.match(topic); got != want {
			t.Errorf("match(%q) = %v，期望 %v", topic, got, want)
		}
	}
	if !parseTopics("*").match("anything.at.all") {
		t.Error("* 应匹配一切 topic")
	}
	// 空 topics 用默认集合（含 agent/room/plugin/state/stats.tick）。
	def := parseTopics("")
	for _, topic := range []string{protocol.TopicAgentOnline, protocol.TopicRoomChanged, plugin.TopicState, statsTickTopic} {
		if !def.match(topic) {
			t.Errorf("默认订阅应包含 %q", topic)
		}
	}
	if def.match(protocol.TopicMsgRouted) {
		t.Error("默认订阅不应包含高频的 msg.routed（仅用于 metrics 计数）")
	}
}

// waitForSeq 等待 realtime 消化完已发布事件（seq 达到 want）。
func waitForSeq(t *testing.T, rig *sseRig, want int) int64 {
	t.Helper()
	rt := rig.realtime()
	deadline := time.Now().Add(5 * time.Second)
	for {
		seq := rt.Stats().LastSeq
		if seq >= int64(want) {
			return seq
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待事件消化超时：seq=%d，期望 >=%d", seq, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForClients 等待连接数达到 want。
func waitForClients(t *testing.T, rig *sseRig, want int) {
	t.Helper()
	rt := rig.realtime()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := rt.Stats().Clients
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待连接数 = %d 超时（实际 %d）", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settledGoroutines 触发 GC 并短暂等待，返回较稳定的 goroutine 数基线。
func settledGoroutines() int {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { wg.Done() }()
	wg.Wait()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}

// TestMsgRoutedIsOptIn：msg.routed 可被显式订阅，但不在默认集合里（V7 ①）。
//
// V7 ① 的「事件流」视图要渲染 msg.routed；它频率高，故延续 msg.content 的做法：
// 加入 busTopics（服务端订阅总线）但**不进 defaultTopics**（客户端不指定时不推），
// 只有显式带 ?topics=msg.routed 的连接才收到。
func TestMsgRoutedIsOptIn(t *testing.T) {
	rig := newSSERig(t, map[string]any{"heartbeat": "50ms", "statsInterval": "10s"})
	defer rig.close()

	// 1) 显式订阅者能收到
	sub := rig.stream(t, "?topics="+protocol.TopicMsgRouted, nil)
	defer sub.Close()
	// 2) 默认订阅者（不带 topics）收不到
	def := rig.stream(t, "", nil)
	defer def.Close()

	rig.hub.Bus.Publish(protocol.TopicMsgRouted, core.MsgRouted{Kind: "direct", Delivered: 1, Seq: 7})

	// 显式订阅：应收到 msg.routed 帧（心跳可能先到，跳过）
	got := ""
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f := sub.readFrame(t)
		if f.Ping {
			continue
		}
		got = f.Event
		break
	}
	if got != protocol.TopicMsgRouted {
		t.Fatalf("显式订阅者应收到 %q，实得 %q", protocol.TopicMsgRouted, got)
	}

	// 默认订阅：只可能收到心跳/stats，绝不应出现 msg.routed
	deadline = time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		f := def.readFrame(t)
		if !f.Ping && f.Event == protocol.TopicMsgRouted {
			t.Fatalf("默认订阅集合不应含 %q（会放大默认 SSE 流量）", protocol.TopicMsgRouted)
		}
	}

	// 3) defaultTopics 白名单本身不含它（防回归）
	for _, tp := range defaultTopics {
		if tp == protocol.TopicMsgRouted {
			t.Fatalf("defaultTopics 不应含 %q", protocol.TopicMsgRouted)
		}
	}
	// 4) busTopics 必须含它（否则显式订阅也收不到）
	found := false
	for _, tp := range rig.rt.busTopics() {
		if tp == protocol.TopicMsgRouted {
			found = true
		}
	}
	if !found {
		t.Fatalf("busTopics 应含 %q（V7 ① 事件流依赖）", protocol.TopicMsgRouted)
	}
}
