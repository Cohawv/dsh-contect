package webconsole

import (
	"embed"
	"io/fs"
	"net/http"
)

// webFS 内嵌控制台静态资源（index.html / styles.css / app.js）。
// 目的是让 Hub 单一二进制即可自带管理界面——无需额外部署文件、无需任何第三方依赖，
// 与项目"零外部依赖、可离线构建"的原则一致。
//
//go:embed webroot
var webFS embed.FS

// RegisterWeb 把内嵌控制台挂载到 "/"（catch-all）。
// 已注册的 /api/* 是更具体的精确路由，会优先匹配，不会被覆盖；
// 其余路径交由文件服务器处理，fs.Sub 天然阻止目录穿越。
func RegisterWeb(mux *http.ServeMux) {
	sub, err := fs.Sub(webFS, "webroot")
	if err != nil {
		return
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// fileServerHandler 返回控制台文件服务器（供插件 Init 注册到共享 mux 的 "/"）。
func fileServerHandler() http.Handler {
	sub, _ := fs.Sub(webFS, "webroot")
	return http.FileServer(http.FS(sub))
}
