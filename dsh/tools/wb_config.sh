#!/bin/sh
# 把 lan-hub 插件指向电脑 Hub（192.0.2.78:8080）
P=/root/.dsh/profiles/web/cordis.patch.yml
LOG=/sdcard/wbconfig.log
{
  echo "===== 1. 备份 ====="
  cp "$P" /root/cordis.patch.yml.bak && echo "backup ok"

  echo ""
  echo "===== 2. 清理旧的 lan-hub 配置（避免重复） ====="
  if grep -q "dsh-plugin-lan-hub" "$P"; then
    # 用 awk 删除从 "- id: lan-hub" 开始到下一个顶层 "- " 之前的块
    awk '
      /^- id: lan-hub$/ { skip=1; next }
      /^- / && skip { skip=0 }
      !skip { print }
    ' /root/cordis.patch.yml.bak > "$P.tmp" && mv "$P.tmp" "$P"
    echo "old block removed"
  else
    echo "no old block"
  fi

  echo ""
  echo "===== 3. 确保文件末尾有换行 ====="
  [ -s "$P" ] && [ "$(tail -c1 "$P" | od -An -c | tr -d ' ')" != "\n" ] && echo "" >> "$P"
  echo "ok"

  echo ""
  echo "===== 4. 追加配置（整块替换，列出全部字段） ====="
  cat >> "$P" <<'PATCH_EOF'
- id: lan-hub
  name: dsh-plugin-lan-hub
  config:
    host: '192.0.2.78'
    port: 8080
    agentId: 'phone-agent'
    agentName: 'PhoneAgent'
    heartbeatMs: 15000
    reconnect: true
    requestTimeoutMs: 3000
    inboxLimit: 200
    autoInject: false
PATCH_EOF
  echo "appended"

  echo ""
  echo "===== 5. 结果 ====="
  cat "$P"

  echo ""
  echo "===== 6. YAML 校验 ====="
  python3 -c "
import yaml,sys
try:
    d=yaml.safe_load(open('$P'))
    print('YAML_OK, entries=',len(d) if isinstance(d,list) else type(d).__name__)
    for e in (d or []):
        if isinstance(e,dict): print('  -',e.get('id') or e.get('insert'))
except Exception as ex:
    print('YAML_FAIL',ex)
" 2>&1

  echo ""
  echo "===== DONE ====="
} > "$LOG" 2>&1
