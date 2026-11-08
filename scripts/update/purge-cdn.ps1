<#
purge-cdn.ps1 -- after publishing a release, make the CDN copies of latest.json
catch up immediately.

Why: jsDelivr keeps its own cache for gh/<owner>/<repo>@<ref>/latest.json.
Right after a release the gateway update check can therefore still see the
previous version (2026-11-08, 1.11.33 release: check returned 1.11.31 even
though raw.githubusercontent was already fresh).

Usage:
  pwsh -File scripts/update/purge-cdn.ps1 -Manifest .\latest.json
  pwsh -File scripts/update/purge-cdn.ps1 -Manifest release/native/latest.json -Owner susuqqim -Repo utils
  pwsh -File scripts/update/purge-cdn.ps1 -Manifest .\latest.json -VerifyOnly

Exit codes:
  0 = CDN serves the expected version/sha
  1 = CDN reachable but still stale
  2 = CDN unreachable from this host (cannot verify; not a release failure)

ASCII only (PS 5.1 compatible; no BOM needed).
#>
param(
  [string]$Manifest = '.\latest.json',
  [string]$Owner = 'susuqqim',
  [string]$Repo  = 'utils',
  [string]$Ref   = 'main',
  [switch]$VerifyOnly,
  [int]$MaxAttempts = 8,
  [int]$DelaySeconds = 3,
  [switch]$Quiet,
  # Default: direct connection. Release hosts often carry broken proxy env vars,
  # which makes every freshness check fail with a TLS/connection error.
  [switch]$UseSystemProxy,
  # Overrides, for self-test only.
  [string]$CdnUrl = '',
  [string]$RawUrl = '',
  [string]$PurgeUrl = ''
)
$ErrorActionPreference = 'Stop'

# Save and clear proxy env vars for this run (default: direct).
$savedProxy = @{}
if (-not $UseSystemProxy) {
  foreach ($k in @('HTTP_PROXY','HTTPS_PROXY','http_proxy','https_proxy','ALL_PROXY','all_proxy')) {
    $savedProxy[$k] = [Environment]::GetEnvironmentVariable($k)
    [Environment]::SetEnvironmentVariable($k, $null)
  }
}

function Say($m) { if (-not $Quiet) { Write-Host $m } }
function Fail($m) { Write-Host ('ERROR: ' + $m) -ForegroundColor Red; exit 1 }

if (-not (Test-Path $Manifest)) { Fail ('manifest not found: ' + $Manifest) }
$m = Get-Content $Manifest -Raw | ConvertFrom-Json
$wantVersion = [string]$m.version
$wantSha = ([string]$m.sha256).ToLower()
if (-not $wantVersion -or -not $wantSha) { Fail 'manifest missing version/sha256' }
Say ('expect: version=' + $wantVersion + ' sha256=' + $wantSha.Substring(0, [Math]::Min(16, $wantSha.Length)) + '..')

if (-not $CdnUrl)   { $CdnUrl   = 'https://cdn.jsdelivr.net/gh/' + $Owner + '/' + $Repo + '@' + $Ref + '/latest.json' }
if (-not $RawUrl)   { $RawUrl   = 'https://raw.githubusercontent.com/' + $Owner + '/' + $Repo + '/' + $Ref + '/latest.json' }
if (-not $PurgeUrl) { $PurgeUrl = 'https://purge.jsdelivr.net/gh/' + $Owner + '/' + $Repo + '@' + $Ref + '/latest.json' }
$cdnUrl = $CdnUrl
$rawUrl = $RawUrl
$purgeUrl = $PurgeUrl

# Network layer prefers curl.exe: pwsh's SChannel occasionally fails TLS handshakes
# on Windows, and curl can force a direct connection with --noproxy.
function Http-Text([string]$url) {
  $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
  if ($curl) {
    # PS 5.1: native stderr becomes a terminating error under EAP=Stop, so relax it here.
    $eap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $out = & curl.exe -sSL --noproxy '*' --retry 1 -m 30 $url 2>$null
    $code = $LASTEXITCODE
    $ErrorActionPreference = $eap
    if ($code -eq 0 -and $out) { return ($out -join "`n") }
  }
  try {
    return (Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 30 -Headers @{ 'Cache-Control' = 'no-cache' }).Content
  } catch { return $null }
}

function Fetch($url) {
  $t = Http-Text $url
  if (-not $t) { return $null }
  try { return ($t | ConvertFrom-Json) } catch { return $null }
}

function Matches($doc) {
  if ($null -eq $doc) { return $false }
  $v = [string]$doc.version
  $s = ([string]$doc.sha256).ToLower()
  return ($v -eq $wantVersion) -and ($s -eq $wantSha)
}

if (-not $VerifyOnly) {
  Say ('purge: ' + $purgeUrl)
  $bodyText = Http-Text $purgeUrl
  if ($bodyText) {
    try {
      $body = $bodyText | ConvertFrom-Json
      Say ('  purge status=' + $body.status + ' id=' + $body.id)
    } catch {
      Say '  purge returned non-JSON (continuing to verify)'
    }
  } else {
    Say '  purge request failed (continuing to verify)'
  }
}

$ok = $false
$reached = $false
for ($i = 1; $i -le $MaxAttempts; $i++) {
  $cdn = Fetch $cdnUrl
  if ($null -ne $cdn) { $reached = $true }
  if (Matches $cdn) {
    Say ('  cdn OK after ' + $i + ' check(s): version=' + $cdn.version)
    $ok = $true
    break
  }
  $got = if ($null -eq $cdn) { '<fetch failed>' } else { ([string]$cdn.version + '/' + ([string]$cdn.sha256).Substring(0, [Math]::Min(12, ([string]$cdn.sha256).Length))) }
  Say ('  cdn still stale (' + $i + '/' + $MaxAttempts + '): ' + $got)
  if ($i -lt $MaxAttempts) { Start-Sleep -Seconds $DelaySeconds }
}

# raw.githubusercontent is served by GitHub itself (short TTL): report, never fail on it.
$raw = Fetch $rawUrl
if (Matches $raw) { Say ('  raw OK: version=' + $raw.version) }
elseif ($null -eq $raw) { Write-Host 'WARN: raw.githubusercontent not reachable (ignored)' -ForegroundColor Yellow }
else { Write-Host ('WARN: raw still ' + $raw.version + ' (GitHub cache, settles on its own)') -ForegroundColor Yellow }

if (-not $ok) {
  if ($reached) {
    Fail ('CDN serves a stale manifest after ' + $MaxAttempts + ' attempts; users keep seeing the old version')
  }
  Write-Host ('WARN: CDN unreachable from this host (no successful fetch of ' + $cdnUrl + '); cannot verify freshness.') -ForegroundColor Yellow
  Write-Host '      If the CDN is blocked on this machine, users are usually still fine; verify from another network.' -ForegroundColor Yellow
  exit 2
}
Say 'CDN manifest is up to date.'
exit 0