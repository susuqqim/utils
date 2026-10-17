<#
release.ps1 -- one-command release for the workbuddy2api public snapshot.
ASCII ONLY (PS 5.1 compatible; also runs on PS7).

Steps: build exe -> manifest -> commit+push snapshot -> create GitHub release -> upload assets.

Usage (from the snapshot root or anywhere):
  pwsh -File scripts\update\release.ps1 -Version 1.11.8
  pwsh -File scripts\update\release.ps1 -Version 1.11.8 -SkipPush      # build+manifest only
Requires: Windows Go on PATH, git, and a GitHub token in Git Credential Manager
(the script reads it via: git credential fill; it never prints it).
#>
param(
  [Parameter(Mandatory=$true)][string]$Version,
  [switch]$SkipPush,
  [string]$Owner = 'susuqqim',
  [string]$Repo  = 'workbuddy2api'
)
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$snap = (Resolve-Path (Join-Path $here '..\..')).Path
Set-Location $snap
function Step($m) { Write-Host ("==> " + $m) -ForegroundColor Cyan }
function Fail($m) { Write-Host ("ERROR: " + $m) -ForegroundColor Red; exit 1 }

Step "0/5 sanity: version + worktree"
if ($Version -notmatch '^\d+\.\d+\.\d+$') { Fail "Version must be x.y.z" }
$tag = 'v' + $Version
& git rev-parse --git-dir *> $null; if ($LASTEXITCODE -ne 0) { Fail "not a git repo: $snap" }

Step "1/5 build wb2api-core.exe"
$env:CGO_ENABLED = '0'
& go build -trimpath -ldflags="-w" -o ".\wb2api-core.exe" .\cmd\server
if ($LASTEXITCODE -ne 0) { Fail "go build failed" }
$sha = (Get-FileHash .\wb2api-core.exe -Algorithm SHA256).Hash.ToLower()
$size = (Get-Item .\wb2api-core.exe).Length
Write-Host ("  built: sha256=" + $sha.Substring(0,16) + ".. size=" + $size)

Step "2/5 manifest (latest.json + SHA256SUMS)"
& (Join-Path $here 'make-manifest.ps1') -Version $Version -Binary .\wb2api-core.exe -RepoOwner $Owner -RepoName $Repo
if (-not (Test-Path .\latest.json)) { Fail "latest.json not produced" }
$m = Get-Content .\latest.json -Raw | ConvertFrom-Json
if ($m.version -ne $Version -or $m.sha256 -ne $sha) { Fail "manifest mismatch (version/sha)" }

if ($SkipPush) { Write-Host "SkipPush: stopping after manifest"; exit 0 }

Step "3/5 commit + push snapshot"
& git add -A
& git -c user.name="workbuddy2api" -c user.email="noreply@local" commit -m ("release: " + $tag) 2>$null
& git push origin main
if ($LASTEXITCODE -ne 0) { Fail "git push failed" }

Step "4/5 create GitHub release $tag"
$cred = "protocol=https`nhost=github.com`n`n" | & git credential fill 2>$null
$tok = ($cred | Where-Object { $_ -like 'password=*' } | Select-Object -First 1) -replace '^password=',''
if (-not $tok) { Fail "no GitHub credential in Git Credential Manager" }
$hdr = @{ Authorization = "Bearer $tok"; Accept = "application/vnd.github+json" }
$body = @{ tag_name = $tag; name = $tag; body = ("workbuddy2api " + $Version); draft = $false; prerelease = $false } | ConvertTo-Json
try {
  $rel = Invoke-RestMethod -Method Post -Uri ("https://api.github.com/repos/$Owner/$Repo/releases") -Headers $hdr -Body $body -ContentType "application/json" -TimeoutSec 90
} catch {
  Fail ("create release failed: " + $_.Exception.Message)
}
Write-Host ("  release id=" + $rel.id + " url=" + $rel.html_url)

Step "5/5 upload assets"
$assets = @(
  @{ p = '.\wb2api-core.exe'; n = 'wb2api-core.exe' },
  @{ p = '.\latest.json';     n = 'latest.json' },
  @{ p = '.\SHA256SUMS';      n = 'SHA256SUMS' }
)
foreach ($a in $assets) {
  if (-not (Test-Path $a.p)) { Write-Host ("  skip missing " + $a.n); continue }
  $url = "https://uploads.github.com/repos/$Owner/$Repo/releases/$($rel.id)/assets?name=$($a.n)"
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Method Post -Uri $url -Headers $hdr -InFile $a.p -ContentType 'application/octet-stream' -TimeoutSec 600
    Write-Host ("  uploaded " + $a.n + " HTTP " + $r.StatusCode)
  } catch { Fail ("upload failed: " + $a.n + " " + $_.Exception.Message) }
}
Write-Host ("DONE: " + $rel.html_url + "  (users get the update via wb2api-update.ps1 once latest.json is on main)") -ForegroundColor Green
