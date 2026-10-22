package core

import (
	"testing"
)

// TestRegistryGetReturnsCopy 验证 Get 返回的是副本：修改副本不影响在线表。
func TestRegistryGetReturnsCopy(t *testing.T) {
	r := NewRegistry()
	r.Bind("A", "Alice", []string{"x"}, 2, nil)

	a, ok := r.Get("A")
	if !ok {
		t.Fatal("Get(A) not found")
	}
	// 篡改副本的字段。
	a.LastSeen = 999
	if len(a.Caps) > 0 {
		a.Caps[0] = "mutated"
	}

	orig, ok := r.Get("A")
	if !ok {
		t.Fatal("Get(A) not found after mutation")
	}
	if orig.LastSeen == 999 {
		t.Fatal("copy mutation leaked into registry (LastSeen)")
	}
	if len(orig.Caps) > 0 && orig.Caps[0] == "mutated" {
		t.Fatal("copy mutation leaked into registry (Caps slice)")
	}
}

// TestRegistryAllSortedByID 验证 All 返回按 ID 升序的稳定副本切片。
func TestRegistryAllSortedByID(t *testing.T) {
	r := NewRegistry()
	r.Bind("c", "C", nil, 0, nil)
	r.Bind("a", "A", nil, 0, nil)
	r.Bind("b", "B", nil, 0, nil)

	out := r.All()
	if len(out) != 3 {
		t.Fatalf("len = %d want 3", len(out))
	}
	want := []string{"a", "b", "c"}
	for i, a := range out {
		if a.ID != want[i] {
			t.Fatalf("All()[%d].ID = %q want %q", i, a.ID, want[i])
		}
	}
	// 副本独立性：修改 All() 返回的切片元素不影响在线表。
	out[0].LastSeen = 12345
	if again, _ := r.Get("a"); again.LastSeen == 12345 {
		t.Fatal("All() slice elements are not isolated copies")
	}
}

// TestRegistrySweepKeepsTouchedAgent 验证刚被 Touch 的 Agent 不会被 Sweep 误清。
func TestRegistrySweepKeepsTouchedAgent(t *testing.T) {
	r := NewRegistry()
	r.Bind("A", "Alice", nil, 0, nil)
	r.Touch("A") // 刷新心跳到当前时刻

	// 以 1000ms 超时清扫：刚 Touch 的 Agent 距 LastSeen 远小于 1000ms，不应被清。
	dropped := r.Sweep(1000)
	if len(dropped) != 0 {
		t.Fatalf("should not drop freshly touched agent, dropped %d", len(dropped))
	}
	if _, ok := r.Get("A"); !ok {
		t.Fatal("A should still be present after Sweep")
	}
}

// TestRegistrySweepDropsStaleAgent 对照：超时 Agent 应被清除。
func TestRegistrySweepDropsStaleAgent(t *testing.T) {
	r := NewRegistry()
	r.Bind("A", "Alice", nil, 0, nil)
	// 人为把 LastSeen 设到很久以前（直接操作 Bind 后的对象）。
	if a, ok := r.Get("A"); ok {
		// 通过设置超时负值来强制过期：用超大 timeout 反向不成立，
		// 这里用 Sweep(-1) —— 任何 now-LastSeen > -1 恒为真，全部清除。
		_ = a
	}
	dropped := r.Sweep(-1)
	if len(dropped) != 1 {
		t.Fatalf("stale agent should be dropped, got %d", len(dropped))
	}
	if _, ok := r.Get("A"); ok {
		t.Fatal("A should be gone after Sweep")
	}
}
