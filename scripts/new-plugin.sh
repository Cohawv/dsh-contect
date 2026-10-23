#!/usr/bin/env bash
# new-plugin.sh —— 生成一个新插件的三件套（V6 ⑧ 脚手架）。
#
# 用法：
#   bash scripts/new-plugin.sh myfeature "我的功能描述"
#
#   name   插件名：必须匹配 ^[a-z][a-z0-9]*$（小写字母开头，仅小写字母与数字）。
#          它同时是目录名、包名与 Meta.Name——三者一致是契约测试的硬断言。
#   desc   可选。一句话描述（写入 Meta.Description 与 README 标题）；
#          不能包含双引号/反斜杠/换行（会破坏生成的 Go 字符串字面量）。
#
# 生成物（internal/plugins/<name>/）：
#   <name>.go        Plugin 接口骨架（Meta/Options/Init/Start/Stop/Apply，TODO 标注）
#   <name>_test.go   rig 测试样板 + TestXxxLifecycle 骨架
#   README.md        插件文档模板（功能/依赖/Options/事件/停用/验收清单）
#
# 行为：参数校验；目录已存在则拒绝（不覆盖）；生成后打印「下一步」指引。
# 实现纪律：set -euo pipefail；heredoc 生成文件；脚本自身 LF（仓库统一 eol=lf）。
set -euo pipefail
cd "$(dirname "$0")/.."

NAME="${1:-}"
DESC="${2:-TODO: 一句话描述这个插件（生成时可用第二个参数传入）}"

if [[ $# -lt 1 ]]; then
	echo "用法: bash scripts/new-plugin.sh <name> [\"描述\"]" >&2
	echo "  name  必须匹配 ^[a-z][a-z0-9]*$（同时是目录名、包名与 Meta.Name）" >&2
	exit 2
fi

if [[ ! "$NAME" =~ ^[a-z][a-z0-9]*$ ]]; then
	echo "错误: 插件名 '$NAME' 非法——必须匹配 ^[a-z][a-z0-9]*$（小写字母开头，仅小写字母与数字）" >&2
	exit 2
fi

# Go 关键字不能用作包名（否则生成的包装不了）。
case "$NAME" in
break | case | chan | const | continue | default | defer | else | fallthrough | for | func | go | goto | if | import | interface | map | package | range | return | select | struct | switch | type | var)
	echo "错误: '$NAME' 是 Go 关键字，不能用作插件名/包名" >&2
	exit 2
	;;
esac

if [[ "$DESC" == *'"'* || "$DESC" == *'\'* || "$DESC" == *$'\n'* ]]; then
	echo "错误: 描述不能包含双引号、反斜杠或换行（会破坏生成的 Go 字符串字面量）" >&2
	exit 2
fi

if [[ -e "internal/plugins/$NAME" ]]; then
	echo "错误: internal/plugins/$NAME 已存在——拒绝覆盖（如需重建请先处理旧目录）" >&2
	exit 1
fi

# 类型名 = 首字母大写（单 token 名的唯一确定性变换；不合意可在生成物里全局改名）。
TYPE="$(printf '%s' "${NAME:0:1}" | tr '[:lower:]' '[:upper:]')${NAME:1}"

mkdir -p "internal/plugins/$NAME"

cat >"internal/plugins/$NAME/$NAME.go" <<GO_EOF
// Package ${NAME} —— ${DESC}
//
// 由 scripts/new-plugin.sh 生成（V6 ⑧ 脚手架）。三个必做事项：
//  1. 登记：在 cmd/hub/plugins/plugins.go 增加一行
//     _ "lan-agent-hub/internal/plugins/${NAME}"
//     不登记 = 编译不进二进制（blank import 是功能总清单的唯一入口）；
//  2. 命名：Meta.Name 必须与目录名一致（test/plugincontract 契约测试强制，
//     目录改名要同步改 Name；类型名可按需改名，但别忘同步 init() 与测试）；
//  3. 生命周期：Init 只做装配（不起 goroutine / 不 listen / 不做磁盘 IO），
//     Start/Stop 必须幂等且可反复启停（运行期 Reload/Enable 会重复调用）；
//     全程零外部依赖（铁律：只用标准库 + internal/core|plugin|protocol）。
package ${NAME}

import (
	"fmt"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// ${TYPE} 是 ${NAME} 插件实体。
// TODO: 改成更达意的类型名（同时同步 init() 与 ${NAME}_test.go）。
type ${TYPE} struct {
	hub *core.Hub
	// TODO: 运行期状态字段放这里（Start 持有、Stop 释放；
	// 事件订阅的取消函数也存这里，Stop 时退订，否则停用会泄漏订阅）。
}

// init 编译期注册（重名会直接 panic——启动期暴露，不进运行期）。
func init() { plugin.Register(&${TYPE}{}) }

// Meta 声明插件元信息。Name 是拓扑排序与启用开关的键，必须与目录名一致。
func (p *${TYPE}) Meta() plugin.Meta {
	return plugin.Meta{
		Name:        "${NAME}",
		Version:     "1.0.0",
		Description: "${DESC}",
		DefaultOn:   true,
		// Dependencies: []string{"httpgateway"}, // TODO: 依赖其它插件时取消注释
	}
}

// Options 声明可配置项：控制台设置视图据此自动生成表单（新增插件零前端改动）。
// 无可配置项时可整个删掉本方法（Optioner 是可选接口）。
func (p *${TYPE}) Options() []plugin.OptionDef {
	return []plugin.OptionDef{
		{
			Key:         "greeting",
			Type:        plugin.OptionString,
			Default:     "hello",
			Description: "TODO: 说明这项配置干什么（显示在控制台表单下方）",
		},
	}
}

// Init 装配期：取上下文、读配置、注册路由、订阅事件。
// 禁止起 goroutine / listen / 磁盘 IO——那些都属于 Start（C8）。
func (p *${TYPE}) Init(ctx *plugin.Context) error {
	hub, ok := ctx.Hub().(*core.Hub)
	if !ok {
		return fmt.Errorf("${NAME}: missing *core.Hub in context")
	}
	p.hub = hub

	// TODO: 按需读取配置项（键必须与 Options() 声明一致）。
	greeting := ctx.String("greeting", "hello")
	_ = greeting

	// TODO: 需要 HTTP 端点时取消下面注释（并在 Meta 声明依赖 httpgateway）。
	// if err := ctx.RegisterHTTP("/api/${NAME}", p.handle${TYPE}); err != nil {
	// 	return fmt.Errorf("${NAME}: %w", err)
	// }

	// TODO: 需要订阅事件时取消注释（慢消费一律 SubscribeAsync，不反压热路径；
	// 取消函数存到结构体字段，Stop 时退订）。
	// p.unsub = hub.Bus.SubscribeAsync(protocol.TopicAgentOnline,
	// 	func(topic string, payload any) { /* TODO: 处理事件 */ }, 64)

	return nil
}

// Start 运行期：真正开始工作（listen、ticker、后台循环）。必须可重复调用。
func (p *${TYPE}) Start() error {
	// TODO: 起协程/监听，取消手段（context cancel / done channel）存到 p，Stop 用。
	return nil
}

// Stop 关闭期：释放 Start 持有的资源。必须幂等（重复调用安全）且快速返回
// （管理器有 5s 超时兜底，但别依赖它）。
func (p *${TYPE}) Stop() error {
	// TODO: 取消上下文、关闭监听、等待协程退出（可模仿 sweeper/tcpgateway）。
	return nil
}

// Apply 实现 plugin.Applier（可选接口）：保存设置后热生效。
// 不实现本方法 = 保存成功但如实标注「重启后生效」——绝不假装热生效。
func (p *${TYPE}) Apply(options map[string]any) error {
	// TODO: 逐键热更新；无法热更新的键返回 error，让上层标注 pendingRestart。
	return nil
}
GO_EOF

cat >"internal/plugins/$NAME/${NAME}_test.go" <<TEST_EOF
package ${NAME}

import (
	"testing"

	"lan-agent-hub/internal/core"
	"lan-agent-hub/internal/plugin"
)

// rig 装配 ${NAME}（经 Manager，hub 真实可用）。自持实例以便断言。
// 依赖其它插件时，把依赖也加进 NewManagerWith 列表（否则拓扑排序报缺失依赖）。
func rig(t *testing.T, opts map[string]any) *${TYPE} {
	t.Helper()
	hub := core.NewHub()
	p := &${TYPE}{}
	mgr := plugin.NewManagerWith([]plugin.Plugin{p})
	mgr.ApplyConfig(map[string]bool{"${NAME}": true}, map[string]map[string]any{"${NAME}": opts})
	mgr.SetEventBus(hub.Bus)
	if err := mgr.BuildContext(hub); err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	mgr.InitAll()
	if _, err := mgr.StartAll(); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Cleanup(mgr.StopAll)
	return p
}

// Test${TYPE}Lifecycle：插件生命周期骨架（TODO：补真实断言）。
//
// 提示：test/plugincontract 提供可复用契约断言 AssertContract(t, p)，
// 在自测里调用即可全套检查（Meta / Options / 双 Init 幂等 / 启停序列 /
// goroutine 泄漏），详见 docs/PLUGIN-ARCHITECTURE.md「如何写一个新插件（V6 ⑧）」。
func Test${TYPE}Lifecycle(t *testing.T) {
	p := rig(t, nil)
	_ = p
	// TODO: 断言 Init/Start/Stop 之后的行为（端点响应、内部状态、事件副作用……）。
}
TEST_EOF

cat >"internal/plugins/$NAME/README.md" <<README_EOF
# ${NAME} —— ${DESC}

> 本文件由 scripts/new-plugin.sh 生成。请把占位与 TODO 替换为真实内容；
> 类型名 ${TYPE} 不合意时可全局改名（同步 init() 与测试）。

## 功能

TODO: 这个插件解决什么问题？典型使用场景是什么？不解决什么（范围外）？

## 依赖声明

- 框架：internal/plugin（Plugin / Context / Optioner / Applier）
- 内核：internal/core（只经 ctx.Hub() 取 *core.Hub，仅用其公开方法）
- 契约：internal/protocol（topic、Envelope 等对外契约）
- 铁律：零外部依赖——仅标准库 + 上述 internal 包（C5）

## Options（设置即插件）

控制台设置视图由 Options() 声明自动生成表单；保存只接受已声明键，未知键 400。

| Key | Type | Default | 说明 |
|-----|------|---------|------|
| greeting | string | hello | TODO: 这项配置控制什么 |

## 事件订阅（若需要）

TODO: 订阅哪些 topic？语义是什么？

约定：慢消费一律 hub.Bus.SubscribeAsync（有界队列，绝不反压消息热路径）；
取消函数存结构体字段，Stop 时退订——停用不退订而泄漏订阅是运行期启停的常见坑。

## 停用注意事项

TODO: 停用时会释放什么？进行中的请求/协程如何处理？

框架行为：停用后路由立即 404（闸门），插件实例与路由注册保留以便再启用；
Stop 必须幂等且快速返回（管理器 5s 超时兜底，超时会被阻断重新装配）。

## 验收清单

- [ ] cmd/hub/plugins/plugins.go 已加 blank import（不登记 = 不进二进制）
- [ ] go build ./... 通过
- [ ] go test ./internal/plugins/${NAME}/ -v 通过
- [ ] go test ./test/plugincontract/ -v 通过（Name 与目录名一致、双 Init 幂等、
      启停序列正确、无 goroutine 泄漏、Options 声明合法）
- [ ] 控制台「插件」视图可见本插件；「设置」视图可改并持久化到 hub.json
README_EOF

cat <<NEXT

已生成 internal/plugins/${NAME}/（类型名 ${TYPE}，不合意可全局改名）：
  ${NAME}.go        Plugin 接口骨架（三个必做事项见文件头注释）
  ${NAME}_test.go   rig 样板 + Test${TYPE}Lifecycle 骨架
  README.md         插件文档模板（功能/依赖/Options/事件/停用/验收清单）

下一步：
  1. 登记插件：在 cmd/hub/plugins/plugins.go 加一行
       _ "lan-agent-hub/internal/plugins/${NAME}"
  2. 实现 TODO：${NAME}.go 内所有 TODO 逐条消灭（先读 internal/plugins/webhook/ 参考）
  3. 构建与自测：
       go build ./...
       go test ./internal/plugins/${NAME}/ -v
  4. 跑插件契约（Meta/Options 合法、双 Init 幂等、启停序列、goroutine 泄漏）：
       go test ./test/plugincontract/ -v
  5. 文档与约定：docs/PLUGIN-ARCHITECTURE.md「如何写一个新插件（V6 ⑧）」
NEXT
