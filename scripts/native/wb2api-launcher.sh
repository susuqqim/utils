#!/usr/bin/env sh
# WorkBuddy2API 启动器（安装为 <prefix>/bin/wb2api）。
# 前缀无关：二进制与本脚本相对位置固定为 ../lib/workbuddy2api/wb2api。
set -e

self_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
lib="$self_dir/../lib/workbuddy2api/wb2api"
if [ ! -x "$lib" ]; then lib="$self_dir/wb2api"; fi
if [ ! -x "$lib" ]; then echo "[错误] 找不到 wb2api 二进制：$lib" >&2; exit 1; fi
lib_dir=$(dirname -- "$lib")

dir="$WB2API_HOME"
[ -n "$dir" ] || dir="$HOME/.workbuddy2api"
if [ ! -d "$dir" ]; then echo "[错误] 数据目录不存在：$dir（请先运行 install.sh）" >&2; exit 1; fi
cd "$dir"

port=$(grep -oE '"listen"[[:space:]]*:[[:space:]]*"[^"]*"' config.json 2>/dev/null | grep -oE '[0-9]+"' | tr -d '"' | tail -1)
[ -n "$port" ] || port=17963

cmd="$1"
[ -n "$cmd" ] || cmd=start

case "$cmd" in
  start)
    if [ -f run.pid ] && kill -0 "$(cat run.pid)" 2>/dev/null; then
      echo "已在运行 (pid $(cat run.pid))： http://127.0.0.1:$port/panel/"
      exit 0
    fi
    mkdir -p logs
    nohup "$lib" -config config.json >> logs/server.log 2>&1 &
    echo $! > run.pid
    i=0
    while [ $i -lt 30 ]; do
      i=$((i+1)); sleep 1
      curl -s -m 2 --noproxy '*' "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
    done
    echo "已启动 (pid $(cat run.pid))： http://127.0.0.1:$port/panel/"
    ;;
  stop)
    if [ -f run.pid ] && kill "$(cat run.pid)" 2>/dev/null; then rm -f run.pid; echo "已停止"; else echo "未在运行"; fi
    ;;
  status)
    curl -s -m 3 --noproxy '*' "http://127.0.0.1:$port/healthz"; echo
    ;;
  panel) echo "http://127.0.0.1:$port/panel/" ;;
  logs)  tail -f logs/server.log ;;
  setkey)
    key="$2"
    [ -n "$key" ] || { echo "用法: wb2api setkey <KEY>"; exit 1; }
    sed "s|\"api_key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"|\"api_key\": \"$key\"|" config.json > config.json.tmp
    mv config.json.tmp config.json
    echo "已写入 api_key（重启生效：wb2api stop && wb2api start）"
    ;;
  autostart)
    as="$lib_dir/autostart.sh"
    if [ ! -f "$as" ]; then echo "[错误] 找不到 autostart.sh（$as）" >&2; exit 1; fi
    shift
    exec sh "$as" "$@"
    ;;
  run|serve) exec "$lib" -config config.json ;;
  *) exec "$lib" -config config.json "$@" ;;
esac
