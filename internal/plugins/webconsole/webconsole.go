// Package webconsole 是内嵌网页控制台插件（"/" 根路径，catch-all）。
//
// webconsole 独占 "/"，其它插件注册 "/" 会被框架的路由冲突预检拒绝。
// 静态资源经 //go:embed webroot 内嵌，零外部依赖、可离线构建。
package webconsole

import (
	"errors"
	"net/http"

	"lan-agent-hub/internal/plugin"
)

// WebConsole 是 webconsole 插件实体。
type WebConsole struct{}

// Meta 声明插件元信息。
func (p *WebConsole) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "webconsole",
		Version:     "1.0.0",
		Description: "内嵌网页控制台（/ 根路径，catch-all）",
		DefaultOn:   true,
	}
}

// Init 把内嵌控制台注册到共享 mux 的 "/"（catch-all）。
func (p *WebConsole) Init(ctx *plugin.Context) error {
	// webconsole 独占 "/"，其它插件注册 "/" 会触发框架冲突预检报错。
	if err := ctx.RegisterHTTP("/", func(w http.ResponseWriter, r *http.Request) {
		fileServerHandler().ServeHTTP(w, r)
	}); err != nil {
		return errors.New("webconsole: " + err.Error())
	}
	return nil
}

// Start 无副作用：静态资源已内嵌，服务由 httpgateway 的共享 mux 承载。
// （插件接口要求实现 Start/Stop；webconsole 不own任何监听端口，故两者均为空实现。）
func (p *WebConsole) Start() error { return nil }

// Stop 无副作用（无自有监听端口/协程需要回收）。
func (p *WebConsole) Stop() error { return nil }

func init() {
	plugin.Register(&WebConsole{})
}
