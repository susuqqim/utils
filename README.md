# workbuddy2api（干净快照）

> **归属**：本项目为 [`Sliverkiss/CodeBuddy2api`](https://github.com/Sliverkiss/CodeBuddy2api) 的 fork，
> 面板改动的同步来源为 [`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)。
> 许可：**MIT**（见 `LICENSE`）；本 fork 的修改清单见 `NOTICE.md`。

本目录是 `workbuddy2api` 网关的**干净快照**：只含源码/脚本/示例配置/文档，
**不含**任何凭证（`.apikey`、`auths/`、`config.json`、运行态 `data/`）、构建产物（`*.exe`）
或仓库历史。

## 来源与许可
- 本项目为上游 fork，许可与署名见 `LICENSE`（若上游另有署名要求，请在此补充）。
- 本快照由本机 `workbuddy2api` 工作副本导出。

## 快速开始
- Windows 安装：见 `install.ps1` / `README-quickstart.md`
- 配置：复制 `config.example.json` 为 `config.json`，填写 `api_key` 与账号（`auths/`）
- 构建：`CGO_ENABLED=0 go build -trimpath -ldflags="-w" -o wb2api-core.exe ./cmd/server`

## 注意
- 首次运行前请设置强随机 `api_key`，并把 `config.json`、`auths/`、`data/` 加入 `.gitignore`。
