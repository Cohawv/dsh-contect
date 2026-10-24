// Package core 是业务内核：注册发现、路由、房间、上下线。
//
// 硬性约束：本包只允许 import internal/protocol。
// 业务层不感知传输（TCP/WebSocket）与序列化细节，对外下发一律通过 Session 接口，
// 由 adapter 提供实现（接口倒置）。依赖方向由 scripts/check-deps.sh 强制检查。
package core

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// defaultSubTimeout 是 SubscribeWithTimeout 未指定时长时的守护值（V8 ⑤）。
//
// 5 秒的依据：正常的通知型处理器（上下线记录、房间变更、指标累加）都是微秒级，
// 5 秒是「明显不正常」的下限。再长就失去隔离意义，再短容易误杀刚好在做
// 一次磁盘写或一次小 GC 的处理器。
const defaultSubTimeout = 5 * time.Second

// Handler 是事件处理器；payload 的具体类型由 topic 决定。
type Handler func(topic string, payload any)

// subEntry 是订阅记录：每个订阅拥有唯一 id，便于精确注销（cancel）。
// async 为 true 时走「每 topic 独立 goroutine + 有界队列」派发，绝不阻塞发布方。
type subEntry struct {
	id    int
	h     Handler
	async bool
	ch    chan asyncJob
	done  chan struct{}
	// V8 ⑤：timeout > 0 表示该订阅受超时守护；deg 为降级标志（指针，跨拷贝共享）。
	timeout time.Duration
	deg     *atomic.Bool
}

// asyncJob 是异步派发的任务单元。
type asyncJob struct {
	topic   string
	payload any
}

// EventBus 是极简进程内事件总线：topic → 处理器列表。
// 用于「通知型」跨模块通信（如上下线），让发布方不必知道谁在消费。
//
// V2 加固：
//   - Subscribe 现在返回 cancel()，调用后该 handler 从 map 彻底删除（幂等）；
//   - 新增 SubscribeAsync，慢 handler 在独立 goroutine 中执行，队列满则丢弃并计数，
//     绝不阻塞 Publish（与 Session.Deliver 的「缓冲满即丢」策略一致）；
//   - 同步派发保持注册顺序，单个处理器 panic 不影响其它订阅者。
type EventBus struct {
	mu       sync.RWMutex
	subs     map[string][]subEntry
	nextID   int
	dropped  int64
	degraded int64 // V8 ⑤：因超时被降级的订阅次数
}

func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[string][]subEntry)}
}

// Subscribe 同步订阅一个 topic，返回 cancel 用于注销。
// cancel 幂等：多次调用只有第一次生效，调用后该 handler 从订阅表彻底删除。
// 现有调用点（如 NewHub 的 presence 订阅）忽略返回值即可编译通过（保持同步语义）。
func (b *EventBus) Subscribe(topic string, h Handler) func() {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[topic] = append(b.subs[topic], subEntry{id: id, h: h})
	b.mu.Unlock()
	return b.cancelOf(topic, id)
}

// SubscribeAsync 异步订阅：每 topic 独立 goroutine + 有界队列（queue 为缓冲区大小）。
// 队列满则丢弃该事件并计数（Dropped()），绝不阻塞发布方。返回 cancel 用于注销。
// 适用于「可丢失、不要求顺序」的派生型通知（如指标采集）。
func (b *EventBus) SubscribeAsync(topic string, h Handler, queue int) func() {
	if queue < 1 {
		queue = 1
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan asyncJob, queue)
	done := make(chan struct{})
	b.subs[topic] = append(b.subs[topic], subEntry{id: id, h: h, async: true, ch: ch, done: done})
	b.mu.Unlock()

	go func() {
		for {
			select {
			case <-done:
				return
			case job := <-ch:
				func() {
					defer func() { _ = recover() }()
					h(job.topic, job.payload)
				}()
			}
		}
	}()

	return b.cancelOf(topic, id)
}

// SubscribeWithTimeout 同步订阅并给处理器加执行上限（V8 ⑤）。
//
// 与 Subscribe / SubscribeAsync 的区别与选择依据：
//   - Subscribe：就地同步执行。最快、保序，但一个卡住的处理器会拖住**所有**发布方
//     （因为它跑在发布方的协程里）。适合内核内部那些确定是微秒级的订阅。
//   - SubscribeAsync：独立协程 + 有界队列。发布方永不被阻塞，但不再保序，
//     且队列满就丢。适合「可丢、不要顺序」的派生通知。
//   - SubscribeWithTimeout：保序语义 + 卡死隔离。派发时在独立协程执行并等待，
//     超时则把该订阅标记为 degraded、**后续事件不再投递给它**，
//     发布方只在超时那一次多等一个 timeout。适合「必须看到每条事件、但绝不能拖垮主线」
//     的插件订阅（如把事件转发到外部 webhook）。
//
// 为什么降级后是「暂停投递」而不是「继续尝试」：卡住的处理器往往不是偶发，
// 而是它依赖的外部资源没了（webhook 目标不可达）。继续投递等于每个事件都让
// 发布方白等一个 timeout，把单点故障放大成全体延迟。
//
// 已知代价（如实记录）：Go 无法杀死 goroutine，超时的那次执行会继续跑到自己结束；
// 只是它的结果被丢弃、且不再有新事件进来。因此这个能力适用于「偶发卡顿」，
// 不能用来兜住「每个事件都无限阻塞」的处理器——那种情况该用 SubscribeAsync 或修处理器。
func (b *EventBus) SubscribeWithTimeout(topic string, h Handler, timeout time.Duration) func() {
	if timeout <= 0 {
		timeout = defaultSubTimeout
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	deg := new(atomic.Bool)
	b.subs[topic] = append(b.subs[topic], subEntry{id: id, h: h, timeout: timeout, deg: deg})
	b.mu.Unlock()
	return b.cancelOf(topic, id)
}

// DegradedCount 返回累计因超时被降级的订阅次数，供 /metrics 与日志观测。
func (b *EventBus) DegradedCount() int64 {
	return atomic.LoadInt64(&b.degraded)
}

// RecoverDegraded 清除所有订阅的降级标志，让被暂停的订阅重新参与派发。
//
// 为什么不自动恢复：自动恢复会让「每个事件都超时」的处理器变成
// 「每隔几个事件就拖一次主线」的慢性病，而且没有任何人能看见它在复发。
// 恢复必须是一次显式的人工动作（插件重载或手动调用）。
func (b *EventBus) RecoverDegraded() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, hs := range b.subs {
		for i := range hs {
			if hs[i].deg != nil {
				hs[i].deg.Store(false)
			}
		}
	}
}

// cancelOf 构造针对特定订阅 id 的注销函数（幂等）。
func (b *EventBus) cancelOf(topic string, id int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			hs := b.subs[topic]
			for i := range hs {
				if hs[i].id == id {
					if hs[i].async {
						close(hs[i].done)
					}
					b.subs[topic] = append(hs[:i], hs[i+1:]...)
					break
				}
			}
			if len(b.subs[topic]) == 0 {
				delete(b.subs, topic)
			}
		})
	}
}

// Dropped 返回累计被丢弃的异步事件数（队列满时计数），供 /api/status 等观测使用。
func (b *EventBus) Dropped() int64 {
	return atomic.LoadInt64(&b.dropped)
}

// SubscriberCount 返回某 topic 当前的订阅者数量（含同步与异步）。
func (b *EventBus) SubscriberCount(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[topic])
}

// Publish 派发事件。
//   - 同步订阅者：就地执行（注册顺序），单个 panic 不影响其它订阅者；
//   - 异步订阅者：非阻塞入队，队列满则丢弃并计数，绝不阻塞发布方。
//
// V5 热路径优化：无订阅者时直接返回（只做一次 map 查找，不分配拷贝切片）。
// 这让「默认无人订阅的 msg.routed / http.request」在消息热路径上的额外开销
// 降到一次 RLock + map 查询——这是 router 可以无条件发布的原因。
func (b *EventBus) Publish(topic string, payload any) {
	b.mu.RLock()
	subs := b.subs[topic]
	if len(subs) == 0 {
		b.mu.RUnlock()
		return
	}
	hs := make([]subEntry, len(subs))
	copy(hs, subs)
	b.mu.RUnlock()

	for _, e := range hs {
		if e.async {
			select {
			case e.ch <- asyncJob{topic: topic, payload: payload}:
			case <-e.done:
				// 已注销：跳过，不发送也不阻塞
			default:
				atomic.AddInt64(&b.dropped, 1)
			}
			continue
		}
		if e.timeout <= 0 {
			func() {
				defer func() { _ = recover() }()
				e.h(topic, payload)
			}()
			continue
		}
		// V8 ⑤ 守护派发：已降级的订阅直接跳过，绝不再让发布方为它等一次超时。
		if e.deg != nil && e.deg.Load() {
			continue
		}
		// Go 1.21 的循环变量在迭代间复用，闭包必须显式拷贝，否则 goroutine 拿到的
		// 可能是下一轮的订阅者——那会变成「派给错的处理器」，比超时更难查。
		entry := e
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = recover() }()
			entry.h(topic, payload)
		}()
		select {
		case <-done:
		case <-time.After(entry.timeout):
			if entry.deg != nil && entry.deg.CompareAndSwap(false, true) {
				n := atomic.AddInt64(&b.degraded, 1)
				log.Printf("eventbus: 订阅者处理 %q 超过 %s，标记 degraded 并暂停投递（累计 %d 次；恢复需显式调用 RecoverDegraded）",
					topic, entry.timeout, n)
			}
		}
	}
}
