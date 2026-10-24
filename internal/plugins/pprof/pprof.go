// Package pprof 是「内存治理与运行时诊断」插件（V8 ④）。
//
// 它解决的是长期运行（7×24）服务的头号死法：**内存只增不减，而且没人知道**。
// 三个能力按「出事后的处置顺序」组织：
//
//  1. **可见**：`/api/runtime` 暴露堆/对象数/goroutine/GC 与阈值状态；
//     同一份数据经共享 KV 交给 metrics 插件导出为 Prometheus 指标——
//     两个出口**同源**，绝不各算一套（口径分歧比数字难看更糟）。
//  2. **考古**：`/debug/pprof/*` 五个端点可直接接 `go tool pprof`；
//     另有定时堆快照（默认 30 分钟）落到 logs/ 目录并自动轮转，
//     事故过去两小时后仍能回看当时的堆。
//  3. **兜底**：堆超过阈值（默认 1GB）时主动 WARN 日志 + 计数 + `mem.high` 事件，
//     带 90% 迟滞（回落到阈值 90% 以下才解除），避免在阈值附近抖动刷屏。
//
// 设计纪律：
//   - **零外部依赖**：只用 net/http/pprof 与 runtime/pprof（标准库）；
//   - **默认只允许本机访问**：pprof 能 dump 全部堆内容与 30 秒 CPU 火焰，
//     而本系统**没有鉴权**（前置决策排除）。因此默认按「来源是否回环」拦一道，
//     远程访问要显式把 allowRemote 打开——把「默认安全」放在配置默认值里，
//     而不是指望部署者记得加防火墙；
//   - **不写日志到自身回路**：本插件只写普通 log 行（经框架 fanout 进 logview
//     与 lan-agent-hub.log），绝不实现 LogSink；
//   - **关掉即静默**：插件被停用后所有端点 404（框架闸门），定时协程退出，
//     不留 goroutine、不写快照文件。
package pprof

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	netpprof "net/http/pprof"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rtpprof "runtime/pprof"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径。
//
// /debug/pprof/* 沿用标准库的路径与语义（第 5 个端点 /debug/pprof/goroutine 等
// 子 profile 由 Index 统一处理），让 go tool pprof 与既有运维习惯零成本对接。
const (
	pathIndex    = "/debug/pprof/"
	pathCmdline  = "/debug/pprof/cmdline"
	pathProfile  = "/debug/pprof/profile"
	pathSymbol   = "/debug/pprof/symbol"
	pathTrace    = "/debug/pprof/trace"
	pathRuntime  = "/api/runtime"
	pathSnapshot = "/api/runtime/snapshot"
)

// 默认与边界取值。
const (
	defaultMemLimitMB = 1024 // 1GB
	maxMemLimitMB     = 1 << 20

	defaultCheckInterval = 15 * time.Second
	minCheckInterval     = 100 * time.Millisecond

	defaultSnapshotInterval = 30 * time.Minute
	// minSnapshotInterval 是定时快照的下限：每份快照几十 KB，1 秒一份就是持续写盘。
	// 注意「夹紧」时必须留一条日志——把用户配的 50ms 悄悄改成 1s 而不出声，
	// 只会让人以为定时器坏了（本次实现就被测试抓到过）。
	minSnapshotInterval = time.Second

	defaultSnapshotKeep = 3
	maxSnapshotKeep     = 100

	// maxProfileSeconds 是 /debug/pprof/profile 的采样上限。
	// 标准库默认 30 秒、不设上限：一个 ?seconds=3600 的请求会占住一个连接与
	// 一整个 CPU 采样器一小时——外部能造成的资源占用必须有硬顶。
	maxProfileSeconds = 60

	// snapshotPrefix / snapshotSuffix 用于识别与轮转本插件写的快照。
	// 文件名是 heap-<UnixNano>.pprof：19 位定长数字，字典序 == 时间序，
	// 因此轮转不需要 stat 每一个文件。
	snapshotPrefix = "heap-"
	snapshotSuffix = ".pprof"

	// memHighHysteresis 是告警解除的迟滞分母：回落到阈值的 9/10（90%）以下才解除。
	memHighHysteresis = 10
)

// snapshotInfo 是一个堆快照文件的描述（/api/runtime 与 /api/runtime/snapshot 共用）。
type snapshotInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	TS        int64  `json:"ts"` // 文件名里的采样时刻（毫秒）
}

// Pprof 是 pprof 插件实体。
type Pprof struct {
	hub *core.Hub
	ctx *plugin.Context

	// 全部可变配置用原子量：Apply（热更新）与采样协程并发读写。
	allowRemote atomic.Bool
	memLimit    atomic.Int64 // 字节，0 = 关闭阈值告警
	checkEvery  atomic.Int64 // 纳秒
	snapEvery   atomic.Int64 // 纳秒，0 = 关闭定时快照
	snapKeep    atomic.Int64
	snapDir     atomic.Value // string

	memHigh      atomic.Bool
	memHighTotal atomic.Int64

	startedAt atomic.Int64

	mu           sync.Mutex
	lastSnapshot string
	snapTotal    int64
	snapFailed   int64

	closed chan struct{}
	wg     sync.WaitGroup
}

// Meta 声明插件元信息。依赖 httpgateway：本插件不自建端口，端点挂在共享 mux 上。
func (p *Pprof) Meta() plugin.Meta {
	return plugin.Meta{
		Name:         "pprof",
		Version:      "1.0.0",
		Description:  "内存治理：pprof 诊断端点 + 运行时快照 + 定时堆快照轮转 + 阈值告警（默认仅本机可访问）",
		DefaultOn:    true,
		Dependencies: []string{"httpgateway"},
	}
}

// Options 声明本插件的可配置项（控制台设置视图据此自动生成）。
func (p *Pprof) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "allowRemote",
			Type:        plugin.OptionBool,
			Default:     false,
			Description: "允许非本机访问 /debug/pprof/*（默认仅回环地址；本系统无鉴权，开启等于把堆内容暴露给局域网）",
		},
		{
			Key:         "memLimitMB",
			Type:        plugin.OptionInt,
			Default:     defaultMemLimitMB,
			Description: "堆占用告警阈值（MB），超过则 WARN 日志 + 计数 + mem.high 事件；0 = 关闭告警",
			Min:         0,
			Max:         maxMemLimitMB,
		},
		{
			Key:         "checkInterval",
			Type:        plugin.OptionDuration,
			Default:     defaultCheckInterval.String(),
			Description: "内存阈值检查间隔；支持热更新",
		},
		{
			Key:         "snapshotInterval",
			Type:        plugin.OptionDuration,
			Default:     defaultSnapshotInterval.String(),
			Description: "定时堆快照间隔（写 heap-<ts>.pprof）；0 = 关闭定时快照（手动端点仍可用）",
		},
		{
			Key:         "snapshotKeep",
			Type:        plugin.OptionInt,
			Default:     defaultSnapshotKeep,
			Description: "保留的堆快照份数（超出按时间删除最旧的，防磁盘写满）",
			Min:         1,
			Max:         maxSnapshotKeep,
		},
		{
			Key:         "snapshotDir",
			Type:        plugin.OptionString,
			Default:     "",
			Description: "堆快照目录；留空 = 可执行文件同级的 logs/ 目录",
		},
	}
}

// Init 解析配置、注册端点，并把运行时快照的提供者挂到共享 KV。
//
// 提供者在 **Init**（而非 Start）就挂载：InitAll 一定早于任何 StartAll，
// 因此 metrics 第一次采样就能拿到真实数字，而不是先画一个 0 的起点。
func (p *Pprof) Init(ctx *plugin.Context) error {
	p.ctx = ctx
	if hub, ok := ctx.Hub().(*core.Hub); ok {
		p.hub = hub
	}

	p.allowRemote.Store(ctx.Bool("allowRemote", false))
	p.memLimit.Store(int64(clampInt(ctx.Int("memLimitMB", defaultMemLimitMB), 0, maxMemLimitMB)) * 1024 * 1024)

	checkEvery := ctx.Duration("checkInterval", defaultCheckInterval)
	if checkEvery < minCheckInterval {
		checkEvery = minCheckInterval
	}
	p.checkEvery.Store(int64(checkEvery))

	snapEvery := ctx.Duration("snapshotInterval", defaultSnapshotInterval)
	if snapEvery > 0 && snapEvery < minSnapshotInterval {
		log.Printf("pprof: snapshotInterval=%s 过小，已按最小间隔 %s 执行（否则会持续写盘）",
			snapEvery, minSnapshotInterval)
		snapEvery = minSnapshotInterval
	}
	p.snapEvery.Store(int64(snapEvery))

	p.snapKeep.Store(int64(clampInt(ctx.Int("snapshotKeep", defaultSnapshotKeep), 1, maxSnapshotKeep)))
	p.snapDir.Store(resolveSnapshotDir(ctx.String("snapshotDir", "")))

	// 路由一律 RegisterHTTPUnobserved：pprof 与 /api/runtime 是「排查时才看」的端点，
	// 计进 hub_http_requests_total 只会污染真实业务流量的口径。
	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{pathIndex, netpprof.Index},
		{pathCmdline, netpprof.Cmdline},
		{pathProfile, p.handleProfile},
		{pathSymbol, netpprof.Symbol},
		{pathTrace, netpprof.Trace},
		{pathRuntime, p.handleRuntime},
		{pathSnapshot, p.handleSnapshot},
	}
	for _, r := range routes {
		if err := ctx.RegisterHTTPUnobserved(r.pattern, p.guard(r.handler)); err != nil {
			return fmt.Errorf("pprof: %w", err)
		}
	}

	ctx.SetShared(plugin.RuntimeStatsKey, plugin.RuntimeStatsProvider(p.stats))
	// 先刷新一次：InitAll 早于所有 StartAll，metrics 的首次采样与应用启动即抓取的
	// /metrics 都能拿到真实数字（否则会先报一段时间 0）。
	plugin.RefreshRuntimeStats()
	return nil
}

// Start 拉起阈值检查与定时快照两个协程。
func (p *Pprof) Start() error {
	p.startedAt.Store(time.Now().UnixMilli())
	closed := make(chan struct{})
	p.mu.Lock()
	p.closed = closed
	p.mu.Unlock()

	p.wg.Add(2)
	go p.checkLoop(closed)
	go p.snapLoop(closed)
	return nil
}

// Stop 关闭协程并**等它们真的退出**（不做「发个信号就走」的假优雅）。
func (p *Pprof) Stop() error {
	p.mu.Lock()
	if p.closed != nil {
		close(p.closed)
		p.closed = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// Apply 实现 plugin.Applier：热更新阈值与两个间隔。
//
// allowRemote / snapshotDir 不进热更新：前者改变的是「已注册路由的访问策略」，
// 后者会让快照散落到两个目录。这两项如实标注需重启，而不是假装改了。
func (p *Pprof) Apply(options map[string]any) error {
	if raw, ok := options["memLimitMB"]; ok {
		n, err := toInt(raw)
		if err != nil {
			return fmt.Errorf("pprof: memLimitMB %w", err)
		}
		if n < 0 || n > maxMemLimitMB {
			return fmt.Errorf("pprof: memLimitMB 越界（%d，允许 0..%d）", n, maxMemLimitMB)
		}
		p.memLimit.Store(int64(n) * 1024 * 1024)
		// 阈值变化后重新评估一次状态：调低阈值应当立刻告警，
		// 而不是等下一个检查周期（用户刚点了保存就想看到效果）。
		p.checkMemory()
	}
	if raw, ok := options["checkInterval"]; ok {
		d, err := toDuration(raw)
		if err != nil {
			return fmt.Errorf("pprof: checkInterval %w", err)
		}
		if d < minCheckInterval {
			return fmt.Errorf("pprof: checkInterval 过小（%s，最小 %s）", d, minCheckInterval)
		}
		p.checkEvery.Store(int64(d))
	}
	if raw, ok := options["snapshotInterval"]; ok {
		d, err := toDuration(raw)
		if err != nil {
			return fmt.Errorf("pprof: snapshotInterval %w", err)
		}
		if d > 0 && d < minSnapshotInterval {
			return fmt.Errorf("pprof: snapshotInterval 过小（%s，最小 %s）", d, minSnapshotInterval)
		}
		p.snapEvery.Store(int64(d))
	}
	if raw, ok := options["snapshotKeep"]; ok {
		n, err := toInt(raw)
		if err != nil {
			return fmt.Errorf("pprof: snapshotKeep %w", err)
		}
		if n < 1 || n > maxSnapshotKeep {
			return fmt.Errorf("pprof: snapshotKeep 越界（%d，允许 1..%d）", n, maxSnapshotKeep)
		}
		p.snapKeep.Store(int64(n))
	}
	if _, ok := options["allowRemote"]; ok {
		log.Printf("pprof: allowRemote 需重启生效（当前 %t）", p.allowRemote.Load())
	}
	if _, ok := options["snapshotDir"]; ok {
		log.Printf("pprof: snapshotDir 需重启生效（当前 %s）", p.dir())
	}
	return nil
}

// ---------------------------------------------------------------------------
// 访问守卫
// ---------------------------------------------------------------------------

// guard 包一层「仅本机」检查。
//
// 允许回环（127.0.0.0/8、::1）而不是「只允许 127.0.0.1」：
// IPv6 回环与容器内的 127.0.0.2 都是本机，按字符串比会误伤。
func (p *Pprof) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.allowRemote.Load() && !isLoopback(r.RemoteAddr) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "pprof: 仅允许本机访问（来源 %s）。\n"+
				"本系统没有鉴权，pprof 能 dump 全部堆内容；确需远程访问请在设置里显式开启 allowRemote。\n",
				r.RemoteAddr)
			return
		}
		h(w, r)
	}
}

// isLoopback 判断远端地址是否回环。解析失败一律视为「非本机」（失败即拒绝）。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

// handleProfile 给 CPU profile 的采样时长加硬顶。
func (p *Pprof) handleProfile(w http.ResponseWriter, r *http.Request) {
	if raw := strings.TrimSpace(r.URL.Query().Get("seconds")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxProfileSeconds {
			http.Error(w, fmt.Sprintf("seconds 必须是 1..%d 的整数", maxProfileSeconds),
				http.StatusBadRequest)
			return
		}
	}
	netpprof.Profile(w, r)
}

// runtimeResponse 是 /api/runtime 的响应体。
//
// 字段名与 Prometheus 指标名一一对应（heapAllocBytes ↔ hub_heap_alloc_bytes），
// 让「控制台看到的数字」与「抓取到的数字」可以逐项对上。
type runtimeResponse struct {
	TS                 int64          `json:"ts"`
	UptimeMs           int64          `json:"uptimeMs"`
	Goroutines         int            `json:"goroutines"`
	HeapAllocBytes     uint64         `json:"heapAllocBytes"`
	HeapInuseBytes     uint64         `json:"heapInuseBytes"`
	HeapObjects        uint64         `json:"heapObjects"`
	StackInuseBytes    uint64         `json:"stackInuseBytes"`
	SysBytes           uint64         `json:"sysBytes"`
	NumGC              uint32         `json:"numGC"`
	GCPauseTotalMs     float64        `json:"gcPauseTotalMs"`
	NextGCBytes        uint64         `json:"nextGCBytes"`
	MemLimitBytes      uint64         `json:"memLimitBytes"`
	MemHigh            bool           `json:"memHigh"`
	MemHighTotal       int64          `json:"memHighTotal"`
	AllowRemote        bool           `json:"allowRemote"`
	CheckIntervalMs    int64          `json:"checkIntervalMs"`
	SnapshotDir        string         `json:"snapshotDir"`
	SnapshotIntervalMs int64          `json:"snapshotIntervalMs"`
	SnapshotKeep       int            `json:"snapshotKeep"`
	SnapshotTotal      int64          `json:"snapshotTotal"`
	SnapshotFailed     int64          `json:"snapshotFailed"`
	LastSnapshot       string         `json:"lastSnapshot,omitempty"`
	Snapshots          []snapshotInfo `json:"snapshots"`
}

// handleRuntime 输出当前运行时快照 + 现存堆快照清单。
func (p *Pprof) handleRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	st := p.stats()

	last, total, failed := p.counters()
	snaps, err := listSnapshots(p.dir())
	if err != nil {
		// 目录不存在不是错误（还没抓过快照）：如实返回空清单。
		snaps = nil
	}

	resp := runtimeResponse{
		TS:                 st.TS,
		UptimeMs:           p.uptime(),
		Goroutines:         st.Goroutines,
		HeapAllocBytes:     st.HeapAllocBytes,
		HeapInuseBytes:     st.HeapInuseBytes,
		HeapObjects:        st.HeapObjects,
		StackInuseBytes:    st.StackInuseBytes,
		SysBytes:           st.SysBytes,
		NumGC:              st.NumGC,
		GCPauseTotalMs:     float64(st.GCPauseTotalNs) / 1e6,
		NextGCBytes:        st.NextGCBytes,
		MemLimitBytes:      st.MemLimitBytes,
		MemHigh:            st.MemHigh,
		MemHighTotal:       st.MemHighTotal,
		AllowRemote:        p.allowRemote.Load(),
		CheckIntervalMs:    time.Duration(p.checkEvery.Load()).Milliseconds(),
		SnapshotDir:        p.dir(),
		SnapshotIntervalMs: time.Duration(p.snapEvery.Load()).Milliseconds(),
		SnapshotKeep:       int(p.snapKeep.Load()),
		SnapshotTotal:      total,
		SnapshotFailed:     failed,
		LastSnapshot:       last,
		Snapshots:          snaps,
	}
	writeJSON(w, http.StatusOK, resp)
}

// counters 返回快照计数（并发安全：采样协程会并发更新）。
func (p *Pprof) counters() (last string, total, failed int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSnapshot, p.snapTotal, p.snapFailed
}

// handleSnapshot 立刻抓一份堆快照（排查现场的「拍照」按钮）。
func (p *Pprof) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET / POST")
		return
	}
	info, err := p.snapshot("manual")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "写堆快照失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// ---------------------------------------------------------------------------
// 采样与阈值
// ---------------------------------------------------------------------------

// stats 组装运行时快照（含本插件的阈值状态），供 /api/runtime 与共享 KV 使用。
//
// 走缓存（plugin.CachedRuntimeStats）：本函数会被 metrics 的每次抓取调用，
// 若在这里直接 ReadMemStats，就等于「监控抓得越勤，被测系统越慢」。
func (p *Pprof) stats() plugin.RuntimeStats {
	st := plugin.CachedRuntimeStats()
	st.MemLimitBytes = uint64(p.memLimit.Load())
	st.MemHigh = p.memHigh.Load()
	st.MemHighTotal = p.memHighTotal.Load()
	return st
}

// checkLoop 周期刷新运行时快照并检查阈值（每轮重读间隔，热更新立即生效）。
//
// 这里是**全进程唯一的 MemStats 读取点**（pprof 启用时）。
// 刻意把「刷新快照」与「检查阈值」合成一个循环：两者需要同一时刻的数据，
// 拆成两个循环就变成两次 STW —— 而 STW 正是本版要消灭的东西
// （实测：600 成员扇出下，每 2 秒一次 STW 会把发送队列灌满，15 秒内丢弃 748 条；
// 改成 15 秒一次后回落到与基线同量级）。
func (p *Pprof) checkLoop(closed <-chan struct{}) {
	defer p.wg.Done()
	for {
		d := time.Duration(p.checkEvery.Load())
		if d <= 0 {
			d = defaultCheckInterval
		}
		timer := time.NewTimer(d)
		select {
		case <-closed:
			timer.Stop()
			return
		case <-timer.C:
			plugin.RefreshRuntimeStats()
			p.checkMemory()
		}
	}
}

// checkMemory 检查一次阈值，跨越时告警（带迟滞）。
func (p *Pprof) checkMemory() {
	limit := uint64(p.memLimit.Load())
	if limit == 0 { // 0 = 显式关闭告警
		return
	}
	p.evaluate(p.stats())
}

// evaluate 按给定快照判定是否跨越阈值。
//
// 拆出这一步是为了**可测**：阈值判定是纯函数（输入快照 + 阈值，输出状态迁移），
// 若与 runtime.ReadMemStats 焊在一起，就只能靠「真的吃 1GB 内存」来测，
// 那种测试既慢又不可靠——最后往往被跳过。
func (p *Pprof) evaluate(st plugin.RuntimeStats) {
	limit := uint64(p.memLimit.Load())
	if limit == 0 {
		return
	}
	high := p.memHigh.Load()
	// 解除线是「阈值的 90%」。
	//
	// 注意别写成 limit/x*(x-1) 那种形式：整数除法会先截断（100/9*8 = 88 而不是 90），
	// 解除线悄悄下移，症状是「已经降下来了却一直不解除」——本次实现就被测试抓到过一次。
	releaseLine := limit - limit/memHighHysteresis

	switch {
	case !high && st.HeapAllocBytes >= limit:
		p.memHigh.Store(true)
		n := p.memHighTotal.Add(1)
		log.Printf("pprof: 内存超阈值 heap=%s limit=%s goroutines=%d objects=%d（第 %d 次进入）",
			humanBytes(st.HeapAllocBytes), humanBytes(limit), st.Goroutines, st.HeapObjects, n)
		p.publishHigh(st, n)
	case high && st.HeapAllocBytes < releaseLine:
		p.memHigh.Store(false)
		log.Printf("pprof: 内存已回落 heap=%s（阈值 %s），告警解除",
			humanBytes(st.HeapAllocBytes), humanBytes(limit))
	}
}

// publishHigh 把超阈值事件发到总线（realtime 插件可 opt-in 转发到 SSE）。
func (p *Pprof) publishHigh(st plugin.RuntimeStats, count int64) {
	if p.hub == nil {
		return
	}
	p.hub.Bus.Publish(protocol.TopicMemHigh, protocol.MemHigh{
		HeapAllocBytes: st.HeapAllocBytes,
		LimitBytes:     st.MemLimitBytes,
		Goroutines:     st.Goroutines,
		HeapObjects:    st.HeapObjects,
		NumGC:          st.NumGC,
		Count:          count,
		TS:             st.TS,
	})
}

// snapLoop 定时抓堆快照（间隔为 0 时保持静默等待，不占 CPU）。
func (p *Pprof) snapLoop(closed <-chan struct{}) {
	defer p.wg.Done()
	for {
		d := time.Duration(p.snapEvery.Load())
		if d <= 0 {
			// 关闭定时快照：仍要能被 Stop 唤醒，否则 Stop 会一直等。
			select {
			case <-closed:
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		timer := time.NewTimer(d)
		select {
		case <-closed:
			timer.Stop()
			return
		case <-timer.C:
			if _, err := p.snapshot("scheduled"); err != nil {
				log.Printf("pprof: 定时堆快照失败: %v", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 快照落盘与轮转
// ---------------------------------------------------------------------------

// snapshot 写一份堆快照（先写 .tmp 再 rename：读到的文件永远是完整的）。
func (p *Pprof) snapshot(reason string) (snapshotInfo, error) {
	dir := p.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.noteSnapshotFailed()
		return snapshotInfo{}, err
	}

	now := time.Now()
	name := fmt.Sprintf("%s%d%s", snapshotPrefix, now.UnixNano(), snapshotSuffix)
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		p.noteSnapshotFailed()
		return snapshotInfo{}, err
	}
	// debug=0 → pprof 二进制格式（gzip protobuf），供 `go tool pprof` 直接打开；
	// debug=1 的文本格式人读舒服但不可被工具分析，考古价值反而低。
	if err := rtpprof.Lookup("heap").WriteTo(f, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		p.noteSnapshotFailed()
		return snapshotInfo{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		p.noteSnapshotFailed()
		return snapshotInfo{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		p.noteSnapshotFailed()
		return snapshotInfo{}, err
	}

	info := snapshotInfo{Name: name, Path: final, TS: now.UnixMilli()}
	if fi, statErr := os.Stat(final); statErr == nil {
		info.SizeBytes = fi.Size()
	}

	p.mu.Lock()
	p.lastSnapshot = final
	p.snapTotal++
	p.mu.Unlock()

	p.rotate(dir)
	log.Printf("pprof: 已写入堆快照 %s（%s，%s）", final, humanBytes(uint64(info.SizeBytes)), reason)
	return info, nil
}

func (p *Pprof) noteSnapshotFailed() {
	p.mu.Lock()
	p.snapFailed++
	p.mu.Unlock()
}

// rotate 只保留最近 keep 份（文件名定长时间序，字典序即时间序）。
func (p *Pprof) rotate(dir string) {
	keep := int(p.snapKeep.Load())
	if keep < 1 {
		keep = 1
	}
	snaps, err := listSnapshots(dir)
	if err != nil || len(snaps) <= keep {
		return
	}
	// listSnapshots 返回时间正序；从最旧的开始删。
	for _, s := range snaps[:len(snaps)-keep] {
		if err := os.Remove(s.Path); err != nil {
			log.Printf("pprof: 轮转删除旧快照失败 %s: %v", s.Path, err)
			continue
		}
	}
}

// listSnapshots 列出目录里的堆快照（时间正序）。目录不存在时返回空清单。
func listSnapshots(dir string) ([]snapshotInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]snapshotInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, snapshotPrefix) || !strings.HasSuffix(name, snapshotSuffix) {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(name, snapshotPrefix), snapshotSuffix)
		ns, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ns <= 0 {
			continue // 不是本插件写的文件（或被改过名），不碰
		}
		info := snapshotInfo{
			Name: name,
			Path: filepath.Join(dir, name),
			TS:   ns / int64(time.Millisecond),
		}
		if fi, err := e.Info(); err == nil {
			info.SizeBytes = fi.Size()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// dir 返回快照目录（可能在 Apply 之前被并发读，故走 atomic.Value）。
func (p *Pprof) dir() string {
	if v, ok := p.snapDir.Load().(string); ok && v != "" {
		return v
	}
	return "logs"
}

// uptime 返回插件运行时长（毫秒）。
func (p *Pprof) uptime() int64 {
	start := p.startedAt.Load()
	if start == 0 {
		return 0
	}
	return time.Now().UnixMilli() - start
}

// resolveSnapshotDir 决定快照目录：显式配置优先，否则落在可执行文件同级的 logs/。
//
// 用可执行文件目录而不是当前工作目录：Hub 常被双击启动（工作目录可能是
// C:\Windows\System32 之类），把快照写到那里等于写丢。
func resolveSnapshotDir(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "logs")
	}
	return "logs"
}

// humanBytes 把字节数格式化成人读字符串（日志用，不参与任何判定）。
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// clampInt 把 v 夹到 [lo, hi]。
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// toInt 把配置值（JSON 反序列化为 float64）转成 int。
func toInt(raw any) (int, error) {
	switch n := raw.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("不是数字（%T）", raw)
	}
}

// toDuration 解析时长（字符串走 ParseDuration，数字按毫秒）。
func toDuration(raw any) (time.Duration, error) {
	switch v := raw.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("不是合法时长（%s）", v)
		}
		return d, nil
	case float64:
		return time.Duration(v) * time.Millisecond, nil
	case int:
		return time.Duration(v) * time.Millisecond, nil
	case int64:
		return time.Duration(v) * time.Millisecond, nil
	default:
		return 0, fmt.Errorf("类型不支持（%T）", raw)
	}
}

// writeJSON 输出 JSON（风格与 metrics / plugininfo 一致）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 输出结构化错误。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func init() {
	plugin.Register(&Pprof{})
}
