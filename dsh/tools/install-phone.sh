#!/bin/sh
# install-phone.sh —— 一键把 dsh 插件装到指定手机并完成验收。
#
# 这个脚本把手工流程固化下来（2026-10-24 手工做了二十来轮才趟通）：
#   push 包与脚本 → 切到终端页 → 注入执行 → 读回日志 → 停止→启动 → 再验收
#
# 用法：
#   sh install-phone.sh phone1 [版本]
#   sh install-phone.sh phone2 [版本]
#
# 环境变量：
#   ADB     adb 可执行文件路径（默认 adb）
#   SERIAL  覆盖默认设备序列号（无线调试设备每次重开端口都会变）
#   SKIP_RESTART=1  跳过「停止→启动」（只装不重载）
#   SKIP_UI=1       只 push，不做任何界面操作（适合手动装）
#
# 机型坐标是「机型 × DSHA 版本」的函数，换版本可能要重取——
# 跑之前先用 `uiautomator dump` 看一眼 text 的 bounds（见 DSHA_PLAYBOOK §5.1）。

set -e

MODE=${1:-phone1}
VER=${2:-0.7.1}
ADB=${ADB:-adb}
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

case "$MODE" in
  phone1)
    # 华为 YAL-AL10 1080×2340（dsh 0.2.0-rc.2）
    SERIAL=${SERIAL:-ERLDU19613005705}
    NAV_Y=2230; TERM_X=944
    STOP_X=765; STOP_Y=806
    START_X=540; START_Y=608
    NEW_X=918; NEW_Y=538
    ;;
  phone2)
    # 荣耀 SDY-AN00 1200×2652（dsh 0.2.1-rc.2）
    SERIAL=${SERIAL:-192.0.2.202:43829}
    NAV_Y=1600; TERM_X=1050
    STOP_X=851; STOP_Y=886
    START_X=600; START_Y=704
    NEW_X=1020; NEW_Y=602
    ;;
  *)
    echo "用法：sh install-phone.sh <phone1|phone2> [版本]" >&2
    exit 2
    ;;
esac

PKG="dsh-plugin-lan-hub-$VER.tgz"
say() { printf '\n=== %s ===\n' "$1"; }

say "目标"
echo "设备=$SERIAL  版本=$VER  模式=$MODE"

# -------------------------------------------------------------------
# 0) 连上设备（无线调试的端口每次重开都会变，必要时用 SERIAL= 覆盖）
# -------------------------------------------------------------------
case "$SERIAL" in
  *:*) $ADB connect "$SERIAL" 2>&1 | tail -1 ;;
esac
$ADB devices | grep -q "^$SERIAL" || {
  echo "设备不在线：$SERIAL" >&2
  exit 3
}

# -------------------------------------------------------------------
# 1) 推包与脚本
# -------------------------------------------------------------------
say "推送"
$ADB -s "$SERIAL" push "$HERE/$PKG" "/sdcard/$PKG" | tail -1
$ADB -s "$SERIAL" push "$HERE/wb_cfg.sh" /sdcard/wb_cfg.sh | tail -1
$ADB -s "$SERIAL" push "$HERE/wb_export_log.sh" /sdcard/wb_export_log.sh | tail -1

if [ "${SKIP_UI:-0}" = "1" ]; then
  say "已推送（SKIP_UI=1）"
  echo "在手机 DSHA 终端里执行：sh /sdcard/wb_cfg.sh $VER <agentId> <agentName>"
  exit 0
fi

# -------------------------------------------------------------------
# 2) 装包 + 写配置（终端注入）
# -------------------------------------------------------------------
AGENT_ID=${AGENT_ID:-$(echo "$MODE" | sed 's/phone/phone/')}
AGENT_NAME=${AGENT_NAME:-$(echo "$MODE" | sed 's/phone/Phone/')}

say "注入安装命令（agentId=$AGENT_ID agentName=$AGENT_NAME）"
# 切到终端页 → 点「+新建」确保会话有效（DSH 重启后旧终端会话会静默失效）
$ADB -s "$SERIAL" shell input tap "$TERM_X" "$NAV_Y"; sleep 4
$ADB -s "$SERIAL" shell input tap "$NEW_X" "$NEW_Y"; sleep 5
# 聚焦终端并逐段注入（input text 不支持空格，空格用 keyevent 62 拼）
$ADB -s "$SERIAL" shell input tap $((TERM_X == 944 ? 540 : 600)) 1300; sleep 2
for tok in "sh" "62" "/sdcard/wb_cfg.sh" "62" "$VER" "62" "$AGENT_ID" "62" "$AGENT_NAME"; do
  if [ "$tok" = "62" ]; then
    $ADB -s "$SERIAL" shell "/system/bin/input keyevent 62"
  else
    $ADB -s "$SERIAL" shell "/system/bin/input text '$tok'"
  fi
  sleep 1
done
$ADB -s "$SERIAL" shell "/system/bin/input keyevent 66"
echo "已执行，等待安装（约 90 秒）..."
sleep 95

say "安装结果"
$ADB -s "$SERIAL" shell "grep -A2 'profile package.json 里的依赖源' /sdcard/wbcfg.log; grep 'CONNECT_\|\"version\"' /sdcard/wbcfg.log" 2>&1 | grep -v '^\* daemon' || true

# -------------------------------------------------------------------
# 3) 停止 → 启动（必须；只点「重启 Web」不会刷新插件清单）
# -------------------------------------------------------------------
if [ "${SKIP_RESTART:-0}" != "1" ]; then
  say "停止 → 启动"
  $ADB -s "$SERIAL" shell input tap $((TERM_X == 944 ? 105 : 150)) "$NAV_Y"; sleep 3
  $ADB -s "$SERIAL" shell input tap "$STOP_X" "$STOP_Y"; sleep 20
  $ADB -s "$SERIAL" shell input tap "$START_X" "$START_Y"; sleep 110
  echo "已重启（约 110 秒）"
fi

# -------------------------------------------------------------------
# 4) 验收：日志里不能有 did not activate / JsonSchemaError
# -------------------------------------------------------------------
say "导出日志并检查"
$ADB -s "$SERIAL" shell input tap "$TERM_X" "$NAV_Y"; sleep 4
$ADB -s "$SERIAL" shell input tap "$NEW_X" "$NEW_Y"; sleep 5
$ADB -s "$SERIAL" shell input tap $((TERM_X == 944 ? 540 : 600)) 1300; sleep 2
$ADB -s "$SERIAL" shell "/system/bin/input text 'sh'"; sleep 1
$ADB -s "$SERIAL" shell "/system/bin/input keyevent 62"; sleep 1
$ADB -s "$SERIAL" shell "/system/bin/input text '/sdcard/wb_export_log.sh'"; sleep 1
$ADB -s "$SERIAL" shell "/system/bin/input keyevent 66"; sleep 22

$ADB -s "$SERIAL" shell "grep -a 'lan-hub' /sdcard/lanhub.log | tail -6; echo '--- 激活失败信号（应为空）---'; grep -a 'JsonSchemaError\|did not activate\|failed to import' /sdcard/lanhub.log | tail -4; echo '(以上为空即通过)'" 2>&1 | grep -v '^\* daemon' || true

say "完成"
echo "PC 侧复核：curl http://127.0.0.1:8081/api/agents"
echo "（期望看到 $AGENT_ID 在线，且 caps 五项齐全）"
