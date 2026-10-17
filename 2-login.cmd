@echo off
rem ===========================================================================
rem  WorkBuddy2API - Add account / OAuth login  (double-click me)
rem
rem  Flow:
rem    1. An authorization URL is printed
rem    2. Open it in your browser and sign in
rem    3. Come back to this window and press y
rem
rem  Run this again to add more accounts (2+ recommended, avoids single point
rem  of failure when one account gets rate-limited).
rem
rem  ASCII-only on purpose - see 1-start.cmd for the reason.
rem  The window will NOT flash-close.
rem ===========================================================================
chcp 65001 >nul
cd /d "%~dp0"
title WorkBuddy2API - Add Account

where docker >nul 2>&1
if errorlevel 1 goto :nodocker

docker info >nul 2>&1
if errorlevel 1 goto :noengine

where powershell >nul 2>&1
if errorlevel 1 goto :nops

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0wb2api.ps1" login
goto :end

:nodocker
echo [ERROR] Docker command not found. Install and start Docker Desktop first.
goto :end

:noengine
echo [ERROR] Docker engine is not responding. Start Docker Desktop first.
goto :end

:nops
echo [ERROR] PowerShell not found. Log in with Git Bash instead:
echo     ./wb2api.sh login
goto :end

:end
echo.
echo (Window kept open on purpose. Press any key to close.)
pause >nul
