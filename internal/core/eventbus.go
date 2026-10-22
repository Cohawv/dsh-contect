// Package core 是业务内核：注册发现、路由、房间、上下线。
//
// 硬性约束：本包只允许 import internal/protocol。
// 业务层不感知传输（TCP/WebSocket）与序列化细节，对外下发一律通过 Session 接口，
// 由 adapter 提供实现（接口倒置）。依赖方向由 scripts/check-deps.sh 强制检查。
package core

import (
	"sync"
	"sync/atomic"
)

// Handler 是事件处理器；payload 的具体类型由 topic 决定。
type Handler func(topic string, payload any)

// subEntry 是订阅记录：每个订阅拥有唯一 id，便于精确注销（cancel）。
// async 为 true 时走「每 topic 独立 goroutine + 有界队列」派发，绝不阻塞发布方。
type subEntry struct {
	id     int
	h      Handler
	async  bool
	ch     chan asyncJob
	done   chan struct{}
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
	mu      sync.RWMutex
	subs    map[string][]subEntry
	nextID  int
	dropped int64
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
		func() {
			defer func() { _ = recover() }()
			e.h(topic, payload)
		}()
	}
}
