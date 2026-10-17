#!/usr/bin/env bash
# rebuild-offline-bundle.sh —— 离线包（release/workbuddy2api-offline/）整套重建（2026-10)。
#
# 为什么需要脚本而不是手工同步（审查 E 的 P2-5）：
#   该目录是"从活部署目录复制"的独立 Go module，历史上曾整块落后于源码。手工补几个 .go 文件
#   只会让**包内源码 / 包内 MANIFEST / config 模板 / 二进制**四者互相不一致（它当时就已经如此：
#   app.js 有 s.paused 而 Go 侧没有 Paused 字段）⇒ 必须"整套重建"：
#     复制源码 → 重算包内 MANIFEST（排除自身）→ 重建二进制 → 跑能力断言与密钥自检。
#
# 用法：
#   bash scripts/rebuild-offline-bundle.sh            # 全量重建（含 build）
#   bash scripts/rebuild-offline-bundle.sh --check     # 只体检：对**现有**包跑能力断言（不写任何文件）
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."   # 模块根 = workbuddy2api/
SRC="$(pwd)"
DEST="$SRC/../release/workbuddy2api-offline"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

# 1) 能力断言（与 pack-sanitized.sh 的 assert_bundle_features 同源，--check 也用它）
assert_bundle_features() {
  local dir="$1" missing=""
  ck() { grep -q "$2" "$dir/$1" 2>/dev/null || missing="$missing $3"; }
  ck internal/pool/pool.go             SetCreditFloor            "积分保底 credit_floor"
  ck internal/pool/cooldown.go         BlockModelBackoff         "11102 模型级负缓存"
  ck internal/pool/pick.go             floorBlockedForRealmModel "保底判据"
  ck internal/upstream/client.go       normalizeModelRate        "目录倍率兜底"
  ck internal/upstream/client.go       IsRequestParseFailure     "11101 请求级快速失败"
  ck internal/upstream/tool_pairing.go mergeAdjacentToolCalls    "工具调用合帧（11148）"
  ck internal/upstream/sanitize.go     reasoning_content         "思维链字段净化"
  ck internal/scheduler/scheduler.go   includeDisabledInTasks    "保号覆盖禁用号"
  ck internal/upstream/sse.go          WithErrorFrameObserver    "error 帧观测通道"
  ck cmd/server/main.go                ParseServerReadTimeout    "server.read_timeout"
  ck internal/panel/app.js             credit_floor              "面板 credit_floor 映射"
  if [ -n "$missing" ]; then
    echo "[FAIL] 离线包缺少这些能力（源码与包不同代）：$missing" >&2
    return 1
  fi
  echo "[OK] 能力断言通过（11 项）"
}

if [ ! -d "$DEST" ]; then
  echo "[INFO] 离线包目录不存在：$DEST"
  echo "       首次生成请用 scripts/pack-sanitized.sh（它负责从活部署目录建包）；"
  echo "       或先手工准备该目录（至少含 go.mod 与本模块源码树），再跑本脚本。"
  exit 0
fi

if [ "$CHECK_ONLY" = 1 ]; then
  echo "[INFO] 只体检（不写文件）：$DEST"
  assert_bundle_features "$DEST" || exit 1
  # 与源码的一致性抽样：关键文件逐对 diff（不一致即提示需重建）
  diff -rq --exclude=.git --exclude=MANIFEST.sha256 --exclude=bin --exclude=release \
      "$SRC/cmd" "$DEST/cmd" >/dev/null 2>&1 || echo "[WARN] cmd/ 与源码不一致（需整套重建）"
  diff -rq --exclude=.git "$SRC/internal" "$DEST/internal" >/dev/null 2>&1 || echo "[WARN] internal/ 与源码不一致（需整套重建）"
  exit 0
fi

echo "[INFO] 全套重建开始（源：$SRC → 目标：$DEST）"
for d in cmd internal; do
  rm -rf "$DEST/$d"
  cp -r "$SRC/$d" "$DEST/$d"
done
for f in go.mod go.sum config.example.json README.md AGENTS.md 1-start.cmd 2-stop.cmd checkin.sh; do
  [ -f "$SRC/$f" ] && cp "$SRC/$f" "$DEST/$f"
done
echo "[INFO] 源码已刷新；重算包内 MANIFEST（排除自身）"
( cd "$DEST" && find . -type f ! -name 'MANIFEST.sha256' ! -path './.git/*' -print0 \
    | sort -z | xargs -0 sha256sum > MANIFEST.sha256 ) || { echo "[FAIL] 重算 MANIFEST 失败" >&2; exit 1; }
assert_bundle_features "$DEST" || exit 1
echo "[INFO] 下一步（本脚本不代劳，避免误用发布流程）："
echo "       1) 在 $DEST 内执行构建/Docker 打包，产出二进制与镜像；"
echo "       2) 跑 scripts/pack-sanitized.sh 的密钥自检（真 key 精确串不得入包）；"
echo "       3) 用 'bash scripts/rebuild-offline-bundle.sh --check' 复检能力断言与一致性。"
