package core

import (
	"encoding/json"
	"testing"
	"time"

	"lan-agent-hub/internal/protocol"
)

// countPresence 统计某个会话收到的 presence 信封。
func countPresence(s *mockSession) []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.Envelope
	for _, e := range s.got {
		if e.Type == protocol.TypePresence {
			out = append(out, e)
		}
	}
	return out
}

func decodePresence(t *testing.T, e protocol.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Content, &m); err != nil {
		t.Fatalf("presence content 不是合法 JSON: %v (%s)", err, string(e.Content))
	}
	return m
}

// 单条变更必须保持历史形状 {id, status}：绝大多数客户端只看这个形状，
// 合批不能把它们的老解析器打挂。
func TestPresenceSingleChangeKeepsLegacyShape(t *testing.T) {
	reg := NewRegistry()
	router := NewRouter(reg, NewRoomService())
	sess := &mockSession{id: "watcher"}
	reg.Bind("watcher", "w", nil, 1, sess)

	pres := NewPresence(reg, router)
	pres.FlushInterval = 0 // 不合批：逐事件发出
	pres.Broadcast("online", "a1")

	got := countPresence(sess)
	if len(got) != 1 {
		t.Fatalf("应收 1 条 presence，实际 %d", len(got))
	}
	m := decodePresence(t, got[0])
	if m["id"] != "a1" || m["status"] != "online" {
		t.Fatalf("单条变更应保持 {id,status} 形状，实际 %v", m)
	}
	if _, has := m["changes"]; has {
		t.Fatalf("单条变更不应带 changes 字段")
	}
	if got[0].Seq != 0 {
		t.Fatalf("presence 的 seq 恒为 0，实际 %d", got[0].Seq)
	}
}

// 窗口内多条变更必须合批成一条信封：这正是 O(N²) → O(N) 的关键。
func TestPresenceBatchesManyChangesIntoOneEnvelope(t *testing.T) {
	reg := NewRegistry()
	router := NewRouter(reg, NewRoomService())
	sess := &mockSession{id: "watcher"}
	reg.Bind("watcher", "w", nil, 1, sess)

	pres := NewPresence(reg, router)
	pres.FlushInterval = time.Hour // 保证窗口内不会自动 flush

	ids := []string{"a1", "a2", "a3", "a4", "a5"}
	for _, id := range ids {
		pres.Broadcast("online", id)
	}
	pres.Flush()

	got := countPresence(sess)
	if len(got) != 1 {
		t.Fatalf("5 条变更应合成 1 条信封，实际 %d 条", len(got))
	}
	m := decodePresence(t, got[0])
	raw, ok := m["changes"].([]any)
	if !ok {
		t.Fatalf("合批信封应带 changes 数组，实际 %v", m)
	}
	if len(raw) != len(ids) {
		t.Fatalf("changes 条数 = %d，期望 %d", len(raw), len(ids))
	}
	if n, _ := m["count"].(float64); int(n) != len(ids) {
		t.Fatalf("count = %v，期望 %d", m["count"], len(ids))
	}
	// 顺序必须稳定：同一输入两次跑出同一结果，报告才可复现。
	for i, item := range raw {
		one, _ := item.(map[string]any)
		if one["id"] != ids[i] {
			t.Fatalf("第 %d 条应为 %s，实际 %v", i, ids[i], one["id"])
		}
	}

	// 不得丢事件：累计变更数等于输入数。
	if _, _, items := pres.Stats(); items != int64(len(ids)) {
		t.Fatalf("累计变更数 = %d，期望 %d（合批不能丢事件）", items, len(ids))
	}
	if batches, _, _ := pres.Stats(); batches != 1 {
		t.Fatalf("批次数 = %d，期望 1", batches)
	}
}

// 同一 id 在同一窗口内反复上下线，只需保留最新状态——这正是「抖动」被压平的原理。
func TestPresenceSameIDCollapsesToLatestStatus(t *testing.T) {
	reg := NewRegistry()
	router := NewRouter(reg, NewRoomService())
	sess := &mockSession{id: "watcher"}
	reg.Bind("watcher", "w", nil, 1, sess)

	pres := NewPresence(reg, router)
	pres.FlushInterval = time.Hour
	for i := 0; i < 10; i++ {
		pres.Broadcast("online", "flappy")
		pres.Broadcast("offline", "flappy")
	}
	pres.Broadcast("online", "flappy")
	pres.Flush()

	got := countPresence(sess)
	if len(got) != 1 {
		t.Fatalf("应收 1 条 presence，实际 %d", len(got))
	}
	m := decodePresence(t, got[0])
	// 只剩一个 id，因此退化为单条形状（最新状态）。
	if m["id"] != "flappy" || m["status"] != "online" {
		t.Fatalf("应保留最新状态 online，实际 %v", m)
	}
}

// 窗口结束后必须有兜底 flush：否则「最后一次变更」会永远留在 pending 里。
func TestPresenceFlushesOnTimerWithoutFurtherEvents(t *testing.T) {
	reg := NewRegistry()
	router := NewRouter(reg, NewRoomService())
	sess := &mockSession{id: "watcher"}
	reg.Bind("watcher", "w", nil, 1, sess)

	pres := NewPresence(reg, router)
	pres.FlushInterval = 20 * time.Millisecond

	pres.Broadcast("online", "a1")
	pres.Broadcast("online", "a2") // 第二条触发定时器兜底

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(countPresence(sess)) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(countPresence(sess)) == 0 {
		t.Fatalf("窗口内最后一批变更没有被发出（缺兜底 flush）")
	}
}

// 端到端：N 个 Agent 注册，presence 派发次数应为 O(批次数) 而非 O(N²)。
//
// 这里用 Registry 里真实挂着的 N 个会话，直接数 Deliver 次数。
func TestPresenceRegistrationStormIsNotQuadratic(t *testing.T) {
	const n = 200
	reg := NewRegistry()
	router := NewRouter(reg, NewRoomService())
	sessions := make([]*mockSession, n)
	for i := 0; i < n; i++ {
		id := string(rune('a' + i%26))
		id = id + "-" + itoa(i)
		s := &mockSession{id: id}
		sessions[i] = s
		reg.Bind(id, "x", nil, 1, s)
	}

	pres := NewPresence(reg, router)
	pres.FlushInterval = time.Hour // 全部压进同一窗口
	for i := 0; i < n; i++ {
		pres.Broadcast("online", sessions[i].id)
	}
	pres.Flush()

	// 每个会话最多收到 1 条 presence；不合批时这里会是 n 条（合计 n² 次 Deliver）。
	total := 0
	for _, s := range sessions {
		c := len(countPresence(s))
		if c > 1 {
			t.Fatalf("会话 %s 收到 %d 条 presence，合批后应至多 1 条", s.id, c)
		}
		total += c
	}
	if total != n {
		t.Fatalf("presence 投递次数 = %d，期望 %d（每个会话 1 条）", total, n)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
