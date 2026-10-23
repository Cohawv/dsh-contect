// Package webhook 实现「事件外发回调」插件（V6 ③）。
//
// 事件发生时回调外部 URL——与其它系统集成的标准手段，零依赖（net/http + crypto/hmac）。
// 典型用途：Agent 下线时推一条到企业微信/钉钉群。
//
// 订阅模型：
//
//	{"id":"wecom","event":"agent.offline","url":"http://x/hook","method":"POST",
//	 "headers":{"X-Token":"abc"},"secret":"s3cr3t"}
//
// 行为契约：
//   - 事件经 EventBus.SubscribeAsync 进入，**异步派发**：慢/挂起的目标不阻塞 Hub；
//   - 载荷 {"event","ts","payload"}（payload 即总线事件的原始载荷）；
//   - 签名：X-Hub-Signature = "sha256=" + HMAC-SHA256(secret, body)（配置了 secret 时），
//     便于接收方校验来源（功能完整性，非安全设计——不涉及鉴权/加密存储）；
//   - 失败按退避重试（默认 3 次：500ms / 2s）；
//   - 熔断：同一 URL 连续失败 N 次（默认 10）后暂停 M 毫秒（默认 5 分钟），
//     暂停期间的投递记 "circuit open" 不发出；
//   - 订阅健康态：一次完整投递周期（含重试）失败记 unhealthy，成功恢复 healthy；
//   - 投递历史：环形缓冲（默认 50 条，含状态码/耗时/第几次尝试）。
//
// 持久化：配置态订阅走 `subscriptions` JSON 选项（落 hub.json，重启恢复）；
// 运行期经 API 创建的订阅驻留内存（与 group/scheduler 同策略）。
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

const (
	pathWebhooks       = "/api/webhooks"
	pathWebhookPrefix  = "/api/webhooks/"
	pathWHdeliveries   = "/api/webhook-deliveries"
	idCharset          = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
	maxIDLen           = 64
	defaultRetries     = 3
	minRetries         = 1
	maxRetries         = 10
	defaultTimeoutMs   = 5000
	defaultMaxHistory  = 50
	defaultBreakerFails = 10
	defaultBreakerPauseMs = 300000

	// 签名与追踪头。
	hdrSignature  = "X-Hub-Signature"
	hdrEvent      = "X-Hub-Event"
	hdrDeliveryID = "X-Hub-Delivery-Id"
	hdrTimestamp  = "X-Hub-Timestamp"
)

// subscription 是一条订阅（配置/API 形态 + 运行态健康）。
type subscription struct {
	ID      string            `json:"id"`
	Event   string            `json:"event"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Secret  string            `json:"secret,omitempty"`
	Healthy bool              `json:"healthy"`

	fromConfig bool `json:"-"`
}

// delivery 是一条投递历史记录。
type delivery struct {
	TS      int64  `json:"ts"`
	SubID   string `json:"subId"`
	Event   string `json:"event"`
	URL     string `json:"url"`
	Status  int    `json:"status"` // HTTP 状态码；0 = 未发出（熔断/网络错）
	OK      bool   `json:"ok"`
	MS      int64  `json:"ms"`
	Attempt int    `json:"attempt"`
	Err     string `json:"err,omitempty"`
}

// urlBreaker 是「按 URL」的熔断状态。
type urlBreaker struct {
	fails     int
	openUntil int64 // 熔断截止（毫秒）；0 = 关闭
}

// Webhook 是 webhook 插件实体。
type Webhook struct {
	hub *core.Hub

	retries       int
	timeout       time.Duration
	maxHistory    int
	breakerFails  int
	breakerPause  time.Duration
	client        *http.Client
	now           func() int64
	sleep         func(time.Duration) // 可注入（测试加速退避）

	mu     sync.RWMutex
	subs   map[string]*subscription
	unsubs map[string]func() // topic → 退订函数（同一 topic 多订阅只订一次）
	hist   []delivery        // 环形缓冲（新末尾）
	brk    map[string]*urlBreaker
}

// Meta 声明插件元信息。
func (p *Webhook) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "webhook",
		Version:     "1.0.0",
		Description: "事件外发回调（纯标准库；零外部依赖）：事件推外部 URL，HMAC 签名、退避重试、熔断、投递历史",
		DefaultOn:   true,
	}
}

// Options 声明配置项。
func (p *Webhook) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:     "subscriptions",
			Type:    plugin.OptionString,
			Default: "[]",
			Description: "订阅列表（JSON 数组）：[{\"id\",\"event\":\"topic 名\",\"url\"," +
				"\"method\"?,\"headers\"?,\"secret\"?}]；event 必须是已知 topic",
		},
		{Key: "retries", Type: plugin.OptionInt, Default: defaultRetries, Min: minRetries, Max: maxRetries,
			Description: "投递尝试总次数（含首次；1=不重试。两次尝试间退避 500ms/2s）"},
		{Key: "timeoutMs", Type: plugin.OptionInt, Default: defaultTimeoutMs, Min: 100, Max: 60000,
			Description: "单次投递超时（毫秒）"},
		{Key: "maxHistory", Type: plugin.OptionInt, Default: defaultMaxHistory, Min: 10, Max: 500,
			Description: "投递历史环形缓冲条数"},
		{Key: "breakerFails", Type: plugin.OptionInt, Default: defaultBreakerFails, Min: 1, Max: 1000,
			Description: "同一 URL 连续失败这么多次后触发熔断"},
		{Key: "breakerPauseMs", Type: plugin.OptionInt, Default: defaultBreakerPauseMs, Min: 100, Max: 3600000,
			Description: "熔断暂停时长（毫秒）"},
	}
}

// Init 注入能力、解析配置订阅、订阅总线 topic、注册端点。
func (p *Webhook) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("webhook: missing *core.Hub in context")
	}
	p.hub = hub
	p.retries = clampInt(ctx.Int("retries", defaultRetries), minRetries, maxRetries)
	p.timeout = time.Duration(clampInt(ctx.Int("timeoutMs", defaultTimeoutMs), 100, 60000)) * time.Millisecond
	p.maxHistory = clampInt(ctx.Int("maxHistory", defaultMaxHistory), 10, 500)
	p.breakerFails = clampInt(ctx.Int("breakerFails", defaultBreakerFails), 1, 1000)
	p.breakerPause = time.Duration(clampInt(ctx.Int("breakerPauseMs", defaultBreakerPauseMs), 100, 3600000)) * time.Millisecond
	p.client = &http.Client{Timeout: p.timeout}
	p.now = func() int64 { return time.Now().UnixMilli() }
	p.sleep = time.Sleep
	p.subs = make(map[string]*subscription)
	p.unsubs = make(map[string]func())
	p.brk = make(map[string]*urlBreaker)

	subs, err := parseSubscriptions(ctx.String("subscriptions", "[]"))
	if err != nil {
		return fmt.Errorf("webhook: 解析 subscriptions 配置失败: %w", err)
	}

	if err := ctx.RegisterHTTP(pathWebhooks, p.handleWebhooks); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathWebhookPrefix, p.handleWebhookOp); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathWHdeliveries, p.handleDeliveries); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range subs {
		s.fromConfig = true
		p.subs[s.ID] = s
	}
	p.resubscribeLocked()
	return nil
}

// Start / Stop 无后台任务；Stop 退订全部总线订阅。
func (p *Webhook) Start() error { return nil }

func (p *Webhook) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for topic, unsub := range p.unsubs {
		if unsub != nil {
			unsub()
		}
		delete(p.unsubs, topic)
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新配置订阅（运行期订阅保留）。
func (p *Webhook) Apply(options map[string]any) error {
	v, ok := options["subscriptions"]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("webhook: subscriptions 选项必须是 JSON 字符串")
	}
	subs, err := parseSubscriptions(s)
	if err != nil {
		return fmt.Errorf("webhook: 热更新 subscriptions 失败: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, sub := range p.subs {
		if sub.fromConfig {
			delete(p.subs, id)
		}
	}
	for _, s := range subs {
		s.fromConfig = true
		p.subs[s.ID] = s
	}
	p.resubscribeLocked()
	return nil
}

// resubscribeLocked 按当前订阅集合调整总线订阅（新 topic 订、无订阅的 topic 退）。
// 调用方持写锁。必须在锁外调用 unsub 时注意：退订函数本身不回调本插件，安全。
func (p *Webhook) resubscribeLocked() {
	want := make(map[string]bool)
	for _, s := range p.subs {
		want[s.Event] = true
	}
	for topic, unsub := range p.unsubs {
		if !want[topic] && unsub != nil {
			unsub()
		}
	}
	// 删除不再需要的（不能在 range 中删，先收集）。
	for topic := range p.unsubs {
		if !want[topic] {
			delete(p.unsubs, topic)
		}
	}
	for topic := range want {
		if _, ok := p.unsubs[topic]; ok {
			continue
		}
		topic := topic
		p.unsubs[topic] = p.hub.Bus.SubscribeAsync(topic, func(_ string, payload any) {
			p.dispatch(topic, payload)
		}, 64)
	}
}

// dispatch 把一条事件派发给所有订阅该 topic 的订阅（每个订阅独立 goroutine，
// 互不阻塞；同一订阅内串行，保序）。
func (p *Webhook) dispatch(topic string, payload any) {
	p.mu.RLock()
	targets := make([]*subscription, 0, 2)
	for _, s := range p.subs {
		if s.Event == topic {
			targets = append(targets, s)
		}
	}
	p.mu.RUnlock()
	for _, s := range targets {
		go p.deliver(s, topic, payload)
	}
}

// deliver 执行一次完整投递周期（含重试与熔断），并记录历史/健康/熔断状态。
func (p *Webhook) deliver(sub *subscription, topic string, payload any) {
	body, err := json.Marshal(map[string]any{
		"event":   topic,
		"ts":      p.now(),
		"payload": payload,
	})
	if err != nil {
		p.record(delivery{TS: p.now(), SubID: sub.ID, Event: topic, URL: sub.URL,
			OK: false, Attempt: 0, Err: "marshal: " + err.Error()})
		return
	}

	// 熔断检查（按 URL）：暂停期间不发出，只记录。
	p.mu.RLock()
	brk := p.brk[sub.URL]
	p.mu.RUnlock()
	if brk != nil && brk.openUntil > p.now() {
		p.record(delivery{TS: p.now(), SubID: sub.ID, Event: topic, URL: sub.URL,
			OK: false, Attempt: 0, Err: "circuit open"})
		return
	}

	deliveryID := protocol.UUID()
	var lastErr error
	for attempt := 1; attempt <= p.retries; attempt++ {
		start := p.now()
		status, err := p.post(sub, topic, body, deliveryID)
		ms := p.now() - start
		ok := err == nil && status >= 200 && status < 300
		rec := delivery{TS: start, SubID: sub.ID, Event: topic, URL: sub.URL,
			Status: status, OK: ok, MS: ms, Attempt: attempt}
		if !ok {
			if err != nil {
				rec.Err = err.Error()
				lastErr = err
			} else {
				rec.Err = fmt.Sprintf("http %d", status)
				lastErr = fmt.Errorf("http %d", status)
			}
		}
		p.record(rec)
		if ok {
			p.markResult(sub, sub.URL, true)
			return
		}
		if attempt < p.retries {
			p.sleep(backoffFor(attempt))
		}
	}
	_ = lastErr
	p.markResult(sub, sub.URL, false)
}

// post 发一次 HTTP 请求，返回状态码与错误（网络错时状态码为 0）。
func (p *Webhook) post(sub *subscription, topic string, body []byte, deliveryID string) (int, error) {
	method := sub.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, sub.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrEvent, topic)
	req.Header.Set(hdrDeliveryID, deliveryID)
	req.Header.Set(hdrTimestamp, strconv.FormatInt(p.now(), 10))
	for k, v := range sub.Headers {
		req.Header.Set(k, v)
	}
	if sub.Secret != "" {
		mac := hmac.New(sha256.New, []byte(sub.Secret))
		mac.Write(body)
		req.Header.Set(hdrSignature, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // 耗尽以便连接复用
	return resp.StatusCode, nil
}

// markResult 更新订阅健康态与 URL 熔断器。
func (p *Webhook) markResult(sub *subscription, url string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	brk := p.brk[url]
	if brk == nil {
		brk = &urlBreaker{}
		p.brk[url] = brk
	}
	if ok {
		brk.fails = 0
		brk.openUntil = 0
		sub.Healthy = true
		return
	}
	brk.fails++
	sub.Healthy = false
	if brk.fails >= p.breakerFails {
		brk.openUntil = p.now() + p.breakerPause.Milliseconds()
	}
}

// record 追加一条投递历史（环形缓冲，调用方不需持锁）。
func (p *Webhook) record(d delivery) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hist = append(p.hist, d)
	if len(p.hist) > p.maxHistory {
		p.hist = p.hist[len(p.hist)-p.maxHistory:]
	}
}

// backoffFor 第 attempt 次失败后的退避（attempt 从 1 起）。
func backoffFor(attempt int) time.Duration {
	if attempt <= 1 {
		return 500 * time.Millisecond
	}
	return 2 * time.Second
}

// ---- 端点 ----

// handleWebhooks 处理 GET /api/webhooks（列表）与 POST（创建）。
func (p *Webhook) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.listWebhooks(w)
	case http.MethodPost:
		p.createWebhook(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listWebhooks 返回全部订阅（含健康态），按 ID 升序。
// 值拷贝快照：编码在锁外进行，若直接拷指针，markResult 持锁写 Healthy 会与
// JSON 编码读 Healthy 构成数据竞争（QA 独立验证抓出，D1）。
func (p *Webhook) listWebhooks(w http.ResponseWriter) {
	p.mu.RLock()
	out := make([]subscription, 0, len(p.subs))
	for _, s := range p.subs {
		out = append(out, *s)
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, map[string]any{"webhooks": out})
}

// createWebhook 处理 POST /api/webhooks：创建订阅（id 已存在 → 409）。
func (p *Webhook) createWebhook(w http.ResponseWriter, r *http.Request) {
	var s subscription
	if err := decodeJSON(r, &s); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.ID == "" {
		s.ID = protocol.UUID()
	}
	if err := validateSubscription(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Method == "" {
		s.Method = http.MethodPost
	}
	s.Healthy = true

	p.mu.Lock()
	if _, dup := p.subs[s.ID]; dup {
		p.mu.Unlock()
		http.Error(w, "webhook id already exists: "+s.ID, http.StatusConflict)
		return
	}
	p.subs[s.ID] = &s
	p.resubscribeLocked()
	p.mu.Unlock()
	writeJSON(w, &s)
}

// handleWebhookOp 处理 {pathWebhookPrefix} 余量：
//   - POST   /api/webhooks/{id}/test —— 测试发送（单次尝试，不走重试/熔断）
//   - DELETE /api/webhooks/{id}      —— 删除
func (p *Webhook) handleWebhookOp(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, pathWebhookPrefix), "/")
	if rest == "" {
		http.Error(w, "missing webhook id", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(rest, "/test") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p.testWebhook(w, strings.TrimSuffix(rest, "/test"))
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.deleteWebhook(w, rest)
}

// testWebhook 处理 POST /api/webhooks/{id}/test：发一条合成事件（单次尝试）。
func (p *Webhook) testWebhook(w http.ResponseWriter, id string) {
	p.mu.RLock()
	sub, ok := p.subs[id]
	p.mu.RUnlock()
	if !ok {
		http.Error(w, "unknown webhook: "+id, http.StatusNotFound)
		return
	}
	body, _ := json.Marshal(map[string]any{
		"event":   sub.Event,
		"ts":      p.now(),
		"payload": map[string]any{"test": true, "subId": id},
	})
	start := p.now()
	status, err := p.post(sub, sub.Event, body, protocol.UUID())
	ms := p.now() - start
	good := err == nil && status >= 200 && status < 300
	p.record(delivery{TS: start, SubID: id, Event: sub.Event, URL: sub.URL,
		Status: status, OK: good, MS: ms, Attempt: 1, Err: errStr(err)})
	resp := map[string]any{"id": id, "ok": good, "status": status, "ms": ms}
	if err != nil {
		resp["err"] = err.Error()
	}
	writeJSON(w, resp)
}

// deleteWebhook 处理 DELETE /api/webhooks/{id}。
func (p *Webhook) deleteWebhook(w http.ResponseWriter, id string) {
	p.mu.Lock()
	_, ok := p.subs[id]
	if ok {
		delete(p.subs, id)
		p.resubscribeLocked()
	}
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown webhook: "+id, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"deleted": id})
}

// handleDeliveries 处理 GET /api/webhook-deliveries：投递历史（旧→新）。
func (p *Webhook) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.mu.RLock()
	out := make([]delivery, len(p.hist))
	copy(out, p.hist)
	p.mu.RUnlock()
	writeJSON(w, map[string]any{"deliveries": out})
}

// ---- 配置解析与校验 ----

// parseSubscriptions 解析 subscriptions 配置 JSON 数组。
func parseSubscriptions(s string) ([]*subscription, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var subs []*subscription
	if err := json.Unmarshal([]byte(s), &subs); err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if err := validateSubscription(sub); err != nil {
			return nil, err
		}
		if sub.Method == "" {
			sub.Method = http.MethodPost
		}
	}
	return subs, nil
}

// validateSubscription 校验订阅字段。
func validateSubscription(s *subscription) error {
	if s.ID == "" {
		return fmt.Errorf("订阅 id 不能为空")
	}
	if !validID(s.ID) {
		return fmt.Errorf("订阅 id %q 含非法字符（允许字母数字 . _ -）", s.ID)
	}
	if s.Event == "" {
		return fmt.Errorf("订阅 %q 需要 event", s.ID)
	}
	if !knownTopic(s.Event) {
		return fmt.Errorf("订阅 %q 的 event %q 不是已知 topic", s.ID, s.Event)
	}
	if s.URL == "" {
		return fmt.Errorf("订阅 %q 需要 url", s.ID)
	}
	if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return fmt.Errorf("订阅 %q 的 url 必须是 http(s):// 开头", s.ID)
	}
	if s.Method != "" && s.Method != http.MethodPost && s.Method != http.MethodPut {
		return fmt.Errorf("订阅 %q 的 method 只支持 POST/PUT", s.ID)
	}
	return nil
}

// knownTopic 判断是否是已知 topic（protocol + plugin 两侧）。
func knownTopic(topic string) bool {
	for _, t := range protocol.AllTopics() {
		if t == topic {
			return true
		}
	}
	for _, t := range plugin.AllTopics() {
		if t == topic {
			return true
		}
	}
	return false
}

// ---- 内部工具 ----

func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune(idCharset, c) {
			return false
		}
	}
	return true
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	plugin.Register(&Webhook{})
}
