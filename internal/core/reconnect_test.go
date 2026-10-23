package core

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"lan-agent-hub/internal/protocol"
)

// countingSession 记录 Close 次数，用于证明「被顶掉的旧连接真的被关掉了」。
type countingSession struct {
	id     string
	closes atomic.Int32
}

func (s *countingSession) ID() string { return s.id }

func (s *countingSession) Deliver(protocol.Envelope) {}

func (s *countingSession) Close() error {
	s.closes.Add(1)
	return nil
}

// 同 id 重连是局域网的常态（网络抖动、客户端重启）。旧连接的读循环要等到
// EOF 才退出，此时新连接可能已经注册并顶替了它。按 id 无条件卸载会误删新会话：
// 连接活着、延迟正常，但消息凭空消失——这正是 1000 连接压测里「送达腰斩、
// p99 却只有 0.8ms」的成因。
func TestUnbindSessionDoesNotEvictReplacement(t *testing.T) {
	reg := NewRegistry()
	oldSess := &countingSession{id: "a1"}
	newSess := &countingSession{id: "a1"}

	reg.Bind("a1", "old", nil, 1, oldSess)
	reg.Bind("a1", "new", nil, 1, newSess)

	if reg.UnbindSession("a1", oldSess) {
		t.Fatalf("旧会话不应能摘除已被顶替的在线记录")
	}
	a, ok := reg.Get("a1")
	if !ok {
		t.Fatalf("新会话被旧会话误删——连接还活着却已不在在线表里")
	}
	if a.Name != "new" {
		t.Fatalf("在线记录应属于新会话，实际 name=%q", a.Name)
	}

	// 反过来：新会话自己断开时必须能正常摘除。
	if !reg.UnbindSession("a1", newSess) {
		t.Fatalf("新会话自己断开应当摘除成功")
	}
	if _, ok := reg.Get("a1"); ok {
		t.Fatalf("新会话断开后仍在在线表里")
	}
}

// 管理面语义不能因此失效：Unbind 是「不管现在是谁，都要下线」。
func TestUnbindForceRemovesRegardlessOfSession(t *testing.T) {
	reg := NewRegistry()
	sess := &countingSession{id: "a2"}
	reg.Bind("a2", "x", nil, 1, sess)
	reg.Unbind("a2")
	if _, ok := reg.Get("a2"); ok {
		t.Fatalf("Unbind 强制摘除失败")
	}
}

func TestUnbindSessionUnknownIDIsNoop(t *testing.T) {
	reg := NewRegistry()
	if reg.UnbindSession("nobody", &countingSession{}) {
		t.Fatalf("不存在的 id 不应报告摘除成功")
	}
	// 会话不匹配时同样不得摘除。
	sess := &countingSession{id: "a3"}
	reg.Bind("a3", "y", nil, 1, sess)
	if reg.UnbindSession("a3", &countingSession{id: "a3"}) {
		t.Fatalf("会话不匹配时不应摘除")
	}
	if _, ok := reg.Get("a3"); !ok {
		t.Fatalf("会话不匹配时在线记录被误删")
	}
}

// Handler 级别的完整链路：旧会话关闭不应发 agent.offline、不应丢房间成员身份。
func TestUnregisterSessionStaleIsSilent(t *testing.T) {
	h := NewHub()
	oldSess := &countingSession{id: "a4"}
	newSess := &countingSession{id: "a4"}

	h.HandleEnvelope(oldSess, &protocol.Envelope{
		Type: protocol.TypeRegister, V: 1, From: "a4", Content: mustContent(t, map[string]any{"name": "old"}),
	})
	// 新连接顶替（模拟客户端重连）
	h.HandleEnvelope(newSess, &protocol.Envelope{
		Type: protocol.TypeRegister, V: 1, From: "a4", Content: mustContent(t, map[string]any{"name": "new"}),
	})
	if got := oldSess.closes.Load(); got != 1 {
		t.Fatalf("被顶替的旧连接应被主动关闭一次，实际 %d 次", got)
	}

	// 旧连接这时才读到 EOF。
	if h.UnregisterSession("a4", oldSess) {
		t.Fatalf("迟到的旧会话不应摘除在线记录")
	}
	if _, ok := h.Registry.Get("a4"); !ok {
		t.Fatalf("新会话被迟到的旧会话摘掉——此后它收不到任何消息")
	}

	// 新连接真正断开时才生效。
	if !h.UnregisterSession("a4", newSess) {
		t.Fatalf("新会话断开应摘除成功")
	}
	if _, ok := h.Registry.Get("a4"); ok {
		t.Fatalf("新会话断开后仍在线")
	}
}

// 同一会话重复发 register（客户端重发/幂等重试）不能被当成「被别人顶替」而自杀。
func TestRepeatedRegisterSameSessionKeepsConnection(t *testing.T) {
	h := NewHub()
	sess := &countingSession{id: "a5"}
	for i := 0; i < 3; i++ {
		h.HandleEnvelope(sess, &protocol.Envelope{
			Type: protocol.TypeRegister, V: 1, From: "a5", Content: mustContent(t, map[string]any{"name": "same"}),
		})
	}
	if got := sess.closes.Load(); got != 0 {
		t.Fatalf("同一会话重复注册不应关闭自己，实际被关 %d 次", got)
	}
	if _, ok := h.Registry.Get("a5"); !ok {
		t.Fatalf("重复注册后应仍在线")
	}
}

func mustContent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("构造 content 失败: %v", err)
	}
	return b
}
