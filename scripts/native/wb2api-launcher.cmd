@echo off
rem WorkBuddy2API native launcher (installed as <AppDir>\wb2api.cmd).
rem ASCII-only on purpose: cmd.exe mis-parses .cmd that mix non-ASCII with code-page changes.
setlocal EnableExtensions
set "PORT=%WB2API_PORT%"
if "%PORT%"=="" set "PORT=17963"
set "DIR=%WB2API_HOME%"
if "%DIR%"=="" set "DIR=%USERPROFILE%\.workbuddy2api"
set "LIB=%~dp0wb2api-core.exe"
if not exist "%LIB%" set "LIB=%~dp0..\lib\workbuddy2api\wb2api.exe"
if not exist "%LIB%" ( echo [ERROR] wb2api binary not found & exit /b 1 )
if not exist "%DIR%" mkdir "%DIR%" >nul 2>&1
if not exist "%DIR%" ( echo [ERROR] cannot create data dir: %DIR% & exit /b 1 )
cd /d "%DIR%"

if "%~1"=="" goto :do_start
if /I "%~1"=="start"  goto :do_start
if /I "%~1"=="stop"   goto :do_stop
if /I "%~1"=="status" goto :do_status
if /I "%~1"=="panel"  goto :do_panel
if /I "%~1"=="setkey" goto :do_setkey
if /I "%~1"=="autostart" goto :do_autostart
if /I "%~1"=="run"    goto :do_run
"%LIB%" -config config.json %*
goto :eof

rem Already-running fast path (2026-10-05): double-clicking the desktop shortcut usually
rem happens while the gateway is ALREADY up. The old behavior silently launched a second
rem instance which lost the port race and exited -> the user saw "nothing happens".
rem Health probe via curl (bundled with Windows 10+); non-zero = not running -> normal start.
:do_start
rem [install-ux] 首次使用：若 api_key 仍是占位符，引导设置（已设置则静默返回）
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0setup-key.ps1" -Config "%USERPROFILE%\.workbuddy2api\config.json"

curl.exe -s -m 2 --noproxy "*" -o nul http://127.0.0.1:%PORT%/healthz >nul 2>&1
if not errorlevel 1 (
  echo Already running. Opening panel: http://127.0.0.1:%PORT%/panel/
  start "" "http://127.0.0.1:%PORT%/panel/"
  goto :eof
)
start "WorkBuddy2API" /min "%LIB%" -config config.json
echo Started. Panel: http://127.0.0.1:%PORT%/panel/
goto :eof

:do_stop
taskkill /IM wb2api-core.exe /F >nul 2>&1
echo Stopped.
goto :eof

:do_status
curl.exe -s -m 3 --noproxy "*" http://127.0.0.1:%PORT%/healthz
echo.
goto :eof

:do_panel
echo http://127.0.0.1:%PORT%/panel/
goto :eof

:do_setkey
rem 密钥经**环境变量**传给 PowerShell，不再插值进 -Command 字符串（第九轮审计 A2：
rem 旧写法把 %~2 直接拼进 PowerShell 源码 —— 含 ' 或 $() 的密钥会破坏引号甚至被当代码执行）。
rem 未提供参数时提示输入（避免密钥进入 cmd 历史/进程列表；有参数时仍兼容旧用法）。
set "WB2A_NEWKEY=%~2"
if not defined WB2A_NEWKEY set /p "WB2A_NEWKEY=请输入新的 api_key: "
if not defined WB2A_NEWKEY echo [ERROR] api_key is empty & goto :eof
powershell -NoProfile -Command "$k=$env:WB2A_NEWKEY; if ($k.Contains([char]34) -or $k.Contains([char]92)) { Write-Error 'api_key 不能包含双引号或反斜杠'; exit 1 }; $q=[char]34;$p='config.json';$t=[IO.File]::ReadAllText($p);$rep=($q+'api_key'+$q+': '+$q)+$k.Replace('$','$$')+$q;$t=$t -replace ($q+'api_key'+$q+'\s*:\s*'+$q+'[^'+$q+']*'+$q),$rep;[IO.File]::WriteAllText($p,$t,(New-Object Text.UTF8Encoding($false)))"
set "WB2A_NEWKEY="
echo api_key updated. Restart: wb2api stop ^&^& wb2api start
goto :eof

rem autostart is implemented inside the binary (writes HKCU\...\Run directly);
rem it does NOT go through a cmd -> powershell chain.
:do_autostart
"%LIB%" autostart %2 %3 %4 %5 %6 %7 %8 %9
goto :eof

rem run: foreground mode. Keep the console visible for logs, so explicitly disable
rem the tray icon (-no-tray). `start` keeps the tray enabled (ui.tray defaults to true).
:do_run
"%LIB%" -config config.json -no-tray
goto :eof
