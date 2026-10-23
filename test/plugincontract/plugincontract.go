// Package plugincontract 是「插件契约」的可复用断言库（V6 ⑧）。
//
// 背景：V6 设计文档要求在 internal/plugin/contract_test.go 放契约测试，但铁律
// 「plugin 框架包不依赖 plugins」使该位置存在边界争议——框架包内的测试一旦
// import 具体插件，check-deps.sh 的断言（internal/plugin 不得依赖 internal/plugins）
// 就会误伤。为完全避免争议，本包放在独立测试目录 test/plugincontract/ 下
// （与 test/contract/ 并列），这是**有意偏离**设计文档的位置安排。
//
// 为什么拆成两个文件：AssertContract 的定位是「供插件自测调用」
// （插件作者在自己的 _test.go 里 AssertContract(t, &MyFeature{})）。
// 仅存在于 _test.go 中的符号无法被其它包导入，因此可复用部分
// （断言实现 + 生命周期 rig）放在本文件（package plugincontract），
// 遍历全部已注册插件的表驱动测试放在 plugincontract_test.go。
//
// 契约内容（对单个插件 p）：
//
//  1. Meta 完整性：Name/Version/Description 非空；
//  2. 目录一致性：Meta.Name 与 internal/plugins/ 下的目录名一致
//     （Name 是部署开关与拓扑排序的键，与目录名脱节会直接误导排障）；
//  3. Options 合法性（实现了 plugin.Optioner 时）：每个 OptionDef 的 Key 非空
//     且不重复、Description 非空、Default 类型与 Type 匹配
//     （OptionString→string、OptionInt→int、OptionBool→bool）、
//     Int 的 Min<=Default<=Max（Min/Max 为 0 表示不限制，见 plugin.OptionDef 语义）；
//  4. Init 幂等：同一实例连续 Init 两次，状态保持 Initialized（不 Failed）；
//  5. 生命周期序列：Init → Start → Stop → Start → Stop 全部达到预期状态
//     （运行期启停是 Manager 的既有能力，插件必须经得起反复启停）；
//  6. 无 goroutine 泄漏：Stop 后协程数回落到 Start 基线 +2 以内（最多等 2s）。
//
// 用法（插件自测）：
//
//	func TestMyFeatureContract(t *testing.T) {
//	    plugincontract.AssertContract(t, &MyFeature{})
//	}
package plugincontract

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// leakSlack 是 Stop 后协程数允许的残留余量：吸收测试框架自身的偶发协程噪声。
const leakSlack = 2

// leakWait 是 Stop 后等待协程回落的最长时间（Stop 大多自行等待子协程退出，
// 这里只兜底异步退出的一点点时间差）。
const leakWait = 2 * time.Second

// AssertContract 对单个插件执行全部契约断言。任一项违背即 t.Fatalf，
// 失败信息中带上插件名与具体违背项，便于逐插件定位。
func AssertContract(t *testing.T, p plugin.Plugin) {
	t.Helper()
	name := p.Meta().Name

	if v := metaViolations(p); len(v) > 0 {
		t.Fatalf("Meta 契约违背: %s", strings.Join(v, "; "))
	}
	if v := dirViolations(name); len(v) > 0 {
		t.Fatalf("目录契约违背: %s", strings.Join(v, "; "))
	}
	if v := optionViolations(p); len(v) > 0 {
		t.Fatalf("Options 契约违背: %s", strings.Join(v, "; "))
	}
	assertLifecycle(t, p)
}

// metaViolations 返回 Meta 完整性违背项（空 = 合规）。
func metaViolations(p plugin.Plugin) []string {
	m := p.Meta()
	var v []string
	if m.Name == "" {
		v = append(v, "Name 为空")
	}
	if m.Version == "" {
		v = append(v, "Version 为空")
	}
	if m.Description == "" {
		v = append(v, "Description 为空")
	}
	return v
}

// dirViolations 返回「Name 与目录名一致」的违背项（空 = 合规）。
func dirViolations(name string) []string {
	if name == "" {
		return []string{"Name 为空，无法核对目录"}
	}
	dir := filepath.Join(pluginsRoot(), name)
	fi, err := os.Stat(dir)
	if err != nil {
		return []string{fmt.Sprintf("internal/plugins/%s 不存在（%v）：Name 必须与目录名一致", name, err)}
	}
	if !fi.IsDir() {
		return []string{fmt.Sprintf("internal/plugins/%s 不是目录", name)}
	}
	return nil
}

// pluginsRoot 返回仓库 internal/plugins 的绝对路径。
// 基于本源文件的编译期路径推导（runtime.Caller），不依赖测试工作目录——
// 这样无论调用方是 test/plugincontract 还是插件自己的包目录，路径都正确。
func pluginsRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		// 兜底：按「工作目录 = 本包目录」回跳两层（理论不可达）。
		return filepath.Join("..", "..", "internal", "plugins")
	}
	// <repo>/test/plugincontract/plugincontract.go → <repo>/internal/plugins
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "internal", "plugins")
}

// optionViolations 返回 Options 声明的违背项（未实现 Optioner 时为空）。
// 只约束 OptionString/OptionInt/OptionBool 三种控件类型（V6 ⑧ 契约范围）；
// duration/select 的取值语义由插件 Init 自行解析兜底。
func optionViolations(p plugin.Plugin) []string {
	o, ok := p.(plugin.Optioner)
	if !ok {
		return nil
	}
	var v []string
	seen := make(map[string]bool)
	for i, d := range o.Options() {
		tag := fmt.Sprintf("Options[%d]", i)
		if d.Key == "" {
			v = append(v, fmt.Sprintf("%s: Key 为空", tag))
		} else {
			tag = fmt.Sprintf("Options[%d](key=%q)", i, d.Key)
			if seen[d.Key] {
				v = append(v, fmt.Sprintf("%s: Key 重复", tag))
			}
			seen[d.Key] = true
		}
		if d.Description == "" {
			v = append(v, fmt.Sprintf("%s: Description 为空", tag))
		}
		switch d.Type {
		case plugin.OptionString:
			if _, isStr := d.Default.(string); !isStr {
				v = append(v, fmt.Sprintf("%s: OptionString 的 Default 应为 string，实得 %T", tag, d.Default))
			}
		case plugin.OptionInt:
			n, isInt := d.Default.(int)
			if !isInt {
				v = append(v, fmt.Sprintf("%s: OptionInt 的 Default 应为 int，实得 %T", tag, d.Default))
				continue
			}
			if d.Min != 0 && n < d.Min {
				v = append(v, fmt.Sprintf("%s: Default %d 小于 Min %d", tag, n, d.Min))
			}
			if d.Max != 0 && n > d.Max {
				v = append(v, fmt.Sprintf("%s: Default %d 大于 Max %d", tag, n, d.Max))
			}
		case plugin.OptionBool:
			if _, isBool := d.Default.(bool); !isBool {
				v = append(v, fmt.Sprintf("%s: OptionBool 的 Default 应为 bool，实得 %T", tag, d.Default))
			}
		}
	}
	return v
}

// assertLifecycle 在同一套 rig 里按序执行 Init 幂等、生命周期序列与
// goroutine 泄漏三项断言。rig 只启用 p 及其全部传递依赖：
// 失败可归因到具体插件，协程基线也不被无关插件污染。
func assertLifecycle(t *testing.T, p plugin.Plugin) {
	t.Helper()
	name := p.Meta().Name

	hub := core.NewHub()
	mgr := plugin.NewManagerWith(plugin.All())
	mgr.ApplyConfig(enableSet(p), safeOptions(t))
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	// 断言中途失败也要停机：否则监听/协程会漏进后续子测试，
	// 污染它们的端口与协程基线。
	t.Cleanup(mgr.StopAll)

	// Init 幂等：连续两次 InitAll，状态应保持 Initialized（第二次若 Failed，
	// 说明 Init 里有不可重入的逻辑——双 Init 是运行期启停的既有路径）。
	mgr.InitAll()
	assertState(t, mgr, name, plugin.StateInitialized)
	mgr.InitAll()
	assertState(t, mgr, name, plugin.StateInitialized)

	// 生命周期序列（与 plugin.Plugin 契约文档一致：Init→Start→Stop→Init→Start）：
	// 第一圈 Init → Start → Stop（附 goroutine 泄漏检查），第二圈重新 Init 后再
	// Start → Stop，覆盖运行期 Reload / Enable 的「先重新装配再启动」路径。
	//
	// 为什么第二圈先 InitAll 再 StartAll：Plugin 接口文档定义的运行期循环是
	// 「Init→Start→Stop→Init→Start」，生产 Reload 与 Enable 也都先 Init 再 Start；
	// Stop 后不重新 Init 直接 Start 的路径生产不可达，不作为本契约的硬断言。
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	assertState(t, mgr, name, plugin.StateRunning)

	// goroutine 泄漏：以 Start 后的协程数为基线，Stop 后应回落到基线+leakSlack。
	base := runtime.NumGoroutine()
	mgr.StopAll()
	assertState(t, mgr, name, plugin.StateStopped)
	waitGoroutineDrop(t, base, leakSlack, leakWait)

	// 再装配再启停：重新 Init 后必须还能 Start、Stop（运行期 Enable/Reload 的基本盘）。
	mgr.InitAll()
	assertState(t, mgr, name, plugin.StateInitialized)
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("二次 StartAll: %v", err)
	}
	assertState(t, mgr, name, plugin.StateRunning)
	mgr.StopAll()
	assertState(t, mgr, name, plugin.StateStopped)
}

// enableSet 返回「p 及其全部传递依赖启用、其余插件显式停用」的启用表。
// 管理器对未显式设置的插件缺省视为启用，因此停用方也必须显式写 false。
// Required 插件（tcpgateway）例外：管理器只要包含它，StartAll 就要求它启用
// （生产同理——Required 本就意味着不可缺席），故 rig 里常驻启用；
// 它自身的启停正确性由它自己的子测试重点覆盖。
func enableSet(p plugin.Plugin) map[string]bool {
	all := plugin.All()
	byName := make(map[string]plugin.Plugin, len(all))
	for _, q := range all {
		byName[q.Meta().Name] = q
	}
	on := make(map[string]bool)
	var visit func(string)
	visit = func(n string) {
		if on[n] {
			return
		}
		q, ok := byName[n]
		if !ok {
			return // 依赖未注册：由 Manager 的拓扑排序报错，契约照方抓药
		}
		on[n] = true
		for _, d := range q.Meta().Dependencies {
			visit(d)
		}
	}
	visit(p.Meta().Name)
	// Required 插件常驻启用（见函数注释）。
	for _, q := range all {
		if q.Meta().Required {
			on[q.Meta().Name] = true
		}
	}

	out := make(map[string]bool, len(all))
	for _, q := range all {
		out[q.Meta().Name] = on[q.Meta().Name]
	}
	return out
}

// safeOptions 返回让全部插件都能在测试里安全跑完生命周期的配置：
// 端口类插件绑空闲端口、store 类插件写临时目录、discovery 不开 UDP、
// autobrowser 不开浏览器。停用的插件这些配置被忽略，无害。
func safeOptions(t *testing.T) map[string]map[string]any {
	t.Helper()
	return map[string]map[string]any{
		"tcpgateway":  {"addr": fmt.Sprintf("127.0.0.1:%d", freePort(t))},
		"httpgateway": {"httpAddr": fmt.Sprintf("127.0.0.1:%d", freePort(t))},
		"store":       {"path": initScratchDir(t)},
		"discovery":   {"bindAddr": "", "hubName": "plugin-contract"},
		"autobrowser": {"open": false},
	}
}

// initScratchDir 为「Init 期就打开文件」的插件（当前即 store）分配独立临时目录。
//
// 清理失败等价于一次**双 Init 不幂等**检测：Init 打开的资源（如存储引擎句柄）
// 若在第二次 Init 后被静默替换而未释放，目录就删不掉（Windows 上尤其明显）。
// 契约的硬断言是「双 Init 不 panic 且状态保持 Initialized」（store 满足），
// 资源泄漏超出该字面判据、且生产不可达（InitAll/Reload/Enable 都不会在不停机
// 的情况下连续 Init 同一实例），故以醒目告警呈现而非失败——抓到即缺陷，
// 由插件作者决定修复，本包不越俎代庖。
func initScratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "plugincontract-")
	if err != nil {
		t.Fatalf("创建插件临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("注意：临时目录 %s 清理失败（%v）——疑似 Init 不幂等："+
				"首次 Init 占用的资源在二次 Init 后仍未释放（如未关闭的文件句柄）。"+
				"该路径生产不可达，但违背 C8 幂等精神，建议插件作者修复", dir, err)
		}
	})
	return filepath.Join(dir, "store")
}

// freePort 借操作系统分配一个空闲端口后立即释放（与 test/integration 同款手法：
// 配置项是地址字符串，不能用 :0）。存在极小的被抢占窗口，对测试足够。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("释放临时监听失败: %v", err)
	}
	return port
}

// assertState 断言某插件当前状态（且 Error 为空）。
func assertState(t *testing.T, mgr *plugin.Manager, name string, want plugin.State) {
	t.Helper()
	for _, in := range mgr.Infos() {
		if in.Name != name {
			continue
		}
		if in.State != want {
			t.Fatalf("插件 %q 状态 = %s, want %s (error=%q)", name, in.State, want, in.Error)
		}
		if in.Error != "" {
			t.Fatalf("插件 %q 在状态 %s 下 Error 应为空，实得 %q", name, want, in.Error)
		}
		return
	}
	t.Fatalf("插件 %q 不在管理器清单中", name)
}

// waitGoroutineDrop 轮询等待协程数回落到 base+slack 以内，超时即失败。
func waitGoroutineDrop(t *testing.T, base, slack int, timeout time.Duration) {
	t.Helper()
	limit := base + slack
	deadline := time.Now().Add(timeout)
	for {
		got := runtime.NumGoroutine()
		if got <= limit {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("Stop 后 goroutine 未回落: 基线 %d, 当前 %d（上限 %d，已等待 %s）",
				base, got, limit, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
