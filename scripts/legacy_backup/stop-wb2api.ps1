# ==============================================================================
# WorkBuddy2API 停止容器脚本 (PowerShell 版)
# ==============================================================================

Write-Host "停止 workbuddy2api 容器..." -ForegroundColor Cyan
docker compose down
Write-Host "容器已停止。auths/ 与 data/ 本地持久化数据已安全保留。" -ForegroundColor Green
