package plugin

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 配置文件相关的文件名后缀。
const (
	// BakSuffix 是写盘前的自动备份后缀（主文件损坏时自动回退到它）。
	BakSuffix = ".bak"
	// ChangelogSuffix 是人可读变更日志后缀（时间戳 + 变更键），排障时很省事。
	ChangelogSuffix = ".changelog"
)

// PluginConfig 是 hub.json 中单个插件的配置段。
type PluginConfig struct {
	Enabled bool           `json:"enabled"` // 是否启用（文件级覆盖 DefaultOn）
	Options map[string]any `json:"options"` // 传给插件 Context 的自定义配置项
}

// Config 是 hub.json 的根结构。
//
// Version 是乐观锁版本号（审查 M4）：写入前必须与磁盘版本一致，否则返回 ErrConflict，
// 从而避免「两个客户端同时保存，后者整份覆盖前者」的丢更新。
type Config struct {
	Version int                     `json:"version"`
	Plugins map[string]PluginConfig `json:"plugins"`
}

// HubConfig 是历史名（V2 引入），保留为 Config 的别名以免破坏既有调用方。
type HubConfig = Config

// Clone 深拷贝配置（避免共享 map 被并发改写）。
func (c Config) Clone() Config {
	out := Config{Version: c.Version, Plugins: make(map[string]PluginConfig, len(c.Plugins))}
	for name, pc := range c.Plugins {
		opts := make(map[string]any, len(pc.Options))
		for k, v := range pc.Options {
			opts[k] = v
		}
		out.Plugins[name] = PluginConfig{Enabled: pc.Enabled, Options: opts}
	}
	return out
}

// MergeDefaults 用插件默认值补齐配置中缺失的插件段与缺失的选项键（不覆盖用户显式值）。
func (c Config) MergeDefaults(plugins []Plugin) Config {
	out := c.Clone()
	defaults := DefaultConfig(plugins)
	for name, dpc := range defaults.Plugins {
		cur, ok := out.Plugins[name]
		if !ok {
			out.Plugins[name] = dpc
			continue
		}
		if cur.Options == nil {
			cur.Options = map[string]any{}
		}
		for k, v := range dpc.Options {
			if _, exists := cur.Options[k]; !exists {
				cur.Options[k] = v
			}
		}
		out.Plugins[name] = cur
	}
	return out
}

// DefaultConfig 生成带全部插件默认项的配置模板（首次运行时落盘，审查建议 M4-小建议）。
// Version 为 0：表示「磁盘上还不存在」，首次保存会写成 1。
func DefaultConfig(plugins []Plugin) Config {
	cfg := Config{Version: 0, Plugins: make(map[string]PluginConfig, len(plugins))}
	for _, p := range plugins {
		name := p.Meta().Name
		opts := map[string]any{}
		if o, ok := p.(Optioner); ok {
			for _, d := range o.Options() {
				if d.Default != nil {
					opts[d.Key] = d.Default
				}
			}
		}
		cfg.Plugins[name] = PluginConfig{Enabled: p.Meta().DefaultOn, Options: opts}
	}
	return cfg
}

// LoadConfig 加载 hub.json 并合并每个插件的启用状态与配置项。
//
// 合并优先级（高 → 低）：CLI 显式开关 (cliEnabled) > 文件 (hub.json) > Meta.DefaultOn。
// 本函数只做「读 + 合并」，不做校验与写回（写回见 Store / Manager.SaveSettings）。
//
// 返回值：
//   - enabled：Name → 最终启用状态；
//   - options：Name → 合并后的配置项（无文件配置时为空 map，便于 Context 读取）；
//   - err：文件不存在视为「全部走默认」且不报错；文件损坏才返回错误。
func LoadConfig(path string, cliEnabled map[string]bool) (enabled map[string]bool, options map[string]map[string]any, err error) {
	enabled = make(map[string]bool)
	options = make(map[string]map[string]any)

	fileCfg := Config{}
	if strings.TrimSpace(path) != "" {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return enabled, options, fmt.Errorf("read config %q: %w", path, readErr)
			}
		} else if len(data) > 0 {
			if uerr := json.Unmarshal(data, &fileCfg); uerr != nil {
				return enabled, options, fmt.Errorf("parse config %q: %w", path, uerr)
			}
		}
	}

	// 遍历全局注册表（编译期已知），确定每个插件的启用状态与选项。
	for _, p := range All() {
		name := p.Meta().Name
		on := p.Meta().DefaultOn
		opts := map[string]any{}

		if pc, ok := fileCfg.Plugins[name]; ok {
			on = pc.Enabled
			if pc.Options != nil {
				opts = pc.Options
			}
		}
		if v, ok := cliEnabled[name]; ok {
			on = v
		}

		enabled[name] = on
		options[name] = opts
	}

	return enabled, options, nil
}

// ---------------------------------------------------------------------------
// 配置校验（写入前必须过这一关：非法配置返回 400 且不写盘）
// ---------------------------------------------------------------------------

// ValidateOptions 按 OptionDef 校验并规范化一组配置项。
//
// 拒绝的情形（一律返回包装了 ErrInvalidConfig 的错误）：
//   - 未知键；
//   - 类型不符（string/bool/int/duration/select）；
//   - select 值不在 Choices 内；
//   - int 越界（Min/Max 非 0 时生效）；
//   - duration 无法被 time.ParseDuration 解析或非正数。
func ValidateOptions(defs []OptionDef, in map[string]any) (map[string]any, error) {
	known := make(map[string]OptionDef, len(defs))
	for _, d := range defs {
		known[d.Key] = d
	}
	out := make(map[string]any, len(in))
	// 按键排序遍历，保证错误文案稳定（便于测试断言与用户复现）。
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d, ok := known[k]
		if !ok {
			return nil, fmt.Errorf("%w: 未知配置项 %q", ErrInvalidConfig, k)
		}
		nv, err := validateOptionValue(d, in[k])
		if err != nil {
			return nil, err
		}
		out[k] = nv
	}
	return out, nil
}

// ValidateOptionsOnly 只校验不保留结果（便于调用方判断「这份配置能不能存」）。
func ValidateOptionsOnly(defs []OptionDef, in map[string]any) error {
	_, err := ValidateOptions(defs, in)
	return err
}

// validateOptionValue 校验单个配置项并返回规范化后的值。
func validateOptionValue(d OptionDef, v any) (any, error) {
	switch d.Type {
	case OptionString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: 配置项 %q 应为字符串", ErrInvalidConfig, d.Key)
		}
		return s, nil

	case OptionBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%w: 配置项 %q 应为布尔值", ErrInvalidConfig, d.Key)
		}
		return b, nil

	case OptionInt:
		n, err := toInt(v)
		if err != nil {
			return nil, fmt.Errorf("%w: 配置项 %q 应为整数", ErrInvalidConfig, d.Key)
		}
		if d.Min != 0 && n < d.Min {
			return nil, fmt.Errorf("%w: 配置项 %q 不能小于 %d（当前 %d）", ErrInvalidConfig, d.Key, d.Min, n)
		}
		if d.Max != 0 && n > d.Max {
			return nil, fmt.Errorf("%w: 配置项 %q 不能大于 %d（当前 %d）", ErrInvalidConfig, d.Key, d.Max, n)
		}
		return n, nil

	case OptionDuration:
		switch raw := v.(type) {
		case string:
			dur, err := time.ParseDuration(strings.TrimSpace(raw))
			if err != nil {
				return nil, fmt.Errorf("%w: 配置项 %q 不是合法时长（示例 5s、1m、500ms）", ErrInvalidConfig, d.Key)
			}
			if dur <= 0 {
				return nil, fmt.Errorf("%w: 配置项 %q 必须为正时长（当前 %s）", ErrInvalidConfig, d.Key, raw)
			}
			return dur.String(), nil
		default:
			// 兼容数字写法：按毫秒解释（与 Context.Duration 的语义一致）
			n, err := toInt(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%w: 配置项 %q 不是合法时长（示例 5s、1m、500ms）", ErrInvalidConfig, d.Key)
			}
			return (time.Duration(n) * time.Millisecond).String(), nil
		}

	case OptionSelect:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: 配置项 %q 应为字符串", ErrInvalidConfig, d.Key)
		}
		for _, c := range d.Choices {
			if c == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%w: 配置项 %q 的值 %q 不在可选范围内（%s）",
			ErrInvalidConfig, d.Key, s, strings.Join(d.Choices, " / "))

	default:
		return nil, fmt.Errorf("%w: 配置项 %q 声明了未知类型 %q", ErrInvalidConfig, d.Key, string(d.Type))
	}
}

// toInt 把 JSON 反序列化得到的各种数字形态统一成 int。
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("not an integer: %v", n)
		}
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, err
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}

// StaleOption 描述一个「插件本身还在、但该 option key 已不在当前 OptionDef 声明里」的
// 磁盘残留项（典型来源：升级后删掉/重命名了某个 OptionDef，或手工改文件时拼错键）。
//
// 它与 unknownPlugins 是同一个失效模式的两半：都只在磁盘上有、都不该让整单保存永久 400，
// 也都应该在保存时被顺手清理。用独立类型而不是拼接字符串，是为了让前端能结构化展示。
type StaleOption struct {
	Plugin string `json:"plugin"`
	Key    string `json:"key"`
}

// sortedPluginNames 返回配置里全部插件名（升序）。
func sortedPluginNames(cfg Config) []string {
	names := make([]string, 0, len(cfg.Plugins))
	for name := range cfg.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedOptionKeys 返回某插件配置里的全部 option 键（升序）。
func sortedOptionKeys(opts map[string]any) []string {
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// declaredKeys 把 OptionDef 列表压成「已声明键」集合。
func declaredKeys(defs []OptionDef) map[string]bool {
	out := make(map[string]bool, len(defs))
	for _, d := range defs {
		out[d.Key] = true
	}
	return out
}

// StaleOptionsOf 列出 cfg 中「已知插件」的未声明 option key（按插件名、键名升序）。
//
// 未知插件段不在这里——它们整个段都归 UnknownPlugins，语义上不拆到键级别。
// 供 GET /api/settings 如实报告「磁盘上有哪些废弃项会被下次保存清掉」。
func StaleOptionsOf(cfg Config, schemas map[string][]OptionDef, known []string) []StaleOption {
	knownSet := make(map[string]bool, len(known))
	for _, n := range known {
		knownSet[n] = true
	}
	out := make([]StaleOption, 0, 4)
	for _, name := range sortedPluginNames(cfg) {
		if !knownSet[name] {
			continue
		}
		declared := declaredKeys(schemas[name])
		for _, k := range sortedOptionKeys(cfg.Plugins[name].Options) {
			if !declared[k] {
				out = append(out, StaleOption{Plugin: name, Key: k})
			}
		}
	}
	return out
}

// ValidateDiskConfig 校验一份**磁盘基线**配置，返回规范化结果与其中的历史残留。
//
// 严格性的两个来源（缺一不可）：
//  1. 已知插件的**已声明键**走 ValidateOptions 全严格校验（类型 / 时长 / 范围 / select）——
//     非法值仍然整单 400，绝不静默改写成默认值；
//  2. 请求体里的未知插件名由调用方先用 unknownInRequest 拦下并 400（见 Manager.SaveSettings）。
//
// 宽容性只有一处例外，且只针对**磁盘**（不是请求体）：
//   - 整个插件段已不存在于本二进制 → 跳过并计入 unknown，保存时清理（F2）；
//   - 插件还在、但某个 option 键不再被声明 → 剔除该键并计入 stale，保存时清理（N1）。
//
// 为什么必须宽容：这两种残留都来自「版本演进 / 手工编辑」，用户自己无法通过控制台修正
// （表单只渲染 schema 里声明的键）。若整单拒绝，就是"配置坏了却永远改不回来"。
//
// 历史说明：本包曾有一个严格的 ValidateConfig，但全仓零调用方，已删除；
// 请求体的未知插件名由 unknownInRequest 负责，不要误以为这里会兜住。
func ValidateDiskConfig(cfg Config, schemas map[string][]OptionDef, known []string) (Config, []string, []StaleOption, error) {
	knownSet := make(map[string]bool, len(known))
	for _, n := range known {
		knownSet[n] = true
	}
	out := Config{Version: cfg.Version, Plugins: make(map[string]PluginConfig, len(cfg.Plugins))}
	unknown := make([]string, 0, 2)
	stale := make([]StaleOption, 0, 4)

	for _, name := range sortedPluginNames(cfg) {
		if !knownSet[name] {
			unknown = append(unknown, name)
			continue
		}
		pc := cfg.Plugins[name]
		defs := schemas[name]
		declared := declaredKeys(defs)

		// 剔除未声明的键（记为 stale），其余键原样交给严格执行校验。
		opts := make(map[string]any, len(pc.Options))
		for _, k := range sortedOptionKeys(pc.Options) {
			if !declared[k] {
				stale = append(stale, StaleOption{Plugin: name, Key: k})
				continue
			}
			opts[k] = pc.Options[k]
		}
		normalized, err := ValidateOptions(defs, opts)
		if err != nil {
			return Config{}, nil, nil, fmt.Errorf("插件 %q: %w", name, err)
		}
		out.Plugins[name] = PluginConfig{Enabled: pc.Enabled, Options: normalized}
	}
	return out, unknown, stale, nil
}

// ---------------------------------------------------------------------------
// 配置文件存储（原子写 + 备份 + 乐观锁 + 变更日志）
// ---------------------------------------------------------------------------

// Store 是 hub.json 的读写门面。
//
// 安全保证：
//   - 原子写：先写同目录临时文件再 rename，进程中途被 kill 也不会留下半截 JSON；
//   - 自动备份：写前把旧文件复制为 hub.json.bak，主文件损坏时自动回退；
//   - 乐观锁：写前比对版本号，不一致返回 ErrConflict（不覆盖他人改动）。
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore 构造配置文件存储。
func NewStore(path string) *Store { return &Store{path: path} }

// Path 返回配置文件路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load 读取配置；文件不存在时返回带全部默认项的模板（Version=0）。
func (s *Store) Load() (Config, error) {
	cfg, _, err := s.LoadOrTemplate()
	return cfg, err
}

// LoadOrTemplate 读取配置，并告知文件是否已存在。
// 模板基于**全局注册表**（All()）——供 cmd/hub 这类「确实要全量清单」的场景使用。
//
// 注意：Manager 请改用 LoadOrTemplateFor(m.plugins)，否则测试里注入的局部插件集合
// 会与全局注册表不一致（全局注册表被别的测试塞过插件时，磁盘基线会凭空多出
// 本 Manager 不认识的插件段，导致 SaveSettings 报「未知插件」）。
func (s *Store) LoadOrTemplate() (cfg Config, existed bool, err error) {
	return s.LoadOrTemplateFor(All())
}

// LoadOrTemplateFor 以**显式插件集合**为模板读取配置，并告知文件是否已存在。
//
// 文件不存在：返回 DefaultConfig(plugins) + existed=false（Version=0）；
// 文件存在：返回 MergeDefaults(plugins) 后的配置（用声明默认值补齐缺失段/键）。
//
// 传显式集合而非全局 All() 是消除「Manager ↔ 全局注册表」隐式耦合的关键：
// 同一进程内多个 Manager（典型是测试）各自持有自己的插件集，互不污染。
func (s *Store) LoadOrTemplateFor(plugins []Plugin) (cfg Config, existed bool, err error) {
	disk, existed, err := s.readDisk()
	if err != nil {
		return Config{}, existed, err
	}
	if !existed {
		return DefaultConfig(plugins), false, nil
	}
	return disk.MergeDefaults(plugins), true, nil
}

// Save 以 cfg.Version 作为期望版本保存（乐观锁）。
func (s *Store) Save(cfg Config) error {
	return s.SaveWithVersion(cfg, cfg.Version)
}

// SaveWithVersion 以 expectVersion 作为期望版本保存；与磁盘不一致时返回 ErrConflict。
// 写入成功后磁盘版本自动 +1（配置版本单调递增，便于追溯）。
func (s *Store) SaveWithVersion(cfg Config, expectVersion int) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("%w: 未指定配置文件路径", ErrNoConfigPath)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	disk, existed, err := s.readDisk()
	if err != nil {
		return err
	}
	diskVersion := 0
	if existed {
		diskVersion = disk.Version
	}
	if expectVersion != diskVersion {
		return fmt.Errorf("%w: 请求版本 %d，磁盘版本 %d", ErrConflict, expectVersion, diskVersion)
	}

	next := cfg.Clone()
	next.Version = diskVersion + 1
	if werr := writeFileAtomic(s.path, next); werr != nil {
		return werr
	}
	if existed {
		s.appendChangelog(disk, next)
	}
	return nil
}

// RestoreFromBackup 用 hub.json.bak 覆盖主文件（主文件损坏时的显式恢复入口）。
func (s *Store) RestoreFromBackup() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("%w: 未指定配置文件路径", ErrNoConfigPath)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path + BakSuffix)
	if err != nil {
		return fmt.Errorf("read backup %q: %w", s.path+BakSuffix, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse backup %q: %w", s.path+BakSuffix, err)
	}
	return writeFileAtomic(s.path, cfg)
}

// AppendChangelog 追加一行人可读的变更记录（运行期启停等非配置写操作也记一笔）。
func (s *Store) AppendChangelog(line string) error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("%w: 未指定配置文件路径", ErrNoConfigPath)
	}
	f, err := os.OpenFile(s.path+ChangelogSuffix, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open changelog: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("write changelog: %w", err)
	}
	return nil
}

// readDisk 读磁盘配置；返回 existed 表示文件是否存在。
//
// 主文件解析失败时自动回退到 hub.json.bak（「写入中断」场景的兜底）。
// 回退**必须可运维**（F3）：日志说明被回退的文件、.bak 的版本、坏文件副本路径，
// 并把 .bak 原子写回主文件——否则磁盘上会一直留着半截 JSON，等下一次成功写入才覆盖，
// 运维完全不知道自己被回退过，下次启动还要再走一遍回退路径。
func (s *Store) readDisk() (cfg Config, existed bool, err error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return Config{}, false, nil
	}
	data, readErr := os.ReadFile(s.path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("read config %q: %w", s.path, readErr)
	}
	if uerr := json.Unmarshal(data, &cfg); uerr != nil {
		bakPath := s.path + BakSuffix
		if bak, bakErr := os.ReadFile(bakPath); bakErr == nil {
			var bcfg Config
			if json.Unmarshal(bak, &bcfg) == nil {
				if bcfg.Plugins == nil {
					bcfg.Plugins = map[string]PluginConfig{}
				}
				s.recoverFromBackup(data, bcfg, uerr)
				return bcfg, true, nil
			}
		}
		return Config{}, true, fmt.Errorf("parse config %q: %w", s.path, uerr)
	}
	if cfg.Plugins == nil {
		cfg.Plugins = map[string]PluginConfig{}
	}
	return cfg, true, nil
}

// corruptSuffix 是主文件损坏时保留的副本后缀（供人工比对，不参与任何自动逻辑）。
const corruptSuffix = ".corrupt"

// recoverFromBackup 在「主文件坏了、.bak 可用」时做三件事：留证据、写日志、修主文件。
//
// 写入主文件时**不能再备份**：否则 writeFileAtomic 会把坏内容写进 .bak，把唯一的
// 好副本覆盖掉，下次回退就彻底没救了。
func (s *Store) recoverFromBackup(corrupt []byte, good Config, parseErr error) {
	corruptPath := s.path + corruptSuffix
	if werr := os.WriteFile(corruptPath, corrupt, 0o644); werr != nil {
		log.Printf("配置恢复：保留损坏副本失败（%s）：%v", corruptPath, werr)
		corruptPath = "(未保留)"
	}
	log.Printf("配置恢复：%q 解析失败（%v），已回退到 %q（version=%d），损坏副本: %s",
		s.path, parseErr, s.path+BakSuffix, good.Version, corruptPath)

	if werr := writeFileAtomicNoBackup(s.path, good); werr != nil {
		log.Printf("配置恢复：把 .bak 写回主文件失败（下次启动会再次回退）：%v", werr)
		return
	}
	log.Printf("配置恢复：已用 .bak 原子修复主文件 %q（version=%d），下次启动不再回退", s.path, good.Version)
	_ = s.AppendChangelog(fmt.Sprintf("%s recovered config from backup (version=%d, corrupt copy=%s)",
		time.Now().Format(time.RFC3339), good.Version, corruptPath))
}

// appendChangelog 记录一次配置写入的差异（人可读时间戳 + 变更键）。
func (s *Store) appendChangelog(oldCfg, newCfg Config) {
	line := fmt.Sprintf("%s config version %d→%d %s",
		time.Now().Format(time.RFC3339), oldCfg.Version, newCfg.Version, describeConfigDiff(oldCfg, newCfg))
	_ = s.AppendChangelog(line)
}

// jsonOneLine 把任意值压成单行 JSON（用于 changelog：清理前留存原内容）。
// 序列化失败时退回 %v 表示——changelog 是尽力而为的旁路，绝不因此阻断主流程。
func jsonOneLine(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(data)
}

// describeConfigDiff 生成配置差异摘要（稳定顺序）。
func describeConfigDiff(oldCfg, newCfg Config) string {
	var changes []string
	names := make(map[string]bool, len(oldCfg.Plugins)+len(newCfg.Plugins))
	for n := range oldCfg.Plugins {
		names[n] = true
	}
	for n := range newCfg.Plugins {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		oldPC, oldOK := oldCfg.Plugins[name]
		newPC, newOK := newCfg.Plugins[name]
		if oldOK != newOK {
			// 整段新增/移除：写清方向，别让人对着 "name.*" 猜。
			verb := "added"
			if !newOK {
				verb = "removed"
			}
			changes = append(changes, fmt.Sprintf("%s=(%s)", name, verb))
			continue
		}
		if oldPC.Enabled != newPC.Enabled {
			changes = append(changes, fmt.Sprintf("%s.enabled=%t", name, newPC.Enabled))
		}
		keys := make(map[string]bool, len(oldPC.Options)+len(newPC.Options))
		for k := range oldPC.Options {
			keys[k] = true
		}
		for k := range newPC.Options {
			keys[k] = true
		}
		optKeys := make([]string, 0, len(keys))
		for k := range keys {
			optKeys = append(optKeys, k)
		}
		sort.Strings(optKeys)
		for _, k := range optKeys {
			ov, oExists := oldPC.Options[k]
			nv, nExists := newPC.Options[k]
			switch {
			case oExists && !nExists:
				// 被清理掉的键：写 (removed) 而不是 "<nil>"，否则读日志的人会以为值被改成了空。
				changes = append(changes, fmt.Sprintf("%s.options.%s=(removed)", name, k))
			case fmt.Sprint(ov) != fmt.Sprint(nv):
				changes = append(changes, fmt.Sprintf("%s.options.%s=%v", name, k, nv))
			}
		}
	}
	if len(changes) == 0 {
		return "(无字段变化，仅版本递增)"
	}
	return "changed: " + strings.Join(changes, ", ")
}

// writeFileAtomic 把配置原子写入 path：临时文件 → fsync → rename，并把旧文件留一份 .bak。
func writeFileAtomic(path string, cfg Config) error {
	return writeConfigFile(path, cfg, true)
}

// writeFileAtomicNoBackup 与 writeFileAtomic 相同，但**不**把旧文件写成 .bak。
// 仅用于「用 .bak 修复主文件」这一场景：此时旧文件正是坏文件，
// 备份它会覆盖掉唯一的好副本。
func writeFileAtomicNoBackup(path string, cfg Config) error {
	return writeConfigFile(path, cfg, false)
}

// writeConfigFile 是原子写实现；backup 控制是否把旧文件留成 .bak。
func writeConfigFile(path string, cfg Config, backup bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %q 失败: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	// 写前备份旧文件（失败不阻断：首次写入本就没有旧文件）。
	if backup {
		if old, rerr := os.ReadFile(path); rerr == nil {
			_ = os.WriteFile(path+BakSuffix, old, 0o644)
		}
	}

	tmp, err := os.CreateTemp(dir, ".hub-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("写入临时配置文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("同步临时配置文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("替换配置文件 %q 失败: %w", path, err)
	}
	return nil
}
