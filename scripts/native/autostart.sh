#!/usr/bin/env sh
# WorkBuddy2API 开机自启一键开关
#   Linux : systemd --user（无 systemd 时回落 crontab @reboot）
#   macOS : launchd LaunchAgent
# 用法：
#   autostart.sh on       开启开机自启
#   autostart.sh off      关闭
#   autostart.sh status   查看
set -e

ACTION="$1"
[ -n "$ACTION" ] || ACTION=status

# ── 定位启动器与数据目录 ──────────────────────────────────────────────────
BIN="$WB2API_BIN"
if [ -z "$BIN" ]; then
  if command -v wb2api >/dev/null 2>&1; then BIN="$(command -v wb2api)"; fi
fi
if [ -z "$BIN" ]; then
  self_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
  if [ -x "$self_dir/wb2api" ]; then BIN="$self_dir/wb2api"; fi
fi
if [ -z "$BIN" ]; then echo "[错误] 找不到 wb2api 启动器（可用 WB2API_BIN 指定）" >&2; exit 1; fi
BIN="$(CDPATH= cd -- "$(dirname -- "$BIN")" && pwd)/$(basename -- "$BIN")"

DATA_DIR="$WB2API_HOME"
[ -n "$DATA_DIR" ] || DATA_DIR="$HOME/.workbuddy2api"

os=$(uname -s)

# ── Linux ────────────────────────────────────────────────────────────────
UNIT="$HOME/.config/systemd/user/workbuddy2api.service"
CRON_TAG="workbuddy2api-autostart"

has_systemd_user() {
  command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1
}

linux_on() {
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$UNIT" <<UNITEOF
[Unit]
Description=WorkBuddy2API gateway
After=network-online.target

[Service]
Type=simple
WorkingDirectory=$DATA_DIR
ExecStart=$BIN run
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
UNITEOF
  if has_systemd_user; then
    systemctl --user daemon-reload >/dev/null 2>&1 || true
    if systemctl --user enable --now workbuddy2api.service; then
      loginctl enable-linger "$USER" >/dev/null 2>&1 || true
      echo "[OK] 已开启（systemd --user: workbuddy2api.service）"
    else
      echo "[WARN] systemd --user 启用失败；unit 文件已写入：$UNIT"
      echo "       可手动： systemctl --user daemon-reload && systemctl --user enable --now workbuddy2api.service"
    fi
  elif command -v crontab >/dev/null 2>&1; then
    ( crontab -l 2>/dev/null | grep -v "$CRON_TAG" ; printf '@reboot cd %s && %s run >/dev/null 2>&1 # %s\n' "$DATA_DIR" "$BIN" "$CRON_TAG" ) | crontab -
    echo "[OK] 已开启（crontab @reboot）"
  else
    echo "[WARN] 无 systemd --user 也无 crontab；unit 文件已写入：$UNIT"
    echo "       请在支持 systemd 的环境执行： systemctl --user daemon-reload && systemctl --user enable workbuddy2api.service"
  fi
}

linux_off() {
  if has_systemd_user; then
    systemctl --user disable --now workbuddy2api.service >/dev/null 2>&1 || true
  fi
  if command -v crontab >/dev/null 2>&1; then
    crontab -l 2>/dev/null | grep -v "$CRON_TAG" | crontab - 2>/dev/null || true
  fi
  rm -f "$UNIT"
  echo "[OK] 已关闭开机自启"
}

linux_status() {
  if has_systemd_user; then
    if systemctl --user is-enabled workbuddy2api.service >/dev/null 2>&1; then
      echo "enabled (systemd --user)"
    else
      echo "disabled (systemd --user)"
    fi
  elif command -v crontab >/dev/null 2>&1 && crontab -l 2>/dev/null | grep -q "$CRON_TAG"; then
    echo "enabled (crontab @reboot)"
  elif [ -f "$UNIT" ]; then
    echo "unit file present, systemd inactive: $UNIT"
  else
    echo "disabled"
  fi
}

# ── macOS ────────────────────────────────────────────────────────────────
PLIST="$HOME/Library/LaunchAgents/com.workbuddy2api.plist"

mac_on() {
  mkdir -p "$HOME/Library/LaunchAgents" "$DATA_DIR/logs"
  cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.workbuddy2api</string>
  <key>ProgramArguments</key>
  <array><string>$BIN</string><string>run</string></array>
  <key>WorkingDirectory</key><string>$DATA_DIR</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$DATA_DIR/logs/autostart.log</string>
  <key>StandardErrorPath</key><string>$DATA_DIR/logs/autostart.log</string>
</dict>
</plist>
PLISTEOF
  launchctl unload "$PLIST" >/dev/null 2>&1 || true
  launchctl load -w "$PLIST"
  echo "[OK] 已开启（launchd: $PLIST）"
}

mac_off() {
  launchctl unload "$PLIST" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  echo "[OK] 已关闭开机自启"
}

mac_status() {
  if [ -f "$PLIST" ]; then echo "enabled (launchd)"; else echo "disabled"; fi
}

case "$os" in
  Linux)  case "$ACTION" in on) linux_on ;; off) linux_off ;; status) linux_status ;; *) echo "用法: autostart.sh on|off|status"; exit 2 ;; esac ;;
  Darwin) case "$ACTION" in on) mac_on ;; off) mac_off ;; status) mac_status ;; *) echo "用法: autostart.sh on|off|status"; exit 2 ;; esac ;;
  *) echo "[错误] 不支持的系统：$os（Windows 请用 wb2api autostart）" >&2; exit 1 ;;
esac

