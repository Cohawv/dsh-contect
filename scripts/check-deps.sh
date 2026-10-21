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

if [ "$fail" -ne 0 ]; then
  echo
  echo "依赖方向违规。请修正 import，或更新 ARCHITECTURE.md 中的边界规则（需评审）。"
  exit 1
fi
echo
echo "全部通过。"
exit 0
