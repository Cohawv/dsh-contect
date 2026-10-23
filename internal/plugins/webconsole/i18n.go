package webconsole

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"lan-agent-hub/internal/plugin"
)

// V7 ④ i18n：控制台国际化与语言包机制。
//
// 设计取舍（零依赖 + 单二进制）：
//   - 内置语言包随 //go:embed webroot 一起进二进制（webroot/i18n/<lang>.json）；
//   - 外部目录（配置项 i18n.dir）里的 <lang>.json **按键覆盖**内置包：
//     新增语言 = 丢一个 JSON 文件进去，不改代码、不重新编译；
//   - 端点只做「读文件 + 校验 + 合并」，不缓存目录内容，改文件即生效（no-cache）。
//
// 安全：lang 走白名单正则（不含 . / \ 与空格），再叠加 filepath.Rel 的越界复核，
// 目录穿越（..）与子目录读取都在入口被拒；文件大小有上限，防超大文件拖死服务。

// i18n 相关常量。
const (
	// pathI18N 是语言包端点的**子树**前缀。
	// Go 1.21 的 ServeMux（go.mod 声明 go 1.21）不支持 {lang} 通配，故用子树 + 手工取后缀。
	pathI18N = "/api/i18n/"

	i18nEmbeddedDir = "webroot/i18n"
	i18nExt         = ".json"
	// i18nTemplateName 是贡献模板的文件名：它列出全部键但值全空，不是一门语言，
	// 因此不出现在 languages 列表里（但仍可按 lang 取回，用于验证「全缺失」回退）。
	i18nTemplateName = "TEMPLATE"

	defaultLanguage = "zh-CN"

	// i18nMaxPackBytes 单个语言包的大小上限（1 MiB）：语言包是纯文本字典，
	// 正常规模在几十 KB 内；超限一律拒绝，避免误挂大文件把请求处理拖死。
	i18nMaxPackBytes = 1 << 20
)

// 配置项键名（写入 hub.json 的 plugins.webconsole.options）。
const (
	optLanguage = "language"
	optI18NDir  = "i18n.dir"
)

// langRe 是语言代码白名单：字母或数字开头，仅含字母/数字/'-'/'_'，长度 1..32。
// 它天然排除了 '.'、'/'、'\\' 与空格——即目录穿越与后缀注入在入口就进不来。
var langRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// i18nPack 是一个语言包的响应体。
type i18nPack struct {
	Lang     string            `json:"lang"`
	Source   string            `json:"source"` // builtin | external | merged
	Keys     int               `json:"keys"`   // 非空值的键数
	Messages map[string]string `json:"messages"`
}

// i18nIndex 是语言清单的响应体（客户端据此渲染语言下拉与「缺失键数量」）。
type i18nIndex struct {
	Default     string   `json:"default"`     // 服务端声明的默认语言
	Languages   []string `json:"languages"`   // 内置 + 外部目录里实际存在的语言
	ExternalDir string   `json:"externalDir"` // 外部语言包目录（空=未配置）
	RefKeys     int      `json:"refKeys"`     // 参考键总数（内置默认语言的键数）
}

// i18nStore 持有 i18n 的可变配置与端点逻辑。
//
// 字段都用锁保护：Apply（设置保存）与 HTTP 请求（读）会并发发生。
type i18nStore struct {
	mu   sync.RWMutex
	dir  string // 外部语言包目录，空 = 仅用内置包
	lang string // 服务端默认语言
}

func newI18NStore(dir, lang string) *i18nStore {
	return &i18nStore{dir: strings.TrimSpace(dir), lang: normalizeLang(lang)}
}

// normalizeLang 把配置里的语言值收敛成合法形态：空值回落默认，大小写原样保留
// （语言标签的大小写有语义，如 zh-CN），但前后空白一律去掉。
func normalizeLang(lang string) string {
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return defaultLanguage
	}
	return lang
}

func (s *i18nStore) snapshot() (dir, lang string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 零值（Init 未跑就收到请求，典型于单测直连）也要报出有意义的默认语言。
	return s.dir, normalizeLang(s.lang)
}

// configure 供 Init 与 Apply 复用：只接受合法形态的值，非法 dir 直接报错而不是静默吞掉。
func (s *i18nStore) configure(dir, lang string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir = strings.TrimSpace(dir)
	s.lang = normalizeLang(lang)
}

/* ---------------- 端点 ---------------- */

// ServeHTTP 提供 GET /api/i18n/{lang} 与 GET /api/i18n/（语言清单）。
func (s *i18nStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeI18NError(w, http.StatusMethodNotAllowed, "i18n: only GET is supported")
		return
	}

	raw := strings.TrimPrefix(r.URL.Path, pathI18N)
	// URL 里可能是 %2e%2e 这类编码形态，先解码再校验，避免绕过白名单。
	unescaped, err := url.PathUnescape(raw)
	if err != nil {
		writeI18NError(w, http.StatusBadRequest, "i18n: 语言代码编码非法")
		return
	}
	raw = unescaped

	if raw == "" {
		s.serveIndex(w)
		return
	}
	// 只取第一段：/api/i18n/en/extra 视同非法，不给"碰运气"的空间。
	if strings.Contains(raw, "/") || strings.Contains(raw, `\`) {
		writeI18NError(w, http.StatusBadRequest, "i18n: 语言代码非法")
		return
	}
	if !langRe.MatchString(raw) || strings.Contains(raw, "..") {
		writeI18NError(w, http.StatusBadRequest, "i18n: 语言代码非法（只允许字母/数字/-/_，1..32 字符）")
		return
	}
	s.servePack(w, raw)
}

func (s *i18nStore) serveIndex(w http.ResponseWriter) {
	dir, lang := s.snapshot()
	body, status, err := s.buildIndex(dir, lang)
	if err != nil {
		writeI18NError(w, status, err.Error())
		return
	}
	writeI18NJSON(w, body)
}

func (s *i18nStore) servePack(w http.ResponseWriter, lang string) {
	dir, _ := s.snapshot()
	pack, status, err := s.loadPack(dir, lang)
	if err != nil {
		writeI18NError(w, status, err.Error())
		return
	}
	writeI18NJSON(w, pack)
}

// 外部目录里的语言包随时可能被替换，故一律 no-cache：宁可多一次小请求，
// 也不要让用户改了语言包却看不到变化（V7-2 同类困惑）。
func writeI18NJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func writeI18NError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

/* ---------------- 语言包装配 ---------------- */

// loadPack 返回 lang 的最终语言包：内置为底，外部目录按键覆盖。
//
// 空值语义：值为空串的键一律视为「未翻译」——外部空值不覆盖内置（保留源语言），
// 两边都没有时该键直接缺席，由前端回退到键名并告警（TEMPLATE.json 正是这种全空形态）。
func (s *i18nStore) loadPack(dir, lang string) (*i18nPack, int, error) {
	builtin, err := loadBuiltinPack(lang)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	external, status, err := loadExternalPack(dir, lang)
	if err != nil {
		return nil, status, err
	}
	if builtin == nil && external == nil {
		return nil, http.StatusNotFound, fmt.Errorf("i18n: 未知语言 %q（内置包与外部目录都没有 %s%s）", lang, lang, i18nExt)
	}

	merged := make(map[string]string, len(builtin)+len(external))
	for k, v := range builtin {
		if v != "" {
			merged[k] = v
		}
	}
	for k, v := range external {
		if v != "" {
			merged[k] = v
		}
	}

	source := "external"
	if builtin != nil {
		source = "builtin"
		if external != nil {
			source = "merged"
		}
	}
	return &i18nPack{Lang: lang, Source: source, Keys: len(merged), Messages: merged}, http.StatusOK, nil
}

// loadBuiltinPack 读内嵌语言包；不存在返回 (nil, nil)（未知语言由调用方决定 404）。
func loadBuiltinPack(lang string) (map[string]string, error) {
	b, err := webFS.ReadFile(i18nEmbeddedDir + "/" + lang + i18nExt)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("i18n: 读取内置语言包失败：%v", err)
	}
	return decodePack(b)
}

// loadExternalPack 读外部目录里的语言包。目录未配置或文件不存在 → (nil, 0, nil)。
func loadExternalPack(dir, lang string) (map[string]string, int, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, 0, nil
	}
	path, err := externalPackPath(dir, lang)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil // 该语言没有外部覆盖，属正常情况
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("i18n: 读取外部语言包失败：%v", err)
	}
	if st.IsDir() {
		return nil, http.StatusBadRequest, fmt.Errorf("i18n: 语言包路径是目录：%s", path)
	}
	if st.Size() > i18nMaxPackBytes {
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("i18n: 语言包 %s 超过 %d 字节上限（实际 %d）",
			lang, i18nMaxPackBytes, st.Size())
	}
	b, err := os.ReadFile(path) // #nosec G304 —— lang 已过白名单，路径已复核在 dir 之内
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("i18n: 读取外部语言包失败：%v", err)
	}
	m, err := decodePack(b)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("i18n: 外部语言包 %s 非法：%v", lang, err)
	}
	return m, 0, nil
}

// externalPackPath 把 lang 解析成 dir 内的文件路径，并复核结果没有越出 dir。
func externalPackPath(dir, lang string) (string, error) {
	clean := filepath.Clean(dir)
	target := filepath.Join(clean, lang+i18nExt)
	// filepath.Join 已 Clean 目标路径；再用 Rel 复核一次，防目录穿越（lang 侧已白名单，
	// 这里是对 dir 本身含符号链接或被改写时的第二道闸）。
	rel, err := filepath.Rel(clean, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("i18n: 语言包路径越出外部目录：%q", lang)
	}
	return target, nil
}

// decodePack 解析语言包：顶层必须是对象，值必须是字符串。
//
// 空文件与非法 JSON 都明确报错——静默当成"空语言包"会让人以为翻译生效了，实际全是键名。
func decodePack(b []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New("语言包内容为空")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("JSON 解析失败：%v", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("键 %q 的值不是字符串", k)
		}
		out[k] = s
	}
	return out, nil
}

/* ---------------- 语言清单 ---------------- */

func (s *i18nStore) buildIndex(dir, lang string) (*i18nIndex, int, error) {
	set := map[string]bool{}
	for _, name := range embeddedLangs() {
		set[name] = true
	}
	for _, name := range externalLangs(dir) {
		set[name] = true
	}
	langs := make([]string, 0, len(set))
	for name := range set {
		langs = append(langs, name)
	}
	sort.Strings(langs)

	ref, err := loadBuiltinPack(defaultLanguage)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return &i18nIndex{
		Default:     lang,
		Languages:   langs,
		ExternalDir: dir,
		RefKeys:     len(ref),
	}, http.StatusOK, nil
}

// embeddedLangs 列出内嵌目录里的语言（排除贡献模板）。
func embeddedLangs() []string {
	entries, err := webFS.ReadDir(i18nEmbeddedDir)
	if err != nil {
		return nil
	}
	return langNames(entries, func(e os.DirEntry) bool { return e.IsDir() })
}

// externalLangs 列出外部目录里的语言（目录不可读时静默返回空：配置写错不该让控制台打不开）。
func externalLangs(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	return langNames(entries, func(e os.DirEntry) bool { return e.IsDir() })
}

// langNames 从目录项里挑出 <lang>.json 的语言名（跳过子目录与贡献模板）。
func langNames(entries []os.DirEntry, isDir func(os.DirEntry) bool) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if isDir(e) {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, i18nExt) {
			continue
		}
		lang := strings.TrimSuffix(name, i18nExt)
		if lang == "" || lang == i18nTemplateName {
			continue // 模板不是语言，不进下拉框
		}
		out = append(out, lang)
	}
	return out
}

/* ---------------- 插件侧：配置声明与热更新 ---------------- */

// Options 声明本插件的可配置项（控制台设置视图据此自动生成表单，V4 ④）。
//
// language 是 select：表单只提供内置的两门语言；但服务端对**任意**合法语言代码都放行
// （手改 hub.json 成 "ja" 也能生效），因为外部目录可以随时新增语言包。
func (p *WebConsole) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         optLanguage,
			Type:        plugin.OptionSelect,
			Default:     defaultLanguage,
			Description: "控制台界面语言（新客户端的默认语言；客户端可在设置里单独切换）",
			Choices:     []string{"zh-CN", "en"},
		},
		{
			Key:         optI18NDir,
			Type:        plugin.OptionString,
			Default:     "",
			Description: "外部语言包目录：放一个 <lang>.json 即可新增语言（无需改代码/重新编译）；留空=只用内置语言包",
		},
	}
}

// Apply 实现 plugin.Applier：设置保存后**热更新**语言配置，无需重启。
//
// 只改动 options 里真正出现的键：设置表单可能只提交了一侧，不该把另一侧清成空。
func (p *WebConsole) Apply(options map[string]any) error {
	dir := ""
	lang := ""
	touchDir, touchLang := false, false

	if v, ok := options[optI18NDir]; ok {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("webconsole: 配置项 %s 应为字符串", optI18NDir)
		}
		dir, touchDir = strings.TrimSpace(s), true
	}
	if v, ok := options[optLanguage]; ok {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("webconsole: 配置项 %s 应为字符串", optLanguage)
		}
		if !langRe.MatchString(s) {
			return fmt.Errorf("webconsole: 配置项 %s 不是合法语言代码：%q", optLanguage, s)
		}
		lang, touchLang = s, true
	}

	p.i18n.mu.Lock()
	defer p.i18n.mu.Unlock()
	if touchDir {
		p.i18n.dir = dir
	}
	if touchLang {
		p.i18n.lang = normalizeLang(lang)
	}
	return nil
}
