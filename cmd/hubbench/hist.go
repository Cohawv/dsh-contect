package main

import (
	"sort"
	"sync"
	"time"
)

// histBounds 是延迟直方图的分桶上界（微秒级分辨率，覆盖 100µs ~ 10s）。
//
// 用**固定分桶**而不是保存全部样本：压测 60 秒、每连接 10 msg/s、100 连接就是
// 6 万条样本，全存会让内存随压测规模线性增长，而且 p99 只需要分桶精度即可。
//
// 边界按约 1.5 倍递增排（毫秒段最密，因为局域网延迟就落在这里）。配上 Quantile
// 的桶内插值，单个分位数的误差约在 ±15% 以内——够用来判断「优化有效/无效」。
// 早先版本用 5ms→10ms→25ms 这种粗档，实测会把 p50 直接顶到桶上界，
// 出现 p50=10ms 却 max=9ms 的自相矛盾数字。
var histBounds = []time.Duration{
	100 * time.Microsecond,
	250 * time.Microsecond,
	500 * time.Microsecond,
	750 * time.Microsecond,
	time.Millisecond,
	1500 * time.Microsecond,
	2500 * time.Microsecond,
	4 * time.Millisecond,
	6 * time.Millisecond,
	8 * time.Millisecond,
	10 * time.Millisecond,
	15 * time.Millisecond,
	25 * time.Millisecond,
	40 * time.Millisecond,
	60 * time.Millisecond,
	100 * time.Millisecond,
	150 * time.Millisecond,
	250 * time.Millisecond,
	400 * time.Millisecond,
	600 * time.Millisecond,
	time.Second,
	1500 * time.Millisecond,
	2500 * time.Millisecond,
	4 * time.Second,
	6 * time.Second,
	10 * time.Second,
}

// Histogram 是并发安全的延迟直方图（固定分桶 + 精确 min/max/mean）。
type Histogram struct {
	mu     sync.Mutex
	counts []int64 // 与 histBounds 等长；最后一个桶是「超过 10s」的溢出桶
	count  int64
	sum    time.Duration
	min    time.Duration
	max    time.Duration
}

func newHistogram() *Histogram {
	return &Histogram{counts: make([]int64, len(histBounds))}
}

// Observe 记录一次观测。负数（时钟回拨等异常）按 0 处理，避免污染分位数。
func (h *Histogram) Observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	idx := sort.Search(len(histBounds), func(i int) bool { return d <= histBounds[i] })
	if idx >= len(histBounds) {
		idx = len(histBounds) - 1 // 溢出桶：> 10s 全归最后一格
	}
	h.mu.Lock()
	h.counts[idx]++
	h.count++
	h.sum += d
	if h.count == 1 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.mu.Unlock()
}

// Merge 把另一个直方图并入（每个连接各持一个，汇总时合并，避免全局锁成为瓶颈）。
func (h *Histogram) Merge(o *Histogram) {
	if o == nil {
		return
	}
	o.mu.Lock()
	counts := append([]int64(nil), o.counts...)
	count, sum, min, max := o.count, o.sum, o.min, o.max
	o.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if count == 0 {
		return
	}
	for i := range counts {
		h.counts[i] += counts[i]
	}
	first := h.count == 0
	h.count += count
	h.sum += sum
	if first || min < h.min {
		h.min = min
	}
	if max > h.max {
		h.max = max
	}
}

// Total 返回观测次数。
func (h *Histogram) Total() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Quantile 返回分位数对应的时间。q 取 0..1；无样本时返回 0。
//
// 在所在桶内按**均匀分布线性插值**，而不是直接返回桶上界。直接返回上界会让
// 分位数永远落在档位刻度上（p50=10.000ms 这种），粗桶时甚至与 max 矛盾，
// 读报告的人会先怀疑工具而不是被测系统。
func (h *Histogram) Quantile(q float64) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return 0
	}
	if q <= 0 {
		return h.min
	}
	if q >= 1 {
		return h.max
	}
	// 目标名次（向上取整），例如 99% of 1000 = 第 990 个样本。
	rank := int64(q*float64(h.count) + 0.999)
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		seen += c
		if seen < rank {
			continue
		}
		lo := time.Duration(0)
		if i > 0 {
			lo = histBounds[i-1]
		}
		if i == len(histBounds)-1 {
			// 溢出桶（> 10s）：桶内分布完全未知，插值只会是编造。返回实测 max，
			// 它至少是个真实测到的数字。
			return h.max
		}
		hi := histBounds[i]
		// 本桶内名次（1..c），取该名次对应区间的中点。
		pos := float64(rank-(seen-c)) - 0.5
		if pos < 0.5 {
			pos = 0.5
		}
		v := lo + time.Duration(float64(hi-lo)*pos/float64(c))
		// 夹到 [min, max]：插值是估算，但绝不能超出实测范围，否则 p99 > max 又回来了。
		if v > h.max {
			v = h.max
		}
		if v < h.min {
			v = h.min
		}
		return v
	}
	return h.max
}

// Stats 是直方图的不可变快照（写进 JSON 报告）。
type Stats struct {
	Count      int64    `json:"count"`
	MinMs      float64  `json:"minMs"`
	MeanMs     float64  `json:"meanMs"`
	P50Ms      float64  `json:"p50Ms"`
	P95Ms      float64  `json:"p95Ms"`
	P99Ms      float64  `json:"p99Ms"`
	MaxMs      float64  `json:"maxMs"`
	HistBucket []Bucket `json:"histogram,omitempty"`
}

// Bucket 是一个直方图分桶（上界 + 次数），供外部工具画图。
type Bucket struct {
	UpperMs float64 `json:"upperMs"`
	Count   int64   `json:"count"`
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Snapshot 生成快照；includeBuckets 为 false 时不输出分桶明细（报告更小）。
func (h *Histogram) Snapshot(includeBuckets bool) Stats {
	h.mu.Lock()
	count, sum, min, max := h.count, h.sum, h.min, h.max
	counts := append([]int64(nil), h.counts...)
	h.mu.Unlock()

	s := Stats{Count: count, MinMs: msOf(min), MaxMs: msOf(max)}
	if count > 0 {
		s.MeanMs = msOf(sum / time.Duration(count))
	}
	s.P50Ms = msOf(h.Quantile(0.50))
	s.P95Ms = msOf(h.Quantile(0.95))
	s.P99Ms = msOf(h.Quantile(0.99))
	if includeBuckets {
		s.HistBucket = make([]Bucket, len(counts))
		for i, c := range counts {
			s.HistBucket[i] = Bucket{UpperMs: msOf(histBounds[i]), Count: c}
		}
	}
	return s
}

// slowBuf 保留最慢的 N 条明细（长尾排查用：分位数只能告诉你「有慢的」，
// 明细才能告诉你「慢成什么样」）。
type slowBuf struct {
	mu   sync.Mutex
	n    int
	rows []SlowSample
}

// SlowSample 是一条长尾样本。
type SlowSample struct {
	Client string  `json:"client"`
	Seq    string  `json:"nonce"`
	Ms     float64 `json:"ms"`
}

func newSlowBuf(n int) *slowBuf { return &slowBuf{n: n} }

func (s *slowBuf) add(client, nonce string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := SlowSample{Client: client, Seq: nonce, Ms: msOf(d)}
	if len(s.rows) < s.n {
		s.rows = append(s.rows, row)
		sort.Slice(s.rows, func(i, j int) bool { return s.rows[i].Ms > s.rows[j].Ms })
		return
	}
	if len(s.rows) > 0 && row.Ms <= s.rows[len(s.rows)-1].Ms {
		return // 不够慢，丢弃（避免为了一条样本做插入排序）
	}
	s.rows[len(s.rows)-1] = row
	sort.Slice(s.rows, func(i, j int) bool { return s.rows[i].Ms > s.rows[j].Ms })
}

func (s *slowBuf) snapshot() []SlowSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) == 0 {
		return nil
	}
	return append([]SlowSample(nil), s.rows...)
}
