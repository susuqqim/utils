@echo off
chcp 65001 >nul
cd /d "%~dp0"

rem 找 Git Bash
set BASH=
if exist "%ProgramFiles%\Git\bin\bash.exe" set BASH=%ProgramFiles%\Git\bin\bash.exe
if exist "%ProgramFiles(x86)%\Git\bin\bash.exe" set BASH=%ProgramFiles(x86)%\Git\bin\bash.exe
if exist "%LOCALAPPDATA%\Programs\Git\bin\bash.exe" set BASH=%LOCALAPPDATA%\Programs\Git\bin\bash.exe

if "%BASH%"=="" (
  echo [错误] 没找到 Git Bash。请手动在 Git Bash 里执行：
  echo     cd /d/.../workbuddy2api ^&^& ./login.sh
  pause
  exit /b 1
)

echo 用 Git Bash 执行 login.sh（会打印授权 URL，请复制到浏览器完成登录）
echo ------------------------------------------------------------
"%BASH%" -c "cd \"$(cygpath -u '%CD%')\" && ./login.sh"
echo ------------------------------------------------------------
echo.
echo 注意：脚本末尾若提示「服务已重启，当前账号数: N」= 容器已在跑，账号已加载；
echo       若提示「容器 workbuddy2api 未运行」= 正常，之后用 start-wb2api.cmd 拉起即可。
echo       用 status-wb2api.cmd 可随时查看账号池。
pause
