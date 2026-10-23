// Package plugins 是 Hub 的「功能总清单」：以 blank import 把内置插件注册进全局注册表。
//
// 设计意图（V3-⑤）：部署形态由这一份清单 + CLI 白/黑名单 + hub.json 共同决定。
// 注释掉（或删除）下面任意一行，该能力即从二进制中干净移除——
// 编译仍通过、EXE 仍可运行，只是少了那一项能力（blank import 不产生未使用告警）。
//
// 注意：本包只做 import 收口，不含任何逻辑，也不得被 internal/* 反向依赖。
package plugins

import (
	_ "lan-agent-hub/internal/plugins/autobrowser" // 启动后自动打开网页控制台
	_ "lan-agent-hub/internal/plugins/discovery"   // 局域网 UDP 自动发现 Beacon
	_ "lan-agent-hub/internal/plugins/healthz"     // 健康/就绪探针（/healthz、/readyz）
	_ "lan-agent-hub/internal/plugins/httpgateway" // REST 网关（11 个 /api 端点）
	_ "lan-agent-hub/internal/plugins/logview"     // 服务端日志环形缓冲（/api/logs）
	_ "lan-agent-hub/internal/plugins/metrics"     // Prometheus 指标与概览趋势
	_ "lan-agent-hub/internal/plugins/plugininfo"  // 管理 API（插件清单/开关/设置/部署清单）
	_ "lan-agent-hub/internal/plugins/realtime"    // SSE 实时推送（/api/stream）
	_ "lan-agent-hub/internal/plugins/ack"         // 投递确认与对账（V6 ②）
	_ "lan-agent-hub/internal/plugins/store"       // 消息持久化与离线回放（纯标准库；零外部依赖）
	_ "lan-agent-hub/internal/plugins/sweeper"     // 心跳超时连接周期清理
	_ "lan-agent-hub/internal/plugins/tcpgateway"  // TCP/NDJSON 长连接接入（Required）
	_ "lan-agent-hub/internal/plugins/webconsole"  // 内嵌网页控制台（/ 根路径）
)
