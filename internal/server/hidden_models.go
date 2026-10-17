// hidden_models.go —— 隐藏"有名无服务"的模型（网关侧过滤 + 本地直接拒绝）。
//
// 起因（实测 2026-10-10）：上游目录里长期保留着一批**没有服务**的模型名 —— 直接请求会收到
// 上游 `http 400 {"code":11102,"msg":"model [glm-4.7] service info not found"}`；而历史用量显示
// 这些模型在本网关**从未产出过一个 token**（请求=失败=账号数）。但目录照旧列出它们 ⇒ 客户端
// 枚举后逐个尝试 ⇒ 每次都在池里写下 6h（→12h→24h）**模型级负缓存**，面板出现一片
// "预计 X 后重试"，既难看又掩盖真正的临时限流。
//
// 本功能的语义：这些模型**连试都不试** ——
//
//	· /v1/models 不再列出（客户端枚举不到就不会试）；
//	· 直接请求则**本地 400 拒绝**（不打上游、不计费、**不写任何冷却**）。
//
// 配置：顶层 `hidden_models: ["glm-4.6", ...]`（按 bare 模型名匹配，`cn:`/`global:` 前缀自动兼容）。
// 默认空 = 完全不改变行为（零回归）。
package server

import "strings"

// hiddenSet 把配置里的名字规范化为集合（去空白、去 realm 前缀、去重）。
func hiddenSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		out[bareModelID(n)] = struct{}{}
	}
	return out
}

// bareModelID 去掉 `cn:` / `global:` 前缀，拿到可比较的模型名。
func bareModelID(id string) string {
	if i := strings.IndexByte(id, ':'); i > 0 {
		switch id[:i] {
		case "cn", "global":
			return id[i+1:]
		}
	}
	return id
}

// isHidden 报告某个（可能带前缀的）模型名是否被隐藏。
func (h *Handler) isHidden(model string) bool {
	set := h.hidden
	if len(set) == 0 || model == "" {
		return false
	}
	_, ok := set[bareModelID(model)]
	return ok
}

// filterHidden 从目录条目里剔除被隐藏的模型（保留原顺序）。
func filterHidden[T any](items []T, id func(T) string, hidden map[string]struct{}) []T {
	if len(hidden) == 0 || len(items) == 0 {
		return items
	}
	out := make([]T, 0, len(items))
	for _, it := range items {
		if _, bad := hidden[bareModelID(id(it))]; bad {
			continue
		}
		out = append(out, it)
	}
	return out
}
