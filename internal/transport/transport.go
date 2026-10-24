// Package transport 提供可插拔的通信实现：只负责「一帧 ↔ 一个 Envelope」，
// 不含任何业务规则。换传输（如加 WebSocket）只需新增一个实现，core 与业务代码不动。
package transport

import (
	"time"

	"lan-agent-hub/internal/protocol"
)

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

// DeadlineConn 是**可选**能力：给底层连接设置读/写截止时间（V8 ⑤）。
//
// 为什么需要它：没有截止时间时，一个「连上但不读」的客户端会让写协程永远阻塞在
// Write 上——那条协程连同它的 goroutine 栈、未被回收的批次缓冲一起钉死，
// 而且因为写协程卡住，队头后面的消息全部堆在 sendCh 里直到被丢弃。
// 反过来，一个「连上但不发」的客户端会让读协程永远阻塞在 Read 上，
// 在线表里留着一条实际已死的会话。
//
// 为什么做成可选接口而不是扩 Conn：TCP 有 deadline 语义，而内存/管道连接、
// 测试替身未必有。用可选接口 + 类型断言，未实现的连接退化为「不设超时」，
// 与 BatchConn 的取舍保持一致（不逼迫所有实现背上与自己无关的能力）。
type DeadlineConn interface {
	Conn
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}
