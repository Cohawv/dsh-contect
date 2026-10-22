package plugin

import (
	"sort"
	"sync"
)

// registry 保存所有通过 Register 在编译期登记的插件。
// 这是「编译期注册」模式：插件在 var 初始化或 init() 中调用 Register，
// 运行时由 Manager 通过 All() 取出并装配。
var (
	regMu    sync.Mutex
	registry []Plugin
)

// Register 登记一个插件。重名（Name 相同）直接 panic——这是编程错误，
// 在启动前就暴露比运行时诡异行为更安全。
func Register(p Plugin) {
	regMu.Lock()
	defer regMu.Unlock()
	name := p.Meta().Name
	for _, e := range registry {
		if e.Meta().Name == name {
			panic("plugin: duplicate registration: " + name)
		}
	}
	registry = append(registry, p)
}

// All 返回所有已注册插件的按 Name 升序排列的副本（确定性顺序，便于断言与稳定展示）。
func All() []Plugin {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Plugin, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Meta().Name < out[j].Meta().Name
	})
	return out
}

// resetRegistry 清空全局注册表，仅供内部单测使用（避免跨用例污染）。
func resetRegistry() {
	regMu.Lock()
	defer regMu.Unlock()
	registry = nil
}
