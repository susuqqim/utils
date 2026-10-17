# workbuddy2api

> **归属**：本项目为 [`Sliverkiss/CodeBuddy2api`](https://github.com/Sliverkiss/CodeBuddy2api) 的 fork，
> 面板改动的同步来源为 [`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)。
> 许可：**MIT**（见 `LICENSE`）；本 fork 的修改清单见 `NOTICE.md`。

一个 OpenAI 兼容的网关：把上游账号池聚合成一个 `/v1` 端点，带管理面板、账号轮转、冷却/熔断、
**会话粘滞**、用量账本与自动更新。

---

## 1. 快速开始

### 安装（Windows，推荐）
1. 下载 **`workbuddy2api-1.11.7-x64.msi`** 双击安装（当前用户级安装，无需管理员）；
2. 安装目录默认 `%LOCALAPPDATA%\workbuddy2api`，配置目录 `%USERPROFILE%\.workbuddy2api`；
3. 首次运行前，把 `config.example.json` 复制为 `config.json` 并填好 `api_key` 与上游账号。

### 其他方式
- **原生单文件**：`wb2api-windows-amd64.exe` / `wb2api-linux-{amd64,arm64}` / `wb2api-darwin-{amd64,arm64}`；
- **离线整包**：`workbuddy2api-offline.tar`（含脚本，可离线重建）；
- **源码构建**：
  ```bash
  CGO_ENABLED=0 go build -trimpath -ldflags="-w" -o wb2api-core.exe ./cmd/server
  ```

### 启动与验证
```powershell
# 前台启动（也可用 wb2api.cmd / wb2api-launcher.cmd）
.\wb2api-core.exe -config "$env:USERPROFILE\.workbuddy2api\config.json"
# 健康检查（应含 "healthy":N）
curl --noproxy '*' http://127.0.0.1:17963/healthz
# 模型目录
curl -H "Authorization: Bearer <api_key>" http://127.0.0.1:17963/v1/models
```

## 2. 配置要点（`config.json`）

| 键 | 说明 |
|---|---|
| `listen` | 监听地址，默认 `127.0.0.1:17963` |
| `api_key` | 访问密钥（`Authorization: Bearer <key>`；`/panel` 同密钥）|
| `server.max_body_mb` | 请求体上限（MB），默认 8，建议 64 |
| `server.read_timeout` | 读超时，如 `"300s"` |
| `session_sticky` | **会话粘滞**开关（见 §4）|
| `hidden_models` | 隐藏"有名无服务"的模型：`/v1/models` 不列出、直调本地 400 `model_hidden` |
| `enterprise_gate` | 企业号跳过（默认关）|
| `reqlog.enabled` / `dir` | 请求日志（环形缓冲 + 磁盘归档）|

## 3. 管理面板
浏览器打开 **`http://127.0.0.1:17963/panel/`**，用同一个 `api_key` 登录。能看到：账号池健康/轮转、
**粘性会话绑定数**、模型冷却（含"换模型才有用"的 11102 提示）、用量/前缀缓存命中、请求日志、任务中心。

## 4. 会话粘滞（Session Stickiness）

**是什么**：把同一个"会话"的请求**固定到同一个上游账号**上，而不是每次随机/按权重选号。

**为什么重要**：
1. **前缀缓存命中** —— 上游的提示词前缀缓存是**按账号**隔离的；同一会话粘在同一账号上，
   才能反复命中自己的前缀缓存（实测：命中率可达 **99%+**，单次费用可降到 1/40 量级）；
2. **行为一致** —— 同一会话的工具调用/多轮上下文不会因为换号而"像换了个模型"；
3. **减少上游噪声** —— 不会在多个账号上各建一份缓存、互相驱逐。

**会话键是怎么取的**（`internal/session`，优先级从高到低）：
1. 请求体里的显式 `conversation_id` / `session_id`；
2. HTTP 请求头（如 `X-Conversation-ID`）；
3. **兜底**：首条 `user` 消息的指纹（`ConversationFingerprint`）—— 因此"不带任何会话标识的客户端"
   只要系统提示与首条用户消息稳定，也能稳定粘住同一账号。

**与 `prompt_cache_key` 的关系**：网关会把上面的会话标识注入出站的 `prompt_cache_key`（按账号隔离，
格式 `wb2a-<uid8>-<convHash>`）—— **客户端没带 conversation id 时回退到会话键**；
否则它会退化成"每账号一个常量"，导致同账号上的多个客户端**共用一个缓存槽并互相驱逐**（这正是
"缓存命中率下降"的典型成因，1.11.7 已修）。

**什么时候会自动解绑**：当绑定的账号出现**失效类**错误（限流/失效/被拒）时，网关会**解绑**该会话，
让它在下一次请求重新选号（避免把会话钉死在一个坏号上）。

**怎么观察**：
- 面板首页的「**粘性会话**」统计 = 当前绑定数；
- `GET /status` 返回里的 `sticky`（以及 `paused`/`cooling`/`enterprise_accounts` 等）。

**开关**：`session_sticky`（默认开启；关闭后按账号权重选号，缓存命中会明显变差）。

## 5. 协议支持
- ✅ **OpenAI 兼容**：`POST /v1/chat/completions`（流式/非流式）、`GET /v1/models`、`GET /status`、`GET /healthz`；
- ❌ **Anthropic 原生协议**（`/v1/messages`）**尚未支持**（可作为后续特性）；
- 模型名可加域前缀：`cn:<id>` / `global:<id>`（国际版）。

## 6. 自动更新（`scripts/update/`，MSI 亦会安装到安装目录）

```powershell
# 只检查（推荐放进每日任务）
wb2api-update.ps1 -CheckOnly

# 检查并更新（下载 → 校验 sha256 → 停服 → 备份 → 替换 → 启动 → 健康检查；
# 健康检查失败会【自动回滚】到旧版本）
wb2api-update.ps1
```
- **通道回退**：GitHub raw → GitHub Release 直连 → **代理前缀**（`ghproxy.net` / `gh-proxy.com` /
  `mirror.ghproxy.com`）→ **Gitee**；哪一个先成功就用哪一个；
- **强制校验**：`latest.json` 里的 `sha256` 必须与下载物一致，否则拒绝安装；
- **日志**：`%LOCALAPPDATA%\workbuddy2api\update.log`；
- **发布方**：`make-manifest.ps1 -Version x.y.z -Binary .\wb2api-core.exe` 生成 `latest.json` + `SHA256SUMS`。

## 7. 常见问题
- **连不上上游**：检查代理/网络与账号是否过期；`/status` 看 `healthy` 与 `cooling`；
- **缓存命中低**：确认 `session_sticky` 开启、客户端会话标识稳定（见 §4）；
- **模型不可用**：`/v1/models` 是否列出它？若被 `hidden_models` 隐藏则直调会回 `400 model_hidden`；
- **升级/回滚**：`upgrade-with-rollback.ps1` 或 §6 的更新脚本；旧包保存在安装目录 `*.bak-*`。

## 8. 免责
仅供学习与研究；请遵守上游服务条款与当地法律法规，并保留本项目 `LICENSE` / `NOTICE.md` 的归属声明。
