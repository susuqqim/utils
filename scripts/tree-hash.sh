#!/usr/bin/env bash
# tree-hash.sh —— 给"当前工作树"算一个稳定指纹（用于把门禁结果绑定到修订）。
#
# 为什么（2026-10-03 第十二轮审计 F1）：审计方与开发方**同时**在同一工作树上跑门禁，
# 于是出现了"红/绿其实对应两个不同版本的代码"——一条假阳性（panic 已被修掉）、
# 一条真阳性（fuzz 语料落盘）。结论：**门禁结果必须绑定修订**，否则没有判据价值。
#
# 用法（门禁前后各跑一次，两次一致才认结果）：
#     H1=$(bash scripts/tree-hash.sh)
#     go build ./... && go vet ./... && go test ./... -count=1
#     H2=$(bash scripts/tree-hash.sh)
#     [ "$H1" = "$H2" ] || echo "⚠️ 门禁期间工作树被修改，结果无效"
#
# 覆盖：已跟踪文件的改动（git diff HEAD）+ 暂存区 + **未跟踪文件的内容**（含 fuzz 语料）。
#
# ⚠️ 能证明什么 / 不能证明什么（第十三轮审计 F2 逐项实测）：
#   能证明：跟踪内容 + 非忽略的未跟踪文件 + index 三项未变（即"这次门禁跑的确实是这份代码"）。
#   不能证明：① 被 .gitignore 覆盖的文件（改 `release/` 里的脚本、`config.json`、`auths/` 指纹**不变**）；
#             ② 权限/mtime/符号链接目标；③ 内容来源可信（它不是签名）。
#   若某次结论依赖被忽略的文件，请把它们移进跟踪目录，或另设校验。
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
{
  git diff HEAD
  git ls-files -s
  # 未跟踪文件逐个哈希内容（排除 .git 与产物目录），保证"新增文件"也会改变指纹。
  git ls-files --others --exclude-standard -z | while IFS= read -r -d '' f; do
    printf '%s\n' "$f"
    sha1sum "$f" 2>/dev/null || true
  done
} | sha1sum | awk '{print $1}'
