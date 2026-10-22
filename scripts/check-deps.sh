#!/usr/bin/env bash
# 依赖方向守卫：模块边界被破坏时立刻失败，而不是靠人记住。
# 用法：bash scripts/check-deps.sh
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0

check() {
  local pkg="$1" pattern="$2" msg="$3"
  if go list -deps "./$pkg" 2>/dev/null | grep -qE "$pattern"; then
    echo "FAIL  $msg"
    echo "      ($pkg 依赖了不该依赖的包)"
    fail=1
  else
    echo "OK    $msg"
  fi
}

echo "检查模块依赖方向..."
check internal/protocol 'lan-agent-hub/internal/(core|transport|adapter|discovery)' 'protocol 是叶子包'
check internal/core     'lan-agent-hub/internal/(transport|adapter|discovery)'     'core 不依赖传输/适配/发现层'
check internal/transport 'lan-agent-hub/internal/(core|adapter|discovery)'        'transport 不依赖业务/适配/发现层'
check internal/discovery 'lan-agent-hub/internal/(core|transport|adapter|protocol)' 'discovery 是叶子包'
# V2 新增：插件化边界守卫
check internal/core     'lan-agent-hub/internal/(plugin|plugins)' 'core 不依赖 plugin/plugins（插件不可反向污染内核）'
check internal/plugin   'lan-agent-hub/internal/plugins'          'plugin（框架）不依赖 plugins（框架与具体插件解耦）'

# V3 新增：adapter 已解散（能力迁入 internal/plugins/*），全仓库不得再出现其 import 路径。
# 只匹配带引号的 import 字面量——注释里提到"历史 internal/adapter/xxx.go"是允许的（用于追溯来源）。
echo
echo "检查 internal/adapter 是否已彻底解散..."
if [ -d internal/adapter ]; then
  echo "FAIL  internal/adapter 目录仍存在（V3 要求已解散，能力应在 internal/plugins/*）"
  fail=1
elif hits=$(grep -rn '"lan-agent-hub/internal/adapter"' --include='*.go' . 2>/dev/null) && [ -n "$hits" ]; then
  echo "FAIL  仍有 Go 源码 import lan-agent-hub/internal/adapter："
  echo "$hits"
  fail=1
else
  echo "OK    adapter 已解散（无目录、无 Go 引用）"
fi


if [ "$fail" -ne 0 ]; then
  echo
  echo "依赖方向违规。请修正 import，或更新 ARCHITECTURE.md 中的边界规则（需评审）。"
  exit 1
fi
echo
echo "全部通过。"
exit 0
