package upstream

// normalizeUsageCacheAliases keeps cache-hit aliases consistent before the
// response leaves the gateway. Some WorkBuddy responses carry the real hit in
// prompt_tokens_details.cached_tokens while also emitting
// cache_read_input_tokens: 0 and cached_tokens: 0 compatibility aliases.
// Strict downstream parsers may prefer those zero aliases and lose the hit.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// Responses API consumers use this nested form. Preserve it when the
	// upstream already supplies it, but do not invent it for Chat-only clients.
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

// UsageCacheHitTokens 提取 usage 中的缓存命中 token 数（多处别名的最大值），
// 供网关侧用量统计与响应改写复用同一口径。
func UsageCacheHitTokens(usage map[string]any) (int64, bool) {
	n, ok := bestUsageCacheHitTokens(usage)
	if !ok || n <= 0 {
		return 0, false
	}
	return int64(n), true
}

// UsageReasoningTokens 取 usage 里的推理 token 数（推理模型的"思考"部分，
// 已含在 completion_tokens / output_tokens 内）。多别名兼容：
//
//	completion_tokens_details.reasoning_tokens  （OpenAI chat）
//	output_tokens_details.reasoning_tokens      （Responses 风格）
//	reasoning_tokens                            （部分网关平铺）
//
// 注意：本函数要求 >0 才返回 ok（与缓存命中同风格）——展示侧若需要区分
// "显式 0"与"缺失"，应改走带 has 语义的解析；当前仅在"有推理"时才展示拆分。
func UsageReasoningTokens(usage map[string]any) (int64, bool) {
	if usage == nil {
		return 0, false
	}
	n, ok := bestUsageNumber(usage, []usagePath{
		{"completion_tokens_details", "reasoning_tokens"},
		{"output_tokens_details", "reasoning_tokens"},
		{"", "reasoning_tokens"},
		{"completion_tokens_details", "reasoning"},
	})
	if !ok {
		return 0, false
	}
	return int64(n), true
}

type usagePath struct {
	section string
	key     string
}

// bestUsageNumber 按别名路径依次取值，返回第一个正数。
func bestUsageNumber(usage map[string]any, paths []usagePath) (float64, bool) {
	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}
