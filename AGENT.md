# AGENT.md — WorkBuddy2API 交付包执行说明（写给 agent）

> ⚠️ **现状指引（2026-10-02）**：本文是 **Docker 交付包**的旧版执行手册（2026-09-17），
> **已被 `AGENTS.md` 取代**。生产环境自 2026-09-30 起为**无 Docker 原生模式**（路线图 §I / §Q）。
> Agent 请优先读 `AGENTS.md`；本文仅作为 Docker 交付路线的历史参考。

> 本文件是给 **执行 agent** 的操作手册。请按顺序执行，不要跳过验证步骤。
> 人类用户无需阅读本文件。

## 0. 这是什么

一个把 **WorkBuddy / CodeBuddy 账号** 转成 **OpenAI 兼容 API** 的自托管网关。
上游项目：`Sliverkiss/workbuddy2api`（MIT）。本包已包含**构建好的镜像**，无需联网构建。

| 项 | 值 |
|---|---|
| 接入地址 | `http://127.0.0.1:17963/v1` |
| 鉴权 | `Authorization: Bearer <config.json 里的 api_key>` |
| 容器名 | `workbuddy2api` |
| 镜像 | `workbuddy2api:offline`（在 `image/workbuddy2api-offline.tar.gz`） |
| 凭证目录 | `./auths/workbuddy-<uid>.json`（**明文 token**） |
| 端口 | 17963（仅绑 `127.0.0.1`，不暴露局域网） |

---

## 0.5 Windows 用户：双击即用（推荐路径）

包内已提供 5 个 `.cmd` 启动器，**双击即可**，窗口不会闪退（结尾都会 pause）：

| 文件 | 作用 |
|---|---|
| `1-start.cmd` | 一键部署：自动导入镜像 → 启动服务 → 打印接入信息 |
| `2-login.cmd` | 加账号（打印授权 URL → 浏览器登录 → 回窗口按 y） |
| `3-status.cmd` | 查看容器状态 / 健康探针 / 账号池 |
| `4-stop.cmd` | 停止服务（保留账号与状态） |
| `5-doctor.cmd` | 环境自检（**启动失败先跑这个**，把输出发给对方） |

> ⚠️ **这些 .cmd 必须保持「纯 ASCII + CRLF」**。
> cmd.exe 在 `chcp 65001` 下解析含 UTF-8 中文的批处理会出错（报
> `'<某字>' is not recognized` 之类的幽灵错误）。因此所有中文输出都交给
> `wb2api.ps1`（UTF-8 **带 BOM**）打印，`.cmd` 只做 ASCII 转发。
> 维护时若新增中文提示，请加在 `.ps1` 里，不要写进 `.cmd`。

命令行用户（agent / 进阶）继续用 `wb2api.sh` 或 `wb2api.ps1`，见下节。

---

## 1. 任务：部署并交付一个可用端点

### 1.1 前置检查

```bash
docker version          # 需要 Docker 可用（Docker Desktop 已启动）
docker compose version  # 需要 compose（v2 子命令或独立 docker-compose 均可）
```

**Windows 用户**：用 **PowerShell** 跑 `wb2api.ps1`，或 **Git Bash** 跑 `wb2api.sh`。
不要用 WSL 的 `docker` shim（可能未启用集成而报 "could not be found in this WSL 2 distro"）。

### 1.2 部署（三步）

```bash
# 1) 导入离线镜像（约 36MB，几秒）
./wb2api.sh load          # PowerShell: .\wb2api.ps1 load

# 2) 设置一个 api_key（务必执行，否则服务对任何请求放行）
./wb2api.sh setkey "$(openssl rand -hex 24)"   # PowerShell: .\wb2api.ps1 setkey <你的密钥>

# 3) 启动
./wb2api.sh up            # PowerShell: .\wb2api.ps1 up
```

> **`up` 会自动补做 `load`**：若检测到镜像未导入，会先导入再启动，
> 所以新机器上直接跑 `up` 也不会失败。`load` 单独存在只是为了显式控制时机。

#### 镜像加载方式（三选一，效果相同）

镜像包是 `image/workbuddy2api-offline.tar.gz`，加载后得到 `workbuddy2api:offline`。

| 方式 | 命令 |
|---|---|
| 脚本（推荐） | `./wb2api.sh load` / `.\wb2api.ps1 load` |
| docker 直接读 gzip | `docker load -i image/workbuddy2api-offline.tar.gz` |
| 管道（Linux/macOS） | `gunzip -c image/workbuddy2api-offline.tar.gz \| docker load` |

要点：
- **`docker load` 能直接读 `.tar.gz`**，Windows 上不需要 gunzip，也**不要**先手动解压成 `.tar`。
- 加载后镜像名固定为 `workbuddy2api:offline`，与 `docker-compose.yml` 里的 `image:` 对应；
  改名会导致 compose 找不到镜像而退回从源码 build（需联网）。
- 镜像约 133MB（含全部二进制与脚本），校验值见 `MANIFEST.sha256` 末行。
- 重复执行 `load` 是安全的；要强制覆盖用 `./wb2api.sh load --force`。

### 1.3 加账号（必需，否则 /healthz 返回 503）

账号必须由**人类**在浏览器完成授权，agent 只能生成链接并轮询。

**方式 A：一条命令（人在旁边时）**

```bash
./wb2api.sh login          # 交互式：选 cn/global → 打印 URL → 人登录后按 y
```

**方式 B：两阶段（agent 自动化，推荐）**

```bash
# 第一步：生成授权 URL，把它交给人类，等其在浏览器完成登录
./wb2api.sh login-url cn        # 输出形如 https://copilot.tencent.com/login?...&state=xxx

# 第二步：人类确认已登录后，取 token 并落盘（自动重启容器加载）
./wb2api.sh login-poll cn
```

- 国内版用 `cn`，国际版用 `global`。**url 与 poll 的 realm 必须一致**，否则状态文件校验失败。
- 重复执行即可加多个账号（每账号一个文件，池子自动轮转）。
- 成功标志：`login-poll` 输出 JSON 含 `"ok": true`，且随后 `/healthz` 的 `healthy` ≥ 1。

### 1.4 验收（必须全部通过）

```bash
./wb2api.sh doctor     # 环境、镜像、容器、api_key、账号数
./wb2api.sh health     # 期望 healthy>=1
```

然后做一次真实调用（把 `<KEY>` 换成实际 api_key）：

```bash
curl -s http://127.0.0.1:17963/v1/chat/completions \
  -H "Authorization: Bearer <KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"cn:glm-5.2","messages":[{"role":"user","content":"回复：OK"}],"max_tokens":16}'
```

**通过标准**：返回 JSON 含 `choices[0].message.content`。

鉴权自检：

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:17963/v1/models          # 期望 401
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer <KEY>" \
     http://127.0.0.1:17963/v1/models                                              # 期望 200
```

---

## 2. 常用命令

| 目的 | 命令 |
|---|---|
| 启动 / 停止 | `./wb2api.sh up` / `./wb2api.sh down` |
| 改完 config.json 生效 | `./wb2api.sh restart` |
| 账号池概览 | `./wb2api.sh status` |
| 看日志（排障首选） | `./wb2api.sh logs` |
| 手动签到 | `./wb2api.sh signin` |
| 积分日报 | `./wb2api.sh credit` |
| 换 api_key | `./wb2api.sh setkey <新密钥>`（自动重启） |
| 换端口 | `WB2A_PORT=27963 ./wb2api.sh up` |

PowerShell 用户把 `./wb2api.sh X` 换成 `.\wb2api.ps1 X`。

---

## 3. ⚠️ 必须知道的坑（踩过，已验证）

### 3.1 端口 7863 不能用，默认已改 17963

Windows 的 WSL/Hyper-V 把 `7798-7897` 整段保留，上游默认端口 7863 落在其中，绑定会失败：

```
bind: An attempt was made to access a socket in a way forbidden by its access permissions
```

**这不是端口占用**。排查同类问题先跑：

```cmd
netsh int ipv4 show excludedportrange protocol=tcp
```

被占用时换端口：`WB2A_PORT=27963 ./wb2api.sh up`。

### 3.2 容器内 `listen` 必须是 `:17963`，不能写 `127.0.0.1:17963`

容器内要绑 `0.0.0.0`，否则宿主机端口映射进不去。宿主侧由 compose 收窄成 `127.0.0.1`。

### 3.3 Git Bash 路径转换（脚本已处理，手敲 docker 命令时必踩）

Git Bash 会把 `/app/xxx` 这类参数改写成 `C:/Program Files/Git/app/xxx`，而且**静默成功**：

- `-v "$(pwd)/auths:/app/auths"` → 挂载到错误目录，容器里是空目录
- `docker ps --filter "name=^/wb2api-test$"` → 过滤串被改写，误判容器未运行

`wb2api.sh` 已在开头全局导出 `MSYS_NO_PATHCONV=1`，宿主机路径用 `cygpath -w` 转换。
**如果你要手敲 docker 命令**，自己加 `MSYS_NO_PATHCONV=1`，宿主机路径用 `cygpath -w`。
PowerShell 不受影响。

### 3.4 `auths/*.json` 是明文凭证

含完整 access/refresh token，等于账号钥匙：

- 不要放网盘同步盘、不要进 git、不要公开分享
- 分享本包给他人前：**先清空 `auths/`**（本包交付时已清空）
- 服务只绑 `127.0.0.1`，端口不要暴露到公网

### 3.5 风控红线（看日志时）

| 日志 | 含义 | 处理 |
|---|---|---|
| `403 (code 11140)` | 账号/设备级风控已标记 | **不要反复重试**（越试越紧），换号或等待 |
| `429 (code 6004)` | 频率限制 | 网关自动软冷却，到期自恢复 |

`pool.max_in_flight` 默认已降为 **2** 以降低并发风控。**建议常备 2 个以上账号**（单号是单点）。

### 3.6 Windows 双击会「闪退」的两个原因（已修，改脚本时别改回去）

**原因 A：`.cmd` 里混了中文。**
cmd.exe 在 `chcp 65001` 下解析含 UTF-8 中文的批处理会错位，抛出
`'<单字>' is not recognized as an internal or external command`，看起来像随机报错。
→ 所有 `.cmd` 必须是**纯 ASCII + CRLF**，中文放在 `.ps1`（UTF-8 带 BOM）里打印。

**原因 B：`$ErrorActionPreference = 'Stop'`（PowerShell 5.1）。**
该设置下，原生命令（docker）只要往 stderr 写一行——例如「镜像不存在」「引擎未启动」——
就会被升级为**终止性错误**，脚本当场中断退出，窗口一闪而过。
→ 本包已改为 `'Continue'`，统一用 `$LASTEXITCODE` 判成败。**不要改回 `Stop`。**

两个 `.cmd` 都自带 `pause`，任何失败都会停在窗口里等你看完。

### 3.7 改源码后

`config.json` 是只读挂载，改完必须 `restart`。改 **Go 源码**后要重建镜像：

```bash
docker compose build && docker compose up -d --force-recreate
```

`auths/`、`data/` 是 bind mount，重建不丢账号与状态。

---

## 4. 配置要点（`config.json`）

| 字段 | 默认 | 说明 |
|---|---|---|
| `api_key` | `CHANGE_ME_BEFORE_USE` | **必须改**。留空 = 完全不鉴权 |
| `listen` | `:17963` | 容器内保持 `0.0.0.0` 形式 |
| `pool.max_in_flight` | `2` | 单账号最大并发；调高会增加风控风险 |
| `session_sticky.enabled` | `true` | 多轮对话粘同一账号 |
| `global.enabled` | `true` | 支持国际版账号；`false` = 锁死纯 CN |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站指纹清洗，建议保持 |
| `upstream.user_agent` | 空 | 空 = 内置对齐官方桌面端的 UA。**乱改会触发 403** |

定时任务开关（`schedule.*_enabled`）：本包默认开启 `checkin` / `travel` / `keepalive`，
关闭 `activity` / `school` / `cat`（后三者是刷数据类）。

全部字段支持 `WB2A_*` 环境变量覆盖。改完执行 `./wb2api.sh restart`。

---

## 5. Agent 速查：最短可用路径

```bash
./wb2api.sh load
./wb2api.sh setkey "$(openssl rand -hex 24)"
./wb2api.sh up
./wb2api.sh login-url cn      # ← 把 URL 给人类，等待其完成授权
./wb2api.sh login-poll cn     # ← 人类确认后执行
./wb2api.sh health            # 期望 healthy>=1
```

完成后向使用者报告：**Base URL = `http://127.0.0.1:17963/v1`**、**api_key**、**可用模型名**。

模型名从 `GET /v1/models` 获取（数量随上游目录变化，2026-10-03 实测 91 条：32 个 cn + 27 个 global + 32 个 CN 裸名）；
国内版前缀 `cn:`，国际版前缀 `global:`。注意 **global 模型只以 `global:` 前缀列出**（裸名一律按 cn 解析）。

客户端（Cherry Studio / NextChat / Cursor / Continue 等）直接填 Base URL + api_key。
若本机走代理，注意 **`127.0.0.1` 必须直连**，不要让客户端把它也塞进代理。
