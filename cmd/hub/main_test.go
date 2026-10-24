package main

import (
	"strings"
	"testing"
	"time"
)

// TestTimeoutPresetOptions 表驱动断言三种档位产出的配置项。
//
// 断言的是**精确键值集合**（keys 数量 + 每键取值），不是"包含"关系：
// 预设档多写一个键，就会去覆盖一个本来不该由它决定的选项——检测方式只有精确比对。
func TestTimeoutPresetOptions(t *testing.T) {
	cases := []struct {
		name string
		want map[string]any
	}{
		{
			// 留空 = 不干预：一个键都不能有（否则等于把插件默认值硬编码到 main 里，
			// 插件改了默认值这里不会跟着变）。
			name: "",
			want: nil,
		},
		{
			name: "strict",
			want: map[string]any{
				"readHeaderTimeout": "5s",
				"idleTimeout":       "30s",
				"handlerTimeout":    "15s",
			},
		},
		{
			name: "relaxed",
			want: map[string]any{
				"readHeaderTimeout": "10s",
				"idleTimeout":       "120s",
				"handlerTimeout":    "60s",
			},
		},
		{
			name: "off",
			want: map[string]any{
				"readHeaderTimeout": "0s",
				"idleTimeout":       "0s",
				"handlerTimeout":    "0s",
			},
		},
		{
			// 大小写与首尾空格容错：Windows 命令行上多一个空格是最常见的输入噪音，
			// 容忍它不会带来任何歧义（只有三个取值，不存在大小写不同的另一档）。
			name: "  Strict ",
			want: map[string]any{
				"readHeaderTimeout": "5s",
				"idleTimeout":       "30s",
				"handlerTimeout":    "15s",
			},
		},
	}

	for _, c := range cases {
		got, err := timeoutPresetOptions(c.name)
		if err != nil {
			t.Fatalf("-timeouts=%q 返回错误: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("-timeouts=%q 产出 %d 个键，期望 %d 个：%v", c.name, len(got), len(c.want), got)
		}
		for k, want := range c.want {
			if got[k] != want {
				t.Errorf("-timeouts=%q 的 %s = %v，期望 %v", c.name, k, got[k], want)
			}
		}
	}
}

// TestTimeoutPresetsAreParseableDurations 断言预设值真的能被解析成预期时长。
//
// 为什么需要这条：httpgateway 侧用 Context.Duration 读配置，而它在
// time.ParseDuration 失败时会**静默回落到默认值**——预设档写个 "5" 之类的笔误，
// 表现为"档位看起来生效了、实际还是默认值"，最难排查。
// 这里同时钉住「0s 就是 0（关闭）」这一语义，避免 off 档被误判为非法值。
func TestTimeoutPresetsAreParseableDurations(t *testing.T) {
	want := map[string]map[string]time.Duration{
		"strict": {
			"readHeaderTimeout": 5 * time.Second,
			"idleTimeout":       30 * time.Second,
			"handlerTimeout":    15 * time.Second,
		},
		"relaxed": {
			"readHeaderTimeout": 10 * time.Second,
			"idleTimeout":       120 * time.Second,
			"handlerTimeout":    60 * time.Second,
		},
		"off": {
			"readHeaderTimeout": 0,
			"idleTimeout":       0,
			"handlerTimeout":    0,
		},
	}

	for preset, perKey := range want {
		got, err := timeoutPresetOptions(preset)
		if err != nil {
			t.Fatalf("-timeouts=%q: %v", preset, err)
		}
		for k, wantDur := range perKey {
			raw, ok := got[k].(string)
			if !ok {
				t.Fatalf("%s.%s 不是字符串（Context.Duration 只接受字符串或数字毫秒）", preset, k)
			}
			d, perr := time.ParseDuration(raw)
			if perr != nil {
				t.Fatalf("%s.%s = %q 不可解析: %v（会被静默回落到插件默认值）", preset, k, raw, perr)
			}
			if d != wantDur {
				t.Errorf("%s.%s 解析为 %s，期望 %s", preset, k, d, wantDur)
			}
		}
	}
}

// TestTimeoutPresetNeverSetsWriteTimeout 钉住「预设档绝不设全局读/写超时」。
//
// V8e 的结论：WriteTimeout 覆盖整个响应写出过程，SSE 挂到第 N 秒就被硬切；
// ReadTimeout 到期给连接打上永久读截止时间，长连接后续读全失败。
// 这两者是历史行为里"故意没有"的东西。httpgateway 目前也确实没有声明这两个选项，
// 所以预设档一写它们，就会被 Context 静默忽略——既不生效，又让人以为配过了。
// 一旦有人给预设档加上这类键，这条用例立刻变红。
func TestTimeoutPresetNeverSetsWriteTimeout(t *testing.T) {
	forbidden := []string{"writeTimeout", "readTimeout", "readIdleTimeout", "readHeaderBytes"}
	for _, preset := range []string{"strict", "relaxed", "off"} {
		got, err := timeoutPresetOptions(preset)
		if err != nil {
			t.Fatalf("-timeouts=%q: %v", preset, err)
		}
		for _, k := range forbidden {
			if _, ok := got[k]; ok {
				t.Errorf("-timeouts=%q 设置了 %s：SSE 长连接会被掐断（V8e 已踩过这个坑）", preset, k)
			}
		}
	}
}

// TestTimeoutPresetRejectsUnknown 断言非法档位返回错误（由 main 报错退出，不静默忽略）。
func TestTimeoutPresetRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"fast", "stric", "on", "true", "1", "strict,relaxed"} {
		if _, err := timeoutPresetOptions(bad); err == nil {
			t.Errorf("-timeouts=%q 应返回错误（非法值必须报错退出，不能静默忽略）", bad)
		} else if !strings.Contains(err.Error(), "strict") {
			// 错误文案要点名可选值：用户拼错时第一眼就该知道正确写法。
			t.Errorf("-timeouts=%q 的错误文案未列出可选值: %v", bad, err)
		}
	}
}

// testGatewayDefaults 是 httpgateway 声明的超时默认值（与插件 Options() 一致）。
//
// 测试里复制一份而不是反射真实插件：这条用例要钉住的是「判定规则」，
// 用固定默认值才能构造"用户写的值恰好等于默认值"这种边界。
// 真实默认值与插件是否一致，由 TestDeclaredDefaultsMatchHTTPGateway 单独守护。
var testGatewayDefaults = map[string]any{
	"readHeaderTimeout": "10s",
	"idleTimeout":       "120s",
	"handlerTimeout":    "5s",
}

// TestDeclaredDefaultsMatchHTTPGateway 断言 main 侧读到的默认值与插件声明一致。
//
// 判定规则依赖「值 == 默认值 ⇒ 用户没表态」。如果 declaredDefaults 读不到插件
// （名字拼错、Optioner 没实现），它会返回 nil 并退化成"只看键是否存在"，
// 于是预设档在真实配置下永远不生效——而这个失效是静默的。
// 这条用例把"读得到、且值对得上"变成硬断言。
func TestDeclaredDefaultsMatchHTTPGateway(t *testing.T) {
	got := declaredDefaults(httpgatewayPlugin)
	if got == nil {
		t.Fatalf("declaredDefaults(%q) = nil：读不到插件选项声明，预设档会静默退化为按键存在性判断", httpgatewayPlugin)
	}
	for k, want := range testGatewayDefaults {
		if got[k] != want {
			t.Errorf("默认值 %s = %v，期望 %v（插件声明改了，需同步本档位设计与测试）", k, got[k], want)
		}
	}
}

// TestFillPresetOptionsDoesNotOverrideExplicit 是本任务最关键的一条。
//
// 语义必须是「只填用户没表态的项」：用户在 hub.json 里写了 readHeaderTimeout: 30s，
// 再带 -timeouts=strict 启动，30s 必须**原样保留**。
// 若改成无条件覆盖，测试立刻红——"我明明设了 30s 却被改成 5s"这类问题
// 现场表现为运行期连接行为异常，根因却在启动参数里，排查成本最高。
func TestFillPresetOptionsDoesNotOverrideExplicit(t *testing.T) {
	options := map[string]map[string]any{
		httpgatewayPlugin: {
			"httpAddr":          ":9091",
			"readHeaderTimeout": "30s", // 用户显式配置（≠ 默认 10s），必须胜出
		},
	}
	fill, err := timeoutPresetOptions("strict")
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	applied := fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill)

	got := options[httpgatewayPlugin]
	if v := got["readHeaderTimeout"]; v != "30s" {
		t.Errorf("readHeaderTimeout = %v，期望 30s（显式配置被预设档覆盖了）", v)
	}
	// 缺席项由预设档填充。
	if v := got["idleTimeout"]; v != "30s" {
		t.Errorf("idleTimeout = %v，期望 30s（缺席项应由预设档填补）", v)
	}
	if v := got["handlerTimeout"]; v != "15s" {
		t.Errorf("handlerTimeout = %v，期望 15s（缺席项应由预设档填补）", v)
	}
	// 与本任务无关的既有项不得被动过。
	if v := got["httpAddr"]; v != ":9091" {
		t.Errorf("httpAddr = %v，期望 :9091", v)
	}
	if len(got) != 4 {
		t.Errorf("选项数 = %d，期望 4：%v（预设档塞了额外的键）", len(got), got)
	}

	// 返回值必须只含"真正应用"的项：启动日志据此自证，多报一项就会让用户
	// 以为自己配的 readHeaderTimeout 被改了（正是本任务要避免的误判）。
	if _, reported := applied["readHeaderTimeout"]; reported {
		t.Error("返回的应用集合里出现了 readHeaderTimeout：它并未被改写，日志会误导用户")
	}
	if len(applied) != 2 {
		t.Errorf("应用集合 = %v，期望 2 项（idleTimeout / handlerTimeout）", applied)
	}
	if v := applied["handlerTimeout"]; v != "15s" {
		t.Errorf("应用集合 handlerTimeout = %v，期望 15s", v)
	}
}

// TestFillPresetOptionsOverridesMergedDefaults 是「预设档到底有没有生效」的硬证据。
//
// 真实链路里 options 已经不是"用户写了什么"，而是 Store.LoadOrTemplate() 的
// MergeDefaults 结果——每个已声明选项都在，值等于插件默认值。
// 这条用例复刻那个形状：三项都是默认值 → 三项都必须被预设档接管。
// 若判定规则退回"键是否存在"，这里会得到 applied 为空、值仍为默认值，
// 也就是"功能看起来实现了、跑起来毫无作用"。
func TestFillPresetOptionsOverridesMergedDefaults(t *testing.T) {
	options := map[string]map[string]any{
		httpgatewayPlugin: {
			"httpAddr":          ":8081",
			"readHeaderTimeout": "10s",  // == 默认值 → 用户没表态
			"idleTimeout":       "120s", // == 默认值
			"handlerTimeout":    "5s",   // == 默认值
		},
	}
	fill, err := timeoutPresetOptions("strict")
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	applied := fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill)

	got := options[httpgatewayPlugin]
	want := map[string]any{"readHeaderTimeout": "5s", "idleTimeout": "30s", "handlerTimeout": "15s"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v，期望 %v（MergeDefaults 补齐的默认值应被预设档接管）", k, got[k], w)
		}
		if applied[k] != w {
			t.Errorf("应用集合缺 %s（=%v），启动日志将无法自证预设档已生效", k, w)
		}
	}
	if len(applied) != 3 {
		t.Errorf("应用集合 = %v，期望 3 项", applied)
	}
	if v := got["httpAddr"]; v != ":8081" {
		t.Errorf("httpAddr = %v，非超时项不得被改动", v)
	}
}

// TestFillPresetOptionsKeepsExplicitZero 断言「显式写 0」算用户表态（关闭该超时）。
//
// 0 与默认值不同，因此必须原样保留：用户写 0 就是在说"这一项我要关掉"，
// 若被当成"没配置"再填回 5s，最需要它的场景（排查连接被超时掐断）就失效了。
func TestFillPresetOptionsKeepsExplicitZero(t *testing.T) {
	for _, preset := range []string{"strict", "relaxed", "off"} {
		options := map[string]map[string]any{
			httpgatewayPlugin: {"readHeaderTimeout": "0s"},
		}
		fill, err := timeoutPresetOptions(preset)
		if err != nil {
			t.Fatalf("%s: %v", preset, err)
		}
		fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill)

		if v := options[httpgatewayPlugin]["readHeaderTimeout"]; v != "0s" {
			t.Errorf("-timeouts=%s: readHeaderTimeout = %v，期望 0s（显式关闭被预设档改回来了）", preset, v)
		}
	}
}

// TestFillPresetOptionsCreatesSectionWhenAbsent 断言插件段不存在时会被建出来。
//
// configToMaps 其实总会为每个已注册插件建一个空 map，但预设档作为独立函数
// 不该依赖那个前提（将来的 -maxconn 等档位会复用同一套逻辑）。
func TestFillPresetOptionsCreatesSectionWhenAbsent(t *testing.T) {
	options := map[string]map[string]any{}
	fill, err := timeoutPresetOptions("relaxed")
	if err != nil {
		t.Fatalf("relaxed: %v", err)
	}
	fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill)

	got, ok := options[httpgatewayPlugin]
	if !ok {
		t.Fatal("插件段没有被建出来，预设档静默失效")
	}
	if len(got) != 3 {
		t.Errorf("选项数 = %d，期望 3：%v", len(got), got)
	}
	if v := got["handlerTimeout"]; v != "60s" {
		t.Errorf("handlerTimeout = %v，期望 60s", v)
	}
}

// TestFillPresetOptionsWithEmptyPresetIsNoop 断言空档位完全不碰配置。
//
// 包括「不建空插件段」：无端建一个空 map 会让上游"这个插件有没有被 CLI 碰过"
// 之类的判断产生假阳性（与 mergeOptions 的行为保持一致）。
func TestFillPresetOptionsWithEmptyPresetIsNoop(t *testing.T) {
	options := map[string]map[string]any{}
	fill, err := timeoutPresetOptions("")
	if err != nil {
		t.Fatalf("空档位: %v", err)
	}
	if applied := fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill); len(applied) != 0 {
		t.Errorf("空档位应用了 %v，期望什么都不做", applied)
	}
	if len(options) != 0 {
		t.Errorf("空档位改动了配置: %v", options)
	}
}

// TestFillPresetOptionsWithoutDefaultsFallsBackToPresence 断言读不到插件声明时退化行为。
//
// defaults 为 nil（插件未注册/不声明选项）时退回「只看键是否存在」：
// 宁可少填，也不要因为"读不到默认值"就把用户的配置当成默认值覆盖掉。
func TestFillPresetOptionsWithoutDefaultsFallsBackToPresence(t *testing.T) {
	options := map[string]map[string]any{
		httpgatewayPlugin: {"readHeaderTimeout": "30s"},
	}
	fill, err := timeoutPresetOptions("strict")
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	fillPresetOptions(options, httpgatewayPlugin, nil, fill)

	if v := options[httpgatewayPlugin]["readHeaderTimeout"]; v != "30s" {
		t.Errorf("readHeaderTimeout = %v，期望 30s（无默认值可比较时必须保守）", v)
	}
	if v := options[httpgatewayPlugin]["idleTimeout"]; v != "30s" {
		t.Errorf("idleTimeout = %v，期望 30s（缺席项仍应被填补）", v)
	}
}

// TestFillPresetOptionsDoesNotTouchOtherPlugins 断言只作用于 httpgateway。
//
// 预设档是 httpgateway 的超时集合；顺手写给别的插件等于把超时语义灌进不相关的插件，
// 而那边的同名键含义可能完全不同。
func TestFillPresetOptionsDoesNotTouchOtherPlugins(t *testing.T) {
	options := map[string]map[string]any{
		"tcpgateway": {"addr": ":8080"},
		"metrics":    {"interval": "5s"},
	}
	fill, err := timeoutPresetOptions("strict")
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	fillPresetOptions(options, httpgatewayPlugin, testGatewayDefaults, fill)

	if len(options["tcpgateway"]) != 1 || options["tcpgateway"]["addr"] != ":8080" {
		t.Errorf("tcpgateway 配置被改动: %v", options["tcpgateway"])
	}
	if len(options["metrics"]) != 1 || options["metrics"]["interval"] != "5s" {
		t.Errorf("metrics 配置被改动: %v", options["metrics"])
	}
}

// TestSameOptionValue 覆盖默认值比较的等价性（JSON 数字 vs 声明值）。
func TestSameOptionValue(t *testing.T) {
	cases := []struct {
		a, b any
		want bool
	}{
		{"10s", "10s", true},
		{"10s", "5s", false},
		{float64(100), 100, true}, // JSON 数字是 float64，声明值可能是 int
		{float64(0), "0s", false}, // 语义不同（毫秒数字 vs 时长文本）→ 保守判为不同
		{nil, nil, true},
		{nil, "5s", false},
	}
	for _, c := range cases {
		if got := sameOptionValue(c.a, c.b); got != c.want {
			t.Errorf("sameOptionValue(%v, %v) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// TestCliOverridesNothingWhenNoFlagExplicit 是 D2 修复的核心证据。
//
// 一个 flag 都没表态时，覆盖表必须为空（连空插件段都不建）：CLI 默认值一旦
// 混进覆盖表，hub.json 的 tcpgateway.addr / httpgateway.httpAddr /
// discovery.bindAddr / hubName / noDiscovery / autobrowser.open 就永远不生效
// ——这正是 V7c 真机冒烟抓到的缺陷现场。退回无条件合并，这条立刻红。
func TestCliOverridesNothingWhenNoFlagExplicit(t *testing.T) {
	f := cliFlags{
		addr: ":8080", httpAddr: ":8081", discover: ":8082", hubName: "",
		noDiscovery: false, openBrowser: true,
		set: map[string]bool{}, // 全部走默认值 = 全部未表态
	}
	if got := cliOverrides(f); len(got) != 0 {
		t.Fatalf("未表态时覆盖表 = %v，期望为空（CLI 默认值不得压过 hub.json）", got)
	}
}

// TestCliOverridesExplicitLandsOnRightPlugin 断言六个 flag 各自落到正确的插件与键。
//
// 映射关系写错（例如 -http 落到 tcpgateway）的症状是「CLI 参数看似生效、
// 实际改的是不相干的插件」，只有逐项断言才抓得住。
func TestCliOverridesExplicitLandsOnRightPlugin(t *testing.T) {
	f := cliFlags{
		addr:        ":9000",
		httpAddr:    ":9001",
		discover:    "", // -discover="" 是合法表态：显式关闭发现
		hubName:     "厨房Hub",
		noDiscovery: true,
		openBrowser: false,
		set:         map[string]bool{"addr": true, "http": true, "discover": true, "name": true, "no-discovery": true, "open": true},
	}
	got := cliOverrides(f)
	want := map[string]map[string]any{
		"tcpgateway":  {"addr": ":9000"},
		"httpgateway": {"httpAddr": ":9001"},
		"discovery":   {"bindAddr": "", "hubName": "厨房Hub", "noDiscovery": true},
		"autobrowser": {"open": false},
	}
	if len(got) != len(want) {
		t.Fatalf("覆盖表插件段 = %v，期望恰 4 段", got)
	}
	for name, opts := range want {
		for k, w := range opts {
			if got[name][k] != w {
				t.Errorf("%s.%s = %v，期望 %v", name, k, got[name][k], w)
			}
		}
	}
}

// TestCliExplicitBeatsFileBeatsDefault 验证完整优先级链：CLI 显式 > hub.json > 内置默认。
//
// 复刻真实链路的形状：文件值经 MergeDefaults 后全键都在（这里手工构造等价形状），
// CLI 只显式给 -addr。期望 tcpgateway.addr 用 CLI 的 :9090，
// httpgateway.httpAddr 保持文件值 :9091——若 CLI 默认值 :8081 混进覆盖表，
// 这里会得到 :8081，即缺陷复现。
func TestCliExplicitBeatsFileBeatsDefault(t *testing.T) {
	options := map[string]map[string]any{
		"tcpgateway":  {"addr": ":8080"}, // 文件里是默认值（MergeDefaults 补齐的形状）
		"httpgateway": {"httpAddr": ":9091"},
	}
	mergeOptions(options, cliOverrides(cliFlags{
		addr:     ":9090",
		httpAddr: ":8081", // CLI 默认值：未表态
		set:      map[string]bool{"addr": true},
	}))
	if v := options["tcpgateway"]["addr"]; v != ":9090" {
		t.Errorf("tcpgateway.addr = %v，期望 :9090（CLI 显式参数必须胜出）", v)
	}
	if v := options["httpgateway"]["httpAddr"]; v != ":9091" {
		t.Errorf("httpgateway.httpAddr = %v，期望 :9091（CLI 未表态的默认值压过了 hub.json——D2 缺陷复现）", v)
	}
}

// TestOptionString 覆盖生效地址读取的回落语义。
//
// 预检 / Beacon 端口 / 启动日志三处都从合并后的 options 取地址，
// 回落规则错了症状是「三处口径不一」：文件值类型被手改坏（JSON 里写成数字）
// 时必须回落 flag 值，与插件侧 Context.String 的口径一致。
func TestOptionString(t *testing.T) {
	options := map[string]map[string]any{
		"tcpgateway": {"addr": ":9090"},
		"weird":      {"addr": 8080}, // 手改坏的类型
		"empty":      {},
	}
	if got := optionString(options, "tcpgateway", "addr", ":8080"); got != ":9090" {
		t.Errorf("正常路径 got=%s，期望 :9090", got)
	}
	if got := optionString(options, "weird", "addr", ":8080"); got != ":8080" {
		t.Errorf("非字符串值 got=%s，期望回落 :8080", got)
	}
	if got := optionString(options, "empty", "addr", ":8080"); got != ":8080" {
		t.Errorf("键缺席 got=%s，期望回落 :8080", got)
	}
	if got := optionString(options, "missing-plugin", "addr", ":8080"); got != ":8080" {
		t.Errorf("插件段缺席 got=%s，期望回落 :8080", got)
	}
	// 显式空串是字符串类型断言成功 → 原样透传而不是回落默认值：
	// 预检会以人话报「不是 host:port 形式」，静默回落反而掩盖配置错误。
	bad := map[string]map[string]any{"tcpgateway": {"addr": ""}}
	if got := optionString(bad, "tcpgateway", "addr", ":8080"); got != "" {
		t.Errorf("显式空串 got=%q，期望原样透传（由预检报人话错误，不静默回落）", got)
	}
}
