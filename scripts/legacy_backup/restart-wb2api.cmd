@echo off
chcp 65001 >nul
cd /d "%~dp0"
setlocal enabledelayedexpansion

rem ── WorkBuddy2API 重启容器脚本 ───────────────────────────────────

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  pause
  exit /b 1
)

echo 重启 workbuddy2api 容器...
docker compose restart
if errorlevel 1 (
  echo [错误] 容器重启失败。
  pause
  exit /b 1
)

echo.
echo 等待服务探活...
for /l %%i in (1,1,8) do (
  curl -s -m 2 --noproxy "*" http://127.0.0.1:17963/healthz >nul 2>&1
  if not errorlevel 1 (
    goto :probe_done
  )
  timeout /t 1 /nobreak >nul
)

:probe_done
echo.
docker compose ps
echo.
echo ------------------------------------------------------------
echo  /healthz 探针响应：
curl -s -m 3 --noproxy "*" http://127.0.0.1:17963/healthz
echo.
echo ------------------------------------------------------------
echo.
echo [成功] 容器已重启完成。
pause
