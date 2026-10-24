package tcpgateway

import (
	"bufio"
	"net"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/protocol"
)

// pipeConn 把 net.Conn 包成 transport.Conn + transport.DeadlineConn。
//
// 为什么用 net.Pipe 而不是起真 listener：net.Pipe 是**无缓冲**的，
// 一端 Write 会一直阻塞到另一端 Read——这正是「客户端连上但不读」的精确模型。
// 用真实 socket 测反而要靠填满内核缓冲区来制造阻塞，慢且不稳定。
type pipeConn struct {
	conn  net.Conn
	codec protocol.Codec
}

func (c *pipeConn) Recv() (protocol.Envelope, error) {
	line, err := bufio.NewReader(c.conn).ReadBytes('\n')
	if err != nil {
		return protocol.Envelope{}, err
	}
	return c.codec.Decode(line)
}

func (c *pipeConn) Send(e protocol.Envelope) error {
	b, err := c.codec.Encode(e)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(b)
	return err
}

func (c *pipeConn) Close() error                       { return c.conn.Close() }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// TestWriteTimeoutDisconnectsSlowReader 验证「客户端连上但不读」不会把写协程钉死。
//
// 这是 V8 ⑤ 的核心用例：没有 SetWriteDeadline 时，writeLoop 会永远阻塞在 Write 上，
// 那条协程连同批次缓冲一起泄漏，而且它卡住之后队列只进不出、消息成片被丢。
func TestWriteTimeoutDisconnectsSlowReader(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() // 故意不读：让 server 侧 Write 阻塞

	stats := &deliverStats{}
	s := newSession(&pipeConn{conn: server, codec: protocol.JSONLine{}}, stats, 50*time.Millisecond, 0)
	go s.writeLoop()

	s.Deliver(protocol.NewEnvelope(protocol.TypeChat, "hub", "a", "", map[string]string{"text": "x"}))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stats.writeTimeout.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := stats.writeTimeout.Load(); got != 1 {
		t.Fatalf("写超时计数 = %d，期望 1（客户端不读时必须靠 deadline 断开）", got)
	}
}

// TestReadIdleTimeoutDisconnects 验证「连上但永不发数据」的会话会被清理。
//
// 没有这条保护时，readLoop 永远阻塞在 Recv 上，在线表里留着一条实际已死的会话：
// 消息照发、全部进队列后被丢，而 Hub 认为对方在线。
func TestReadIdleTimeoutDisconnects(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() // 故意不发任何字节

	stats := &deliverStats{}
	s := newSession(&pipeConn{conn: server, codec: protocol.JSONLine{}}, stats, 0, 50*time.Millisecond)
	hub := core.NewHub()

	done := make(chan struct{})
	go func() {
		s.readLoop(hub)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("读空闲超时后 readLoop 未退出：死会话会永久占着在线表")
	}
	if got := stats.readTimeout.Load(); got != 1 {
		t.Fatalf("读空闲超时计数 = %d，期望 1", got)
	}
}

// TestTimeoutZeroDisables 验证「填 0 = 关闭保护」真的关得掉。
//
// 没有这条：把超时写成 0 却被当成默认值，用户以为关了保护、实际仍在断线，
// 而且断得比默认值更早时最难排查。
func TestTimeoutZeroDisables(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	stats := &deliverStats{}
	s := newSession(&pipeConn{conn: server, codec: protocol.JSONLine{}}, stats, 0, 0)
	hub := core.NewHub()

	done := make(chan struct{})
	go func() {
		s.readLoop(hub)
		close(done)
	}()

	// 200ms 远大于上面用例里的 50ms：若 0 被误当成默认值或仍设了 deadline，这里会退出。
	select {
	case <-done:
		t.Fatal("readIdle=0 表示关闭保护，连接不应因空闲被断开")
	case <-time.After(200 * time.Millisecond):
	}
	if got := stats.readTimeout.Load(); got != 0 {
		t.Fatalf("关闭保护后不该有超时计数，实得 %d", got)
	}
}

// TestWriteDeadlineClearedAfterSuccess 验证写成功后清零 deadline。
//
// 不清零的后果很隐蔽：net.Conn 的 deadline 是**绝对时间**，第一批发出去之后它已经过期，
// 下一批写立刻失败——表现为「第一条消息正常，之后全断」。
func TestWriteDeadlineClearedAfterSuccess(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()

	stats := &deliverStats{}
	s := newSession(&pipeConn{conn: server, codec: protocol.JSONLine{}}, stats, 200*time.Millisecond, 0)
	go s.writeLoop()

	// 这一轮对端要读，否则写不出去。
	go func() {
		r := bufio.NewReader(client)
		for i := 0; i < 3; i++ {
			if _, err := r.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 3; i++ {
		s.Deliver(protocol.NewEnvelope(protocol.TypeChat, "hub", "a", "", map[string]string{"text": "x"}))
		// 间隔要小于 writeTimeout：如果 deadline 没被清零，第二批会继承上一批的过期时间而失败。
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	if got := stats.writeTimeout.Load(); got != 0 {
		t.Fatalf("连续写成功却触发了写超时（%d 次）：deadline 未被清零", got)
	}
}

// TestNewSessionWithoutDeadlineCapability 验证连接不支持 deadline 时退化为不设超时。
//
// 这是「可选接口」的兜底：测试替身、内存连接没有 deadline 语义，
// 不能因为配了超时就在 nil 接口上 panic。
func TestNewSessionWithoutDeadlineCapability(t *testing.T) {
	s := newSession(&nopConn{}, &deliverStats{}, 10*time.Millisecond, 10*time.Millisecond)
	if s.dc != nil {
		t.Fatal("nopConn 未实现 DeadlineConn，dc 应为 nil")
	}
	go s.writeLoop()
	s.Deliver(protocol.Envelope{})
	time.Sleep(30 * time.Millisecond) // 不应 panic
}
