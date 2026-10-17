#!/usr/bin/env bash
# check-tracked-build-inputs.sh —— 校验"构建输入是否真的入库"（2026-10-03 加）。
#
# 起因（第十三轮审计 F1，高危）：`internal/prompt/defaultprompt.md` 是 `//go:embed` 的输入，
# 却被 `*.md` 规则忽略 ⇒ `git archive HEAD` 出来的纯净副本 **go build 直接失败**
# （pattern defaultprompt.md: no matching files found）。本地能构建只是因为工作区里恰好有它。
#
# 本脚本把这类"本地能过、干净克隆就炸"的坑变成可执行门禁：
#   1. 扫全仓 `//go:embed` 指令（**含测试文件** —— embed 输入不分测试/生产），逐个确认
#      目标存在且**已被 git 跟踪**；
#   2. 支持 `"引号路径含空格"`、`*.通配`、多模式同行（空格分隔）、目录形态、行尾注释。
#
# 用法：  bash workbuddy2api/scripts/check-tracked-build-inputs.sh
# 退出码：0 = 全部入库；1 = 有构建输入游离在 git 之外（禁止发布/打包）
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
ROOT="$(pwd)"
fail=0

# check_one <源文件> <模式>：校验单个 embed 目标。
check_one() {
  local src="$1" pat="$2"
  [ -n "$pat" ] || return 0
  case "$pat" in
    /*) ;;                    # 绝对路径：直接判存在
    *) pat_rel="$pat" ;;
  esac
  local target="$pat"
  case "$target" in
    /*) ;;
    *) target="$(dirname "$src")/$pat" ;;
  esac
  if [ ! -e "$target" ]; then
    echo "[FAIL] $src: go:embed 目标 '$pat' 不存在（$target）" >&2; fail=1; return 0
  fi
  if [ -d "$target" ]; then
    [ -n "$(ls -A "$target" 2>/dev/null)" ] || { echo "[FAIL] $src: 嵌入目录 '$pat' 为空" >&2; fail=1; }
    # 目录形态：只需要"里面确实有文件入库"（git 不跟踪空目录）
    if [ "$(git ls-files "$target" | wc -l)" -eq 0 ]; then
      echo "[FAIL] $src: 嵌入目录 '$target' 内无文件入库（干净克隆会构建失败）" >&2; fail=1
    fi
    return 0
  fi
  # 单文件：必须被 git 跟踪（相对仓库根的路径）
  local rel="$target"
  case "$rel" in "$ROOT"/*) rel="${rel#"$ROOT"/}" ;; esac
  if ! git ls-files --error-unmatch "$rel" >/dev/null 2>&1; then
    echo "[FAIL] $src: 嵌入的 $rel **未被 git 跟踪**（干净克隆将无法构建；用 git add -f 纳管或改 .gitignore）" >&2
    fail=1
  fi
}

# 通配模式展开（相对源文件目录）
check_glob() {
  local src="$1" pat="$2" m
  local dir; dir="$(dirname "$src")"
  # shellcheck disable=SC2086
  for m in $dir/$pat; do
    [ -e "$m" ] || continue
    check_one "$src" "$(basename "$m")"
  done
  [ -e "$dir/${pat//\*/_}" ] && return 0
  # 无匹配即报错（embed 无匹配同样编译失败）
  if ! ls $dir/$pat >/dev/null 2>&1; then
    echo "[FAIL] $src: go:embed 通配 '$pat' 无匹配文件" >&2; fail=1
  fi
}

while IFS= read -r hit; do
  [ -n "$hit" ] || continue
  file=$(printf '%s' "$hit" | cut -d: -f1)
  content=$(printf '%s' "$hit" | cut -d: -f3-)
  raw=$(printf '%s' "$content" | sed 's|.*//go:embed||')
  # ① 引号路径（可含空格）：按行逐个处理，**不能**走 for 的单词切分（那会把空格路径再切开）
  while IFS= read -r q; do
    [ -n "$q" ] || continue
    check_one "$file" "$q"
  done < <(printf '%s' "$raw" | grep -o '"[^"]*"' | tr -d '"' || true)
  # ② 其余模式：剥掉引号段与行尾注释后按空白切分
  rest=$(printf '%s' "$raw" | sed 's|"[^"]*"||g; s|//.*$||')
  for pat in $rest; do
    case "$pat" in
      *'*'*|*'?'*) check_glob "$file" "$pat" ;;
      *)            check_one "$file" "$pat" ;;
    esac
  done
done < <(grep -rn '//go:embed' --include='*.go' . 2>/dev/null)

if [ "$fail" = 0 ]; then
  echo "[OK] 全部 go:embed 构建输入均已入库"
else
  echo "[FAIL] 存在未入库的构建输入 —— 干净克隆/CI 会构建失败，先纳管再提交。" >&2
fi
exit "$fail"
