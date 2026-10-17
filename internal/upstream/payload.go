// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"workbuddy2api/internal/logfmt"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEfforts(src, sanitize, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	// `null` 是**合法 JSON**，Unmarshal 成功但留下 nil map → 下一行写 map 就 panic。
	// 实测：`POST /v1/chat/completions` 请求体为 `null` 时，网关在 handler 内 panic、
	// 连接被直接掐断（客户端拿到的是"无响应/EOF"而不是 400），日志出现
	// "http: panic serving ... assignment to entry in nil map"（fuzz 目标 FuzzPrepareBody 发现）。
	// 契约：**改写不了就原样透传**（上游会以 11101 明确拒绝），绝不 panic。
	if obj == nil {
		return src
	}
	obj["stream"] = true
	// 强制 include_usage=true（D7 强化）：网关依赖末帧 usage 做用量/成本/缓存命中
	// 记账；若客户端显式传 include_usage:false，旧实现原样透传会让上游不回 usage，
	// 整条统计链失去观测。上游本就要求 stream，这里保留客户端其它 stream_options 键。
	if so, ok := obj["stream_options"].(map[string]any); ok {
		so["include_usage"] = true
	} else {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeMaxTokens(obj)
	clampGPTMinMaxTokens(obj) // GPT 系过小 max_tokens 上游一律 400(11133)，发前抬到下限
	normalizeToolsAndFunctions(obj)
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	normalizeToolMessages(obj)
	// tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行。
	if msgs, ok := obj["messages"].([]any); ok { // tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行。
		// 顺序不能换：mergeAdjacentToolCalls 必须最先跑 —— 它把「背靠背的两条 assistant.tool_calls」
		// 合成一条（部分 agent 客户端回放并行调用的报文形状），deepseek 系 11148 的正面修复。
		msgs, _ = mergeAdjacentToolCalls(msgs)

		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		obj["messages"] = msgs
	}
	// 思维链开关（见 thinking.go）：推理模型注入 thinking.type=enabled（仅 deepseek）+ 缺档补默认最高档 max。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	injectThinking(obj)
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	warnUnknownBodyKeys(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传

// knownBodyKeys OpenAI Chat Completions 请求体的已知顶层键（网关会改写或原样透传）。
// 仅用于"未知键"观测告警——不删除、不改写，避免误伤上游其实支持的字段。
var knownBodyKeys = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "temperature": true, "top_p": true,
	"n": true, "stop": true, "presence_penalty": true, "frequency_penalty": true,
	"logit_bias": true, "user": true, "seed": true, "response_format": true,
	"logprobs": true, "top_logprobs": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "functions": true, "function_call": true,
	"reasoning_effort": true, "thinking": true, "metadata": true, "store": true,
	"service_tier": true, "modalities": true, "audio": true, "prediction": true,
	"web_search_options": true, "prompt_cache_key": true, "safety_identifier": true,
	"verbosity": true, "top_k": true, "repetition_penalty": true, "enable_thinking": true,
	"thinking_budget": true, "chat_template_kwargs": true,
}

// warnedBodyKeys 每个未知键只告警一次，避免每请求刷日志。
var warnedBodyKeys sync.Map

// warnUnknownBodyKeys 对未知顶层键做一次性告警。上游对 unknown field 的容忍度会变
// （11129 的 \_ pattern 就是同类前车之鉴），提前观测能在上游收紧时第一时间定位。
func warnUnknownBodyKeys(obj map[string]any) {
	for k := range obj {
		if knownBodyKeys[k] {
			continue
		}
		if _, loaded := warnedBodyKeys.LoadOrStore(k, true); loaded {
			continue
		}
		log.Printf("[upstream] WARN: unknown request body key %q passed through as-is", k)
	}
}

// IsParameterSchemaError 判断 400 是否为工具/参数 schema 级拒收：同一请求换任何账号
// 都会被同样拒收（与账号无关），调用方应直接透传而不是轮转烧号。已知实案：
//
//	11129 invalid_function_call_parameters（tools[].function.parameters 不合规）
func IsParameterSchemaError(body string) bool {
	for _, marker := range []string{
		"11129",
		"invalid_function_call_parameters",
		"invalid function call parameters",
		"invalid_function_parameters",
	} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("WARN: [upstream] reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("WARN: [upstream] reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// maxTokensAsFloat 取 max_tokens 的数值（三型兼容），非数值/缺省返回 false。
func maxTokensAsFloat(obj map[string]any) (float64, bool) {
	switch n := obj["max_tokens"].(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限（实测 15 拒、16 过）。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景（上游 fd3e142 移植）：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）
// 对 max_tokens < 16 一律 400 code=11133 model_param_invalid（同号同 body 对照：15 拒、16 过），
// hy4 等非 GPT 模型无此限制。Claude Code 切模型时会发 max_tokens 极小的探针，全号轮转
// 同样被拒 → 客户端 503，模型永远切不过去（换号无用，只能在发送前修）。
// 抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	fv, ok := maxTokensAsFloat(obj)
	if !ok {
		return
	}
	// **只抬 0<v<16**：v<=0（含客户端用 -1 表"不限"）与越界/非有限值一律不动 —— 把它们
	// 静默改成 16 会把"本该由上游 400 明示"的请求变成 16-token 截断的"成功"，
	// 与"只放宽上限"的语义相反（审查 A 的 P1-3）。负数/0/NaN/±Inf/超 int64 范围全部放行。
	if fv <= 0 || fv >= float64(gptMinMaxTokens) {
		return
	}
	obj["max_tokens"] = int64(gptMinMaxTokens)
	log.Printf("max_tokens clamped model=%s %v -> %d", logfmt.Truncate(model, 64), fv, gptMinMaxTokens)
}

// normalizeMaxTokens 兼容现代 OpenAI 规范的 max_completion_tokens：
// 若请求携带 max_completion_tokens，确保映射至上游 Go struct 识别的 max_tokens。
func normalizeMaxTokens(obj map[string]any) {
	if mct, ok := obj["max_completion_tokens"]; ok && mct != nil {
		obj["max_tokens"] = mct
		// 映射后删除现代字段：同时下发两个会让严格校验的上游 400。
		delete(obj, "max_completion_tokens")
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - 请求携带 tools 列表且未显式指定 tool_choice 时，自动补齐 "auto"（OpenAI 标准默认行为）
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	// 携带 tools 但不指定 tool_choice 时**不改写**：A/B 对照探测（各 3/3）表明上游对
	// "仅带 tools" 的默认处理与显式 "auto" 完全一致，省略即省略更贴近透传语义。
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolsAndFunctions 处理空工具数组清理以及旧版 functions 映射
func normalizeToolsAndFunctions(obj map[string]any) {
	// 1. 若带 functions 但不带 tools，自动包装映射为 tools
	if funcs, hasFuncs := obj["functions"].([]any); hasFuncs {
		if _, hasTools := obj["tools"]; !hasTools {
			var newTools []any
			for _, f := range funcs {
				newTools = append(newTools, map[string]any{
					"type":     "function",
					"function": f,
				})
			}
			obj["tools"] = newTools
		}
	}

	// 2. 若请求携带 "tools": []（空列表），自动从出站请求体中清除 tools 与 tool_choice
	if tools, hasTools := obj["tools"].([]any); hasTools && len(tools) == 0 {
		delete(obj, "tools")
		delete(obj, "functions")
		delete(obj, "tool_choice")
	}
}

// normalizeToolMessages 对 messages 中的工具消息进行自愈和兜底
func normalizeToolMessages(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)

		// 1. 旧版 `role: "function"` 映射为 `role: "tool"` 并补齐 `tool_call_id`
		if role == "function" {
			msg["role"] = "tool"
			name, _ := msg["name"].(string)
			msg["tool_call_id"] = "call_" + name
			role = "tool"
		}

		// 2. 对所有 `role: "tool"` 消息的 `content` 进行检测与自愈
		if role == "tool" {
			content, hasContent := msg["content"]
			if hasContent && content != nil {
				switch v := content.(type) {
				case string:
					// 已经是字符串，放行
				default:
					extracted := false
					if arr, ok := v.([]any); ok {
						textBuf := ""
						isTextParts := true
						for _, item := range arr {
							if m, ok := item.(map[string]any); ok {
								if t, _ := m["type"].(string); t == "text" {
									if txt, _ := m["text"].(string); txt != "" {
										textBuf += txt
									}
								} else {
									isTextParts = false
									break
								}
							} else {
								isTextParts = false
								break
							}
						}
						if isTextParts && len(arr) > 0 {
							msg["content"] = textBuf
							extracted = true
						}
					}
					if !extracted {
						b, err := json.Marshal(v)
						if err == nil {
							msg["content"] = string(b)
						}
					}
				}
			} else {
				msg["content"] = ""
			}
		}

		// 3. Assistant 消息带 tool_calls 时，空 content 兜底
		if role == "assistant" {
			if tc, hasTc := msg["tool_calls"].([]any); hasTc && len(tc) > 0 {
				if msg["content"] == nil {
					msg["content"] = ""
				}
			}
		}
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes 递归改写 schema 树里 pattern 值与 patternProperties
// 键中的 `\_` → `_`（patternProperties 的键也是正则；map 键不可原地改，命中时重建
// 该层）。
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}
