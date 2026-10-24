package httpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newUpdateMux 构造只含更新预留端点的路由表，避免测试耦合 14 个历史端点。
func newUpdateMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerUpdateRoutes(func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, h)
	})
	return mux
}

func doGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// 没有清单文件时，接口必须返回 200 + latest 为空——"没有更新"是合法状态，
// 不是 404。客户端据此区分"接口不存在"与"已是最新"。
func TestUpdateManifestEmptyWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)

	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	var m UpdateManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if m.Latest != "" {
		t.Errorf("latest = %q want empty", m.Latest)
	}
	if m.Items == nil || len(m.Items) != 0 {
		t.Errorf("artifacts = %v want empty object", m.Items)
	}
}

// 正常清单应原样透传，且 Source 指向实际文件，便于排查分发问题。
func TestUpdateManifestPassthrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	body := `{"name":"dsh-plugin-lan-hub","latest":"0.6.0","proto":3,"minHost":"0.2.0",
	          "artifacts":{"0.6.0":{"url":"/api/updates/pkg/a.tgz","sha256":"abc","size":123,"proto":3}}}
	`
	if err := os.WriteFile(filepath.Join(dir, updateManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	var m UpdateManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Latest != "0.6.0" || m.Proto != 3 {
		t.Errorf("latest/proto = %q/%d want 0.6.0/3", m.Latest, m.Proto)
	}
	if a := m.Items["0.6.0"]; a.SHA256 != "abc" || a.Size != 123 {
		t.Errorf("artifact 未透传: %+v", a)
	}
	if m.Source == "" {
		t.Error("Source 为空，排查时无法定位清单文件")
	}
}

// 签名字段必须原样透传。Hub 只是分发点，不理解也不校验签名——信任判断全在客户端，
// 因为客户端的公钥是本地 pin 的，Hub 说什么都不影响结论。
func TestUpdateManifestSignaturePassthrough(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	body := `{"name":"dsh-plugin-lan-hub","latest":"0.7.0","keyId":"ab12cd34","proto":3,
	          "artifacts":{"0.7.0":{"url":"/api/updates/pkg/a.tgz","sha256":"abc","size":123,
	          "proto":3,"sig":"SGVsbG8=","sigAlgo":"ed25519"}}}`
	if err := os.WriteFile(filepath.Join(dir, updateManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (%s)", rec.Code, rec.Body.String())
	}
	var m UpdateManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.KeyID != "ab12cd34" {
		t.Errorf("keyId = %q want ab12cd34", m.KeyID)
	}
	a := m.Items["0.7.0"]
	if a.Sig != "SGVsbG8=" {
		t.Errorf("sig = %q want SGVsbG8=", a.Sig)
	}
	if a.SigAlgo != "ed25519" {
		t.Errorf("sigAlgo = %q want ed25519", a.SigAlgo)
	}
}

// 未签名的制品也必须能透传（本地开发常见）。是否拒绝由客户端按"是否配置了公钥"决定，
// Hub 不做这个判断——它没有立场知道客户端的信任策略。
func TestUpdateManifestUnsignedAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	body := `{"latest":"0.7.0","artifacts":{"0.7.0":{"url":"/a.tgz","sha256":"abc","size":1,"proto":3}}}`
	if err := os.WriteFile(filepath.Join(dir, updateManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	var m UpdateManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Items["0.7.0"].Sig != "" {
		t.Errorf("未签名制品的 sig 应为空，实际 %q", m.Items["0.7.0"].Sig)
	}
}

// latest 指向不存在的制品属于分发事故：必须 500 报错，不能装作"没有更新"。
func TestUpdateManifestInconsistentLatest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	body := `{"latest":"9.9.9","artifacts":{}}`
	if err := os.WriteFile(filepath.Join(dir, updateManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d want 500", rec.Code)
	}
}

func TestUpdateManifestCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, updateManifestFile), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doGet(t, newUpdateMux(), UpdateManifestPath)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d want 500", rec.Code)
	}
}

// 制品下载：合法文件名可取到内容，穿越与非法后缀一律拒绝。
func TestUpdateArtifactDownloadAndTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANHUB_UPDATE_DIR", dir)
	pkgDir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("fake-tgz-bytes")
	if err := os.WriteFile(filepath.Join(pkgDir, "a.tgz"), want, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := doGet(t, newUpdateMux(), updateArtifactPrefix+"a.tgz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	if got := rec.Body.Bytes(); string(got) != string(want) {
		t.Errorf("body = %q want %q", got, want)
	}

	for _, bad := range []string{
		updateArtifactPrefix + "../lanhub-update.json",
		updateArtifactPrefix + "..%2Fa.tgz",
		updateArtifactPrefix + "a.zip",
		updateArtifactPrefix,
	} {
		if rec := doGet(t, newUpdateMux(), bad); rec.Code == http.StatusOK {
			t.Errorf("%s 应被拒绝，实际 200", bad)
		}
	}
}

// 同一 mux 重复挂载必须幂等：插件 reload 会再次 Init，而 ctx.ServeMux() 返回同一对象，
// 直接 HandleFunc 重复注册会 panic。
func TestAttachUpdateRoutesIdempotent(t *testing.T) {
	t.Setenv("LANHUB_UPDATE_DIR", t.TempDir())
	mux := http.NewServeMux()
	attachUpdateRoutes(mux)
	attachUpdateRoutes(mux) // 不应 panic
	if rec := doGet(t, mux, UpdateManifestPath); rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
}
