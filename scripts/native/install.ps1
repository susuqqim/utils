# WorkBuddy2API native installer (Windows) -- no Docker, no npm.
# Online:  $env:WB2API_BASE_URL='https://host/native'; irm "$env:WB2API_BASE_URL/install.ps1" | iex
# Offline: run  powershell -ExecutionPolicy Bypass -File install.ps1  in the extracted folder.
$ErrorActionPreference = 'Stop'
$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)

$BaseUrl   = $env:WB2API_BASE_URL
$Arch      = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$BinName   = "wb2api-windows-$Arch.exe"
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
$AppDir    = Join-Path $env:LOCALAPPDATA 'workbuddy2api'
$CorePath  = Join-Path $AppDir 'wb2api-core.exe'
$CmdPath   = Join-Path $AppDir 'wb2api.cmd'
$DataDir   = if ($env:WB2API_HOME) { $env:WB2API_HOME } else { Join-Path $HOME '.workbuddy2api' }

Write-Host "==> WorkBuddy2API native install (windows/$Arch)"
New-Item -ItemType Directory -Force -Path $AppDir, $DataDir, (Join-Path $DataDir 'auths'), (Join-Path $DataDir 'data'), (Join-Path $DataDir 'logs') | Out-Null

# --- binary ---
$localBin = Join-Path $ScriptDir $BinName
$tmpBin = "$CorePath.new"
$sumFile = $null
if (Test-Path $localBin) {
  Write-Host "==> using local binary: $localBin"
  Copy-Item $localBin $tmpBin -Force
  $sumFile = Join-Path $ScriptDir 'SHA256SUMS'
} elseif ($BaseUrl) {
  Write-Host "==> downloading $BaseUrl/$BinName"
  Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$BinName" -OutFile $tmpBin
  $sumFile = Join-Path $AppDir 'SHA256SUMS'
  try { Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/SHA256SUMS" -OutFile $sumFile } catch { $sumFile = $null }
} else {
  throw "local $BinName not found and WB2API_BASE_URL is not set"
}

# 校验清单缺失/不含本文件 → **必须显式失败**（除非用户主动设 WB2A_SKIP_VERIFY=1）。
# 旧实现是"有就校验、没有就静默跳过"——下载源被替换时等于无保护地安装任意二进制，
# 而网关持有全部账号凭证（第九轮审计 C2）。
$verified = $false
if ($sumFile -and (Test-Path $sumFile)) {
  $line = Select-String -Path $sumFile -Pattern ([regex]::Escape(" $BinName")) | Select-Object -First 1
  if ($line) {
    $want = ($line.Line -split '\s+')[0].ToLower()
    $got = (Get-FileHash $tmpBin -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) { Remove-Item $tmpBin -Force; throw "SHA256 mismatch: want $want got $got" }
    Write-Host "==> SHA256 verified"
    $verified = $true
  }
}
if (-not $verified -and $env:WB2A_SKIP_VERIFY -ne '1') {
  Remove-Item $tmpBin -Force -ErrorAction SilentlyContinue
  throw "找不到 $BinName 的 SHA256 校验值（清单缺失或不含该文件）。确认来源可信后可用 WB2A_SKIP_VERIFY=1 跳过校验。"
}

# 覆盖前先停掉正在运行的本用户网关实例（否则 Windows 上覆盖运行中的 exe 会失败并中途抛错；
# MSI 安装包由 CaStopAppBeforeInstall 做同样的事）。best-effort：停不掉就让后面的 Move 报错。
$running = Get-Process -Name 'wb2api-core' -ErrorAction SilentlyContinue
if ($running) {
  Write-Host "==> 检测到正在运行的网关，先停止（升级需要替换 exe）"
  $stopCmd = Join-Path $AppDir 'wb2api.cmd'
  if (Test-Path $stopCmd) { & $stopCmd stop | Out-Null } 
  Start-Sleep -Milliseconds 800
  Get-Process -Name 'wb2api-core' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 500
}
Move-Item $tmpBin $CorePath -Force

# --- launcher (.cmd must be CRLF) ---
$localCmd = Join-Path $ScriptDir 'wb2api-launcher.cmd'
if (Test-Path $localCmd) { Copy-Item $localCmd $CmdPath -Force }
elseif ($BaseUrl) { Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/wb2api-launcher.cmd" -OutFile $CmdPath }
else { throw "wb2api-launcher.cmd missing" }
$txt = [IO.File]::ReadAllText($CmdPath)
$txt = $txt.Replace([string][char]13, '').Replace([string][char]10, [string][char]13 + [string][char]10)
[IO.File]::WriteAllText($CmdPath, $txt, [Text.Encoding]::ASCII)

# --- autostart ---
# 自启已内置到二进制：wb2api autostart on|off|status（直接写 HKCU\...\Run），
# 不再安装 autostart.ps1，也不再有 cmd -> powershell -ExecutionPolicy Bypass 链路
# （该链路会触发杀软"启动项防护"误报，见 README FAQ Q5）。顺手清理历史残留。
Remove-Item (Join-Path $AppDir 'autostart.ps1') -Force -ErrorAction SilentlyContinue

# --- config ---
$cfg = Join-Path $DataDir 'config.json'
if (-not (Test-Path $cfg)) {
  $localCfg = Join-Path $ScriptDir 'config.example.json'
  if (Test-Path $localCfg) { Copy-Item $localCfg $cfg -Force }
  elseif ($BaseUrl) { try { Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/config.example.json" -OutFile $cfg } catch { } }
  if (-not (Test-Path $cfg)) {
    [IO.File]::WriteAllText($cfg, '{"listen":"127.0.0.1:17963","api_key":"CHANGE_ME_BEFORE_USE","auth_dir":"./auths","state_file":"./data/state.json"}', $Utf8NoBom)
  }
  $t = Get-Content -Raw -LiteralPath $cfg
  $t = $t -replace '"listen"\s*:\s*"[^"]*"', '"listen": "127.0.0.1:17963"'
  [IO.File]::WriteAllText($cfg, $t, $Utf8NoBom)
  Write-Host "==> wrote config: $cfg (set your api_key)"
}

# --- user PATH ---
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
if (($userPath -split ';') -notcontains $AppDir) {
  $newPath = if ($userPath) { "$userPath;$AppDir" } else { $AppDir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  Write-Host "==> added to user PATH: $AppDir (reopen terminal to take effect)"
}

# --- icon & desktop shortcut ---
$localIco = Join-Path $ScriptDir 'wb2api.ico'
$targetIco = Join-Path $AppDir 'wb2api.ico'
if (Test-Path $localIco) {
  Copy-Item $localIco $targetIco -Force
} elseif (Test-Path (Join-Path $ScriptDir '..\..\internal\tray\icon.ico')) {
  Copy-Item (Join-Path $ScriptDir '..\..\internal\tray\icon.ico') $targetIco -Force
}
$desktopPath = [Environment]::GetFolderPath('Desktop')
if ($desktopPath -and (Test-Path $desktopPath)) {
  try {
    $shortcutPath = Join-Path $desktopPath 'WorkBuddy2API.lnk'
    $wsh = New-Object -ComObject WScript.Shell
    $shortcut = $wsh.CreateShortcut($shortcutPath)
    $shortcut.TargetPath = $CmdPath
    $shortcut.Arguments = "start"
    $shortcut.WorkingDirectory = $DataDir
    $shortcut.Description = "WorkBuddy2API Gateway - Control Panel & Service"
    if (Test-Path $targetIco) {
      $shortcut.IconLocation = "$targetIco,0"
    } elseif (Test-Path $CorePath) {
      $shortcut.IconLocation = "$CorePath,0"
    }
    $shortcut.Save()
    Write-Host "==> desktop shortcut created: $shortcutPath"
  } catch {
    Write-Host "WARN: desktop shortcut creation skipped: $_"
  }
}

Write-Host ""
Write-Host "==> done"
Write-Host "    command: $CmdPath"
Write-Host "    data:    $DataDir"
Write-Host "next:"
Write-Host "    wb2api setkey <YOUR_KEY>"
Write-Host "    wb2api start"
Write-Host "    open http://127.0.0.1:17963/panel/"
