package plugin

import "strings"

// hubVersion 是二进制版本号的框架侧副本（由 cmd/hub 通过 SetVersion 注入）。
//
// 为什么框架也需要它：健康详情（/api/health/detail）要报 version，
// 而那个端点属于可选插件，不应让它去 import 另一个插件包（plugininfo）取版本号。
// 注入点仍只有一处（cmd/hub/main.go），两处共享同一个字符串，不会分叉。
var hubVersion = "dev"

// SetVersion 注入二进制版本号（空串忽略，保持 "dev" 默认值）。
func SetVersion(v string) {
	if strings.TrimSpace(v) != "" {
		hubVersion = v
	}
}

// Version 返回二进制版本号（未注入时为 "dev"）。
func Version() string { return hubVersion }
