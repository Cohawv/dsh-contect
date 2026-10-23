// Package store 是「消息持久化与离线回放」插件（V6 ①）。
//
// 技术选型说明（重要偏差，已记入实施记录与版本索引）：
//   设计文档 V6 原计划用 modernc.org/sqlite（纯 Go、无 cgo），但本机环境拉不到该依赖
//   （proxy 502），且引入它会破坏「零外部依赖」铁律。经决策改为**纯标准库实现**——
//   分段追加日志 + 内存索引 + 崩溃安全截断——这反而更严格地守住了零依赖准则。
//
// 存储模型：
//   - messages.log：追加写 JSON 行（每行一条消息），天然 append-only；
//   - 内存索引：recs []record + idx seq→下标，使 Since/Query 为 O(匹配数)；
//   - 崩溃安全：每条消息是完整 JSON 行；若写入中途进程崩溃，重启加载时最后一行若解析
//     失败即视为不完整并丢弃（截断最后一个不完整行），绝不把半截数据当真；
//   - 批量落盘：每 flushEvery 次追加做一次 fsync（默认 1000），避免每条一次 fsync 拖慢
//     热路径；进程正常退出时 Close 兜底全量 fsync。
//
// seq 归属：沿用 V5 决策——seq 仍由 core.Router 分配，Engine 只**记录**已分配的 seq，
// 不自己分配（INTEGER 主键语义由 seq 天然去重保证），完全不动既有契约。
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record 是一条已持久化的消息（存储引擎的内部表示；导出到 JSON 时字段名稳定）。
type Record struct {
	Seq      int64  `json:"seq"`
	TS       int64  `json:"ts"`
	Type     string `json:"type"`
	From     string `json:"from"`
	To       string `json:"to"`
	RoomID   string `json:"roomId,omitempty"`
	Content  string `json:"content"` // 原始 JSON 字符串（Envelope.Content 的原文）
	Delivered int   `json:"delivered"`
}

// Filter 是消息查询条件（全部可选；零值表示不限制）。
type Filter struct {
	From   string // 发送方
	To     string // 接收方（含 "" 表示广播/房间）
	RoomID string // 房间
	Since  int64  // seq 下限（不含）
	Before int64  // ts 上限（不含），毫秒
	Limit  int    // 返回条数上限（0 = 用默认）
}

// DefaultLimit 是 Query 在未指定 limit 时的默认条数。
const DefaultLimit = 100

// maxRecordsHardCap 是内存索引的硬上限（防止极端情况下 OOM）。
// LAN 场景消息量有限；超过此值后最旧的消息从内存索引淘汰（磁盘仍保留，可由 Vacuum 重建）。
const maxRecordsHardCap = 2_000_000

// Engine 是纯标准库的消息存储引擎。
//
// 并发：所有读写都走 mu（Append 持写锁，Query/Since 持读锁）。Append 是消息热路径上的
// 同步调用，因此必须非阻塞且不 panic——本实现只在追加时做一次文件写（fsync 仅每
// flushEvery 次触发一次），锁持有时间极短。
type Engine struct {
	mu       sync.RWMutex
	dir      string
	logPath  string
	file     *os.File
	writer   *bufio.Writer
	recs     []Record
	idx      map[int64]int // seq → recs 下标
	maxSeq   int64
	sinceLastFlush int

	flushEvery     int
	retentionDays  int
}

// Open 打开（或创建）存储目录与日志文件，并重建内存索引。
// dir 不存在时自动创建；日志文件损坏（中间行）时跳过最后不完整行，绝不因存储问题阻塞启动。
func Open(dir string, flushEvery, retentionDays int) (*Engine, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: 目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建目录 %s 失败: %w", dir, err)
	}
	logPath := filepath.Join(dir, "messages.log")
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: 打开日志 %s 失败: %w", logPath, err)
	}

	e := &Engine{
		dir:           dir,
		logPath:       logPath,
		file:          f,
		writer:        bufio.NewWriter(f),
		idx:           make(map[int64]int),
		flushEvery:    flushEvery,
		retentionDays: retentionDays,
	}
	if err := e.load(); err != nil {
		// 加载失败：关闭已打开的文件，向上返回（调用方决定是否降级）。
		_ = f.Close()
		return nil, err
	}
	return e, nil
}

// load 从日志文件重建内存索引。遇到第一个无法解析的行（通常是进程崩溃遗留的半截行）
// 即停止——该行及其后的内容被视为不完整并丢弃（崩溃安全截断）。
func (e *Engine) load() error {
	e.recs = e.recs[:0]
	e.idx = make(map[int64]int)
	e.maxSeq = 0

	f, err := os.Open(e.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 全新：空索引
		}
		return fmt.Errorf("store: 读取日志失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 单行长消息可达数十 KB
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			// 不完整/损坏的尾行：停止加载，丢弃它及其后内容（崩溃安全）。
			break
		}
		if _, dup := e.idx[r.Seq]; dup {
			continue // 理论上不会重复（seq 单调），防御性跳过
		}
		e.idx[r.Seq] = len(e.recs)
		e.recs = append(e.recs, r)
		if r.Seq > e.maxSeq {
			e.maxSeq = r.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("store: 扫描日志失败: %w", err)
	}
	return nil
}

// Append 持久化一条消息（来自 core.Router 已分配 seq 后的信封）。非阻塞语义由调用方保证。
func (e *Engine) Append(r Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: 序列化消息失败: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, err := e.writer.Write(data); err != nil {
		return fmt.Errorf("store: 写日志失败: %w", err)
	}
	if err := e.writer.WriteByte('\n'); err != nil {
		return fmt.Errorf("store: 写日志换行失败: %w", err)
	}
	e.sinceLastFlush++
	needFlush := e.sinceLastFlush >= e.flushEvery || e.flushEvery <= 0
	if needFlush {
		if err := e.writer.Flush(); err != nil {
			return fmt.Errorf("store: 刷新日志失败: %w", err)
		}
		if err := e.file.Sync(); err != nil {
			return fmt.Errorf("store: fsync 日志失败: %w", err)
		}
		e.sinceLastFlush = 0
	}

	// 更新内存索引。
	if _, dup := e.idx[r.Seq]; !dup {
		e.idx[r.Seq] = len(e.recs)
		e.recs = append(e.recs, r)
		if r.Seq > e.maxSeq {
			e.maxSeq = r.Seq
		}
	}
	// 内存索引硬上限：超出后淘汰最旧（磁盘仍保留，可由 Vacuum 重建）。
	if len(e.recs) > maxRecordsHardCap {
		drop := len(e.recs) - maxRecordsHardCap
		e.recs = e.recs[drop:]
		e.reindexLocked()
	}
	return nil
}

// reindexLocked 在 e.recs 上重建 idx（调用方持写锁）。
func (e *Engine) reindexLocked() {
	e.idx = make(map[int64]int, len(e.recs))
	for i, r := range e.recs {
		e.idx[r.Seq] = i
	}
}

// LastSeq 返回已持久化的最大 seq（无消息为 0）。
func (e *Engine) LastSeq() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.maxSeq
}

// Since 返回 seq > since 的消息（按 seq 升序），最多 limit 条。用于离线回放与续传。
func (e *Engine) Since(since, limit int64) []Record {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sinceLocked(since, limit)
}

// sinceLocked 在持读锁前提下返回 seq > since 的消息（升序），最多 limit 条。
func (e *Engine) sinceLocked(since, limit int64) []Record {
	if limit <= 0 {
		limit = DefaultLimit
	}
	out := make([]Record, 0, limit)
	for i := 0; i < len(e.recs) && int64(len(out)) < limit; i++ {
		r := e.recs[i]
		if r.Seq > since {
			out = append(out, r)
		}
	}
	return out
}

// Query 按条件查询消息（升序），最多 limit 条。
func (e *Engine) Query(f Filter) []Record {
	e.mu.RLock()
	defer e.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	out := make([]Record, 0, limit)
	for _, r := range e.recs {
		if f.Since > 0 && r.Seq <= f.Since {
			continue
		}
		if f.From != "" && r.From != f.From {
			continue
		}
		if f.To != "" && r.To != f.To {
			continue
		}
		if f.RoomID != "" && r.RoomID != f.RoomID {
			continue
		}
		if f.Before > 0 && r.TS >= f.Before {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ExportJSONL 把全部消息（升序）以 JSONL 写到 w（供 grep/awk 排查，与设计小建议一致）。
func (e *Engine) ExportJSONL(w interface{ Write([]byte) (int, error) }) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	enc := json.NewEncoder(w)
	for _, r := range e.recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// VacuumResult 是 Vacuum 的返回。
type VacuumResult struct {
	MessageCount int   `json:"messageCount"`
	FileSize     int64 `json:"fileSize"`
	Pruned       int   `json:"pruned"`
	VacuumedAt   int64 `json:"vacuumedAt"`
}

// Vacuum 压缩存储：若设置了 retentionDays，则先按时间淘汰过期消息（只增不改既有契约——
// 删除的是过期数据，且先留在磁盘、仅内存索引移除，真正丢弃发生在重写文件时）；
// 随后以内存索引为真相重写日志文件（消除空洞/半截行），并 fsync。
//
// 注意：Vacuum 不是清理「历史」的常规手段，而是磁盘维护操作；默认 retentionDays=0 时
// 只做文件重写与 fsync，不丢弃任何消息。
func (e *Engine) Vacuum() (VacuumResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pruned := 0
	if e.retentionDays > 0 {
		cutoff := time.Now().UnixMilli() - int64(e.retentionDays)*24*60*60*1000
		kept := e.recs[:0]
		for _, r := range e.recs {
			if r.TS < cutoff {
				pruned++
				continue
			}
			kept = append(kept, r)
		}
		e.recs = kept
		e.reindexLocked()
		// 重新计算 maxSeq（保留消息中的最大值）。
		e.maxSeq = 0
		for _, r := range e.recs {
			if r.Seq > e.maxSeq {
				e.maxSeq = r.Seq
			}
		}
	}

	// 以内存索引重写日志文件（临时文件 + 原子替换，避免写到一半被再次打断导致全损）。
	tmp := e.logPath + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return VacuumResult{}, fmt.Errorf("store: 创建临时文件失败: %w", err)
	}
	w := bufio.NewWriter(tf)
	enc := json.NewEncoder(w)
	for _, r := range e.recs {
		if err := enc.Encode(r); err != nil {
			_ = tf.Close()
			return VacuumResult{}, fmt.Errorf("store: 重写日志失败: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tf.Close()
		return VacuumResult{}, err
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		return VacuumResult{}, err
	}
	if err := tf.Close(); err != nil {
		return VacuumResult{}, err
	}
	// 原子替换：先关旧文件再 rename，Windows 上需先关闭被替换的文件句柄。
	_ = e.file.Close()
	if err := os.Rename(tmp, e.logPath); err != nil {
		// 替换失败：尝试回退（重新打开原文件），不丢失已写数据。
		_, _ = os.OpenFile(e.logPath, os.O_RDWR|os.O_APPEND, 0o644)
		return VacuumResult{}, fmt.Errorf("store: 替换日志失败: %w", err)
	}
	// 重新打开新文件以追加。
	f, err := os.OpenFile(e.logPath, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return VacuumResult{}, fmt.Errorf("store: 重新打开日志失败: %w", err)
	}
	e.file = f
	e.writer = bufio.NewWriter(f)
	e.sinceLastFlush = 0

	fi, _ := f.Stat()
	return VacuumResult{
		MessageCount: len(e.recs),
		FileSize:     fi.Size(),
		Pruned:       pruned,
		VacuumedAt:   time.Now().UnixMilli(),
	}, nil
}

// Close 刷盘并关闭日志文件（进程退出前调用，确保数据落盘）。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writer != nil {
		if err := e.writer.Flush(); err != nil {
			return err
		}
	}
	if e.file != nil {
		if err := e.file.Sync(); err != nil {
			return err
		}
		return e.file.Close()
	}
	return nil
}

// RebuildOnDiskSize 返回当前日志文件字节数（供 Vacuum 结果之外的诊断）。
func (e *Engine) RebuildOnDiskSize() int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.file == nil {
		return 0
	}
	if fi, err := e.file.Stat(); err == nil {
		return fi.Size()
	}
	return 0
}

// seqsSorted 辅助：返回已持久化 seq 的升序切片（单测/诊断用）。
func (e *Engine) seqsSorted() []int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]int64, 0, len(e.recs))
	for _, r := range e.recs {
		out = append(out, r.Seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
