package tcpgateway

import (
	"testing"

	"lan-agent-hub/internal/protocol"
)

// nopConn 是 transport.Conn 的空实现：Close 只置标记，不碰网络。
type nopConn struct{ closed bool }

func (c *nopConn) Recv() (protocol.Envelope, error) { return protocol.Envelope{}, nil }
func (c *nopConn) Send(protocol.Envelope) error     { return nil }
func (c *nopConn) Close() error                     { c.closed = true; return nil }

// newTestSession 造一个不接网络的会话：只测 Deliver 的记账与丢弃语义。
func newTestSession(buf int, stats *deliverStats) *session {
	return &session{
		conn:   &nopConn{},
		sendCh: make(chan protocol.Envelope, buf),
		closed: make(chan struct{}),
		stats:  stats,
	}
}

// 队列满必须计「丢弃」而不是「已关闭」——这两个数字含义完全不同：
// 前者说明消费跟不上（要扩容或压背压），后者说明连接已经死了（属正常收尾）。
func TestDeliverCountsQueueFullAsDropped(t *testing.T) {
	stats := &deliverStats{}
	s := newTestSession(1, stats)

	s.Deliver(protocol.Envelope{Type: protocol.TypeChat}) // 入队，占满
	s.Deliver(protocol.Envelope{Type: protocol.TypeChat}) // 队列满 → 丢

	if got := stats.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d，期望 1", got)
	}
	if got := stats.closed.Load(); got != 0 {
		t.Fatalf("closed = %d，期望 0（连接没关，不该计入关闭）", got)
	}
}

// 连接已关闭后继续下发，计「已关闭」，且不再占用队列。
func TestDeliverCountsClosedAfterClose(t *testing.T) {
	stats := &deliverStats{}
	s := newTestSession(4, stats) // 队列有空位，排除「满」的干扰
	_ = s.Close()

	s.Deliver(protocol.Envelope{Type: protocol.TypeChat})

	if got := stats.closed.Load(); got != 1 {
		t.Fatalf("closed = %d，期望 1", got)
	}
	if got := stats.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d，期望 0（不是队列满）", got)
	}
	if n := len(s.sendCh); n != 0 {
		t.Fatalf("关闭后不应再往队列里塞消息，实际 %d 条", n)
	}
}

// 未提供统计口子（例如纯内核形态的测试装配）时不得 panic。
func TestDeliverWithoutStatsDoesNotPanic(t *testing.T) {
	s := newTestSession(1, nil)
	s.Deliver(protocol.Envelope{Type: protocol.TypeChat})
	s.Deliver(protocol.Envelope{Type: protocol.TypeChat}) // 满 → default 分支
	_ = s.Close()
	s.Deliver(protocol.Envelope{Type: protocol.TypeChat}) // 关闭分支
}

// DropStats 与 session 记账同源：这是 /metrics 上那两个数字的来源。
func TestGatewayDropStatsReadsThrough(t *testing.T) {
	g := &TCPGateway{}
	s := newTestSession(1, &g.stats)
	s.Deliver(protocol.Envelope{})
	s.Deliver(protocol.Envelope{}) // 丢弃
	_ = s.Close()

	dropped, closed := g.DropStats()
	if dropped != 1 {
		t.Fatalf("dropped = %d，期望 1", dropped)
	}
	if closed != 0 {
		t.Fatalf("closed = %d，期望 0", closed)
	}
}
