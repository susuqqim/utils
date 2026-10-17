@echo off
chcp 65001 >nul
cd /d "%~dp0"

python credit.py
if errorlevel 1 (
    if exist ".\credit.exe" (
        .\credit.exe
    )
)

echo.
pause