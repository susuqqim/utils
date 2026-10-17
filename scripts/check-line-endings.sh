#!/usr/bin/env bash
# check-line-endings.sh —— 行尾门禁（2026-10-05 加）。
#
# 起因（用户实测）：桌面快捷方式指向 wb2api.cmd，双击**什么都不发生**，控制台只闪过
# `'ocal'/'f'/'data'` 之类的路径碎片报错 —— 根因是该 .cmd 是 **LF-only** 行尾，
# Windows cmd.exe 解析 LF 批处理会在 if(...)/goto/start 等构造上崩掉。
# 而仓库此前没有 .gitattributes、build-native.sh 又是 `cp` 原样拷贝，
# 于是 Linux 侧编辑出的 LF 版一路被带到 Windows。
#
# 规则（**作用域 = 本模块 `workbuddy2api/`**，脚本自带 cd 到模块根）：
#   · 随包发到 Windows 的批处理（*.cmd / *.bat）必须**每行 CRLF**；
#   · 模块内受版本控制的 *.sh / *.py / *.go / *.js / *.json 必须**无 CR**
#     （CRLF 的 sh 会报 `\r: command not found`；.go/.json 也会污染 diff 与工具链）。
#
# 刻意**不在**范围内的：仓库根 `工作区文档/sw_src-上游原始源码/` 下那批上游**原始**源码快照
# （实测 25 个 CRLF .go）—— 它们是对上游文件的逐字留存，改行尾就失去留存意义；又因为脚本
# 固定在模块根执行 `git ls-files`，天然不会把它们纳入检查。此说明由审查 E 的 P2-2 提出：
# 此前头注只写 `.sh`，与实现（4 类 glob）不一致，容易让人误判"漏报"。。
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
fail=0

check_crlf() {
  local f="$1" total cr
  total=$(wc -l < "$f" 2>/dev/null || echo 0)
  cr=$(grep -c $'\r$' "$f" 2>/dev/null || true)
  cr=${cr:-0}
  if [ "$total" != "$cr" ]; then
    echo "[FAIL] $f: $cr/$total 行以 CRLF 结尾（Windows 批处理必须全部 CRLF，否则 cmd.exe 解析出错）" >&2
    fail=1
  fi
}

check_lf() {
  local f="$1"
  if grep -q $'\r' "$f" 2>/dev/null; then
    echo "[FAIL] $f: 含 CR（Linux/WSL 脚本必须 LF，否则 sh 报 '\\r: command not found'）" >&2
    fail=1
  fi
}

while IFS= read -r f; do [ -f "$f" ] && check_crlf "$f"; done < <(git ls-files '*.cmd' '*.bat')
while IFS= read -r f; do [ -f "$f" ] && check_lf "$f"; done < <(git ls-files '*.sh' '*.py' '*.go' '*.js' '*.json')

if [ "$fail" = 0 ]; then
  echo "[OK] 行尾策略正确（模块内：.cmd/.bat = CRLF；.sh/.py/.go/.js/.json = LF；工作区文档/ 上游快照不在范围）"
else
  echo "[FAIL] 行尾策略被破坏 —— 修法：.gitattributes 已声明 text eol=crlf/lf，重新 checkout 或显式转换。" >&2
fi
exit "$fail"
