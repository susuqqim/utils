<#
make-manifest.ps1 -- build latest.json + SHA256SUMS for a release directory.
ASCII only (PS 5.1).
Usage: powershell -NoProfile -File make-manifest.ps1 -Version 1.11.8 -Binary .\wb2api-core.exe -RepoOwner susuqqim -RepoName workbuddy2api
Writes: latest.json (next to the binary) + SHA256SUMS
#>
param(
  [Parameter(Mandatory=$true)][string]$Version,
  [Parameter(Mandatory=$true)][string]$Binary,
  [string]$RepoOwner = 'susuqqim',
  [string]$RepoName  = 'workbuddy2api',
  [string]$OutFile   = 'latest.json',
  # Optional: MSI package for in-app self-update (plan C: msiexec installs, MSI rolls back).
  [string]$Msi       = ''
)
$ErrorActionPreference = 'Stop'
$b = (Resolve-Path $Binary).Path
$h = (Get-FileHash $b -Algorithm SHA256).Hash.ToLower()
$s = (Get-Item $b).Length
$tag = 'v' + $Version
$rel = "https://github.com/$RepoOwner/$RepoName/releases/download/$tag/wb2api-core.exe"
$gitee = "https://gitee.com/$RepoOwner/$RepoName/releases/download/$tag/wb2api-core.exe"
$o = [ordered]@{ version = $Version; sha256 = $h; size = $s; urls = @($rel, $gitee); generated = (Get-Date).ToString('s') }
# Self-update block: the app only downloads+verifies the MSI, then spawns msiexec;
# Self-update block: the app downloads+verifies the MSI, then spawns msiexec;
if ($Msi) {
  $mp = (Resolve-Path $Msi).Path
  $mh = (Get-FileHash $mp -Algorithm SHA256).Hash.ToLower()
  $ms = (Get-Item $mp).Length
  $mn = Split-Path $mp -Leaf
  $o.msi = [ordered]@{
    name   = $mn
    url    = "https://github.com/$RepoOwner/$RepoName/releases/download/$tag/$mn"
    sha256 = $mh
    size   = $ms
  }
}
$o | ConvertTo-Json -Depth 4 | Out-File -FilePath $OutFile -Encoding ascii
"{0}  {1}" -f $h, (Split-Path $b -Leaf) | Out-File -FilePath (Join-Path (Split-Path $b) 'SHA256SUMS') -Encoding ascii
Write-Host ("wrote {0}: version={1} sha256={2} size={3} msi={4}" -f $OutFile, $Version, $h, $s, [bool]$Msi)
