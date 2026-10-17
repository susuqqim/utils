@echo off
chcp 65001 >nul
cd /d "%~dp0"
setlocal enabledelayedexpansion

rem ── WorkBuddy2API Docker 一键重构镜像与启动脚本 ─────────────────────
rem 当修改了 Go 代码、加权策略、提示词或配置时，运行此脚本重新编译并重建容器

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  echo        请确认已安装 Docker Desktop，并让它处于运行状态。
  pause
  exit /b 1
)

docker info >nul 2>&1
if errorlevel 1 (
  echo [错误] Docker 引擎没有响应。
  echo        请先启动 Docker Desktop（托盘鲸鱼图标变绿）再运行本脚本。
  pause
  exit /b 1
)

echo ============================================================
echo   WorkBuddy2API 镜像重构向导 (含 Phase 1/2/3 智能加权调度)
echo ============================================================
echo.
echo  [1] 增量快速重构 (推荐，使用 Docker 缓存，仅编译更新源码，约 10~20 秒)
echo  [2] 完全全新重构 (--no-cache，从头下载并重新构建所有依赖，约 2~3 分钟)
echo.
set "BUILD_MODE=1"
set /p "USER_CHOICE=请输入选项 [1 或 2，默认 1]: "
if "%USER_CHOICE%"=="2" (
  set "BUILD_CMD=docker compose build --no-cache"
  echo.
  echo [模式] 执行完全全新构建 (--no-cache)...
) else (
  set "BUILD_CMD=docker compose build"
  echo.
  echo [模式] 执行增量快速构建 (利用构建缓存)...
)

echo.
echo ============================================================
echo  [1/3] 编译并构建 Docker 镜像...
echo ============================================================
%BUILD_CMD%
if errorlevel 1 (
  echo.
  echo [错误] 镜像构建失败，请检查上方编译输出或网络连接。
  pause
  exit /b 1
)

echo.
echo ============================================================
echo  [2/3] 强制重建并后台启动容器...
echo ============================================================
docker compose up -d --force-recreate
if errorlevel 1 (
  echo.
  echo [错误] 容器启动失败。
  pause
  exit /b 1
)

echo.
echo ============================================================
echo  [3/3] 等待服务自愈权限与探活 (http://127.0.0.1:17963/healthz)...
echo ============================================================
for /l %%i in (1,1,10) do (
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
echo  /healthz 探针测试：
curl -s -m 3 --noproxy "*" http://127.0.0.1:17963/healthz
echo.
echo ------------------------------------------------------------
echo.
echo [成功] workbuddy2api 容器重构并启动成功！
echo.
echo  常用命令：
echo    - 查看账号池状态: status-wb2api.cmd
echo    - 查看实时日志:   logs-wb2api.cmd
echo    - 额度查询:       credit-wb2api.cmd
echo    - 停止容器:       stop-wb2api.cmd
echo ============================================================
echo.
pause
