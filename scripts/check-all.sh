#!/usr/bin/env bash
# 一键执行全部质量闸门：vet + test + 依赖方向守卫。
# 用法：bash scripts/check-all.sh
#
# V2 引入本脚本，降低"漏跑某一项"的概率（尤其契约测试需要带/不带 -update 各跑一次）。
set -euo pipefail
cd "$(dirname "$0")/.."

echo "=============================================="
echo "[1/3] go vet ./..."
echo "=============================================="
go vet ./...

echo
echo "=============================================="
echo "[2/3] go test ./..."
echo "=============================================="
go test ./...

echo
echo "=============================================="
echo "[3/3] bash scripts/check-deps.sh"
echo "=============================================="
bash scripts/check-deps.sh

echo
echo "全部通过 ✅"
