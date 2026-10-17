#!/usr/bin/env bash
# build-native.sh —— 构建"无 Docker / 无 npm"原生发行物
#   · 各平台 wb2api 单文件二进制（CGO_ENABLED=0，静态）
#   · 安装器 / 启动器 / config 模板 / SHA256SUMS
# 产物目录： release/native/
# 用法：     GO=/path/to/go bash scripts/native/build-native.sh
set -eo pipefail

GO="$GO"
[ -n "$GO" ] || GO=go

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SRC="$REPO_ROOT/workbuddy2api"
NATIVE_SRC="$SRC/scripts/native"
OUT="$REPO_ROOT/release/native"

TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

# 保护既有发布物（第九轮审计 C3）：$OUT 里可能躺着 MSI / 签名产物 / 上一次的交付目录，
# 无条件 rm -rf 会让"只为重建二进制而跑一次脚本"变成发布物丢失。默认拒绝，除非显式 --force。
if [ -d "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ] && [ "${1:-}" != "--force" ]; then
  echo "[错误] $OUT 非空——本脚本会先清空该目录。若确认要重建（会删除其中的 MSI 等产物），" >&2
  echo "       请显式加 --force：  bash scripts/native/build-native.sh --force" >&2
  echo "       现存内容：" >&2
  ls -1 "$OUT" | head -10 | sed 's/^/         /' >&2
  exit 1
fi
rm -rf "$OUT"
mkdir -p "$OUT"

for t in $TARGETS; do
  os="$(printf '%s' "$t" | cut -d/ -f1)"
  arch="$(printf '%s' "$t" | cut -d/ -f2)"
  name="wb2api-$os-$arch"
  if [ "$os" = "windows" ]; then name="$name.exe"; fi
  # Windows 目标刻意**不用 -s**：剥掉符号表后，火绒（Huorong）会把二进制启发式判为
  # HEUR:VirTool/Obfuscator.a 并连同 go-build 中间产物一起隔离——实测 -s 即触发、
  # -w 不触发（详见路线图 §R）。代价是体积 +2.4MB 左右，换安装可用性。
  ldflags="-s -w"
  if [ "$os" = "windows" ]; then ldflags="-w"; fi
  echo "==> 构建 $t -> $name"
  ( cd "$SRC" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$GO" build \
      -trimpath -ldflags="$ldflags" -o "$OUT/$name" ./cmd/server )
done

cp "$NATIVE_SRC/install.sh"           "$OUT/install.sh"
cp "$NATIVE_SRC/install.ps1"          "$OUT/install.ps1"
cp "$NATIVE_SRC/wb2api-launcher.sh"   "$OUT/wb2api-launcher.sh"
cp "$NATIVE_SRC/wb2api-launcher.cmd"  "$OUT/wb2api-launcher.cmd"
# 行尾归一（2026-10-05）：Windows 批处理必须 CRLF —— LF-only 的 .cmd 会让 cmd.exe
# 解析出错（实测"双击快捷方式什么都不发生"，只闪过路径碎片报错）。
# 依赖 .gitattributes 的 eol=crlf 是主保障，这里再兜一次底（脚本可能在 Linux 侧被改写）。
python3 - "$OUT/wb2api-launcher.cmd" <<'PYEOF_INNER'
import sys
p = sys.argv[1]
d = open(p, 'rb').read()
crlf = d.replace(b'\r\n', b'\n').replace(b'\r', b'\n').replace(b'\n', b'\r\n')
if crlf != d:
    open(p, 'wb').write(crlf)
PYEOF_INNER
cp "$NATIVE_SRC/autostart.sh"         "$OUT/autostart.sh"
cp "$NATIVE_SRC/README-quickstart.md" "$OUT/README-quickstart.md"
cp "$NATIVE_SRC/使用指南.html"      "$OUT/使用指南.html"
cp "$SRC/internal/tray/icon.ico"      "$OUT/wb2api.ico"

# 更新说明（取 工作区文档/ 下最新一份，随包分发给从旧包升级的人）
DOC="$(ls -1t "$REPO_ROOT/工作区文档/"离线包更新说明-*.md 2>/dev/null | head -1)"
[ -n "$DOC" ] && cp "$DOC" "$OUT/更新说明.md" && echo "==> 附更新说明: $DOC"
cp "$SRC/config.example.json"         "$OUT/config.example.json"
chmod +x "$OUT/install.sh" "$OUT/wb2api-launcher.sh"

( cd "$OUT" && sha256sum wb2api-* > SHA256SUMS )

echo
echo "==> 产物：$OUT"
ls -la "$OUT"
