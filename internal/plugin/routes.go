package plugin

import (
	"sort"
	"sync"
	"sync/atomic"
)

// gate 是插件的「路由闸门」。
//
// 背景：http.ServeMux 不支持注销路由，而 V4 要求「停用某插件后它的端点立即消失」
// （典型验收：停用 webconsole 后 / 返回 404，而 /api/plugins 仍可用）。
// 做法是在注册时套一层极薄的闸门 handler：插件处于「已装配/运行中」时原样放行，
// 被停用/失败时直接 404。放行路径与原始 handler 完全等价，故契约响应逐字节不变。
type gate struct {
	on int32
}

// newGate 构造一个默认关闭的闸门（StateRegistered 时路由尚不可用）。
func newGate() *gate { return &gate{} }

// set 切换闸门开关。
func (g *gate) set(on bool) {
	if g == nil {
		return
	}
	var v int32
	if on {
		v = 1
	}
	atomic.StoreInt32(&g.on, v)
}

// enabled 返回闸门是否放行。
func (g *gate) enabled() bool {
	if g == nil {
		return true // 无闸门（如测试直接装配 mux）时一律放行
	}
	return atomic.LoadInt32(&g.on) == 1
}

// stateGateOn 决定某状态下路由是否应放行：
// 装配完成（initialized）即可放行——否则 httpgateway.Start 起服务到 webconsole.Start
// 之间会出现一个「/ 短暂 404」的窗口；运行中当然放行；其余状态一律关闭。
func stateGateOn(s State) bool {
	return s == StateInitialized || s == StateRunning
}

// routeEntry 是影子路由表中的一条记录。
type routeEntry struct {
	owner   string // 归属插件名
	pattern string // 路由 pattern
	gate    *gate  // 归属插件的闸门（真实 handler 由 mux 持有，这里只做归属与闸门登记）
}

// routeTable 是影子路由表：pattern → 归属插件。
//
// 作用有二：
//  1. 冲突预检——不依赖 *http.ServeMux 对重复 pattern 的 panic 行为；
//  2. 路由归属查询——供 /api/plugins 展示「这条路由是谁注册的」，以及排障。
//
// 用显式锁而不是 sync.Map：此表只在 Init/启停与查询时访问，读多写极少，
// 显式 RWMutex 的语义比 sync.Map 更清晰。
type routeTable struct {
	mu      sync.RWMutex
	entries map[string]*routeEntry
}

// newRouteTable 构造空路由表。
func newRouteTable() *routeTable {
	return &routeTable{entries: make(map[string]*routeEntry)}
}

// get 查询某个 pattern 的归属记录。
func (t *routeTable) get(pattern string) (*routeEntry, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[pattern]
	return e, ok
}

// set 写入/覆盖某 pattern 的归属记录（同插件重复注册时用于刷新 handler 归属）。
func (t *routeTable) set(pattern string, e *routeEntry) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[pattern] = e
}

// removeByOwner 摘除某插件的全部路由记录（插件被彻底移除时使用）。
func (t *routeTable) removeByOwner(owner string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for p, e := range t.entries {
		if e.owner == owner {
			delete(t.entries, p)
		}
	}
}

// list 导出全部路由归属，按 Pattern 升序（确定性输出，便于 golden 断言）。
func (t *routeTable) list() []RouteInfo {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	out := make([]RouteInfo, 0, len(t.entries))
	for p, e := range t.entries {
		out = append(out, RouteInfo{Pattern: p, Plugin: e.owner})
	}
	t.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// ofOwner 返回某插件注册的全部 pattern（按字典序）。
func (t *routeTable) ofOwner(owner string) []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	var out []string
	for p, e := range t.entries {
		if e.owner == owner {
			out = append(out, p)
		}
	}
	t.mu.RUnlock()
	sort.Strings(out)
	return out
}
