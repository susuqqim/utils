@echo off
chcp 65001 >nul
cd /d "%~dp0"
setlocal enabledelayedexpansion

rem ── WorkBuddy2API Docker 启动脚本 ────────────────────────────────
rem 服务运行在容器 workbuddy2api 内，宿主机端口 127.0.0.1:17963

where docker >nul 2>&1
if errorlevel 1 (
  echo [错误] 找不到 docker 命令。
  echo        请确认已安装 Docker Desktop，并将其加入 PATH 环境变量。
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

echo [1/3] 启动 workbuddy2api 容器...
docker compose up -d
if errorlevel 1 (
  echo.
  echo [错误] 容器启动失败，请检查 Docker 日志或端口占用情况。
  pause
  exit /b 1
)

echo.
echo [2/3] 等待服务初始化探活 (http://127.0.0.1:17963/healthz)...
set SUCCESS=0
for /l %%i in (1,1,10) do (
  curl -s -m 2 --noproxy "*" http://127.0.0.1:17963/healthz >nul 2>&1
  if not errorlevel 1 (
    set SUCCESS=1
    goto :probe_done
  )
  timeout /t 1 /nobreak >nul
)

:probe_done
echo.
echo ============================================================
echo [3/3] 容器与服务状态
echo ============================================================
docker compose ps
echo.
echo ------------------------------------------------------------
echo  /healthz 探针响应：
curl -s -m 3 --noproxy "*" http://127.0.0.1:17963/healthz
echo.
echo ------------------------------------------------------------
echo.
echo  本地接入地址:  http://127.0.0.1:17963/v1
echo  模型列表查询:  http://127.0.0.1:17963/v1/models
echo.
echo  常用命令：
echo    - 查看账号池状态: status-wb2api.cmd
echo    - 查看实时日志:   logs-wb2api.cmd
echo    - 账号登录授权:   login.cmd
echo    - 重启容器:       restart-wb2api.cmd
echo    - 停止容器:       stop-wb2api.cmd
echo    - 重新构建镜像:   rebuild-wb2api.cmd
echo ============================================================
echo.
pause
