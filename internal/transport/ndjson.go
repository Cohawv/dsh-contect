package transport

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"time"

	"lan-agent-hub/internal/protocol"
)

// TCPListener 监听 TCP，接受的连接按 NDJSON（每行一个 JSON）分帧。
type TCPListener struct {
	ln    net.Listener
	codec protocol.Codec
}

func ListenTCP(addr string, codec protocol.Codec) (*TCPListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &TCPListener{ln: ln, codec: codec}, nil
}

func (l *TCPListener) Accept() (Conn, error) {
	c, err := l.n().Accept()
	if err != nil {
		return nil, err
	}
	return &ndjsonConn{conn: c, codec: l.codec, reader: bufio.NewReader(c)}, nil
}

func (l *TCPListener) Close() error { return l.ln.Close() }

func (l *TCPListener) n() net.Listener { return l.ln }

type ndjsonConn struct {
	conn   net.Conn
	codec  protocol.Codec
	reader *bufio.Reader
	sbuf   []byte // SendBatch 的复用缓冲：避免每批都新分配
}

// Recv 读一帧并解码。解码失败时返回包装了 ErrBadFrame 的错误（可恢复）。
func (c *ndjsonConn) Recv() (protocol.Envelope, error) {
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return protocol.Envelope{}, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue // 空行忽略
		}
		env, err := c.codec.Decode(line)
		if err != nil {
			return protocol.Envelope{}, fmt.Errorf("%w: %v", ErrBadFrame, err)
		}
		return env, nil
	}
}

func (c *ndjsonConn) Send(e protocol.Envelope) error {
	b, err := c.codec.Encode(e)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(b)
	return err
}

// SendBatch 把多条信封拼成一段连续字节，一次 Write 写出。
//
// 为什么不用 net.Buffers（writev）：它在 Windows 上没有 writev 实现，
// 会退化成逐条 Write——恰恰是我们要消除的东西。而 NDJSON 是无长度前缀的
// 字节流，多帧首尾相接本身就是合法帧序列，所以「拼缓冲 + 一次 Write」
// 在各平台都能真的把系统调用数降下来。
//
// 一致性要求：一批多帧必须**一次性**写进内核。若分多次 Write，中间可能被
// 其他写入者插入半帧，对端就会读到半行 JSON。因此这里要求单写协程调用
// （tcpgateway 的 writeLoop 是每个连接唯一的写入者）。
func (c *ndjsonConn) SendBatch(envs []protocol.Envelope) error {
	switch len(envs) {
	case 0:
		return nil
	case 1:
		return c.Send(envs[0])
	}
	buf := c.sbuf[:0]
	for _, e := range envs {
		b, err := c.codec.Encode(e)
		if err != nil {
			return err
		}
		buf = append(buf, b...)
	}
	c.sbuf = buf // 保留容量供下一批复用
	_, err := c.conn.Write(buf)
	return err
}

func (c *ndjsonConn) Close() error { return c.conn.Close() }

// SetReadDeadline 设置下次 Recv 的截止时间（V8 ⑤ 读空闲超时的落地手段）。
// 传零值 time.Time 表示清除截止时间。
func (c *ndjsonConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline 设置下次 Write 的截止时间（V8 ⑤ 写超时的落地手段）。
// 传零值 time.Time 表示清除截止时间。
func (c *ndjsonConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
