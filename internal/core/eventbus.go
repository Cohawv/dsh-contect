// Package core 是业务内核：注册发现、路由、房间、上下线。
//
// 硬性约束：本包只允许 import internal/protocol。
// 业务层不感知传输（TCP/WebSocket）与序列化细节，对外下发一律通过 Session 接口，
// 由 adapter 提供实现（接口倒置）。依赖方向由 scripts/check-deps.sh 强制检查。
package core

import "sync"

// Handler 是事件处理器；payload 的具體类型由 topic 决定。
type Handler func(topic string, payload any)

// EventBus 是极简进程内事件总线：topic → 处理器列表。
// 用于「通知型」跨模块通信（如上下线），让发布方不必知道谁在消费。
type EventBus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
}

func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[string][]Handler)}
}

func (b *EventBus) Subscribe(topic string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], h)
}

// Publish 同步派发；单个处理器 panic 不影响其它订阅者。
func (b *EventBus) Publish(topic string, payload any) {
	b.mu.RLock()
	handlers := make([]Handler, len(b.subs[topic]))
	copy(handlers, b.subs[topic])
	b.mu.RUnlock()
	for _, h := range handlers {
		func() {
			defer func() { _ = recover() }()
			h(topic, payload)
		}()
	}
}
