# ==============================================================================
# WorkBuddy2API 实时日志追踪脚本 (PowerShell 版)
# ==============================================================================

Write-Host "正在追踪 workbuddy2api 容器日志（按 Ctrl+C 可退出）..." -ForegroundColor Cyan
docker compose logs -f --tail=100 wb2api
