#!/usr/bin/env bash
# sync-to-bundle.sh —— 把 scripts/deploy/ 里的脚本同步进离线交付包目录。
# 交付包目录整体被 .gitignore 忽略（含源码副本/二进制/凭证），故用本脚本单向同步。
# 用法： bash workbuddy2api/scripts/deploy/sync-to-bundle.sh [目标目录=release/workbuddy2api-offline]
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
dst="${1:-$root/release/workbuddy2api-offline}"
[ -d "$dst" ] || { echo "[错误] 目标目录不存在：$dst" >&2; exit 1; }
mkdir -p "$dst/scripts"
n=0
for f in docker-entrypoint.sh wb2api.sh; do
  cp -f "$here/$f" "$dst/$f"; n=$((n+1)); echo "  → $dst/$f"
done
for f in setkey.py agent_login.py; do
  cp -f "$here/$f" "$dst/scripts/$f"; n=$((n+1)); echo "  → $dst/scripts/$f"
done
echo "[OK] 已同步 $n 个脚本到 $dst"
