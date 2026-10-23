// Package plugincontract 的测试入口（V6 ⑧）。
//
// 表驱动遍历 plugin.All() —— blank import cmd/hub/plugins 触发全部内置插件的
// init() 注册，与生产二进制的能力清单一致：新增插件只要在
// cmd/hub/plugins/plugins.go 加一行，就自动纳入本契约。
//
// 位置说明：设计文档原文要求在 internal/plugin/contract_test.go，但铁律
// 「plugin 框架包不依赖 plugins」使该位置有边界争议，故放在独立测试目录
// test/plugincontract/（与 test/contract/ 并列）——这是**有意偏离**，
// 详见 plugincontract.go 头部注释。可复用断言（AssertContract）也因「要被
// 插件自测 import」而必须位于非测试文件，同样见该注释。
package plugincontract

import (
	"strings"
	"testing"

	"lan-agent-hub/internal/plugin"

	// 触发全部内置插件的编译期注册（blank import，不含任何逻辑）。
	_ "lan-agent-hub/cmd/hub/plugins"
)

// TestAllRegisteredPlugins 对全部已注册插件执行契约断言（表驱动，逐插件子测试）。
//
// 覆盖范围随 plugins.go 自动增长；并行开发中的插件（如 internal/plugins/file）
// 一旦进入注册清单，也会被本测试纳入——抓到的违背即缺陷，按名汇报，不顺手修。
func TestAllRegisteredPlugins(t *testing.T) {
	plugins := plugin.All()
	if len(plugins) == 0 {
		t.Fatal("plugin.All() 为空：cmd/hub/plugins 的 blank import 未生效？")
	}
	for _, p := range plugins {
		p := p // go.mod 声明 go 1.21：循环变量按迭代共享，子测试必须捕获
		t.Run(p.Meta().Name, func(t *testing.T) {
			AssertContract(t, p)
		})
	}
}

// ---------------------------------------------------------------------------
// 断言自身的防 vacuum 测试：证明违背真的会被抓到（而不是永远绿的空断言）
// ---------------------------------------------------------------------------

// badPlugin 是刻意违反多项契约的假插件（不注册，仅供验证断言会抓错）。
type badPlugin struct{}

func (badPlugin) Meta() plugin.Meta {
	return plugin.Meta{Name: "badplugin"} // 缺 Version / Description
}

func (badPlugin) Init(*plugin.Context) error { return nil }
func (badPlugin) Start() error              { return nil }
func (badPlugin) Stop() error               { return nil }

func (badPlugin) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{Key: "", Type: plugin.OptionString, Default: "x", Description: "空 Key"},
		{Key: "s", Type: plugin.OptionString, Default: 42, Description: "类型不符"},
		{Key: "i", Type: plugin.OptionInt, Default: "5", Description: "类型不符"},
		{Key: "i", Type: plugin.OptionInt, Default: 100, Min: 1, Max: 10, Description: "越界"},
		{Key: "b", Type: plugin.OptionBool, Default: "yes", Description: "类型不符"},
		{Key: "nodesc", Type: plugin.OptionBool, Default: true}, // 缺 Description
	}
}

// goodPlugin 是各项声明都合规的假插件（不注册，仅供验证断言不误伤）。
type goodPlugin struct{}

func (goodPlugin) Meta() plugin.Meta {
	return plugin.Meta{Name: "goodplugin", Version: "1.0.0", Description: "合规假插件"}
}

func (goodPlugin) Init(*plugin.Context) error { return nil }
func (goodPlugin) Start() error              { return nil }
func (goodPlugin) Stop() error               { return nil }

func (goodPlugin) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{Key: "s", Type: plugin.OptionString, Default: "x", Description: "字符串项"},
		{Key: "i", Type: plugin.OptionInt, Default: 5, Min: 1, Max: 10, Description: "整型项"},
		{Key: "b", Type: plugin.OptionBool, Default: true, Description: "开关项"},
	}
}

// TestValidatorsCatchBadPlugin：坏插件的每一项违背都必须被抓到。
func TestValidatorsCatchBadPlugin(t *testing.T) {
	if v := metaViolations(badPlugin{}); len(v) == 0 {
		t.Fatal("metaViolations 对缺 Version/Description 的插件未报违背")
	}
	violations := strings.Join(optionViolations(badPlugin{}), " | ")
	for _, want := range []string{
		"Key 为空", "Key 重复", "Description 为空",
		"OptionString 的 Default 应为 string",
		"OptionInt 的 Default 应为 int",
		"OptionBool 的 Default 应为 bool",
		"大于 Max 10",
	} {
		if !strings.Contains(violations, want) {
			t.Fatalf("optionViolations 未覆盖 %q，实得: %s", want, violations)
		}
	}
}

// TestValidatorsAcceptWellFormed：合规声明零误伤；目录核对两边都要验。
func TestValidatorsAcceptWellFormed(t *testing.T) {
	if v := metaViolations(goodPlugin{}); len(v) > 0 {
		t.Fatalf("metaViolations 误伤合规插件: %v", v)
	}
	if v := optionViolations(goodPlugin{}); len(v) > 0 {
		t.Fatalf("optionViolations 误伤合规声明: %v", v)
	}
	if v := dirViolations("webhook"); len(v) > 0 {
		t.Fatalf("webhook 是真实目录却被报违背: %v", v)
	}
	if v := dirViolations("no-such-plugin-xyz"); len(v) == 0 {
		t.Fatal("不存在的插件目录未被报违背")
	}
}
