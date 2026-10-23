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
//
// i18n 持有语言包配置（外部目录 + 默认语言），由 Options/Apply 与 /api/i18n/ 端点共用；
// 字段本身带锁，故零值即可用（plugin.Register 在 init 里构造，Init 之前不会有请求）。
type WebConsole struct {
	i18n i18nStore
}

// Meta 声明插件元信息。
func (p *WebConsole) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "webconsole",
		Version:     "1.0.0",
		Description: "内嵌网页控制台（/ 根路径，catch-all）",
		DefaultOn:   true,
	}
}

// Init 把内嵌控制台注册到共享 mux 的 "/"（catch-all），并挂上 V7 ④ 的语言包端点。
func (p *WebConsole) Init(ctx *plugin.Context) error {
	// webconsole 独占 "/"，其它插件注册 "/" 会触发框架冲突预检报错。
	if err := ctx.RegisterHTTP("/", func(w http.ResponseWriter, r *http.Request) {
		fileServerHandler().ServeHTTP(w, r)
	}); err != nil {
		return errors.New("webconsole: " + err.Error())
	}

	// 语言包配置：i18n.dir 指向外部目录（新增语言只需丢一个 <lang>.json），
	// language 是新客户端的默认界面语言（默认 zh-CN，保持既有偏好不变）。
	p.i18n.configure(ctx.String(optI18NDir, ""), ctx.String(optLanguage, defaultLanguage))

	// /api/i18n/ 是比 "/" 更具体的子树，不会被控制台的文件服务器吃掉。
	if err := ctx.RegisterHTTP(pathI18N, p.i18n.ServeHTTP); err != nil {
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
