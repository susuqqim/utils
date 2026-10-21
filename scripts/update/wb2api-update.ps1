<#
wb2api-update.ps1 -- check for and apply a newer gateway build.

Manifest format (latest.json):
  { "version": "1.11.8",
    "urls":   ["https://github.com/u/r/releases/download/v1.11.8/wb2api-core.exe", "..."],
    "sha256": "<hex of the exe>",
    "size":   10012160 }

Tries, in order: GitHub raw -> jsDelivr -> GitHub release + proxy prefixes.

Apply = lock -> backup -> stop -> replace -> start -> health -> auto rollback.

2026-10-21 独立审查后集中修复（不依赖既有测试）：
  B1  StartCore 缺少 -WindowStyle Hidden ⇒ 每次更新都会弹出一个控制台窗口
      （用户明确要求"检查更新不许弹 cmd"，网关自身自启早已用 CREATE_NO_WINDOW）。
  B2  替换阶段无保护：Move-Item 失败会带着 $ErrorActionPreference='Stop' 抛异常，
      脚本终止在"已停服"状态且不触发回滚 ⇒ 服务无人拉起。
  B3  回滚前不看 StopCore 返回值：新进程没停干净就 Copy-Item ⇒ 文件占用 ⇒ 回滚失败。
  B4  无并发锁：手动 + 定时/面板同时跑会互抢 StopCore / Move-Item。
  B5  版本比较用 [version] 强转：'1.11.15-panel' 这类带后缀的版本会抛异常并落进
      catch ⇒ 判定"需要更新" ⇒ 每次运行都重装同版本（Go 侧 versionNewer 用
      leadingDigits 能正确处理，两边语义必须一致）。
  B12 健康检查端口硬编码 17963；config.json 的 listen 改过就会打错端口 ⇒ 误判失败回滚。
  B13 备份永不清理：实测安装目录已累积 10 份 .bak/.old、占 161MB。
#>
param(
  [switch]$CheckOnly,
  [switch]$Force,
  [switch]$Quiet,
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
$oldExe     = Join-Path $installDir 'wb2api-core.exe.old'
$lockFile   = Join-Path $installDir 'update.lock'

# --- update sources (order matters; first working wins) -----------------------
$ghOwner = 'susuqqim'; $ghRepo = 'utils'
$ghRaw   = "https://raw.githubusercontent.com/$ghOwner/$ghRepo/main/latest.json"
$ghRel   = "https://github.com/$ghOwner/$ghRepo/releases/latest/download/latest.json"
$proxies = @('https://ghproxy.net/', 'https://gh-proxy.com/', 'https://mirror.ghproxy.com/')
$jsd        = "https://cdn.jsdelivr.net/gh/$ghOwner/$ghRepo@main/latest.json"   # jsDelivr CDN（实测可用）
$manifestSources = @($ghRaw, $jsd, $ghRel)
foreach ($p in $proxies) { $manifestSources += ($p + $ghRel) }   # proxy prefix + full URL

function Log([string]$m) {
  $line = '[{0}] {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $m
  if (-not $Quiet) { Write-Host $line }
  try { $line | Out-File -FilePath $logFile -Append -Encoding ascii } catch { }
}
function Get-Sha([string]$p) { (Get-FileHash $p -Algorithm SHA256).Hash.ToLower() }
function Get-LocalVersion {
  if (Test-Path $versionFile) { return (Get-Content $versionFile -Raw).Trim() }
  return '0.0.0'
}

# --- B5: 与 Go 侧 panel.versionNewer 完全一致的版本比较 -----------------------
# 取每段开头的连续数字；'1.11.15-panel' 的第 3 段取到 15（后缀忽略）。
function Get-LeadingDigits([string]$s) {
  $m = [regex]::Match($s, '^\d+')
  if ($m.Success) {
    try { return [int]$m.Value } catch { return [int]::MaxValue }   # 超长数字不炸
  }
  return 0
}
# 返回 $true 表示 $a 比 $b 新（语义同 Go 的 versionNewer）
function Test-VersionNewer([string]$a, [string]$b) {
  if ([string]::IsNullOrWhiteSpace($a)) { return $false }
  if ([string]::IsNullOrWhiteSpace($b)) { return $true }
  if ($a.Trim() -eq $b.Trim()) { return $false }
  $pa = $a.Trim().Split('.'); $pb = $b.Trim().Split('.')
  $n = [Math]::Max($pa.Length, $pb.Length)
  for ($i = 0; $i -lt $n; $i++) {
    $na = 0; $nb = 0
    if ($i -lt $pa.Length) { $na = Get-LeadingDigits $pa[$i] }
    if ($i -lt $pb.Length) { $nb = Get-LeadingDigits $pb[$i] }
    if ($na -ne $nb) { return ($na -gt $nb) }
  }
  return $false
}

# --- B12: 健康检查地址跟随 config.json 的 listen -----------------------------
function Get-HealthURL {
  $listen = ''
  try {
    $path = $cfgFile
    if (-not [string]::IsNullOrWhiteSpace($ConfigPath) -and (Test-Path $ConfigPath)) { $path = $ConfigPath }
    if (Test-Path $path) {
      $cfg = Get-Content $path -Raw | ConvertFrom-Json
      if ($cfg.listen) { $listen = [string]$cfg.listen }
    }
  } catch { }
  if ([string]::IsNullOrWhiteSpace($listen)) { $listen = '127.0.0.1:17963' }
  # ':17963' / '0.0.0.0:17963' / '[::]:17963' → 统一成回环地址
  $listen = $listen -replace '^0\.0\.0\.0:', '127.0.0.1:'
  $listen = $listen -replace '^\[::\]:', '127.0.0.1:'
  if ($listen.StartsWith(':')) { $listen = '127.0.0.1' + $listen }
  return "http://$listen/healthz"
}

function Health {
  # 健康判据（2026-10-21 修）：
  #   ① 服务级：200 且确实是本服务在应答（防端口被别的程序占用时误判成功）；
  #   ② 账号级：池里有账号时**至少一个可用**。
  #
  # 旧实现写死 '"healthy":5' —— 只要池里有一个号在冷却/禁用/暂停，就永远不满足，
  # 结果是**每次更新都必然回滚**（生产实测 healthy=4/total=5 → AUTOMATIC ROLLBACK）。
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Uri (Get-HealthURL) -TimeoutSec 5
    if ($r.StatusCode -ne 200) { return $false }
    $c = [string]$r.Content
    if ($c -notmatch '"service"\s*:\s*"workbuddy2api"') { return $false }
    $total = 0
    $healthy = 0
    if ($c -match '"total"\s*:\s*(\d+)') { $total = [int]$Matches[1] }
    if ($c -match '"healthy"\s*:\s*(\d+)') { $healthy = [int]$Matches[1] }
    if ($total -gt 0 -and $healthy -lt 1) { return $false }
    return $true
  } catch { return $false }
}

function StartCore {
  # B1：更新是后台操作，绝不能弹控制台（用户明确要求）。
  # 网关自身的自重启走 CREATE_NO_WINDOW，这里用 -WindowStyle Hidden 达到同样效果。
  $argList = @('-config', $cfgFile)
  Start-Process -FilePath $exe -ArgumentList $argList -WorkingDirectory $cfgDir -WindowStyle Hidden | Out-Null
}

function StopCore {
  taskkill /IM wb2api-core.exe /F 2>$null | Out-Null
  for ($i = 0; $i -lt 25; $i++) {
    if (-not (Get-Process wb2api-core -ErrorAction SilentlyContinue)) { return $true }
    Start-Sleep -Seconds 1
  }
  return (-not (Get-Process wb2api-core -ErrorAction SilentlyContinue))
}

# --- B4: 单实例锁（陈旧锁自动接管，避免残留把更新永久卡死）------------------
$lockHandle = $null
function Acquire-Lock {
  param([int]$StaleMinutes = 30)
  if (Test-Path $lockFile) {
    try {
      $age = (Get-Date) - (Get-Item $lockFile).LastWriteTime
      if ($age.TotalMinutes -lt $StaleMinutes) {
        Log ("another update is running (lock age {0:N0}m); aborting" -f $age.TotalMinutes)
        return $false
      }
      Log ("stale lock ({0:N0}m old) -> taking over" -f $age.TotalMinutes)
      Remove-Item $lockFile -Force -ErrorAction SilentlyContinue
    } catch { }
  }
  try {
    $script:lockHandle = [IO.File]::Open($lockFile, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    $script:lockHandle.SetLength(0)
    $bytes = [Text.Encoding]::ASCII.GetBytes("pid=$PID started=$(Get-Date -Format s)")
    $script:lockHandle.Write($bytes, 0, $bytes.Length)
    $script:lockHandle.Flush()
    return $true
  } catch {
    Log ("cannot acquire update lock: {0}" -f $_.Exception.Message)
    return $false
  }
}
function Release-Lock {
  if ($script:lockHandle) {
    try { $script:lockHandle.Close() } catch { }
    $script:lockHandle = $null
  }
  Remove-Item $lockFile -Force -ErrorAction SilentlyContinue
}

# --- B13: 备份只保留最近 N 份（旧实现永不清理，实测累积 161MB）------------
function Prune-Backups {
  param([int]$Keep = 5)
  try {
    $baks = Get-ChildItem -Path $installDir -Filter 'wb2api-core.exe.bak-auto-*' -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending
    if ($baks -and $baks.Count -gt $Keep) {
      $baks | Select-Object -Skip $Keep | ForEach-Object {
        Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
        Log ("pruned old backup: {0}" -f $_.Name)
      }
    }
    $cfgs = Get-ChildItem -Path $cfgDir -Filter 'config.json.bak-auto-*' -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending
    if ($cfgs -and $cfgs.Count -gt $Keep) {
      $cfgs | Select-Object -Skip $Keep | ForEach-Object {
        Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
      }
    }
    # 历史遗留的 .bak / .bak-<时间戳>（非 auto 命名）也清掉老的那些，保留最近 2 份
    $legacy = Get-ChildItem -Path $installDir -Filter 'wb2api-core.exe.bak*' -ErrorAction SilentlyContinue |
              Where-Object { $_.Name -notlike '*.bak-auto-*' } | Sort-Object LastWriteTime -Descending
    if ($legacy -and $legacy.Count -gt 2) {
      $legacy | Select-Object -Skip 2 | ForEach-Object {
        Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
        Log ("pruned legacy backup: {0}" -f $_.Name)
      }
    }
  } catch { }
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

# --- 2) decide (B5: 语义比较，与 Go 侧一致) ----------------------------------
$need = $false
if ($Force) {
  $need = $true
} elseif (Test-VersionNewer $manifest.version $local) {
  $need = $true
} elseif ($manifest.version.Trim() -ne $local.Trim()) {
  # 清单版本更旧（或不可比）⇒ 明确不降级，只提示，避免被误发旧版反刷
  Log ("remote {0} is not newer than local {1}; refusing downgrade" -f $manifest.version, $local)
}
if (-not $need) { Log 'up to date; nothing to do'; exit 0 }
if ($CheckOnly) { Log ("update available: {0} -> {1}" -f $local, $manifest.version); exit 10 }

# --- 3) download with the same fallback chain -------------------------------
if (-not (Acquire-Lock)) { exit 8 }
try {
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
      if ($got -eq $manifest.sha256.ToLower()) {
        # 清单给了 size 就顺手校验（截断/半包的第二道闸）
        if ($manifest.size -and ((Get-Item $tmpExe).Length -ne [int64]$manifest.size)) {
          Log ("size mismatch: got {0} want {1}" -f (Get-Item $tmpExe).Length, $manifest.size)
          Remove-Item $tmpExe -Force -ErrorAction SilentlyContinue
          continue
        }
        $ok = $true; break
      }
      Log ("sha256 mismatch: got {0} want {1}" -f $got, $manifest.sha256)
      Remove-Item $tmpExe -Force -ErrorAction SilentlyContinue
    } catch { Log ("download failed: {0} ({1})" -f $u, $_.Exception.Message) }
  }
  if (-not $ok) {
    Remove-Item $tmpExe -Force -ErrorAction SilentlyContinue   # 下载失败不留残骸
    Log 'ERROR: no artifact passed sha256 verification'
    exit 4
  }

  # --- 4) apply (backup -> replace -> start -> health -> auto rollback) -----
  $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
  $bak = Join-Path $installDir ("wb2api-core.exe.bak-auto-" + $stamp)
  Copy-Item $exe $bak -Force
  if (Test-Path $cfgFile) {
    Copy-Item $cfgFile (Join-Path $cfgDir ("config.json.bak-auto-" + $stamp)) -Force
  }
  Log ("backup: {0}" -f $bak)

  # B2/B3：替换阶段任何异常都必须落到"把服务拉起来"，绝不能停在停服状态。
  $replaced = $false
  try {
    if (-not (StopCore)) { throw 'cannot stop old process' }
    Remove-Item $oldExe -Force -ErrorAction SilentlyContinue      # .old 可能被占用/残留
    Move-Item -Force $exe $oldExe -ErrorAction Stop
    Move-Item -Force $tmpExe $exe -ErrorAction Stop
    Set-Content -Path $versionFile -Value $manifest.version -NoNewline -Encoding ascii
    $replaced = $true
  } catch {
    Log ("ERROR: replace failed: {0}" -f $_.Exception.Message)
    if (-not (Test-Path $exe) -and (Test-Path $bak)) {
      Copy-Item $bak $exe -Force -ErrorAction SilentlyContinue
      Log 'restored executable from backup'
    }
    StartCore
    Release-Lock
    exit 7
  }

  StartCore
  $good = $false
  for ($i = 0; $i -lt 30; $i++) { Start-Sleep -Seconds 2; if (Health) { $good = $true; break } }
  if ($good) {
    Log ("UPDATE OK: {0} -> {1}" -f $local, $manifest.version)
    Prune-Backups
    Release-Lock
    exit 0
  }

  # --- rollback -------------------------------------------------------------
  Log 'health check failed -> AUTOMATIC ROLLBACK'
  if (-not (StopCore)) {
    Log 'WARN: new process did not stop; rollback may fail'
  }
  try {
    Copy-Item $bak $exe -Force -ErrorAction Stop
    Set-Content -Path $versionFile -Value $local -NoNewline -Encoding ascii
  } catch {
    Log ("ERROR: rollback copy failed: {0}" -f $_.Exception.Message)
  }
  StartCore
  $rb = $false
  for ($i = 0; $i -lt 30; $i++) { Start-Sleep -Seconds 2; if (Health) { $rb = $true; break } }
  if ($rb) { Log 'ROLLED BACK: previous version restored and healthy'; Release-Lock; exit 1 }
  Log 'ROLLBACK FAILED: manual intervention required'
  Release-Lock
  exit 6
}
finally {
  # 任何提前 exit 都走这里，确保锁不残留
  if ($script:lockHandle) { Release-Lock }
}
