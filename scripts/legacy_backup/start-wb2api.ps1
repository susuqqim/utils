# ==============================================================================
# WorkBuddy2API Docker 启动脚本 (PowerShell 版)
# ==============================================================================

[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Write-Host "[1/3] 启动 workbuddy2api 容器..." -ForegroundColor Cyan
docker compose up -d
if ($LASTEXITCODE -ne 0) {
    Write-Host "[错误] 容器启动失败。" -ForegroundColor Red
    Exit 1
}

Write-Host "`n[2/3] 等待服务初始化探活 (http://127.0.0.1:17963/healthz)..." -ForegroundColor Cyan
for ($i = 1; $i -le 10; $i++) {
    try {
        $resp = Invoke-RestMethod -Uri "http://127.0.0.1:17963/healthz" -TimeoutSec 2 -ErrorAction SilentlyContinue
        if ($resp) { break }
    } catch {}
    Start-Sleep -Seconds 1
}

Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "[3/3] 容器与服务状态" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
docker compose ps
Write-Host ""
Write-Host "本地接入地址:  http://127.0.0.1:17963/v1" -ForegroundColor Green
Write-Host "模型列表查询:  http://127.0.0.1:17963/v1/models" -ForegroundColor Green
Write-Host ""
