package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSettingsManager 构造一个带配置文件的管理器（含一个声明 Options 的插件）。
func newSettingsManager(t *testing.T, readOnly bool) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}
	m := newManagerFrom([]Plugin{rich})
	m.SetConfigPath(path, readOnly)
	m.ApplyConfig(map[string]bool{"rich": true}, map[string]map[string]any{"rich": {}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	return m, path
}

// boolPtr 便于构造 PluginPatch.Enabled（*bool：nil 表示「本次不改启用状态」）。
func boolPtr(b bool) *bool { return &b }

// validSettings 返回一份合法配置载荷（version 由调用方填）。
func validSettings(version int) SettingsRequest {
	return SettingsRequest{
		Version: version,
		Plugins: map[string]PluginPatch{
			"rich": {
				Enabled: boolPtr(true),
				Options: map[string]any{
					"label":    "hi",
					"count":    5,
					"enabled":  false,
					"interval": "45s",
					"mode":     "slow",
				},
			},
		},
	}
}

// TestStoreOptimisticLock 验证乐观锁：两次同版本保存，第二次 ErrConflict 且磁盘未被覆盖。
func TestStoreOptimisticLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	store := NewStore(path)

	tpl := DefaultConfig(All())
	if err := store.SaveWithVersion(tpl, 0); err != nil {
		t.Fatalf("首次写盘: %v", err)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("首次写盘后版本 = %d want 1", cfg.Version)
	}

	first := Config{Version: 1, Plugins: map[string]PluginConfig{
		"a": {Enabled: true, Options: map[string]any{"k": "first"}},
	}}
	if err := store.SaveWithVersion(first, 1); err != nil {
		t.Fatalf("第一次保存: %v", err)
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 第二个客户端仍拿着版本 1：必须被拒绝，且磁盘内容不变（不丢更新）。
	second := Config{Version: 1, Plugins: map[string]PluginConfig{
		"a": {Enabled: false, Options: map[string]any{"k": "second"}},
	}}
	err = store.SaveWithVersion(second, 1)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("第二次保存应返回 ErrConflict，实际 %v", err)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterFirst) != string(afterSecond) {
		t.Fatal("冲突写入不应改变磁盘内容（发生了覆盖丢失）")
	}
}

// TestStoreBackupFallbackAndChangelog 验证：.bak 自动回退 + changelog 记录变更键。
func TestStoreBackupFallbackAndChangelog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	store := NewStore(path)

	if err := store.SaveWithVersion(Config{Version: 0, Plugins: map[string]PluginConfig{
		"discovery": {Enabled: true, Options: map[string]any{"interval": "5s"}},
	}}, 0); err != nil {
		t.Fatalf("第一次写盘: %v", err)
	}
	// 第二次写盘：会把上一份备份为 hub.json.bak，并追加 changelog。
	if err := store.SaveWithVersion(Config{Version: 1, Plugins: map[string]PluginConfig{
		"discovery": {Enabled: true, Options: map[string]any{"interval": "1m"}},
	}}, 1); err != nil {
		t.Fatalf("第二次写盘: %v", err)
	}
	if _, err := os.Stat(path + BakSuffix); err != nil {
		t.Fatalf("写盘前应生成 %s: %v", BakSuffix, err)
	}
	changelog, err := os.ReadFile(path + ChangelogSuffix)
	if err != nil {
		t.Fatalf("应生成 changelog: %v", err)
	}
	if !strings.Contains(string(changelog), "config version 1→2") ||
		!strings.Contains(string(changelog), "discovery.options.interval") {
		t.Fatalf("changelog 内容不符（应含版本变化与变更键）:\n%s", changelog)
	}

	// 模拟「写入中断」：把主文件写成半截 JSON，加载应自动回退到 .bak。
	if err := os.WriteFile(path, []byte(`{"version":2,"plugins":{"disc`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("主文件损坏时应自动回退 .bak，实际报错: %v", err)
	}
	if got := cfg.Plugins["discovery"].Options["interval"]; got != "5s" {
		t.Fatalf(".bak 回退内容 = %v want 5s（应为上一份有效配置）", got)
	}
	// 显式恢复也应当可用。
	if err := store.RestoreFromBackup(); err != nil {
		t.Fatalf("RestoreFromBackup: %v", err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("恢复后应可解析: %v", err)
	}
}

// TestValidateOptionsRejects 覆盖四类非法配置：未知键 / 类型不符 / select 越界 / int 越界。
func TestValidateOptionsRejects(t *testing.T) {
	defs := (&richPlugin{}).Options()
	cases := []struct {
		name string
		opts map[string]any
		want string
	}{
		{"未知键", map[string]any{"ghost": 1}, "未知配置项"},
		{"类型不符", map[string]any{"count": "many"}, "应为整数"},
		{"select 越界", map[string]any{"mode": "turbo"}, "不在可选范围内"},
		{"int 越界", map[string]any{"count": 999}, "不能大于"},
		{"时长非法", map[string]any{"interval": "soon"}, "不是合法时长"},
		{"字符串类型错", map[string]any{"label": 42}, "应为字符串"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateOptions(defs, c.opts)
			if err == nil {
				t.Fatalf("%v 应校验失败", c.opts)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("错误应为 ErrInvalidConfig，实际 %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误 %q 应包含 %q", err.Error(), c.want)
			}
		})
	}
}

// TestSaveSettingsInvalidDoesNotTouchDisk 验证非法配置返回错误且磁盘（内容与 mtime）不变。
func TestSaveSettingsInvalidDoesNotTouchDisk(t *testing.T) {
	m, path := newSettingsManager(t, false)
	if _, _, err := m.SaveSettings(validSettings(0)); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	bad := validSettings(1)
	bad.Plugins["rich"] = PluginPatch{Enabled: boolPtr(true), Options: map[string]any{"nope": true}}
	if _, _, err := m.SaveSettings(bad); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非法配置应返回 ErrInvalidConfig，实际 %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("非法配置不得写盘（磁盘内容被改变）")
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("非法配置不得写盘（mtime 被改变）")
	}
}

// TestSaveSettingsAppliesApplierAndReportsPending 验证热更新与 pendingRestart 的诚实标注。
func TestSaveSettingsAppliesApplierAndReportsPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}    // 实现 Applier
	slow := &optionsPlugin{meta: Meta{Name: "slow", DefaultOn: true}} // 未实现 Applier
	m := newManagerFrom([]Plugin{rich, slow})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"rich": true, "slow": true},
		map[string]map[string]any{"rich": {}, "slow": {"addr": ":0"}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	req := SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"rich": {Enabled: boolPtr(true), Options: map[string]any{"interval": "7s"}},
		"slow": {Enabled: boolPtr(true), Options: map[string]any{"addr": ":9099"}},
	}}
	snap, pending, err := m.SaveSettings(req)
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if snap.Version != 1 {
		t.Fatalf("保存后版本 = %d want 1", snap.Version)
	}
	rich.mu.Lock()
	applies, applied := rich.applies, rich.applied
	rich.mu.Unlock()
	if applies != 1 {
		t.Fatalf("rich 的 Apply 应被调用 1 次，实际 %d", applies)
	}
	if applied["interval"] != "7s" {
		t.Fatalf("Apply 收到的 interval = %v want 7s", applied["interval"])
	}
	// 未实现 Applier 的插件必须被如实标注（绝不假装热生效）。
	if !containsString(pending, "slow") {
		t.Fatalf("pendingRestart = %v 应包含 slow", pending)
	}
	if containsString(pending, "rich") {
		t.Fatalf("pendingRestart = %v 不应包含已热生效的 rich", pending)
	}
}

// TestSaveSettingsConflict 验证并发保存：第二个客户端收到 ErrConflict，并拿到磁盘最新值。
func TestSaveSettingsConflict(t *testing.T) {
	m, _ := newSettingsManager(t, false)
	if _, _, err := m.SaveSettings(validSettings(0)); err != nil {
		t.Fatalf("第一次保存: %v", err)
	}
	// 第二个客户端仍以 version=0 提交（它读到的是旧版本）。
	second := validSettings(0)
	second.Plugins["rich"] = PluginPatch{Enabled: boolPtr(true), Options: map[string]any{"label": "conflict"}}
	snap, _, err := m.SaveSettings(second)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("应返回 ErrConflict，实际 %v", err)
	}
	if snap.Version != 1 {
		t.Fatalf("冲突时应返回磁盘版本 1，实际 %d", snap.Version)
	}
	if got := snap.Plugins["rich"].Options["label"]; got != "hi" {
		t.Fatalf("冲突时返回的磁盘值 = %v want hi（第一次保存的值）", got)
	}
}

// TestSaveSettingsReadOnlyMode 验证只读模式拒绝写盘。
func TestSaveSettingsReadOnlyMode(t *testing.T) {
	m, path := newSettingsManager(t, true)
	_, _, err := m.SaveSettings(validSettings(0))
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("只读模式应返回 ErrReadOnly，实际 %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("只读模式不得创建配置文件")
	}
}

// TestSaveSettingsRejectsDisablingRequired 验证必需插件不能在配置里被关掉。
func TestSaveSettingsRejectsDisablingRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	req := &richPlugin{meta: Meta{Name: "req", Required: true, DefaultOn: true}}
	m := newManagerFrom([]Plugin{req})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"req": true}, nil)
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	_, _, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"req": {Enabled: boolPtr(false)},
	}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("应拒绝停用必需插件（ErrInvalidConfig），实际 %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("校验失败不得写盘")
	}
}

// TestSettingsSchemaDrivesFrontend 验证「新增带 Options 的插件 → 设置端点的 schema 自动出现它」
// （即控制台零前端改动即可渲染表单）。
func TestSettingsSchemaDrivesFrontend(t *testing.T) {
	m, _ := newSettingsManager(t, false)
	snap, err := m.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	defs, ok := snap.Schema["rich"]
	if !ok || len(defs) != 5 {
		t.Fatalf("schema[rich] = %+v (存在=%v) want 5 项", defs, ok)
	}
	if snap.ConfigPath == "" {
		t.Fatal("Settings 应回传配置文件路径（用户需要知道改的是哪个文件）")
	}
	if snap.Version != 0 {
		t.Fatalf("文件不存在时版本应为 0，实际 %d", snap.Version)
	}
	// 默认值随 schema 一起下发，供「恢复默认」使用。
	if got := snap.Defaults["rich"]["mode"]; got != "fast" {
		t.Fatalf("Defaults[rich][mode] = %v want fast", got)
	}
}

// TestSaveSettingsKeepsUntouchedPlugins 验证「局部提交」不会把其它插件的配置抹掉。
// （手工 curl 时很容易只发一个插件；整份覆盖会让用户莫名丢掉别的设置。）
func TestSaveSettingsKeepsUntouchedPlugins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	a := &optionsPlugin{meta: Meta{Name: "alpha", DefaultOn: true}}
	b := &optionsPlugin{meta: Meta{Name: "bravo", DefaultOn: true}}
	m := newManagerFrom([]Plugin{a, b})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"alpha": true, "bravo": true}, map[string]map[string]any{
		"alpha": {"addr": ":1"},
		"bravo": {"addr": ":2"},
	})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// 先写一份完整配置（version 0 → 1）。
	if _, _, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"alpha": {Enabled: boolPtr(true), Options: map[string]any{"addr": ":1"}},
		"bravo": {Enabled: boolPtr(true), Options: map[string]any{"addr": ":2"}},
	}}); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	// 只提交 alpha（version 1 → 2）。
	if _, _, err := m.SaveSettings(SettingsRequest{Version: 1, Plugins: map[string]PluginPatch{
		"alpha": {Enabled: boolPtr(true), Options: map[string]any{"addr": ":100"}},
	}}); err != nil {
		t.Fatalf("局部保存: %v", err)
	}

	disk, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := disk.Plugins["alpha"].Options["addr"]; got != ":100" {
		t.Fatalf("alpha.addr = %v want :100（提交的改动应生效）", got)
	}
	if got := disk.Plugins["bravo"].Options["addr"]; got != ":2" {
		t.Fatalf("bravo.addr = %v want :2（未提交的插件配置不应被抹掉）", got)
	}
}

// TestSaveSettingsMergesOptionsWithinPlugin 验证「同一插件内只提交部分 option」时，
// 未提交的其它 option 必须保留。否则「只改 interval」会把同插件的 bindAddr/hubName
// 一起抹成零值（这是「局部提交抹掉配置」缺陷的第二个层次，冒烟时由 changelog 暴露：
// 里面出现一串 `xxx.options.bindAddr=<nil>` 的误导性记录）。
func TestSaveSettingsMergesOptionsWithinPlugin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	// richPlugin 声明 5 个 option，正好用来验证「只交 1 个，其余 4 个保留」。
	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}
	m := newManagerFrom([]Plugin{rich})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"rich": true}, map[string]map[string]any{"rich": {}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	full := validSettings(0)
	if _, _, err := m.SaveSettings(full); err != nil {
		t.Fatalf("首次保存: %v", err)
	}

	// 只提交 interval 一个键。
	if _, _, err := m.SaveSettings(SettingsRequest{Version: 1, Plugins: map[string]PluginPatch{
		"rich": {Enabled: boolPtr(true), Options: map[string]any{"interval": "7s"}},
	}}); err != nil {
		t.Fatalf("局部保存: %v", err)
	}

	disk, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	opts := disk.Plugins["rich"].Options
	if got := opts["interval"]; got != "7s" {
		t.Fatalf("interval = %v want 7s（提交的键应生效）", got)
	}
	for k, want := range map[string]any{"label": "hi", "mode": "slow", "enabled": false, "count": float64(5)} {
		if got := opts[k]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s = %v want %v（未提交的同插件选项不得被抹掉）", k, got, want)
		}
	}
}

// TestSaveSettingsCleansUnknownDiskSection 覆盖 F2（生产事故级）：
// 磁盘上残留一个二进制里已不存在的插件段时，
//   - GET /api/settings 必须把它放进 unknownPlugins，而不是混进 plugins（别让人以为它在运行）；
//   - 合法 POST 必须 200（绝不能永久 400，否则用户无法通过控制台自救）；
//   - 保存时顺手清理该段，并在 changelog 记一条 removed unknown plugin section。
func TestSaveSettingsCleansUnknownDiskSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	// 手工造一份带「幽灵插件段」的磁盘配置（降级 / 删插件 / 手工编辑都会造成）。
	seed := `{
	  "version": 3,
	  "plugins": {
	    "rich": {"enabled": true, "options": {"label": "hi", "count": 5, "enabled": false, "interval": "45s", "mode": "slow"}},
	    "ghost": {"enabled": true, "options": {"x": 1}}
	  }
	}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("写种子配置: %v", err)
	}

	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}
	m := newManagerFrom([]Plugin{rich})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"rich": true}, map[string]map[string]any{"rich": {}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// GET：ghost 只能在 unknownPlugins 里。
	snap, err := m.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if _, ok := snap.Plugins["ghost"]; ok {
		t.Fatalf("ghost 不应出现在 plugins 里（它不是本二进制的插件）：%+v", snap.Plugins)
	}
	if !containsString(snap.UnknownPlugins, "ghost") {
		t.Fatalf("unknownPlugins = %v 应包含 ghost", snap.UnknownPlugins)
	}

	// POST 合法改动 → 200，且 ghost 段被清理。
	saved, _, err := m.SaveSettings(SettingsRequest{Version: 3, Plugins: map[string]PluginPatch{
		"rich": {Options: map[string]any{"label": "yo"}},
	}})
	if err != nil {
		t.Fatalf("磁盘残留未知插件不得让合法保存 400（用户将无法自救）: %v", err)
	}
	if saved.Version != 4 {
		t.Fatalf("保存后版本 = %d want 4", saved.Version)
	}
	disk, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := disk.Plugins["ghost"]; ok {
		t.Fatal("保存时应顺手清理未知插件段（ghost 仍在磁盘上）")
	}
	if got := disk.Plugins["rich"].Options["label"]; got != "yo" {
		t.Fatalf("rich.label = %v want yo", got)
	}
	changelog, err := os.ReadFile(path + ChangelogSuffix)
	if err != nil {
		t.Fatalf("读取 changelog: %v", err)
	}
	if !strings.Contains(string(changelog), "removed unknown plugin section: ghost") {
		t.Fatalf("changelog 应记录清理动作，实际:\n%s", changelog)
	}
	// N3：清理前必须把**完整内容**写进 changelog，否则「降级→升级→再降级」永久丢配置。
	if !strings.Contains(string(changelog), `ghost = {"enabled":true,"options":{"x":1}}`) {
		t.Fatalf("changelog 应保留被清理段的完整 JSON（N3），实际:\n%s", changelog)
	}
}

// TestSaveSettingsRejectsUnknownPluginInRequest 是 F2 的另一半：
// **请求体里**提交未知插件仍必须 400（用户显式写错了插件名，不能悄悄忽略）。
func TestSaveSettingsRejectsUnknownPluginInRequest(t *testing.T) {
	m, path := newSettingsManager(t, false)
	_, _, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"ghost": {Options: map[string]any{"x": 1}},
	}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("请求体里的未知插件应返回 ErrInvalidConfig，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("错误 %q 应点名 ghost", err.Error())
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("校验失败不得写盘")
	}
}

// TestStoreRecoversFromBackupAndRepairs 覆盖 F3：主文件损坏 + .bak 可用时
//   - 能从 .bak 加载（版本取 .bak 的）；
//   - 日志出现「配置恢复」字样（运维必须知道自己被回退过）；
//   - 坏文件留副本 hub.json.corrupt；
//   - 主文件被 .bak 原子修复，下次启动不再走回退路径。
func TestStoreRecoversFromBackupAndRepairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	store := NewStore(path)

	tpl := DefaultConfig(All())
	if err := store.SaveWithVersion(tpl, 0); err != nil { // hub.json v1
		t.Fatalf("首次写盘: %v", err)
	}
	if err := store.SaveWithVersion(tpl, 1); err != nil { // hub.json v2 + hub.json.bak(v1)
		t.Fatalf("二次写盘: %v", err)
	}

	// 把主文件写坏（模拟写入过程中被 kill）。
	broken := []byte("{ \"version\": 9, \"plugins\": { oops")
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatalf("写坏主文件: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("主文件损坏时应从 .bak 回退而不是报错: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("回退读到的版本 = %d want 1（.bak 里的版本）", cfg.Version)
	}
	if !strings.Contains(buf.String(), "配置恢复") {
		t.Fatalf(".bak 回退必须写日志（否则运维完全不知道），实际日志: %q", buf.String())
	}

	// 主文件已被修复为可解析，且版本是 .bak 的版本。
	fixedData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取修复后的主文件: %v", err)
	}
	var fixed Config
	if uerr := json.Unmarshal(fixedData, &fixed); uerr != nil {
		t.Fatalf("主文件应已被 .bak 原子修复，实际仍不可解析: %v", uerr)
	}
	if fixed.Version != 1 {
		t.Fatalf("修复后主文件版本 = %d want 1", fixed.Version)
	}

	// 坏文件保留了副本，供人工比对。
	corrupt, err := os.ReadFile(path + ".corrupt")
	if err != nil {
		t.Fatalf("应保留损坏副本 %s.corrupt: %v", path, err)
	}
	if !bytes.Equal(corrupt, broken) {
		t.Fatal(".corrupt 内容应等于损坏时的原始内容")
	}

	// .bak 没有被坏内容覆盖（修复时禁止备份）。
	bakCfg, err := store.Load()
	if err != nil || bakCfg.Version != 1 {
		t.Fatalf("修复后再次读取应直接读到主文件（version=1），实际 %d / %v", bakCfg.Version, err)
	}
}

// containsString 判断切片是否包含某字符串。
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestSaveSettingsCleansStaleOptionOnDisk 覆盖 N1（F2 的兄弟）：
// 磁盘上「已知插件 + 已废弃/拼错的 option key」既不该让保存永久 400，也不该静默消失得无痕。
//   - GET 必须把它放进 staleOptions（不在 plugins，看不出异常就最危险）；
//   - 合法 POST 必须 200，且该键被清掉；
//   - changelog 必须留存**完整原值**（N3：只记名字会让"降级→升级→再降级"永久丢配置）。
func TestSaveSettingsCleansStaleOptionOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	// rich 声明 label/count/enabled/interval/mode；legacyRetention 是"上一版遗留"的键。
	seed := `{
	  "version": 5,
	  "plugins": {
	    "rich": {"enabled": true, "options": {"label": "hi", "count": 5, "enabled": false, "interval": "45s", "mode": "slow", "legacyRetention": "7d"}}
	  }
	}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("写种子配置: %v", err)
	}

	rich := &richPlugin{meta: Meta{Name: "rich", DefaultOn: true}}
	m := newManagerFrom([]Plugin{rich})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"rich": true}, map[string]map[string]any{"rich": {}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// GET：废弃键只在 staleOptions 里，plugins 里看不到它。
	snap, err := m.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if _, ok := snap.Plugins["rich"].Options["legacyRetention"]; ok {
		t.Fatalf("legacyRetention 不应出现在 plugins.rich.options 里：%+v", snap.Plugins["rich"].Options)
	}
	if len(snap.StaleOptions) != 1 || snap.StaleOptions[0].Plugin != "rich" || snap.StaleOptions[0].Key != "legacyRetention" {
		t.Fatalf("staleOptions = %+v want [{rich legacyRetention}]", snap.StaleOptions)
	}
	if len(snap.UnknownPlugins) != 0 {
		t.Fatalf("unknownPlugins = %v want 空（这是废弃键，不是废弃插件）", snap.UnknownPlugins)
	}

	// 合法 POST（改已声明的键）→ 200，废弃键被清。
	saved, _, err := m.SaveSettings(SettingsRequest{Version: 5, Plugins: map[string]PluginPatch{
		"rich": {Options: map[string]any{"label": "yo"}},
	}})
	if err != nil {
		t.Fatalf("磁盘残留废弃 option 键不得让合法保存 400（用户无法自救）: %v", err)
	}
	if saved.Version != 6 {
		t.Fatalf("保存后版本 = %d want 6", saved.Version)
	}
	if len(saved.StaleOptions) != 0 {
		t.Fatalf("保存后 staleOptions 应为空，实际 %+v", saved.StaleOptions)
	}
	disk, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := disk.Plugins["rich"].Options["legacyRetention"]; ok {
		t.Fatal("保存时应清理废弃的 option 键")
	}
	if got := disk.Plugins["rich"].Options["label"]; got != "yo" {
		t.Fatalf("rich.label = %v want yo", got)
	}

	// changelog 必须能 grep 到原值 "7d"（N3：内容要留痕）。
	changelog, err := os.ReadFile(path + ChangelogSuffix)
	if err != nil {
		t.Fatalf("读取 changelog: %v", err)
	}
	if !strings.Contains(string(changelog), "removed stale option: rich.legacyRetention") {
		t.Fatalf("changelog 应记录清理动作，实际:\n%s", changelog)
	}
	if !strings.Contains(string(changelog), `"7d"`) {
		t.Fatalf("changelog 应保留完整原值（N3），实际:\n%s", changelog)
	}
}

// TestSaveSettingsRejectsUnknownOptionInRequest 是 N1 的另一半：
// **请求体里**提交未声明的 option key 仍必须 400 且不写盘
// （与磁盘残留的宽容处理严格区分：那是"用户显式写错"，静默丢弃等于偷改用户输入）。
func TestSaveSettingsRejectsUnknownOptionInRequest(t *testing.T) {
	m, path := newSettingsManager(t, false)
	_, _, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"rich": {Options: map[string]any{"legacyRetention": "7d"}},
	}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("请求体里的未知 option 键应返回 ErrInvalidConfig，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "legacyRetention") {
		t.Fatalf("错误应点名该键，实际: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("校验失败不得写盘")
	}
}

// TestSaveSettingsKeepsEnabledWhenOmitted 验证「没提交 enabled」= 本次不改启用状态。
// 这是插件级增量的关键：否则「只改 interval」会被 JSON 零值静默解释成「停用该插件」，
// 与同一 API 里 option 级增量合并的语义自相矛盾。
func TestSaveSettingsKeepsEnabledWhenOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	a := &optionsPlugin{meta: Meta{Name: "alpha", DefaultOn: true}}
	m := newManagerFrom([]Plugin{a})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"alpha": true}, map[string]map[string]any{"alpha": {"addr": ":1"}})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}

	// 1) 全量保存：显式 enabled=true。
	if _, _, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"alpha": {Enabled: boolPtr(true), Options: map[string]any{"addr": ":1"}},
	}}); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	// 2) 局部保存：**不带 enabled**，只改 addr。
	if _, _, err := m.SaveSettings(SettingsRequest{Version: 1, Plugins: map[string]PluginPatch{
		"alpha": {Options: map[string]any{"addr": ":2"}},
	}}); err != nil {
		t.Fatalf("局部保存: %v", err)
	}

	disk, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !disk.Plugins["alpha"].Enabled {
		t.Fatal("未提交 enabled 时不得把插件改成停用（磁盘 enabled 应为 true）")
	}
	if got := disk.Plugins["alpha"].Options["addr"]; got != ":2" {
		t.Fatalf("alpha.addr = %v want :2（已提交的键应生效）", got)
	}
	changelog, err := os.ReadFile(path + ChangelogSuffix)
	if err != nil {
		t.Fatalf("读取 changelog: %v", err)
	}
	if strings.Contains(string(changelog), "enabled=false") {
		t.Fatalf("changelog 不应出现 enabled=false（未提交启用状态）：\n%s", changelog)
	}
	// GET 响应里的 enabled 必须是布尔值，绝不出现 null。
	rawSnap, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := rawSnap.Plugins["alpha"].Enabled; !got {
		t.Fatal("响应/磁盘的 enabled 必须是布尔 true")
	}
}

// hotColdPlugin 模拟 discovery 的热更新契约：interval 可热更，bindAddr 需重启。
// 它只对「收到的键」逐个判断——这正是 Applier 接收增量补丁（而非全量配置）的前提。
type hotColdPlugin struct {
	meta     Meta
	interval string
	bindAddr string
	calls    int
}

func (p *hotColdPlugin) Meta() Meta { return p.meta }

func (p *hotColdPlugin) Init(ctx *Context) error {
	p.interval = ctx.String("interval", "5s")
	p.bindAddr = ctx.String("bindAddr", ":8082")
	p.calls = 0
	return nil
}

func (p *hotColdPlugin) Start() error { return nil }
func (p *hotColdPlugin) Stop() error  { return nil }

func (p *hotColdPlugin) Options() []OptionDef {
	return []OptionDef{
		{Key: "interval", Type: OptionDuration, Default: "5s", Description: "广播周期（热更新）"},
		{Key: "bindAddr", Type: OptionString, Default: ":8082", Description: "监听地址（需重启）"},
	}
}

func (p *hotColdPlugin) Apply(options map[string]any) error {
	p.calls++
	for k, v := range options {
		if k == "interval" {
			p.interval = fmt.Sprint(v) // 热更新：立即生效
			continue
		}
		return fmt.Errorf("%s 需要重启才生效", k)
	}
	return nil
}

// TestPendingRestartOnlyForSubmittedChanges 锁死 pendingRestart 的两个方向：
//   - 只改可热更新的键（interval）→ pendingRestart **必须为空**，且热更新真的生效；
//   - 改需要重启的键（bindAddr）→ pendingRestart 含该插件。
//
// 反例（修复前）：Applier 收到的是「全量配置」，CLI 覆盖值与文件值的固有差异
// （-discover :50130 vs 文件 :8082）会被算成改动 → 只改 interval 也报待重启（假阳性）。
func TestPendingRestartOnlyForSubmittedChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	hot := &hotColdPlugin{meta: Meta{Name: "hot", DefaultOn: true}}
	m := newManagerFrom([]Plugin{hot})
	m.SetConfigPath(path, false)
	m.ApplyConfig(map[string]bool{"hot": true}, map[string]map[string]any{
		"hot": {"interval": "5s", "bindAddr": ":8082"},
	})
	if err := m.BuildContext(nil); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	m.InitAll()
	if _, err := m.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}

	// 1) 只改 interval → 热更新生效，且不算待重启。
	snap, pending, err := m.SaveSettings(SettingsRequest{Version: 0, Plugins: map[string]PluginPatch{
		"hot": {Options: map[string]any{"interval": "42s"}},
	}})
	if err != nil {
		t.Fatalf("只改 interval 应保存成功: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("只改可热更新项时 pendingRestart 应为空，实际 %v", pending)
	}
	if hot.interval != "42s" {
		t.Fatalf("热更新未生效：插件内 interval = %q want 42s", hot.interval)
	}
	if hot.calls != 1 {
		t.Fatalf("Applier 调用次数 = %d want 1", hot.calls)
	}
	if got := snap.Plugins["hot"].Options["bindAddr"]; got != ":8082" {
		t.Fatalf("bindAddr = %v want :8082（未提交的键不得被抹掉）", got)
	}

	// 2) 改需要重启的键 → pendingRestart 必须点名它。
	snap2, pending2, err := m.SaveSettings(SettingsRequest{Version: 1, Plugins: map[string]PluginPatch{
		"hot": {Options: map[string]any{"bindAddr": ":9999"}},
	}})
	if err != nil {
		t.Fatalf("改 bindAddr 应保存成功（磁盘可写，只是需重启生效）: %v", err)
	}
	if len(pending2) != 1 || pending2[0] != "hot" {
		t.Fatalf("改 bindAddr 时 pendingRestart = %v want [hot]", pending2)
	}
	if got := snap2.Plugins["hot"].Options["interval"]; got != "42s" {
		t.Fatalf("上一次保存的 interval = %v want 42s（不得被本次提交抹掉）", got)
	}
}
