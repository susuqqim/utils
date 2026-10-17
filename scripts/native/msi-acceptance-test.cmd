@echo off
rem MSI acceptance test for WorkBuddy2API -- run INSIDE a Windows guest (throwaway VM).
rem Runs in the user's interactive session, which is the real-world install context.
rem Writes a full report to C:\Temp\wb2api-msi-report.txt
setlocal EnableExtensions
set "RPT=C:\Temp\wb2api-msi-report.txt"
rem MSI path: pass it as argument 1, otherwise auto-pick the first *.msi next to this script.
rem (build-msi.ps1 emits workbuddy2api-<ver>-x64.msi, so a hardcoded name would never match.)
set "MSI=%~1"
if not "%MSI%"=="" goto :have_msi
for %%F in ("%~dp0*.msi") do set "MSI=%%~fF"
:have_msi
set "APPDIR=%LOCALAPPDATA%\workbuddy2api"
set "DATADIR=%USERPROFILE%\.workbuddy2api"

if not exist C:\Temp mkdir C:\Temp
echo ==== WorkBuddy2API MSI acceptance test ==== > "%RPT%"
echo [env] user=%USERNAME%  computer=%COMPUTERNAME%  date=%DATE% %TIME% >> "%RPT%"
if not exist "%MSI%" ( echo [FATAL] MSI not found next to script: %MSI% >> "%RPT%" & goto :end )
for %%A in ("%MSI%") do echo [msi] %%A  size=%%~zA bytes >> "%RPT%"

echo. >> "%RPT%"
echo ---- 1) INSTALL (msiexec /i /qn) ---- >> "%RPT%"
msiexec /i "%MSI%" /qn /norestart /l*v C:\Temp\wb2api-install.log
echo msiexec_exit=%ERRORLEVEL% >> "%RPT%"

echo. >> "%RPT%"
echo ---- 2) INSTALLED FILES ---- >> "%RPT%"
dir /b "%APPDIR%" >> "%RPT%" 2>&1
echo ---- DESKTOP SHORTCUTS ---- >> "%RPT%"
dir /b "%USERPROFILE%\Desktop" >> "%RPT%" 2>&1
echo ---- START MENU ---- >> "%RPT%"
dir /b "%APPDATA%\Microsoft\Windows\Start Menu\Programs\WorkBuddy2API" >> "%RPT%" 2>&1
echo ---- ARP: HKCU ---- >> "%RPT%"
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall" /s /f "WorkBuddy2API" 2>&1 | findstr /i "WorkBuddy2API DisplayVersion InstallLocation HKEY" >> "%RPT%"
echo ---- ARP: HKLM ---- >> "%RPT%"
reg query "HKLM\Software\Microsoft\Windows\CurrentVersion\Uninstall" /s /f "WorkBuddy2API" 2>&1 | findstr /i "WorkBuddy2API DisplayVersion InstallLocation HKEY" >> "%RPT%"
echo ---- HKCU RUN (autostart) ---- >> "%RPT%"
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v WorkBuddy2API >> "%RPT%" 2>&1
echo ---- USER PATH ---- >> "%RPT%"
powershell -NoProfile -Command "[Environment]::GetEnvironmentVariable('Path','User')" >> "%RPT%" 2>&1
echo ---- DATA DIR (seeded by MSI install) ---- >> "%RPT%"
if exist "%DATADIR%\config.json" (echo config.json seeded OK >> "%RPT%" & findstr /i "listen" "%DATADIR%\config.json" >> "%RPT%") else (echo config.json MISSING >> "%RPT%")
if exist "%DATADIR%\auths" (echo auths dir OK >> "%RPT%") else (echo auths dir MISSING >> "%RPT%")

echo. >> "%RPT%"
echo ---- 3) START VIA LAUNCHER (this is what the desktop shortcut runs) ---- >> "%RPT%"
call "%APPDIR%\wb2api.cmd" start >> "%RPT%" 2>&1
echo launcher_exit=%ERRORLEVEL% >> "%RPT%"
timeout /t 8 /nobreak >nul
tasklist /FI "IMAGENAME eq wb2api-core.exe" 2>&1 | findstr /i "wb2api-core" >> "%RPT%"
for %%A in ("%APPDIR%\wb2api-core.exe") do echo exe_before_reinstall size=%%~zA mtime=%%~tA >> "%RPT%"

echo. >> "%RPT%"
echo ---- 4) INSTALL AGAIN WHILE RUNNING (tests stop-before-InstallFiles CA) ---- >> "%RPT%"
start "" /min msiexec /i "%MSI%" /qn /norestart /l*v C:\Temp\wb2api-reinstall.log
timeout /t 45 /nobreak >nul
echo msiexec_still_running (expect empty): >> "%RPT%"
tasklist /FI "IMAGENAME eq msiexec.exe" 2>&1 | findstr /i "msiexec.exe" >> "%RPT%"
echo process_after_reinstall (expect empty, killed by CA): >> "%RPT%"
tasklist /FI "IMAGENAME eq wb2api-core.exe" 2>&1 | findstr /i "wb2api-core" >> "%RPT%"
for %%A in ("%APPDIR%\wb2api-core.exe") do echo exe_after_reinstall size=%%~zA mtime=%%~tA >> "%RPT%"
rem fallback: never leave a stuck msiexec that would hang the remaining steps
taskkill /IM msiexec.exe /F >nul 2>&1
timeout /t 5 /nobreak >nul

echo. >> "%RPT%"
echo ---- 5) UNINSTALL ---- >> "%RPT%"
msiexec /x "%MSI%" /qn /norestart /l*v C:\Temp\wb2api-uninstall.log
echo msiexec_exit=%ERRORLEVEL% >> "%RPT%"
timeout /t 3 /nobreak >nul

echo. >> "%RPT%"
echo ---- 6) AFTER UNINSTALL ---- >> "%RPT%"
if exist "%APPDIR%" (echo install dir STILL EXISTS >> "%RPT%" & dir /b "%APPDIR%" >> "%RPT%" 2>&1) else (echo install dir removed OK >> "%RPT%")
echo desktop: >> "%RPT%"
dir /b "%USERPROFILE%\Desktop" >> "%RPT%" 2>&1
if exist "%APPDATA%\Microsoft\Windows\Start Menu\Programs\WorkBuddy2API" (echo start menu STILL EXISTS >> "%RPT%") else (echo start menu removed OK >> "%RPT%")
echo ARP after uninstall: >> "%RPT%"
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall" /s /f "WorkBuddy2API" 2>&1 | findstr /i "WorkBuddy2API" >> "%RPT%"
echo run key after uninstall: >> "%RPT%"
reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v WorkBuddy2API >> "%RPT%" 2>&1
echo process after uninstall: >> "%RPT%"
tasklist /FI "IMAGENAME eq wb2api-core.exe" 2>&1 | findstr /i "wb2api-core" >> "%RPT%"
echo marker key: >> "%RPT%"
reg query "HKCU\Software\WorkBuddy2API" >> "%RPT%" 2>&1

:end
echo. >> "%RPT%"
echo ==== DONE ==== >> "%RPT%"
echo.
echo Report: %RPT%
echo.
pause
