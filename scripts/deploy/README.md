# Docker/离线交付包脚本（唯一真相所在目录）

## 为什么有这一份
`release/workbuddy2api-offline/` 是**离线交付包目录**：里面除了这些脚本，还有
**整份源码副本**（`cmd/`、`internal/`）、编译好的二进制（`wb2api.exe`、`wb2api_linux`…）、
以及**含真实凭证的 `config.json` / `.apikey` / `auths/` / `data/`** —— 因此整个
`release/` 被 `.gitignore` 忽略，是**生成物**，不是源码。

问题（第十三轮审计 F5）：那天修好的 4 个部署脚本曾用 `git add -f` 强加进版本控制，
造成"半入库"：目录里 4 个文件在库、其余 40+ 文本与全部凭证不在库 →
`git status` 会误导（改 Dockerfile 看不见、改这 4 个却看得见），`git clean -xdf`
会删掉未入库的凭证却保留这 4 个。

## 现在的约定
- **本目录是这些脚本的唯一真相**（在版本控制里，可 review、可回溯）。
- `release/workbuddy2api-offline/` 保持**整体忽略**（生成物 + 凭证）。
- 交付/部署前把本目录同步进交付包：

      bash workbuddy2api/scripts/deploy/sync-to-bundle.sh            # 默认 release/workbuddy2api-offline
      bash workbuddy2api/scripts/deploy/sync-to-bundle.sh <目标目录>

## 文件
| 文件 | 作用 |
|---|---|
| `docker-entrypoint.sh` | 容器入口：目录/权限自愈（**不再 777/666**，改 `u+rwX,g+rwX,o-rwx` + config 640）、app 用户降权与 root 逃生分支 |
| `wb2api.sh` | 宿主侧运维脚本：`up/down/logs/health/doctor/setkey/login/…`；`setkey` 省略参数即从终端/stdin 读（不进 shell 历史），登录 URL 0600 且流程结束即清理 |
| `setkey.py` | 把 api_key 写入 config.json（密钥只经环境变量 `WB2A_NEWKEY`、写入 tmp+fsync+`os.replace`） |
| `agent_login.py` | 容器内 OAuth 登录写凭证（tmp + `os.replace` 原子替换，覆盖前留 `.bak`） |

改动这些脚本时请同步跑一次 `sync-to-bundle.sh`，避免交付包与真相漂移。

## ⚠️ 交付包里的源码副本不要改
`release/workbuddy2api-offline/` 内还有 `cmd/`（20 文件）与 `internal/`（150 文件）—— 那是**打包当时
的源码快照**（供容器内构建/对照），**不是**源码真相。改那里的副本不会进版本控制、下次打包即被覆盖，
且会与仓库源码悄悄分叉。**要改源码请改仓库里的 `workbuddy2api/`**（第十四轮审计 B4 的提醒）。
