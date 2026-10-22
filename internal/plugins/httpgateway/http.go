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

// HTTPGateway 是 httpgateway 插件实体。
type HTTPGateway struct {
	hub      *core.Hub
	disco    *discovery.Service
	mux      *http.ServeMux
	httpAddr string
	srv      *http.Server
}

// Meta 声明插件元信息。
func (p *HTTPGateway) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "httpgateway",
		Version:     "1.0.0",
		Description: "REST/控制台 HTTP 网关，承载 11 个 /api 端点与控制台根 /",
		DefaultOn:   true,
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
	// 插件实例是进程级单例（init 里 Register 的是同一个对象），先清掉上一次装配的残留，
	// 避免「上一次装配启用了 discovery、这一次禁用」时仍读到旧的 Service。
	p.disco = nil
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
		if e := ctx.RegisterHTTP(pattern, h); e != nil {
			regErr = e
		}
	}, hub, p.disco)
	return regErr
}

// Start 在 httpAddr 上启动 HTTP 服务（共享 mux）。
//
// 端口绑定是**同步**做的：绑定失败直接返回 error（State 变 Failed），
// 而不是把 ListenAndServe 扔进 goroutine 只记日志——否则端口被占时插件会停在
// 一个"假 Running"状态，而实际没有任何 HTTP 服务。
func (p *HTTPGateway) Start() error {
	if p.mux == nil {
		return errors.New("httpgateway: mux not built")
	}
	ln, err := net.Listen("tcp", p.httpAddr)
	if err != nil {
		return fmt.Errorf("httpgateway listen %s: %w", p.httpAddr, err)
	}
	p.srv = &http.Server{Addr: p.httpAddr, Handler: p.mux}
	go func() {
		if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http gateway error: %v", err)
		}
	}()
	log.Printf("LAN Agent Hub HTTP gateway on %s", p.httpAddr)
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

// ---------------------------------------------------------------------------
// 端点定义：registerRoutes 把 11 个端点逐一注册到给定 registrar。
// registrar 在插件场景是 ctx.RegisterHTTP（带冲突预检），在测试/兼容场景是 mux.HandleFunc。
// 行为与历史 adapter/http.go 逐字节一致。
// ---------------------------------------------------------------------------

// registerRoutes 应用全部 11 个网关路由到 registrar。
func registerRoutes(reg func(pattern string, h http.HandlerFunc), hub *core.Hub, disco *discovery.Service) {
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
	reg("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{
			"proto":      protocol.ProtoCurrent,
			"tcpPort":    hub.TCPPort,
			"httpPort":   hub.HTTPPort,
			"agentCount": len(hub.ListAgentsDetailed()),
			"roomCount":  len(hub.Rooms.List()),
			"discovery":  disco != nil,
			"serverTime": time.Now().UnixMilli(),
		})
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

// NewMux 构造 REST 网关路由表（仅 11 个 /api 端点，不含控制台根 /）。
// 行为必须与历史 adapter.NewMux 的端点部分逐字节一致，以保证契约 golden 仍为绿。
// 控制台根 / 由 webconsole 插件（或测试中的 webconsole.RegisterWeb）挂载。
func NewMux(hub *core.Hub, disco *discovery.Service) *http.ServeMux {
	mux := http.NewServeMux()
	registerRoutes(func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
	}, hub, disco)
	return mux
}
