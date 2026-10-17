@echo off
rem ===========================================================================
rem  WorkBuddy2API - Rebuild & Recreate Container  (double-click me)
rem
rem  This launcher is intentionally ASCII-ONLY. cmd.exe mis-parses .cmd files
rem  that mix UTF-8 Chinese text with "chcp 65001" (produces phantom errors
rem  like "'<char>' is not recognized"). All Chinese output is printed by
rem  wb2api.ps1 instead, which is UTF-8 BOM encoded and safe.
rem
rem  The window will NOT flash-close: it pauses at the end.
rem ===========================================================================
chcp 65001 >nul
cd /d "%~dp0"
title WorkBuddy2API - Rebuild

where docker >nul 2>&1
if errorlevel 1 goto :nodocker

docker info >nul 2>&1
if errorlevel 1 goto :noengine

where powershell >nul 2>&1
if errorlevel 1 goto :nops

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0wb2api.ps1" rebuild
goto :end

:nodocker
echo [ERROR] Docker command not found.
echo.
echo   Please install and start Docker Desktop first:
echo     https://www.docker.com/products/docker-desktop/
echo   Then close this window and double-click 6-rebuild.cmd again.
goto :end

:noengine
echo [ERROR] Docker engine is not responding.
echo.
echo   Start Docker Desktop (the whale icon in the taskbar must turn green),
echo   wait until it finishes starting, then run 6-rebuild.cmd again.
goto :end

:nops
echo [ERROR] PowerShell not found. Rebuild the service with Git Bash instead:
echo     ./wb2api.sh rebuild
goto :end

:end
echo.
echo (Window kept open on purpose. Press any key to close.)
pause >nul
