package healthz

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// stubGateway 满足 "healthz 依赖 httpgateway" 的拓扑约束。
type stubGateway struct{}

func (stubGateway) Meta() plugin.Meta          { return plugin.Meta{Name: "httpgateway", DefaultOn: true} }
func (stubGateway) Init(*plugin.Context) error { return nil }
func (stubGateway) Start() error               { return nil }
func (stubGateway) Stop() error                { return nil }

// requiredOK 是能正常启动的「必需插件」。
type requiredOK struct{}

func (requiredOK) Meta() plugin.Meta {
	return plugin.Meta{Name: "reqok", DefaultOn: true, Required: true, RequiredReason: "测试用必需插件"}
}
func (requiredOK) Init(*plugin.Context) error { return nil }
func (requiredOK) Start() error               { return nil }
func (requiredOK) Stop() error                { return nil }

// requiredBroken 是 Init 必然失败的「必需插件」（模拟端口被占等场景）。
type requiredBroken struct{}

func (requiredBroken) Meta() plugin.Meta {
	return plugin.Meta{Name: "reqbad", DefaultOn: true, Required: true, RequiredReason: "测试用必需插件"}
}
func (requiredBroken) Init(*plugin.Context) error {
	return errors.New("bind :8082: address already in use")
}
func (requiredBroken) Start() error { return nil }
func (requiredBroken) Stop() error  { return nil }

// hzRig 是一套装配好的 healthz 插件 + 真实 HTTP 服务器。
type hzRig struct {
	hub *core.Hub
	mgr *plugin.Manager
	srv *httptest.Server
}

func newHZRig(t *testing.T, extra ...plugin.Plugin) *hzRig {
	t.Helper()
	hub := core.NewHub()
	hub.HTTPPort = 8081
	hub.TCPPort = 8080

	plugins := append([]plugin.Plugin{&Healthz{}, stubGateway{}}, extra...)
	mgr := plugin.NewManagerWith(plugins)
	enabled := map[string]bool{"healthz": true, "httpgateway": true}
	for _, p := range extra {
		enabled[p.Meta().Name] = true
	}
	mgr.ApplyConfig(enabled, nil)
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	// 必需插件失败时 StartAll 会返回错误——这正是被测场景，不能当致命错误。
	_, _ = mgr.StartAll()

	srv := httptest.NewServer(mgr.Mux())
	rig := &hzRig{hub: hub, mgr: mgr, srv: srv}
	t.Cleanup(func() {
		mgr.StopAll()
		srv.Close()
	})
	return rig
}

// get 发一个 GET 并返回 (状态码, 响应体)。
func (r *hzRig) get(t *testing.T, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header
}

// TestHealthzIsMinimal 断言 /healthz 极轻：固定 200 + 常量体，不含任何动态字段。
func TestHealthzIsMinimal(t *testing.T) {
	rig := newHZRig(t)
	code, body, hdr := rig.get(t, pathHealthz)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}
	if strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("响应体 = %q，期望常量 {\"status\":\"ok\"}（存活探针不读任何状态）", body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// TestHealthzVerboseIncludesDetail 断言 ?verbose=1 输出明细。
func TestHealthzVerboseIncludesDetail(t *testing.T) {
	rig := newHZRig(t)
	code, body, _ := rig.get(t, pathHealthz+"?verbose=1")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	var d detailResponse
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析明细: %v", err)
	}
	if len(d.Plugins) == 0 {
		t.Fatal("verbose 明细应包含插件清单")
	}
	for _, key := range []string{"httpPort", "agents", "rooms"} {
		if _, ok := d.Checks[key]; !ok {
			t.Errorf("checks 缺少 %q", key)
		}
	}
}

// TestReadyzOKWhenRequiredRunning 断言必需插件都在运行时 /readyz 返回 200。
func TestReadyzOKWhenRequiredRunning(t *testing.T) {
	rig := newHZRig(t, requiredOK{})
	code, body, _ := rig.get(t, pathReadyz)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", code, body)
	}
	var resp struct {
		Status  string                 `json:"status"`
		Checks  map[string]checkResult `json:"checks"`
		Plugins []pluginHealth         `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析 readyz: %v", err)
	}
	if resp.Status != "ready" {
		t.Errorf("status = %q，期望 ready", resp.Status)
	}
	for _, key := range []string{"httpPort", "agents", "rooms"} {
		c, ok := resp.Checks[key]
		if !ok {
			t.Fatalf("checks 缺少 %q", key)
		}
		if !c.OK {
			t.Errorf("check %q 应通过：%+v", key, c)
		}
	}
}

// TestReadyzNotReadyWhenRequiredFailed 断言必需插件非 running 时 /readyz 返回 503。
func TestReadyzNotReadyWhenRequiredFailed(t *testing.T) {
	rig := newHZRig(t, requiredBroken{})
	code, body, _ := rig.get(t, pathReadyz)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503：%s", code, body)
	}
	var resp struct {
		Status   string   `json:"status"`
		Blockers []string `json:"blockers"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析 readyz: %v", err)
	}
	if resp.Status != "not ready" {
		t.Errorf("status = %q，期望 not ready", resp.Status)
	}
	if len(resp.Blockers) != 1 || resp.Blockers[0] != "reqbad" {
		t.Errorf("blockers = %v，期望 [reqbad]", resp.Blockers)
	}
}

// TestHealthDetailReportsFailureReason 断言详情能看出「哪个插件、什么原因」。
func TestHealthDetailReportsFailureReason(t *testing.T) {
	rig := newHZRig(t, requiredBroken{})
	code, body, _ := rig.get(t, pathDetail)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（诊断详情本身永远可读）", code)
	}
	var d detailResponse
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析详情: %v", err)
	}
	if d.Status != "degraded" {
		t.Errorf("status = %q，期望 degraded", d.Status)
	}
	var found bool
	for _, ph := range d.Plugins {
		if ph.Name == "reqbad" {
			found = true
			if ph.State != plugin.StateFailed.String() {
				t.Errorf("reqbad.state = %q，期望 failed", ph.State)
			}
			if !strings.Contains(ph.Error, "address already in use") {
				t.Errorf("reqbad.error = %q，应包含原始失败原因", ph.Error)
			}
			if !ph.Required {
				t.Error("reqbad 应标记 required=true")
			}
		}
	}
	if !found {
		t.Fatal("详情里找不到失败插件 reqbad")
	}
	if d.Version == "" {
		t.Error("version 不应为空")
	}
	if d.UptimeMs < 0 {
		t.Errorf("uptimeMs = %d", d.UptimeMs)
	}
	if len(d.Blockers) != 1 {
		t.Errorf("blockers = %v，期望 1 个", d.Blockers)
	}
}

// TestProbesAreUnobserved 断言探针端点不参与 HTTP 访问计数（不会把 /metrics 刷成自画像）。
func TestProbesAreUnobserved(t *testing.T) {
	hub := core.NewHub()
	mgr := plugin.NewManagerWith([]plugin.Plugin{&Healthz{}, stubGateway{}})
	mgr.ApplyConfig(map[string]bool{"healthz": true, "httpgateway": true}, nil)
	mgr.SetEventBus(hub.Bus) // 注入真实总线，观察是否有人发布 http.request

	var mu sync.Mutex
	var paths []string
	hub.Bus.Subscribe("http.request", func(_ string, payload any) {
		if req, ok := payload.(plugin.HTTPRequest); ok {
			mu.Lock()
			paths = append(paths, req.Path)
			mu.Unlock()
		}
	})
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	srv := httptest.NewServer(mgr.Mux())
	defer func() {
		mgr.StopAll()
		srv.Close()
	}()

	for _, path := range []string{pathHealthz, pathReadyz, pathDetail} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("探针端点不应发布 http.request，实际发布了 %v", got)
	}
}
