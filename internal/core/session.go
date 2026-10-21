package core

import "lan-agent-hub/internal/protocol"

// Session 是业务内核对「一个已接入连接」的唯一抽象。
//
// 接口定义在消费者（core）侧，实现在 adapter 侧 —— Go 的隐式接口满足让实现方
// 无需 import 本包声明。业务层只调 Deliver，不知道底下是 TCP、WebSocket 还是内存管道。
type Session interface {
	ID() string
	// Deliver 下发一条信封。允许非阻塞与丢弃（背压策略由实现方决定），
	// 业务层不因发送失败而阻塞或 panic。
	Deliver(env protocol.Envelope)
	Close() error
}
