@echo off
rem ===========================================================================
rem  WorkBuddy2API - Stop service  (double-click me)
rem  Accounts (auths\) and state (data\) are preserved.
rem  ASCII-only on purpose - see 1-start.cmd. Will NOT flash-close.
rem ===========================================================================
chcp 65001 >nul
cd /d "%~dp0"
title WorkBuddy2API - Stop

where docker >nul 2>&1
if errorlevel 1 goto :nodocker

echo ============================================================
echo   Stopping WorkBuddy2API
echo ============================================================
echo.
docker compose down
echo.
echo Stopped. Accounts (auths\) and state (data\) are kept.
echo Double-click 1-start.cmd to start it again.
goto :end

:nodocker
echo [ERROR] Docker command not found. Install and start Docker Desktop first.

:end
echo.
echo (Window kept open on purpose. Press any key to close.)
pause >nul
