package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// ---- rig ----

// webhookRig 装配 webhook（经 Manager，hub/bus 真实可用）。自持实例以便断言。
func webhookRig(t *testing.T, opts map[string]any) (*Webhook, *core.Hub) {
	t.Helper()
	hub := core.NewHub()
	wh := &Webhook{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{wh})
	mgr.ApplyConfig(map[string]bool{"webhook": true}, map[string]map[string]any{"webhook": opts})
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Cleanup(mgr.StopAll)
	return wh, hub
}

// captureTarget 是一个记录请求的 httptest 目标（可配置状态码与延迟）。
type captureTarget struct {
	mu       sync.Mutex
	got      []capturedReq
	status   int
	delay    time.Duration
}

type capturedReq struct {
	body []byte
	hdr  http.Header
}

func newTarget(status int) *captureTarget { return &captureTarget{status: status} }

func (c *captureTarget) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.got = append(c.got, capturedReq{body: body, hdr: r.Header.Clone()})
	d := c.delay
	c.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	w.WriteHeader(c.status)
	_, _ = w.Write([]byte("ok"))
}

func (c *captureTarget) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *captureTarget) last() capturedReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[len(c.got)-1]
}

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

// ---- 校验与解析 ----

// TestValidateSubscription：订阅字段校验。
func TestValidateSubscription(t *testing.T) {
	cases := []struct {
		name    string
		sub     subscription
		wantErr string
	}{
		{"ok", subscription{ID: "a", Event: protocol.TopicAgentOffline, URL: "http://x/hook"}, ""},
		{"no-id", subscription{Event: protocol.TopicAgentOffline, URL: "http://x"}, "id"},
		{"bad-id", subscription{ID: "a b", Event: protocol.TopicAgentOffline, URL: "http://x"}, "非法字符"},
		{"unknown-event", subscription{ID: "a", Event: "nope.topic", URL: "http://x"}, "已知 topic"},
		{"no-url", subscription{ID: "a", Event: protocol.TopicAgentOffline}, "url"},
		{"bad-url", subscription{ID: "a", Event: protocol.TopicAgentOffline, URL: "ftp://x"}, "http(s)"},
		{"bad-method", subscription{ID: "a", Event: protocol.TopicAgentOffline, URL: "http://x", Method: "DELETE"}, "POST/PUT"},
		{"put-ok", subscription{ID: "a", Event: protocol.TopicAgentOffline, URL: "http://x", Method: "PUT"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSubscription(&c.sub)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误 = %v, want 含 %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
		})
	}
}

// TestParseSubscriptions：配置解析 + method 缺省 POST。
func TestParseSubscriptions(t *testing.T) {
	subs, err := parseSubscriptions(`[{"id":"a","event":"agent.offline","url":"http://x/hook"}]`)
	if err != nil {
		t.Fatalf("parseSubscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].Method != http.MethodPost {
		t.Fatalf("解析不正确: %+v", subs)
	}
	if _, err := parseSubscriptions(`[{"id":"a","event":"bad","url":"http://x"}]`); err == nil {
		t.Fatal("未知 event 应报错")
	}
}

// ---- 派发与签名 ----

// TestDispatchDeliversWithSignature：真实事件 → 目标收到 POST，载荷/头/HMAC 签名正确。
func TestDispatchDeliversWithSignature(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, map[string]any{})
	// 注入零退避（本用例不触发重试，仅为一致性）。
	wh.sleep = func(time.Duration) {}

	rec := doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q,"secret":"s3cr3t"}`, srv.URL))
	if rec.Code != http.StatusOK {
		t.Fatalf("创建订阅应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}

	hub.Bus.Publish(protocol.TopicAgentOffline, "agent-x")

	waitFor(t, "目标收到投递", func() bool { return target.count() == 1 })
	req := target.last()

	// 载荷形状：{"event","ts","payload"}
	var got struct {
		Event   string `json:"event"`
		TS      int64  `json:"ts"`
		Payload any    `json:"payload"`
	}
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v (%s)", err, req.body)
	}
	if got.Event != protocol.TopicAgentOffline || got.TS == 0 || got.Payload != "agent-x" {
		t.Fatalf("载荷内容不正确: %s", req.body)
	}

	// 追踪头
	if req.hdr.Get(hdrEvent) != protocol.TopicAgentOffline {
		t.Fatalf("%s 头缺失/错误: %q", hdrEvent, req.hdr.Get(hdrEvent))
	}
	if req.hdr.Get(hdrDeliveryID) == "" || req.hdr.Get(hdrTimestamp) == "" {
		t.Fatal("deliveryId/timestamp 头缺失")
	}

	// HMAC 签名可校验
	sig := req.hdr.Get(hdrSignature)
	if sig == "" {
		t.Fatal("配置了 secret 但无签名头")
	}
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(req.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Fatalf("签名不匹配: got %q want %q", sig, want)
	}
}

// TestNoSecretNoSignatureHeader：未配置 secret 时不应出现签名头。
func TestNoSecretNoSignatureHeader(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, nil)
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"room.changed","url":%q}`, srv.URL))
	hub.Bus.Publish(protocol.TopicRoomChanged, map[string]string{"x": "y"})

	waitFor(t, "目标收到投递", func() bool { return target.count() == 1 })
	if sig := target.last().hdr.Get(hdrSignature); sig != "" {
		t.Fatalf("无 secret 不应带签名头，实得 %q", sig)
	}
}

// TestCustomHeadersForwarded：自定义头透传。
func TestCustomHeadersForwarded(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, nil)
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q,"headers":{"X-Token":"abc"}}`, srv.URL))
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")

	waitFor(t, "目标收到投递", func() bool { return target.count() == 1 })
	if got := target.last().hdr.Get("X-Token"); got != "abc" {
		t.Fatalf("自定义头未透传: %q", got)
	}
}

// ---- 重试与熔断 ----

// TestRetryThenUnhealthy：连续 500 → 按重试次数尝试，最终 unhealthy，历史含每次尝试。
func TestRetryThenUnhealthy(t *testing.T) {
	target := newTarget(500)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, map[string]any{"retries": 2})
	wh.sleep = func(time.Duration) {} // 退避归零，加速用例

	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")

	waitFor(t, "两次尝试都记录", func() bool {
		wh.mu.RLock()
		defer wh.mu.RUnlock()
		return len(wh.hist) == 2
	})
	if target.count() != 2 {
		t.Fatalf("应共 2 次 HTTP 尝试（1+1 重试），实得 %d", target.count())
	}
	wh.mu.RLock()
	h0, h1 := wh.hist[0], wh.hist[1]
	wh.mu.RUnlock()
	if h0.Attempt != 1 || h1.Attempt != 2 || h0.OK || h1.OK {
		t.Fatalf("历史记录不正确: %+v %+v", h0, h1)
	}
	if h1.Status != 500 {
		t.Fatalf("应记录 HTTP 500，实得 %d", h1.Status)
	}
	// 订阅标记 unhealthy
	wh.mu.RLock()
	healthy := wh.subs["wh1"].Healthy
	wh.mu.RUnlock()
	if healthy {
		t.Fatal("连续失败后订阅应 unhealthy")
	}
}

// TestCircuitBreakerOpensAndRecovers：连续失败达阈值 → 熔断暂停；到期后恢复尝试。
func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	target := newTarget(500)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, map[string]any{
		"retries": 1, "breakerFails": 2, "breakerPauseMs": 200,
	})
	wh.sleep = func(time.Duration) {}

	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))

	// 第 1、2 个事件：各失败一次 → fails=2 → 熔断开启
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	waitFor(t, "第 1 次失败", func() bool { return target.count() == 1 })
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	waitFor(t, "第 2 次失败", func() bool { return target.count() == 2 })

	// 第 3 个事件：熔断中，不发出，只记录 circuit open
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	waitFor(t, "circuit open 记录", func() bool {
		wh.mu.RLock()
		defer wh.mu.RUnlock()
		for _, d := range wh.hist {
			if d.Err == "circuit open" {
				return true
			}
		}
		return false
	})
	if target.count() != 2 {
		t.Fatalf("熔断期间不应再发 HTTP，实得 %d", target.count())
	}

	// 熔断到期（200ms）后：恢复尝试
	time.Sleep(260 * time.Millisecond)
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	waitFor(t, "恢复后再次尝试", func() bool { return target.count() == 3 })
}

// TestSuccessResetsBreaker：成功投递重置熔断计数与健康态。
func TestSuccessResetsBreaker(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, map[string]any{"retries": 1, "breakerFails": 5})
	wh.sleep = func(time.Duration) {}
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))

	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	waitFor(t, "投递成功", func() bool { return target.count() == 1 })

	wh.mu.RLock()
	brk := wh.brk[srv.URL]
	healthy := wh.subs["wh1"].Healthy
	wh.mu.RUnlock()
	if brk == nil || brk.fails != 0 || brk.openUntil != 0 {
		t.Fatalf("成功后熔断器应复位: %+v", brk)
	}
	if !healthy {
		t.Fatal("成功后订阅应 healthy")
	}
}

// ---- 历史与端点 ----

// TestHistoryRingCap：历史封顶（环形缓冲）。
func TestHistoryRingCap(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, map[string]any{"maxHistory": 10})
	wh.sleep = func(time.Duration) {}
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))

	for i := 0; i < 15; i++ {
		hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	}
	waitFor(t, "15 条全部投递", func() bool { return target.count() == 15 })

	wh.mu.RLock()
	n := len(wh.hist)
	wh.mu.RUnlock()
	if n != 10 {
		t.Fatalf("历史应封顶 10，实得 %d", n)
	}

	rec := doJSON(t, wh.handleDeliveries, http.MethodGet, "/api/webhook-deliveries", "")
	var resp struct {
		Deliveries []delivery `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码历史: %v", err)
	}
	if len(resp.Deliveries) != 10 {
		t.Fatalf("历史端点应返回 10 条，实得 %d", len(resp.Deliveries))
	}
}

// TestTestEndpoint：测试发送（单次尝试，合成载荷）。
func TestTestEndpoint(t *testing.T) {
	target := newTarget(204)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, _ := webhookRig(t, nil)
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))

	rec := doJSON(t, wh.handleWebhookOp, http.MethodPost, "/api/webhooks/wh1/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test 应 200，实得 %d", rec.Code)
	}
	var resp struct {
		OK     bool `json:"ok"`
		Status int  `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	if !resp.OK || resp.Status != 204 {
		t.Fatalf("test 结果不正确: %+v", resp)
	}
	waitFor(t, "目标收到合成事件", func() bool { return target.count() == 1 })
	var got map[string]any
	_ = json.Unmarshal(target.last().body, &got)
	payload, _ := got["payload"].(map[string]any)
	if payload["test"] != true {
		t.Fatalf("合成载荷不正确: %s", target.last().body)
	}
}

// TestCRUDAndUnsubscribeOnDelete：创建/列表/删除；删除后不再收到投递。
// 事件用 group.changed——core.NewHub() 自身订阅了 agent.online/offline
// （presence 广播），SubscriberCount 归零断言会被 hub 自身订阅干扰。
func TestCRUDAndUnsubscribeOnDelete(t *testing.T) {
	target := newTarget(200)
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, nil)
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":%q,"url":%q}`, protocol.TopicGroupChanged, srv.URL))

	// 列表
	rec := doJSON(t, wh.handleWebhooks, http.MethodGet, "/api/webhooks", "")
	var list struct {
		Webhooks []subscription `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解码列表: %v", err)
	}
	if len(list.Webhooks) != 1 || list.Webhooks[0].ID != "wh1" {
		t.Fatalf("列表不正确: %+v", list.Webhooks)
	}

	// 删除
	rec = doJSON(t, wh.handleWebhookOp, http.MethodDelete, "/api/webhooks/wh1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，实得 %d", rec.Code)
	}
	rec = doJSON(t, wh.handleWebhookOp, http.MethodDelete, "/api/webhooks/wh1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实得 %d", rec.Code)
	}

	// 删除后发布事件：不应再投递
	hub.Bus.Publish(protocol.TopicGroupChanged, "a")
	time.Sleep(150 * time.Millisecond)
	if target.count() != 0 {
		t.Fatalf("删除后不应再投递，实得 %d", target.count())
	}
	// 总线订阅已注销（hub 自身不订阅 group.changed，此断言干净）
	if n := hub.Bus.SubscriberCount(protocol.TopicGroupChanged); n != 0 {
		t.Fatalf("删除后总线订阅应清零，实得 %d", n)
	}
}

// TestCreateDuplicate409：重复 id → 409。
func TestCreateDuplicate409(t *testing.T) {
	wh, _ := webhookRig(t, nil)
	body := `{"id":"wh1","event":"agent.offline","url":"http://x/hook"}`
	if rec := doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks", body); rec.Code != http.StatusOK {
		t.Fatalf("首次创建应 200，实得 %d", rec.Code)
	}
	if rec := doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks", body); rec.Code != http.StatusConflict {
		t.Fatalf("重复创建应 409，实得 %d", rec.Code)
	}
}

// TestCreateBadRequest：非法创建 400（缺 id 不在此列——见 TestCreateAutoGeneratesID）。
func TestCreateBadRequest(t *testing.T) {
	wh, _ := webhookRig(t, nil)
	for _, body := range []string{
		`{"id":"wh1","url":"http://x"}`,                    // 缺 event
		`{"id":"wh1","event":"agent.offline"}`,             // 缺 url
		`{"id":"wh1","event":"bad.topic","url":"http://x"}`, // 未知 event
	} {
		if rec := doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s 应 400，实得 %d", body, rec.Code)
		}
	}
}

// TestCreateAutoGeneratesID：缺 id 时服务端自动生成 UUID（与 scheduler 一致的有意设计）。
func TestCreateAutoGeneratesID(t *testing.T) {
	wh, _ := webhookRig(t, nil)
	rec := doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		`{"event":"agent.offline","url":"http://x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("缺 id 应自动生成并 200，实得 %d body=%s", rec.Code, rec.Body.String())
	}
	var created subscription
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解码创建响应: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("应返回自动生成的 id，响应: %s", rec.Body.String())
	}
	if len(created.ID) != 24 {
		t.Fatalf("自动生成的 id 应为 24 位 hex（protocol.UUID）: %q", created.ID)
	}
	// 自动生成的 id 可再次命中（证明真的落库）
	if rec := doJSON(t, wh.handleWebhookOp, http.MethodDelete, "/api/webhooks/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("按生成的 id 删除应 200，实得 %d", rec.Code)
	}
}

// TestDispatchNonBlocking：慢目标不阻塞发布方（异步派发）。
func TestDispatchNonBlocking(t *testing.T) {
	target := newTarget(200)
	target.delay = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(target.handler))
	defer srv.Close()

	wh, hub := webhookRig(t, nil)
	wh.sleep = func(time.Duration) {}
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		fmt.Sprintf(`{"id":"wh1","event":"agent.offline","url":%q}`, srv.URL))

	start := time.Now()
	hub.Bus.Publish(protocol.TopicAgentOffline, "a")
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("发布方被慢目标阻塞: %v", elapsed)
	}
	waitFor(t, "慢目标最终仍收到", func() bool { return target.count() == 1 })
}

// TestApplyHotUpdateKeepsRuntimeSubs：热更新替换配置订阅、保留运行期订阅。
func TestApplyHotUpdateKeepsRuntimeSubs(t *testing.T) {
	wh, _ := webhookRig(t, map[string]any{
		"subscriptions": `[{"id":"cfg","event":"agent.offline","url":"http://x/hook"}]`,
	})
	// 运行期创建一个订阅
	doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
		`{"id":"rt","event":"room.changed","url":"http://y/hook"}`)

	if err := wh.Apply(map[string]any{
		"subscriptions": `[{"id":"cfg2","event":"agent.offline","url":"http://x/hook"}]`,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wh.mu.RLock()
	_, hasCfg := wh.subs["cfg"]
	_, hasCfg2 := wh.subs["cfg2"]
	_, hasRT := wh.subs["rt"]
	wh.mu.RUnlock()
	if hasCfg {
		t.Fatal("旧配置订阅应被替换")
	}
	if !hasCfg2 {
		t.Fatal("新配置订阅应生效")
	}
	if !hasRT {
		t.Fatal("运行期订阅应保留")
	}
	if err := wh.Apply(map[string]any{"subscriptions": `bad`}); err == nil {
		t.Fatal("热更新非法 JSON 应报错")
	}
}

// TestListConcurrentWithMarkResult：并发下列表编码与健康态写入互不破坏。
// 回归 D1（QA 独立验证抓出）：listWebhooks 曾锁外经 *subscription 指针编码，
// markResult 持锁写 Healthy 构成同字段并发读写；修为值拷贝后编码读的是快照。
// 有 -race 环境时本测试即竞争探针；无 race 环境也验证并发下形状与计数稳定。
func TestListConcurrentWithMarkResult(t *testing.T) {
	wh, _ := webhookRig(t, nil)
	for i := 0; i < 8; i++ {
		doJSON(t, wh.handleWebhooks, http.MethodPost, "/api/webhooks",
			fmt.Sprintf(`{"id":"wh%d","event":"agent.offline","url":"http://127.0.0.1:1/h%d"}`, i, i))
	}

	// 写侧：持续把订阅标记为不健康/健康（markResult 持锁写 Healthy）。
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			wh.mu.RLock()
			subs := make([]*subscription, 0, len(wh.subs))
			for _, s := range wh.subs {
				subs = append(subs, s)
			}
			wh.mu.RUnlock()
			for _, s := range subs {
				wh.markResult(s, s.URL, false)
				wh.markResult(s, s.URL, true)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// 读侧：4 个 goroutine 各做 200 次列表请求（编码在锁外进行）。
	errCh := make(chan error, 4)
	var readers sync.WaitGroup
	for g := 0; g < 4; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				rec := httptest.NewRecorder()
				wh.handleWebhooks(rec, httptest.NewRequest(http.MethodGet, "/api/webhooks", nil))
				if rec.Code != http.StatusOK {
					errCh <- fmt.Errorf("列表应 200，实得 %d", rec.Code)
					return
				}
				var list struct {
					Webhooks []subscription `json:"webhooks"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
					errCh <- fmt.Errorf("解码列表: %v", err)
					return
				}
				if len(list.Webhooks) != 8 {
					errCh <- fmt.Errorf("列表应 8 条，实得 %d", len(list.Webhooks))
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	<-writerDone

	select {
	case err := <-errCh:
		t.Fatalf("并发列表出错: %v", err)
	default:
	}
}
