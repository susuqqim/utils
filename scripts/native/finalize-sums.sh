#!/usr/bin/env bash
# finalize-sums.sh -- 发布流程的【最后一步】：等所有产物（含 MSI）都落盘后，
# 按【线上资产名】重新生成 release/native/SHA256SUMS，并自校验；任何不一致 → 退出 1。
# 教训（2026-10-17）：SHA256SUMS 若在 MSI 重建前生成，会登记过期哈希，用户 sha256sum -c 看到 MSI FAILED。
set -euo pipefail
DIR="${1:-release/native}"
MSI_GLOB="${DIR}"/workbuddy2api-*-x64.msi
MSI=$(ls -1 $MSI_GLOB 2>/dev/null | head -1 || true)
[ -n "$MSI" ] || { echo "ERR: no MSI in $DIR" >&2; exit 1; }
# 映射：本地文件名 -> 线上资产名（哈希内容不变）
declare -a PAIRS=(
  "$MSI:$(basename "$MSI")"
  "${DIR}/wb2api-windows-amd64.exe:wb2api-core.exe"
  "${DIR}/latest.json:latest.json"
  "${DIR}/更新说明.md:RELEASE-NOTES.md"
)
TMP=$(mktemp); trap 'rm -f "$TMP"' EXIT
: > "$TMP"
for p in "${PAIRS[@]}"; do
  f="${p%:*}"; n="${p##*:}"
  [ -f "$f" ] || { echo "ERR: missing $f" >&2; exit 1; }
  printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$n" >> "$TMP"
done
mv "$TMP" "${DIR}/SHA256SUMS"
echo "==> wrote ${DIR}/SHA256SUMS"
cat "${DIR}/SHA256SUMS"
echo "==> self-check (hash of source files vs listed)"
ok=0; bad=0
for p in "${PAIRS[@]}"; do
  f="${p%:*}"; n="${p##*:}"
  want=$(grep -E "  ${n}$" "${DIR}/SHA256SUMS" | cut -d' ' -f1)
  got=$(sha256sum "$f" | cut -d' ' -f1)
  if [ "$want" = "$got" ]; then ok=$((ok+1)); else bad=$((bad+1)); echo "  FAIL $n"; fi
done
echo "==> ${ok} OK / ${bad} FAILED"
[ "$bad" -eq 0 ] || { echo "ERR: SHA256SUMS self-check failed" >&2; exit 1; }
echo "==> SHA256SUMS ready (${ok}/${ok} OK)"
