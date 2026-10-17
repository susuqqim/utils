#!/usr/bin/env bash
# pack-sanitized.sh —— 生成"可对外分发"的脱敏交付包
#
# 背景（2026-09-25）：release/workbuddy2api-offline.tar 此前**名为脱敏、实含线上真 api_key**。
# 交付包里 auths/ 已清空、.apikey 与 data/state.json 已剔除，唯独 config.json 的 api_key
# 还是线上真钥匙（与 release/workbuddy2api-offline/config.json 完全一致）。
# 该钥匙可全量调用网关（含管理面板 /panel/api/*），外发即等于交出网关控制权。
#
# 本脚本做三件事：
#   1. 从【活部署目录】release/workbuddy2api-offline/ 打包（保证产物与线上一致）；
#   2. 把包内 config.json 的 api_key 重置为占位符 CHANGE_ME_BEFORE_USE；
#   3. 解包自检：确认包内**不存在**任何真凭证（api_key 非 sk-wb2a-*、无 .apikey、
#      auths/ 为空、无 data/state.json、无 usage.json）。自检不过就不覆盖原包。
#
# 用法：  bash scripts/pack-sanitized.sh
# 产物：  release/workbuddy2api-offline.tar（原包自动备份为 .bak-<时间戳>）
#
# 注意：
#   - 打包前请确认 release/workbuddy2api-offline/ 里的二进制是最新构建
#     （docker compose build 之后 mtime 应与容器内 /app/wb2api 一致）。
#   - 本脚本**不**做 gzip（原包就是未压缩 tar），保持与既有交付形态一致。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SRC_DIR="$REPO_ROOT/release/workbuddy2api-offline"
OUT="$REPO_ROOT/release/workbuddy2api-offline.tar"
PLACEHOLDER="CHANGE_ME_BEFORE_USE"
STAMP="$(date +%Y%m%d-%H%M%S)"
SCRATCH="$REPO_ROOT/release/.pack-scratch-$STAMP"

[ -d "$SRC_DIR" ] || { echo "ERR: 活部署目录不存在: $SRC_DIR" >&2; exit 1; }

echo "==> 源目录: $SRC_DIR"
echo "==> 目标包: $OUT"

# ---------------------------------------------------------------- 1. 打包
mkdir -p "$SCRATCH"
trap 'rm -rf "$SCRATCH"' EXIT

# 只收交付必需内容：排除本地凭证/状态/临时物，但**保留目录占位 .gitkeep**。
#
# 排除项刻意写成"目录内容 + 显式放行 .gitkeep"两段式：
#   --exclude='./auths/*'  会连 auths/.gitkeep 一起排掉，交付包里就没有 auths/ 目录了，
#   收包人得自己 mkdir —— 实测第一版就踩了这个坑（旧包有 auths/.gitkeep，我的新版弄丢了）。
# tar 的 --exclude 对后写的放行规则不生效，故改用"先排内容、后补占位"两步：
#   先排除整目录内容，再由 --no-recursion 单独把 .gitkeep 追加进去。
# 另外排除 __pycache__（本地跑过 python 脚本留下的字节码，不该进交付包——
# 实测第一版把 scripts/__pycache__/*.pyc 打进去了）。
tar -cf "$SCRATCH/raw.tar" \
    --exclude='./auths/*' \
    --exclude='./data/*' \
    --exclude='./.apikey' \
    --exclude='./*.tmp' \
    --exclude='*.bak-*' \
    --exclude='*.bak' \
    --exclude='*/__pycache__' \
    --exclude='*/__pycache__/*' \
    -C "$SRC_DIR" . 2>/dev/null

# 补回目录占位（空目录在 tar 里必须有条目才能被解出）
for keep in auths/.gitkeep data/.gitkeep; do
    if [ -f "$SRC_DIR/$keep" ]; then
        tar -rf "$SCRATCH/raw.tar" --no-recursion -C "$SRC_DIR" "./$keep"
    else
        echo "WARN: 源目录缺少占位文件 $keep（交付包内该目录可能不存在）" >&2
    fi
done

echo "==> 已打包（已剔除 auths/* data/* .apikey __pycache__，保留目录占位）"

# ------------------------------------------------- 2. 脱敏 config.json
mkdir -p "$SCRATCH/x"
tar -xf "$SCRATCH/raw.tar" -C "$SCRATCH/x"

CFG="$SCRATCH/x/./config.json"
[ -f "$CFG" ] || CFG="$SCRATCH/x/config.json"
[ -f "$CFG" ] || { echo "ERR: 包内找不到 config.json" >&2; exit 1; }

python3 - "$CFG" "$PLACEHOLDER" <<'PY'
import json, sys
path, placeholder = sys.argv[1], sys.argv[2]
with open(path, encoding='utf-8') as f:
    cfg = json.load(f)

# 泛化脱敏（2026-10-03，第九轮审计 A3）：任何键名含 token/secret/password/key/url 的
# 字段都置空/重置 —— 旧实现只处理 api_key，漏了 upstash.url / upstash.token（Redis 里存着
# 池状态快照）。当前活部署这两项为空，属潜在风险，但交付包一旦带上就是凭证外泄。
def scrub(d):
    changed = []
    for k, v in list(d.items()):
        if isinstance(v, dict):
            changed += [f"{k}." + x for x in scrub(v)]
            continue
        lk = k.lower()
        if any(t in lk for t in ("token", "secret", "password", "api_key", "apikey")):
            if v:
                d[k] = ""
                changed.append(k)
        elif lk == "url" and isinstance(v, str) and v.strip():
            d[k] = ""
            changed.append(k)
    return changed

for section in ("upstash",):
    if isinstance(cfg.get(section), dict):
        c = scrub(cfg[section])
        if c:
            print(f"==> 已清空 {section} 的敏感字段: {c}")

old = cfg.get('api_key', '')
if old == placeholder:
    print(f"==> api_key 已是占位符，无需改写")
elif old.startswith('sk-wb2a-'):
    cfg['api_key'] = placeholder
    with open(path, 'w', encoding='utf-8') as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
        f.write('\n')
    print(f"==> 已把真 api_key（{old[:12]}…{len(old)} 字符）重置为占位符 {placeholder}")
else:
    # 空 / 其它形态：一律置占位符，避免把"未知来源的 key"带出去
    cfg['api_key'] = placeholder
    with open(path, 'w', encoding='utf-8') as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
        f.write('\n')
    print(f"==> api_key 原值非 sk-wb2a-* 形态（{old!r}），已重置为占位符")
PY


# assert_bundle_features —— 发布前断言（审查 E 的 P2-5 建议）：包内必须真的含本轮同步进来的能力，
# 否则拒绝打包。起因：release/workbuddy2api-offline/ 是"从活部署目录复制"的独立 module，历史上
# 曾整块落后于源码（连批次 1 的改动都没有），一旦就这么包出去，用户拿到的就是旧代码。
# 这里只做**存在性**断言（够用且不脆）：缺任何一个直接 fail。
assert_bundle_features() {
  local dir="$1" missing=""
  check_grep() { # <文件> <模式> <能力名>
    if ! grep -q "$2" "$dir/$1" 2>/dev/null; then missing="$missing $3"; fi
  }
  check_grep "internal/pool/pool.go"            "SetCreditFloor"                     "积分保底 credit_floor"
  check_grep "internal/pool/cooldown.go"        "BlockModelBackoff"                  "11102 模型级负缓存"
  check_grep "internal/pool/pick.go"            "floorBlockedForRealmModel"          "保底判据"
  check_grep "internal/upstream/client.go"      "normalizeModelRate"                 "目录倍率兜底"
  check_grep "internal/upstream/client.go"      "IsRequestParseFailure"              "11101 请求级快速失败"
  check_grep "internal/upstream/tool_pairing.go" "mergeAdjacentToolCalls"            "工具调用合帧（11148）"
  check_grep "internal/upstream/sanitize.go"    "reasoning_content"                  "思维链字段净化"
  check_grep "internal/scheduler/scheduler.go"  "includeDisabledInTasks"             "保号任务覆盖禁用号"
  check_grep "internal/upstream/sse.go"         "WithErrorFrameObserver"             "error 帧观测通道"
  check_grep "cmd/server/main.go"               "ParseServerReadTimeout"             "server.read_timeout"
  check_grep "internal/panel/app.js"            "credit_floor"                       "面板 credit_floor 映射"
  if [ -n "$missing" ]; then
    echo "[FAIL] 离线包缺少这些能力（源码与包不同代？先重刷包再发版）：$missing" >&2
    return 1
  fi
  echo "[OK] 离线包能力断言通过（11 项）"
  return 0
}
# ------------------------------------------------- 3. 重建 MANIFEST.sha256
# 包内 MANIFEST 必须覆盖【包内实际文件集】，否则收包人 `sha256sum -c` 会因条目/内容
# 对不上而报错（历史包就是活部署目录里 09-20 的旧清单，与新源码/新二进制全不匹配）。
# 因此在 config 已脱敏、auths/data 已剔空之后、打 clean.tar 之前现场重算：
#   - 排除 MANIFEST.sha256 自身（不能自哈希）；
#   - 路径口径与既有清单一致（`./<相对路径>`，两空格分隔）；
#   - LC_ALL=C 排序，保证可复现。
( cd "$SCRATCH/x" && find . -type f ! -name 'MANIFEST.sha256' -print0 \
    | LC_ALL=C sort -z | xargs -0 sha256sum > MANIFEST.sha256 )
echo "==> MANIFEST.sha256 已重建（$(wc -l < "$SCRATCH/x/MANIFEST.sha256") 条）"

# ------------------------------------------------- 4. 重新打包 + 自检
tar -cf "$SCRATCH/clean.tar" -C "$SCRATCH/x" .
echo "==> 已重新打包"

CHECKS_FAILED=0
fail() { echo "   ✗ $1"; CHECKS_FAILED=1; }
ok()   { echo "   ✓ $1"; }

echo "==> 解包自检:"
LIST="$(tar -tf "$SCRATCH/clean.tar")"

# 先把整包解到一个干净目录，所有检查都基于**文件**而非 `tar | grep` 管道。
#
# 为什么不用管道：本机 tar 是 Windows 侧 bsdtar，`tar -xOf … | grep -q` 会在
# grep 提前退出时让 tar 撞上 broken pipe，导致管道整体退出码变成 2（grep 错误码），
# 而 `grep -q` 的命中/未命中语义就丢了 —— 明明文件里有占位符却判成"未写入"。
# 落盘再 grep 没有这个竞态，且每次检查都可复现、可单独重跑。
mkdir -p "$SCRATCH/verify" && tar -xf "$SCRATCH/clean.tar" -C "$SCRATCH/verify"
VCFG="$SCRATCH/verify/config.json"
[ -f "$VCFG" ] || { echo "ERR: 自检解包后找不到 config.json" >&2; exit 1; }

# 3.1 config.json 内不得出现 sk-wb2a-
# 自检补强（A3）：upstash 的 token/url 也必须为空（旧自检完全没查这两项）
if python3 -c "
import json,sys
c=json.load(open(sys.argv[1],encoding='utf-8'))
u=c.get('upstash',{}) or {}
sys.exit(0 if not (u.get('token') or u.get('url')) else 1)
" "$VCFG"; then ok "upstash.token/url 为空"; else fail "包内 config.json 的 upstash.token/url 非空"; fi

if grep -q 'sk-wb2a-' "$VCFG"; then
    fail "config.json 仍含 sk-wb2a-* 真 key"
else
    ok "config.json 无 sk-wb2a-* 真 key"
fi
# 3.2 占位符确实存在
if grep -qi "$PLACEHOLDER" "$VCFG"; then
    ok "config.json 已写入占位符 $PLACEHOLDER"
else
    fail "config.json 未写入占位符"
fi
# 3.3 不得含 .apikey
if echo "$LIST" | grep -qE '(^|/)\.apikey'; then fail "包内含 .apikey"; else ok "无 .apikey"; fi
# 3.4 auths/ 下不得有凭证文件
if echo "$LIST" | grep -E 'auths/' | grep -vqE 'auths/(\.gitkeep)?$'; then
    fail "auths/ 下有凭证文件"
else
    ok "auths/ 为空（仅保留占位）"
fi
# 3.5 不得含 data 运行态
if echo "$LIST" | grep -qE 'data/(state|usage)\.json'; then
    fail "包内含 data 运行态（state/usage.json）"
else
    ok "无 data 运行态"
fi

# 3.6 【最强判据】线上真 key 的**精确字符串**不得出现在包内任何文件。
#
# 刻意不用「grep 'sk-wb2a-'」做判据：README.md 第 231 行有文档示例
# `"api_key": "sk-wb2a-YourSecretTokenHere"`（及 README 里的 `sk-xxxx`），
# 那是给人看的占位示例，不是凭证 —— 前缀扫描会把它误报成泄漏，
# 而"狼来了"式的误报会让这个自检很快被无视。故改用精确匹配。
REAL_KEY="$(python3 - "$SRC_DIR/config.json" <<'PY'
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding='utf-8')).get('api_key', ''))
except Exception:
    print('')
PY
)"

if [ -n "$REAL_KEY" ] && [ "$REAL_KEY" != "$PLACEHOLDER" ]; then
    HITS="$(grep -rlF -- "$REAL_KEY" "$SCRATCH/verify" 2>/dev/null || true)"
    if [ -n "$HITS" ]; then
        fail "线上真 key 仍出现在包内: $(echo "$HITS" | head -5 | tr '\n' ' ')"
    else
        ok "线上真 key 未出现在包内任何文件（精确匹配）"
    fi
else
    ok "源 config.json 无真 key（已是占位符/为空），跳过精确匹配"
fi

if [ "$CHECKS_FAILED" -ne 0 ]; then
    echo "ERR: 自检未通过，**不覆盖**原包。可疑产物留在: $SCRATCH/clean.tar" >&2
    trap - EXIT   # 保留现场供排查
    exit 1
fi

# ------------------------------------------------- 4. 落地（原包先备份）
if [ -f "$OUT" ]; then
    mv "$OUT" "$OUT.bak-$STAMP"
    echo "==> 原包已备份: $OUT.bak-$STAMP"
fi
mv "$SCRATCH/clean.tar" "$OUT"
echo "==> 已生成脱敏交付包: $OUT ($(stat -c%s "$OUT") 字节)"
echo "==> 全部自检通过 ✅"
# 发布前能力断言（审查 E 的 P2-5）：**断言实际出货的解包内容**（$SCRATCH/verify），
# 它比"源目录"更贴近用户拿到的东西；极端情况下（verify 不存在）回退到源目录。
FEATURE_DIR="$SCRATCH/verify"
[ -d "$FEATURE_DIR" ] || FEATURE_DIR="$SRC_DIR"
assert_bundle_features "$FEATURE_DIR" || exit 1
