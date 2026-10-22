# ==============================================================================
# WorkBuddy2API 账号池状态查看脚本 (PowerShell 版)
# ==============================================================================

[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$running = docker ps --format "{{.Names}}" | Select-String -Pattern "^workbuddy2api$"
if (-not $running) {
    Write-Host "[提示] 容器 workbuddy2api 暂未运行，请先执行 .\start-wb2api.ps1" -ForegroundColor Yellow
    Exit 1
}

docker exec workbuddy2api python3 /app/scripts/status.py
if ($LASTEXITCODE -ne 0) {
    Write-Host "`n[提示] 尝试直接查询 /healthz 基础存活状态：" -ForegroundColor Yellow
    docker exec workbuddy2api wget -qO- http://127.0.0.1:17963/healthz
}
