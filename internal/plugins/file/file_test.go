package file

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// ---- rig ----

// fileRig 装配 file 插件（经 Manager，与生产同路径：BuildContext → Init → Start）。
// file 插件不使用 *core.Hub（只注册 HTTP 端点），hub 仅用于构建 Context。
func fileRig(t *testing.T, opts map[string]any) (*File, *plugin.Manager) {
	t.Helper()
	hub := core.NewHub()
	f := &File{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{f})
	mgr.ApplyConfig(map[string]bool{"file": true}, map[string]map[string]any{"file": opts})
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	results, err := mgr.StartAll()
	if err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	for _, r := range results {
		if r.Name == "file" && r.State != plugin.StateRunning {
			t.Fatalf("file 插件未正常运行: state=%s err=%s", r.State, r.Error)
		}
	}
	t.Cleanup(mgr.StopAll)
	return f, mgr
}

// ---- 辅助 ----

// postUpload 直接调用上传端点（不经 mux，与 webhook 测试同风格）。
func postUpload(f *File, query string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, pathFileUpload+"?"+query, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handleUpload(rec, req)
	return rec
}

// hiddenLenReader 包装一个 Reader 使其类型不再是 *bytes.Reader/*strings.Reader，
// 从而 httptest.NewRequest 会把 ContentLength 置为 -1（未知长度），
// 用来走 MaxBytesReader 的读时计数路径。
type hiddenLenReader struct{ r io.Reader }

func (h *hiddenLenReader) Read(p []byte) (int, error) { return h.r.Read(p) }

func getFile(f *File, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodGet, pathFilePrefix+id, nil))
	return rec
}

func deleteFile(f *File, id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodDelete, pathFilePrefix+id, nil))
	return rec
}

func refOp(f *File, id, op string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodPost, pathFilePrefix+id+"/"+op, nil))
	return rec
}

func listFiles(t *testing.T, f *File) []fileView {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handleList(rec, httptest.NewRequest(http.MethodGet, pathFileList, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Files []fileView `json:"files"`
		Count int        `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码列表: %v", err)
	}
	if resp.Count != len(resp.Files) {
		t.Fatalf("count=%d 与 files 长度 %d 不一致", resp.Count, len(resp.Files))
	}
	return resp.Files
}

func findView(t *testing.T, f *File, id string) fileView {
	t.Helper()
	for _, v := range listFiles(t, f) {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("列表中没有文件 %s", id)
	return fileView{}
}

// uploadOK 上传并断言 200，返回 fileId。
func uploadOK(t *testing.T, f *File, query string, body []byte) string {
	t.Helper()
	rec := postUpload(f, query, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("上传 %q 应 200，实得 %d: %s", query, rec.Code, rec.Body.String())
	}
	var resp struct {
		FileID string `json:"fileId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码上传响应: %v", err)
	}
	if resp.FileID == "" {
		t.Fatalf("响应缺少 fileId: %s", rec.Body.String())
	}
	return resp.FileID
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pngLike 构造带 PNG 魔术头的确定性二进制内容（类型检查按扩展名，内容任意二进制）。
func pngLike(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	for i := 8; i < n; i++ {
		b[i] = byte(i*7 % 251)
	}
	return b
}

func decodeUpload(rec *httptest.ResponseRecorder) (id, name, sum string, size int64, complete bool, received int) {
	var resp struct {
		FileID   string `json:"fileId"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
		Complete bool   `json:"complete"`
		Received int    `json:"received"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		panic(err)
	}
	return resp.FileID, resp.Name, resp.SHA256, resp.Size, resp.Complete, resp.Received
}

// ---- 上传/下载 roundtrip ----

// TestUploadDownloadRoundtrip：上传 → 下载字节一致、sha256 一致、响应头正确、列表字段齐全。
func TestUploadDownloadRoundtrip(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	content := pngLike(4096)

	rec := postUpload(f, "name=photo.png", content, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("上传应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	id, name, sum, size, complete, _ := decodeUpload(rec)
	if !complete || name != "photo.png" || size != int64(len(content)) {
		t.Fatalf("上传响应字段不正确: %s", rec.Body.String())
	}
	if sum != sha256Hex(content) {
		t.Fatalf("sha256 不符: got %s want %s", sum, sha256Hex(content))
	}
	if !validFileID(id) {
		t.Fatalf("fileId 格式不正确: %q", id)
	}

	// 下载：字节一致 + 头部
	rec = getFile(f, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("下载应 200，实得 %d", rec.Code)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, content) {
		t.Fatalf("下载内容不一致: %d vs %d 字节", len(got), len(content))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "photo.png") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if got := rec.Header().Get(hdrFileSHA256); got != sum {
		t.Fatalf("%s = %q, want %q", hdrFileSHA256, got, sum)
	}

	// 列表：恰好一条，字段齐全
	views := listFiles(t, f)
	if len(views) != 1 {
		t.Fatalf("列表应 1 条，实得 %d", len(views))
	}
	v := views[0]
	if v.ID != id || v.Name != "photo.png" || v.Size != int64(len(content)) ||
		v.SHA256 != sum || v.Refs != 0 || v.CreatedAt == 0 || v.ExpiresAt <= v.CreatedAt {
		t.Fatalf("列表字段不正确: %+v", v)
	}
}

// ---- 分片上传 ----

// TestChunkedUpload：3 片逐片 POST（name 关联，文档化流程），前两片 complete=false，
// 第三片 complete=true；合并后下载与原文一致，磁盘无分片残留。
func TestChunkedUpload(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	content := pngLike(100 * 1024) // 100KB 分 3 片
	chunks := [][]byte{content[:40000], content[40000:70000], content[70000:]}

	var id string
	for i := 0; i < 2; i++ {
		rec := postUpload(f, fmt.Sprintf("name=big.png&chunk=%d&chunks=3", i), chunks[i], nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("分片 %d 应 200，实得 %d: %s", i, rec.Code, rec.Body.String())
		}
		gotID, _, _, _, complete, received := decodeUpload(rec)
		if complete {
			t.Fatalf("分片 %d 不应标记完成", i)
		}
		if received != i+1 {
			t.Fatalf("分片 %d received = %d, want %d", i, received, i+1)
		}
		if id == "" {
			id = gotID
		} else if gotID != id {
			t.Fatalf("fileId 不稳定: %q vs %q", gotID, id)
		}
	}

	rec := postUpload(f, "name=big.png&chunk=2&chunks=3", chunks[2], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("最后一片应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	gotID, _, sum, size, complete, _ := decodeUpload(rec)
	if !complete || gotID != id || size != int64(len(content)) || sum != sha256Hex(content) {
		t.Fatalf("完成响应不正确: %s", rec.Body.String())
	}

	// 合并后可下载且字节一致
	rec = getFile(f, id)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("合并后下载应 200 且内容一致，实得 %d（%d 字节）", rec.Code, rec.Body.Len())
	}

	// 磁盘无分片残留、会话已清除
	parts, _ := filepath.Glob(filepath.Join(f.dataDir, id, partFilePattern))
	if len(parts) != 0 {
		t.Fatalf("合并后不应残留分片: %v", parts)
	}
	f.mu.RLock()
	_, live := f.sessions[id]
	f.mu.RUnlock()
	if live {
		t.Fatal("完成后会话应清除")
	}
}

// TestChunkResumeByFileIDOutOfOrder：显式 fileId 续传 + 乱序分片；
// 顺带覆盖未知 fileId → 404 与 chunks 不一致 → 400。
func TestChunkResumeByFileIDOutOfOrder(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	content := pngLike(30000)
	chunks := [][]byte{content[:10000], content[10000:20000], content[20000:]}

	// chunk 0 建会话
	rec := postUpload(f, "name=resume.png&chunk=0&chunks=3", chunks[0], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	id, _, _, _, complete, received := decodeUpload(rec)
	if complete || received != 1 {
		t.Fatalf("chunk 0 响应不正确: %s", rec.Body.String())
	}

	// 未知 fileId → 404
	rec = postUpload(f, "name=resume.png&chunk=1&chunks=3&fileId="+fileIDPrefix+strings.Repeat("0", fileIDHexLen), chunks[1], nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 fileId 应 404，实得 %d", rec.Code)
	}

	// chunks 不一致 → 400
	rec = postUpload(f, fmt.Sprintf("name=resume.png&chunk=1&chunks=4&fileId=%s", id), chunks[1], nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chunks 不一致应 400，实得 %d: %s", rec.Code, rec.Body.String())
	}

	// 乱序：chunk 2 → 仍差一片
	rec = postUpload(f, fmt.Sprintf("name=resume.png&chunk=2&chunks=3&fileId=%s", id), chunks[2], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("chunk 2 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if _, _, _, _, complete, received := decodeUpload(rec); complete || received != 2 {
		t.Fatalf("chunk 2 响应不正确: %s", rec.Body.String())
	}

	// chunk 1 到齐 → 完成
	rec = postUpload(f, fmt.Sprintf("name=resume.png&chunk=1&chunks=3&fileId=%s", id), chunks[1], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("chunk 1 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	gotID, _, sum, _, complete, _ := decodeUpload(rec)
	if !complete || gotID != id || sum != sha256Hex(content) {
		t.Fatalf("完成响应不正确: %s", rec.Body.String())
	}
	rec = getFile(f, id)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("乱序合并后下载应 200 且内容一致，实得 %d", rec.Code)
	}
}

// ---- 校验：sha256 / 大小 / 类型 ----

// TestSHA256MismatchRejected：声明错误 X-File-SHA256 → 400，且文件全部数据被删除。
func TestSHA256MismatchRejected(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir})
	content := pngLike(2048)

	rec := postUpload(f, "name=x.png", content, map[string]string{hdrFileSHA256: strings.Repeat("a", 64)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sha 不符应 400，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sha256 mismatch") {
		t.Fatalf("错误信息应含 sha256 mismatch: %s", rec.Body.String())
	}
	// 数据被清掉：列表空、磁盘空
	if views := listFiles(t, f); len(views) != 0 {
		t.Fatalf("sha 不符后列表应为空，实得 %+v", views)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("sha 不符后数据目录应清空，实得 %v", entries)
	}
}

// TestChunkSHA256MismatchRejected：分片上传在合并时校验声明 sha，不符 → 400 且目录清掉。
func TestChunkSHA256MismatchRejected(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir})
	content := pngLike(20000)
	bad := map[string]string{hdrFileSHA256: strings.Repeat("b", 64)}

	rec := postUpload(f, "name=y.png&chunk=0&chunks=2", content[:10000], bad)
	if rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	rec = postUpload(f, "name=y.png&chunk=1&chunks=2", content[10000:], nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("合并时 sha 不符应 400，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sha256 mismatch") {
		t.Fatalf("错误信息应含 sha256 mismatch: %s", rec.Body.String())
	}
	if views := listFiles(t, f); len(views) != 0 {
		t.Fatalf("失败后列表应为空，实得 %+v", views)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("失败后数据目录应清空，实得 %v", entries)
	}
}

// TestOversizeRejected：maxSizeBytes=1024，上传 2KB → 413。
// 覆盖两条路径：Content-Length 前置拦截 + 长度未知时 MaxBytesReader 读时计数。
func TestOversizeRejected(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir, "maxSizeBytes": 1024})
	big := pngLike(2048)

	// A) 声明了 Content-Length：前置拦截
	rec := postUpload(f, "name=big.png", big, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大应 413，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "file too large") {
		t.Fatalf("错误信息应含 file too large: %s", rec.Body.String())
	}

	// B) 隐藏长度（ContentLength=-1）：读时计数拦截
	req := httptest.NewRequest(http.MethodPost, pathFileUpload+"?name=big2.png", &hiddenLenReader{r: bytes.NewReader(big)})
	rec = httptest.NewRecorder()
	f.handleUpload(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("隐藏长度的超大上传应 413，实得 %d: %s", rec.Code, rec.Body.String())
	}

	// 两次失败都不留数据
	if views := listFiles(t, f); len(views) != 0 {
		t.Fatalf("超大上传后列表应为空，实得 %+v", views)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("超大上传后数据目录应清空，实得 %v", entries)
	}
}

// TestContentTypeRejected：evil.exe → 415；无/未知扩展名 → 415；白名单内放行。
func TestContentTypeRejected(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})

	rec := postUpload(f, "name=evil.exe", []byte("MZ"), nil)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf(".exe 应 415，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "content type not allowed: .exe") {
		t.Fatalf("错误信息应含 content type not allowed: .exe: %s", rec.Body.String())
	}
	for _, name := range []string{"noext", "a.xyz"} {
		if rec := postUpload(f, "name="+name, []byte("x"), nil); rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("name=%s 应 415，实得 %d", name, rec.Code)
		}
	}
	// 白名单内的类型放行（默认 image/*,text/*）
	for _, name := range []string{"ok.png", "note.txt", "cfg.yaml", "data.json"} {
		if rec := postUpload(f, "name="+name, []byte("hello"), nil); rec.Code != http.StatusOK {
			t.Fatalf("name=%s 应放行，实得 %d: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// ---- 参数与 404 ----

// TestMissingName：缺 name / 空 name / 路径穿越 name → 400；分片参数不成对 → 400。
func TestMissingName(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})

	if rec := postUpload(f, "", []byte("x"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400，实得 %d", rec.Code)
	}
	if rec := postUpload(f, "name=", []byte("x"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 name 应 400，实得 %d", rec.Code)
	}
	if rec := postUpload(f, "name=..", []byte("x"), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("name=.. 应 400，实得 %d", rec.Code)
	}
	// 带目录的名字只取基名（净化为 b.png 后放行）
	rec := postUpload(f, "name=a%2F..%2F..%2Fb.png", []byte("x"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("带目录的 name 应净化为基名后放行，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if _, name, _, _, _, _ := decodeUpload(rec); name != "b.png" {
		t.Fatalf("净化的 name = %q, want b.png", name)
	}
	// 分片参数不成对 / 越界 → 400
	for _, q := range []string{"name=x.png&chunk=1", "name=x.png&chunks=3", "name=x.png&chunk=0&chunks=0", "name=x.png&chunk=3&chunks=3"} {
		if rec := postUpload(f, q, []byte("x"), nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q 应 400，实得 %d", q, rec.Code)
		}
	}
}

// TestUnknownFile404：GET/DELETE/ref/unref 未知 fileId → 404；非法格式 id 同样 404。
func TestUnknownFile404(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	unknown := fileIDPrefix + strings.Repeat("0", fileIDHexLen)

	if rec := getFile(f, unknown); rec.Code != http.StatusNotFound {
		t.Fatalf("GET 未知应 404，实得 %d", rec.Code)
	}
	if rec := deleteFile(f, unknown); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE 未知应 404，实得 %d", rec.Code)
	}
	if rec := refOp(f, unknown, "ref"); rec.Code != http.StatusNotFound {
		t.Fatalf("ref 未知应 404，实得 %d", rec.Code)
	}
	if rec := refOp(f, unknown, "unref"); rec.Code != http.StatusNotFound {
		t.Fatalf("unref 未知应 404，实得 %d", rec.Code)
	}
	// 非法格式 id（路径安全闸门）
	if rec := getFile(f, "../etc/passwd"); rec.Code != http.StatusNotFound {
		t.Fatalf("非法 id 应 404，实得 %d", rec.Code)
	}
}

// ---- 引用计数 ----

// TestRefsProtectDelete：ref 后 DELETE → 409；unref 后 DELETE → 200；unref 下限 0。
func TestRefsProtectDelete(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir})
	id := uploadOK(t, f, "name=keep.png", pngLike(1024))

	rec := refOp(f, id, "ref")
	if rec.Code != http.StatusOK {
		t.Fatalf("ref 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var refResp struct {
		FileID string `json:"fileId"`
		Refs   int    `json:"refs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refResp); err != nil {
		t.Fatalf("解码 ref 响应: %v", err)
	}
	if refResp.FileID != id || refResp.Refs != 1 {
		t.Fatalf("ref 响应不正确: %s", rec.Body.String())
	}

	// 引用中 → 409
	rec = deleteFile(f, id)
	if rec.Code != http.StatusConflict {
		t.Fatalf("引用中删除应 409，实得 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "file is referenced (refs=1)") {
		t.Fatalf("409 信息不正确: %s", rec.Body.String())
	}
	// 文件仍在
	if rec := getFile(f, id); rec.Code != http.StatusOK {
		t.Fatalf("引用中文件应仍可下载，实得 %d", rec.Code)
	}

	// unref → 0；再 unref 仍为 0（下限保护）
	rec = refOp(f, id, "unref")
	if rec.Code != http.StatusOK {
		t.Fatalf("unref 应 200，实得 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refResp); err != nil || refResp.Refs != 0 {
		t.Fatalf("unref 响应不正确: %s", rec.Body.String())
	}
	rec = refOp(f, id, "unref")
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 unref 应 200，实得 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refResp); err != nil || refResp.Refs != 0 {
		t.Fatalf("重复 unref 后 refs 应仍为 0: %s", rec.Body.String())
	}

	// 无引用 → 删除成功，随后 404，磁盘目录已删
	rec = deleteFile(f, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("无引用删除应 200，实得 %d", rec.Code)
	}
	rec = getFile(f, id)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删除后 GET 应 404，实得 %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, id)); !os.IsNotExist(err) {
		t.Fatalf("删除后磁盘目录应不存在，实得 %v", err)
	}
}

// ---- TTL 清理 ----

// TestTTLSweep：注入 now 越过 expiresAt 后手动 sweep——无引用文件消失、
// refs>0 文件保留、磁盘目录同步删除。
func TestTTLSweep(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir, "ttlDays": 1, "sweepInterval": "1m"})

	base := time.Now().UnixMilli()
	f.now = func() int64 { return base }
	id1 := uploadOK(t, f, "name=a.png", pngLike(1024))
	id2 := uploadOK(t, f, "name=b.png", pngLike(1024))

	// 引用其中一个（模拟发送方 ref）
	if rec := refOp(f, id2, "ref"); rec.Code != http.StatusOK {
		t.Fatalf("ref 应 200，实得 %d", rec.Code)
	}

	// 时间越过两个文件的 expiresAt（base + 24h）
	f.now = func() int64 { return base + 25*3600*1000 }
	f.sweep()

	if rec := getFile(f, id1); rec.Code != http.StatusNotFound {
		t.Fatalf("过期无引用文件应被清掉（GET 404），实得 %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, id1)); !os.IsNotExist(err) {
		t.Fatalf("过期文件的磁盘目录应被删除，实得 %v", err)
	}
	if rec := getFile(f, id2); rec.Code != http.StatusOK {
		t.Fatalf("refs>0 的过期文件不应被清掉，实得 %d", rec.Code)
	}
	views := listFiles(t, f)
	if len(views) != 1 || views[0].ID != id2 {
		t.Fatalf("清理后列表应只剩被引用文件，实得 %+v", views)
	}
}

// TestIncompleteSweep：未完成分片目录按 mtime 清理——mtime 新的不删、
// mtime 超过 incompleteTTL 的删；已完成文件即使目录 mtime 老也不受此清理影响。
func TestIncompleteSweep(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir, "incompleteTTLHours": 24, "sweepInterval": "1m"})

	// 已完成文件（默认 ttlDays=7，未过期）：目录 mtime 造假到 25h 前也不该被未完成清理碰
	doneID := uploadOK(t, f, "name=done.png", pngLike(512))
	doneDir := filepath.Join(dir, doneID)
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(doneDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// 手动造一个未完成分片目录（无 meta.json / upload.json）
	stale := filepath.Join(dir, fileIDPrefix+strings.Repeat("ab", fileIDHexLen/2))
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "part-00000.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// mtime 新：不删
	f.sweep()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("mtime 新的未完成目录不应被清掉: %v", err)
	}

	// mtime 老化到 25h 前：删
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	f.sweep()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("过期未完成目录应被清掉，实得 %v", err)
	}

	// 已完成文件不受影响
	if rec := getFile(f, doneID); rec.Code != http.StatusOK {
		t.Fatalf("已完成文件不应被未完成清理波及，实得 %d", rec.Code)
	}
}

// ---- 重启恢复 ----

// TestRestartRecovery：Stop 前上传（含 ref 与进行中的分片），Reload（Stop→Init→Start，
// 同 dataDir）后列表仍可下载、refs 保留、分片会话可续传完成。
func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	f, mgr := fileRig(t, map[string]any{"dataDir": dir})

	// 完整文件 + ref（验证 meta.json 持久化 refs）
	content := pngLike(3000)
	id := uploadOK(t, f, "name=photo.png", content)
	if rec := refOp(f, id, "ref"); rec.Code != http.StatusOK {
		t.Fatalf("ref 应 200，实得 %d", rec.Code)
	}

	// 进行中的分片上传（只传第 0 片）
	part := pngLike(20000)
	rec := postUpload(f, "name=pending.png&chunk=0&chunks=2", part[:10000], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("chunk 0 应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}

	// 重启（生产路径）
	mgr.Reload()

	// 完整文件恢复：列表在、refs 保留、可下载
	v := findView(t, f, id)
	if v.Refs != 1 || v.Name != "photo.png" || v.Size != int64(len(content)) || v.SHA256 != sha256Hex(content) {
		t.Fatalf("重启后元数据不正确: %+v", v)
	}
	rec = getFile(f, id)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("重启后下载应 200 且内容一致，实得 %d", rec.Code)
	}

	// 分片会话恢复：续传最后一片 → 完成并可下载
	rec = postUpload(f, "name=pending.png&chunk=1&chunks=2", part[10000:], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("重启后续传应 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	pid, _, psum, _, complete, _ := decodeUpload(rec)
	if !complete || psum != sha256Hex(part) {
		t.Fatalf("重启后续传完成响应不正确: %s", rec.Body.String())
	}
	rec = getFile(f, pid)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), part) {
		t.Fatalf("重启后续传完成的文件应可下载且内容一致，实得 %d", rec.Code)
	}
}

// ---- 生命周期 ----

// TestStartIdempotent：双 Start 不得起第二个循环（loopStarts 不 +1）；
// 单 Stop 必须及时返回（若泄漏循环，wg.Wait 会挂住）；停后可再启动。
func TestStartIdempotent(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir(), "sweepInterval": "10ms"})

	f.mu.Lock()
	before := f.loopStarts
	f.mu.Unlock()
	if before != 1 {
		t.Fatalf("rig 启动后 loopStarts 应为 1，实得 %d", before)
	}

	if err := f.Start(); err != nil {
		t.Fatalf("二次 Start 应成功: %v", err)
	}
	f.mu.Lock()
	after := f.loopStarts
	f.mu.Unlock()
	if after != before {
		t.Fatalf("双 Start 泄漏了清理循环: loopStarts %d → %d", before, after)
	}

	// Stop 必须及时返回（wg.Wait 等到循环退出；泄漏则挂住 → 超时失败）
	done := make(chan error, 1)
	go func() { done <- f.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 挂住：清理循环泄漏")
	}
	f.mu.Lock()
	cleared := f.cancel == nil
	f.mu.Unlock()
	if !cleared {
		t.Fatal("Stop 后 cancel 应清空")
	}

	// 停后可再启动（运行期启用路径）；t.Cleanup 的 StopAll 会负责最终停止
	if err := f.Start(); err != nil {
		t.Fatalf("停止后重新 Start 应成功: %v", err)
	}
}

// TestMethodNotAllowed：各端点的方法约束（405）与未知 op（404）。
func TestMethodNotAllowed(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	id := fileIDPrefix + strings.Repeat("0", fileIDHexLen)

	rec := httptest.NewRecorder()
	f.handleUpload(rec, httptest.NewRequest(http.MethodPut, pathFileUpload, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/file 应 405，实得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.handleList(rec, httptest.NewRequest(http.MethodPost, pathFileList, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/files 应 405，实得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodPut, pathFilePrefix+id, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/file/{id} 应 405，实得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodGet, pathFilePrefix+id+"/ref", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/file/{id}/ref 应 405，实得 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.handleFileOp(rec, httptest.NewRequest(http.MethodPost, pathFilePrefix+id+"/bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 op 应 404，实得 %d", rec.Code)
	}
}

// ---- 配置热更新 ----

// TestApplyHotUpdate：ttlDays 热更新只影响之后新建的文件（expiresAt 创建时写死进
// meta.json）；allowedTypes 热更新立即生效；非法类型值报错。
func TestApplyHotUpdate(t *testing.T) {
	dir := t.TempDir()
	f, _ := fileRig(t, map[string]any{"dataDir": dir, "ttlDays": 1, "sweepInterval": "1m"})

	base := time.Now().UnixMilli()
	f.now = func() int64 { return base }
	id1 := uploadOK(t, f, "name=old.png", pngLike(1024)) // expiresAt = base + 1d

	if err := f.Apply(map[string]any{"ttlDays": 2}); err != nil {
		t.Fatalf("Apply ttlDays: %v", err)
	}
	id2 := uploadOK(t, f, "name=new.png", pngLike(1024)) // expiresAt = base + 2d

	v1 := findView(t, f, id1)
	if v1.ExpiresAt != base+24*3600*1000 {
		t.Fatalf("老文件应保留原 expiresAt: %d", v1.ExpiresAt)
	}
	v2 := findView(t, f, id2)
	if v2.ExpiresAt != base+48*3600*1000 {
		t.Fatalf("新文件应按新 ttlDays 计算 expiresAt: %d", v2.ExpiresAt)
	}

	// 时间越过第 1 天：老文件过期被清，新文件还在
	f.now = func() int64 { return base + 25*3600*1000 }
	f.sweep()
	if rec := getFile(f, id1); rec.Code != http.StatusNotFound {
		t.Fatalf("老文件应过期被清，实得 %d", rec.Code)
	}
	if rec := getFile(f, id2); rec.Code != http.StatusOK {
		t.Fatalf("新文件不应被清，实得 %d", rec.Code)
	}

	// allowedTypes 热更新：缩窄到只有 image/png
	if err := f.Apply(map[string]any{"allowedTypes": "image/png"}); err != nil {
		t.Fatalf("Apply allowedTypes: %v", err)
	}
	if rec := postUpload(f, "name=note.txt", []byte("hi"), nil); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("热更新后 .txt 应 415，实得 %d", rec.Code)
	}
	if rec := postUpload(f, "name=ok.png", []byte("hi"), nil); rec.Code != http.StatusOK {
		t.Fatalf("热更新后 .png 应放行，实得 %d", rec.Code)
	}
	// 非法类型值 → 报错（不静默吞掉）
	if err := f.Apply(map[string]any{"allowedTypes": 123}); err == nil {
		t.Fatal("allowedTypes 传非字符串应报错")
	}
}

// ---- 列表顺序 ----

// TestListOrderByCreatedAt：按 createdAt 升序；同刻上传按 id 排序（确定性）。
func TestListOrderByCreatedAt(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})

	var clock int64 = 1700000000000
	f.now = func() int64 { return clock }
	a := uploadOK(t, f, "name=a.png", pngLike(256))
	clock += 5000
	b := uploadOK(t, f, "name=b.png", pngLike(256))
	clock += 5000
	c := uploadOK(t, f, "name=c.png", pngLike(256))

	views := listFiles(t, f)
	if len(views) != 3 || views[0].ID != a || views[1].ID != b || views[2].ID != c {
		t.Fatalf("列表应按 createdAt 升序: %s", recIDs(views))
	}
	// 同刻：d、e 按 id 升序（d/e 谁 id 小不确定，断言有序即可）
	clock += 5000
	d := uploadOK(t, f, "name=d.png", pngLike(256))
	e := uploadOK(t, f, "name=e.png", pngLike(256))
	views = listFiles(t, f)
	last3 := []string{views[3].ID, views[4].ID}
	if !((last3[0] == d && last3[1] == e) || (last3[0] == e && last3[1] == d)) {
		t.Fatalf("同刻上传的两条应在末尾: %s", recIDs(views))
	}
	if views[3].CreatedAt != views[4].CreatedAt || views[3].ID > views[4].ID {
		t.Fatalf("同刻上传应按 id 升序: %s", recIDs(views))
	}
}

func recIDs(views []fileView) string {
	ids := make([]string, len(views))
	for i, v := range views {
		ids[i] = v.ID
	}
	return strings.Join(ids, ",")
}

// ---- 并发 ----

// TestConcurrentUploadDownload：多文件并发上传 + 下载，数据不串。
func TestConcurrentUploadDownload(t *testing.T) {
	f, _ := fileRig(t, map[string]any{"dataDir": t.TempDir()})
	const n = 8
	contents := make([][]byte, n)
	for i := range contents {
		contents[i] = pngLike(64 * 1024)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := contents[i]
			rec := postUpload(f, fmt.Sprintf("name=c%d.png", i), content, nil)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("文件 %d 上传应 200，实得 %d: %s", i, rec.Code, rec.Body.String())
				return
			}
			id, _, sum, size, complete, _ := decodeUpload(rec)
			if !complete || size != int64(len(content)) || sum != sha256Hex(content) {
				errs <- fmt.Errorf("文件 %d 上传响应不正确: %s", i, rec.Body.String())
				return
			}
			rec = getFile(f, id)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("文件 %d 下载应 200，实得 %d", i, rec.Code)
				return
			}
			if !bytes.Equal(rec.Body.Bytes(), content) {
				errs <- fmt.Errorf("文件 %d 下载数据不一致（串文件）", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if views := listFiles(t, f); len(views) != n {
		t.Fatalf("并发上传后列表应 %d 条，实得 %d", n, len(views))
	}
}
