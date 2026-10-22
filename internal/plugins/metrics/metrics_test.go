package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// stubGateway 满足 "metrics 依赖 httpgateway" 的拓扑约束。
type stubGateway struct{}

func (stubGateway) Meta() plugin.Meta          { return plugin.Meta{Name: "httpgateway", DefaultOn: true} }
func (stubGateway) Init(*plugin.Context) error { return nil }
func (stubGateway) Start() error               { return nil }
func (stubGateway) Stop() error                { return nil }

// metricsRig 是一套装配好的 metrics 插件 + 真实 HTTP 服务器。
type metricsRig struct {
	hub *core.Hub
	mt  *Metrics
	mgr *plugin.Manager
	srv *httptest.Server
}

func newMetricsRig(t *testing.T, options map[string]any) *metricsRig {
	t.Helper()
	hub := core.NewHub()
	hub.HTTPPort = 8081
	mt := &Metrics{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{mt, stubGateway{}})
	mgr.ApplyConfig(
		map[string]bool{"metrics": true, "httpgateway": true},
		map[string]map[string]any{"metrics": options},
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
	rig := &metricsRig{hub: hub, mt: mt, mgr: mgr, srv: srv}
	t.Cleanup(func() {
		mgr.StopAll()
		srv.Close()
	})
	return rig
}

// scrape 抓取 /metrics 并返回原始文本。
func (r *metricsRig) scrape(t *testing.T) string {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + pathMetrics)
	if err != nil {
		t.Fatalf("GET %s: %v", pathMetrics, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("Content-Type = %q，期望 text/plain; version=0.0.4", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// TestMetricsFormat 断言每个指标族都具备 # HELP / # TYPE / 值 三行，且每行可被逐行解析。
func TestMetricsFormat(t *testing.T) {
	rig := newMetricsRig(t, map[string]any{"interval": "200ms"})

	// 造一点流量：2 个 agent、1 个房间、3 条消息、2 次 HTTP 请求。
	sess := &noopSession{}
	rig.hub.Registry.Bind("A", "agentA", nil, protocol.ProtoCurrent, sess)
	rig.hub.Registry.Bind("B", "agentB", nil, protocol.ProtoCurrent, sess)
	rig.hub.Rooms.Create("r1", nil)
	for i := 0; i < 3; i++ {
		rig.hub.Router.Route(protocol.NewEnvelope(protocol.TypeBroadcast, "A", "", "", nil))
	}
	rig.hub.Bus.Publish(protocol.TopicHTTPRequest, plugin.HTTPRequest{Path: "/api/agents", Method: "GET"})
	rig.hub.Bus.Publish(protocol.TopicHTTPRequest, plugin.HTTPRequest{Path: "/api/agents", Method: "GET"})
	waitFor(t, "http 计数到 2", func() bool {
		return strings.Contains(rig.scrape(t), `hub_http_requests_total{path="/api/agents"} 2`)
	})

	body := rig.scrape(t)
	samples, helps, types := parseMetrics(t, body)

	families := []string{
		"hub_agents_online", "hub_rooms", "hub_messages_total", "hub_http_requests_total",
		"hub_plugin_uptime_seconds", "hub_events_dropped_total", "hub_sse_clients",
		"hub_sse_events_dropped_total",
	}
	for _, name := range families {
		if !helps[name] {
			t.Errorf("缺少 # HELP %s", name)
		}
		if !types[name] {
			t.Errorf("缺少 # TYPE %s", name)
		}
		hasValue := false
		for key := range samples {
			if key == name || strings.HasPrefix(key, name+"{") {
				hasValue = true
				break
			}
		}
		if !hasValue {
			t.Errorf("指标族 %s 没有值行", name)
		}
	}

	// 值正确性：与内核快照同源。
	if v := samples["hub_agents_online"]; v != 2 {
		t.Errorf("hub_agents_online = %v，期望 2", v)
	}
	if v := samples["hub_rooms"]; v != 1 {
		t.Errorf("hub_rooms = %v，期望 1", v)
	}
	if v := samples["hub_messages_total"]; v != 3 {
		t.Errorf("hub_messages_total = %v，期望 3", v)
	}
	if v := samples[`hub_http_requests_total{path="/api/agents"}`]; v != 2 {
		t.Errorf(`hub_http_requests_total{path="/api/agents"} = %v，期望 2`, v)
	}
	if v, ok := samples[`hub_plugin_uptime_seconds{plugin="metrics"}`]; !ok {
		t.Error("缺少 metrics 自身的 uptime 样本")
	} else if v < 0 {
		t.Errorf("uptime = %v，不应为负", v)
	}
	if _, ok := samples["hub_sse_clients"]; !ok {
		t.Error("缺少 hub_sse_clients（未启用 realtime 时应为 0）")
	}

	// 类型断言：counter/gauge 与实际语义一致。
	if !strings.Contains(body, "# TYPE hub_messages_total counter") {
		t.Error("hub_messages_total 应为 counter")
	}
	if !strings.Contains(body, "# TYPE hub_agents_online gauge") {
		t.Error("hub_agents_online 应为 gauge")
	}

	// 高基数防护：不应出现 agent 维度的标签。
	if strings.Contains(body, `agent="A"`) || strings.Contains(body, `id="A"`) {
		t.Error("指标里出现了 agentId 维度（会导致 Prometheus 基数爆炸）")
	}
}

// TestSummaryEndpoint 覆盖 /api/metrics/summary 的趋势采样与速率。
func TestSummaryEndpoint(t *testing.T) {
	rig := newMetricsRig(t, map[string]any{"interval": "200ms"})

	sess := &noopSession{}
	rig.hub.Registry.Bind("A", "agentA", nil, protocol.ProtoCurrent, sess)

	// 等至少两个采样点，速率才有意义。
	waitFor(t, "采样点 >= 2", func() bool {
		return len(rig.summary(t).Samples) >= 2
	})

	for i := 0; i < 4; i++ {
		rig.hub.Router.Route(protocol.NewEnvelope(protocol.TypeBroadcast, "A", "", "", nil))
	}
	waitFor(t, "消息计数到 4", func() bool { return rig.summary(t).MessagesTotal >= 4 })

	s := rig.summary(t)
	if s.AgentsOnline != 1 {
		t.Errorf("agentsOnline = %d，期望 1", s.AgentsOnline)
	}
	if s.IntervalMs != 200 {
		t.Errorf("intervalMs = %d，期望 200", s.IntervalMs)
	}
	if s.UptimeMs < 0 {
		t.Errorf("uptimeMs = %d", s.UptimeMs)
	}
	if len(s.Samples) > sampleCount {
		t.Errorf("采样点 = %d，超过上限 %d", len(s.Samples), sampleCount)
	}
	for i := 1; i < len(s.Samples); i++ {
		if s.Samples[i].TS < s.Samples[i-1].TS {
			t.Fatalf("采样点未按时间正序: %+v", s.Samples)
		}
	}
	if s.MessagesPerSecond < 0 {
		t.Errorf("messagesPerSecond = %v，不应为负", s.MessagesPerSecond)
	}
}

// summary 拉取并解析 /api/metrics/summary。
func (r *metricsRig) summary(t *testing.T) summaryResponse {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + pathSummary)
	if err != nil {
		t.Fatalf("GET %s: %v", pathSummary, err)
	}
	defer resp.Body.Close()
	var s summaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("解析 summary: %v", err)
	}
	return s
}

// TestRatePerSecond 单测速率计算（含边界：采样点不足 / 时间倒退）。
func TestRatePerSecond(t *testing.T) {
	if got := ratePerSecond(nil); got != 0 {
		t.Errorf("空采样 = %v，期望 0", got)
	}
	if got := ratePerSecond([]Sample{{TS: 1, MessagesTotal: 10}}); got != 0 {
		t.Errorf("单点采样 = %v，期望 0", got)
	}
	got := ratePerSecond([]Sample{
		{TS: 1000, MessagesTotal: 10},
		{TS: 3000, MessagesTotal: 30},
	})
	if got != 10 {
		t.Errorf("速率 = %v，期望 10/s", got)
	}
	// 计数器回退（重置）时不产生负速率。
	if got := ratePerSecond([]Sample{
		{TS: 1000, MessagesTotal: 30},
		{TS: 2000, MessagesTotal: 10},
	}); got != 0 {
		t.Errorf("回退场景速率 = %v，期望 0", got)
	}
}

// TestLabelCardinalityCap 断言 path 标签数量超限后归入 other。
func TestLabelCardinalityCap(t *testing.T) {
	mt := &Metrics{} // 不装配：这里只验证纯计数逻辑（countHTTP 会懒建 map）
	for i := 0; i < maxLabelPaths+10; i++ {
		mt.countHTTP(plugin.HTTPRequest{Path: "/p/" + strconv.Itoa(i), Method: "GET"})
	}
	body := mt.render()
	if !strings.Contains(body, `hub_http_requests_total{path="other"} 10`) {
		t.Errorf("超出基数上限的请求应聚合到 other：\n%s", body)
	}
}

// parseMetrics 逐行解析指标文本，返回 样本→值、HELP 集合、TYPE 集合。
func parseMetrics(t *testing.T, body string) (samples map[string]float64, helps, types map[string]bool) {
	t.Helper()
	samples = make(map[string]float64)
	helps = make(map[string]bool)
	types = make(map[string]bool)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# HELP "):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				t.Fatalf("HELP 行不合法: %q", line)
			}
			helps[fields[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("TYPE 行不合法: %q", line)
			}
			if fields[3] != "gauge" && fields[3] != "counter" {
				t.Fatalf("未知指标类型: %q", line)
			}
			types[fields[2]] = true
		case strings.HasPrefix(line, "#"):
			t.Fatalf("未知注释行: %q", line)
		default:
			key, value, ok := splitSample(line)
			if !ok {
				t.Fatalf("样本行不合法: %q", line)
			}
			samples[key] = value
		}
	}
	return samples, helps, types
}

// splitSample 把 `name{k="v"} 1.5` 拆成 (key, value)。
func splitSample(line string) (string, float64, bool) {
	idx := strings.LastIndex(line, " ")
	if idx <= 0 {
		return "", 0, false
	}
	key := strings.TrimSpace(line[:idx])
	raw := strings.TrimSpace(line[idx+1:])
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", 0, false
	}
	return key, v, true
}

// waitFor 轮询等待条件成立。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noopSession 是只做空投递的 Session 替身。
type noopSession struct{}

func (noopSession) ID() string                { return "noop" }
func (noopSession) Deliver(protocol.Envelope) {}
func (noopSession) Close() error              { return nil }
