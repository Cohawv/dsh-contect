package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Settings 是「设置」端点返回的完整快照：控制台据此**自动生成**表单。
//
// 字段语义：
//   - Version：磁盘上的配置版本，POST 时必须原样带回（乐观锁）；
//   - Plugins：磁盘上的每插件配置（enabled + options），即表单的初始值；
//   - Schema：各插件 Options() 的声明，决定渲染什么控件；
//   - ConfigPath：配置文件绝对/相对路径，让用户知道改的是哪个文件；
//   - ReadOnly：只读模式（-config-readonly）时为 true，前端据此禁用保存；
//   - Defaults：各插件默认值，供「恢复默认」使用。
type Settings struct {
	Version    int                       `json:"version"`
	Plugins    map[string]PluginConfig   `json:"plugins"`
	Schema     map[string][]OptionDef    `json:"schema"`
	ConfigPath string                    `json:"configPath"`
	ReadOnly   bool                      `json:"readOnly"`
	Defaults   map[string]map[string]any `json:"defaults"`
	// UnknownPlugins 是磁盘上存在、但当前二进制里没有对应实现的插件段（降级/删插件/手工编辑的残留）。
	// 它们被**排除**在 Plugins 之外（不要让人误以为它们在运行），单独列出以便前端提示
	// 「这些段会在下次保存时被清理」。
	UnknownPlugins []string `json:"unknownPlugins"`
	// StaleOptions 是「插件还在、但该 option 键已不再被 OptionDef 声明」的残留（N1），
	// 同样被排除在 Plugins 之外。与 UnknownPlugins 并列：一个是整段废弃，一个是单个键废弃。
	StaleOptions []StaleOption `json:"staleOptions"`
}

// PluginPatch 是 POST /api/settings 中**单个插件的增量补丁**。
//
// 为什么 Enabled 用指针：必须能区分「本次没提交启用状态」与「显式提交 false」。
// 若用普通 bool，局部提交（例如只改 interval）会被 JSON 零值静默解释成「停用该插件」，
// 与「配置项按增量合并」的语义自相矛盾。
//
// 注意：磁盘格式与响应格式仍用 PluginConfig（Enabled 是布尔值），
// 指针只存在于 API 请求边界——绝不让 null 泄漏到 hub.json 或 GET 响应。
type PluginPatch struct {
	Enabled *bool          `json:"enabled,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

// SettingsRequest 是 POST /api/settings 的请求体。
type SettingsRequest struct {
	Version int                    `json:"version"`
	Plugins map[string]PluginPatch `json:"plugins"`
}

// SetConfigPath 设置配置文件路径与只读模式（由装配代码在启动时调用一次）。
func (m *Manager) SetConfigPath(path string, readOnly bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfgPath = path
	m.cfgReadOnly = readOnly
	m.store = NewStore(path)
}

// ConfigPath 返回配置文件路径。
func (m *Manager) ConfigPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfgPath
}

// ConfigReadOnly 返回是否处于只读模式。
func (m *Manager) ConfigReadOnly() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfgReadOnly
}

// Settings 返回设置视图快照（读磁盘；文件缺失时回退到默认模板 + Version=0）。
func (m *Manager) Settings() (Settings, error) {
	cfg, _, err := m.diskConfig()
	if err != nil {
		return Settings{}, err
	}
	schemas := m.Schemas()

	// 磁盘残留分两类，都从 plugins 里摘出来单独列，否则前端会把它们当成"正常但读不到 schema"：
	//   - 整个插件段已不存在 → UnknownPlugins（F2）；
	//   - 插件还在、某个 option 键已不再声明 → StaleOptions（N1）。
	// 摘掉 stale 键后，plugins 里只剩「本二进制真正认识的东西」，界面不会被幽灵字段污染。
	staleOpts := StaleOptionsOf(cfg, schemas, m.names())
	staleByPlugin := make(map[string]map[string]bool, len(staleOpts))
	for _, so := range staleOpts {
		if staleByPlugin[so.Plugin] == nil {
			staleByPlugin[so.Plugin] = make(map[string]bool, 1)
		}
		staleByPlugin[so.Plugin][so.Key] = true
	}

	plugins := make(map[string]PluginConfig, len(cfg.Plugins))
	unknown := make([]string, 0, 2)
	for _, name := range sortedPluginNames(cfg) {
		pc := cfg.Plugins[name]
		if _, ok := m.byName[name]; !ok {
			unknown = append(unknown, name)
			continue
		}
		if drop := staleByPlugin[name]; len(drop) > 0 {
			opts := make(map[string]any, len(pc.Options))
			for k, v := range pc.Options {
				if drop[k] {
					continue
				}
				opts[k] = v
			}
			pc.Options = opts
		}
		plugins[name] = pc
	}

	m.mu.RLock()
	cfgPath, readOnly := m.cfgPath, m.cfgReadOnly
	m.mu.RUnlock()
	return Settings{
		Version:        cfg.Version,
		Plugins:        plugins,
		Schema:         schemas,
		ConfigPath:     cfgPath,
		ReadOnly:       readOnly,
		Defaults:       m.DefaultOptions(),
		UnknownPlugins: unknown,
		StaleOptions:   staleOpts,
	}, nil
}

// diskConfig 读取磁盘配置并按**本 Manager 的**插件集合补齐默认项。
//
// 必须传 m.plugins 而不是全局 All()（F1）：同一进程内可能有多个 Manager（测试典型），
// 全局注册表被别的用例塞过插件时，磁盘基线会凭空多出本 Manager 不认识的段，
// 后续 SaveSettings 就会莫名其妙报「未知插件」。
func (m *Manager) diskConfig() (Config, bool, error) {
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return m.runtimeConfig(0), false, nil
	}
	return store.LoadOrTemplateFor(m.plugins)
}

// runtimeConfig 用内存中的启用状态与选项组成一份配置（无配置文件时使用）。
func (m *Manager) runtimeConfig(version int) Config {
	cfg := Config{Version: version, Plugins: make(map[string]PluginConfig, len(m.plugins))}
	for _, p := range m.plugins {
		name := p.Meta().Name
		cfg.Plugins[name] = PluginConfig{
			Enabled: m.isEnabled(p),
			Options: m.OptionsOf(name),
		}
	}
	return cfg
}

// SaveSettings 执行一次设置保存：校验 → 乐观锁写盘 → 应用配置。
//
// 返回：
//   - Settings：保存后的新快照（新版本号）；
//   - pendingRestart：需要重启才生效的插件名（未实现 Applier，或启用状态与运行期不一致）；
//   - error：ErrConflict（409）/ ErrInvalidConfig（400）/ ErrReadOnly（400）等。
//
// 诚实原则（审查 M6）：能热更新的插件立即生效，不能的明确标注 pendingRestart，
// 绝不假装全部热生效。
func (m *Manager) SaveSettings(req SettingsRequest) (Settings, []string, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	store := m.store
	cfgPath, readOnly := m.cfgPath, m.cfgReadOnly
	m.mu.RUnlock()

	if readOnly {
		return Settings{}, nil, fmt.Errorf("%w: 当前以只读模式启动（-config-readonly），配置不可修改", ErrReadOnly)
	}
	if store == nil || cfgPath == "" {
		return Settings{}, nil, fmt.Errorf("%w: 启动时未指定配置文件", ErrNoConfigPath)
	}

	// 0) 请求体里提交了未知插件 → 严格的 400。
	//    这与「磁盘基线里的未知残留」语义不同：前者是用户显式写错了插件名，
	//    必须当场拒绝（否则会悄悄吞掉他的改动）；后者是历史残留，保存时顺手清理。
	if bad := m.unknownInRequest(req.Plugins); len(bad) > 0 {
		return Settings{}, nil, fmt.Errorf("%w: 未知插件 %q（当前支持：%s）",
			ErrInvalidConfig, bad[0], strings.Join(m.names(), "、"))
	}

	// 0.1) 请求体里提交了未声明的 option key → 同样严格 400（N1 的另一半）。
	//      宽容只适用于「磁盘基线里的废弃键」；用户显式写错的键必须当场拒绝，
	//      否则他的改动会被静默丢弃（比报错更糟）。
	if err := m.validateRequestOptions(req.Plugins); err != nil {
		return Settings{}, nil, err
	}

	// 1) 以磁盘配置为基线，把请求里提交的插件覆盖上去。
	//    这一步避免「局部提交」把其它插件的配置抹掉（curl 手工调用时很容易只发一个插件）。
	disk, _, derr := m.diskConfig()
	if derr != nil {
		return Settings{}, nil, derr
	}
	merged := Config{Version: req.Version, Plugins: make(map[string]PluginConfig, len(disk.Plugins)+len(req.Plugins))}
	for name, pc := range disk.Plugins {
		merged.Plugins[name] = pc
	}
	for name, patch := range req.Plugins {
		base, inDisk := disk.Plugins[name]
		merged.Plugins[name] = m.mergePatch(name, base, inDisk, patch)
	}

	// 2) 校验（未知键 / 类型不符 / 越界）——失败一律不写盘。
	//    用「磁盘宽容版」（ValidateDiskConfig）：只对**磁盘基线**里的历史残留例外
	//    （整段已不存在的插件 F2、已废弃的 option 键 N1），
	//    已知插件的已声明键仍走全套严格校验，非法值照样 400。
	normalized, staleSections, staleOptions, err := ValidateDiskConfig(merged, m.Schemas(), m.names())
	if err != nil {
		return Settings{}, nil, err
	}
	// 必需插件不可在配置里被关掉（否则下次启动必然失败，不如现在就拒绝）。
	for _, p := range m.plugins {
		meta := p.Meta()
		if !meta.Required {
			continue
		}
		if pc, ok := normalized.Plugins[meta.Name]; ok && !pc.Enabled {
			reason := meta.RequiredReason
			if reason == "" {
				reason = "它是必需插件，必须启用"
			}
			return Settings{}, nil, fmt.Errorf("%w: 插件 %q 是必需插件，不能在配置中停用（%s）",
				ErrInvalidConfig, meta.Name, reason)
		}
	}

	// 2) 乐观锁写盘（版本不一致 → ErrConflict，磁盘不被覆盖）。
	if err := store.SaveWithVersion(normalized, req.Version); err != nil {
		if errors.Is(err, ErrConflict) {
			snap, snapErr := m.Settings()
			if snapErr != nil {
				return Settings{}, nil, err
			}
			return snap, nil, err
		}
		return Settings{}, nil, err
	}

	// 3.1) 磁盘残留（未知插件段 / 废弃 option 键）已随本次写盘一起被清掉：逐条记 changelog。
	//
	// 必须记**完整原内容**（N3）：只记名字的话，一次保存后原值就彻底消失在磁盘与 .bak 之外，
	// 「降级 → 升级 → 再降级」的配置会永久丢失且无痕迹可查。
	for _, name := range staleSections {
		_ = store.AppendChangelog(fmt.Sprintf("%s removed unknown plugin section: %s = %s",
			time.Now().Format(time.RFC3339), name, jsonOneLine(disk.Plugins[name])))
	}
	for _, so := range staleOptions {
		orig := disk.Plugins[so.Plugin].Options[so.Key]
		_ = store.AppendChangelog(fmt.Sprintf("%s removed stale option: %s.%s = %s",
			time.Now().Format(time.RFC3339), so.Plugin, so.Key, jsonOneLine(orig)))
	}

	// 4) 应用：内存配置整体同步；但 pendingRestart 只针对**本次请求真正提交的插件**
	//    （否则磁盘里那些「CLI 覆盖过的默认值」会一直误报为待重启）。
	touched := make([]string, 0, len(req.Plugins))
	for name := range req.Plugins {
		if _, ok := m.byName[name]; ok {
			touched = append(touched, name)
		}
	}
	sort.Strings(touched)

	// 未提交的插件：只同步内存，不做 pending 判定。
	for name, pc := range normalized.Plugins {
		if _, inReq := req.Plugins[name]; inReq {
			continue
		}
		m.SetOptions(name, pc.Options)
	}

	pending := make([]string, 0, len(touched))
	for _, name := range touched {
		pc := normalized.Plugins[name]
		p := m.byName[name]
		current := m.OptionsOf(name)

		// 只把「本次请求真的提交了、且有效值与运行期不同」的键交给 Applier。
		// 传全量会让 CLI 覆盖值与文件值的固有差异（如 -discover :50130 vs 文件 :8082）
		// 被误判成「需要重启」（假阳性），从而把只改 interval 的保存也标成待重启。
		submitted := req.Plugins[name].Options
		changed := make(map[string]any, len(submitted))
		for k := range submitted {
			v, ok := pc.Options[k]
			if !ok {
				continue
			}
			if fmt.Sprint(current[k]) != fmt.Sprint(v) {
				changed[k] = v
			}
		}

		m.SetOptions(name, pc.Options)

		// 启用状态与运行期不一致 → 只有重启（或插件视图里显式启停）才真正改变「有没有在跑」。
		if pc.Enabled != m.isEnabled(p) {
			pending = append(pending, name)
		}
		if len(changed) == 0 {
			continue // 没有实际变化的键，什么都不必做（尤其不要无意义地重建内部循环）
		}
		applier, supports := p.(Applier)
		if !supports {
			pending = append(pending, name)
			continue
		}
		if applyErr := applier.Apply(changed); applyErr != nil {
			pending = append(pending, name)
			m.logRuntime("apply-failed", name, []string{applyErr.Error()})
		}
	}

	// 去重 + 排序，保证响应稳定（便于 golden 断言）。
	seen := make(map[string]bool, len(pending))
	uniq := pending[:0]
	for _, name := range pending {
		if seen[name] {
			continue
		}
		seen[name] = true
		uniq = append(uniq, name)
	}
	pending = uniq
	sort.Strings(pending)

	snap, snapErr := m.Settings()
	if snapErr != nil {
		return Settings{}, pending, snapErr
	}
	return snap, pending, nil
}

// unknownInRequest 返回请求体里提交了、但当前 Manager 不认识的插件名（升序）。
//
// 这是用户的显式错误（写错了插件名），必须当场 400——与「磁盘基线里的历史残留」
// 的宽容处理区分开：前者若忽略会静默吞掉用户的改动，后者若拒绝会让人无法自救。
func (m *Manager) unknownInRequest(plugins map[string]PluginPatch) []string {
	var bad []string
	for name := range plugins {
		if _, ok := m.byName[name]; !ok {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	return bad
}

// validateRequestOptions 严格校验**请求体**里提交的 option 键名（N1 的另一半）。
//
// 与 ValidateDiskConfig 的宽容形成对照：
//   - 磁盘基线里的未声明键 = 历史残留 → 剔除 + 留痕（用户无法通过 UI 修，必须宽容）；
//   - 请求体里的未声明键 = 用户显式写错 → 400（若静默丢弃，用户的改动会消失且无提示）。
func (m *Manager) validateRequestOptions(plugins map[string]PluginPatch) error {
	schemas := m.Schemas()
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, ok := m.byName[name]; !ok {
			continue // 未知插件名已由 unknownInRequest 拦下
		}
		declared := declaredKeys(schemas[name])
		supported := make([]string, 0, len(declared))
		for k := range declared {
			supported = append(supported, k)
		}
		sort.Strings(supported)

		for _, k := range sortedOptionKeys(plugins[name].Options) {
			if declared[k] {
				continue
			}
			if len(supported) == 0 {
				return fmt.Errorf("%w: 插件 %q 不接受任何配置项，收到未知配置项 %q",
					ErrInvalidConfig, name, k)
			}
			return fmt.Errorf("%w: 插件 %q: 未知配置项 %q（当前支持：%s）",
				ErrInvalidConfig, name, k, strings.Join(supported, "、"))
		}
	}
	return nil
}

// mergePatch 把请求里的插件补丁叠加到磁盘基线上，返回合并后的完整 PluginConfig。
//
// 两层增量语义（缺一不可）：
//   - 启用状态：patch.Enabled == nil 表示「本次不改」，沿用磁盘值；
//     仅当请求显式提交 true/false 时才覆盖（否则局部提交会静默停用插件）；
//   - 配置项：只覆盖 patch.Options 里出现的键，其余键沿用磁盘值
//     （否则「只改 interval」会把同插件的 bindAddr/hubName 抹成零值）。
func (m *Manager) mergePatch(name string, base PluginConfig, inDisk bool, patch PluginPatch) PluginConfig {
	enabled := base.Enabled
	if !inDisk {
		// 磁盘上还没有这个插件（新增插件首次提交）：先按元信息的 DefaultOn 取默认值。
		enabled = true
		if p, ok := m.byName[name]; ok {
			enabled = p.Meta().DefaultOn
		}
	}
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}

	opts := make(map[string]any, len(base.Options)+len(patch.Options))
	if !inDisk {
		// 磁盘上还没这个插件（例如刚升级、插件是新加的）：以声明默认值为基线，
		// 免得第一次局部提交只写进用户改的那一个键、其余键在磁盘上凭空缺失。
		for k, v := range m.DefaultOptions()[name] {
			opts[k] = v
		}
	}
	for k, v := range base.Options {
		opts[k] = v
	}
	for k, v := range patch.Options {
		opts[k] = v
	}
	return PluginConfig{Enabled: enabled, Options: opts}
}

// optionsEqual 判断两份选项是否等价（按 key 集合 + 字符串化取值比对，
// 兼容 JSON 反序列化带来的 float64/int 差异）。
func optionsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			return false
		}
	}
	return true
}

// logRuntime 把运行期操作（启停/热更新失败）追加到 hub.json.changelog，保证可追溯（小建议 8）。
// 只读模式或未配置文件时静默跳过——日志是尽力而为的旁路，绝不影响主流程。
func (m *Manager) logRuntime(action, name string, detail []string) {
	m.mu.RLock()
	store, readOnly := m.store, m.cfgReadOnly
	m.mu.RUnlock()
	if store == nil || readOnly {
		return
	}
	line := fmt.Sprintf("%s runtime %s %s", time.Now().Format(time.RFC3339), action, name)
	if len(detail) > 0 {
		line += " [" + joinComma(detail) + "]"
	}
	_ = store.AppendChangelog(line)
}

// joinComma 用逗号连接字符串（避免为一个 join 引入额外依赖表达）。
func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
