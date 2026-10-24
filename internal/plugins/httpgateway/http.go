// Package httpgateway 是 REST/控制台 HTTP 网关插件，承载 11 个 /api 端点。
//
// 端点路径、响应字段、回执文案、Header 与历史 internal/adapter/http.go 逐字节一致，
// 以保证契约 golden 仍为绿。**迁移禁止项**：/api/discover 不得补 X-Proto-Version
// （现状绕过 writeJSON 直接 Write），必须保持。
package httpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/discovery"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// writeJSON 设置 Content-Type 与 X-Proto-Version（与历史行为一致）。
// 注意：/api/discover 不使用本函数（直接 Write），以保持其无 X-Proto-Version 的现状。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// 便于观测：客户端可据此判断服务端支持的协议版本
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

// HTTP 超时默认值（V8 ⑤）。
//
// 这里只有两个「安全」的超时，另外两个（ReadTimeout / WriteTimeout）**故意不设**，
// 原因见 startTimeout 上方的说明——它们会掐断 SSE 长连接。
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultHandlerTimeout    = 5 * time.Second
	// slowHandlerThreshold 是「慢 handler」告警线：超过就记一条 WARN。
	slowHandlerThreshold = time.Second
	// handlerTimeoutBody 是 handler 超时时返回的响应体（TimeoutHandler 要求给字符串）。
	//
	// 用 JSON 而不是纯文本：控制台的 fetch 一律按 JSON 解析，
	// 返回纯文本会先抛一个 SyntaxError，把「服务端超时」伪装成「前端解析错误」。
	handlerTimeoutBody = `{"error":"handler timeout"}`
)

// HTTPGateway 是 httpgateway 插件实体。
type HTTPGateway struct {
	hub      *core.Hub
	disco    *discovery.Service
	mgr      *plugin.Manager // 框架管理器（V4：/api/status?features=plugins 需要）
	mux      *http.ServeMux
	httpAddr string
	srv      *http.Server
	// V8 ⑤ 超时配置（Init 时解析）。
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	handlerTimeout    time.Duration
}

// Meta 声明插件元信息。
func (p *HTTPGateway) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "httpgateway",
		Version:     "1.1.0",
		Description: "REST/控制台 HTTP 网关，承载 11 个 /api 端点与控制台根 /",
		DefaultOn:   true,
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单）。
// httpAddr 需要重新绑定端口，故本插件不实现 Applier（改后标注 pendingRestart）。
func (p *HTTPGateway) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "httpAddr",
			Type:        plugin.OptionString,
			Default:     ":8081",
			Description: "HTTP 监听地址，形如 :8081 或 0.0.0.0:8081（改动需重启插件生效）",
		},
		{
			Key:         "readHeaderTimeout",
			Type:        plugin.OptionDuration,
			Default:     "10s",
			Description: "读完请求头的上限（如 10s）：防止只发半个请求头的连接长期占资源。填 0 关闭",
		},
		{
			Key:         "idleTimeout",
			Type:        plugin.OptionDuration,
			Default:     "120s",
			Description: "keep-alive 空闲连接的上限（如 120s）。不影响正在传输的请求，含 SSE。填 0 关闭",
		},
		{
			Key:         "handlerTimeout",
			Type:        plugin.OptionDuration,
			Default:     "5s",
			Description: "单个 API handler 的执行上限（如 5s），超时返回 503。只作用于本插件的 11 个端点，不影响流式端点",
		},
	}
}

// Init 取出内核、HTTP 地址，从共享 KV 取 discovery 服务，并逐个注册 11 个端点
// （经 ctx.RegisterHTTP，触发框架的路由冲突预检）。同时把控制台地址交接给 autobrowser。
func (p *HTTPGateway) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return errors.New("httpgateway: missing *core.Hub in context")
	}
	p.hub = hub
	p.mux = ctx.ServeMux()
	p.httpAddr = ctx.String("httpAddr", ":8081")
	// V8 ⑤：超时配置。负数视为关闭（Duration 解析失败也会走到默认值）。
	p.readHeaderTimeout = ctx.Duration("readHeaderTimeout", defaultReadHeaderTimeout)
	p.idleTimeout = ctx.Duration("idleTimeout", defaultIdleTimeout)
	p.handlerTimeout = ctx.Duration("handlerTimeout", defaultHandlerTimeout)
	if p.readHeaderTimeout < 0 {
		p.readHeaderTimeout = 0
	}
	if p.idleTimeout < 0 {
		p.idleTimeout = 0
	}
	if p.handlerTimeout < 0 {
		p.handlerTimeout = 0
	}
	// 插件实例是进程级单例，先清掉上一次装配的残留，
	// 避免「上一次装配启用了 discovery、这一次禁用」时仍读到旧的 Service。
	p.disco = nil
	p.mgr = nil
	if m, ok := plugin.ManagerFromContext(ctx); ok {
		p.mgr = m
	}
	// GetShared 返回 (any, bool)：先判存在，再做类型断言。
	// discovery 插件被禁用/被移除时共享 KV 无该键，p.disco 保持 nil → /api/discover 返回 404（与历史一致）。
	if v, ok := ctx.GetShared("discovery"); ok {
		if svc, ok := v.(*discovery.Service); ok {
			p.disco = svc
		}
	}
	// 把控制台地址交给 autobrowser（依赖本插件，Init 顺序在后）。
	ctx.SetShared("dashboardURL", fmt.Sprintf("http://localhost%s/", p.httpAddr))

	var regErr error
	registerRoutes(func(pattern string, h http.HandlerFunc) {
		if regErr != nil {
			return
		}
		if e := ctx.RegisterHTTP(pattern, p.guard(h)); e != nil {
			regErr = e
		}
	}, hub, p.disco, p.mgr)
	if regErr != nil {
		return regErr
	}
	// 更新通道预留端点（不入本插件路由表，见 registerRoutes 上方的说明）
	attachUpdateRoutes(p.mux)
	return nil
}

// guard 给单个 API handler 套上「执行上限 + 慢 handler 日志」两层保护（V8 ⑤）。
//
// 为什么只包本插件自己的 11 个端点、而不是整个共享 mux：
// 共享 mux 上还有 realtime 的 /api/stream（SSE）和 logview 的日志流。
// TimeoutHandler 的语义是「到点就返回 503 并丢弃后续写入」——套在 SSE 上
// 等于给一条设计上要挂几小时的长连接设了 5 秒死刑。按插件自治划分边界，
// 每个插件只对自己注册的端点负责，是这里唯一不会误伤的切法。
func (p *HTTPGateway) guard(h http.HandlerFunc) http.HandlerFunc {
	inner := http.Handler(http.HandlerFunc(h))
	if p.handlerTimeout > 0 {
		inner = http.TimeoutHandler(inner, p.handlerTimeout, handlerTimeoutBody)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// 用 defer 记日志：handler panic 时会向上传播，若写在调用之后，
		// 正是最需要这条日志的那次不会被执行。
		defer func() {
			if d := time.Since(start); d > slowHandlerThreshold {
				log.Printf("httpgateway: 慢 handler %s %s 耗时 %s（告警线 %s）",
					r.Method, r.URL.Path, d.Round(time.Millisecond), slowHandlerThreshold)
			}
		}()
		inner.ServeHTTP(w, r)
	}
}

// Start 在 httpAddr 上启动 HTTP 服务（共享 mux）。
//
// 端口绑定是**同步**做的：绑定失败直接返回 error（State 变 Failed），
// 而不是把 ListenAndServe 扔进 goroutine 只记日志——否则端口被占时插件会停在
// 一个"假 Running"状态，而实际没有任何 HTTP 服务。
//
// 超时取舍（V8 ⑤）：只设 ReadHeaderTimeout 与 IdleTimeout，**绝不设**
// ReadTimeout / WriteTimeout。
//   - ReadHeaderTimeout 挡住的是「连上但只发半个请求头」的连接，这是 Slowloris 的本体：
//     实测 50 条这样的连接会让 goroutine 数从 21 涨到 71 且永不自愈（每条一协程一 fd）。
//     它只约束「读请求头」这一段，请求开始处理后就不再作用，因此不影响任何长响应。
//   - IdleTimeout 只收割 keep-alive 的空闲连接，SSE 在传输期间是活跃的，不受影响。
//   - WriteTimeout 从「读完请求头」开始计时并**覆盖整个响应写出过程**，
//     SSE 挂到第 N 秒就会被硬切——这正是历史行为里没有它的原因，不是遗漏。
//   - ReadTimeout 到期会给连接打上永久的读截止时间，长连接的后续读全部失败。
//
// 长响应的保护交给 guard 的 handlerTimeout（按端点、按语义分别设定）。
// newServer 构造 http.Server，把本插件的超时配置传导进去。
//
// 单独抽出来是为了可测：如果超时只在 Start 里就地组装，测试就只能重复一遍
// 同样的字段赋值（测的是测试自己的代码），而无法断言「配置真的传到了 server」。
func (p *HTTPGateway) newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              p.httpAddr,
		Handler:           handler,
		ReadHeaderTimeout: p.readHeaderTimeout,
		IdleTimeout:       p.idleTimeout,
	}
}

func (p *HTTPGateway) Start() error {
	if p.mux == nil {
		return errors.New("httpgateway: mux not built")
	}
	ln, err := net.Listen("tcp", p.httpAddr)
	if err != nil {
		return fmt.Errorf("httpgateway listen %s: %w", p.httpAddr, err)
	}
	p.srv = p.newServer(p.mux)
	go func() {
		if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http gateway error: %v", err)
		}
	}()
	log.Printf("LAN Agent Hub HTTP gateway on %s（读头超时 %s · 空闲超时 %s · handler 超时 %s）",
		p.httpAddr, p.readHeaderTimeout, p.idleTimeout, p.handlerTimeout)
	return nil
}

// Stop 优雅关闭 HTTP 服务（最多 5s）。
func (p *HTTPGateway) Stop() error {
	if p.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("httpgateway shutdown: %w", err)
	}
	return nil
}

func init() {
	plugin.Register(&HTTPGateway{})
}

// SkipReload 实现 plugin.ReloadSkipper：本插件就是承载 /api/* 的 HTTP 服务。
//
// 全量重载会逐个 Stop→Init→Start，若在「处理 /api/plugins/reload 请求的过程中」Stop 自己，
// 正在等待响应的客户端连接会被一起掐断（表现为 reload 请求超时）。
// 因此这里显式声明「别重载我」，并把原因回传给调用方——而不是让请求静默超时。
// 改 httpAddr 请用插件开关单独重启，或直接重启进程。
func (p *HTTPGateway) SkipReload() (bool, string) {
	return true, "本插件承载 /api/* 的 HTTP 服务，全量重载会中断当前请求；请用插件开关单独重启该插件，或重启进程"
}

// ---------------------------------------------------------------------------
// 端点定义：registerRoutes 把 11 个端点逐一注册到给定 registrar。
// registrar 在插件场景是 ctx.RegisterHTTP（带冲突预检），在测试/兼容场景是 mux.HandleFunc。
// 行为与历史 adapter/http.go 逐字节一致。
// ---------------------------------------------------------------------------

// registerRoutes 应用 11 个历史契约端点到 registrar。
// mgr 为框架管理器：仅用于 /api/status?features=plugins 的插件汇总（为 nil 时该能力不可用，
// 默认响应保持逐字节不变）。
func registerRoutes(reg func(pattern string, h http.HandlerFunc), hub *core.Hub, disco *discovery.Service, mgr *plugin.Manager) {
	reg("/api/discover", func(w http.ResponseWriter, r *http.Request) {
		if disco == nil {
			http.Error(w, "discovery disabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(disco.BeaconJSON())
	})

	reg("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"agents": hub.ListAgentsDetailed()})
	})

	// 控制台状态总览：协议版本、暴露端口、在线/房间计数、发现服务开关。
	//
	// 契约纪律（V1 C6 / V4 ⑤）：默认响应的 7 个字段**一个不动**。
	// 新增的插件信息必须通过 ?features=plugins 显式索取（沿用既有 features 协商机制），
	// 这样老客户端零感知，golden 基线无需改动。
	reg("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp := map[string]any{
			"proto":      protocol.ProtoCurrent,
			"tcpPort":    hub.TCPPort,
			"httpPort":   hub.HTTPPort,
			"agentCount": len(hub.ListAgentsDetailed()),
			"roomCount":  len(hub.Rooms.List()),
			"discovery":  disco != nil,
			"serverTime": time.Now().UnixMilli(),
		}
		if mgr != nil && wantsFeature(r, "plugins") {
			resp["plugins"] = pluginSummaryOf(mgr)
		}
		writeJSON(w, resp)
	})

	// 房间列表（含成员），供控制台房间视图展示。
	reg("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"rooms": hub.Rooms.List()})
	})

	reg("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.To == "" || p.Text == "" {
			http.Error(w, "need from/to/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeChat, p.From, p.To, "",
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	reg("/api/room", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" || p.Text == "" {
			http.Error(w, "need from/roomId/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeRoom, p.From, "", p.RoomID,
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	reg("/api/broadcast", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From string `json:"from"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Text == "" {
			http.Error(w, "need from/text", http.StatusBadRequest)
			return
		}
		hub.Router.Route(protocol.NewEnvelope(protocol.TypeBroadcast, p.From, "", "",
			map[string]string{"text": p.Text}))
		writeJSON(w, map[string]string{"status": "ok"})
	})

	reg("/api/create_room", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From    string   `json:"from"`
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"roomId": hub.Rooms.Create(p.Name, p.Members)})
	})

	reg("/api/join", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" {
			http.Error(w, "need roomId", http.StatusBadRequest)
			return
		}
		hub.Rooms.Join(p.RoomID, p.From)
		writeJSON(w, map[string]string{"status": "ok"})
	})

	reg("/api/leave", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			From   string `json:"from"`
			RoomID string `json:"roomId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.RoomID == "" {
			http.Error(w, "need roomId", http.StatusBadRequest)
			return
		}
		hub.Rooms.Leave(p.RoomID, p.From)
		writeJSON(w, map[string]string{"status": "ok"})
	})
}

// 更新通道的两个预留端点**故意不进 registerRoutes**：/api/plugins 的 v2 基线把
// httpgateway 的 routeCount 与 routes 清单逐字节钉死了，为了两个"预留"端点去改
// 事实契约不划算。它们直接挂在共享 mux 上（见 attachUpdateRoutes），
// 待更新通道正式立项时再拆成独立 updates 插件并显式登记到基线。

// NewMux 构造 REST 网关路由表（仅 11 个 /api 端点，不含控制台根 /）。
// 行为必须与历史 adapter.NewMux 的端点部分逐字节一致，以保证契约 golden 仍为绿。
// 控制台根 / 由 webconsole 插件（或测试中的 webconsole.RegisterWeb）挂载。
//
// 注意：本函数不接入插件管理器，故 /api/status?features=plugins 在这条装配路径上不可用
// （默认响应不受影响）。
func NewMux(hub *core.Hub, disco *discovery.Service) *http.ServeMux {
	mux := http.NewServeMux()
	registerRoutes(func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
	}, hub, disco, nil)
	attachUpdateRoutes(mux)
	return mux
}

// wantsFeature 判断请求是否通过 ?features=a,b 显式索取某个能力。
// 沿用 V1 的 features 协商机制：不索取就绝不追加字段（契约护栏）。
func wantsFeature(r *http.Request, name string) bool {
	raw := r.URL.Query().Get("features")
	if raw == "" {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == name {
			return true
		}
	}
	return false
}

// pluginFailure 是一个失败插件的简短说明（failedPlugins 里带原因，便于肉眼判断）。
type pluginFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// pluginSummary 是 /api/status?features=plugins 追加的插件汇总。
type pluginSummary struct {
	Count    int             `json:"count"`
	Enabled  []string        `json:"enabled"`
	Disabled []string        `json:"disabled"`
	Failed   []pluginFailure `json:"failed"`
}

// pluginSummaryOf 由管理器快照生成插件汇总（顺序稳定，便于 golden 断言）。
func pluginSummaryOf(mgr *plugin.Manager) pluginSummary {
	out := pluginSummary{Enabled: []string{}, Disabled: []string{}, Failed: []pluginFailure{}}
	for _, info := range mgr.Snapshot() {
		out.Count++
		switch info.State {
		case plugin.StateDisabled:
			out.Disabled = append(out.Disabled, info.Name)
		case plugin.StateFailed:
			out.Failed = append(out.Failed, pluginFailure{Name: info.Name, Error: info.Error})
			out.Enabled = append(out.Enabled, info.Name)
		default:
			out.Enabled = append(out.Enabled, info.Name)
		}
	}
	return out
}
