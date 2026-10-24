#!/bin/sh
# wb_cfg.sh —— 把 lan-hub 插件的配置块改成指定 Agent 身份，并固化依赖源。
#
# 为什么需要它：
#   1) profile 的 cordis.patch.yml 是「顶层 - id: xxx」列表格式，
#      patch 的 config 是**整块替换**而非深合并 → 必须整块重写，不能只改一行；
#   2) `dsh plugin add` 把依赖源记成 file:/sdcard/xxx.tgz，
#      /sdcard 是 Android 侧挂载，容器内 pnpm 再解析不稳 → 固化到 /root/local-plugins/；
#   3) 多台手机必须用不同 agentId，否则在 Hub 上按 id 注册会互相顶掉（坑 19）。
#
# 用法：sh wb_cfg.sh <版本> <agentId> <agentName>
#   例：sh wb_cfg.sh 0.7.0 phone1 Phone1
#
# 环境变量（可选，覆盖默认值）：
#   HOST       Hub 地址，默认 auto（UDP 局域网自动发现）
#   FALLBACK   发现失败时的兜底地址，默认 192.0.2.78
#   PORT       Hub TCP 端口，默认 8080
#   PROFILE    DSHA profile，默认 web

export DSH_HOME=/root/.dsh
export PATH=/usr/local/bin:/usr/bin:/bin:$PATH

VER=${1:-0.7.0}
AGENT_ID=${2:-phone1}
AGENT_NAME=${3:-Phone1}
HOST=${HOST:-auto}
FALLBACK=${FALLBACK:-192.0.2.78}
PORT=${PORT:-8080}
PROFILE=${PROFILE:-web}

ROOT=/root/.dsh/profiles/$PROFILE
P="$ROOT/cordis.patch.yml"
LOG=/sdcard/wbcfg.log

{
  echo "===== 0. 参数 ====="
  echo "ver=$VER agentId=$AGENT_ID agentName=$AGENT_NAME host=$HOST:$PORT fallback=$FALLBACK profile=$PROFILE"

  echo ""
  echo "===== 1. 固化依赖源到容器内部 ====="
  mkdir -p /root/local-plugins
  cp "/sdcard/dsh-plugin-lan-hub-$VER.tgz" "/root/local-plugins/dsh-plugin-lan-hub-$VER.tgz" 2>&1
  ls -la /root/local-plugins/
  # 覆盖安装前必须删目录（坑 17：pnpm 判定「已满足」会跳过）
  rm -rf "$ROOT/node_modules/dsh-plugin-lan-hub"
  dsh plugin --profile "$PROFILE" add "/root/local-plugins/dsh-plugin-lan-hub-$VER.tgz" > /sdcard/wbadd2.log 2>&1
  echo "ADD_EXIT=$?"
  tail -15 /sdcard/wbadd2.log 2>&1
  echo "--- profile package.json 里的依赖源 ---"
  grep 'dsh-plugin-lan-hub' "$ROOT/package.json" 2>&1

  echo ""
  echo "===== 2. 备份并清掉旧的 lan-hub 配置块 ====="
  if [ -f "$P" ]; then
    cp "$P" "/root/cordis.patch.yml.bak.$(date +%s)"
    echo "backup ok"
    # 顶层列表项以 "- " 开头；块内是缩进行
    awk '
      /^- id: lan-hub$/ { skip=1; next }
      /^- / && skip { skip=0 }
      !skip { print }
    ' "$P" > "$P.tmp" && mv "$P.tmp" "$P"
    echo "old lan-hub block removed"
  else
    echo "no patch file, will create"
    mkdir -p "$ROOT"
    : > "$P"
  fi
  # 保证末尾有换行，否则追加会粘到上一行
  [ -s "$P" ] && [ "$(tail -c1 "$P" | od -An -c | tr -d ' ')" != "\n" ] && echo "" >> "$P"

  echo ""
  echo "===== 3. 追加完整配置块（字段写全，整块替换） ====="
  cat >> "$P" <<PATCH_EOF
- id: lan-hub
  name: dsh-plugin-lan-hub
  config:
    host: '$HOST'
    port: $PORT
    agentId: '$AGENT_ID'
    agentName: '$AGENT_NAME'
    heartbeatMs: 15000
    reconnect: true
    requestTimeoutMs: 3000
    inboxLimit: 200
    autoDiscover: true
    discoverPort: 8082
    fallbackHost: '$FALLBACK'
    autoRespond: true
    autoInject: false
    wakeRoom: 'mention'
    batchMs: 800
    maxWaitMs: 3000
    maxAutoTurns: 6
    maxHop: 3
    perPeerPerMin: 8
    globalPerMin: 30
    muteNotice: true
    policyFile: '/root/.dsh/lan-hub.policy.json'
    policyPollMs: 1500
    updateAutoSync: true
    updateAutoFetch: true
    updateAutoStage: true
    updatePublicKey: |
      -----BEGIN PUBLIC KEY-----
      MCowBQYDK2VwAyEAMAH+EyacshC+6Y8/1QPJWYCchINsf/mMjAsOGGDESpo=
      -----END PUBLIC KEY-----
    updateManifest: ''
    updateIntervalMs: 600000
    updateCacheDir: ''
    updateStagedDir: ''
PATCH_EOF
  echo "appended"

  echo ""
  echo "===== 4. 校验 lan-hub 块（应当只有 1 个） ====="
  echo "count=$(grep -c '^- id: lan-hub$' "$P")"
  awk '/^- id: lan-hub$/ {f=1} f && /^- id:/ && !/lan-hub/ {f=0} f {print}' "$P"

  echo ""
  echo "===== 5. YAML 解析校验 ====="
  python3 -c "
import yaml
d = yaml.safe_load(open('$P'))
print('YAML_OK entries=', len(d) if isinstance(d, list) else type(d).__name__)
for e in (d or []):
    if isinstance(e, dict) and e.get('id') == 'lan-hub':
        c = e.get('config', {})
        print('  lan-hub agentId=', c.get('agentId'), 'host=', c.get('host'), 'port=', c.get('port'))
        print('  fields=', len(c))
" 2>&1

  echo ""
  echo "===== 6. 到 Hub 的连通性 ====="
  node -e "const n=require('net');const s=n.connect($PORT,'$FALLBACK',()=>{console.log('CONNECT_OK $FALLBACK:$PORT');s.end()});s.on('error',e=>console.log('CONNECT_FAIL',e.message));setTimeout(()=>process.exit(0),6000)" 2>&1

  echo ""
  echo "===== 7. 装进去的版本 ====="
  grep -o '\"version\"[[:space:]]*:[[:space:]]*\"[^\"]*\"' "$ROOT/node_modules/dsh-plugin-lan-hub/package.json" 2>&1

  echo ""
  echo "===== DONE ====="
  echo "接下来回启动页做「停止 → 启动」，插件配置才会重新加载。"
} > "$LOG" 2>&1
