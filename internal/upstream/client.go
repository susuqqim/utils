// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"workbuddy2api/internal/cstime"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	ErrAccountFault                  // 账号级授权/配额故障（11140 request illegal / 14017 trial not activated）→ 冷却轮换，不无限重试
	ErrChannelRefused                // 11128 request illegal 通道校验失败（上游认为请求不是官方客户端发的）→ 不罚账号
	ErrModelBlocked                  // 11102「该后端无此模型」→ (账号,模型) 负缓存避让，切模型/切账号
	ErrWafBlock                      // 403 + 非业务信封体（APISIX WAF 拦截页/空体）→ 账号软冷却 + 抖动退避
	ErrPromptTooLong                 // 11115「prompt is too long」→ 请求级错误（上下文超限）：不罚号、不轮转，立即透传原文
	ErrImageInvalid                  // 11135 / 图片格式或数据无效 → 请求级错误：不罚号、不轮转，立即 400 透传原文
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrChannelRefused:
		return "channel_refused"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// 注意：严禁收录裸 "too many"，否则会误伤 "too many tokens" 等超长 Context 客户端参数错误，
// 引发健康账号被误判 ErrSoftRate 施加 600s 软冷却并击穿全池（issue #4 优化项）。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many concurrent",
	"too many connections",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	// 这两句历史上一度被当作内容审核文案；2026-10-21 上游把它们用作 11128 渠道校验的 msg。
	// 保留在此是为了**不改变**"无业务码裸文案"的既有归类（ErrContentBlocked）；
	// 带 code 11128 的请求会先被 isChannelRefused 截走，不受影响。
	"unapproved channel",
	"illegal api invocation",
}

// contentModerationOnlyMarkers 只属于**内容审核**的文案（11128 的另一种变体）。
// isChannelRefused 用它做排除：11128 + 这些文案 ⇒ 内容问题，不是渠道校验。
var contentModerationOnlyMarkers = []string{"blocked by security policy"}

// contentBlockedClientMsg 内容拦截返回给调用方的固定文案。
// [关键词] 填分类词（色情 / nsfw / 暴力 等），绝不填业务 code、账号、冷却、upstream 前缀。
const contentBlockedClientMsg = "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。"

const contentBlockedFallbackKeyword = "违禁词"

// contentBlockedKeywords 审核分类词，按优先级扫描上游文案（大小写不敏感）。
// 只收录可直接展示给调用方的分类标签，不收录错误码（如 11128）。
var contentBlockedKeywords = []string{
	"色情", "porn", "nsfw", "adult",
	"暴力", "violence",
	"政治", "politics",
	"赌博", "gambling",
	"毒品", "drug",
	"违禁词",
}

// ContentBlockedClientMessage 把上游内容拦截改写成网关防火墙口径，不含账号/错误码。
func ContentBlockedClientMessage(body string) string {
	return fmt.Sprintf(contentBlockedClientMsg, contentBlockedKeyword(body))
}

// contentBlockedKeyword 从审核文案抽出分类关键词；抽不到则回「违禁词」。
func contentBlockedKeyword(body string) string {
	text := body
	var env struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && strings.TrimSpace(env.Msg) != "" {
		text = env.Msg
	}
	lower := strings.ToLower(text)
	for _, kw := range contentBlockedKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return contentBlockedFallbackKeyword
}

// badParamsMarkers 请求体/参数/上下文超限关键词：HTTP 400 + 上游参数或上下文超长报错。
// 涵盖：Unmarshal 失败（code 11101）、上下文超限（too many tokens、context length 等）、参数越界（11133 等）。
// 与账号健康无关——不罚号、不进软冷却，避免单次超限请求误伤全池。
var badParamsMarkers = []string{
	"unmarshal chat params failed",
	`"code":11101`,
	`"code": 11101`,
	`"code":11133`,
	`"code": 11133`,
	"too many tokens",
	"context length",
	"maximum context",
	"tokens exceed",
	"max_tokens must be",
	"integer_below_min_value",
	"exceeds model context",
}

// IsRequestParseFailure 报告上游 400 的 body 是否属于**请求体解析失败**这一类。
//
// 判据只用"解析失败"标志：这类错误与账号/模型/后端**无关**——同一段字符串在任何账号
// 上都是同样的解析结果，因此换号无用、应立即 400 不轮转（上游 fd5c5a6 的语义）。
//
// 与之相对，参数取值/上下文类标记（11133 model_param_invalid、too many tokens、
// context length、max_tokens must be、integer_below_min_value）是**模型/后端相关**的：
// 本仓自带证据——同一份 body 在 hy4 上通过、在 GPT 系 15 被拒 16 通过
// （见 clampGPTMinMaxTokens）。这些必须保留轮转，不能一律判成请求级错误。
func IsRequestParseFailure(body string) bool {
	lb := strings.ToLower(body)
	return strings.Contains(lb, "unmarshal chat params failed") ||
		strings.Contains(lb, `"code":11101`) || strings.Contains(lb, `"code": 11101`)
}

// ModelBlockReason 11102「该后端无此模型」负缓存条目的固定原因文案。
// **必须以 "11102" 开头**：pool.BlockModelClear 用该前缀区分 11102 条目与 6004 条目
// （6004 有自身上游重置墙钟，成功不该清它）。
const ModelBlockReason = "11102 model not found on this backend (negative cache)"

var badParamsMarkerMsg = "Unmarshal chat params failed"
var badParamsMarkerCode = `"code":11101`

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}

// accountFaultMarkers 账号级授权/配额故障关键词（大小写不敏感子串匹配）。
//
// 定位：这类错误是**账号本身状态**决定的本机故障，不是请求格式、不是临时限流、
// 也不是内容误报——继续重试只会反复刷上游风控/配额检查，必须把该账号冷却轮换。
//   - "request illegal"（code 11140）→ 上游 auth/auth_forbidden，账号级授权风控
//     （实测 global 账号：同一规范化请求 A1 403/11140 vs A2 429/14017，差异全由账号
//     数据决定）。需重新 OAuth 登录才能恢复，短冷却只能阻止继续送死。
//   - code 14017（"trial not activated" / "The trial version is not yet activated"）→
//     上游 quota/quota_not_activated，register 未完成的试用未激活账号，同样账号级。
//
// 注意 11140 **不能**按 code 判定：该 code 也承载模型级限流文案（"The model provider
// is rate-limiting requests."），那种场景必须保持 ErrSoftRate（上方 softRateMarkers
// 先命中）。故此处只收 msg 关键词 "request illegal"（auth_forbidden 的真实文案），
// 120 与 private 均落同一分类。14017 文案唯一（无软限流歧义），可安全收录。
// modelMissingMarkers 模型不存在 / 该后端无此模型 的文案族（小写比较 + 原文比较双通道）。
// 与 badParamsMarkers 分开：这族的责任在"模型名或模型在后端的可用性"，不在请求体本身；
// 且它必须在 **404 分支之前** 判定——实测未知模型既可能回 400+codepage，也可能回 404，
// 后者若落进 ErrNotFound 会被"短冷却防雪崩"白罚一个健康号，终态还是误导性的 503。
var modelMissingMarkers = []string{
	"no such model", "model not found", "model does not exist", "model doesn't exist",
	"unknown model", "invalid model name", "invalid model id", "unsupported model name",
	"no available channel",
	"该后端无此模型", "模型不存在", "不支持的模型",
}

var accountFaultMarkers = []string{
	"trial not activated",
	"trial version is not yet activated",
}

// accountRestrictedMsg 是 WorkBuddy 11140 账号限制的正文关键词。
//
// 背景（2026-10-20，上游 PR #233）：WorkBuddy 的 **11128 通道校验失败**与
// **11140 账号被限制**，msg 都是 "request illegal"，文案完全一致：
//   - 11128：`displayTips` 明确说「可能不是官方客户端发出」→ 账号本身健康；
//   - 11140：账号存在异常行为被限制 → 需要申诉。
//
// 旧实现按裸文案判定，会把一次通道校验失败误判成封禁并硬禁用健康账号。
const accountRestrictedMsg = "request illegal"

// IsAccountRestricted 报告 body 是否为账号级限制（11140 "request illegal"）。
// 必须 **code + 文案双条件**：
//   - 只看文案："request illegal" 也出现在 11128 通道校验失败里（健康账号）；
//   - 只看 code：11140 也承载模型级限流文案（"The model provider is rate-limiting
//     requests."），那种场景必须保持 ErrSoftRate。
func IsAccountRestricted(body string) bool {
	if !strings.Contains(strings.ToLower(body), accountRestrictedMsg) {
		return false
	}
	return hasBusinessCode(body, "11140")
}

// isChannelRefused 报告 body 是否为通道校验失败（11128 "request illegal"）。
// 11128 还承载内容审核变体（"blocked by security policy"，归 ErrContentBlocked），
// 故同样必须 code + 文案双条件，不能按 code 一刀切。
func isChannelRefused(body string) bool {
	if !hasBusinessCode(body, "11128") {
		return false
	}
	// 11128 同时承载**内容审核**变体（"blocked by security policy"）→ 那是内容问题，
	// 由 ErrContentBlocked 处理（不罚号、走降级重试）。
	// 注意排除清单必须用内容审核**独有**的文案：不能拿 contentBlockedMarkers 整表，
	// 否则 "unapproved channel"（新渠道校验文案）会把渠道校验自己排除掉。
	lower := strings.ToLower(body)
	for _, m := range contentModerationOnlyMarkers {
		if strings.Contains(lower, m) {
			return false
		}
	}
	// 渠道校验的文案族（上游 2026-10-21 起换了新文案，旧文案仍兼容）：
	//   - 旧："request illegal"
	//   - 新："Illegal API invocation from an unapproved channel"
	for _, m := range channelRefusedMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// channelRefusedMarkers 渠道校验失败（11128）的文案族，大小写不敏感。
// 上游 2026-10-21 把 msg 从 "request illegal" 改为
// "Illegal API invocation from an unapproved channel"（displayTips：这次调用没通过渠道校验）。
var channelRefusedMarkers = []string{
	"request illegal",
	"illegal api invocation",
	"unapproved channel",
	"channel verification",
}

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
// softRateResetLoc 上游"将在 … 重置"墙钟的时区（UTC+8 == CST）：统一到 internal/cstime。
var softRateResetLoc = cstime.Zone

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带「将在 … 重置」），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时间（上游 UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// invalidImageMarkers 图片格式/数据错误文案（确定性的请求级错误）。
var invalidImageMarkers = []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}

// codeMarker JSON code 字段命中（`"code":N` / `"code": N` / `"code":"N"` 形态）。lower 须为小写 body。
func codeMarker(lower, code string) bool {
	for _, v := range []string{`"code":` + code, `"code": ` + code, `"code":"` + code + `"`, `"code": "` + code + `"`, `"code":" ` + code + `"`, `"code": '` + code + `'`} {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}

// hasBusinessCode reports whether a JSON error envelope contains an exact
// business code in a field named "code". Upstream envelopes vary between
// top-level and nested error/data objects, so walk the decoded structure.
// transientModelBlockMarkers 路由/容量类"该模型暂时不可用"文案：常见上游语义是几分钟内
// 可能自愈，不该按"该后端没有这个模型"落 6h 起的负缓存并给不可重试 400。
var transientModelBlockMarkers = []string{
	"no available channel", "no available channels", "no channel",
	"temporarily unavailable", "服务繁忙", "暂无可用渠道", "临时不可用",
}

// modelLevelCodes 上游的**模型级**业务码：问题在「这个模型对这个账号/用户类型不可用」，
// 不在账号健康、也不在网关。
//
// 2026-10-20 对照 ardeyouxipianyi/workbuddy2api-hub（#499 / #505）：
//   - 11102 model not found on this backend（该后端无此模型）
//   - 11134 model provider temporarily unavailable（模型提供方暂不可用）
//   - 11150 model not available
//   - 11158 model not allowed for user type（**新**：免费/低档账号无权使用该模型）
//   - 11129 invalid function call parameters（模型相关的参数拒绝）
//
// 旧实现只认 11102（且只在 4xx 分支），于是 11158 这类码会落到 ErrServer ⇒ **喂熔断、罚整个账号**，
// 而实际上换模型就好、账号完全健康（用户可能连续被"熔断"掉一池号）。
var modelLevelCodes = []string{"11102", "11134", "11150", "11158", "11129"}

// hasModelLevelCode 报告 body 是否带模型级业务码。
func hasModelLevelCode(body string) bool {
	for _, c := range modelLevelCodes {
		if hasBusinessCode(body, c) {
			return true
		}
	}
	return false
}

// IsTransientModelBlock 报告一次 ErrModelBlocked 是否属于**路由/容量类**（可能自愈）。
// 显式的 11102 业务码优先（有码就是确定性"没有这个模型"），只有纯文案且命中路由族才算瞬时。
//
// 为什么不能反过来（只认 body 里含 "11102" 才落负缓存）：ErrModelBlocked 有三条生产者
// ——4xx+11102 码、**404+modelMissingMarkers 文案**、4xx+文案族，后两类 body 里没有
// "11102"；排除它们会让账号不被 modelCooled ⇒ 每次请求继续逐个烧号，且 >MaxRotate 的池
// 因 modelUnavailableFinal 恒 false 而对这些模型**永久 503**（审查 E 的 P1-2）。
func IsTransientModelBlock(body string) bool {
	if hasBusinessCode(body, "11102") {
		return false
	}
	lb := strings.ToLower(body)
	for _, m := range transientModelBlockMarkers {
		if strings.Contains(lb, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func hasBusinessCode(body, want string) bool {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if code, ok := node["code"]; ok && strings.TrimSpace(fmt.Sprint(code)) == want {
				return true
			}
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

// isPromptTooLongStatus 11115 只在请求级 4xx 状态码上判定（400/404/413）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. accountFaultMarkers —— 账号级授权/配额故障（11140 request illegal auth 风控、
//     14017 trial not activated register 未完成）。与 429 一起纳入轮换冷却，且必须
//     先于 softRate/status429 判定：14017 常带 429 状态码，若落到 status==429 兜底
//     会误归 soft_rate（"限流"语义不符：限流可指数退避等自愈，账号级故障等不来）。
//     11140 的 model 级限流变体（rate-limiting 文案）因 marker 不含该文案而天然
//     落到 softRateMarkers 层，不受影响。
//  4. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  5. 429 + code 14018 —— 明确的账号积分耗尽，归 ErrHardCredit（issue #175）。
//  6. status==429 —— body 无文案时的兜底识别。
//  7. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
//

func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	// WorkBuddy 11128 / 11140 的 msg 同为 "request illegal"，必须按业务码分流：
	//   - 11128 = 通道校验失败（displayTips：可能不是官方客户端发送）→ 账号健康，不罚号；
	//   - 11140 = 账号被上游限制（异常行为/风控）→ account_fault，由 handler 禁用。
	// 两者都要求 code + 文案双条件，避免 11140 的模型级限流变体被误判。
	if isChannelRefused(body) {
		return ErrChannelRefused
	}
	if IsAccountRestricted(body) {
		return ErrAccountFault
	}
	for _, m := range accountFaultMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrAccountFault
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	// 14018 是明确的账号积分耗尽业务码。它必须先于通用 429 兜底，否则会被误判为
	// 可自愈的软限流并在全池冷却时反复兜底选中（issue #175）。仅按结构化 code
	// 判定；无该 code 的 "credits exhausted" 文案仍保持普通 429 的软限流语义。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return ErrHardCredit
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	// 11115「prompt is too long」：请求级语义最具体（上下文超限），须先于宽泛的状态码兜底与参数错误。
	// 请求级错误（同一 body 换任何号都超限）：不罚号、不轮转，末端透传原文。
	if isPromptTooLongStatus(status) && (strings.Contains(lower, "prompt is too long") || strings.Contains(body, "11115")) {
		return ErrPromptTooLong
	}
	// 图片格式/数据错误是确定性的请求级错误：同 body 换账号结果不变，直接
	// fail-fast，避免把健康账号轮转一遍后仍把最终 503 返回给客户端。
	// 11135 业务码走 codeMarker（JSON 空白容差），文案走 invalidImageMarkers。
	if status == http.StatusBadRequest && codeMarker(lower, "11135") {
		return ErrImageInvalid
	}
	if status == http.StatusBadRequest {
		for _, m := range invalidImageMarkers {
			if strings.Contains(lower, m) {
				return ErrImageInvalid
			}
		}
	}
	// 11102「该后端无此模型」业务码：语义唯一（模型维度的不可用），与状态码无关，
	// 故最先判定。hasBusinessCode 是真 JSON 遍历，空格/引号形态无关。
	if status >= 400 && status < 500 && hasModelLevelCode(body) {
		// 模型级业务码（11102/11134/11150/11158/11129）：换模型或换号即可，账号本身健康，
		// 归 ErrModelBlocked ⇒ 落 (账号,模型) 负缓存，不熔断整池。
		return ErrModelBlocked
	}
	if status == http.StatusNotFound {
		// 未知模型的真实响应形态未定（可能 400 带码、也可能 404 带文案）：404 命中
		// "模型不存在"文案即判模型按不可用处理——否则落 ErrNotFound 会白冷却一个健康号
		// 60s，且终态仍是误导性的 503。
		if containsAnyMarker(lower, body, modelMissingMarkers) {
			return ErrModelBlocked
		}
		return ErrNotFound
	}
	if status >= 500 {
		// 模型级业务码即使在 5xx 也不罚账号：hub #499 实测 11134 以 HTTP 500 送达，
		// 旧口径会让"某个模型挂了"把整个账号熔断掉。
		if hasModelLevelCode(body) {
			return ErrModelBlocked
		}
		return ErrServer
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败 / 参数或上下文超限（HTTP 400 + badParamsMarkers）：
		// 这是"发给上游的 body 或参数有问题"（如 context length 超限、too many tokens、畸形 JSON）。
		// 与账号健康无关——绝不判 ErrSoftRate，不罚号（不冷却/不熔断/不计错）。
		for _, m := range badParamsMarkers {
			if strings.Contains(lower, m) || strings.Contains(body, m) {
				return ErrBadParams
			}
		}
		// 业务码兜底：11101（Unmarshal chat params failed）/ 11133（model_param_invalid）
		// 用 hasBusinessCode 做真 JSON 遍历，避免 codeMarker 那种"必须匹配空格形态"的脆弱匹配
		// （同一 11133 因 JSON 空格不同会落到不同分类，见审查 F6③）。
		if hasBusinessCode(body, "11101") || hasBusinessCode(body, "11133") {
			return ErrBadParams
		}
		// 文案级"模型不存在"**必须放在参数类判定之后**："invalid model parameters" 这类
		// 参数错误若先撞上模型族，会被误判成"换模型/换号"，而真实原因是请求参数。
		if containsAnyMarker(lower, body, modelMissingMarkers) {
			return ErrModelBlocked
		}
		// 403 + HTML 拦截页 = 上游 WAF（Cloudflare/APISIX 的 "WAF Block Page"）。
		// 必须早于 ErrClient：ErrClient 会直接透传给客户端且不冷却，实测表现为
		// "国际版时好时坏、偶发 403 client"。归 ErrWafBlock ⇒ 软冷却 + 抖动退避。
		if status == http.StatusForbidden && looksLikeHTMLBlockPage(body) {
			return ErrWafBlock
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// looksLikeHTMLBlockPage 判断响应体是否为 WAF/网关的 HTML 拦截页（非业务信封）。
// 只看开头一小段：拦截页体积可能很大，且业务 JSON 绝不会以 <!DOCTYPE/<html 开头。
func looksLikeHTMLBlockPage(body string) bool {
	head := strings.TrimSpace(body)
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(head)
	if strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html") {
		return true
	}
	for _, m := range []string{"waf block page", "web application firewall", "access denied", "request rejected"} {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// containsAnyMarker 文案族匹配：小写 body 与原文 body 双通道（中文 marker 不受 ToLower 影响，
// 但大小写敏感的英文 marker 需要原文比对，与既有 hardMarkers/softRateMarkers 同口径）。
func containsAnyMarker(lower, raw string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(raw, m) {
			return true
		}
	}
	return false
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// DialTimeout 出站 TCP 建连上限（0 = defaultDialTimeout）。main.go 按
	// config upstream.dial_timeout_seconds 注入；只作用于**新建连接**，
	// 不影响已建连后的流式读取（首字节/流中空闲各有自己的闸）。
	DialTimeout time.Duration

	// v3Cache 最近一次**两路 UA 都成功**的 /v3/config 目录：单路失败时与它取并集，
	// 避免"部分并集"被上层当成功写进 10 分钟缓存而让 /v1/models 缩水（见 fetchV3Models）。
	// 读写在 v3Mu 内（Client 以指针传递，无按值复制）。
	v3Mu    sync.Mutex
	v3Cache []ModelInfo

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 键按 realm 分层（map[realm]map[model][]efforts）：CN 探测结果不得被 global 同模型名
	// 请求复用（同名不同档位会错误降级，C-2）。global 侧暂无 efforts 探测 → 桶缺失即透传。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// modelRates 各模型当前生效积分倍率（规范化数值，如 "1.62"/"0.00"），按 realm 分层。
	// 供 pool 的积分保底做"目录倍率兜底"：本地还没实测到该模型的成本时，用目录价判断
	// 是否收费（上游 39af6b7）。与 efforts 共用 effortsMu（同一批目录解析后写入）。
	modelRates map[string]map[string]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认 WorkBuddy
	// 三段式与 billingUA 单段式）。空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing/checkin 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空，见 billingUA）。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 见 defaultWorkBuddyUA。默认值已对齐官方（A 段变更），
	// 用户仍可显式配置完全自定义的 UA。
	UserAgent string

	// DeviceToken 设备风控 Token（X-Device-Token 头）兜底来源：config upstream.device_token。
	// 仅当 auth.Auth.DeviceToken 为空时才取此值；两者皆空则不注入该头。
	// 容器内无桌面端 Turing SDK，这是把外部（宿主/桌面端）生成的 token 注入的入口。
	// 另见 DeviceTokenFile 缓存读取：宿主可把 token 落 /app/data/device_token 共用。
	DeviceToken string

	// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
	// 读取频率限 5 分钟一次缓存（见 device_token.go），>1KB 或读失败则忽略。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceTokenFile string

	// Identity 出站身份预设：""（默认，历史行为）/ "wb" / "vscode" / "cli"（见 identity.go）。
	// 上游 2026-10-21 收紧渠道校验（11128）后，身份头组不一致会被拦；
	// 该字段让"用 vsc 或 CLI 出站"成为可配置项，默认空 = 零回归。
	Identity string

	// PrivateData X-Private-Data（「数据私有」声明）取值：
	//   "" / "auto" = 按出站身份的官方默认（默认值）
	//   "true"      = 数据私有，不参与模型优化
	//   "false"     = 允许参与模型优化
	// 注意语义方向：true 更隐私，与变量名 enableModelOptimization 正相反。
	PrivateData string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	// 非空（如 "WorkBuddy"）则四头跟随，对齐官方桌面端 client 识别。
	ClientName string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + B 段的
	// X-IDE-Version）。空 = 内置默认 defaultClientVersion（对齐官方 5.5.4 分发包）。
	// config upstream.client_version 覆盖。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认 defaultCliVersion
	// （对齐官方内置 CLI 2.137.1）。config upstream.cli_version 覆盖。
	CliVersion string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 参数传入 ChatStream，
	// 由 ChatHeaders 注入（不再挂共享字段，杜绝并发串扰）。
	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// ToolCallMetaRepeat 兼容开关（config features.tool_call_meta_repeat，缺省 false）：
	//   false = 遵循 OpenAI 规范，tool_calls 的 id/type/function.name **只在首帧**出现；
	//   true  = 旧行为，每个续帧都重复带上元数据（为"状态机需要重复元数据"的客户端保留）。
	// 为什么要默认 false：重复元数据会让按"带 id 即新调用"解析的客户端（实测 Codex）
	// 把一次工具调用拆成几百个碎片（实测 327 帧、每帧 1~11 字符 → 完全无法发送代码）。
	ToolCallMetaRepeat bool

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 即显式逃生门：即使用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool
}

// New 生产默认值。配置连接池减少 TLS 握手。
// defaultDialTimeout 出站 TCP 建连默认上限（见 New 里的放宽说明）。
const defaultDialTimeout = 20 * time.Second

func New() *Client {
	// 2026-10 硬化（QA 发现：本机 DNS 把域名解析到 fake-IP 时 TCP 能连但 TLS 永不回包，
	// 旧 Transport 没有 TLSHandshakeTimeout/DialContext 超时 ⇒ 请求永久阻塞）。
	// 同时补 ProxyFromEnvironment：生产/测试环境常通过 HTTPS_PROXY 出网。
	// 【2026-10-21 放宽建连上限】原先硬编码 8s：生产实测「10 次探测 4 次 12s 都连不上、
	// 成功的也有 7.8s 才建连」的网络下，8s 会把本来能成功的请求提前判死，网关自己返回
	// 503 upstream_timeout（日志特征 `dial tcp <ip>:443: i/o timeout`），客户端只看到
	// "上游超时"却不知道其实是本地建连太急。
	// 默认放宽到 20s，并可通过 config upstream.dial_timeout_seconds 覆盖：
	//   - 该值只管**建连**（TCP handshake），不影响已建连后的流式读取
	//     （首字节由 HeaderTimeout/ResponseHeaderTimeout 管，流中空闲由 IdleTimeout 管）；
	//   - 放宽只在"网络半死"时多等一会儿，正常情况下建连是毫秒级，无额外成本。
	dialer := &net.Dialer{Timeout: defaultDialTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: dialer.DialContext,
		// TLS 握手上限：卡在握手上是最常见的"连得上不回包"形态。
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
		// 100-continue 等待上限，避免服务端不回 100 时干等（Go 默认 1s，显式声明便于审计）。
		ExpectContinueTimeout: 1 * time.Second,
	}
	c := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:     "https://www.workbuddy.cn",
		// GlobalEnabled 默认 **false**：global realm 路由必须由调用方显式开启 ——
		// cmd/server 按 config global.enabled 注入（启动 + 热重载两处），cmd/trial 因为
		// 是 global 专属端点也显式开启。默认 false 是**测试卫生**要求：若默认开启，
		// 测试里 `upstream.New()` + 池内 global 账号 + 任何模型清单调用就会真的打上游
		// （第七轮审计披露过一次这类意外出网）。
		GlobalEnabled: false,
	}
	c.SanitizeFingerprints.Store(true)
	return c
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c != nil && c.ChatBaseGlobal != "" {
		return strings.TrimRight(c.ChatBaseGlobal, "/")
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c != nil && c.BillingBaseGlobal != "" {
		return strings.TrimRight(c.BillingBaseGlobal, "/")
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 显式传 realm 使 effort 降级按域取桶：CN 探测信息不得作用到 global 请求（C-2）。
func (c *Client) prepareBody(body []byte, realm string) []byte {
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints.Load(), c.effortsSnapshot(realm))
}

// effortsSnapshot 返回指定 realm 的 effort 能力缓存副本；该域无探测 → nil（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测 → nil（调用方回落静态兜底表）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	out := make(map[string]string, len(bucket))
	for k, v := range bucket {
		out[k] = v
	}
	return out
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 base，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（R9：国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
//
// 传输层容错（2026-10-21 实测：www.workbuddy.ai 间歇性 TLS 握手卡住 17s；资产同步出现
// `unexpected end of JSON input` 即响应体被提前断开）：本函数的调用者全是**幂等** RPC
// （查资源/刷新 token/签到/上报），因此对 TLS 握手超时与响应截断各重试一次。
// 流式 chat 不走这里（见 ChatStream 的 doRetryTLS：只重握手，不重响应）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	const attempts = 2
	var lastErr error
	for i := 0; i < attempts; i++ {
		r := req
		if i > 0 {
			var cerr error
			r, cerr = cloneRequest(req)
			if cerr != nil {
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		data, retryable, err := c.doJSONOnce(r)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("request body is not replayable")
	}
	return nil, lastErr
}

// doJSONOnce 单次执行 doJSON 的"发送 + 读取 + 解析"，并报告该错误是否值得重试。
func (c *Client) doJSONOnce(req *http.Request) (json.RawMessage, bool, error) {
	resp, err := c.doRetryTLS(c.HTTP, req)
	if err != nil {
		// TLS 握手超时已在 doRetryTLS 内重试过一次仍失败 ⇒ 交给外层再试一次
		return nil, isTLSHandshakeErr(err), err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, false, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 响应体被提前断开 ⇒ JSON 截断，可重试；其余格式错误不可重试
		return nil, isTruncatedJSONErr(err), fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, false, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, false, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。采用两段式锁，缩短持锁窗口，网络 I/O 在锁外执行。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外）
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// 第 2 段（锁内）：校验快照一致后写回
	a.Lock()
	defer a.Unlock()
	// 并发刷新守卫：只要 accessToken **或** refreshToken 与我们进入网络 I/O 前的快照不同，
	// 就说明已经有别的 goroutine 写回过（更新的）一代凭证 —— 此时必须丢弃本次结果。
	//
	// 曾经写成 &&（两值都变才丢弃），但上游常常**不轮换 refreshToken**：并发场景下
	// 先完成的那个只会改 AccessToken，&& 判定为"没人动过"，后完成的就把旧一代 accessToken
	// 覆盖回去（该号随即 401 → 软冷却，且并发越高越容易复现）。
	if a.AccessToken != atBefore || a.RefreshToken != rtSnapshot {
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// chatPath 按 realm 返回 chat 端点路径（不含 base）：
// global → /console/chat/completions（404/405 时由 ChatStream fallback /v2/chat/completions）；
// cn → /v2/chat/completions（现状逐字，零回归）。
func (c *Client) chatPath(a *auth.Auth) string {
	if c.globalOn(a) {
		return globalChatConsolePath
	}
	return chatCompletionsPath
}

// 路径常量：CN 现状路径（chatCompletionsPath）与 global 双候选路径。
const (
	chatCompletionsPath   = "/v2/chat/completions"
	globalChatConsolePath = "/console/chat/completions"
)

// chatFallbackHTTPStatus global chat fallback 只在 404/405 时发生（R9：上游新旧路径分叉）。
func chatFallbackHTTPStatus(status int) bool { return status == 404 || status == 405 }

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// clientIP 为本次请求的客户端 IP（PassthroughIP=true 时注入；空串表示不透传）。
// 按**请求传递**而非读共享字段：避免并发请求交叉污染对方 IP（issue：ClientIP 竞态）。
// meta 为会话头族（issue #35）：同一次 user send 的换号/重试/降级复用同一
// conversationRequestID，后台按它聚合（handler 轮转循环外生成，循环内原样传）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
//
// global realm：先打 /console/chat/completions，404/405 时同一 base 二次换 /v2/chat/completions
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 发 chat 请求并返回原始 SSE body 流，支持通过 ctx 监听下游客户端生命周期。
// 当下游取消（如点击“停止生成”）时，立即中断向上游发出的 HTTP 请求并释放资源。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	// global 首次路径 404/405 时换 fallback 路径重试；ensureConsoleSystem 在 prepareBody 后统一套用
	// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128）。
	prepared := c.prepareBody(body, a.Realm())
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	for attempt, path := range c.chatPaths(a) {
		url := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		var reqCtx context.Context
		reqCtx, cancel = context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		// 仅 TLS 握手超时重试一次：握手未完成 ⇒ 请求字节未发出，不会重复生成。
		resp, err := c.doRetryTLS(c.chatHTTP(), req)
		if err != nil {
			cancel()
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return nil, 0, nil, ctx.Err()
			}
			log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
				logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
			// global 首次路径 404/405 → 换 fallback 路径重试；其余状态码直接返回。
			if attempt < len(c.chatPaths(a))-1 && chatFallbackHTTPStatus(resp.StatusCode) {
				continue
			}
			return nil, resp.StatusCode, raw, nil
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
		return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	if cancel != nil {
		cancel()
	}
	return nil, 0, nil, nil
}

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [console, /v2]（向 Fallback 迭代）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{globalChatConsolePath, chatCompletionsPath}
	}
	return []string{chatCompletionsPath}
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens + 上游模型对象全字段）。
// CN /console 与 global /v2 的模型对象同构，故共用此结构；上游省略的字段保持零值，
// /v1/models 侧按「空值省略」透出（不编造）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens（思考与最终回答共享此预算，上游无独立思考上限字段）
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	DefaultEffort string   // reasoning.defaultEffort（新模型键）或 reasoning.effort（老模型键）；空=未返回

	// 模型目录全字段（models-full-fields）：
	Description        string   // descriptionZh 中文描述
	Credits            string   // credits 积分倍率原文（如 "x0.05"）；透出展示 + 供积分保底判"是否收费"
	Tags               []string // tags 模型标签（含 badge:限时免费 等）
	Vendor             string   // vendor 厂商标识
	IsDefault          bool     // isDefault 是否默认模型
	SupportsReasoning  bool     // supportsReasoning 是否支持推理
	SupportsToolCall   bool     // supportsToolCall 是否支持工具调用
	OnlyReasoning      bool     // onlyReasoning 是否纯推理模型
	SupportsImages     bool     // 顶层 supportsImages（多模态能力，透出到 /v1/models）
	MaxAllowedSize     int64    // maxAllowedSize 最大允许上下文（与 maxInputTokens 口径并列，上游各自下发）
	CanDisableThinking bool     // reasoning.canDisableThinking：思考可关（off 档可用）
	ReasoningEffort    string   // reasoning.effort 推理模式（与 supportedEfforts 数组不同源）
	ReasoningSummary   string   // reasoning.summary 推理摘要模式（如 "auto"）

	// 优惠（modelPromotions，/v3/config data.modelPromotions）：Credits 是**牌价**
	//（转正后基准倍率），Promo* 是当前生效的限时优惠——面板据此显示「生效价 +
	// 标签 + 牌价」。PromoFactor 为 nil 表示无 machine-readable 折扣（如「错峰
	// 使用」只有时段文案无 factor），仅挂标签/提示。
	PromoFactor  *float64 // 折扣系数（0=限时免费，0.5=五折）；nil=无
	PromoCredits string   // 折扣后倍率原文（如 "0x" / "0.50x"），仅展示
	PromoLabel   string   // 徽章文案（限时免费 / 夜间折扣 / 错峰使用）
	PromoNote    string   // hover 说明原文（含时段/日期描述）
}

// dynModelEntry 上游模型目录（CN /console 与 global /v2 同构）的单条模型解析形态，
// FetchModels 与 global_models.go 的探测共用。iconUrl/descriptionEn/生成参数等
// 按「不透出」原则不解析。modelInfo() 是 dynEntry→ModelInfo 映射的单一事实来源，
// 杜绝两域映射漂移。
type dynModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModelID / Model id 的宽松回退键（仅 global 目录的多信封兜底用，CN 目录
	// 不下发这两个键；字段加在这里只是让 typed 解析能"看见"它们）。
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// modelInfo 按解析条目构造 ModelInfo（dynEntry→ModelInfo 映射的单一事实来源）。
// defaultEffort 新老双键兼容：defaultEffort 优先，缺省回落 effort。
func (m dynModelEntry) modelInfo() ModelInfo {
	def := m.Reasoning.DefaultEffort
	if def == "" {
		def = m.Reasoning.Effort
	}
	return ModelInfo{
		ID:                 m.ID,
		Name:               m.Name,
		ContextWindow:      m.MaxInputTokens,
		MaxTokens:          m.MaxOutputTokens,
		Efforts:            m.Reasoning.SupportedEfforts,
		DefaultEffort:      def,
		SupportsImages:     m.SupportsImages,
		Description:        m.Description,
		Credits:            m.Credits,
		Tags:               m.Tags,
		Vendor:             m.Vendor,
		IsDefault:          m.IsDefault,
		SupportsReasoning:  m.SupportsReason,
		SupportsToolCall:   m.SupportsTool,
		OnlyReasoning:      m.OnlyReasoning,
		MaxAllowedSize:     m.MaxAllowedSize,
		CanDisableThinking: m.Reasoning.CanDisableThinking,
		ReasoningEffort:    m.Reasoning.Effort,
		ReasoningSummary:   m.Reasoning.Summary,
	}
}

// nonChatModel 判定是否非对话模型（应从模型列表过滤掉）。
// 来源：harness buddy.ts:547-555。三类规则：
//   - id 前缀 nes-/completion-/codewise-：嵌入/补全/代码专用模型，选了报 code=11102。
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型。
//   - tags 含生成类标签（text-to-image / image-to-image / text-to-video / image-to-video，
//     比较前统一 ToLower+TrimSpace）：生成模型走各自专用端点，非本网关用途。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t0 := range tags {
		t := strings.ToLower(strings.TrimSpace(t0)) // 上游标签大小写/空白未证实统一
		// 生成类标签随上游扩充（上游 7b24e22）：早期只有 text-to-image，桌面端目录实测
		// 另有 text-to-video / image-to-video（seedance 系列）与 image-to-image
		// （gpt-image 系列，后者本就被 text-to-image 覆盖）。本函数 CN 与 global 共用。
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	// 名字级兜底：**仅在 maxOutputTokens 未知（0）时**启用。实测 hunyuan-image-alpha-edit
	// （图像编辑）既没有 text-to-image 标签、也缺 maxOutputTokens，于是溜进 /v1/models；
	// 真机实测该 id 调 /v1/chat/completions 被上游 400 拒绝，属"列出来但用不了"。
	// 限定在"没有 token 元数据"时判，避免误伤带 v/vision 的正常对话模型（如 glm-4.6v）。
	if maxOutputTokens == 0 && nonChatNameHint(id) {
		return true
	}
	// 已实测确认非对话的 id（即便给了 token 数也排除）。
	if nonChatKnownIDs[id] {
		return true
	}
	return false
}

// nonChatNameHint 名字级非对话线索（图像/向量/语音/视频类）。
func nonChatNameHint(id string) bool {
	for _, m := range [...]string{"image", "embed", "rerank", "text-to-speech", "tts-", "whisper", "-asr", "ocr", "video"} {
		if strings.Contains(id, m) {
			return true
		}
	}
	return false
}

// nonChatKnownIDs 实测确认"不是对话模型"的 id（调 /v1/chat/completions 被上游拒绝）。
// 2026-10-03 于测试实例 17964 实测：hunyuan-image-alpha-edit → 上游 400（本表只增不删；
// 若某条后来真能对话，删掉即可）。
var nonChatKnownIDs = map[string]bool{
	"hunyuan-image-alpha-edit": true,
}

// NonChatCatalogID 报告该 id 是否是"非对话模型"（按 nonChatModel 判据，无 token/tag 元数据）。
// 导出仅为跨包守卫：server 侧用它校验静态兜底表里没有图像/向量类条目（静态路径不经过
// fetchV3Models 的过滤，需要单独守）。
func NonChatCatalogID(id string) bool { return nonChatModel(id, 0, nil) }

// codeBuddyIDEUA /v3/config 要求能解析出 CodeBuddy 版本号的 UA。
// CLI 三段式 WorkBuddy UA 会拿到精简目录（flash 输出 128K、无 supportedEfforts）；
// 官方 IDE 头 `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 才返回完整能力
// （flash：393216 + low/high/max）。
// 版本号需随上游 IDE 发版跟进：UAn 版本过旧时该端点可能同样返回精简目录。
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// codeBuddyCLIUA CLI 三段式 UA。**实测（2026-09-22）该端点对不同 UA 下发的模型集合不同**：
//   - IDE UA  → 14 条（10 个 chat：含 o4-mini / enhance-1.0 / auto-chat，**无 deepseek 系列**）
//   - CLI UA  → 22 条（22 个 chat：**含 deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
//     gpt-6-astra / kimi-k2.8-preview**，但无 o4-mini / enhance-1.0 / auto-chat）
//
// 注意两点，都与旧注释相反，勿再按旧注释推断：
//  1. 旧注释称「CLI UA 拿到精简目录、IDE UA 才返回完整能力」——实测模型数量恰好相反，
//     但 **IDE 响应体积更大**（26003B vs 21111B），故「完整能力」应理解为**单条字段更全**，
//     而非模型更多。两路各有独有模型，缺一不可。
//  2. 2026-10-03 起 **CN 侧也走双 UA 并集**（见 fetchV3Models）：原先只走 IDE 单路，
//     导致 /v1/models 少列 deepseek 系列等 7 个在用模型（客户端配置按该清单生成）。
const codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// httpStatusError 携带上游 HTTP 状态码。
//
// 用途：让调用方能区分"重试有意义"（5xx/传输错误）与"重试只会放大问题"（4xx：鉴权失败、
// 限流、风控）。2026-10-03 之前 v3/config 的失败一律是字符串错误，重试逻辑只能盲重试 ——
// 401/429 时会把请求翻倍（审查 F3）。
type httpStatusError struct {
	Code int
	Msg  string
}

func (e *httpStatusError) Error() string { return e.Msg }

// isDeterministicHTTPErr 报告该错误是否属于"4xx 确定性拒绝"（不该重试）。
func isDeterministicHTTPErr(err error) bool {
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.Code >= 400 && se.Code < 500
	}
	return false
}

// safeProbe 在**探测 goroutine** 内兜住 panic：goroutine 里的 panic 不会被 net/http 的
// handler recover 兜住，会直接终止整个网关进程。探测失败只应降级为一条 error 结果
// （send 由调用方提供：各探测点的 channel 与结果类型不同）。
//
// 用法：go func(){ defer safeProbe("xxx probe", func(err error){ ch <- result{err: err} }) ; ... }()
func safeProbe(label string, send func(error)) {
	if p := recover(); p != nil {
		send(fmt.Errorf("%s panic: %v", label, p))
	}
}

// FetchModels 调上游动态模型接口（CN 侧；global 账号见 global_models.go 家族）。
//
// v3-config-merge：动态目录 = /v3/config（主，IDE UA 完整能力版）+ 企业端点
// （/console，cli 面过滤，补缺）的并集，两路**并发**探测。合并去重 key = 模型 id，
// v3 条目优先（credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。
// /v3 失败（400/网络错/解析失败）不拖累企业端点结果——降级为仅企业端点，warn 日志；
// 反之亦然（两路独立容错）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	type probeResult struct {
		infos []ModelInfo
		err   error
	}
	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)
	// 探测 goroutine 必须自带 recover（见 safeProbe）：否则 panic 会终止整个进程；
	// 探测出问题只应降级为一条 error，让另一路结果照常返回（两路独立容错）。
	go func() {
		defer safeProbe("enterprise models probe", func(err error) { enterpriseCh <- probeResult{err: err} })
		infos, err := c.fetchEnterpriseModels(a)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		defer safeProbe("v3/config models probe", func(err error) { v3Ch <- probeResult{err: err} })
		infos, err := c.fetchV3Models(a)
		v3Ch <- probeResult{infos, err}
	}()
	enterprise := <-enterpriseCh
	v3 := <-v3Ch
	if enterprise.err != nil && v3.err != nil {
		return nil, enterprise.err // 两路全失败：返回企业端点错误（既有调用方语义零漂移）
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}
	out := mergeModelInfos(v3.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 倍率快照：**必须在下面那个 effort 提前 return 之前**写 —— 常见目录是"有倍率无档位"，
	// 放在 return 之后会让保底拿不到目录价（上游踩过）。
	// complete=false（本次有路降级）时与旧快照取并集：否则"只在 /v3 出现"的模型倍率会凭空
	// 消失 → ModelRate 返回 "" → 积分保底把它当"未知"放行（审查 D 的 P2-6）。
	c.storeModelRates(a.Realm(), out, v3.err == nil && enterprise.err == nil)
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入桶）。
	// 空桶时跳过写：避免「某探测无档位数据」清掉既有桶。
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// 按探测账号的 realm 写入对应桶：CN 探测只进 cn 桶，global 同模型名不被污染（C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// mergeModelInfos 合并两路模型目录：primary 为主（同 id 以 primary 条目为准——
// credits 等字段以主端点为权威），secondary 只补 primary 缺失的 id。
// 去重 key = 模型 id；输出顺序 = primary 原序在前、secondary 补充项（secondary 原序）
// 在后——稳定输出，不依赖 map 迭代序。
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" {
			continue
		}
		if seen[mi.ID] {
			// 同 ID 冲突：primary 保留自身字段，**但缺失的能力字段用 secondary 补**。
			// 否则会出现"primary 的 [low] 覆盖 secondary 的 [low high max]"——档位是
			// storeEfforts 桶与请求体降级的依据，白丢一层保护（审查 F2）。
			for i := range out {
				if out[i].ID != mi.ID {
					continue
				}
				if len(out[i].Efforts) == 0 && len(mi.Efforts) > 0 {
					// 采用 secondary 的档位时，推理能力标志用 **OR 语义**（只增不减）：
					// "有档位"本身就蕴含支持推理，而 primary 的 true 也不该被 secondary 的
					// 缺省 false 覆盖（审查 F3）。
					out[i].Efforts = mi.Efforts
					out[i].SupportsReasoning = out[i].SupportsReasoning || mi.SupportsReasoning || len(mi.Efforts) > 0
					out[i].CanDisableThinking = out[i].CanDisableThinking || mi.CanDisableThinking
				}
				// default 必须落在**最终采用的档位集合**内，否则条目自相矛盾
				// （Efforts=[low] + DefaultEffort=high，面板会直接展示这组矛盾；审查 F4）。
				if mi.DefaultEffort != "" &&
					(out[i].DefaultEffort == "" || (len(out[i].Efforts) > 0 && !slices.Contains(out[i].Efforts, out[i].DefaultEffort))) {
					out[i].DefaultEffort = mi.DefaultEffort
				}
				if out[i].ContextWindow == 0 && mi.ContextWindow != 0 {
					out[i].ContextWindow = mi.ContextWindow
				}
				if out[i].MaxTokens == 0 && mi.MaxTokens != 0 {
					out[i].MaxTokens = mi.MaxTokens
				}
				break
			}
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// fetchEnterpriseModels 单路探测企业模型端点（/console/enterprises/personal/models）。
// 解析口径：agents[cli].models 过滤 + nonChatModel 剔除 + disabled 剔除。
func (c *Client) fetchEnterpriseModels(a *auth.Auth) ([]ModelInfo, error) {
	endpoint := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if m, ok := dynMap[id]; ok && !m.Disabled {
			out = append(out, m.modelInfo())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchV3Models **双 UA 并集**探测 /v3/config（IDE + CLI，见 codeBuddyCLIUA 注释）。
//
// 为什么必须两路（2026-10-03 修复）：该端点按 UA 下发**不同模型集合** —— IDE UA 的 10 个
// chat 模型里没有 deepseek 系列，CLI UA 的 22 个里没有 o4-mini/enhance-1.0/auto-chat。
// 此前 CN 侧只走 IDE 单路，于是 `/v1/models` 少列 7 个**在用**模型
// （deepseek-v4.1-flash、glm-5.3/glm-5.3-flash、hy4-preview(-f)、kimi-k2.6、kimi-k2.8-preview
// —— 与用户权威清单 19 个模型的交集只有 12 个），而 DSH/OpenCode/Cherry Studio 的配置
// 正是按这份清单生成的。global 侧早已双 UA 取并集（probeGlobalModels），此处补齐 CN 侧。
//
// 合并口径与 mergeModelInfos 一致：IDE 条目优先（单条字段更全：supportedEfforts/contextWindow），
// CLI 独有 ID 追加。
func (c *Client) fetchV3Models(a *auth.Auth) ([]ModelInfo, error) {
	type res struct {
		infos []ModelInfo
		err   error
	}
	probe := func(label, ua string) chan res {
		ch := make(chan res, 1)
		go func() {
			defer safeProbe("v3/config probe ("+label+")", func(err error) { ch <- res{err: err} })
			byID, err := c.fetchV3ConfigModelMap(a, ua)
			if err != nil && !isDeterministicHTTPErr(err) {
				// 单路瞬时抖动若直接降级，返回的"部分并集"会被上层当**成功**写进 10 分钟缓存
				// → 清单在此期间缩水。故对**可重试**错误（5xx/传输）先重试一次；
				// 4xx（401/403/429/风控）是确定性拒绝，重试只会放大限流，直接降级。
				time.Sleep(200 * time.Millisecond)
				byID, err = c.fetchV3ConfigModelMap(a, ua)
			}
			if err != nil {
				ch <- res{err: err}
				return
			}
			out := make([]ModelInfo, 0, len(byID))
			for _, mi := range byID {
				if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
					continue
				}
				out = append(out, mi)
			}
			ch <- res{infos: out}
		}()
		return ch
	}
	ideCh, cliCh := probe("ide", codeBuddyIDEUA), probe("cli", codeBuddyCLIUA)
	ide, cli := <-ideCh, <-cliCh
	degraded := ide.err != nil || cli.err != nil
	if ide.err != nil && cli.err != nil {
		return nil, ide.err // 两路全失败：保持既有错误语义（调用方会降级到静态表）
	}
	if ide.err != nil {
		log.Printf("WARN: [upstream] v3/config IDE UA 探测失败（本轮回退 CLI 单路，可能少列该 UA 独有模型）: %v", ide.err)
	}
	if cli.err != nil {
		log.Printf("WARN: [upstream] v3/config CLI UA 探测失败（本轮回退 IDE 单路，可能少列 deepseek 等模型）: %v", cli.err)
	}
	out := mergeModelInfos(ide.infos, cli.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	if degraded {
		// 有一路失败：与**上一份完整成功**的目录取并集，避免"部分并集"被上层当成功写进
		// 10 分钟缓存，从而在 TTL 内对外少列该 UA 独有模型（重试只救瞬时抖动，救不了
		// "上游持续拒绝某个 UA"）。等两路都成功时缓存自然收敛到最新。
		c.v3Mu.Lock()
		prev := c.v3Cache
		c.v3Mu.Unlock()
		out = mergeModelInfos(out, prev)
	} else {
		c.v3Mu.Lock()
		c.v3Cache = out
		c.v3Mu.Unlock()
	}
	return out, nil
}

type v3ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"` // "23:00"
			End   string `json:"end"`   // "7:50"
		} `json:"daily"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// promoZone 优惠/积分包到期时间口径：统一到 internal/cstime。
var promoZone = cstime.Zone

func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if sc := p.Schedule; sc != nil {
		if sc.ValidFrom != "" {
			from, err := time.Parse(time.RFC3339, sc.ValidFrom)
			if err == nil && now.Before(from) {
				return false
			}
		}
		if sc.ValidUntil != "" {
			until, err := time.Parse(time.RFC3339, sc.ValidUntil)
			if err == nil && !now.Before(until) {
				return false
			}
		}
		if len(sc.Daily) > 0 {
			cur := now.Hour()*60 + now.Minute()
			inWindow := false
			for _, w := range sc.Daily {
				st, ok1 := promoClock(w.Start)
				ed, ok2 := promoClock(w.End)
				if !ok1 || !ok2 {
					continue
				}
				if st <= ed {
					if cur >= st && cur < ed {
						inWindow = true
						break
					}
				} else if cur >= st || cur < ed {
					inWindow = true
					break
				}
			}
			if !inWindow {
				return false
			}
		}
	}
	return true
}

func applyModelPromotions(out map[string]ModelInfo, promos []v3ModelPromotion) {
	if len(promos) == 0 || len(out) == 0 {
		return
	}
	now := time.Now().In(promoZone)
	type cand struct {
		prio int
		p    *v3ModelPromotion
	}
	best := map[string]cand{}
	for i := range promos {
		p := &promos[i]
		if !promoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if _, ok := out[id]; !ok {
				continue
			}
			if b, seen := best[id]; !seen || p.Priority > b.prio {
				best[id] = cand{prio: p.Priority, p: p}
			}
		}
	}
	for id, c := range best {
		mi := out[id]
		if c.p.Badge != nil {
			mi.PromoLabel = c.p.Badge.Label
		}
		if c.p.Hover != nil {
			mi.PromoNote = c.p.Hover.TextZh
		}
		if c.p.Discount != nil {
			f := c.p.Discount.Factor
			mi.PromoFactor = &f
			mi.PromoCredits = c.p.Discount.DiscountedCredits
		}
		out[id] = mi
	}
}

// normalizeModelRate 把目录里的倍率原文规范成数值字符串（"x0.05"→"0.05"、"0.50x"→"0.5"、
// "1.62 credits"→"1.62"）。解析失败原样返回（不编造倍率，让上游判"未知"）。
var modelRateNumRe = regexp.MustCompile(`[-+]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][-+]?[0-9]+)?`)

// warnedModelRate 记录已告警过的倍率原文（每种只 WARN 一次，避免目录长尾刷屏）。
var warnedModelRate sync.Map

// normalizeModelRate 从倍率原文里**抽第一个数值 token** 并规范化（"x0.05"→"0.05"、
// "0.50x"→"0.5"、"1.62 credits"→"1.62"、**"x0.00 (限时免费)"→"0"**、**"×1.62"→"1.62"**）。
//
// 早期实现按位置剥前后缀再 ParseFloat：带注解/乘号（U+00D7）的形态一律解析失败 →
// ModelRate 返回原文 → pool 侧 ParseFloat 失败 → **静默放行**，等于让"目录判收费"的安全网
// 被格式绕过（审查 D 的 P2-4；仓库离线元数据里就有 `x0.00 (限时免费)` 与 `×0.00`）。
// 完全抽不出数值时原样返回（不编造倍率）并节流 WARN 一次。
func normalizeModelRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	tok := modelRateNumRe.FindString(s)
	if tok == "" {
		warnUnparsableModelRate(raw)
		return s
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		warnUnparsableModelRate(raw)
		return s
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// warnUnparsableModelRate 每种无法识别的原文只告警一次（暴露"安全网被格式绕过"）。
func warnUnparsableModelRate(raw string) {
	if _, loaded := warnedModelRate.LoadOrStore(raw, struct{}{}); !loaded {
		log.Printf("WARN: [upstream] unparsable model rate text %q（积分保底的目录兜底对它不生效）", raw)
	}
}

// effectiveModelRate 取当前**生效**倍率：有机器可读折扣（PromoFactor + PromoCredits）时用
// 折后价（限时免费 = "0"），否则用牌价 Credits。
func effectiveModelRate(mi ModelInfo) string {
	raw := mi.Credits
	if mi.PromoFactor != nil && strings.TrimSpace(mi.PromoCredits) != "" {
		raw = mi.PromoCredits // 有限时折扣时以折后价为准
	}
	r := normalizeModelRate(raw)
	if r == "" {
		return ""
	}
	if _, err := strconv.ParseFloat(r, 64); err != nil {
		// 抽不出数值（"动态选模"这类）：**不落表**（当未知 ⇒ 保底放行），
		// 否则这段原文会覆盖掉同一模型上一次可解析的价格，等于把安全网自己拆掉。
		return ""
	}
	return r
}

// storeModelRates 按 realm **整体替换**倍率快照：目录成功刷新但没有可解析倍率时写空桶，
// 防止旧倍率继续冒充当前价（上游 39af6b7 的语义）。
func (c *Client) storeModelRates(realm string, infos []ModelInfo, complete bool) {
	// 注：complete=true 且一个倍率都解析不出来时写空桶是**有意**的（目录确实没下发价，
	// 不能让旧价继续冒充当前价）；complete=false 时见下方"空桶不写"。
	rates := make(map[string]string, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		if rate := effectiveModelRate(mi); rate != "" {
			rates[mi.ID] = rate
		}
	}
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.modelRates == nil {
		c.modelRates = map[string]map[string]string{}
	}
	key := realmKey(realm)
	if !complete {
		// 降级目录：旧值保底（未知不等于免费）；一个倍率都没解析出来时**不写**，避免空桶冒充当前价。
		if len(rates) == 0 {
			return
		}
		for id, r := range c.modelRates[key] {
			if _, ok := rates[id]; !ok {
				rates[id] = r
			}
		}
	}
	c.modelRates[key] = rates
}

// ModelRate 返回 (realm, 模型) 当前生效积分倍率（"" = 未知）。供 pool 的积分保底目录兜底；
// pool 不依赖 upstream 包，由 main 注入闭包。
func (c *Client) ModelRate(realm, model string) string {
	if c == nil || model == "" {
		return ""
	}
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	return c.modelRates[realmKey(realm)][model]
}

func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

func (c *Client) fetchV3ConfigModelMap(a *auth.Auth, ua string) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	if ua == "" {
		ua = codeBuddyIDEUA
	}
	req.Header.Set("User-Agent", ua)
	c.injectCodeBuddyRequest(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Code: resp.StatusCode,
			Msg: fmt.Sprintf("v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))}
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models                []dynModelEntry `json:"models"`
			ProductFeaturesConfig struct {
				ModelTrialBanner struct {
					Banners []struct {
						ModelID       string `json:"modelId"`
						TargetModelID string `json:"targetModelId"`
					} `json:"banners"`
				} `json:"ModelTrialBanner"`
			} `json:"productFeaturesConfig"`
			ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code=%d", env.Code)
	}
	out := make(map[string]ModelInfo, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out[m.ID] = m.modelInfo()
	}
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := out[tgt]; ok {
				mi = base
				mi.ID = id
			}
		}
		mi.Credits = ""
		mi.Tags = nil
		out[id] = mi
	}
	applyModelPromotions(out, env.Data.ModelPromotions)

	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// billingMeterJSON 按 realm 候选路径发 billing/meter 域请求，ErrNotFound 时换下一候选路径
// （global：/billing/meter/* → /v2/billing/meter/*；cn：单路径 /v2/billing/meter/* 现状）。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	var lastErr error
	for i, p := range paths {
		data, err := c.billingJSON(a, method, p, body)
		if err != nil {
			lastErr = err
			var ue *Error
			if i < len(paths)-1 && errors.As(err, &ue) && ue.Kind == ErrNotFound {
				continue // /billing/meter/* 404 → 换 /v2/billing/meter/*
			}
			return nil, err
		}
		return data, nil
	}
	return nil, lastErr
}

// CreditPackage 单个积分包的构成明细（面板「积分构成」用）。
type CreditPackage struct {
	Name    string `json:"name"`
	Remain  int64  `json:"remain"`
	Used    int64  `json:"used"`
	Size    int64  `json:"size"`
	EndTime string `json:"end_time,omitempty"`
	// ExpiresAt 与 EndTime 同源的 Unix 毫秒时间戳，供面板按精确剩余天数聚合。
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	Cycle          bool   `json:"cycle,omitempty"`
}

// CreditPackages 返回账号当前的逐包构成。remain/size 为各包求和。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					ExpiredTime         string `json:"ExpiredTime"`
					PackageEndTime      string `json:"PackageEndTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					CreateTime          int64  `json:"CreateTime"`
					PackageCode         string `json:"PackageCode"`
					SubProductCode      string `json:"SubProductCode"`
					SubProductName      string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name:           p.PackageName,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		switch {
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		default:
			cp.EndTime = p.CycleEndTime
		}
		if cp.EndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, cp.EndTime, softRateResetLoc); perr == nil {
				cp.ExpiresAt = end.UnixMilli()
			}
		}
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		if p.CycleCapacitySize > 0 {
			cp.Cycle = true
			cp.Remain, cp.Size = p.CycleCapacityRemain, p.CycleCapacitySize
			cp.Used = cp.Size - cp.Remain
			if p.CycleCapacityUsed > cp.Used {
				cp.Used = p.CycleCapacityUsed
				cp.Remain = cp.Size - cp.Used
			}
			if cp.Remain < 0 {
				cp.Remain = 0
			}
		} else {
			cp.Remain, cp.Used, cp.Size = p.CapacityRemain, p.CapacityUsed, p.CapacitySize
			if cp.Used == 0 && cp.Size > cp.Remain {
				cp.Used = cp.Size - cp.Remain
			}
		}
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, sumRemain, sumSize, nil
}

// packageEndLayout 上游套餐到期时间的墙钟格式（UTC+8，与 softRateResetLoc 同口径）。
const packageEndLayout = "2006-01-02 15:04:05"

// UserResource 查询账号当前可花费积分余额与总额度（所有套餐聚合）。
func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// UserResourceDetailed 在 UserResource 基础上额外返回「快过期」积分子集：
// soon > 0 且套餐 CycleEndTime 解析成功且到期时刻 ≤ now+soon 的余额计入 expiring
// （pool 据此优先消耗，避免官方活动赠送的奖励积分到期作废）；soon ≤ 0 时 expiring
// 恒 0（禁用分桶，行为与引入前一致）。expiring 是 remain 的一部分。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// 余额查询同样做瞬时错误有界重试（签到后紧接着的 user-resource 偶发 500 会让
	// 该账号错过本次解冻/到期快照更新，只能等下一个刷新周期）。
	var data json.RawMessage
	err = c.retryBillingTransient(func() error {
		var e error
		data, e = c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
		return e
	})
	if err != nil {
		return 0, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, _, size := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if soon > 0 && r > 0 && acct.CycleEndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, acct.CycleEndTime, softRateResetLoc); perr == nil {
				if !end.After(now.Add(soon)) {
					expiring += r
				}
			}
		}
	}
	return remain, total, expiring, nil
}

// parseBillingTime 解析上游套餐日期（支持多种时间格式）。
func parseBillingTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006/01/02 15:04:05",
	}
	for _, l := range layouts {
		// 无时区标记的墙钟按 **CST** 解释（上游是中国区服务；布局里带偏移量时
		// ParseInLocation 会用字符串里的偏移，不受此影响）。旧实现用 time.Local：
		// 非 CST 宿主上到期时间会整体偏 8 小时（第十轮审计点名的最后一处 time.Local）。
		if t, err := time.ParseInLocation(l, s, cstime.Zone); err == nil {
			return t
		}
	}
	return time.Time{}
}

// PackageItem 单个生效中的积分套餐包明细（用于本地多包资产管道）。
type PackageItem struct {
	Remain int64     `json:"remain"`
	Expire time.Time `json:"expire"`
}

// ResourceDetail 账号积分与到期时间明细（Phase 1 资产数据感知 + Phase 3 临期层感知 + Direction 1 多包资产管道）。
type ResourceDetail struct {
	Remain          int64         `json:"remain"`
	Used            int64         `json:"used"`
	Size            int64         `json:"size"`
	Packages        int           `json:"packages"`
	EarliestExpire  time.Time     `json:"earliest_expire"`
	Urgent24hRemain int64         `json:"urgent_24h_remain"` // 兼容历史键，现代表 Tier 1 紧急临期量（48h）
	Urgent48hRemain int64         `json:"urgent_48h_remain,omitempty"`
	Urgent72hRemain int64         `json:"urgent_72h_remain"`
	PackageList     []PackageItem `json:"package_list,omitempty"`
}

// ResourceDetail 查询账号积分套餐及最早到期时间。
func (c *Client) ResourceDetail(a *auth.Auth) (*ResourceDetail, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64 `json:"TotalDosage"`
				Accounts    []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					PackageEndTime      string `json:"PackageEndTime"`
					ExpireTime          string `json:"ExpireTime"`
					EndTime             string `json:"EndTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					ExpiredTime         string `json:"ExpiredTime"`
					DeductionEndTime    int64  `json:"DeductionEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	var remain, used, size int64
	var earliestExpire time.Time
	var urgent24hRemain, urgent72hRemain int64
	var packageList []PackageItem
	for _, acct := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		remain += r
		used += u
		size += s

		// 仅对有剩余额度 (r > 0) 的套餐统计其到期时间与临期额度
		if r > 0 {
			var exp time.Time
			// 优先读取 CycleEndTime（上游标准套餐周期截止时间，如 "2026-09-21 01:48:24"）
			for _, expStr := range []string{
				acct.CycleEndTime,
				acct.ExpiredTime,
				acct.PackageEndTime,
				acct.ExpireTime,
				acct.EndTime,
			} {
				if expStr != "" {
					if t := parseBillingTime(expStr); !t.IsZero() {
						exp = t
						break
					}
				}
			}
			// 毫秒时间戳兜底（如 DeductionEndTime）
			if exp.IsZero() && acct.DeductionEndTime > 0 {
				exp = time.UnixMilli(acct.DeductionEndTime)
			}

			if exp.IsZero() || exp.After(now) {
				packageList = append(packageList, PackageItem{
					Remain: r,
					Expire: exp,
				})
			}

			if !exp.IsZero() && exp.After(now) {
				if earliestExpire.IsZero() || exp.Before(earliestExpire) {
					earliestExpire = exp
				}
				if exp.Before(now.Add(48 * time.Hour)) {
					urgent24hRemain += r
				}
				if exp.Before(now.Add(72 * time.Hour)) {
					urgent72hRemain += r
				}
			}
		}
	}

	// 排序 packageList: 按 Expire 升序排列（有过期时间的排在前面，永不过期的排在后面）
	sort.Slice(packageList, func(i, j int) bool {
		if packageList[i].Expire.IsZero() {
			return false
		}
		if packageList[j].Expire.IsZero() {
			return true
		}
		return packageList[i].Expire.Before(packageList[j].Expire)
	})

	// 合并相同到期时间的套餐
	var mergedPackages []PackageItem
	for _, p := range packageList {
		if len(mergedPackages) > 0 && mergedPackages[len(mergedPackages)-1].Expire.Equal(p.Expire) {
			mergedPackages[len(mergedPackages)-1].Remain += p.Remain
		} else {
			mergedPackages = append(mergedPackages, p)
		}
	}
	packageList = mergedPackages

	packs := len(resp.Response.Data.Accounts)
	// TotalDosage 作 size 下限（历史口径：已消耗的不该比总剂量小）。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return &ResourceDetail{
		Remain:          remain,
		Used:            used,
		Size:            size,
		Packages:        packs,
		EarliestExpire:  earliestExpire,
		Urgent24hRemain: urgent24hRemain,
		Urgent48hRemain: urgent24hRemain,
		Urgent72hRemain: urgent72hRemain,
		PackageList:     packageList,
	}, nil
}

// ResourceSummary 查询账号积分套餐的完整聚合口径（remain=剩余可花积分、used=已用、
// size=总量、packs=套餐数），供运维工具（cmd/credit）按 realm 展示真实余额。
// 与 UserResource 的差异：UserResource 只取 remain；本方法额外聚合 used/size/packs，
// 且 TotalDosage 作 size 下限（与 cmd/credit 历史口径一致，见其 packageRemainUsed）。
//
// realm 感知继承 billingMeterPaths：global 账号打 workbuddy.ai /billing/meter/*（404
// fallback /v2），CN 账号维持 /v2/billing/meter/get-user-resource（现状逐字，零回归）。
func (c *Client) ResourceSummary(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	d, err := c.ResourceDetail(a)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return d.Remain, d.Used, d.Size, d.Packages, nil
}

// respAccount 供 packageRemainUsed 解析的套餐字段（与 cmd/credit resourcePackage 同构）。
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// packageRemainUsed 聚合单套餐的 remain/used/size（历史口径见 cmd/credit/billing.go，
// 迁移至此作为单一事实来源）。Cycle 期套餐优先：用 CycleCapacity 三字段，
// used 取 CycleUsed 与 size-remain 的较大者；否则回退 Capacity 三字段。
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
}

// DailyChatGlobal 国际版每日活跃打卡对话（满足官方国际版每日对话赠送 30~50 积分规则）。
func (c *Client) DailyChatGlobal(a *auth.Auth) error {
	reqBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Hi"}],"max_tokens":10,"stream":true}`)
	rc, status, respBody, err := c.ChatStream(a, reqBody, "127.0.0.1", ChatMeta{})
	if err != nil {
		return err
	}
	if rc != nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d: %s", status, string(respBody))
	}
	return nil
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// truncate 按**rune 边界**截断到至多 n 字节（先 TrimSpace）。
// 上游错误体常含中文/emoji，直接 s[:n] 会把多字节字符切成半个：日志显示乱码，
// 且这些串会进 *Error.Msg 透传给客户端（encoding/json 会把非法字节替换成 U+FFFD）。
func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
