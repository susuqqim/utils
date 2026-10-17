# build-msi.ps1 -- Build the per-user MSI installer for WorkBuddy2API (WiX v4+ CLI).
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/native/build-msi.ps1
#   optional: -Version 1.11.6   (default: parsed from cmd/server/main.go appVersion)
#   optional: -OutDir <dir>     (default: <repo>/release/native)
#   optional: -KeepStage        (keep the staging dir for inspection)
#
# Prerequisite: dotnet tool install --global wix     (provides the single `wix` CLI)
# Output: <OutDir>/workbuddy2api-<version>-x64.msi
#
# NOTE: this file is saved as UTF-8 **with BOM** on purpose -- Windows PowerShell 5.1
# needs the BOM to decode the non-ASCII (Chinese) file names referenced below.

param(
  [string]$Version = "",
  [string]$OutDir  = "",
  [string]$InstallDirName = "workbuddy2api",
  [string]$MsiSuffix = "",
  # Build a "test MSI": strips ALL side-effect custom actions (autostart on/off,
  # taskkill, data removal) and lets you pick a different install folder name.
  # WARNING: the stripped actions are GLOBAL in scope (HKCU Run value name
  # "WorkBuddy2API" and image name "wb2api-core.exe" are shared with the production
  # instance). Never install/uninstall a package that still has them on a machine
  # running production -- on 2026-10-02 the uninstall of such a package killed the
  # production process via `taskkill /IM wb2api-core.exe /F`.
  [switch]$NoSideEffectCA,
  [switch]$KeepStage
)

$ErrorActionPreference = 'Stop'

$here   = Split-Path -Parent $MyInvocation.MyCommand.Path     # ...\workbuddy2api\scripts\native
$repo   = (Resolve-Path (Join-Path $here '..\..\..')).Path    # workspace root
$src    = Join-Path $repo 'workbuddy2api'
$native = Join-Path $repo 'release\native'

if (-not $OutDir) { $OutDir = $native }
if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Force -Path $OutDir | Out-Null }

# 1) Resolve version: MSI requires a pure numeric x.y.z.
if (-not $Version) {
  $mainGo = Join-Path $src 'cmd\server\main.go'
  $m = Select-String -Path $mainGo -Pattern 'appVersion\s*=\s*"([^"]+)"' | Select-Object -First 1
  if (-not $m) { throw "cannot parse appVersion from $mainGo" }
  $raw = $m.Matches[0].Groups[1].Value            # e.g. 1.11.6-panel
  if ($raw -match '^(\d+)\.(\d+)\.(\d+)') {
    $Version = "$($Matches[1]).$($Matches[2]).$($Matches[3])"
  } else {
    throw "appVersion '$raw' is not x.y.z; MSI cannot use it"
  }
}
Write-Host "==> MSI version: $Version"

# 2) Locate the wix CLI (global dotnet tool).
$wixExe = $null
$cmd = Get-Command wix -ErrorAction SilentlyContinue
if ($cmd) { $wixExe = $cmd.Source }
if (-not $wixExe) {
  $tools = Join-Path $env:USERPROFILE '.dotnet\tools'
  $cand  = Join-Path $tools 'wix.exe'
  if (Test-Path $cand) {
    $env:PATH = "$env:PATH;$tools"
    $wixExe = $cand
  }
}
if (-not $wixExe) { throw "wix CLI not found. Run: dotnet tool install --global wix" }

# 3) Stage the payload (rename + normalize + generate the .url shortcut target).
$stage = Join-Path $native '.msi-stage'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force -Path $stage | Out-Null

$coreSrc = Join-Path $native 'wb2api-windows-amd64.exe'
if (-not (Test-Path $coreSrc)) {
  throw "missing $coreSrc -- run scripts/native/build-native.sh first"
}

# 3.1 core binary -> wb2api-core.exe
Copy-Item $coreSrc (Join-Path $stage 'wb2api-core.exe') -Force

# 3.2 launcher -> wb2api.cmd (must be CRLF + pure ASCII, same as install.ps1 does)
$launcherTxt = [IO.File]::ReadAllText((Join-Path $here 'wb2api-launcher.cmd'))
$launcherTxt = $launcherTxt.Replace("`r`n", "`n").Replace("`r", "`n").Replace("`n", "`r`n")
[IO.File]::WriteAllText((Join-Path $stage 'wb2api.cmd'), $launcherTxt, [Text.Encoding]::ASCII)

# 3.3 config template + docs
Copy-Item (Join-Path $src 'config.example.json')   (Join-Path $stage 'config.example.json')   -Force
Copy-Item (Join-Path $here '使用指南.html')        (Join-Path $stage '使用指南.html')        -Force
Copy-Item (Join-Path $here 'README-quickstart.md') (Join-Path $stage 'README-quickstart.md') -Force

# config 模板的 listen 改写为 127.0.0.1:17963（与 install.ps1 的安装期改写一致）。
# MSI 会把这份模板**播种**成数据目录里的初始 config.json；若保留 ":17963"（绑 0.0.0.0），
# 干净机器上装完就会把网关暴露到局域网。
$cfgPath = Join-Path $stage 'config.example.json'
$cfgTxt  = [IO.File]::ReadAllText($cfgPath, [Text.Encoding]::UTF8)
$cfgTxt  = [Text.RegularExpressions.Regex]::Replace($cfgTxt, '"listen"\s*:\s*"[^"]*"', '"listen": "127.0.0.1:17963"')
[IO.File]::WriteAllText($cfgPath, $cfgTxt, (New-Object Text.UTF8Encoding($false)))

# 3.4 icon (Add/Remove Programs + shortcuts)
Copy-Item (Join-Path $src 'internal\tray\icon.ico') (Join-Path $stage 'wb2api.ico') -Force

# 3.5 panel URL shortcut target (InternetShortcut .url, opens the browser without a console)
# IconFile 用**相对路径**：.url 与 wb2api.ico 都装在安装目录里，任何用户/安装位置都能取到。
# （写绝对路径会把构建机的 staging 路径烧进用户的 .url，图标必然失效且泄露构建机路径。）
$urlTxt = "[InternetShortcut]`r`nURL=http://127.0.0.1:17963/panel/`r`nIconFile=wb2api.ico`r`nIconIndex=0`r`n"
[IO.File]::WriteAllText((Join-Path $stage 'wb2api-panel.url'), $urlTxt, (New-Object Text.UTF8Encoding($false)))

# 4) Build.
$msi = Join-Path $OutDir ("workbuddy2api-$Version-x64$MsiSuffix.msi")
$noSideEffect = if ($NoSideEffectCA) { "1" } else { "0" }
if ($NoSideEffectCA) {
  Write-Host "==> TEST MSI: side-effect custom actions stripped (autostart / taskkill / rmdir)" -ForegroundColor Yellow
}
$buildArgs = @(
  'build', '-arch', 'x64',
  '-d', "Version=$Version",
  '-d', "StageDir=$stage",
  '-d', "InstallDirName=$InstallDirName",
  '-d', "NoSideEffectCA=$noSideEffect",
  '-o', $msi,
  # 把 WiX 调试符号(.wixpdb)重定向进 staging，随 staging 一起删除，保持发行目录干净
  '-pdb', (Join-Path $stage 'wb2api.wixpdb'),
  (Join-Path $here 'wb2api.wxs')
)
Write-Host "==> wix $($buildArgs -join ' ')"
& $wixExe @buildArgs
if ($LASTEXITCODE -ne 0) { throw "wix build failed (exit $LASTEXITCODE)" }

Write-Host "==> output: $msi"
Get-Item $msi | Format-List Name, Length, LastWriteTime

if (-not $KeepStage) { Remove-Item -Recurse -Force $stage }
Write-Host "==> done"
