@echo off
chcp 65001 >nul
cd /d "%~dp0"

rem ── 停止并移除容器（镜像、auths/、data/ 数据都保留）────────────

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  pause
  exit /b 1
)

echo 停止 workbuddy2api 容器...
docker compose down
if errorlevel 1 (
  echo.
  echo [提示] 停止命令返回非零，可能容器本来就没在跑。
)

timeout /t 2 /nobreak >nul
echo.
echo 已停止。数据（auths/ 与 data/）和镜像都还在，start-wb2api.cmd 可再次拉起。
echo.
pause
