package transport

import (
	"bufio"
	"bytes"
	"fmt"
	"net"

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

func (c *ndjsonConn) Close() error { return c.conn.Close() }
