// Package file 实现「Agent 间文件传输」插件（V6 ⑦）。
//
// 解决 LAN 场景下 Agent 之间传文件（图片、日志、配置片段）没有通道的问题：
// 上传方 POST 二进制拿到 fileId，把 fileId 写进 chat 消息；接收方按 fileId 下载。
// 纯标准库实现（crypto/sha256 + encoding/json + net/http + os + path/filepath），零外部依赖。
//
// 端点：
//
//	POST   /api/file?name=x.png             上传（body=二进制）；分片时带 chunk=N&chunks=M
//	GET    /api/files                       列表（按 createdAt 升序）
//	GET    /api/file/{fileId}               下载（Content-Type/Content-Disposition 按扩展名）
//	DELETE /api/file/{fileId}               删除（refs>0 → 409）
//	POST   /api/file/{fileId}/ref           refs+1（发送方发消息前引用，防误删）
//	POST   /api/file/{fileId}/unref         refs-1（下限 0；接收方确认下载后释放）
//
// 引用计数约定（客户端协议，服务端只提供机制）：发送方在发送带 fileId 的 chat
// 消息前先 ref，对方确认下载后可 unref；refs>0 的文件 DELETE 返回 409，
// 避免「消息还在飞，文件已被 TTL/手动清理」。
//
// 存储布局（自持，不塞 store 的消息引擎——store.Record 模型不匹配文件元数据）：
//
//	dataDir/{fileId}/part-00000.bin   分片（上传中，下标零填充 5 位）
//	dataDir/{fileId}/data.bin         合并后的文件内容
//	dataDir/{fileId}/meta.json        元数据（id/name/size/sha256/refs/createdAt/expiresAt/contentType）
//	dataDir/{fileId}/upload.json      分片会话状态（name/chunks/声明 sha/创建时间；崩溃后可恢复续传）
//
// fileId = "f_" + 24 位 hex（复用 protocol.UUID() 的 12 随机字节）。
// 启动时 load() 扫描 dataDir 恢复元数据与会话（对齐 store「重启不丢」理念）。
//
// 与 V6 设计文档 ⑦ 的有意偏离（代码内均有标注）：
//   - TTL 清理为自管循环，不依赖 sweeper 插件：sweeper 可能被停用，文件过期
//     清理若挂在它身上会随停用而静默失效；自管更稳（Start 起 goroutine，Stop 幂等回收）。
//   - 扩展名 → MIME 映射中 .json/.md/.csv/.xml/.yaml 映射为 text/json、
//     text/markdown、text/csv、text/xml、text/yaml（而非 IANA 注册值
//     application/json 等），以保持与默认白名单 "image/*,text/*" 的自洽。
//   - ttlDays=0 表示永不过期（对齐 store.retentionDays 的 0 语义）。
//   - 分片会话默认按 name 关联（文档化流程 chunk=N&chunks=M 不带 fileId），
//     也支持 query 显式带 fileId 恢复/续传（未知 fileId → 404）。
package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lan-agent-hub/internal/plugin"
	"lan-agent-hub/internal/protocol"
)

// 端点路径。
const (
	pathFileUpload = "/api/file"
	pathFileList   = "/api/files"
	pathFilePrefix = "/api/file/"
)

// 客户端声明期望 sha256 的头（上传）；下载响应同样回带该头便于校验。
const hdrFileSHA256 = "X-File-SHA256"

// 目录内文件名。
const (
	metaFileName    = "meta.json"
	dataFileName    = "data.bin"
	uploadFileName  = "upload.json"
	tmpDataName     = "data.tmp"
	partFilePrefix  = "part-"
	partFileSuffix  = ".bin"
	partFilePattern = "part-*.bin"
)

// 配置默认值与边界。
const (
	defaultDataDir             = "data/files"
	defaultMaxSizeBytes        = 64 << 20 // 64MB
	minMaxSizeBytes            = 1 << 10  // 1KB
	maxMaxSizeBytes            = 1 << 30  // 1GB
	defaultTTLDays             = 7
	maxTTLDays                 = 3650
	defaultIncompleteTTLHours  = 24
	maxIncompleteTTLHours      = 8760
	defaultSweepInterval       = time.Minute
	defaultAllowedTypes        = "image/*,text/*"
	maxNameLen                 = 200
	fileIDPrefix               = "f_"
	fileIDHexLen               = 24 // protocol.UUID() 产出 24 位 hex
)

// extMIME 是扩展名 → MIME 映射表（白名单判定的基础）。
// 偏离说明：.json/.md/.csv/.xml/.yaml 映射为 text/* 而非 IANA 注册值
// （application/json、text/markdown 等），以保证默认白名单 "image/*,text/*"
// 的自洽——否则 .json 会被自己的默认配置拒之门外。
var extMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".txt":  "text/plain",
	".json": "text/json",
	".md":   "text/markdown",
	".markdown": "text/markdown",
	".csv":  "text/csv",
	".xml":  "text/xml",
	".yaml": "text/yaml",
	".yml":  "text/yaml",
}

// fileMeta 是已完成文件的元数据（meta.json 的磁盘形态，重启后由此恢复）。
type fileMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Refs        int    `json:"refs"`
	CreatedAt   int64  `json:"createdAt"`   // UTC 毫秒
	ExpiresAt   int64  `json:"expiresAt"`   // UTC 毫秒；0 = 永不过期（ttlDays=0）
	ContentType string `json:"contentType"` // 按 name 扩展名映射
}

// fileView 是 GET /api/files 列表中单个文件的形态（契约字段，不含 contentType）。
type fileView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Refs      int    `json:"refs"`
	ExpiresAt int64  `json:"expiresAt"`
	CreatedAt int64  `json:"createdAt"`
}

// uploadSession 是一条进行中的分片上传（内存态 + upload.json 持久态）。
// seen/received 不落盘：重启时由磁盘上的 part 文件重建（scanParts）。
type uploadSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Declared  string `json:"sha256,omitempty"` // 客户端经 X-File-SHA256 声明的期望值（首片携带，先到优先）
	Chunks    int    `json:"chunks"`           // 分片总数
	CreatedAt int64  `json:"createdAt"`        // UTC 毫秒（首片到达时刻；TTL 自此起算）

	mu       sync.Mutex   `json:"-"` // 串行化同一会话的分片写入与合并
	seen     map[int]bool `json:"-"` // 已收到的分片下标
	received int          `json:"-"` // == len(seen)，内存态计数
}

// opError 是带 HTTP 状态码的内部错误，端点层直接映射（不拖泥带水）。
type opError struct {
	Code int
	Msg  string
}

func (e *opError) Error() string { return e.Msg }

// File 是 file 插件实体。
type File struct {
	dataDir         string
	sweepInterval   time.Duration // 清理循环间隔（Start 时读取；热更新需重启生效）
	now             func() int64  // 可注入（测试）；必须是闭包，见 Init 注释
	maxSizeBytes    int64         // 以下四个支持 Apply 热更新，读写均走 mu
	ttlDays         int
	incompleteTTL   time.Duration
	allowedTypes    []string // 规范化（小写）后的 MIME 模式列表，支持 image/* 通配

	mu       sync.RWMutex
	files    map[string]*fileMeta     // fileId → 元数据（仅已完成文件）
	sessions map[string]*uploadSession // fileId → 进行中的分片上传

	loopStarts int               // 实际启动清理循环的次数（诊断/测试用：双 Start 不得 +1）
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// Meta 声明插件元信息。
func (p *File) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "file",
		Version:     "1.0.0",
		Description: "Agent 间文件传输（纯标准库；零外部依赖）：上传/分片/下载/引用计数防误删/TTL 自清理，重启恢复",
		DefaultOn:   true,
	}
}

// Options 声明配置项。
func (p *File) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "dataDir",
			Type:        plugin.OptionString,
			Default:     defaultDataDir,
			Description: "文件存储目录（相对工作目录或绝对路径）；每个文件一个子目录（修改需重启生效）",
		},
		{
			Key:         "maxSizeBytes",
			Type:        plugin.OptionInt,
			Default:     defaultMaxSizeBytes,
			Min:         minMaxSizeBytes,
			Max:         maxMaxSizeBytes,
			Description: "单文件大小上限（字节）；上传超出返回 413",
		},
		{
			Key:         "allowedTypes",
			Type:        plugin.OptionString,
			Default:     defaultAllowedTypes,
			Description: "允许的内容类型白名单（逗号分隔，支持 image/* 通配）：按 name 扩展名映射 MIME 后匹配，不满足返回 415",
		},
		{
			Key:         "ttlDays",
			Type:        plugin.OptionInt,
			Default:     defaultTTLDays,
			Min:         0,
			Max:         maxTTLDays,
			Description: "文件保留天数（0 = 永不过期）；仅清理 refs=0 且过期的文件；热更新只影响之后新建的文件",
		},
		{
			Key:         "incompleteTTLHours",
			Type:        plugin.OptionInt,
			Default:     defaultIncompleteTTLHours,
			Min:         1,
			Max:         maxIncompleteTTLHours,
			Description: "未完成分片目录的保留小时数（按目录 mtime 判定，活跃上传不会误删）",
		},
		{
			Key:         "sweepInterval",
			Type:        plugin.OptionDuration,
			Default:     "1m",
			Description: "清理循环间隔（时长文本如 30s/1m；修改需重启生效）",
		},
	}
}

// Init 解析配置、恢复磁盘数据、注册端点（不启动清理循环——Start 才启动）。
func (p *File) Init(ctx *plugin.Context) error {
	p.dataDir = ctx.String("dataDir", defaultDataDir)
	p.maxSizeBytes = int64(clampInt(ctx.Int("maxSizeBytes", defaultMaxSizeBytes), minMaxSizeBytes, maxMaxSizeBytes))
	p.ttlDays = clampInt(ctx.Int("ttlDays", defaultTTLDays), 0, maxTTLDays)
	p.incompleteTTL = time.Duration(clampInt(ctx.Int("incompleteTTLHours", defaultIncompleteTTLHours), 1, maxIncompleteTTLHours)) * time.Hour
	p.sweepInterval = ctx.Duration("sweepInterval", defaultSweepInterval)
	if p.sweepInterval <= 0 {
		p.sweepInterval = defaultSweepInterval
	}
	allowed := parseAllowedTypes(ctx.String("allowedTypes", defaultAllowedTypes))
	if len(allowed) == 0 {
		allowed = parseAllowedTypes(defaultAllowedTypes)
	}
	p.allowedTypes = allowed

	// 注意：必须是闭包。`p.now = time.Now().UnixMilli` 是方法值绑定陷阱
	// （scheduler 同款注释），会把 Init 那一刻的 Time 抓死，之后 now() 永远
	// 返回同一时刻——TTL 判定彻底失灵。
	p.now = func() int64 { return time.Now().UnixMilli() }

	p.files = make(map[string]*fileMeta)
	p.sessions = make(map[string]*uploadSession)

	if err := os.MkdirAll(p.dataDir, 0o755); err != nil {
		return fmt.Errorf("file: 创建数据目录 %q 失败: %w", p.dataDir, err)
	}
	if err := p.load(); err != nil {
		return fmt.Errorf("file: 恢复数据失败: %w", err)
	}

	if err := ctx.RegisterHTTP(pathFileUpload, p.handleUpload); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathFileList, p.handleList); err != nil {
		return err
	}
	if err := ctx.RegisterHTTP(pathFilePrefix, p.handleFileOp); err != nil {
		return err
	}
	return nil
}

// Start 启动清理循环。幂等：已在运行时重复调用直接返回。
//
// 为什么必须幂等（scheduler 同款教训）：双 Start 会起两个循环——同一份过期
// 文件被重复清理（无害但浪费），且后一次 Start 覆盖 p.cancel 后，前一个循环
// 的 ctx 再无取消途径，Stop 的 wg.Wait 永久挂住（协程泄漏）。
func (p *File) Start() error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.loopStarts++
	p.mu.Unlock()
	p.wg.Add(1)
	go p.loop(ctx)
	return nil
}

// Stop 停止清理循环（等在飞 sweep 结束，避免停机后半截删除）。幂等。
func (p *File) Stop() error {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
	return nil
}

// Apply 实现 plugin.Applier：热更新 maxSizeBytes / ttlDays / incompleteTTLHours /
// allowedTypes。dataDir 与 sweepInterval 不在热更新范围（需重启生效）。
//
// ttlDays 热更新语义：expiresAt 在文件创建时写死进 meta.json，因此改 ttlDays
// 只影响之后新建的文件；已存文件保留各自 meta.json 里的 expiresAt（明确、可预期）。
func (p *File) Apply(options map[string]any) error {
	if v, ok := options["maxSizeBytes"]; ok {
		n := clampInt(toInt(v, int(p.maxSizeBytes)), minMaxSizeBytes, maxMaxSizeBytes)
		p.mu.Lock()
		p.maxSizeBytes = int64(n)
		p.mu.Unlock()
	}
	if v, ok := options["ttlDays"]; ok {
		n := clampInt(toInt(v, p.ttlDays), 0, maxTTLDays)
		p.mu.Lock()
		p.ttlDays = n
		p.mu.Unlock()
	}
	if v, ok := options["incompleteTTLHours"]; ok {
		n := clampInt(toInt(v, int(p.incompleteTTL.Hours())), 1, maxIncompleteTTLHours)
		p.mu.Lock()
		p.incompleteTTL = time.Duration(n) * time.Hour
		p.mu.Unlock()
	}
	if v, ok := options["allowedTypes"]; ok {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("file: allowedTypes 选项必须是字符串")
		}
		if list := parseAllowedTypes(s); len(list) > 0 {
			p.mu.Lock()
			p.allowedTypes = list
			p.mu.Unlock()
		}
	}
	return nil
}

// ---- 磁盘恢复 ----

// load 扫描 dataDir 恢复元数据与分片会话（Init 调用；重启不丢）。
//
//   - 有 meta.json 且 data.bin 完好 → 已完成文件，入索引（顺带清理崩溃残留的半成品）；
//   - 有 meta.json 但数据缺失/对不上 → 损坏，整目录清除；
//   - 无 meta.json 但有 upload.json → 未完成分片上传，恢复会话（可续传）；
//   - 其余（如裸 part 目录）→ 不动，留给 sweep 按目录 mtime 清理。
func (p *File) load() error {
	entries, err := os.ReadDir(p.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !validFileID(e.Name()) {
			continue // 不碰目录里的异物
		}
		id := e.Name()
		dir := filepath.Join(p.dataDir, id)
		if b, err := os.ReadFile(filepath.Join(dir, metaFileName)); err == nil {
			var m fileMeta
			if json.Unmarshal(b, &m) == nil && m.ID == id {
				if _, err := os.Stat(filepath.Join(dir, dataFileName)); err == nil {
					removeIncompleteArtifacts(dir)
					p.files[id] = &m
					continue
				}
			}
			_ = os.RemoveAll(dir) // 元数据与数据不匹配：视为损坏，整体清除
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, uploadFileName)); err == nil {
			var s uploadSession
			if json.Unmarshal(b, &s) == nil && s.ID == id && s.Chunks >= 1 {
				s.seen = scanParts(dir) // 已收分片以磁盘为准重建
				s.received = len(s.seen)
				p.sessions[id] = &s
			}
		}
	}
	return nil
}

// ---- 清理循环 ----

func (p *File) loop(ctx context.Context) {
	defer p.wg.Done()
	// 启动即扫一遍：停机期间过期的文件/分片在此回收（不必等一个间隔）。
	p.sweep()
	ticker := time.NewTicker(p.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

// sweep 执行一轮清理（抽成独立函数：测试注入 now 后手动触发，无需等真实时钟）。
//
//   - 已完成文件：refs==0 且已过 expiresAt（expiresAt==0 表示永不过期）；
//   - 未完成分片目录：无 meta.json 且目录 mtime 早于 cutoff。
//
// 未完成目录用 mtime 而非会话创建时间判定：分片写入会刷新目录 mtime，活跃上传
// 永远不会被误删；被抛弃的目录（客户端放弃/进程崩溃）mtime 停止刷新，到点即清。
func (p *File) sweep() {
	now := p.now()

	// 1) 已完成文件。
	p.mu.RLock()
	var expired []string
	for id, m := range p.files {
		if m.Refs == 0 && m.ExpiresAt > 0 && now >= m.ExpiresAt {
			expired = append(expired, id)
		}
	}
	p.mu.RUnlock()
	if len(expired) > 0 {
		p.mu.Lock()
		for _, id := range expired {
			m := p.files[id]
			if m == nil || m.Refs > 0 {
				continue // 收集与删除之间可能被 ref——复查，引用中的文件不删
			}
			if err := os.RemoveAll(filepath.Join(p.dataDir, id)); err == nil {
				delete(p.files, id)
			}
		}
		p.mu.Unlock()
	}

	// 2) 未完成分片目录。
	cutoff := now - p.incompleteTTLDuration().Milliseconds()
	entries, err := os.ReadDir(p.dataDir)
	if err != nil {
		return
	}
	var stale []string
	for _, e := range entries {
		if !e.IsDir() || !validFileID(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.dataDir, e.Name(), metaFileName)); err == nil {
			continue // 已完成（或损坏待 load 清理）的目录不属此列
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().UnixMilli() < cutoff {
			stale = append(stale, e.Name())
		}
	}
	if len(stale) == 0 {
		return
	}
	p.mu.Lock()
	for _, id := range stale {
		// 复查：收集与删除之间目录可能刚好合并完成（meta.json 落盘）——不属清理列。
		if _, err := os.Stat(filepath.Join(p.dataDir, id, metaFileName)); err == nil {
			continue
		}
		_ = os.RemoveAll(filepath.Join(p.dataDir, id))
		delete(p.sessions, id)
	}
	p.mu.Unlock()
}

// ---- 端点 ----

// handleUpload 处理 POST /api/file：上传（单次或分片）。
func (p *File) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	name, ok := sanitizeName(q.Get("name"))
	if !ok {
		writeError(w, http.StatusBadRequest, "missing or invalid name")
		return
	}
	contentType := mimeForExt(filepath.Ext(name))
	if contentType == "" || !p.typeAllowed(contentType) {
		shown := filepath.Ext(name)
		if shown == "" {
			shown = name
		}
		writeError(w, http.StatusUnsupportedMediaType, "content type not allowed: "+shown)
		return
	}
	declared := strings.ToLower(strings.TrimSpace(r.Header.Get(hdrFileSHA256)))
	if declared != "" && !isSHA256Hex(declared) {
		writeError(w, http.StatusBadRequest, "invalid "+hdrFileSHA256+" header (want 64 hex chars)")
		return
	}

	chunkStr, chunksStr := q.Get("chunk"), q.Get("chunks")
	if chunkStr == "" && chunksStr == "" {
		p.uploadSingle(w, r, name, contentType, declared)
		return
	}
	if chunkStr == "" || chunksStr == "" {
		writeError(w, http.StatusBadRequest, "chunk and chunks must be provided together")
		return
	}
	chunk, err1 := strconv.Atoi(chunkStr)
	chunks, err2 := strconv.Atoi(chunksStr)
	if err1 != nil || err2 != nil || chunk < 0 || chunks < 1 || chunk >= chunks {
		writeError(w, http.StatusBadRequest, "invalid chunk/chunks parameters")
		return
	}
	p.uploadChunk(w, r, name, contentType, declared, chunk, chunks)
}

// uploadSingle 处理单次上传：落盘 → 校验 → 登记。
func (p *File) uploadSingle(w http.ResponseWriter, r *http.Request, name, contentType, declared string) {
	// 大小双保险之一：声明长度前置拦截（省得把 64MB 读进来再扔）。
	if r.ContentLength > p.maxSize() {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file too large: %d > %d", r.ContentLength, p.maxSize()))
		return
	}
	id := p.newID()
	dir := filepath.Join(p.dataDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "create file dir: "+err.Error())
		return
	}
	tmp := filepath.Join(dir, tmpDataName)
	f, err := os.Create(tmp)
	if err != nil {
		_ = os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "create file: "+err.Error())
		return
	}
	// 大小双保险之二：读时计数（Content-Length 缺失/撒谎时兜底）。
	lr := http.MaxBytesReader(w, r.Body, p.maxSize()+1)
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(f, h), lr)
	closeErr := f.Close()
	if cerr != nil || closeErr != nil || n > p.maxSize() {
		_ = os.RemoveAll(dir)
		if n > p.maxSize() {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file too large: %d > %d", n, p.maxSize()))
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+errString(cerr))
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if declared != "" && sum != declared {
		_ = os.RemoveAll(dir) // 声明值不符：删除该文件全部数据
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("sha256 mismatch: got %s want %s", sum, declared))
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, dataFileName)); err != nil {
		_ = os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "install file: "+err.Error())
		return
	}
	createdAt := p.now()
	m := &fileMeta{
		ID:          id,
		Name:        name,
		Size:        n,
		SHA256:      sum,
		CreatedAt:   createdAt,
		ExpiresAt:   p.expiresAtFor(createdAt),
		ContentType: contentType,
	}
	if err := writeMeta(dir, m); err != nil {
		_ = os.RemoveAll(dir)
		writeError(w, http.StatusInternalServerError, "write meta: "+err.Error())
		return
	}
	p.mu.Lock()
	p.files[id] = m
	p.mu.Unlock()
	writeJSON(w, map[string]any{
		"fileId": id, "name": name, "size": n, "sha256": sum, "complete": true,
	})
}

// uploadChunk 处理分片上传：累积 part 文件，最后一片到齐即合并、校验、登记。
func (p *File) uploadChunk(w http.ResponseWriter, r *http.Request, name, contentType, declared string, chunk, chunks int) {
	sess, code, msg := p.resolveSession(r.URL.Query().Get("fileId"), name, declared, chunks)
	if code != 0 {
		writeError(w, code, msg)
		return
	}
	sess.mu.Lock()
	dir := filepath.Join(p.dataDir, sess.ID)

	// 每片大小预检（声明长度）。
	if r.ContentLength > p.maxSize() {
		limit := p.maxSize()
		sess.mu.Unlock()
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file too large: %d > %d", r.ContentLength, limit))
		return
	}
	part := partPath(dir, chunk)
	f, err := os.Create(part)
	if err != nil {
		sess.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "create chunk file: "+err.Error())
		return
	}
	lr := http.MaxBytesReader(w, r.Body, p.maxSize()+1)
	n, cerr := io.Copy(f, lr)
	closeErr := f.Close()
	if cerr != nil || closeErr != nil || n > p.maxSize() {
		_ = os.Remove(part) // 半片不留
		limit := p.maxSize()
		sess.mu.Unlock()
		if n > p.maxSize() {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file too large: %d > %d", n, limit))
			return
		}
		writeError(w, http.StatusBadRequest, "read chunk body: "+errString(cerr))
		return
	}
	if !sess.seen[chunk] {
		sess.seen[chunk] = true
		sess.received++
	}
	received := sess.received
	complete := received == sess.Chunks

	var size int64
	var sum string
	if complete {
		size, sum, err = p.mergeLocked(sess)
		if err != nil {
			p.discardSession(sess) // 失败不留半成品（含 sha 不符/超大）
			sess.mu.Unlock()
			respondErr(w, err)
			return
		}
		m := &fileMeta{
			ID:          sess.ID,
			Name:        name,
			Size:        size,
			SHA256:      sum,
			CreatedAt:   sess.CreatedAt,
			ExpiresAt:   p.expiresAtFor(sess.CreatedAt),
			ContentType: contentType,
		}
		if err := writeMeta(dir, m); err != nil {
			p.discardSession(sess)
			sess.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "write meta: "+err.Error())
			return
		}
		removeIncompleteArtifacts(dir)
		p.mu.Lock()
		p.files[m.ID] = m
		delete(p.sessions, m.ID)
		p.mu.Unlock()
	}
	id := sess.ID
	sess.mu.Unlock()

	resp := map[string]any{"fileId": id, "name": name, "complete": complete}
	if complete {
		resp["size"] = size
		resp["sha256"] = sum
	} else {
		resp["received"] = received
	}
	writeJSON(w, resp)
}

// resolveSession 解析分片会话：显式 fileId 优先，否则按 name 关联同名未完成
// 会话（V6 ⑦ 文档化流程），都没有则新建。返回 (会话, HTTP 码, 错误信息)，
// HTTP 码为 0 表示成功。
func (p *File) resolveSession(id, name, declared string, chunks int) (*uploadSession, int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != "" {
		s, ok := p.sessions[id]
		if !ok {
			return nil, http.StatusNotFound, "unknown upload session: " + id
		}
		if s.Chunks != chunks {
			return nil, http.StatusBadRequest, fmt.Sprintf(
				"chunks mismatch: session %s has %d, request has %d", id, s.Chunks, chunks)
		}
		return s, 0, ""
	}
	var best *uploadSession
	for _, s := range p.sessions {
		if s.Name != name {
			continue
		}
		if best == nil || s.CreatedAt < best.CreatedAt {
			best = s // 同名多个会话时取最旧的（确定性）
		}
	}
	if best != nil {
		if best.Chunks != chunks {
			return nil, http.StatusBadRequest, fmt.Sprintf(
				"chunks mismatch: session %s has %d, request has %d", best.ID, best.Chunks, chunks)
		}
		return best, 0, ""
	}
	s := &uploadSession{
		ID:        p.newIDLocked(),
		Name:      name,
		Declared:  declared,
		Chunks:    chunks,
		CreatedAt: p.now(),
		seen:      make(map[int]bool),
	}
	dir := filepath.Join(p.dataDir, s.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, http.StatusInternalServerError, "create upload dir: "+err.Error()
	}
	// upload.json 只为「崩溃后恢复会话」服务；写失败不影响本次上传（仅失去恢复能力）。
	_ = writeJSONFile(filepath.Join(dir, uploadFileName), s)
	p.sessions[s.ID] = s
	return s, 0, ""
}

// mergeLocked 按序合并全部分片为 data.bin 并做大小/哈希校验。调用方持 sess.mu。
func (p *File) mergeLocked(sess *uploadSession) (int64, string, error) {
	dir := filepath.Join(p.dataDir, sess.ID)
	tmp := filepath.Join(dir, tmpDataName)
	f, err := os.Create(tmp)
	if err != nil {
		return 0, "", fmt.Errorf("create merged file: %w", err)
	}
	h := sha256.New()
	var total int64
	for i := 0; i < sess.Chunks; i++ {
		rc, err := os.Open(partPath(dir, i))
		if err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return 0, "", fmt.Errorf("open chunk %d: %w", i, err)
		}
		n, err := io.Copy(io.MultiWriter(f, h), rc)
		rc.Close()
		if err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return 0, "", fmt.Errorf("merge chunk %d: %w", i, err)
		}
		total += n
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("close merged file: %w", err)
	}
	if limit := p.maxSize(); total > limit {
		_ = os.Remove(tmp)
		return 0, "", &opError{Code: http.StatusRequestEntityTooLarge,
			Msg: fmt.Sprintf("file too large: %d > %d", total, limit)}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sess.Declared != "" && sum != sess.Declared {
		_ = os.Remove(tmp)
		return 0, "", &opError{Code: http.StatusBadRequest,
			Msg: fmt.Sprintf("sha256 mismatch: got %s want %s", sum, sess.Declared)}
	}
	if err := os.Rename(tmp, filepath.Join(dir, dataFileName)); err != nil {
		_ = os.Remove(tmp)
		return 0, "", fmt.Errorf("install merged file: %w", err)
	}
	return total, sum, nil
}

// handleList 处理 GET /api/files：全部已完成文件，按 createdAt 升序。
func (p *File) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p.mu.RLock()
	views := make([]fileView, 0, len(p.files))
	for _, m := range p.files {
		views = append(views, fileView{ // 值拷贝快照：编码在锁外进行（webhook D1 同款教训）
			ID: m.ID, Name: m.Name, Size: m.Size, SHA256: m.SHA256,
			Refs: m.Refs, ExpiresAt: m.ExpiresAt, CreatedAt: m.CreatedAt,
		})
	}
	p.mu.RUnlock()
	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAt != views[j].CreatedAt {
			return views[i].CreatedAt < views[j].CreatedAt
		}
		return views[i].ID < views[j].ID // 同刻上传按 id 排序，保证确定性
	})
	writeJSON(w, map[string]any{"files": views, "count": len(views)})
}

// handleFileOp 处理 {pathFilePrefix} 余量：
//   - GET    /api/file/{fileId}          下载
//   - DELETE /api/file/{fileId}          删除
//   - POST   /api/file/{fileId}/ref      引用 +1
//   - POST   /api/file/{fileId}/unref    引用 -1
func (p *File) handleFileOp(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, pathFilePrefix), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "missing file id")
		return
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, op := rest[:i], rest[i+1:]
		switch op {
		case "ref", "unref":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			p.handleRefOp(w, id, op == "ref")
		default:
			writeError(w, http.StatusNotFound, "unknown file operation: "+op)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.handleDownload(w, rest)
	case http.MethodDelete:
		p.handleDelete(w, rest)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodDelete)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleDownload 处理 GET /api/file/{fileId}：二进制流 + 类型/处置头。
func (p *File) handleDownload(w http.ResponseWriter, id string) {
	if !validFileID(id) {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	p.mu.RLock()
	m, ok := p.files[id]
	var snapshot fileMeta
	if ok {
		snapshot = *m // 值拷贝：Content-Type/Name 等只读字段随后在锁外使用
	}
	p.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	f, err := os.Open(filepath.Join(p.dataDir, id, dataFileName))
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found: "+id)
			return
		}
		writeError(w, http.StatusInternalServerError, "open file: "+err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", snapshot.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(snapshot.Size, 10))
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": snapshot.Name}); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set(hdrFileSHA256, snapshot.SHA256) // 便于下载方校验完整性
	_, _ = io.Copy(w, f)
}

// handleDelete 处理 DELETE /api/file/{fileId}：refs>0 → 409（引用中的文件不删）。
func (p *File) handleDelete(w http.ResponseWriter, id string) {
	if !validFileID(id) {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	p.mu.Lock()
	m, ok := p.files[id]
	if !ok {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	if m.Refs > 0 {
		refs := m.Refs
		p.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Sprintf("file is referenced (refs=%d)", refs))
		return
	}
	if err := os.RemoveAll(filepath.Join(p.dataDir, id)); err != nil {
		p.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "remove file: "+err.Error())
		return
	}
	delete(p.files, id)
	p.mu.Unlock()
	writeJSON(w, map[string]any{"deleted": id})
}

// handleRefOp 处理 POST /api/file/{fileId}/ref 与 /unref：引用计数调整（落盘持久化）。
func (p *File) handleRefOp(w http.ResponseWriter, id string, inc bool) {
	if !validFileID(id) {
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	p.mu.Lock()
	m, ok := p.files[id]
	if !ok {
		p.mu.Unlock()
		writeError(w, http.StatusNotFound, "file not found: "+id)
		return
	}
	changed := false
	if inc {
		m.Refs++
		changed = true
	} else if m.Refs > 0 {
		m.Refs--
		changed = true
	}
	refs := m.Refs
	if err := writeMeta(filepath.Join(p.dataDir, id), m); err != nil {
		if changed { // 回滚内存态，如实报错（不假装成功）
			if inc {
				m.Refs--
			} else {
				m.Refs++
			}
		}
		p.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "persist refs: "+err.Error())
		return
	}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"fileId": id, "refs": refs})
}

// ---- 内部辅助 ----

// discardSession 丢弃一条分片会话（删目录 + 移出会话表）。调用方持 sess.mu。
func (p *File) discardSession(s *uploadSession) {
	p.mu.Lock()
	delete(p.sessions, s.ID)
	p.mu.Unlock()
	_ = os.RemoveAll(filepath.Join(p.dataDir, s.ID))
}

// newID 生成不冲突的 fileId（查重 files + sessions 两张表）。
func (p *File) newID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.newIDLocked()
}

func (p *File) newIDLocked() string {
	for i := 0; i < 8; i++ {
		id := fileIDPrefix + protocol.UUID()
		if _, dup := p.files[id]; dup {
			continue
		}
		if _, dup := p.sessions[id]; dup {
			continue
		}
		return id
	}
	return fileIDPrefix + protocol.UUID() // 理论上不可达（96 位随机空间）
}

// 热更新配置的读访问器（与 Apply 的写同受 mu 保护，避免数据竞争）。
func (p *File) maxSize() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.maxSizeBytes
}

func (p *File) expiresAtFor(createdAt int64) int64 {
	p.mu.RLock()
	days := p.ttlDays
	p.mu.RUnlock()
	if days <= 0 {
		return 0 // 0 = 永不过期
	}
	return createdAt + int64(days)*24*3600*1000
}

func (p *File) incompleteTTLDuration() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.incompleteTTL
}

func (p *File) typeAllowed(contentType string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return typeAllowedLocked(contentType, p.allowedTypes)
}

// ---- 包级工具 ----

// typeAllowedLocked 判断 MIME 是否命中白名单模式（支持 image/* 通配）。
func typeAllowedLocked(contentType string, patterns []string) bool {
	for _, pat := range patterns {
		if pat == contentType {
			return true
		}
		if strings.HasSuffix(pat, "/*") && strings.HasPrefix(contentType, strings.TrimSuffix(pat, "*")) {
			return true
		}
	}
	return false
}

// parseAllowedTypes 解析逗号分隔的白名单配置（规范化：去空白、转小写、去空项）。
func parseAllowedTypes(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func mimeForExt(ext string) string {
	return extMIME[strings.ToLower(ext)]
}

// sanitizeName 规范化上传文件名：去空白、限长、只取基名（防路径穿越）。
func sanitizeName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLen {
		return "", false
	}
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if base == "" || base == "." || base == ".." || base == "/" || strings.ContainsRune(base, 0) {
		return "", false
	}
	return base, true
}

// validFileID 校验 fileId 形态（f_ + 24 位小写 hex）——同时是路径安全闸门。
func validFileID(id string) bool {
	if len(id) != len(fileIDPrefix)+fileIDHexLen || !strings.HasPrefix(id, fileIDPrefix) {
		return false
	}
	for i := len(fileIDPrefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func partPath(dir string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf("%s%05d%s", partFilePrefix, idx, partFileSuffix))
}

// scanParts 从磁盘重建已收到的分片下标集合（load 恢复会话用）。
func scanParts(dir string) map[int]bool {
	seen := make(map[int]bool)
	matches, _ := filepath.Glob(filepath.Join(dir, partFilePattern))
	for _, m := range matches {
		base := filepath.Base(m)
		base = strings.TrimSuffix(strings.TrimPrefix(base, partFilePrefix), partFileSuffix)
		if n, err := strconv.Atoi(base); err == nil && n >= 0 {
			seen[n] = true
		}
	}
	return seen
}

// removeIncompleteArtifacts 清理目录里的上传中间产物（分片/临时文件/会话状态）。
func removeIncompleteArtifacts(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, partFilePattern))
	for _, m := range matches {
		_ = os.Remove(m)
	}
	_ = os.Remove(filepath.Join(dir, tmpDataName))
	_ = os.Remove(filepath.Join(dir, uploadFileName))
}

// writeMeta 以「临时文件 + 重命名」落盘元数据（防崩溃写一半导致 load 失败）。
func writeMeta(dir string, m *fileMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, metaFileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, metaFileName))
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proto-Version", strconv.Itoa(protocol.ProtoCurrent))
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// respondErr 把内部错误映射为 HTTP 响应：opError 用其状态码，其余一律 500。
func respondErr(w http.ResponseWriter, err error) {
	var oe *opError
	if errors.As(err, &oe) {
		writeError(w, oe.Code, oe.Msg)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func init() {
	plugin.Register(&File{})
}
