<#
wb2api-update.ps1 -- check for and apply a newer gateway build.
ASCII ONLY on purpose: Windows PowerShell 5.1 mis-decodes UTF-8 without BOM.

Manifest format (latest.json):
  { "version": "1.11.8",
    "urls":   ["https://github.com/u/r/releases/download/v1.11.8/wb2api-core.exe", "..."],
    "sha256": "<hex of the exe>",
    "size":   10012160 }

Tries, in order: GitHub raw -> proxy prefixes + GitHub release URL -> Gitee raw.
Apply = stop -> backup -> replace -> start -> health check -> auto rollback on failure.
#>
param(
  [switch]$CheckOnly,
  [switch]$Force,
  [string]$ConfigPath = ""
)
$ErrorActionPreference = 'Stop'

$installDir = Join-Path $env:LOCALAPPDATA 'workbuddy2api'
$exe        = Join-Path $installDir 'wb2api-core.exe'
$cfgDir     = Join-Path $env:USERPROFILE '.workbuddy2api'
$cfgFile    = Join-Path $cfgDir 'config.json'
$logFile    = Join-Path $installDir 'update.log'
$versionFile= Join-Path $installDir 'version.txt'
$tmpExe     = Join-Path $installDir 'wb2api-core.exe.download'

# --- update sources (order matters; first working wins) -----------------------
$ghOwner = 'susuqqim'; $ghRepo = 'workbuddy2api'
$ghRaw   = "https://raw.githubusercontent.com/$ghOwner/$ghRepo/main/latest.json"
$ghRel   = "https://github.com/$ghOwner/$ghRepo/releases/latest/download/latest.json"
$giteeRaw= "https://gitee.com/$ghOwner/$ghRepo/raw/main/latest.json"
$proxies = @('https://ghproxy.net/', 'https://gh-proxy.com/', 'https://mirror.ghproxy.com/')
$manifestSources = @($ghRaw, $ghRel)
foreach ($p in $proxies) { $manifestSources += ($p + $ghRel) }   # proxy prefix + full URL
$manifestSources += $giteeRaw

function Log([string]$m) {
  $line = '[{0}] {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $m
  Write-Host $line
  try { $line | Out-File -FilePath $logFile -Append -Encoding ascii } catch { }
}
function Get-Sha([string]$p) { (Get-FileHash $p -Algorithm SHA256).Hash.ToLower() }
function Get-LocalVersion {
  if (Test-Path $versionFile) { return (Get-Content $versionFile -Raw).Trim() }
  return '0.0.0'
}
function Health {
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:17963/healthz' -TimeoutSec 5
    return ($r.StatusCode -eq 200 -and $r.Content -match '"healthy":5')
  } catch { return $false }
}
function StartCore {
  Start-Process -FilePath $exe -ArgumentList '-config', $cfgFile -WorkingDirectory $cfgDir | Out-Null
}
function StopCore {
  taskkill /IM wb2api-core.exe /F 2>$null | Out-Null
  for ($i = 0; $i -lt 25; $i++) {
    if (-not (Get-Process wb2api-core -ErrorAction SilentlyContinue)) { return $true }
    Start-Sleep -Seconds 1
  }
  return (-not (Get-Process wb2api-core -ErrorAction SilentlyContinue))
}

# --- 1) fetch manifest from the first working source -------------------------
$manifest = $null; $used = ''
foreach ($u in $manifestSources) {
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Uri $u -TimeoutSec 15
    $o = $r.Content | ConvertFrom-Json
    if ($o.version -and $o.sha256) { $manifest = $o; $used = $u; break }
  } catch { Log ("manifest source failed: {0} ({1})" -f $u, $_.Exception.Message) }
}
if (-not $manifest) { Log 'ERROR: no manifest reachable (github/proxies/gitee all failed)'; exit 3 }
$local = Get-LocalVersion
Log ("manifest ok via {0}: remote={1} local={2}" -f $used, $manifest.version, $local)

# --- 2) decide -----------------------------------------------------------------
$need = $Force
if (-not $need) {
  $need = ($manifest.version -ne $local)
  if ($need) { try { $need = ([version]$manifest.version -gt [version]$local) } catch { $need = $true } }
}
if (-not $need) { Log 'up to date; nothing to do'; exit 0 }
if ($CheckOnly) { Log ("update available: {0} -> {1}" -f $local, $manifest.version); exit 10 }

# --- 3) download with the same fallback chain (proxy prefix + github url) ----
$urls = @($manifest.urls)
foreach ($u in @($manifest.urls)) {
  foreach ($p in $proxies) { if ($u -match '^https://github\.com/') { $urls += ($p + $u) } }
}
$ok = $false
foreach ($u in $urls) {
  try {
    Log ("downloading: {0}" -f $u)
    Invoke-WebRequest -UseBasicParsing -Uri $u -OutFile $tmpExe -TimeoutSec 300
    $got = Get-Sha $tmpExe
    if ($got -eq $manifest.sha256.ToLower()) { $ok = $true; break }
    Log ("sha256 mismatch: got {0} want {1}" -f $got, $manifest.sha256); Remove-Item $tmpExe -Force
  } catch { Log ("download failed: {0} ({1})" -f $u, $_.Exception.Message) }
}
if (-not $ok) { Log 'ERROR: no artifact passed sha256 verification'; exit 4 }

# --- 4) apply (backup -> replace -> start -> health -> auto rollback) --------
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$bak = Join-Path $installDir ("wb2api-core.exe.bak-auto-" + $stamp)
Copy-Item $exe $bak -Force
Copy-Item $cfgFile (Join-Path $cfgDir ("config.json.bak-auto-" + $stamp)) -Force
Log ("backup: {0}" -f $bak)
if (-not (StopCore)) { Log 'ERROR: cannot stop old process; nothing replaced'; exit 5 }
Move-Item -Force $exe (Join-Path $installDir 'wb2api-core.exe.old') -ErrorAction SilentlyContinue
Move-Item -Force $tmpExe $exe
Set-Content -Path $versionFile -Value $manifest.version -NoNewline -Encoding ascii
StartCore
$good = $false
for ($i = 0; $i -lt 30; $i++) { Start-Sleep -Seconds 2; if (Health) { $good = $true; break } }
if ($good) { Log ("UPDATE OK: {0} -> {1}" -f $local, $manifest.version); exit 0 }

Log 'health check failed -> AUTOMATIC ROLLBACK'
StopCore | Out-Null
Copy-Item $bak $exe -Force
Set-Content -Path $versionFile -Value $local -NoNewline -Encoding ascii
StartCore
$rb = $false
for ($i = 0; $i -lt 30; $i++) { Start-Sleep -Seconds 2; if (Health) { $rb = $true; break } }
if ($rb) { Log 'ROLLED BACK: previous version restored and healthy'; exit 1 }
Log 'ROLLBACK FAILED: manual intervention required'; exit 6
