package capdir

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"lan-agent-hub/internal/plugin"
)

// stubView 是 plugin.HubView 的内存实现（仅 Agents 有数据）。
type stubView struct{ agents []plugin.AgentInfo }

func (s stubView) Agents() []plugin.AgentInfo    { return s.agents }
func (s stubView) Rooms() []plugin.RoomInfo      { return nil }
func (s stubView) Stats() plugin.HubStats        { return plugin.HubStats{} }

func testView() stubView {
	return stubView{agents: []plugin.AgentInfo{
		{ID: "A", Name: "Alice", Caps: []string{"python", "camera"}},
		{ID: "B", Name: "Bob", Caps: []string{"Python"}}, // 大小写变体：应归并
		{ID: "C", Name: "Carol", Caps: nil},              // 无 caps：不进索引
	}}
}

// TestBuildIndex：多 cap agent 出现在各能力列表；大小写归并；按 ID 升序；无 caps 不进索引。
func TestBuildIndex(t *testing.T) {
	idx := buildIndex(testView().agents)
	want := map[string][]string{
		"python": {"A", "B"},
		"camera": {"A"},
	}
	if !reflect.DeepEqual(idx, want) {
		t.Fatalf("索引不正确:\n got %v\nwant %v", idx, want)
	}
}

// TestBuildIndexEmpty：空输入 → 空索引（非 nil，JSON 序列化为 {}）。
func TestBuildIndexEmpty(t *testing.T) {
	idx := buildIndex(nil)
	if len(idx) != 0 {
		t.Fatalf("空输入应得空索引，实得 %v", idx)
	}
}

// TestHandleCapabilities：GET /api/capabilities 汇总视图。
func TestHandleCapabilities(t *testing.T) {
	p := &CapDir{view: testView()}
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	rec := httptest.NewRecorder()
	p.handleCapabilities(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	var resp struct {
		Capabilities map[string][]string `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(resp.Capabilities["python"], []string{"A", "B"}) {
		t.Fatalf("python 列表不正确: %v", resp.Capabilities["python"])
	}
	if !reflect.DeepEqual(resp.Capabilities["camera"], []string{"A"}) {
		t.Fatalf("camera 列表不正确: %v", resp.Capabilities["camera"])
	}
}

// TestHandleCapabilityCaseInsensitive：查询大小写不敏感（PYTHON → python）。
func TestHandleCapabilityCaseInsensitive(t *testing.T) {
	p := &CapDir{view: testView()}
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities/PYTHON", nil)
	rec := httptest.NewRecorder()
	p.handleCapability(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	var resp struct {
		Capability string   `json:"capability"`
		Agents     []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Capability != "python" {
		t.Fatalf("capability 应归一化为小写: %q", resp.Capability)
	}
	if !reflect.DeepEqual(resp.Agents, []string{"A", "B"}) {
		t.Fatalf("agents 不正确: %v", resp.Agents)
	}
}

// TestHandleCapabilityUnknown：未知能力返回 200 + 空列表（归一化为 [] 而非 null）。
func TestHandleCapabilityUnknown(t *testing.T) {
	p := &CapDir{view: testView()}
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities/nosuch", nil)
	rec := httptest.NewRecorder()
	p.handleCapability(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	var resp struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Agents == nil || len(resp.Agents) != 0 {
		t.Fatalf("未知能力应返回空列表（非 null）: %v", resp.Agents)
	}
}

// TestHandleCapabilityMissing：空能力名返回 400。
func TestHandleCapabilityMissing(t *testing.T) {
	p := &CapDir{view: testView()}
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities/", nil)
	rec := httptest.NewRecorder()
	p.handleCapability(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400", rec.Code)
	}
}

// TestHandlersMethodNotAllowed：POST 到 GET 端点返回 405。
func TestHandlersMethodNotAllowed(t *testing.T) {
	p := &CapDir{view: testView()}
	for _, path := range []string{"/api/capabilities", "/api/capabilities/python"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		if path == "/api/capabilities" {
			p.handleCapabilities(rec, req)
		} else {
			p.handleCapability(rec, req)
		}
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s POST 状态码 = %d, want 405", path, rec.Code)
		}
	}
}

// TestNilViewDegrades：view 为 nil（未接线）时降级为空目录，不 panic。
func TestNilViewDegrades(t *testing.T) {
	p := &CapDir{}
	req := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	rec := httptest.NewRecorder()
	p.handleCapabilities(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
}
