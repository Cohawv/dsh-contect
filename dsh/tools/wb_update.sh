#!/usr/bin/env sh
# DSHA / 容器内的更新通道快捷入口：把参数交给 lanhub-update.mjs。
#
# 默认清单地址指向 Hub 的 HTTP 端口（:8081），可用 LAN_HUB_UPDATE_MANIFEST 覆盖；
# 缓存默认 /root/.dsh/updates，可用 LAN_HUB_UPDATE_CACHE 覆盖。
#
# 典型流程（贴合手机上的真实用法）：
#   sh tools/wb_update.sh check                 # 看有没有新版
#   sh tools/wb_update.sh fetch                 # 下载并校验到缓存（不动安装）
#   sh tools/wb_update.sh apply --yes           # 安装（装完必须重启宿主才生效）
#   sh tools/wb_update.sh rollback --yes        # 出问题回滚到上一个缓存版本
#
# 注意：--yes 是刻意的门槛。更新会替换插件代码，不该在无人值守时自动发生。

set -e
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

: "${LAN_HUB_UPDATE_MANIFEST:=http://127.0.0.1:8081/api/updates}"
: "${LAN_HUB_UPDATE_CACHE:=/root/.dsh/updates}"
export LAN_HUB_UPDATE_MANIFEST LAN_HUB_UPDATE_CACHE

# 发布包里只有打包后的单文件 CLI；仓库内开发时优先用 dist
CLI="$DIR/../dist/lanhub-update.mjs"
[ -f "$CLI" ] || CLI="$DIR/lanhub-update.mjs"

exec node "$CLI" "$@"
