package webconsole

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	"lan-agent-hub/internal/plugin"
	"path/filepath"
	"strings"
	"testing"
)

// do 直接驱动端点（不经 mux 的路径清洗），以便精确断言目录穿越这类输入。
func do(t *testing.T, s *i18nStore, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decodePackResp(t *testing.T, rec *httptest.ResponseRecorder) i18nPack {
	t.Helper()
	var p i18nPack
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应不是合法语言包：%v（body=%s）", err, rec.Body.String())
	}
	return p
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error
}

/* ---------- 1. 内置语言包 ---------- */

func TestI18NBuiltinPacksReadable(t *testing.T) {
	s := newI18NStore("", defaultLanguage)

	rec := do(t, s, "/api/i18n/zh-CN")
	if rec.Code != http.StatusOK {
		t.Fatalf("zh-CN 应可读，实得 %d（%s）", rec.Code, rec.Body.String())
	}
	p := decodePackResp(t, rec)
	if p.Source != "builtin" {
		t.Fatalf("内置包 source 应为 builtin，实得 %q", p.Source)
	}
	if got := p.Messages["nav.agents"]; got != "在线 Agent" {
		t.Fatalf("zh-CN 的 nav.agents 应为「在线 Agent」，实得 %q", got)
	}
	if p.Keys != len(p.Messages) || p.Keys == 0 {
		t.Fatalf("keys 应与非空消息数一致且非零：keys=%d messages=%d", p.Keys, len(p.Messages))
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type 应为 JSON，实得 %q", got)
	}

	rec = do(t, s, "/api/i18n/en")
	if rec.Code != http.StatusOK {
		t.Fatalf("en 应可读，实得 %d", rec.Code)
	}
	en := decodePackResp(t, rec)
	if got := en.Messages["nav.agents"]; got != "Online Agents" {
		t.Fatalf("en 的 nav.agents 应为 Online Agents，实得 %q", got)
	}
}

// TestI18NDefaultIsZhCN 钉住「默认不变成英文」这条验收点：
// 未配置任何东西时，清单里的默认语言必须是 zh-CN。
func TestI18NDefaultIsZhCN(t *testing.T) {
	s := &i18nStore{} // 零值：模拟 Init 未跑
	rec := do(t, s, "/api/i18n/")
	if rec.Code != http.StatusOK {
		t.Fatalf("索引应可读，实得 %d", rec.Code)
	}
	var idx i18nIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("索引不是合法 JSON：%v", err)
	}
	if idx.Default != "zh-CN" {
		t.Fatalf("默认语言必须是 zh-CN（既有偏好不变），实得 %q", idx.Default)
	}
	if idx.RefKeys == 0 {
		t.Fatal("RefKeys 应为内置默认语言的键数，实得 0")
	}
	for _, want := range []string{"zh-CN", "en"} {
		found := false
		for _, l := range idx.Languages {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("内置语言 %q 未出现在清单里（实得 %v）", want, idx.Languages)
		}
	}
	for _, l := range idx.Languages {
		if l == i18nTemplateName {
			t.Fatalf("贡献模板 %s 不应作为语言出现在清单里", i18nTemplateName)
		}
	}
}

/* ---------- 2. 外部目录覆盖 ---------- */

func TestI18NExternalDirOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	// 只覆盖一个键 + 新增一个键：证明是**按键合并**而非整包替换。
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("en.json", `{"nav.agents":"Online Agents (custom)","my.extra.key":"extra"}`)

	s := newI18NStore(dir, defaultLanguage)
	rec := do(t, s, "/api/i18n/en")
	if rec.Code != http.StatusOK {
		t.Fatalf("en 应可读，实得 %d（%s）", rec.Code, rec.Body.String())
	}
	p := decodePackResp(t, rec)
	if p.Source != "merged" {
		t.Fatalf("内置+外部同时存在时 source 应为 merged，实得 %q", p.Source)
	}
	if got := p.Messages["nav.agents"]; got != "Online Agents (custom)" {
		t.Fatalf("外部包应覆盖同键，实得 %q", got)
	}
	if got := p.Messages["nav.rooms"]; got != "Rooms" {
		t.Fatalf("未覆盖的键应保留内置值，实得 %q", got)
	}
	if got := p.Messages["my.extra.key"]; got != "extra" {
		t.Fatalf("外部包应能新增键，实得 %q", got)
	}
}

// TestI18NExternalDirAddsLanguage 钉住「新增语言 = 丢一个 JSON 文件」这条验收点。
func TestI18NExternalDirAddsLanguage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ja.json"), []byte(`{"nav.agents":"オンラインエージェント"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newI18NStore(dir, defaultLanguage)
	rec := do(t, s, "/api/i18n/ja")
	if rec.Code != http.StatusOK {
		t.Fatalf("外部新增的 ja 应可读（未改代码、未重新编译），实得 %d（%s）", rec.Code, rec.Body.String())
	}
	p := decodePackResp(t, rec)
	if p.Source != "external" {
		t.Fatalf("ja 无内置包，source 应为 external，实得 %q", p.Source)
	}
	if got := p.Messages["nav.agents"]; got != "オンラインエージェント" {
		t.Fatalf("ja 的 nav.agents 实得 %q", got)
	}

	// 清单里必须能看见这门新语言，否则前端下拉框无从选择。
	rec = do(t, s, "/api/i18n/")
	var idx i18nIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range idx.Languages {
		if l == "ja" {
			found = true
		}
	}
	if !found {
		t.Fatalf("外部新增的 ja 未出现在语言清单里（实得 %v）", idx.Languages)
	}
	if idx.ExternalDir != dir {
		t.Fatalf("清单应回显外部目录，实得 %q，期望 %q", idx.ExternalDir, dir)
	}
}

/* ---------- 3. 安全：目录穿越 / 后缀 / 方法 ---------- */

func TestI18NRejectsPathTraversal(t *testing.T) {
	// 外部目录里放一份"机密"文件，穿越若成功就会读到它。
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"leak":"yes"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"leak":"yes"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newI18NStore(dir, defaultLanguage)

	cases := []struct {
		name   string
		target string
	}{
		{"父目录穿越", "/api/i18n/../settings"},
		{"多级穿越到绝对路径", "/api/i18n/..%2f..%2fetc%2fpasswd"},
		{"编码后的点点", "/api/i18n/%2e%2e%2f%2e%2e%2fetc%2fpasswd"},
		{"反斜杠分隔", "/api/i18n/..\\..\\etc\\passwd"},
		{"带路径分隔符", "/api/i18n/sub/dir"},
		{"带扩展名（只接受 .json）", "/api/i18n/en.txt"},
		{"含点号的语言名", "/api/i18n/..json"},
		{"空语言名之外的非法字符", "/api/i18n/zh CN"},
	}
	for _, c := range cases {
		// 直接构造请求并回写 URL.Path：走 httptest.NewRequest(string) 会被 URL 解析
		// 提前规范化（空格等非法字符还会 panic），测不到编码形态的穿越输入。
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = c.target
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s：应被拒绝为 400，实得 %d（body=%s）", c.name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "leak") {
			t.Fatalf("%s：响应里泄漏了外部文件内容", c.name)
		}
	}
}

// TestI18NExternalPathStaysInsideDir 直接复核路径拼装本身（绕开 HTTP 层）。
func TestI18NExternalPathStaysInsideDir(t *testing.T) {
	dir := t.TempDir()
	p, err := externalPackPath(dir, "ja")
	if err != nil {
		t.Fatalf("合法语言应拼出路径：%v", err)
	}
	if want := filepath.Join(filepath.Clean(dir), "ja.json"); p != want {
		t.Fatalf("路径拼装错误：实得 %q，期望 %q", p, want)
	}
}

func TestI18NMethodNotAllowed(t *testing.T) {
	s := newI18NStore("", defaultLanguage)
	req := httptest.NewRequest(http.MethodPost, "/api/i18n/en", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应为 405，实得 %d", rec.Code)
	}
}

/* ---------- 4. 异常输入：未知语言 / 空值 / 非法 JSON / 超限 / 空文件 ---------- */

func TestI18NUnknownLanguageIs404(t *testing.T) {
	s := newI18NStore("", defaultLanguage)
	rec := do(t, s, "/api/i18n/de")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知语言应 404，实得 %d（%s）", rec.Code, rec.Body.String())
	}
	if decodeErr(t, rec) == "" {
		t.Fatal("404 响应应带 error 说明")
	}
}

// TestI18NEmptyValuesCountAsMissing 钉住空值语义：外部包值全空时，
// 该键视为「未翻译」——内置值保留；两边都空则该键缺席（前端回退键名并告警）。
func TestI18NEmptyValuesCountAsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "en.json"), []byte(`{"nav.agents":"","view.overview.title":"Overview"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newI18NStore(dir, defaultLanguage)
	p := decodePackResp(t, do(t, s, "/api/i18n/en"))

	if got := p.Messages["nav.agents"]; got != "Online Agents" {
		t.Fatalf("外部空值不应覆盖内置值，实得 %q（期望内置 Online Agents）", got)
	}
	if got := p.Messages["view.overview.title"]; got != "Overview" {
		t.Fatalf("外部非空值应生效，实得 %q", got)
	}

	// 全部为空的语言（如 TEMPLATE 形态）→ 键全缺席，但请求本身仍成功（不阻塞渲染）。
	if err := os.WriteFile(filepath.Join(dir, "ja.json"), []byte(`{"nav.agents":"","nav.rooms":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := do(t, s, "/api/i18n/ja")
	if rec.Code != http.StatusOK {
		t.Fatalf("全空语言包仍应 200，实得 %d", rec.Code)
	}
	jp := decodePackResp(t, rec)
	if jp.Keys != 0 || len(jp.Messages) != 0 {
		t.Fatalf("全空值应视为零个已翻译键，实得 keys=%d messages=%d", jp.Keys, len(jp.Messages))
	}
}

// TestI18NTemplatePackServesAllKeysEmpty 顺带把 webroot/i18n/TEMPLATE.json 当语言取回：
// 它列出全部键但值全空，正是「缺失键」演示与翻译协作的输入。
func TestI18NTemplatePackServesAllKeysEmpty(t *testing.T) {
	s := newI18NStore("", defaultLanguage)
	rec := do(t, s, "/api/i18n/"+i18nTemplateName)
	if rec.Code != http.StatusOK {
		t.Fatalf("TEMPLATE 应可取回，实得 %d（%s）", rec.Code, rec.Body.String())
	}
	p := decodePackResp(t, rec)
	if p.Keys != 0 {
		t.Fatalf("TEMPLATE 的值应全空（keys=0），实得 %d", p.Keys)
	}
}

func TestI18NRejectsBrokenExternalPack(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"非法 JSON", `{not json`, http.StatusInternalServerError},
		{"空文件", ``, http.StatusInternalServerError},
		{"顶层是数组", `["a"]`, http.StatusInternalServerError},
		{"值不是字符串", `{"nav.agents":42}`, http.StatusInternalServerError},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "en.json"), []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newI18NStore(dir, defaultLanguage)
		rec := do(t, s, "/api/i18n/en")
		if rec.Code != c.want {
			t.Fatalf("%s：应为 %d，实得 %d（body=%s）", c.name, c.want, rec.Code, rec.Body.String())
		}
		if decodeErr(t, rec) == "" {
			t.Fatalf("%s：错误响应应带 error 说明", c.name)
		}
	}
}

func TestI18NRejectsOversizedExternalPack(t *testing.T) {
	dir := t.TempDir()
	body := `{"nav.agents":"` + strings.Repeat("x", i18nMaxPackBytes+64) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "en.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newI18NStore(dir, defaultLanguage)
	rec := do(t, s, "/api/i18n/en")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限语言包应为 413，实得 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

/* ---------- 5. 内置语言包的键奇偶（V7-4：防键名漂移） ---------- */

func TestI18NBuiltinPacksShareSameKeySet(t *testing.T) {
	ref, err := loadBuiltinPack(defaultLanguage)
	if err != nil {
		t.Fatalf("内置默认语言包不可读：%v", err)
	}
	if len(ref) == 0 {
		t.Fatal("内置默认语言包为空")
	}
	for _, lang := range []string{"en", i18nTemplateName} {
		got, err := loadBuiltinPack(lang)
		if err != nil {
			t.Fatalf("内置 %s 不可读：%v", lang, err)
		}
		for k := range ref {
			if _, ok := got[k]; !ok {
				t.Fatalf("内置 %s 缺少键 %q（zh-CN 有 %d 个键，%s 有 %d 个）", lang, k, len(ref), lang, len(got))
			}
		}
		for k := range got {
			if _, ok := ref[k]; !ok {
				t.Fatalf("内置 %s 多出键 %q（zh-CN 没有）", lang, k)
			}
		}
	}
	// 内置语言包的值不能为空串：源语言缺值会让默认界面直接显示键名。
	for k, v := range ref {
		if v == "" {
			t.Fatalf("内置 %s 的键 %q 值为空", defaultLanguage, k)
		}
	}
}

/* ---------- 6. 插件侧：配置声明与热更新 ---------- */

func TestWebConsoleOptionsDeclared(t *testing.T) {
	p := &WebConsole{}
	defs := p.Options()
	want := map[string]bool{optLanguage: false, optI18NDir: false}
	for _, d := range defs {
		if _, ok := want[d.Key]; !ok {
			t.Fatalf("意外的配置项 %q", d.Key)
		}
		want[d.Key] = true
		if d.Description == "" {
			t.Fatalf("配置项 %q 缺 Description（插件契约要求）", d.Key)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("缺少配置项 %q", k)
		}
	}
	for _, d := range defs {
		if d.Key != optLanguage {
			continue
		}
		if d.Type != plugin.OptionSelect || len(d.Choices) == 0 {
			t.Fatalf("language 应是带 Choices 的 select，实得 type=%q choices=%v", d.Type, d.Choices)
		}
		if d.Default != defaultLanguage {
			t.Fatalf("language 默认值应为 %q，实得 %v", defaultLanguage, d.Default)
		}
	}
}

func TestWebConsoleApplyHotSwitchesI18N(t *testing.T) {
	p := &WebConsole{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ja.json"), []byte(`{"nav.agents":"エージェント"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 热更新：外部目录生效，无需重启进程。
	if err := p.Apply(map[string]any{optI18NDir: dir}); err != nil {
		t.Fatalf("Apply(i18n.dir) 失败：%v", err)
	}
	rec := do(t, &p.i18n, "/api/i18n/ja")
	if rec.Code != http.StatusOK {
		t.Fatalf("Apply 后 ja 应可读，实得 %d（%s）", rec.Code, rec.Body.String())
	}

	// 只提交一侧时，另一侧不该被清空。
	if err := p.Apply(map[string]any{optLanguage: "en"}); err != nil {
		t.Fatalf("Apply(language) 失败：%v", err)
	}
	if d, l := p.i18n.snapshot(); d != dir || l != "en" {
		t.Fatalf("只提交 language 不应清空 dir：dir=%q lang=%q", d, l)
	}

	// 服务端默认语言进入清单。
	rec = do(t, &p.i18n, "/api/i18n/")
	var idx i18nIndex
	_ = json.Unmarshal(rec.Body.Bytes(), &idx)
	if idx.Default != "en" {
		t.Fatalf("Apply 后清单默认语言应为 en，实得 %q", idx.Default)
	}

	// 非法语言代码必须报错，而不是静默写入。
	if err := p.Apply(map[string]any{optLanguage: "zh CN"}); err == nil {
		t.Fatal("非法语言代码应报错")
	}
	if err := p.Apply(map[string]any{optI18NDir: 123}); err == nil {
		t.Fatal("非字符串目录应报错")
	}
}

/* ---------- 7. 端点确实被注册（走一次真实 mux） ---------- */

func TestI18NEndpointRegisteredOnMux(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", http.NotFoundHandler()) // 更粗的兜底，验证 /api/i18n/ 优先命中

	p := &WebConsole{}
	mux.Handle(pathI18N, http.HandlerFunc(p.i18n.ServeHTTP))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/i18n/zh-CN", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("经 mux 请求 /api/i18n/zh-CN 应 200，实得 %d", rec.Code)
	}

	// 控制台自己的静态目录不该被误当成语言。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/i18n/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知语言经 mux 应 404，实得 %d", rec.Code)
	}
}
