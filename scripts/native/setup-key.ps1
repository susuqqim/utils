# setup-key.ps1 -- 首次使用/安装后引导设置 api_key（ASCII, PS5.1）
# 若 api_key 已是有效值则静默退出 0；否则交互引导（可自动生成或手输），原子写回，密钥只显示一次。
param(
  [string]$Config = (Join-Path $env:USERPROFILE '.workbuddy2api\config.json'),
  [switch]$NonInteractive
)
$ErrorActionPreference = 'Stop'
$PH = 'CHANGE_ME_BEFORE_USE'
if (-not (Test-Path $Config)) { Write-Host ('  FIND-CONFIG-FAILED: ' + $Config); exit 2 }
$raw = Get-Content $Config -Raw -Encoding UTF8
try { $cfg = $raw | ConvertFrom-Json } catch { Write-Host ('  BAD-JSON: ' + $_.Exception.Message); exit 2 }
$cur = [string]$cfg.api_key
if ($cur.Trim() -ne '' -and $cur.Trim() -ne $PH) { exit 0 }

Write-Host ''
Write-Host '=== WorkBuddy2API 首次设置 ==='
Write-Host '  当前 api_key 尚未设置（仍是占位符）。'
Write-Host ('  配置文件: ' + $Config)
Write-Host ''
$key = ''
if ($NonInteractive) {
  $alpha = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
  $buf = New-Object byte[] 43
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
  $s = ''
  foreach ($b in $buf) { $s = $s + $alpha[$b % $alpha.Length] }
  $key = 'sk-wb2a-' + $s
  Write-Host '  （非交互）已自动生成随机密钥'
} else {
  Write-Host '  [1] 自动生成强随机密钥（推荐）'
  Write-Host '  [2] 自己输入'
  Write-Host '  [Q] 取消'
  $c = (Read-Host '  选择').Trim()
  if ($c -eq '1') {
    $alpha = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
    $buf = New-Object byte[] 43
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($buf)
    $s = ''
    foreach ($b in $buf) { $s = $s + $alpha[$b % $alpha.Length] }
    $key = 'sk-wb2a-' + $s
  } elseif ($c -eq '2') {
    $key = (Read-Host '  请输入密钥（建议 >=32 位）').Trim()
    if ($key.Length -lt 16) { Write-Host '  密钥太短（至少 16 位），已取消'; exit 1 }
  } else { Write-Host '  已取消（未设置密钥，网关无法启动）'; exit 1 }
}
$cfg.api_key = $key
$tmp = $Config + '.new'
# 关键：必须写【无 BOM】的 UTF-8 —— Go 的 encoding/json 不接受 BOM，否则网关读不出配置
$json = ($cfg | ConvertTo-Json -Depth 20)
[IO.File]::WriteAllText($tmp, $json, (New-Object System.Text.UTF8Encoding($false)))
Move-Item -Force $tmp $Config
Write-Host ''
Write-Host '=== 你的 API 密钥（只显示这一次）==='
Write-Host ('  ' + $key)
Write-Host ''
Write-Host '  请立即复制保存（密码管理器）。面板登录与全部 /v1 请求都用它。'
Write-Host '  提示：config.json 为明文；若泄露请改 api_key 后重启网关。'

# [install-ux] 只在首次询问"是否开机自启"（用标记文件保证只问一次）
$__marker = Join-Path (Split-Path $Config) '.firstrun-done'
if (-not (Test-Path $__marker)) {
  $__exe = Join-Path $PSScriptRoot 'wb2api-core.exe'
  if (Test-Path $__exe) {
    Write-Host ''
    if ($NonInteractive) {
      & $__exe autostart on | Out-Null
      Write-Host '  （非交互）已开启开机自启'
    } else {
      $__a = (Read-Host '  是否开机自启？（Y/n）').Trim()
      if ($__a -eq '' -or $__a -match '^[Yy]') {
        & $__exe autostart on | Out-Null
        Write-Host '  ✓ 已开启开机自启（以后可: wb2api-launcher.cmd autostart off 关闭）'
      } else {
        & $__exe autostart off | Out-Null
        Write-Host '  ✓ 已关闭开机自启（以后可: wb2api-launcher.cmd autostart on 开启）'
      }
    }
  }
  New-Item -ItemType File -Path $__marker -Force | Out-Null
}
Write-Host ''
if (-not $NonInteractive) { Read-Host '  按回车继续' | Out-Null }
exit 0
