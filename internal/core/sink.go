// Package core 的「消息落盘 / 回放」窄接口。
//
// 设计动机（V6 ① store）：持久化与离线回放需要拿到**完整的路由后信封**（含 seq / ts），
// 而 V5 的 msg.routed 事件刻意只带计数（避免向每个订阅者泄漏内容），因此 store 不能靠
// 订阅事件拿到消息正文。这里用「编译期窄接口 + 装配期注入」解决：
//   - core 定义接口，store 实现并把自己挂上 Router / Hub；
//   - 依赖方向严格单向：store import core，core 绝不 import store（check-deps.sh 守护）。
//
// 两个钩子的值（接口 / 函数）为 nil 时（未挂载 store，或 store 被运行期停用），
// Router / Hub 静默跳过——老客户端、未声明 replay 的客户端、以及「没有 store」的部署
// 行为完全不变。
package core

import "lan-agent-hub/internal/protocol"

// MessageSink 是「消息落盘」的窄接口：经 Router 路由的消息（已分配 seq 与 ts）由 Router
// 在路由完成后交给实现了本接口的插件（store）做持久化。
type MessageSink interface {
	// Append 收到一条已路由完成的信封（含服务端分配的 Seq / TS）。
	// 实现方必须非阻塞、不 panic——它是消息热路径上的同步调用。
	Append(env protocol.Envelope)
}

// ReplayProvider 是「离线消息回放」的窄接口：声明了 replay cap 的客户端重连时，
// Hub 调用它把 seq > sinceSeq 且与该客户端相关的消息补发给其会话。
type ReplayProvider func(sess Session, from string, sinceSeq int64)
