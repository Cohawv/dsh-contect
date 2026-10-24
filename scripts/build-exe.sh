#!/usr/bin/env bash
# 构建 Windows EXE 交付物，并按「每版本一个文件夹」编目归档：
#
#   dist/releases/<版本号>/
#     1.服务端/      lan-agent-hub.exe（控制台版）、lan-agent-hub-gui.exe（无窗口版）
#     2.前端/        README.txt（控制台已内嵌于服务端二进制，无独立文件；记录当前 SW 版本）
#     3.客户端/      hubctl.exe、hubbench.exe、dsh 插件 tgz（node 可用时自动构建）
#     4.更新说明.md  版本信息 + 变更记录 + 全部文件的 MD5 清单（编目与校验合一）
#
# 零依赖：仅用 Go 工具链 + git（dsh 部分需要 node，缺失时跳过并在更新说明中注明）。
# 用法（Git Bash / WSL / macOS / Linux 均可，交叉编译到 windows/amd64）：
#   bash scripts/build-exe.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# 版本号：git 描述 + 短 sha；无 tag 时回退到 dev+sha
VER="v1.0.0+$(git describe --tags --always 2>/dev/null | head -c 7 || echo dev)"
BT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
LDFLAGS="-s -w -X main.version=$VER -X main.buildTime=$BT"

REL="dist/releases/$VER"
SRV="$REL/1.服务端"
FE="$REL/2.前端"
CLI="$REL/3.客户端"
NOTES="$REL/4.更新说明.md"
mkdir -p "$SRV" "$FE" "$CLI"

echo "version=$VER  build=$BT  -> $REL"

echo "==> 构建服务端·控制台版（内置全部插件 + 内嵌 Web 控制台）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$SRV/lan-agent-hub.exe" ./cmd/hub

echo "==> 构建服务端·无窗口版（-H=windowsgui，双击即弹浏览器 UI）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS -H=windowsgui" -o "$SRV/lan-agent-hub-gui.exe" ./cmd/hub

echo "==> 构建客户端·CLI（独立二进制，经 REST/SSE 与 Hub 通信）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$CLI/hubctl.exe" ./cmd/hubctl

echo "==> 构建客户端·压测工具（独立二进制，走真实协议）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$CLI/hubbench.exe" ./cmd/hubbench

# dsh 插件（Node 侧客户端）：node 缺失或构建失败都不阻塞 Go 交付物，
# 但更新说明里必须如实注明「未构建」，不能让清单看起来比实际完整。
DSH_NOTE="未构建（需要 node >= 22：cd dsh && npm install && npm run build）"
if command -v node >/dev/null 2>&1 && [ -d dsh/node_modules ]; then
  echo "==> 构建客户端·DSH 插件（Node）"
  if (cd dsh && npm run build --silent && npm pack --silent >/dev/null 2>&1); then
    if cp dsh/dsh-plugin-lan-hub-*.tgz "$CLI/" 2>/dev/null; then
      DSH_NOTE="$(ls "$CLI"/dsh-plugin-lan-hub-*.tgz | xargs -n1 basename | head -1)（npm 包，node >= 22）"
    fi
  else
    echo "WARN: dsh 构建失败，客户端编目将注明未构建" >&2
    DSH_NOTE="构建失败（node 存在但 npm run build 报错，见构建日志）"
  fi
fi

# 2.前端：控制台是 //go:embed 内嵌资源，没有独立交付文件。
# 留一个 README 讲清楚这件事 + 记录当前 SW 版本，避免拿到目录的人以为漏打包了。
SWV="$(grep -o "SW_VERSION = '[^']*'" internal/plugins/webconsole/webroot/sw.js 2>/dev/null | grep -o '[0-9.]*' | head -1 || echo unknown)"
cat > "$FE/README.txt" <<EOF
前端（Web 控制台）说明
======================

控制台（index.html / app.js / styles.css / sw.js / 语言包）通过 //go:embed
**内嵌在服务端二进制内部**，不作为独立文件交付——这是「单文件即可部署」设计的
一部分，部署时无需附带任何静态资源。

当前内嵌的 PWA Service Worker 版本：${SWV}
（SW 版本号在每次前端改动时必须 bump，否则老用户的浏览器会继续用旧缓存壳。）

访问方式：启动 1.服务端/ 下的任一 exe，浏览器打开 http://127.0.0.1:8081/ 即可。
EOF

# 4.更新说明：版本信息 + 变更记录 + 全量 MD5（编目与完整性校验合一，
# 接收方拿到目录后可逐文件核对，不必逐个来找 hash）。
{
  echo "# 更新说明 · $VER"
  echo
  echo "- 构建时间：$BT（UTC）"
  echo "- 源码提交：$COMMIT"
  echo "- 本目录：\`$REL\`"
  echo
  echo "## 编目"
  echo
  echo "| 分类 | 内容 |"
  echo "|---|---|"
  echo "| 1.服务端 | lan-agent-hub.exe（控制台版）、lan-agent-hub-gui.exe（无窗口版）；内置全部插件与 Web 控制台 |"
  echo "| 2.前端 | 无独立文件：控制台内嵌于服务端二进制（SW 版本 ${SWV}），见 2.前端/README.txt |"
  echo "| 3.客户端 | hubctl.exe（CLI）、hubbench.exe（压测）、dsh 插件：$DSH_NOTE |"
  echo "| 4.更新说明 | 本文件（含 MD5 清单） |"
  echo
  echo "## 最近变更"
  echo
  git log --oneline -12 2>/dev/null | sed 's/^/- /' || echo "- （git 不可用，略）"
  echo
  echo "## 文件清单与 MD5"
  echo
  echo "| 文件 | 大小 | MD5 |"
  echo "|---|---|---|"
  find "$REL" -type f ! -name "4.更新说明.md" | LC_ALL=C sort | while read -r f; do
    printf '| %s | %s | %s |\n' "${f#$REL/}" "$(du -h "$f" | cut -f1)" "$(md5sum "$f" | cut -d' ' -f1)"
  done
} > "$NOTES"

echo "done -> $REL"
ls -la "$SRV" "$FE" "$CLI"
