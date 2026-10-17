@echo off
chcp 65001 >nul
cd /d "%~dp0"

rem ── 查看账号池概览 ──────────────────────────────────────────
rem 优先在容器内调用 /app/scripts/status.py，免除宿主机 Python 依赖。

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  pause
  exit /b 1
)

docker ps --format "{{.Names}}" 2>nul | findstr /x /c:"workbuddy2api" >nul
if errorlevel 1 (
  echo [提示] 容器 workbuddy2api 没在运行，请先执行 start-wb2api.cmd
  pause
  exit /b 1
)

docker exec workbuddy2api python3 /app/scripts/status.py
if errorlevel 1 (
  echo.
  echo [提示] 调用 status.py 失败，打印 /healthz 基础存活状态：
  docker exec workbuddy2api wget -qO- http://127.0.0.1:17963/healthz
)
echo.
pause
