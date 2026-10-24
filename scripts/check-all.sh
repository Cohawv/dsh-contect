#!/usr/bin/env bash
# 一键执行全部质量闸门：vet + test + 依赖方向守卫 + 控制台 sim。
# 用法：bash scripts/check-all.sh
#
# V2 引入本脚本，降低"漏跑某一项"的概率（尤其契约测试需要带/不带 -update 各跑一次）。
# V8f 追加第 4 项：控制台是内嵌资源（//go:embed），**改动不经过 Go 编译器**，
# 所以 go test ./... 对 app.js / index.html 的错误一无所知。在此之前前端
# 261 项 sim 只能靠人记得手工跑——门禁没覆盖的东西等于不存在。
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=============================================="
echo "[1/4] go vet ./..."
echo "=============================================="
go vet ./...

echo
echo "=============================================="
echo "[2/4] go test ./..."
echo "=============================================="
go test ./...

echo
echo "=============================================="
echo "[3/4] bash scripts/check-deps.sh"
echo "=============================================="
bash scripts/check-deps.sh

echo
echo "=============================================="
echo "[4/4] 控制台 sim（node，零依赖脚本）"
echo "=============================================="
# node 缺失直接失败而不是跳过：dsh 插件本就要求 node >= 22，
# 这个仓库已经是 Go + Node 混合工程；"跳过"会让前端门禁形同虚设。
if ! command -v node >/dev/null 2>&1; then
  echo "缺少 node（控制台 sim 需要；dsh 插件亦要求 node >= 22）" >&2
  exit 1
fi
# 只跑 *sim.cjs（console-dom.cjs 是共用的 DOM harness，不是测试用例）
for f in internal/plugins/webconsole/console*sim.cjs; do
  printf '  %-28s ' "$(basename "$f")"
  node "$f" >/dev/null
  echo "ok"
done

echo
echo "全部通过 ✅"
