@echo off
rem ===========================================================================
rem  WorkBuddy2API - Environment self-check  (double-click me)
rem  Run this FIRST if start-up fails, and send the output to support.
rem  ASCII-only on purpose - see 1-start.cmd. Will NOT flash-close.
rem ===========================================================================
chcp 65001 >nul
cd /d "%~dp0"
title WorkBuddy2API - Doctor

echo ============================================================
echo   Environment self-check
echo ============================================================
echo.

echo -- OS -------------------------------------------------------
ver
echo.

echo -- docker command -------------------------------------------
where docker
if errorlevel 1 (
  echo   [MISSING] Docker not installed, or not on PATH
) else (
  docker --version
)
echo.

echo -- docker engine --------------------------------------------
docker info --format "  version: {{.ServerVersion}}  storage: {{.Driver}}" 2>nul
if errorlevel 1 (
  echo   [FAIL] Engine not responding - start Docker Desktop
  echo          (taskbar whale icon must turn green)
) else (
  echo   OK
)
echo.

echo -- docker compose -------------------------------------------
docker compose version 2>nul
if errorlevel 1 (
  docker-compose --version 2>nul
  if errorlevel 1 echo   [MISSING] neither "docker compose" nor "docker-compose"
)
echo.

echo -- required files -------------------------------------------
if exist "wb2api.ps1" (echo   wb2api.ps1          OK) else (echo   wb2api.ps1          MISSING)
if exist "wb2api.sh"  (echo   wb2api.sh           OK) else (echo   wb2api.sh           MISSING)
if exist "config.json" (echo   config.json         OK) else (echo   config.json         MISSING)
if exist "docker-compose.yml" (echo   docker-compose.yml  OK) else (echo   docker-compose.yml  MISSING)
if exist "image\workbuddy2api-offline.tar.gz" (echo   offline image tar   OK) else (echo   offline image tar   MISSING)
echo.

echo -- image imported? ------------------------------------------
docker image inspect workbuddy2api:offline --format "  imported: {{.Id}}" 2>nul
if errorlevel 1 echo   NOT imported - 1-start.cmd imports it automatically
echo.

rem NOTE: netstat cannot see Docker Desktop port mappings (verified), so test
rem reachability instead - far more reliable than parsing netstat output.
echo -- port 17963 reachable? ------------------------------------
curl -s -m 4 --noproxy "*" http://127.0.0.1:17963/healthz >nul 2>&1
if errorlevel 1 (
  echo   Not reachable - service is down, or the port is taken by
  echo   something else. If start-up reports a bind failure, check
  echo   whether the port falls in a Windows reserved range:
  echo     netsh int ipv4 show excludedportrange protocol=tcp
) else (
  echo   Service is responding:
  curl -s -m 4 --noproxy "*" http://127.0.0.1:17963/healthz
)
echo.

echo -- Windows reserved port ranges -----------------------------
netsh int ipv4 show excludedportrange protocol=tcp 2>nul
echo.

echo ============================================================
echo   Self-check done. If start-up still fails, send ALL of the
echo   above text to whoever gave you this package.
echo ============================================================

echo.
echo (Window kept open on purpose. Press any key to close.)
pause >nul
