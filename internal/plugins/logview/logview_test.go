package logview

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/plugin"
)

// stubGateway 满足 "logview 依赖 httpgateway" 的拓扑约束（测试里共享 mux 直接由 httptest 服务）。
type stubGateway struct{}

func (stubGateway) Meta() plugin.Meta          { return plugin.Meta{Name: "httpgateway", DefaultOn: true} }
func (stubGateway) Init(*plugin.Context) error { return nil }
func (stubGateway) Start() error               { return nil }
func (stubGateway) Stop() error                { return nil }

// lvRig 是一套装配好的 logview 插件 + 真实 HTTP 服务器。
type lvRig struct {
	lv  *LogView
	mgr *plugin.Manager
	srv *httptest.Server
}

// newLVRig 走与生产一致的装配路径（ApplyConfig → BuildContext → InitAll → StartAll），
// 因此测的是真实的容量配置解析与路由注册，而不是测试私有的旁路。
func newLVRig(t *testing.T, capacity int) *lvRig {
	t.Helper()
	lv := &LogView{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{lv, stubGateway{}})
	var opts map[string]map[string]any
	if capacity > 0 {
		opts = map[string]map[string]any{"logview": {"capacity": capacity}}
	}
	mgr.ApplyConfig(map[string]bool{"logview": true, "httpgateway": true}, opts)
	if err := mgr.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(mgr.Mux())
	rig := &lvRig{lv: lv, mgr: mgr, srv: srv}
	t.Cleanup(rig.close)
	return rig
}

func (r *lvRig) close() {
	r.mgr.StopAll()
	r.srv.Close()
}

// write 写一条日志（模拟框架 fanout 的调用）。
func (r *lvRig) write(level, msg string, ts time.Time) {
	r.lv.Write(level, msg, ts)
}

// TestRingBufferOverwritesOldest 断言环形缓冲超过容量后覆盖最旧条目，且**内存不增长**。
func TestRingBufferOverwritesOldest(t *testing.T) {
	const capacity = 16 // 容量下限；配置更小时会被夹到它
	rig := newLVRig(t, capacity)

	base := time.Now()
	for i := 0; i < capacity+4; i++ {
		rig.write(plugin.LevelInfo, fmt.Sprintf("line-%d", i), base.Add(time.Duration(i)*time.Millisecond))
	}

	got := rig.lv.Query(QueryOptions{})
	if len(got) != capacity {
		t.Fatalf("条数 = %d，期望 %d（容量上限）", len(got), capacity)
	}
	if got[0].Msg != "line-4" || got[len(got)-1].Msg != fmt.Sprintf("line-%d", capacity+3) {
		t.Fatalf("覆盖后首尾 = %q..%q，期望 line-4..line-%d", got[0].Msg, got[len(got)-1].Msg, capacity+3)
	}

	// 内存不增长：底层切片长度恒等于容量。
	rig.lv.mu.RLock()
	bufLen := len(rig.lv.buf)
	rig.lv.mu.RUnlock()
	if bufLen != capacity {
		t.Fatalf("缓冲区长度 = %d，期望恒为容量 %d", bufLen, capacity)
	}

	// 再灌 1000 条，长度仍然不变（seq 单调递增，不因覆盖回退）。
	for i := 0; i < 1000; i++ {
		rig.write(plugin.LevelInfo, "flood", base)
	}
	rig.lv.mu.RLock()
	bufLen = len(rig.lv.buf)
	total := rig.lv.seq
	rig.lv.mu.RUnlock()
	if bufLen != capacity {
		t.Fatalf("压测后缓冲区长度 = %d，期望 %d", bufLen, capacity)
	}
	if want := int64(capacity + 4 + 1000); total != want {
		t.Fatalf("写入总数 = %d，期望 %d", total, want)
	}
}

// TestLevelFilterAndSince 覆盖级别过滤（warn 及以上）、since 游标与 limit 取最新。
func TestLevelFilterAndSince(t *testing.T) {
	rig := newLVRig(t, 0) // 走默认容量
	base := time.Now().UnixMilli()
	entries := []struct {
		level string
		msg   string
		ts    int64
	}{
		{plugin.LevelInfo, "i1", base},
		{plugin.LevelWarn, "w1", base + 10},
		{plugin.LevelError, "e1", base + 20},
		{plugin.LevelInfo, "i2", base + 30},
	}
	for _, e := range entries {
		rig.lv.Write(e.level, e.msg, time.UnixMilli(e.ts))
	}

	cases := []struct {
		name string
		opt  QueryOptions
		want []string
	}{
		{"全部", QueryOptions{}, []string{"i1", "w1", "e1", "i2"}},
		{"仅 warn 及以上", QueryOptions{Level: "warn"}, []string{"w1", "e1"}},
		{"仅 error", QueryOptions{Level: "error"}, []string{"e1"}},
		{"info 及以上", QueryOptions{Level: "info"}, []string{"i1", "w1", "e1", "i2"}},
		{"since 截止", QueryOptions{Since: base + 10}, []string{"e1", "i2"}},
		{"limit 取最新", QueryOptions{Limit: 2}, []string{"e1", "i2"}},
	}
	for _, c := range cases {
		got := rig.lv.Query(c.opt)
		msgs := make([]string, len(got))
		for i, e := range got {
			msgs[i] = e.Msg
		}
		if strings.Join(msgs, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: = %v，期望 %v", c.name, msgs, c.want)
		}
	}
}

// TestApplyResizesAndKeepsNewest 覆盖容量热更新：缩容保留最新条目，越界配置被拒。
func TestApplyResizesAndKeepsNewest(t *testing.T) {
	rig := newLVRig(t, 100)
	base := time.Now()
	for i := 0; i < 50; i++ {
		rig.write(plugin.LevelInfo, fmt.Sprintf("n-%d", i), base)
	}
	if err := rig.lv.Apply(map[string]any{"capacity": float64(20)}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := rig.lv.Query(QueryOptions{})
	if len(got) != 20 {
		t.Fatalf("缩容后条数 = %d，期望 20", len(got))
	}
	if got[0].Msg != "n-30" || got[19].Msg != "n-49" {
		t.Fatalf("缩容应保留最新 20 条，首尾 = %q..%q", got[0].Msg, got[19].Msg)
	}
	if err := rig.lv.Apply(map[string]any{"capacity": 1}); err == nil {
		t.Fatal("容量低于下限应返回错误（非法配置必须被拒）")
	}
}

// TestHTTPLogsEndpoint 覆盖 /api/logs 的查询参数与响应结构。
func TestHTTPLogsEndpoint(t *testing.T) {
	rig := newLVRig(t, 50)

	base := time.Now()
	rig.write(plugin.LevelInfo, "启动完成", base)
	rig.write(plugin.LevelError, "httpgateway listen failed", base.Add(time.Millisecond))

	resp, err := rig.srv.Client().Get(rig.srv.URL + pathLogs + "?level=error&limit=10")
	if err != nil {
		t.Fatalf("GET %s: %v", pathLogs, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var body logsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.Count != 1 || body.Entries[0].Msg != "httpgateway listen failed" {
		t.Fatalf("响应 = %+v", body)
	}
	if body.Entries[0].Level != plugin.LevelError {
		t.Errorf("级别 = %q", body.Entries[0].Level)
	}
	if body.Capacity != 50 {
		t.Errorf("capacity = %d，期望 50", body.Capacity)
	}

	// limit 上限夹到 1000（防止一次拉爆浏览器）。
	big, err := rig.srv.Client().Get(rig.srv.URL + pathLogs + "?limit=99999")
	if err != nil {
		t.Fatal(err)
	}
	defer big.Body.Close()
	var bigBody logsResponse
	if err := json.NewDecoder(big.Body).Decode(&bigBody); err != nil {
		t.Fatal(err)
	}
	if bigBody.Limit != maxLimit {
		t.Errorf("limit = %d，期望夹到 %d", bigBody.Limit, maxLimit)
	}

	// 非法 level 必须 400（而不是静默返回全部）。
	bad, err := rig.srv.Client().Get(rig.srv.URL + pathLogs + "?level=loud")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 level 状态码 = %d，期望 400", bad.StatusCode)
	}
}

// TestHTTPLogsStreamAndReplay 覆盖 /api/logs/stream 的帧格式与 Last-Event-ID 补发。
func TestHTTPLogsStreamAndReplay(t *testing.T) {
	rig := newLVRig(t, 100)

	base := time.Now()
	for i := 0; i < 3; i++ {
		rig.write(plugin.LevelInfo, fmt.Sprintf("pre-%d", i), base)
	}

	// 带 Last-Event-ID=1 重连：应补发 seq 2、3。
	req, _ := http.NewRequest(http.MethodGet, rig.srv.URL+pathLogsStream, nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := rig.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("连接流失败: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	for _, want := range []string{"pre-1", "pre-2"} {
		ev, data, id := readLogFrame(t, reader)
		if ev != "log" {
			t.Fatalf("event = %q，期望 log", ev)
		}
		if !strings.Contains(data, want) {
			t.Errorf("data = %q，期望包含 %q", data, want)
		}
		if id == "" {
			t.Error("id 不应为空（补发游标依赖它）")
		}
	}

	// 增量：写入一条新日志，应立即收到。
	rig.write(plugin.LevelWarn, "live-line", time.Now())
	_, data, _ := readLogFrame(t, reader)
	if !strings.Contains(data, "live-line") {
		t.Fatalf("增量帧 data = %q", data)
	}
}

// TestSubscribeReplayChannelNoDuplicate 是 P2-1 的回归测试：
// 注册订阅者与拍摄补发快照之间存在窗口，窗口内写入的日志会同时进入 replay 与 sub.ch。
// 真实场景里这条日志被 fanout 推入通道、同时也还在 buf（Seq>lastID）会被 replay 选中，
// 因此合并下发时**同一 Seq 必须只出现一次**——既不能重复、也不能因此丢行。
func TestSubscribeReplayChannelNoDuplicate(t *testing.T) {
	rig := newLVRig(t, 100)
	base := time.Now()
	for i := 0; i < 10; i++ {
		rig.write(plugin.LevelInfo, fmt.Sprintf("line-%d", i), base)
	}

	// lastID=5 → replay 应含 seq 6..10。
	sub, replay, _ := rig.lv.subscribe(5, 0)
	if len(replay) != 5 {
		t.Fatalf("replay 长度 = %d，期望 5（seq 6..10）", len(replay))
	}

	// 模拟窗口内写入：把一条已存在于 replay 的日志（seq=8）和一条新日志（seq=11）
	// 塞进通道——真实里这条 8 会同时出现在 replay 与通道中。
	sub.ch <- Entry{Seq: 8, Level: plugin.LevelInfo, Msg: "dup-should-drop"}
	sub.ch <- Entry{Seq: 11, Level: plugin.LevelInfo, Msg: "new-should-keep"}

	d := &seqDedup{last: 5}
	merged := rig.lv.mergeReplayChannel(sub, replay, d)

	seen := map[int64]bool{}
	var seqs []int64
	for _, e := range merged {
		if seen[e.Seq] {
			t.Fatalf("出现重复帧 seq=%d（P2-1 未修复）", e.Seq)
		}
		seen[e.Seq] = true
		seqs = append(seqs, e.Seq)
	}
	want := []int64{6, 7, 8, 9, 10, 11}
	if !reflect.DeepEqual(seqs, want) {
		t.Fatalf("合并后 seq 序列 = %v，期望 %v（8 去重、11 保留、无丢行）", seqs, want)
	}
}

// TestLogFileBehaviorUnchanged 断言框架接管 log 后，原输出（模拟 lan-agent-hub.log）仍拿到完整一行，
// 且 logview 的环形缓冲也收到同一条（两处是"额外副本"关系，不是替代）。
func TestLogFileBehaviorUnchanged(t *testing.T) {
	orig := log.Writer()
	defer log.SetOutput(orig)

	var fileBuf bytes.Buffer
	log.SetOutput(&fileBuf)

	lv := &LogView{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{lv, stubGateway{}})
	mgr.ApplyConfig(map[string]bool{"logview": true, "httpgateway": true}, nil)
	if err := mgr.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	log.Printf("sweeper: sweep loop did not exit within 5s")

	if !strings.Contains(fileBuf.String(), "sweep loop did not exit") {
		t.Fatalf("原输出丢失（lan-agent-hub.log 行为必须不变）: %q", fileBuf.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := lv.Query(QueryOptions{})
		if len(got) == 1 {
			if !strings.Contains(got[0].Msg, "sweep loop did not exit") {
				t.Fatalf("缓冲区内容 = %q", got[0].Msg)
			}
			if got[0].Level != plugin.LevelInfo {
				t.Errorf("级别 = %q，期望 info（不含 error/warn 关键字）", got[0].Level)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("logview 未收到日志（框架未把 log 分发给 LogSink？）")
		}
		time.Sleep(5 * time.Millisecond)
	}
	log.SetOutput(io.Discard) // fanout 已接管 log，收尾避免污染其它测试输出
}

// readLogFrame 读一帧 SSE 日志事件，返回 (event, data, id)。
func readLogFrame(t *testing.T, r *bufio.Reader) (event, data, id string) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读帧失败: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event == "" && data == "" && id == "" {
				continue
			}
			return event, data, id
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, ":"):
			continue // 心跳注释行
		}
	}
}
