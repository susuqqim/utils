# NOTICE / 归属

本项目是以下上游项目的 **fork / 二次开发**，遵循 **MIT License**（见 `LICENSE`）：

- **原始上游**：`https://github.com/Sliverkiss/CodeBuddy2api`
  （`LICENSE` 中的 `Copyright (c) 2026 Sliverkiss`）
- **面板相关改动的同步来源**：`https://github.com/linguo2625469/workbuddy2api-panel`

## 本 fork 的修改
- 端点/协议：`prompt_cache_key` 会话回退（无 conversation id 时）、空 `function.name` 的 tool_call 否决、
  DSML 原生工具调用标记的解析与流式/非流式接线；
- 安全：标记修复解析器的缓冲上界（超限放弃修复并原文透出）、放弃路径不丢字节；
- 运维：`scripts/update/` 下的**远端自动更新检查/应用**（含 GitHub → 代理前缀 → Gitee 的通道回退、
  sha256 强制校验、健康检查失败自动回滚）、离线包与原生分发的打包脚本。

## 免责
本项目仅供学习与研究使用；使用者需自行承担合规责任，并遵守上游许可与本文件所述归属要求。
