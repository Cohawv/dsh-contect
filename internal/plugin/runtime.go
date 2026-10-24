package plugin

import (
	"runtime"
	"sync/atomic"
	"time"
)

// statsSnap 缓存最近一次运行时快照（值类型 RuntimeStats，零指针共享）。
var statsSnap atomic.Value

// CachedRuntimeStats 返回**缓存**的运行时快照，读取路径零 STW。
//
// 为什么必须有这一层：runtime.ReadMemStats 会 stop-the-world。
// 在 600 成员房间扇出（约 18 万投递/秒）下，一次几毫秒的 STW 就足以把
// 每连接 256 深的发送队列灌满并开始丢消息——**抓一次 /metrics 就让被测系统变差**
// （实测：同一场景带抓取丢弃 1360 条，不抓取只丢 42 条）。
// 观测端点绝不能改变被观测系统的行为，因此把「读」与「取」拆开：
// 后台按固定节奏刷新（见 RefreshRuntimeStats），抓取端只读缓存。
//
// 首次调用且尚无缓存时退化为一次刷新（保证永远返回真实数字，而不是 1970 年的零值）。
func CachedRuntimeStats() RuntimeStats {
	if v, ok := statsSnap.Load().(RuntimeStats); ok && v.TS != 0 {
		return v
	}
	return RefreshRuntimeStats()
}

// RefreshRuntimeStats 重新读取 runtime.MemStats 并更新缓存（只应由后台采样协程调用）。
func RefreshRuntimeStats() RuntimeStats {
	st := ReadRuntimeStats()
	statsSnap.Store(st)
	return st
}

// RuntimeStats 是进程运行时资源快照（V8 ④ 内存治理）。
//
// 为什么放在框架层：它是**跨插件共享的基础设施值**，不属于任何具体插件的业务。
// pprof 插件负责提供（带内存阈值状态），metrics 插件负责导出（Prometheus 文本），
// 两边必须是**同一份数据**——否则「控制台曲线」与 `/metrics` 数字会各说各话，
// 而这正是审查里最容易被忽略的一类不一致（口径分歧比数字难看更糟）。
//
// 类型放在这里而不是各自声明，是为了让共享 KV 里的断言是**强类型**的：
// 若由两个插件各自声明同名结构，断言必然失败且失败方式是「数字静默变成 0」。
type RuntimeStats struct {
	Goroutines      int    // 当前 goroutine 数（泄漏的第一信号）
	HeapAllocBytes  uint64 // 存活堆对象占用（内存治理的核心观测量）
	HeapInuseBytes  uint64 // 已向 OS 申请且在用（含未回收的 span）
	HeapObjects     uint64 // 存活对象数
	StackInuseBytes uint64 // 栈占用
	SysBytes        uint64 // 从 OS 申请的总内存
	NumGC           uint32 // 累计 GC 次数
	GCPauseTotalNs  uint64 // 累计 GC 停顿
	NextGCBytes     uint64 // 下一次 GC 的目标堆大小
	// 以下三项由 pprof 插件提供（阈值来自其配置）；无提供者时为 0。
	MemLimitBytes uint64 // 告警阈值（0 = 未配置）
	MemHigh       bool   // 当前是否处于超阈值状态
	MemHighTotal  int64  // 累计进入超阈值的次数（带迟滞，不抖动）
	TS            int64  // 采样时刻（毫秒）
}

// RuntimeStatsKey 是跨插件共享 KV 中的键（值是 RuntimeStatsProvider）。
//
// 刻意用常量而非各插件写字面量：这是唯一的跨插件契约点，
// 拼错一个字母的症状是「指标永远是 0」，排查成本极高。
const RuntimeStatsKey = "runtime.stats"

// RuntimeStatsProvider 是运行时快照的提供者签名（pprof 插件实现）。
type RuntimeStatsProvider func() RuntimeStats

// ReadRuntimeStats 直接读取 runtime.MemStats（**会 STW**），是快照的原始来源。
//
// 除缓存刷新外的任何路径都不应直接调用它：观测端点用 CachedRuntimeStats。
func ReadRuntimeStats() RuntimeStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return RuntimeStats{
		Goroutines:      runtime.NumGoroutine(),
		HeapAllocBytes:  ms.HeapAlloc,
		HeapInuseBytes:  ms.HeapInuse,
		HeapObjects:     ms.HeapObjects,
		StackInuseBytes: ms.StackInuse,
		SysBytes:        ms.Sys,
		NumGC:           ms.NumGC,
		GCPauseTotalNs:  ms.PauseTotalNs,
		NextGCBytes:     ms.NextGC,
		// TS 由这里统一填：任何读快照的出口都必须带采样时刻，
		// 否则「这条数字是哪一刻的」无从判断（曲线会画出 1970 年）。
		TS: time.Now().UnixMilli(),
	}
}
