#!/usr/bin/env bash
# wb2api.sh — WorkBuddy2API 离线部署 / 运维一体脚本（Linux / macOS / Windows Git Bash 通用）
#
# 用法：
#   ./wb2api.sh load            导入离线镜像（image/*.tar.gz）
#   ./wb2api.sh up              启动服务
#   ./wb2api.sh down            停止服务（保留 auths/ data/）
#   ./wb2api.sh restart         重启（改完 config.json 后用这个）
#   ./wb2api.sh rebuild         重新构建镜像并重启容器（代码/透传优化更新后用）
#   ./wb2api.sh status          账号池概览
#   ./wb2api.sh health          健康探针
#   ./wb2api.sh login           交互式加账号（浏览器授权）
#   ./wb2api.sh login-url       只生成授权 URL（自动化第一步）
#   ./wb2api.sh login-poll      取 token 并落盘（自动化第二步）
#   ./wb2api.sh logs            跟踪日志
#   ./wb2api.sh signin          手动签到
#   ./wb2api.sh credit          积分日报
#   ./wb2api.sh setkey <KEY>    写入 api_key 到 config.json 并重启
#   ./wb2api.sh doctor          环境自检
#
# Windows 注意：本脚本必须用 Git Bash 运行（PowerShell 用 wb2api.ps1）。
set -uo pipefail

# 解析脚本真实位置（兼容用相对路径/软链接调用；必须在 cd 之前取）
SCRIPT_PATH="$0"
case "$SCRIPT_PATH" in
  */*) : ;;
  *) SCRIPT_PATH="$(command -v -- "$SCRIPT_PATH" 2>/dev/null || printf './%s' "$SCRIPT_PATH")" ;;
esac
SCRIPT_DIR="$(cd "$(dirname "$SCRIPT_PATH")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "$SCRIPT_PATH")"

cd "$SCRIPT_DIR"
# 可用环境变量覆盖（便于同机跑第二个实例 / 规避端口冲突）
#   WB2A_CONTAINER  容器名（默认 workbuddy2api）
#   WB2A_PORT       宿主端口（默认 17963，被占时改这个，同时改 config 无需动listen）
#   WB2A_IMAGE      镜像名（默认 workbuddy2api:offline）
CONTAINER="${WB2A_CONTAINER:-workbuddy2api}"
IMAGE="${WB2A_IMAGE:-workbuddy2api:offline}"
PORT="${WB2A_PORT:-17963}"
BASE="http://127.0.0.1:${PORT}"

# ─── MSYS/Git Bash 路径转换防护（全局，必须）────────────────────────────
# Git Bash 会把参数里以 / 开头的串自动改写成 Windows 路径，且**静默成功**，
# 踩到的三处：
#   1) -v "$(pwd)/auths:/app/auths"  → 容器路径被改写，挂载到错误目录（空目录）
#   2) docker ps --filter "name=^/wb2api-test$" → 过滤串被改写，误判「容器未运行」
#   3) docker exec -w /app ... /app/signin_bin → 工作目录与程序路径被改写
# 因此在 MSYS 环境下全局关闭转换；宿主机路径另用 cygpath -w 显式转 Windows 形式。
MSYS=0
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) MSYS=1 ;; esac
if [ "$MSYS" = "1" ]; then
  export MSYS_NO_PATHCONV=1
  export MSYS2_ARG_CONV_EXCL='*'
fi

to_host_path() {
  if [ "$MSYS" = "1" ] && command -v cygpath >/dev/null 2>&1; then
    cygpath -w "$1"
  else
    printf '%s' "$1"
  fi
}

# docker 包装（保留为统一入口；路径转换已由上面的全局开关处理）
dk() { docker "$@"; }

# ─── 基础工具 ──────────────────────────────────────────────────────────
die() { echo "[错误] $*" >&2; exit 1; }
info() { echo "[信息] $*"; }

need_docker() {
  command -v docker >/dev/null 2>&1 || die "找不到 docker。请先安装并启动 Docker Desktop / Docker Engine。"
  docker info >/dev/null 2>&1 || die "Docker 引擎无响应。请先启动 Docker Desktop。"
  # 提前校验 compose 可用，避免跑到一半才失败
  if ! docker compose version >/dev/null 2>&1 && ! command -v docker-compose >/dev/null 2>&1; then
    die "找不到 docker compose。请升级 Docker Desktop，或安装 docker-compose。"
  fi
}

# docker compose 探测：优先内置子命令（v2），回退独立的 docker-compose（旧版/部分发行版）
COMPOSE=""
dc() {
  if [ -z "$COMPOSE" ]; then
    if docker compose version >/dev/null 2>&1; then
      COMPOSE="docker compose"
    elif command -v docker-compose >/dev/null 2>&1; then
      COMPOSE="docker-compose"
    else
      die "找不到 docker compose（请升级 Docker Desktop，或安装 docker-compose）"
    fi
  fi
  if [ "$COMPOSE" = "docker compose" ]; then docker compose "$@"; else docker-compose "$@"; fi
}

# 读 config.json 的 api_key（无 jq 依赖）
cfg_api_key() {
  sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' config.json 2>/dev/null | head -1
}

container_running() {
  [ -n "$(docker ps --filter "name=^/${CONTAINER}$" --format '{{.Names}}' 2>/dev/null)" ]
}

# curl 探针；本机回环必须绕过代理
probe() {
  if command -v curl >/dev/null 2>&1; then
    curl -s -m 3 --noproxy '*' "$BASE$1" ${2:+-H "$2"}
  else
    docker exec "$CONTAINER" wget -qO- "http://127.0.0.1:${PORT}$1" 2>/dev/null
  fi
}

# ─── 子命令 ────────────────────────────────────────────────────────────
cmd_load() {
  need_docker
  if docker image inspect "$IMAGE" >/dev/null 2>&1 && [ "${1:-}" != "--force" ]; then
    info "镜像 $IMAGE 已存在，跳过导入（要强制重导加 --force）"
    return 0
  fi
  local tgz
  tgz=$(ls -1 image/*.tar.gz 2>/dev/null | head -1)
  [ -n "$tgz" ] || die "找不到 image/*.tar.gz 离线镜像包"
  info "导入离线镜像 $tgz ..."
  gunzip -c "$tgz" | docker load || die "镜像导入失败"
  info "镜像导入完成：$IMAGE"
}

cmd_up() {
  need_docker
  local key; key=$(cfg_api_key)
  if [ -z "$key" ] || [ "$key" = "CHANGE_ME_BEFORE_USE" ]; then
    echo "[警告] config.json 的 api_key 还是占位符，服务将对任何请求放行（留空=不鉴权）。"
    echo "        建议执行： ./wb2api.sh setkey \$(openssl rand -hex 24)   # 或自定义一串密钥"
  fi
  # 镜像未导入则自动导入：否则 compose 会退回从源码 build（需联网拉 golang 镜像与依赖），
  # 在离线交付场景下必然失败。这里先确保离线镜像就位，做到「解压即可用」。
  if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    info "本地没有镜像 $IMAGE，先自动导入离线镜像..."
    cmd_load
  fi
  # 同名容器冲突预检：给出可操作的提示，而不是让 docker 抛原始错误
  if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER" \
     && [ -z "$(docker ps --filter "name=^/${CONTAINER}$" --format '{{.Names}}' 2>/dev/null)" ]; then
    info "发现同名已停止容器 $CONTAINER，将被重建（凭证与状态不受影响）"
  elif docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
    info "容器 $CONTAINER 已在运行，执行就地更新"
  fi
  if ! dc up -d; then
    echo "[错误] 容器启动失败。" >&2
    echo "        若提示 container name \"$CONTAINER\" is already in use：" >&2
    echo "        · 同机已有该名称的容器（可能来自别的项目目录）" >&2
    echo "        · 换名字或端口： WB2A_CONTAINER=wb2api-2 WB2A_PORT=27963 ./wb2api.sh up" >&2
    echo "        · 或确认无误后清理旧容器： docker rm -f $CONTAINER" >&2
    exit 1
  fi
  info "等待服务就绪..."
  local i
  for i in $(seq 1 15); do
    if probe /healthz >/dev/null 2>&1; then break; fi
    sleep 1
  done
  dc ps
  echo
  echo "  Base URL : $BASE/v1"
  echo "  API Key  : ${key:-（空）}"
  echo "  健康探针 : $(probe /healthz)"
}

cmd_down()   { need_docker; dc down; info "已停止（auths/ 与 data/ 保留）"; }
cmd_restart(){ need_docker; dc restart; sleep 2; dc ps; }

cmd_rebuild() {
  need_docker
  local no_cache=0
  for arg in "$@"; do
    if [ "$arg" = "--no-cache" ]; then no_cache=1; fi
  done
  info "正在重新构建 Docker 镜像 (含最新源码与透传优化)..."
  if [ "$no_cache" = "1" ]; then
    dc build --no-cache || die "镜像构建失败"
  else
    dc build || die "镜像构建失败"
  fi
  info "正在重新创建并启动容器..."
  dc up -d --force-recreate || die "容器启动失败"
  info "等待服务就绪..."
  local ready=0
  local i
  for i in $(seq 1 15); do
    if probe /healthz >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  dc ps
  info "容器重构并启动完成！"
  cmd_health
}

cmd_status() {
  need_docker
  container_running || die "容器未运行，请先 ./wb2api.sh up"
  docker exec "$CONTAINER" python3 /app/scripts/status.py || probe /healthz
}

cmd_health() {
  need_docker
  echo "GET $BASE/healthz"
  probe /healthz; echo
}

# 生成授权 URL：在「容器内」跑 login，避开 Windows 无法执行 Linux ELF 的问题
login_url() {
  local realm="${1:-cn}"
  local host_auths; host_auths=$(to_host_path "$PWD/auths")
  mkdir -p auths
  dk run --rm -v "${host_auths}:/app/auths" "$IMAGE" \
     python3 /app/scripts/agent_login.py url "--realm=${realm}"
}

login_poll() {
  local realm="${1:-cn}"
  local host_auths; host_auths=$(to_host_path "$PWD/auths")
  dk run --rm -v "${host_auths}:/app/auths" "$IMAGE" \
     python3 /app/scripts/agent_login.py poll "--realm=${realm}"
}

cmd_login_url() {
  need_docker
  login_url "${1:-cn}" || die "生成授权 URL 失败"
}

cmd_login_poll() {
  need_docker
  local realm="${1:-}"
  if [ -z "$realm" ] && [ -f "auths/.login-state.json" ]; then
    realm=$(python3 -c "import json; print(json.load(open('auths/.login-state.json')).get('realm',''))" 2>/dev/null || true)
  fi
  realm="${realm:-cn}"
  login_poll "$realm" || die "获取 token 失败（浏览器登录可能尚未完成）"
  rm -f auths/.login-state.json
  dc restart >/dev/null 2>&1 && sleep 2
  info "已落盘新账号并重启容器加载"
  cmd_health
}

cmd_login() {
  need_docker
  local realm="${1:-cn}"

  if [ -t 0 ] && [ -z "${1:-}" ]; then
    echo "选择登录版本: 1) 国内版 cn   2) 国际版 global"
    read -rp "输入 1 或 2 [默认 1]: " ans
    case "$ans" in 2|global|Global|GLOBAL) realm="global" ;; *) realm="cn" ;; esac
  fi
  info "realm = $realm"

  local url
  url=$(login_url "$realm") || die "生成授权 URL 失败"
  echo
  echo "============================================================"
  echo "  请在浏览器打开下面的链接完成登录："
  echo
  echo "  $url"
  echo
  if [ "$realm" = "global" ]; then
    echo "  [国际版登录特别提示]"
    echo "  1. 浏览器完成 Google / 邮箱登录后，页面会进入工作区选择界面；"
    echo "  2. 务必在网页上点击选择个人版（Personal）或对应工作区进入；"
    echo "  3. 若首次登录弹出完善国家/地区信息，请选择并提交；"
    echo "  4. 待浏览器页面出现「登录成功 / Return to CLI」后，再回本窗口按 y 继续！"
    echo
  fi
  echo "============================================================"
  # 授权 URL 含一次性 state：写到 0600 的临时文件供用户复制，**登录完成后由 login 子命令删除**
  # （旧实现从不清理，state/URL 长期残留在会被备份/分享的 auths/ 目录里，第九轮审计 A5）。
  umask 077
  printf '%s\n' "$url" > auths/.login-url.txt
  chmod 600 auths/.login-url.txt 2>/dev/null || true
  echo "（链接已存到 auths/.login-url.txt，权限 600；完成授权后请删除）"
  echo
  read -rp "完成登录后按 y 继续: " ok
  if [ "$ok" != "y" ] && [ "$ok" != "Y" ]; then
    rm -f auths/.login-url.txt   # 取消也要清（state 一次性，留着只有泄漏风险）
    die "已取消（重新运行 ./wb2api.sh login 再试）"
  fi
  cmd_login_poll "$realm"
  # 轮询返回即视为本次授权流程结束：清掉含一次性 state 的 URL 文件（第九轮审计 A5）。
  rm -f auths/.login-url.txt
}

cmd_logs() { need_docker; dc logs -f --tail=100 wb2api; }

# 容器内执行辅助二进制（容器在跑用 exec，否则临时起一个）
run_in_image() {
  need_docker
  if container_running; then
    docker exec -w /app "$CONTAINER" "$@"
  else
    local host_auths; host_auths=$(to_host_path "$PWD/auths")
    dk run --rm -v "${host_auths}:/app/auths" -w /app "$IMAGE" "$@"
  fi
}

cmd_signin() { run_in_image /app/signin_bin /app/auths; }
cmd_credit() { run_in_image /app/credit -pretty; }

cmd_setkey() {
  local key="${1:-}"
  # 未给参数时提示输入（第九轮审计 A2）：`setkey <KEY>` 会把密钥留在 shell 历史里
  # （bash history / PowerShell PSReadLine）并且 ps 可见；提示式输入不进历史。
  if [ -z "$key" ]; then
    if [ -t 0 ]; then
      printf '请输入新的 api_key（不回显）: ' >&2
      IFS= read -rs key || true
      printf '\n' >&2
    else
      IFS= read -r key || true   # 管道/重定向：从 stdin 读
    fi
  fi
  [ -n "$key" ] || die "用法: ./wb2api.sh setkey [密钥]   （省略参数则从终端/stdin 读取，不进 shell 历史）"
  [ -f config.json ] || die "找不到 config.json"
  # 密钥经**环境变量**传给 python，不经 argv、不插值进 -c 源码（旧实现两处都做错：
  # argv 会进 ps/历史；docker 分支把值拼进 python 源码，含引号即注入）。
  # 写入也改成 tmp+rename 的原子替换，避免中断留下半截 config.json。
  export WB2A_NEWKEY="$key"
  if command -v python3 >/dev/null 2>&1; then
    python3 "$SCRIPT_DIR/scripts/setkey.py" config.json
  elif docker image inspect "$IMAGE" >/dev/null 2>&1; then
    local host_cfg; host_cfg=$(to_host_path "$PWD/config.json")
    dk run --rm -e WB2A_NEWKEY -v "${host_cfg}:/cfg.json" "$IMAGE" python3 /app/scripts/setkey.py /cfg.json
  else
    unset WB2A_NEWKEY
    die "setkey 需要宿主机 python3，或先导入镜像（./wb2api.sh load）"
  fi
  unset WB2A_NEWKEY
  if container_running; then dc restart >/dev/null && info "容器已重启"; fi
}

cmd_doctor() {
  echo "── 环境自检 ──────────────────────────────"
  printf '%-14s %s\n' "docker" "$(command -v docker || echo '缺失')"
  if command -v docker >/dev/null 2>&1; then
    printf '%-14s %s\n' "引擎" "$(docker info >/dev/null 2>&1 && echo 正常 || echo '未响应（启动 Docker Desktop）')"
    printf '%-14s %s\n' "镜像" "$(docker image inspect "$IMAGE" >/dev/null 2>&1 && echo "$IMAGE 已导入" || echo '未导入（跑 ./wb2api.sh load）')"
    printf '%-14s %s\n' "容器" "$(container_running && echo 运行中 || echo 未运行)"
  fi
  printf '%-14s %s\n' "config.json" "$([ -f config.json ] && echo 存在 || echo 缺失)"
  printf '%-14s %s\n' "api_key" "$(k=$(cfg_api_key); [ -z "$k" ] && echo '空（不鉴权）' || { [ "$k" = "CHANGE_ME_BEFORE_USE" ] && echo '仍是占位符，请 setkey' || echo '已设置'; })"
  printf '%-14s %s\n' "auths" "$(ls -1 auths/workbuddy-*.json 2>/dev/null | wc -l | tr -d ' ') 个账号"
  [ "$MSYS" = "1" ] && printf '%-14s %s\n' "Git Bash" "已检测（已启用路径转换防护）"
}

# 注意：脚本开头已 cd 到自身目录，$0 可能是相对路径而失效，故用绝对路径
usage() { sed -n '2,25p' "$SCRIPT_PATH"; }

main() {
  local cmd="${1:-}"; shift || true
  case "$cmd" in
    load)       cmd_load "$@" ;;
    up)         cmd_up "$@" ;;
    down)       cmd_down ;;
    restart)    cmd_restart ;;
    rebuild|build) cmd_rebuild "$@" ;;
    status|ps)  cmd_status ;;
    health)     cmd_health ;;
    login)      cmd_login "$@" ;;
    login-url)  cmd_login_url "$@" ;;
    login-poll) cmd_login_poll "$@" ;;
    logs)       cmd_logs ;;
    signin)     cmd_signin ;;
    credit)     cmd_credit ;;
    setkey)     cmd_setkey "$@" ;;
    doctor)     cmd_doctor ;;
    ""|-h|--help|help) usage ;;
    *)          die "未知命令：$cmd（./wb2api.sh help 查看用法）" ;;
  esac
}
main "$@"
