package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/protocol"
)

// fakeSession 是 core.Session 的内存实现：记录下发的信封，便于断言离线回放结果。
type fakeSession struct {
	mu  sync.Mutex
	got []protocol.Envelope
}

func (f *fakeSession) ID() string { return "B" }
func (f *fakeSession) Deliver(env protocol.Envelope) {
	f.mu.Lock()
	f.got = append(f.got, env)
	f.mu.Unlock()
}
func (f *fakeSession) Close() error { return nil }
func (f *fakeSession) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// TestEngineAppendQuerySince 验证追加 / 查询 / 续传 / LastSeq 的基本契约。
func TestEngineAppendQuerySince(t *testing.T) {
	dir := t.TempDir()
	eng, err := Open(dir, 1000, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	recs := []Record{
		{Seq: 1, TS: 100, Type: protocol.TypeChat, From: "A", To: "B", Content: `{"text":"hi"}`},
		{Seq: 2, TS: 200, Type: protocol.TypeBroadcast, From: "A", To: "", Content: `{"text":"all"}`},
		{Seq: 3, TS: 300, Type: protocol.TypeRoom, From: "A", To: "", RoomID: "r1", Content: `{"text":"room"}`},
		{Seq: 4, TS: 400, Type: protocol.TypeChat, From: "A", To: "C", Content: `{"text":"toC"}`},
	}
	for _, r := range recs {
		if err := eng.Append(r); err != nil {
			t.Fatalf("Append seq=%d: %v", r.Seq, err)
		}
	}
	if got := eng.LastSeq(); got != 4 {
		t.Fatalf("LastSeq = %d want 4", got)
	}
	// Query 按 To 过滤。
	toB := eng.Query(Filter{To: "B"})
	if len(toB) != 1 || toB[0].Seq != 1 {
		t.Fatalf("Query To=B = %+v want [seq1]", toB)
	}
	// Since(2) 返回 seq>2 的 2 条（seq3,seq4，升序）。
	since := eng.Since(2, 100)
	if len(since) != 2 || since[0].Seq != 3 || since[1].Seq != 4 {
		t.Fatalf("Since(2) = %+v want seq3,seq4", since)
	}
	// maxReplay 上限生效。
	limited := eng.Since(0, 2)
	if len(limited) != 2 {
		t.Fatalf("Since(0,2) = %d want 2", len(limited))
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestEngineReload 验证「崩溃安全」：重启后索引重建，且不完整的尾行被丢弃。
func TestEngineReload(t *testing.T) {
	dir := t.TempDir()
	eng, _ := Open(dir, 1000, 0)
	for i := int64(1); i <= 3; i++ {
		_ = eng.Append(Record{Seq: i, TS: i * 100, Type: protocol.TypeChat, From: "A", To: "B", Content: `{}`})
	}
	_ = eng.Close()

	// 重新打开：索引应重建，LastSeq=3。
	reopen, err := Open(dir, 1000, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopen.LastSeq() != 3 {
		t.Fatalf("reload LastSeq = %d want 3", reopen.LastSeq())
	}
	if n := len(reopen.recs); n != 3 {
		t.Fatalf("reload recs = %d want 3", n)
	}
	_ = reopen.Close()

	// 模拟进程崩溃：在完整日志尾部追加一行半截 JSON（无换行）。
	f, _ := os.OpenFile(filepath.Join(dir, "messages.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"seq":4,"ts":400,"type":"chat","from":"A","to":"B","content":{`)
	_ = f.Close()

	// 再次打开：半截行必须被丢弃，LastSeq 仍为 3、recs 仍为 3（绝不把半截当真）。
	recover, err := Open(dir, 1000, 0)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recover.LastSeq() != 3 {
		t.Fatalf("after corrupt tail LastSeq = %d want 3", recover.LastSeq())
	}
	if n := len(recover.recs); n != 3 {
		t.Fatalf("after corrupt tail recs = %d want 3", n)
	}
	_ = recover.Close()
}

// TestEngineVacuumRetention 验证 Vacuum 按 retentionDays 淘汰过期消息。
func TestEngineVacuumRetention(t *testing.T) {
	dir := t.TempDir()
	eng, _ := Open(dir, 1000, 7) // 保留 7 天
	old := time.Now().UnixMilli() - 30*24*60*60*1000
	recent := time.Now().UnixMilli()
	_ = eng.Append(Record{Seq: 1, TS: old, Type: protocol.TypeChat, From: "A", To: "B", Content: `{}`})
	_ = eng.Append(Record{Seq: 2, TS: recent, Type: protocol.TypeChat, From: "A", To: "B", Content: `{}`})

	res, err := eng.Vacuum()
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	if res.Pruned != 1 {
		t.Fatalf("Pruned = %d want 1", res.Pruned)
	}
	if res.MessageCount != 1 {
		t.Fatalf("MessageCount = %d want 1", res.MessageCount)
	}
	if eng.LastSeq() != 2 {
		t.Fatalf("LastSeq after prune = %d want 2", eng.LastSeq())
	}
	_ = eng.Close()
}

// TestStoreReplay 端到端验证离线回放：「与重连客户端相关」的消息才补发。
func TestStoreReplay(t *testing.T) {
	hub := core.NewHub()
	// 建两个房间：r1 含 B（应补发），r2 不含 B（应跳过）。
	// 注意：Create 返回服务端生成的房间 ID，不能用名称 "r1"/"r2" 当 RoomID。
	r1 := hub.Rooms.Create("r1", []string{"A", "B"})
	r2 := hub.Rooms.Create("r2", []string{"A", "C"})

	st := &Store{hub: hub, maxReplay: 500}
	dir := t.TempDir()
	eng, err := Open(dir, 1000, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.engine.Store(eng)

	// 5 条历史消息：1 私聊给 B、2 广播、3 r1（B 是成员）、4 私聊给 C、5 r2（B 非成员）。
	seed := []Record{
		{Seq: 1, TS: 1, Type: protocol.TypeChat, From: "A", To: "B", Content: `{"text":"toB"}`},
		{Seq: 2, TS: 2, Type: protocol.TypeBroadcast, From: "A", To: "", Content: `{"text":"all"}`},
		{Seq: 3, TS: 3, Type: protocol.TypeRoom, From: "A", To: "", RoomID: r1, Content: `{"text":"r1"}`},
		{Seq: 4, TS: 4, Type: protocol.TypeChat, From: "A", To: "C", Content: `{"text":"toC"}`},
		{Seq: 5, TS: 5, Type: protocol.TypeRoom, From: "A", To: "", RoomID: r2, Content: `{"text":"r2"}`},
	}
	for _, r := range seed {
		_ = eng.Append(r)
	}

	sess := &fakeSession{}
	st.replay(sess, "B", 0) // 重连的 B 要求补发 seq>0 的离线消息

	if got := sess.count(); got != 3 {
		t.Fatalf("replay to B delivered %d want 3 (toB, all, r1)", got)
	}
	// 校验补发的 seq 集合确实是 {1,2,3}。
	sess.mu.Lock()
	seqs := map[int64]bool{}
	for _, e := range sess.got {
		seqs[e.Seq] = true
	}
	sess.mu.Unlock()
	for _, want := range []int64{1, 2, 3} {
		if !seqs[want] {
			t.Errorf("replay missing seq %d", want)
		}
	}
	if seqs[4] || seqs[5] {
		t.Errorf("replay leaked seq 4/5 (private to C / room r2 without B): %v", seqs)
	}

	// sinceSeq 门控：若 B 已收到过 seq<=3，则只补发 seq>3（本例无，应 0 条）。
	sess2 := &fakeSession{}
	st.replay(sess2, "B", 3)
	if got := sess2.count(); got != 0 {
		t.Fatalf("replay since=3 delivered %d want 0", got)
	}
	_ = eng.Close()
}
