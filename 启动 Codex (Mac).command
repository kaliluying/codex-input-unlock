#!/bin/bash
# ==============================================================================
# Codex / ChatGPT 输入框解锁 —— macOS 专属一键启动脚本
# 功能特性：
# 1. 零环境依赖：优先检测本地已有二进制；若无但有 Go 则本地极速编译；若皆无则自动从 GitHub Release 下载预编译包；
# 2. 静默脱离终端 (--detach)：成功注入解锁断点后，终端窗口在几百毫秒内自动退出，不遮挡桌面；
# 3. 生命周期跟随：当在 ChatGPT 中按 Cmd+Q 彻底退出软件时，后台守护进程自动停止，无需人工维护。
# ==============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
BIN="$ROOT/codex-input-unlock"
RELEASE_REPO="${CODEX_UNLOCK_RELEASE_REPO:-kaliluying/codex-input-unlock}"

pause_on_error() {
  code="$1"
  echo
  echo '启动失败。请查看上方错误信息。'
  read -r -p '按回车关闭此窗口…' _
  exit "$code"
}

trap 'pause_on_error $?' ERR

# 检查当前目录是否已存在编译好的可执行文件
if [[ ! -x "$BIN" ]]; then
  if command -v go >/dev/null 2>&1; then
    echo "正在编译 codex-input-unlock..."
    (cd "$ROOT" && go build -trimpath -ldflags="-s -w" -o "$BIN" .)
  else
    echo "未检测到可执行程序，正在从 GitHub Release 下载预编译版本..."
    arch="$(uname -m)"
    case "$arch" in
      arm64|aarch64) target_arch="arm64" ;;
      x86_64|amd64) target_arch="amd64" ;;
      *) echo "不支持的 CPU 架构: $arch" >&2; exit 1 ;;
    esac
    url="https://github.com/${RELEASE_REPO}/releases/latest/download/codex-input-unlock-darwin-${target_arch}"
    if command -v curl >/dev/null 2>&1; then
      curl -fL --progress-bar "$url" -o "$BIN"
    elif command -v wget >/dev/null 2>&1; then
      wget -q --show-progress "$url" -O "$BIN"
    else
      echo "未找到 curl 或 wget，无法自动下载: $url" >&2
      exit 1
    fi
    chmod 755 "$BIN"
  fi
fi

# 启动并脱离控制台：断点就绪后自动显示 macOS 系统通知并秒退终端窗口
"$BIN" launch --detach


