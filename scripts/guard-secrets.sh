#!/usr/bin/env bash
# guard-secrets.sh —— 提交前密钥守卫（2026-10-03 加）。
#
# 起因：一次提交前扫描发现**真实 api_key 明文写在两个被跟踪的文档里**，且自初始提交起就在
# git 历史中（`上游能力清单/客户端接入与思考档位适配指南.md`、`备份说明.md`）。
# 教训：.gitignore 挡不住"文档里手抄的密钥"，必须在提交前用**实际值**扫一遍。
#
# guard-secrets:allow
#   ↑ 自豁免标记：本文件登记的就是"要检测的形态"，必然命中自己。
#     任何文件也可以写这行来豁免**通用形态**检查（但**不能**豁免"当前生产 api_key 明文"那一项）。
#
# 用法：
#   bash workbuddy2api/scripts/guard-secrets.sh            # 扫工作区（跳过被 .gitignore 覆盖的文件）
#   bash workbuddy2api/scripts/guard-secrets.sh --staged   # 只扫**暂存内容**（推荐作为 pre-commit）
# 退出码：0 = 干净；1 = 发现疑似真实凭证（禁止提交）
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

MODE="${1:-worktree}"
CFG="${WB2A_LIVE_CONFIG:-/mnt/c/Users/GG/.workbuddy2api/config.json}"
fail=0

# file_list：本次要检查的文件清单。
#   --staged：暂存区新增/修改/改名/复制的文件（按**暂存内容**检查，不看工作区）
#   默认    ：工作区全部文本文件，**排除被 .gitignore 覆盖的**（那些不可能进提交）
file_list() {
  if [ "$MODE" = "--staged" ]; then
    git diff --cached --name-only --diff-filter=ACMR
  else
    grep -rIl "" --exclude-dir=.git --exclude-dir=.checkpoints --exclude-dir=backups . 2>/dev/null |
      while IFS= read -r f; do git check-ignore -q "$f" 2>/dev/null || printf '%s\n' "$f"; done
  fi
}

# file_content：取"将要提交的内容"（暂存版）或工作区内容。
file_content() {
  if [ "$MODE" = "--staged" ]; then git show ":$1" 2>/dev/null; else cat "$1" 2>/dev/null; fi
}

# PLACEHOLDER_RE：允许出现在文档里的占位符/示例（不视为泄漏）。
# 只对"通用形态"生效；"当前生产 api_key 明文及编码变体"永不白名单。
PLACEHOLDER_RE='^(sk-wb2a-CHANGE_ME_BEFORE_USE|sk-wb2a-YourSecretTokenHere|sk-wb2a-\*\*\*|sk-wb2a-\.\.\.|sk-xxxx|sk-xxx)$'

# check_all：**单趟**遍历文件（每个文件只取一次内容，再跑全部模式）——
# 早期实现是"每个模式 × 每个文件"各起一次 git show，上百个暂存文件时要跑几分钟。
# PATTERNS：每行 "模式<TAB>说明<TAB>级别(fail|warn)"
check_all() {
  local pats_file="$1" tmp f line pat desc level hits=""
  tmp=$(mktemp)
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    file_content "$f" > "$tmp" 2>/dev/null || continue
    allowed=0
    grep -qF 'guard-secrets:allow' "$tmp" 2>/dev/null && allowed=1
    while IFS=$'\t' read -r pat desc level; do
      [ -n "$pat" ] || continue
      # 豁免只对通用形态（warn）生效；"当前生产 api_key 明文"（fail）永远检查。
      [ "$allowed" = 1 ] && [ "$level" = "warn" ] && continue
      # 模式既可能是固定串（活密钥）也可能是正则（形态）→ 用 -E 统一处理；
      # 通用形态允许占位符白名单（文档里的示例不该天天误报）。
      hit=$(grep -oE -- "$pat" "$tmp" 2>/dev/null | sort -u | head -20)
      if [ -n "$hit" ]; then
        if [ "$level" = "warn" ]; then
          hit=$(printf '%s\n' "$hit" | grep -vE "$PLACEHOLDER_RE" | head -1)
          [ -n "$hit" ] || continue
        fi
        echo "[${level^^}] $desc 命中：$f" >&2
        fail=1 # fail/warn 一律阻断：宁可让人复核一次误报，也不要漏放真实凭证
      fi
    done < "$pats_file"
  done < <(file_list)
  rm -f "$tmp"
}

# 1) 组装模式表：已知活密钥（若可读）+ 通用凭证形态
PATS=$(mktemp)
if [ -f "$CFG" ]; then
  KEY=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1],encoding='utf-8')).get('api_key',''))" "$CFG" 2>/dev/null || true)
  if [ -n "${KEY:-}" ]; then
    printf '%s\t%s\tfail\n' "$KEY" "**当前生产 api_key 明文**（长度 ${#KEY}）" >> "$PATS"
    KEY_HINT="钥匙已按实际值参与扫描（长度 ${#KEY}）"
    # 编码变体（第十三轮审计 F3 建议）：明文比对挡不住 base64/hex 抄写，成本却极低。
    b64=$(printf '%s' "$KEY" | base64 -w0 2>/dev/null || printf '%s' "$KEY" | base64 2>/dev/null | tr -d '\n')
    [ -n "${b64:-}" ] && printf '%s\t%s\tfail\n' "$b64" "**当前 api_key 的 base64 变体**" >> "$PATS"
    hexv=$(printf '%s' "$KEY" | xxd -p 2>/dev/null | tr -d '\n')
    [ -n "${hexv:-}" ] && printf '%s\t%s\tfail\n' "$hexv" "**当前 api_key 的 hex 变体**" >> "$PATS"
  fi
fi
add_pat() { # pat desc level [positive-sample]
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >> "$PATS"
  if [ -n "${4:-}" ]; then
    if ! printf '%s' "$4" | grep -qE -- "$1" 2>/dev/null; then
      echo "[FAIL] 守卫自检：模式 /$1/ 匹配不了正样例 '$4' —— 模式已失效（写错转义/语法），守卫形同虚设" >&2
      SELFTEST_FAIL=1
    fi
  fi
}

add_pat 'ghp_[A-Za-z0-9]{20,}' 'GitHub token 形态' warn 'ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789'
add_pat 'sk-[A-Za-z0-9]{32,}' 'OpenAI 风格密钥形态' warn 'sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcd'
add_pat 'sk-wb2a-[A-Za-z0-9_-]{16,}' '本项目 api_key 形态（sk-wb2a-…）' warn 'sk-wb2a-9f3c1d2e4b5a6c7d8e9f0a1b'
add_pat 'AKIA[0-9A-Z]{16}' 'AWS AccessKey 形态' warn 'AKIAIOSFODNN7EXAMPLE'
add_pat '-----BEGIN [A-Z ]*PRIVATE KEY-----' '私钥文件内容' warn '-----BEGIN RSA PRIVATE KEY-----'
add_pat '"access_token"[[:space:]]*:[[:space:]]*"[A-Za-z0-9._-]{20,}"' '明文 access_token 字段' warn '"access_token": "abcdefghijklmnopqrstuvwxyz012345"'
add_pat '"refresh_token"[[:space:]]*:[[:space:]]*"[A-Za-z0-9._-]{20,}"' '明文 refresh_token 字段' warn '"refresh_token": "abcdefghijklmnopqrstuvwxyz012345"'

if [ "${SELFTEST_FAIL:-0}" != 0 ]; then
  echo "[FAIL] 守卫自检未通过：上面的模式不可用，先修模式再看结论。" >&2
  rm -f "$PATS"
  exit 1
fi

check_all "$PATS"
rm -f "$PATS"

if [ "$fail" = 0 ]; then
  echo "[OK] 未发现疑似真实凭证（模式：$MODE${KEY_HINT:+；$KEY_HINT}）"
else
  echo "[FAIL] 存在疑似真实凭证 —— 禁止提交/推送，先清理。" >&2
fi
exit "$fail"
