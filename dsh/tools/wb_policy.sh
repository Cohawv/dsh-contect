#!/usr/bin/env sh
# DSHA / 容器内的快捷入口：把参数直接交给 lanhub-policy.mjs。
#
# 默认策略文件放在 /root/.dsh/ 下（DSHA 的 profile 目录旁边），
# 可用 LAN_HUB_POLICY_FILE 覆盖；插件侧的 config.policyFile 必须指向同一路径。
#
# 示例：
#   sh tools/wb_policy.sh get
#   sh tools/wb_policy.sh set autoRespond=false
#   sh tools/wb_policy.sh set wakeRoom=all maxAutoTurns=10
#   sh tools/wb_policy.sh reset

set -e
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

: "${LAN_HUB_POLICY_FILE:=/root/.dsh/lan-hub.policy.json}"
export LAN_HUB_POLICY_FILE

# 发布包里只有打包后的单文件 CLI（src/ 不进包）；仓库内开发时两处都在，优先用 dist
CLI="$DIR/../dist/lanhub-policy.mjs"
[ -f "$CLI" ] || CLI="$DIR/lanhub-policy.mjs"

exec node "$CLI" "$@"
