# wb2api.ps1 — WorkBuddy2API 离线部署 / 运维一体脚本（Windows PowerShell 版）
#
# 用法（在 PowerShell 中执行）：
#   .\wb2api.ps1 load            导入离线镜像（image\*.tar.gz）
#   .\wb2api.ps1 up              启动服务
#   .\wb2api.ps1 down            停止服务（保留 auths\ data\）
#   .\wb2api.ps1 restart         重启（改完 config.json 后用这个）
#   .\wb2api.ps1 status          账号池概览
#   .\wb2api.ps1 health          健康探针
#   .\wb2api.ps1 login           交互式加账号（浏览器授权）
#   .\wb2api.ps1 login-url       只生成授权 URL（自动化第一步）
#   .\wb2api.ps1 login-poll      取 token 并落盘（自动化第二步）
#   .\wb2api.ps1 logs            跟踪日志
#   .\wb2api.ps1 signin          手动签到
#   .\wb2api.ps1 credit          积分日报
#   .\wb2api.ps1 setkey [KEY]    写入 api_key 到 config.json 并重启
#   .\wb2api.ps1 doctor          环境自检
#
# 若提示「禁止运行脚本」，先执行：
#   Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
#
# 注意：本脚本用原生 docker CLI，路径以 Windows 形式传入，不经过 Git Bash，
#       因此不存在 MSYS 路径转换问题（这是 Windows 下的推荐入口）。

[CmdletBinding()]
param(
    [Parameter(Position = 0)][string]$Command = "help",
    [Parameter(Position = 1, ValueFromRemainingArguments = $true)][string[]]$Rest
)

$ErrorActionPreference = "Continue"
# 注意：这里必须是 Continue，不能是 Stop。
# Windows PowerShell 5.1 下，若 ErrorActionPreference='Stop'，原生命令（docker）
# 往 stderr 写一行（例如「镜像不存在」「引擎未启动」）就会被升级为**终止性错误**，
# 脚本直接在那一行中断 —— 表现为「双击后闪退」。
# 本脚本对所有原生命令一律用 $LASTEXITCODE 判定成败，因此 Continue 是正确语义。
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Set-Location -Path $PSScriptRoot

# 与 wb2api.sh 保持一致：支持环境变量覆盖（同机跑第二实例 / 换端口）
$script:Container = if ($env:WB2A_CONTAINER) { $env:WB2A_CONTAINER } else { "workbuddy2api" }
$script:Image     = if ($env:WB2A_IMAGE)     { $env:WB2A_IMAGE }     else { "workbuddy2api:offline" }
$script:Port      = if ($env:WB2A_PORT)      { [int]$env:WB2A_PORT } else { 17963 }
$script:Base      = "http://127.0.0.1:$($script:Port)"
$script:Compose   = $null   # 首次使用时探测 docker compose / docker-compose

function Die([string]$msg) { Write-Host "[错误] $msg" -ForegroundColor Red; exit 1 }
function Info([string]$msg) { Write-Host "[信息] $msg" -ForegroundColor Cyan }

# ─── docker compose 命令探测（新版子命令 vs 旧版独立二进制）─────────────
function Get-Compose {
    if ($script:Compose) { return $script:Compose }
    docker compose version *> $null
    if ($LASTEXITCODE -eq 0) { $script:Compose = @("docker", "compose"); return $script:Compose }
    if (Get-Command docker-compose -ErrorAction SilentlyContinue) {
        $script:Compose = @("docker-compose"); return $script:Compose
    }
    Die "找不到 docker compose（请升级 Docker Desktop，或安装 docker-compose）"
}

function Invoke-Compose {
    $c = Get-Compose
    $exe = $c[0]
    $pre = @()
    if ($c.Count -gt 1) { $pre = $c[1..($c.Count - 1)] }
    & $exe @pre @args
}

function Assert-Docker {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Die "找不到 docker。请先安装并启动 Docker Desktop。"
    }
    docker info *> $null
    if ($LASTEXITCODE -ne 0) { Die "Docker 引擎无响应。请先启动 Docker Desktop（托盘鲸鱼变绿）。" }
}

function Get-ApiKey {
    if (-not (Test-Path "config.json")) { return "" }
    try {
        return (Get-Content "config.json" -Raw | ConvertFrom-Json).api_key
    } catch { return "" }
}

function Test-Running {
    $names = docker ps --filter "name=^/$($script:Container)$" --format "{{.Names}}" 2>$null
    return [bool]$names
}

function Get-Probe([string]$path) {
    if (Get-Command curl.exe -ErrorAction SilentlyContinue) {
        return (curl.exe -s -m 3 --noproxy "*" "$($script:Base)$path" 2>$null)
    }
    try { return (Invoke-RestMethod -Uri "$($script:Base)$path" -TimeoutSec 3 | ConvertTo-Json -Compress) }
    catch { return "" }
}

# ─── 子命令 ────────────────────────────────────────────────────────────
function Cmd-Load {
    Assert-Docker
    $force = $Rest -contains "--force"
    docker image inspect $script:Image *> $null
    if ($LASTEXITCODE -eq 0 -and -not $force) {
        Info "镜像 $($script:Image) 已存在，跳过导入（强制重导： .\wb2api.ps1 load --force）"
        return
    }
    $tgz = Get-ChildItem "image\*.tar.gz" -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $tgz) { Die "找不到 image\*.tar.gz 离线镜像包" }
    Info "导入离线镜像 $($tgz.Name) ..."
    # Windows 无 gunzip：docker load 可直接读 gzip 压缩的 tar
    docker load -i $tgz.FullName
    if ($LASTEXITCODE -ne 0) { Die "镜像导入失败" }
    Info "镜像导入完成：$($script:Image)"
}

function Cmd-Up {
    Assert-Docker
    $key = Get-ApiKey
    if ([string]::IsNullOrEmpty($key) -or $key -eq "CHANGE_ME_BEFORE_USE") {
        Write-Host "[警告] config.json 的 api_key 仍是占位符，服务将对任何请求放行。" -ForegroundColor Yellow
        Write-Host "        建议： .\wb2api.ps1 setkey [你的密钥]" -ForegroundColor Yellow
    }
    # 镜像未导入则自动导入：否则 compose 会退回从源码 build（需联网），离线场景必然失败
    docker image inspect $script:Image *> $null
    if ($LASTEXITCODE -ne 0) {
        Info "本地没有镜像 $($script:Image)，先自动导入离线镜像..."
        Cmd-Load
    }
    Invoke-Compose up -d
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[错误] 容器启动失败。" -ForegroundColor Red
        Write-Host ('        若提示 container name "{0}" is already in use：' -f $script:Container)
        Write-Host "        · 同机已有同名容器（可能来自别的目录）"
        Write-Host '        · 换名字/端口： $env:WB2A_CONTAINER="wb2api-2"; $env:WB2A_PORT="27963"; .\wb2api.ps1 up'
        Write-Host "        · 或确认后清理旧容器： docker rm -f $($script:Container)"
        exit 1
    }
    Info "等待服务就绪..."
    foreach ($i in 1..15) {
        if ((Get-Probe "/healthz")) { break }
        Start-Sleep -Seconds 1
    }
    Invoke-Compose ps
    $keyDisplay = if ($key) { $key } else { "(空)" }
    Write-Host ""
    Write-Host "  Base URL : $($script:Base)/v1" -ForegroundColor Green
    Write-Host "  API Key  : $keyDisplay" -ForegroundColor Green
    Write-Host "  健康探针 : $(Get-Probe '/healthz')"
}

function Cmd-Down    { Assert-Docker; Invoke-Compose down; Info "已停止（auths\ 与 data\ 保留）" }
function Cmd-Restart { Assert-Docker; Invoke-Compose restart; Start-Sleep -Seconds 2; Invoke-Compose ps }

function Cmd-Rebuild {
    Assert-Docker
    $noCache = $Rest -contains "--no-cache"
    Info "正在重新构建 Docker 镜像 (含最新源码与透传优化)..."
    if ($noCache) {
        Invoke-Compose build --no-cache
    } else {
        Invoke-Compose build
    }
    if ($LASTEXITCODE -ne 0) { Die "镜像构建失败" }
    Info "正在重新创建并启动容器..."
    Invoke-Compose up -d --force-recreate
    if ($LASTEXITCODE -ne 0) { Die "容器启动失败" }
    Info "等待服务就绪..."
    foreach ($i in 1..15) {
        if ((Get-Probe "/healthz")) { break }
        Start-Sleep -Seconds 1
    }
    Invoke-Compose ps
    Info "容器重构并启动完成！"
    Cmd-Health
}

function Cmd-Status {
    Assert-Docker
    if (-not (Test-Running)) { Die "容器未运行，请先 .\wb2api.ps1 up" }
    docker exec $script:Container python3 /app/scripts/status.py
    if ($LASTEXITCODE -ne 0) { Get-Probe "/healthz" }
}

function Cmd-Health { Assert-Docker; Write-Host "GET $($script:Base)/healthz"; Get-Probe "/healthz" }

# 生成授权 URL：在「容器内」跑 login，跨平台一致，且不受宿主二进制差异影响
function Invoke-LoginTool([string]$mode, [string]$realm) {
    if (-not (Test-Path "auths")) { New-Item -ItemType Directory -Path "auths" -Force *> $null }
    $auths = (Resolve-Path "auths").Path.Replace('\', '/')
    docker run --rm -v "${auths}:/app/auths" $script:Image python3 /app/scripts/agent_login.py $mode "--realm=$realm"
}

function Cmd-LoginUrl([string]$targetRealm) {
    Assert-Docker
    $realm = if ($targetRealm) { $targetRealm } elseif ($Rest.Count -ge 1 -and $Rest[0]) { $Rest[0] } else { "cn" }
    Invoke-LoginTool "url" $realm
    if ($LASTEXITCODE -ne 0) { Die "生成授权 URL 失败" }
}

function Cmd-LoginPoll([string]$targetRealm) {
    Assert-Docker
    $realm = if ($targetRealm) { $targetRealm } elseif ($Rest.Count -ge 1 -and $Rest[0]) { $Rest[0] } else { "" }
    if (-not $realm -and (Test-Path "auths\.login-state.json")) {
        try {
            $stateJson = Get-Content "auths\.login-state.json" -Raw | ConvertFrom-Json
            if ($stateJson.realm) { $realm = $stateJson.realm }
        } catch {}
    }
    if (-not $realm) { $realm = "cn" }
    Invoke-LoginTool "poll" $realm
    if ($LASTEXITCODE -ne 0) { Die "获取 token 失败（浏览器登录可能尚未完成）" }
    Remove-Item "auths\.login-state.json" -ErrorAction SilentlyContinue
    Invoke-Compose restart *> $null
    Start-Sleep -Seconds 2
    Info "已落盘新账号并重启容器加载"
    Cmd-Health
}

function Cmd-Login {
    Assert-Docker
    $realm = "cn"
    if ($Rest.Count -ge 1 -and $Rest[0]) {
        $realm = $Rest[0]
    } else {
        Write-Host "选择登录版本: 1) 国内版 cn   2) 国际版 global"
        $ans = Read-Host "输入 1 或 2 [默认 1]"
        if ($ans -eq "2" -or $ans -match "global") { $realm = "global" }
    }
    Info "realm = $realm"
    $url = (Invoke-LoginTool "url" $realm | Select-Object -Last 1)
    if (-not $url -or $url -notmatch "^http") { Die "生成授权 URL 失败" }
    Write-Host ""
    Write-Host "============================================================" -ForegroundColor Cyan
    Write-Host "  请在浏览器打开下面的链接完成登录："
    Write-Host ""
    Write-Host "  $url" -ForegroundColor Green
    Write-Host ""
    if ($realm -eq "global") {
        Write-Host "  [国际版登录特别提示]" -ForegroundColor Yellow
        Write-Host "  1. 浏览器完成 Google / 邮箱登录后，页面会进入工作区选择界面；" -ForegroundColor Yellow
        Write-Host "  2. 务必在网页上点击选择个人版（Personal）或对应工作区进入；" -ForegroundColor Yellow
        Write-Host "  3. 若首次登录弹出完善国家/地区信息，请选择并提交；" -ForegroundColor Yellow
        Write-Host "  4. 待浏览器页面出现「登录成功 / Return to CLI」后，再回本窗口按 y 继续！" -ForegroundColor Yellow
        Write-Host ""
    }
    Write-Host "============================================================" -ForegroundColor Cyan
    $url | Set-Content "auths/.login-url.txt" -Encoding UTF8
    Write-Host "（链接已存到 auths/.login-url.txt）"
    Write-Host ""
    $ok = Read-Host "完成登录后按 y 继续"
    if ($ok -notmatch "^[yY]") { Die "已取消（重新运行 .\wb2api.ps1 login 再试）" }
    Cmd-LoginPoll $realm
}

function Cmd-Logs { Assert-Docker; Invoke-Compose logs -f --tail=100 wb2api }

function Invoke-InImage {
    Assert-Docker
    if (Test-Running) {
        docker exec -w /app $script:Container @args
    } else {
        if (-not (Test-Path "auths")) { New-Item -ItemType Directory -Path "auths" -Force *> $null }
        $auths = (Resolve-Path "auths").Path.Replace('\', '/')
        docker run --rm -v "${auths}:/app/auths" -w /app $script:Image @args
    }
}

function Cmd-Signin { Invoke-InImage /app/signin_bin /app/auths }
function Cmd-Credit { Invoke-InImage /app/credit -pretty }

function Cmd-SetKey {
    $key = if ($Rest.Count -ge 1) { $Rest[0] } else { "" }
    if ([string]::IsNullOrEmpty($key)) { Die "用法: .\wb2api.ps1 setkey [你的密钥]" }
    $cfg = Get-Content "config.json" -Raw | ConvertFrom-Json
    $cfg.api_key = $key
    $cfg | ConvertTo-Json -Depth 10 | Set-Content "config.json" -Encoding UTF8
    Info "api_key 已写入 config.json"
    if (Test-Running) { Invoke-Compose restart *> $null; Info "容器已重启" }
}

function Cmd-Doctor {
    Write-Host "── 环境自检 ──────────────────────────────"
    $dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
    Write-Host ("{0,-12} {1}" -f "docker", $(if ($dockerCmd) { $dockerCmd.Source } else { "缺失" }))
    if ($dockerCmd) {
        docker info *> $null
        $engine = if ($LASTEXITCODE -eq 0) { "正常" } else { "未响应（启动 Docker Desktop）" }
        Write-Host ("{0,-12} {1}" -f "引擎", $engine)
        docker image inspect $script:Image *> $null
        $img = if ($LASTEXITCODE -eq 0) { "$($script:Image) 已导入" } else { "未导入（.\wb2api.ps1 load）" }
        Write-Host ("{0,-12} {1}" -f "镜像", $img)
        Write-Host ("{0,-12} {1}" -f "容器", $(if (Test-Running) { "运行中" } else { "未运行" }))
    }
    Write-Host ("{0,-12} {1}" -f "config.json", $(if (Test-Path "config.json") { "存在" } else { "缺失" }))
    $key = Get-ApiKey
    $keyState = if ([string]::IsNullOrEmpty($key)) { "空（不鉴权）" }
                elseif ($key -eq "CHANGE_ME_BEFORE_USE") { "仍是占位符，请 setkey" }
                else { "已设置" }
    Write-Host ("{0,-12} {1}" -f "api_key", $keyState)
    $n = (Get-ChildItem "auths\workbuddy-*.json" -ErrorAction SilentlyContinue | Measure-Object).Count
    Write-Host ("{0,-12} {1} 个账号" -f "auths", $n)
}

function Show-Usage {
    Write-Host ""
    Write-Host "wb2api.ps1 — WorkBuddy2API 离线部署 / 运维" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "  load            导入离线镜像（image\*.tar.gz）"
    Write-Host "  up              启动服务"
    Write-Host "  down            停止服务（保留 auths\ data\）"
    Write-Host "  restart         重启（改完 config.json 后用）"
    Write-Host "  rebuild         重新构建镜像并重启容器（代码/透传优化更新后用）"
    Write-Host "  status          账号池概览"
    Write-Host "  health          健康探针"
    Write-Host "  login           交互式加账号（浏览器授权）"
    Write-Host "  login-url       只生成授权 URL（自动化第一步）"
    Write-Host "  login-poll      取 token 并落盘（自动化第二步）"
    Write-Host "  logs            跟踪日志"
    Write-Host "  signin          手动签到"
    Write-Host "  credit          积分日报"
    Write-Host "  setkey [KEY]    写入 api_key 并重启"
    Write-Host "  doctor          环境自检"
    Write-Host ""
}

switch ($Command.ToLower()) {
    "load"       { Cmd-Load }
    "up"         { Cmd-Up }
    "down"       { Cmd-Down }
    "restart"    { Cmd-Restart }
    "rebuild"    { Cmd-Rebuild }
    "build"      { Cmd-Rebuild }
    "status"     { Cmd-Status }
    "ps"         { Cmd-Status }
    "health"     { Cmd-Health }
    "login"      { Cmd-Login }
    "login-url"  { Cmd-LoginUrl }
    "login-poll" { Cmd-LoginPoll }
    "logs"       { Cmd-Logs }
    "signin"     { Cmd-Signin }
    "credit"     { Cmd-Credit }
    "setkey"     { Cmd-SetKey }
    "doctor"     { Cmd-Doctor }
    default      { Show-Usage }
}
