// Package adapter 把传输连接适配为业务内核能理解的 Session。
// 这是唯一同时接触通信层与业务层的地方：上接 core.Hub，下接 transport.Conn。
package adapter

import (
	"errors"
	"sync"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/protocol"
	"lan-agent-hub/internal/transport"
)

const sendBufferSize = 256

// session 是 core.Session 的 TCP 实现。
// 发送走独立写协程 + 有缓冲信道：业务层 Deliver 永不阻塞，缓冲满则丢弃（背压策略）。
type session struct {
	id     string
	conn   transport.Conn
	sendCh chan protocol.Envelope
	closed chan struct{}
	once   sync.Once
}

func newSession(conn transport.Conn) *session {
	return &session{
		conn:   conn,
		sendCh: make(chan protocol.Envelope, sendBufferSize),
		closed: make(chan struct{}),
	}
}

func (s *session) ID() string { return s.id }

// Deliver 非阻塞下发；缓冲满则丢弃，连接已关闭时静默返回（不 panic）。
func (s *session) Deliver(env protocol.Envelope) {
	defer func() { _ = recover() }()
	select {
	case s.sendCh <- env:
	case <-s.closed:
	default:
	}
}

func (s *session) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.conn.Close()
}

func (s *session) writeLoop() {
	for {
		select {
		case env := <-s.sendCh:
			if err := s.conn.Send(env); err != nil {
				return
			}
		case <-s.closed:
			return
		}
	}
}

// readLoop 读帧 → 交给业务内核；退出时摘除在线记录。
func (s *session) readLoop(hub *core.Hub) {
	defer func() {
		hub.Unregister(s.id)
		_ = s.Close()
	}()
	for {
		env, err := s.conn.Recv()
		if err != nil {
			// 帧内容坏了是可恢复的：回执并继续，与历史行为一致
			if errors.Is(err, transport.ErrBadFrame) {
				s.Deliver(protocol.NewEnvelope(protocol.TypeSystem, "hub", s.id, "", map[string]any{
					"text":   "bad json: " + err.Error(), // 旧契约，保留
					"action": protocol.ActionError,
					"code":   protocol.CodeBadJSON,
				}))
				continue
			}
			return
		}
		if env.Type == protocol.TypeRegister {
			s.id = env.From
		}
		hub.HandleEnvelope(s, &env)
	}
}

// ServeTCP 接受连接并驱动读写循环（阻塞）。
func ServeTCP(ln transport.Listener, hub *core.Hub) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		s := newSession(conn)
		go s.writeLoop()
		go s.readLoop(hub)
	}
}
