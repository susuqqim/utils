@echo off
rem ===========================================================================
rem  WorkBuddy2API - Show status  (double-click me)
rem  Shows container state, health probe and account pool summary.
rem  ASCII-only on purpose - see 1-start.cmd. Will NOT flash-close.
rem ===========================================================================
chcp 65001 >nul
cd /d "%~dp0"
title WorkBuddy2API - Status

where docker >nul 2>&1
if errorlevel 1 goto :nodocker

echo ============================================================
echo   WorkBuddy2API status
echo ============================================================
echo.
echo -- container ------------------------------------------------
rem Filter by NAME instead of "docker compose ps": compose ps is scoped to
rem THIS folder's compose project, so it shows an empty table when the running
rem container was started from another folder (different project name).
docker ps -a --filter "name=workbuddy2api" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"
echo.
echo -- health probe ---------------------------------------------
curl -s -m 5 --noproxy "*" http://127.0.0.1:17963/healthz
echo.
echo.
echo -- account pool ---------------------------------------------
docker exec workbuddy2api python3 /app/scripts/status.py
if errorlevel 1 (
  echo   Container not running or no account yet.
  echo   Run 1-start.cmd first, then 2-login.cmd to add an account.
)
echo.
echo   Base URL : http://127.0.0.1:17963/v1
echo   API Key  : see "api_key" in config.json
goto :end

:nodocker
echo [ERROR] Docker command not found. Install and start Docker Desktop first.

:end
echo.
echo (Window kept open on purpose. Press any key to close.)
pause >nul
