package core

import (
	"sync"
	"time"

	"lan-agent-hub/internal/protocol"
)

// defaultPresenceFlush 是 presence 合批的时间窗。
//
// 100ms 是「人眼无感」与「压制风暴」的折中：上下线通知晚 100ms 没人会发现，
// 但 1000 个连接同时上线时，派发量从 O(N²)（百万次）降到每窗口一次 O(N)。
const defaultPresenceFlush = 100 * time.Millisecond

// Presence 订阅上下线事件，并向所有在线 Agent 广播 presence 信封。
//
// 它只依赖 Registry 与 Router，不关心是谁发布的事件 —— 新增「房间变更通知」这类
// 需求时，只需再加一个订阅者，Registry 与 Router 都不用改。
//
// **为什么必须合批**：每个上下线事件都要写给全部在线 Agent，N 个 Agent 一阵抖动
// 就是 N² 次投递。1000 个 Agent 在几秒内陆续注册是百万级投递，会瞬间灌满每连接
// 的发送队列——而队列满时的丢弃不区分类型，业务消息被 presence 顺手一起丢掉。
// 压测表现极具迷惑性：连接 100% 成功、p99 不到 1ms、业务消息却丢了 9%。
// 合批把「每个事件一次全网广播」变成「每个时间窗一次全网广播」。
type Presence struct {
	reg    *Registry
	router *Router

	// FlushInterval 是合批窗口（<=0 表示不合批，回退到逐事件广播的旧行为）。
	FlushInterval time.Duration

	mu        sync.Mutex
	pending   map[string]string // id → 最新状态（同一 id 后来的状态覆盖先前）
	order     []string          // 首次出现顺序，保证批次内顺序稳定、结果可复现
	lastFlush time.Time
	timer     *time.Timer
	batches   int64 // 已发出的批次数（测试与诊断用）
	singles   int64 // 其中「只含一条变更」的次数
	items     int64 // 累计发出多少条变更（合批前后应相等，证明没有丢事件）
}

// presenceBatchItem 是批次里的单条变更。
type presenceBatchItem struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func NewPresence(reg *Registry, router *Router) *Presence {
	return &Presence{
		reg:           reg,
		router:        router,
		FlushInterval: defaultPresenceFlush,
		pending:       make(map[string]string, 8),
		// 从「刚刚 flush 过」开始计时：这样第一条变更也进窗口，
		// 一批注册不会因为「首条立即发出」而裂成 1 + (n-1) 两个批次。
		lastFlush: time.Now(),
	}
}

// Broadcast 记录一条上下线变更，按合批窗口发出。
// 使用 BroadcastRaw：不分配 seq —— 与历史行为一致（presence 的 seq 恒为 0）。
func (p *Presence) Broadcast(status, id string) {
	if p.FlushInterval <= 0 {
		p.emit([]presenceBatchItem{{ID: id, Status: status}})
		return
	}
	p.mu.Lock()
	if _, ok := p.pending[id]; !ok {
		p.order = append(p.order, id)
	}
	p.pending[id] = status

	due := time.Since(p.lastFlush) >= p.FlushInterval
	if !due && p.timer == nil {
		// 窗口内还有待发变更：用一个定时器兜底，保证最后一批一定会发出去，
		// 不会因为「之后再没有事件」而永远留在 pending 里。
		p.timer = time.AfterFunc(p.FlushInterval, p.flushLater)
	}
	p.mu.Unlock()

	if due {
		p.flush()
	}
}

// flushLater 是定时器回调。
func (p *Presence) flushLater() {
	p.mu.Lock()
	p.timer = nil
	p.mu.Unlock()
	p.flush()
}

// Flush 立即发出所有待发变更（停机、测试用）。
func (p *Presence) Flush() { p.flush() }

// Stats 返回（批次数, 单条批次数, 累计变更数），供诊断与测试断言合批确实生效。
func (p *Presence) Stats() (batches, singles, items int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batches, p.singles, p.items
}

func (p *Presence) flush() {
	p.mu.Lock()
	if len(p.order) == 0 {
		p.lastFlush = time.Now()
		p.mu.Unlock()
		return
	}
	order := p.order
	pend := p.pending
	p.order = nil
	p.pending = make(map[string]string, len(order))
	p.lastFlush = time.Now()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()

	items := make([]presenceBatchItem, 0, len(order))
	for _, id := range order {
		items = append(items, presenceBatchItem{ID: id, Status: pend[id]})
	}
	p.emit(items)
}

// emit 发出一个（或一批）presence 信封。
//
// 只有一条变更时沿用历史形状 `{id, status}`——绝大多数场景（单个 Agent 上下线）
// 的老客户端因此完全不受影响。多条才用 `{changes: [...]}`，一次投递表达全部变更。
func (p *Presence) emit(items []presenceBatchItem) {
	p.mu.Lock()
	p.batches++
	if len(items) == 1 {
		p.singles++
	}
	p.items += int64(len(items))
	p.mu.Unlock()

	if len(items) == 1 {
		p.router.BroadcastRaw(protocol.NewEnvelope(protocol.TypePresence, "hub", "", "",
			map[string]string{"id": items[0].ID, "status": items[0].Status}))
		return
	}
	p.router.BroadcastRaw(protocol.NewEnvelope(protocol.TypePresence, "hub", "", "",
		map[string]any{"changes": items, "count": len(items)}))
}
