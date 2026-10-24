package tcpgateway

import (
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/protocol"
)

// batchRecorder 记录每次 SendBatch 收到的帧（用于断言合并效果）。
type batchRecorder struct {
	nopConn
	mu      sync.Mutex
	batches [][]protocol.Envelope
	singles []protocol.Envelope
	signal  chan struct{}
}

func newBatchRecorder() *batchRecorder {
	return &batchRecorder{signal: make(chan struct{}, 1024)}
}

func (b *batchRecorder) Send(env protocol.Envelope) error {
	b.mu.Lock()
	b.singles = append(b.singles, env)
	b.mu.Unlock()
	b.signal <- struct{}{}
	return nil
}

func (b *batchRecorder) SendBatch(envs []protocol.Envelope) error {
	b.mu.Lock()
	b.batches = append(b.batches, append([]protocol.Envelope(nil), envs...))
	b.mu.Unlock()
	b.signal <- struct{}{}
	return nil
}

func (b *batchRecorder) snapshot() (batches [][]protocol.Envelope, singles []protocol.Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.batches, b.singles
}

func chatEnv(i int) protocol.Envelope {
	return protocol.NewEnvelope(protocol.TypeChat, "a", "b", "", map[string]string{"i": string(rune('a' + i))})
}

// 队列里已有的多条消息必须合并成一次写出——这是扇出场景吞吐的关键。
func TestWriteLoopBatchesQueuedMessages(t *testing.T) {
	rec := newBatchRecorder()
	s := &session{conn: rec, sendCh: make(chan protocol.Envelope, 64), closed: make(chan struct{})}

	const n = 10
	for i := 0; i < n; i++ {
		s.sendCh <- chatEnv(i)
	}
	// 先把消息全部入队再启动写协程：第一次 drain 必然一次取空，
	// 因此「一次 SendBatch 收 10 条」是确定性的，不是碰运气。
	go s.writeLoop()

	select {
	case <-rec.signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("写协程没有写出任何批次")
	}

	batches, singles := rec.snapshot()
	if len(singles) != 0 {
		t.Fatalf("已实现 BatchConn 时不应回退到逐条 Send，实际 %d 条", len(singles))
	}
	if len(batches) != 1 {
		t.Fatalf("应合并成 1 批，实际 %d 批", len(batches))
	}
	if len(batches[0]) != n {
		t.Fatalf("批内帧数 = %d，期望 %d", len(batches[0]), n)
	}
	// 合批绝不能乱序：单连接内顺序不变是硬约束。
	for i, env := range batches[0] {
		var got map[string]string
		if err := protocol.UnmarshalContent(env.Content, &got); err != nil {
			t.Fatalf("第 %d 帧 content 解不开: %v", i, err)
		}
		if want := string(rune('a' + i)); got["i"] != want {
			t.Fatalf("第 %d 帧 = %q，期望 %q（合批必须保序）", i, got["i"], want)
		}
	}
}

// 单批不得超过 maxWriteBatch：不能让一次 Write 无限变大而拉长队头延迟。
func TestWriteLoopCapsBatchSize(t *testing.T) {
	rec := newBatchRecorder()
	s := &session{conn: rec, sendCh: make(chan protocol.Envelope, 256), closed: make(chan struct{})}

	total := maxWriteBatch + 20
	for i := 0; i < total; i++ {
		s.sendCh <- chatEnv(i % 20)
	}
	go s.writeLoop()

	deadline := time.After(3 * time.Second)
	for {
		batches, _ := rec.snapshot()
		size := 0
		for _, b := range batches {
			if len(b) > maxWriteBatch {
				t.Fatalf("单批 %d 条超过上限 %d", len(b), maxWriteBatch)
			}
			size += len(b)
		}
		if size >= total {
			return
		}
		select {
		case <-rec.signal:
		case <-deadline:
			t.Fatalf("只写出 %d/%d 条", size, total)
		}
	}
}

// 连接不支持批量能力时必须逐条退化，行为与历史一致（不能因为没实现接口就丢消息）。
func TestWriteLoopFallsBackToSingleSend(t *testing.T) {
	rec := &singleRecorder{signal: make(chan struct{}, 64)}
	s := &session{conn: rec, sendCh: make(chan protocol.Envelope, 16), closed: make(chan struct{})}

	const n = 5
	for i := 0; i < n; i++ {
		s.sendCh <- chatEnv(i)
	}
	go s.writeLoop()

	for i := 0; i < n; i++ {
		select {
		case <-rec.signal:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 条没有写出", i)
		}
	}
	rec.mu.Lock()
	got := append([]protocol.Envelope(nil), rec.singles...)
	rec.mu.Unlock()
	if len(got) != n {
		t.Fatalf("逐条发送 %d 条，期望 %d", len(got), n)
	}
	for i, env := range got {
		var m map[string]string
		_ = protocol.UnmarshalContent(env.Content, &m)
		if want := string(rune('a' + i)); m["i"] != want {
			t.Fatalf("第 %d 条 = %q，期望 %q", i, m["i"], want)
		}
	}
}

// singleRecorder 只实现 Conn（不实现 BatchConn），用于验证退化路径。
type singleRecorder struct {
	nopConn
	mu      sync.Mutex
	singles []protocol.Envelope
	signal  chan struct{}
}

func (r *singleRecorder) Send(env protocol.Envelope) error {
	r.mu.Lock()
	r.singles = append(r.singles, env)
	r.mu.Unlock()
	r.signal <- struct{}{}
	return nil
}
