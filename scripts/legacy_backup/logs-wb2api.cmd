@echo off
chcp 65001 >nul
cd /d "%~dp0"

rem ── WorkBuddy2API 实时日志追踪 ───────────────────────────────────
rem 按 Ctrl + C 退出日志查看

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  pause
  exit /b 1
)

echo 正在追踪 workbuddy2api 容器日志（按 Ctrl+C 可退出）...
echo ============================================================
docker compose logs -f --tail=100 wb2api
