# AGENTS.md — WorkBuddy2API 接入与 Agent 集成手册

> **动手改这块代码前，先扫一遍 [`工作区文档/踩坑清单.md`](../工作区文档/踩坑清单.md)**（31 条坑：协议兼容/_finish_reason 重复_、`<=0` 约定、时区、LF 行尾、embed 入库、密钥守卫、ConPTY 控制台…）。


> 面向 **AI Agent / Coding Agent** 的专用文档（人类用户请先读 README.md）。
> 目标：让 Agent 在 30 秒内知道这个网关暴露什么、怎么连、项目已经把它接进了哪些 Agent、
> 以及排障与安全边界。

---

## 0. 一句话

本项目是一个**本地自托管网关**：把 WorkBuddy / CodeBuddy 账号额度池化成 **OpenAI 兼容 API**，
监听 **http://127.0.0.1:17963**，供 DSH / OpenCode / Cherry Studio / Cursor / Cline / Roo 等
Agent 以标准 OpenAI 协议调用。上游是 Go 单二进制（含内嵌管理面板），支持 Docker 与原生两种部署。

---

## 1. 端点契约（Agent 必读）

基础信息：

    Base URL   : http://127.0.0.1:17963/v1
    Auth       : Authorization: Bearer <api_key>
                 （api_key 来自 config.json；若为空则网关不鉴权）
    协议       : OpenAI Chat Completions（含 SSE 流式）

可用端点：

    GET  /v1/models               模型列表（含 context_length / max_output_tokens）
    POST /v1/chat/completions     对话（stream 可选）
    GET  /healthz                 健康探针（无需鉴权）
    GET  /status                  账号池状态（需 Bearer）
- **`/status` 汇总键契约**（运维/脚本按此消费）：`total` / `healthy` / `cooling` / `disabled`（**口径含 paused**，上游语义）/ `paused`（独立计数，暂停选号）/ `credit_floor`（当前积分保底，0=关闭）/ `in_flight_full` / `sticky_sessions` / `realm_totals` / `accounts[]`（逐账号 `disabled` 与 `paused` **严格分离**）。新增键只做追加，不改既有语义。

    GET  /panel/                  管理面板（浏览器；数据接口 /panel/api/* 需 Bearer）

模型命名协议（网关侧路由前缀）：

    裸名         -> realm 缺省为 cn，例如 glm-5.3 / deepseek-v4.1-flash
    cn:<name>    -> 强制国内版，例如 cn:glm-5.3
    global:<name>-> 强制国际版，例如 global:gpt-5.5
    前缀在出站前会被剥离，上游收到的是裸名。

    /v1/models 的列出契约（2026-10-03 起）：
      - cn 模型：同时提供 `cn:<名>` 与裸名（裸名按 cn 解析，**可直接用**）；
      - global 模型：**只提供 `global:<名>`**。global 独有的模型不再以裸名列出 —— 因为裸名
        会按 cn 解析，列出来等于一张必然失败的票（实测曾有 18 个这样的条目）。

---

## 2. 对 Agent 友好的行为（保真特性）

Agent（尤其是工具调用型）依赖这些行为，网关已对齐：

    出站强制 stream:true，非流式由网关本地聚合成一次性响应。
    reasoning_content 归一化：reasoning / thought 统一为 delta.reasoning_content。
    思考档位动态降级：从上游 supportedEfforts 取权威档位，按请求档平滑降到 <= 请求档的最高合法档。
    工具调用保真：
        - finish_reason 纠偏（有 tool_calls 时强制为 tool_calls）；
        - role:tool 的复合 content 自愈序列化为字符串；
        - 跨分片 tool_call 元数据回填；
        - 旧版 functions 自动升级为 tools；
        - tool_choice 改写：省略则不改写（实测上游默认与显式 auto 等价）、required 原样、
          none 抑制 tools、函数对象扁平化为名称。
    缓存命中口径：prompt_tokens_details.cached_tokens 镜像到 cache_read_input_tokens 等别名；
    网关自身的用量统计也采集该命中量（/panel/api/usage 的 cached_tokens），面板用量页展示命中率。
    客户端取消：记为 499，不惩罚账号、立即断开上游（止损省积分）；用量统计计请求但不计失败。
    出站强制 stream_options.include_usage=true（保留其它键）：保证末帧 usage 可用于记账。
    SSE 解析容忍 "data:" 后无空格；流式读错误/上游 error 帧都会以 error 帧告知客户端（不静默截断）；
    `event: error` 控制行透传，`id:`/`retry:`/其它 event 丢弃。
    非流式聚合按 choice.index 分桶（n>1 不串台），识别 error 帧、保留 function_call/logprobs/音频。
    长思考保活：静默 10s 推送 SSE 注释行 ": keep-alive"。

---

## 3. 项目已经如何把网关接入 Agent（现状链路）

这是本工作区已有的、正在运行的集成方式——以“清单”为单点真相，同步到各客户端的配置：

    [上游能力清单/ 或本机客户端日志]   （离线采集，零上游消耗）
              |
              v  生成 / 维护
    /mnt/d/kk/agent配置/workbuddy_models_manifest.json   <-- 权威单点真相
              |     gateway: { baseURL, apiKey, protocol: openai-completions }
              |     models : 17 个官方 CN 模型 + reasoningEfforts / contextWindow / maxTokens / input
              v
    /mnt/d/kk/agent配置/sync_to_clients.py   （增量无损合并；改前自动 .bak-YYYYMMDD-HHMMSS）
              |
              +--> WSL DSH      : ~/.dsh/settings.yaml + ~/.dsh/.credentials.yaml
              +--> Windows DSH  : /mnt/c/Users/GG/.dsh/settings.yaml + .credentials.yaml
              +--> WSL OpenCode : ~/.config/opencode/opencode.json
              +--> Windows OpenCode : /mnt/c/Users/GG/.config/opencode/opencode.json
              +--> Cherry Studio    : cherry_studio/cherry_studio_models.json
              +--> CC-Switch 模板   : tools/ccswitch/settings_config.json

关键约束与设计：

    同步器只更新 workbuddy2api 这一个 provider，完整保留其它 provider（tokenrhythm 等）。
    规范模型名（裸名），避免 DSH 界面出现 (cn:) 重复项。
    YAML dumper 关闭别名，并把 off/on/yes/no 强制加引号，防止 YAML 1.1 误判为布尔。
    运行方式： python3 sync_to_clients.py   （在 /mnt/d/kk/agent配置 目录下）
    客户端模板： /mnt/d/kk/agent配置/dsh/ 、 /opencode/ 、 /cherry_studio/

DSH 提供商片段（节选自 dsh/dsh_settings_workbuddy2api.yaml）：

    llm-pi-ai:
      providers:
        workbuddy2api:
          displayName: WorkBuddy Gateway
          apiKeyEnv: WORKBUDDY_API_KEY
          api: openai-completions
          baseURL: http://127.0.0.1:17963/v1
          models:
          - id: deepseek-v4.1-flash
            reasoningEfforts: { 'off': null, low: low, high: high, max: max }

---

## 4. Agent 直接接入（不改任何客户端配置）

任何支持 OpenAI 协议的 Agent / SDK，直接填：

    base_url : http://127.0.0.1:17963/v1
    api_key  : config.json 中的 api_key（初始值也在同目录 .apikey）
    model    : 裸名（如 glm-5.3 / deepseek-v4.1-flash / kimi-k2.7）或带前缀（cn:/global:）

Python 示例：

    from openai import OpenAI
    client = OpenAI(base_url="http://127.0.0.1:17963/v1", api_key="YOUR_KEY")
    r = client.chat.completions.create(
        model="glm-5.3",
        messages=[{"role": "user", "content": "ping"}],
        stream=True,
    )

---

## 5. 部署形态（Agent 可能被要求操作）

    选型：桌面/个人默认用原生（B）；服务器/NAS/需环境一致/已用 Docker 用容器（A）。
    两者同源码、核心功能一致、config.json schema 互通。

    A. Docker（服务器 / 固定环境）
       目录： release/workbuddy2api-offline/
       容器： workbuddy2api ，镜像 workbuddy2api:offline ，端口 127.0.0.1:17963
       脚本： 1-start.cmd / wb2api.sh up 等
       额外： 镜像内附带 credit/signin/trial/activity CLI 与 scripts/*.py 辅助脚本

    B. 原生（无 Docker，无 npm）—— 桌面默认
       目录： release/native/ ，源码 workbuddy2api/scripts/native/
       安装： install.sh（Linux/macOS） / install.ps1（Windows）
            或 Windows 图形安装：workbuddy2api-<版本>-x64.msi（每用户、免管理员、
            建桌面+开始菜单快捷方式；升级前先 wb2api stop）
       命令： wb2api start|stop|status|panel|logs|setkey|run|autostart
       托盘： Windows 默认开启（与网关同进程，左键开面板、右键菜单含重启/自启/退出）；
            启用时隐藏控制台，`wb2api run` 不启用托盘以保留前台日志；
            关闭用 config.json `ui.tray=false` 或 `-no-tray`。
            非 Windows 恒不生效。

---

## 6. 运维与排障（Agent 常用）

    健康： curl -s --noproxy '*' http://127.0.0.1:17963/healthz
    账号： curl -s --noproxy '*' -H "Authorization: Bearer $KEY" http://127.0.0.1:17963/status

错误码语义（务必区分，别乱重试）：

    403 + code 11140 : 账号/设备级风控，不要反复重试（越试越紧），换号或等待。
    429 + code 6004  : 频率限制，网关按上游给的 reset 自动软冷却（模型级独立台账）。
    499              : 客户端主动取消，非错误，不惩罚账号。
    400 + code invalid_request_error
                     : 轮转耗尽后判定"请求体本身有问题"（11101 Unmarshal chat params failed /
                       11133 model_param_invalid / 上下文超限 / too many tokens）。**不要重试**，
                       改请求。若请求看起来正常，查网关日志（网关侧改写回归也会产生 11101）。
    400 + code model_unavailable
                     : 该模型在已试账号的后端不可用（上游 11102「该后端无此模型」）。**不要重试**；
                       若这是国际版模型，用 `global:<模型名>` 前缀；可用清单见 /v1/models。
    403 + code cross_origin_denied
                     : 仅当 `api_key` 为空时出现——网关此时不鉴权，只放行本机回环同源来源
                       （防 DNS rebinding / 任意网页驱动本机网关）。设置 api_key 即可放行其它来源。
    503 + code no_healthy_account
                     : **唯一可重试**的终态：池里确实没有可用账号（冷却/熔断/禁用/暂停选号/在途占满），
                       或未识别的上游 4xx/5xx/传输错误。SDK 按退避重试即可。

Docker 操作注意（WSL 调宿主 docker.exe）：

    命令末尾必须加 < /dev/null，否则 WSL 互操作会挂起。

测试约定（改代码前必读，来自 2026-10-03 三轮审计）：

    1. 后台 goroutine 按形态三选一（goroutine 内的 panic **不被 net/http 的 handler recover
       兜住**，会直接终止整个网关进程；包错了则会"静默残废"，比崩溃更难查）：
         - 长驻循环（ticker/for-select）→ 顶层 `gosafe.Do` + **每轮迭代** `gosafe.Loop`
           （只包顶层时，一次 panic 会让"落盘/GC/跨日重置/排程"永久停摆且无任何迹象）；
         - 一次性任务 → `gosafe.Do`；上游探测另有 `safeProbe`（同族，带结果通道）；
         - **调用方在等结果**（channel/wg）→ `gosafe.DoErr`，把 panic 变成一条结果，
           否则等待方假挂起（等满超时，如 auto_all 会白等 15 分钟）。
       锁纪律：被 `Loop` 包住的迭代体内持锁必须 `defer Unlock()`——Go 的 mutex 不随 panic 释放，
       手工解锁遇到 panic 就是永久死锁（会话/账号池层面尤其致命）。
    2. `t.Parallel()` 用例**禁止**改包级状态（os.Stdout / chatLogOut / 包级 var / 环境变量）：
       并行用例之间会重叠，串行用例不会 —— 污染只会在"多了一个并行用例"时突然发作。
    3. 分布/时序类断言必须注入确定性时钟（`Pool.SetClock`）或零化时间窗；真实时钟下同一实现
       会随机器快慢漂移（实测 11%~23%），这类用例既不能当门禁也会 flake。
    4. 修 bug 时同步写"回退即变红"的回归用例：只断言公式常数的用例等于没测（详见路线图 §T.7.1）。
    5. **测试不得依赖真实网络**：用 `newFakeUpstream` 之类的注入设备；`upstream.New()`
       默认 `GlobalEnabled=false`（生产由 cmd/server 按配置显式开启），所以**不要**在测试里
       把它打开——那会让"池内有 global 账号 + 读模型清单"的用例真的出网（曾发生过一次）。
    6. **门禁必须包含 `TZ=UTC go test ./... -count=1`**：本仓所有日界/时段已统一为固定 CST
       （`internal/cstime`），本机（Windows/macOS 在中国区）`time.Local == CST`，所以
       "生成用 CST、解析用本地"这类错配在开发机上**完全不可见**（第十一轮审计实测：
       `nextFire` 改 CST 后本机四道门禁全绿，`TZ=UTC` 一跑立刻 8 条红）。
       测试里的时点一律用 `cstime.Zone` 构造，不要用 `time.Local`。

---

## 7. 安全边界（Agent 必须遵守）

    绝不外发： auths/*.json（明文 access/refresh token）、config.json、.apikey、
              workbuddy_models_manifest.json 里的 apiKey、以及任何运行态日志中的 token。
    账号授权必须由人类在浏览器完成 OAuth，Agent 不接触、不读取、不输出 token 内容。
    服务只绑 127.0.0.1，不要改绑 0.0.0.0（不要暴露局域网/公网）。
    修改 config.json 后需重启生效（Docker 重建容器 / 原生 wb2api stop && wb2api start）。

    工作目录：进程启动时会自动 chdir 到 -config 指定文件所在目录（见路线图 §O）。
    因此 config.json 里的相对路径（默认 ./auths、./data/state.json）以**配置文件所在目录**为准，
    与手动启动/开机自启无关；派生子进程也继承该工作目录。

---

## 8. 关键文件索引

    网关源码            workbuddy2api/
    端点实现            workbuddy2api/internal/server/handler.go
    账号池/调度         workbuddy2api/internal/pool/
    上游客户端/SSE      workbuddy2api/internal/upstream/
    管理面板            workbuddy2api/internal/panel/
    原生发行脚本        workbuddy2api/scripts/native/
    Docker 活部署        release/workbuddy2api-offline/
    原生发行物          release/native/
    Agent 配置中心       /mnt/d/kk/agent配置/（manifest + sync_to_clients.py + 模板）
    离线能力清单        /mnt/d/kk/workbuddy反代/上游能力清单/
    项目全景路线图      /mnt/d/kk/workbuddy反代/工作区文档/项目全景与改进路线图.md

## 9. 管理面板现状（Agent 操作面板时参考）

面板为 go:embed 单文件（`internal/panel/index.html` + `app.js`），前端 0 构建依赖，CSP `script-src 'self'`。
密钥与 `/v1/*` 同口径；面板 HTML 匿名可加载，密钥只发给 `/panel/api/*`。

    账号池      卡片视图（顶栏「完整 / 精简」切换，localStorage 持久化）。每卡：身份/域、
                状态与梯队（Tier / 主刷中 / 冷却 / 禁用 / 已暂停选号）、积分大数+包数、今日/累计消耗、
                48h 临期（>48h 显示"最近 N 天后到期"）、按金额占比的积分包进度条、
                成功/失败/在途/最近成功、今日签到徽标、模型级限流台账（模型 + 预计解封时间），
                以及强制/签到/余额/任务/解冻/移除。已移除旧表格视图。
    用量        分组 KPI（Token 用量 / 请求与质量 / 积分消耗），含缓存命中 token 与命中率；
                Token/积分 双模时序图；独立「调度卷轴」区块（来自 /panel/api/logs）。
    积分构成    按账号默认折叠（<details>），展开看逐包明细；顶部"积分到期分布"按剩余天数
                聚合、段色按账号稳定分配（/panel/api/packages 的 expires_at 为 Unix 毫秒）。
    任务中心    扫描待办 / 并发执行队列 / 进度条 / 按账号分组；单号任务弹窗。
    配置        表单映射 config.json；修改后热重载。
    运行日志    按通道（task/chat/sys）分色，逐条分隔线。

新增只读数据字段：

    /panel/api/overview 账号对象   last_checkin_at / last_checkin_status（运行态签到结果 ok/already/fail/skipped）
    /panel/api/usage              totals / by_realm / by_account / by_model / series 均含 cached_tokens

## 10. 本机运行环境（两套原生实例，务必区分）

    A. 生产（Native Desktop；2026-09-30 由 Docker 迁移，见路线图 §I/§J）
       程序： %LOCALAPPDATA%\workbuddy2api\wb2api-core.exe
       数据： %USERPROFILE%\.workbuddy2api\（auths/ + data/ + config.json）
       端口： 127.0.0.1:17963        进程名： wb2api-core.exe
       控制： %LOCALAPPDATA%\workbuddy2api\wb2api.cmd start|stop|status|panel|setkey

    B. 测试（独立实例，仅用于验证改动，不参与生产）
       程序： %LOCALAPPDATA%\workbuddy2api-test\wb2api-core-test.exe
       数据： %USERPROFILE%\.workbuddy2api-test\（生产快照；排程全部关闭）
       端口： 127.0.0.1:17964        进程名： wb2api-core-test.exe
       控制： %LOCALAPPDATA%\workbuddy2api-test\wb2api.cmd start|stop|status|panel

隔离与操作约定：

    端口 / 数据目录 / 进程名三者皆不同：测试启动器的 taskkill 目标为 wb2api-core-test.exe，
    因此 wb2api stop 不会误杀生产。切勿把测试产物安装到生产的 AppDir/数据目录。
    测试实例的 auths/ 与 data/ 是生产快照；排程（签到/保活/旅行/活跃/成长/夜猫/余额刷新）
    全部关闭，避免与生产抢同一批账号的上游调用。
    更新测试实例：重建二进制后只替换 wb2api-core-test.exe；生产待验收后再换。
    两实例共用同一批上游凭证，故**不要**在测试实例上开启排程或跑批量任务。
