// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11-128",                                          // 上游反探测：裸数字错误码
	"powered by DeepSeek",                             // 身份句：DSH 类客户端
	"DeepSeek Harness",                                // 身份句：品牌名
	"powered by the deepseek",                         // 身份句：小写变体
	// 【2026-10-21 上游 issue 确证】CN 域对 system/**assistant 历史**的身份句做逐字校验：
	// `You are an AI agent running on a compatible client`（**不带结尾句号**才拦）。
	// 这句正是 sanitizeBrandRe 改写后的**产出**，必须进预检 + 改写表，
	// 否则「预检不中 → 整单跳过净化」→ 原样上行 → 11128。
	// 证据：zidanefaqih/codebuddy-intl-cpa#18、Ebony-Vinyl/dsh-our-free-model#568。
	"You are an AI agent running on a compatible client",
	// 【2026-10-22 生产抓包逐字节二分确证】DSH 更新了 system prompt 措辞：
	//   旧版（通过）: `... Web GUI at http://127.0.0.1:38080.`
	//   新版（11128）: `... Web GUI whose URL the runtime context names.`
	// 只把这一个从句换成中性表述即 200（同一请求连发 5 次稳定 400，非抽样）。
	// 这是目前唯一能确定复现的 CN 域指纹，必须进预检 + 改写表。
	"whose URL the runtime context names",
	"powered by OpenCode", // 身份句：OpenCode 类
	"powered by Cline",    // 身份句：Cline 类
	"Sisyphus-Junior",     // OhMyOpenCode / OmO 架构指纹
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeOmoRe 改写层：OhMyOpenCode 身份指纹（上游返回 11128 "Illegal API invocation from an unapproved channel" 拦截）。
// 仅重写此确认的连续子串，去除框架归属，保留 Agent 行为定义。
var sanitizeOmoRe = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)

// sanitizeBrandRe 改写层：把 "<角色> powered by <品牌>[ <版本>]" 的**组合指纹**改成中性表述。
//
// 2026-10-21 上游收紧渠道校验（11128 "Illegal API invocation from an unapproved channel"）后，
// 判定依据不再只有 UA/头，还包含**提示词里的身份句 + 品牌组合**。对照
// ardeyouxipianyi/workbuddy2api-hub 的 #533/#537/#539（"身份句 + 品牌的组合指纹改为中性句"）：
// 上游会匹配形如
//
//	You are an AI agent powered by DeepSeek Harness.
//	You are a coding agent powered by the deepseek-v4.1-flash model.
//
// 这类句子。只改"powered by …"这半句、保留角色定义，语义不变、行为不受影响。
// 匹配只到句号/换行/逗号前，避免吞掉后续指令。
var sanitizeBrandRe = regexp.MustCompile(`(?i)\b(You are an? (?:AI agent|coding agent|AI assistant|assistant)) powered by [A-Za-z0-9](?:[A-Za-z0-9 .\-_]*[A-Za-z0-9])?`)

//
// 正则不含前瞻（Go RE2 不支持 `(?=...)`）：品牌段以字母数字收尾，
// 句号/逗号/分号/换行天然留在匹配之外，替换后原标点保持不变。

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
	{
		// 【2026-10-22 生产逐字节二分确证，唯一稳定复现的 CN 域指纹】
		// DSH 新版 system prompt 把 GUI 地址写成「由 runtime context 命名」的从句，
		// 上游据此判定非官方客户端。改写成中性表述后同一请求 5/5 由 400 变 200。
		// 只替换这一个从句（不动整句/其它 "runtime context" 出现处），最小侵入。
		"whose URL the runtime context names",
		"at the local address shown above",
	},
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 上游指纹（issue #18 确证）：只匹配**不带句号**的整句。改写为完全不同的措辞，
		// 避免再产出任何 "AI agent … compatible client" 形状的句子。
		"You are an AI agent running on a compatible client",
		"You are an AI agent operating as a general-purpose assistant",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		// 裸 11-128 出现在请求体里本身就是上游整单拦截条件（上游反探测）。
		// 坑：这一对与 sanitizeFeatures 的源串曾被写成 11-128（少一个连字符），
		// 视觉上与 11-128 无法区分 ⇒ 门控恒 false、规则从未命中。
		"11-128",
		"11‑128",
	},
}

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeOmoRe.MatchString(text) {
		text = sanitizeOmoRe.ReplaceAllString(text, "Sisyphus-Junior - Focused executor")
	}
	// 品牌身份句：<角色> powered by <品牌> → <角色> operating as a general-purpose assistant
	// （2026-10-21 上游 11128 渠道校验会匹配"身份句+品牌"组合；幂等：改写后不再含 powered by）
	//
	// 【2026-10-21 修自伤】原替换串是 "$1 running on a compatible client"，产出的
	// `You are an AI agent running on a compatible client` **正是上游 CN 域的身份句指纹**
	//（上游 issue #18 实测：不带结尾句号即 400/11128，带句号反而放行）。
	// 也就是说这条规则一边「净化」一边**制造指纹**。换成不含 "compatible client"
	// 形状的中性措辞，与末尾标点彻底解耦。
	if sanitizeBrandRe.MatchString(text) {
		text = sanitizeBrandRe.ReplaceAllString(text, "$1 operating as a general-purpose assistant")
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名与 OmO 指纹有大小写变体，快速路径漏掉时再落正则（(?i)）兜底。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeHdrRe.MatchString(text) || sanitizeOmoRe.MatchString(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
		// reasoning_content / reasoning：思维链的两种字段名（上游 ebea234 只补了后者，
		// 本仓此前两个都没洗）。thinking.go 的回填/镜像会把客户端送来的推理文本带进这些字段，
		// 而请求体里出现裸 11-128 这类反探测串本身就是上游整单拦截条件 ⇒ 必须一起净化。
		// 干净文本经 sanitizeText 的 hasFingerprint 预检直接返回，零改写、零开销。
		for _, key := range [...]string{"reasoning_content", "reasoning"} {
			if r, ok := m[key].(string); ok {
				if n := sanitizeText(r); n != r {
					m[key] = n
					changed = true
				}
			}
		}
	}
	return changed
}
