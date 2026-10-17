# ==============================================================================
# WorkBuddy2API Docker 一键重构镜像与启动脚本 (PowerShell 版)
# ==============================================================================

[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "  WorkBuddy2API 容器镜像重构向导 (含 Phase 1/2/3 智能加权调度)" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host ""

# 检查 Docker
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Write-Host "[错误] 找不到 docker 命令，请确认已安装 Docker Desktop 并加入 PATH。" -ForegroundColor Red
    Pause
    Exit 1
}

$dockerInfo = docker info 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Host "[错误] Docker 引擎未响应，请先启动 Docker Desktop。" -ForegroundColor Red
    Pause
    Exit 1
}

Write-Host "[1] 增量快速重构 (推荐，使用 Docker 编译缓存，约 10~20 秒)" -ForegroundColor Green
Write-Host "[2] 完全全新重构 (--no-cache，从头拉取并构建，约 2~3 分钟)" -ForegroundColor Yellow
$choice = Read-Host "请输入选项 [1 或 2，默认 1]"
if ($choice -eq "2") {
    $buildCmd = "docker compose build --no-cache"
    Write-Host "`n[模式] 执行完全全新重构 (--no-cache)..." -ForegroundColor Yellow
} else {
    $buildCmd = "docker compose build"
    Write-Host "`n[模式] 执行增量快速重构..." -ForegroundColor Green
}

Write-Host "`n[1/3] 编译并构建 Docker 镜像..." -ForegroundColor Cyan
Invoke-Expression $buildCmd
if ($LASTEXITCODE -ne 0) {
    Write-Host "`n[错误] 镜像构建失败，请检查编译输出与网络状态。" -ForegroundColor Red
    Pause
    Exit 1
}

Write-Host "`n[2/3] 强制重建并后台启动容器..." -ForegroundColor Cyan
docker compose up -d --force-recreate
if ($LASTEXITCODE -ne 0) {
    Write-Host "`n[错误] 容器启动失败。" -ForegroundColor Red
    Pause
    Exit 1
}

Write-Host "`n[3/3] 等待服务初始化自愈权限与探活..." -ForegroundColor Cyan
$healthy = $false
for ($i = 1; $i -le 10; $i++) {
    try {
        $resp = Invoke-RestMethod -Uri "http://127.0.0.1:17963/healthz" -TimeoutSec 2 -ErrorAction SilentlyContinue
        if ($resp) {
            $healthy = $true
            break
        }
    } catch {}
    Start-Sleep -Seconds 1
}

Write-Host ""
docker compose ps
Write-Host ""
Write-Host "------------------------------------------------------------" -ForegroundColor DarkGray
Write-Host "/healthz 响应：" -ForegroundColor DarkGray
try {
    $hz = Invoke-RestMethod -Uri "http://127.0.0.1:17963/healthz" -TimeoutSec 3 -ErrorAction SilentlyContinue
    $hz | ConvertTo-Json -Compress | Write-Host -ForegroundColor Green
} catch {
    Write-Host "服务初始化中或暂无可用账号" -ForegroundColor Yellow
}
Write-Host "------------------------------------------------------------" -ForegroundColor DarkGray
Write-Host ""
Write-Host "[成功] workbuddy2api 容器重构并启动完成！" -ForegroundColor Green
Write-Host "  - 查看账号池状态: .\status-wb2api.ps1 或 status-wb2api.cmd" -ForegroundColor Gray
Write-Host "  - 查看实时日志:   .\logs-wb2api.ps1 或 logs-wb2api.cmd" -ForegroundColor Gray
Write-Host "  - 停止容器:       .\stop-wb2api.ps1 或 stop-wb2api.cmd" -ForegroundColor Gray
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host ""
