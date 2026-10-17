#!/usr/bin/env sh
# ============================================================================
# WorkBuddy2API 原生安装器 —— 无需 Docker、无需 npm
#
#   在线（一条命令）：
#     WB2API_BASE_URL=<托管目录> sh -c 'curl -fsSL "$WB2API_BASE_URL/install.sh" | sh'
#   离线：解压发行目录后执行  ./install.sh
#
#   安装位置（环境变量可覆盖）：
#     WB2API_PREFIX（默认 ~/.local）
#       程序 <prefix>/lib/workbuddy2api/wb2api
#       命令 <prefix>/bin/wb2api
#     WB2API_HOME（默认 ~/.workbuddy2api）
#       config.json / auths / data / logs
# ============================================================================
set -e

BASE_URL="$WB2API_BASE_URL"
PREFIX="$WB2API_PREFIX"
[ -n "$PREFIX" ] || PREFIX="$HOME/.local"
DATA_DIR="$WB2API_HOME"
[ -n "$DATA_DIR" ] || DATA_DIR="$HOME/.workbuddy2api"
LIB_DIR="$PREFIX/lib/workbuddy2api"
BIN_DIR="$PREFIX/bin"

os=$(uname -s 2>/dev/null || echo unknown)
arch=$(uname -m 2>/dev/null || echo unknown)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) echo "[错误] 不支持的系统：$os" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "[错误] 不支持的架构：$arch" >&2; exit 1 ;;
esac
BIN_NAME="wb2api-$os-$arch"
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else echo "[错误] 需要 curl 或 wget" >&2; return 1; fi
}
hash_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else echo ""; fi
}

echo "==> WorkBuddy2API 原生安装（$os/$arch）"
mkdir -p "$LIB_DIR" "$BIN_DIR" "$DATA_DIR/auths" "$DATA_DIR/data" "$DATA_DIR/logs"

# ── 二进制 ────────────────────────────────────────────────────────────────
TMP_BIN="$LIB_DIR/.wb2api.new"
SUM_FILE=""
if [ -f "$SCRIPT_DIR/$BIN_NAME" ]; then
  echo "==> 使用本地二进制：$SCRIPT_DIR/$BIN_NAME"
  cp "$SCRIPT_DIR/$BIN_NAME" "$TMP_BIN"
  SUM_FILE="$SCRIPT_DIR/SHA256SUMS"
elif [ -n "$BASE_URL" ]; then
  echo "==> 下载：$BASE_URL/$BIN_NAME"
  fetch "$BASE_URL/$BIN_NAME" "$TMP_BIN"
  SUM_FILE="$LIB_DIR/.SHA256SUMS"
  fetch "$BASE_URL/SHA256SUMS" "$SUM_FILE" 2>/dev/null || SUM_FILE=""
else
  echo "[错误] 目录内没有 $BIN_NAME，且未设置 WB2API_BASE_URL" >&2
  echo "       在线示例： WB2API_BASE_URL=https://your-host/native sh install.sh" >&2
  exit 1
fi

# 校验清单缺失/不含本文件 → **必须显式失败**（除非用户主动设 WB2A_SKIP_VERIFY=1）：
# 旧实现"有就校验、没有就静默跳过"，等于给"下载源被替换"开了无保护通道，而网关持有全部凭证。
verified=0
if [ -n "$SUM_FILE" ] && [ -f "$SUM_FILE" ]; then
  want=$(grep " $BIN_NAME$" "$SUM_FILE" | awk '{print $1}' | head -1)
  if [ -n "$want" ]; then
    got=$(hash_of "$TMP_BIN")
    if [ "$got" != "$want" ]; then
      echo "[错误] SHA256 校验失败：期望 $want 实得 $got" >&2
      rm -f "$TMP_BIN"
      exit 1
    fi
    echo "==> SHA256 校验通过"
    verified=1
  fi
fi
if [ "$verified" != "1" ] && [ "${WB2A_SKIP_VERIFY:-}" != "1" ]; then
  echo "[错误] 找不到 $BIN_NAME 的 SHA256 校验值（清单缺失或不含该文件）。" >&2
  echo "       确认来源可信后可用 WB2A_SKIP_VERIFY=1 跳过校验。" >&2
  rm -f "$TMP_BIN"
  exit 1
fi
chmod +x "$TMP_BIN"
# 覆盖前停掉正在运行的网关（升级需替换二进制；与 MSI 的 CaStopAppBeforeInstall 对齐）
if command -v "$LIB_DIR/../wb2api" >/dev/null 2>&1 || [ -x "$LIB_DIR/../wb2api" ]; then
  "$LIB_DIR/../wb2api" stop >/dev/null 2>&1 || true
elif pgrep -f 'wb2api-core' >/dev/null 2>&1; then
  echo "==> 检测到运行中的 wb2api-core，尝试停止"
  pkill -f 'wb2api-core' 2>/dev/null || true
  sleep 1
fi
mv "$TMP_BIN" "$LIB_DIR/wb2api"
if [ "$os" = darwin ]; then xattr -dr com.apple.quarantine "$LIB_DIR/wb2api" 2>/dev/null || true; fi

# ── 启动器 ────────────────────────────────────────────────────────────────
if [ -f "$SCRIPT_DIR/wb2api-launcher.sh" ]; then
  cp "$SCRIPT_DIR/wb2api-launcher.sh" "$BIN_DIR/wb2api"
elif [ -n "$BASE_URL" ]; then
  fetch "$BASE_URL/wb2api-launcher.sh" "$BIN_DIR/wb2api"
else
  echo "[错误] 缺少 wb2api-launcher.sh" >&2; exit 1
fi
chmod +x "$BIN_DIR/wb2api"

# ── 开机自启脚本 ──────────────────────────────────────────────────────────
if [ -f "$SCRIPT_DIR/autostart.sh" ]; then
  cp "$SCRIPT_DIR/autostart.sh" "$LIB_DIR/autostart.sh"
elif [ -n "$BASE_URL" ]; then
  fetch "$BASE_URL/autostart.sh" "$LIB_DIR/autostart.sh" || true
fi
[ -f "$LIB_DIR/autostart.sh" ] && chmod +x "$LIB_DIR/autostart.sh" || true

# ── config 模板 ───────────────────────────────────────────────────────────
if [ ! -f "$DATA_DIR/config.json" ]; then
  if [ -f "$SCRIPT_DIR/config.example.json" ]; then
    cp "$SCRIPT_DIR/config.example.json" "$DATA_DIR/config.json"
  elif [ -n "$BASE_URL" ]; then
    fetch "$BASE_URL/config.example.json" "$DATA_DIR/config.json" || true
  fi
  if [ ! -f "$DATA_DIR/config.json" ]; then
    printf '%s\n' '{"listen":"127.0.0.1:17963","api_key":"CHANGE_ME_BEFORE_USE","auth_dir":"./auths","state_file":"./data/state.json"}' > "$DATA_DIR/config.json"
  fi
  sed 's|"listen"[[:space:]]*:[[:space:]]*"[^"]*"|"listen": "127.0.0.1:17963"|' "$DATA_DIR/config.json" > "$DATA_DIR/config.json.tmp"
  mv "$DATA_DIR/config.json.tmp" "$DATA_DIR/config.json"
  echo "==> 已生成配置：$DATA_DIR/config.json（api_key 仍是占位符，请 setkey）"
fi

echo
echo "==> 安装完成"
echo "    命令： $BIN_DIR/wb2api"
echo "    数据： $DATA_DIR"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "    [!] 请把 $BIN_DIR 加入 PATH" ;;
esac
echo
echo "下一步："
echo "    $BIN_DIR/wb2api setkey <你的密钥>"
echo "    $BIN_DIR/wb2api start"
echo "    浏览器打开 http://127.0.0.1:17963/panel/ 登录账号"
