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

// Listener 接受连接。
type Listener interface {
	Accept() (Conn, error)
	Close() error
}
