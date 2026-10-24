// Package transport 提供可插拔的通信实现：只负责「一帧 ↔ 一个 Envelope」，
// 不含任何业务规则。换传输（如加 WebSocket）只需新增一个实现，core 与业务代码不动。
package transport

import "lan-agent-hub/internal/protocol"

// ErrBadFrame 表示「帧本身能读出来，但内容无法解码」。
// 它是可恢复错误：adapter 收到后应回一条 bad json 回执并继续读下一帧，
// 而不是断开连接。真正的连接错误（EOF / reset）不包装此错误。
var ErrBadFrame = errBadFrame{}

type errBadFrame struct{}

func (errBadFrame) Error() string { return "bad frame" }
func (errBadFrame) Is(target error) bool {
	_, ok := target.(errBadFrame)
	return ok
}

// Conn 是一条已建立的传输连接。
type Conn interface {
	Recv() (protocol.Envelope, error)
	Send(protocol.Envelope) error
	Close() error
}

// BatchConn 是**可选**能力：一次写入多条信封。
//
// 为什么要它：房间扇出时单个连接每秒要收几百上千条消息，逐条 Send 就是逐条
// 系统调用，写协程会先被 syscall 打满（实测 600 成员房间需求 35.9 万投递/秒、
// 只跑到 29.3 万，其余进队列后被丢弃）。合批把 N 次写并成 1 次，
// 系统调用数直接除以批大小。
//
// 为什么不直接扩 Conn 接口：那会逼迫所有实现（含测试里的空实现）都提供批处理，
// 而批处理只对「字节流帧格式」有意义。用可选接口 + 类型断言，未实现的连接
// 自动退化为逐条 Send，行为不变。
type BatchConn interface {
	Conn
	SendBatch(envs []protocol.Envelope) error
}

// Listener 接受连接。
type Listener interface {
	Accept() (Conn, error)
	Close() error
}
