#!/usr/bin/env bash
# 构建 Windows EXE 交付物（控制台版 + 无窗口版），并注入版本号与构建时间。
# 零依赖：仅用 Go 工具链 + git，产物为单文件、内置 webroot，无需附带任何文件。
# 用法（Git Bash / WSL / macOS / Linux 均可，交叉编译到 windows/amd64）：
#   bash scripts/build-exe.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# 版本号：git 描述 + 短 sha；无 tag 时回退到 dev+sha
VER="v1.0.0+$(git describe --tags --always 2>/dev/null | head -c 7 || echo dev)"
BT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
LDFLAGS="-s -w -X main.version=$VER -X main.buildTime=$BT"

mkdir -p dist
echo "version=$VER  build=$BT"

echo "==> 构建控制台版 dist/lan-agent-hub.exe"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/lan-agent-hub.exe ./cmd/hub

echo "==> 构建无窗口版 dist/lan-agent-hub-gui.exe（-H=windowsgui，双击即弹浏览器 UI）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS -H=windowsgui" -o dist/lan-agent-hub-gui.exe ./cmd/hub

echo "==> 构建 CLI 版 dist/hubctl.exe（V7 ②：独立二进制，经 REST/SSE 与 Hub 通信）"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/hubctl.exe ./cmd/hubctl

echo "done -> dist/"
ls -la dist/
