// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"strings"
	"sync/atomic"
	"time"
	"workbuddy2api/internal/cstime"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// cstZone 中国标准时间 UTC+8（自然日 00:00 刷新基准）。
// cstZone 上游自然日时区：统一指向 internal/cstime（此前各包各写一份 FixedZone，
// 是"日界口径分歧"的温床 —— 第七轮审计就是这么发现 3 处用宿主本地日的）。
var cstZone = cstime.Zone

// currentDayCST 返回 t 所属的中国标准时间自然日，格式 2006-01-02。
// 口径统一在 internal/cstime（本函数保留为包内薄封装，避免 pool 依赖 scheduler 侧实现）。
func currentDayCST(t time.Time) string {
	return cstime.Day(t)
}

// TokenUsage 账号聊天请求的累计 token 用量摘要（不包含任何原始凭证）。
type TokenUsage struct {
	RequestCount        int64     `json:"request_count,omitempty"`
	UsageCount          int64     `json:"usage_count,omitempty"`
	PromptTokens        int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens    int64     `json:"completion_tokens,omitempty"`
	TotalTokens         int64     `json:"total_tokens,omitempty"`
	LastLatencyMs       int64     `json:"last_latency_ms,omitempty"`
	LastTokensPerSecond *float64  `json:"last_tokens_per_second,omitempty"`
	LastUsedAt          time.Time `json:"last_used_at,omitempty"`
	LastModel           string    `json:"last_model,omitempty"`
}

// TokenUsageDelta 是一次聊天账号尝试的 usage 增量。
// 各 Has* 字段用于区分上游缺少字段与字段值确实为 0。
type TokenUsageDelta struct {
	Model               string
	HasPromptTokens     bool
	PromptTokens        int64
	HasCompletionTokens bool
	CompletionTokens    int64
	HasTotalTokens      bool
	TotalTokens         int64
	HasCachedTokens     bool
	CachedTokens        int64
	HasLatencyMs        bool
	LatencyMs           int64
	HasTokensPerSecond  bool
	TokensPerSecond     float64
	HasCredit           bool
	Credit              float64
}

// PackageItem 单个积分套餐包（用于多包资产管道与本地滑动窗口）。
type PackageItem struct {
	Remain int64     `json:"remain"`
	Expire time.Time `json:"expire"`
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID              string        `json:"uid"`
	EnterpriseID     string        `json:"enterprise_id,omitempty"` // 企业版账号（auth 带 enterpriseId）；空 = 个人号
	Realm            string        `json:"realm,omitempty"`
	Nickname         string        `json:"nickname,omitempty"`
	Credits          int64         `json:"credits"`
	CreditsTotal     int64         `json:"credits_total,omitempty"`
	CreditsUsed      float64       `json:"credits_used,omitempty"`
	DailyCreditsUsed float64       `json:"daily_credits_used"`
	DailyTokensUsed  int64         `json:"daily_tokens_used,omitempty"`
	DailyDate        string        `json:"daily_date,omitempty"`
	TotalTokens      int64         `json:"total_tokens,omitempty"`
	TokenUsage       TokenUsage    `json:"token_usage,omitempty"`
	EarliestExpire   time.Time     `json:"earliest_expire,omitempty"`
	DaysUntilExpire  float64       `json:"days_until_expire,omitempty"`
	Urgent24hRemain  int64         `json:"urgent_24h_remain,omitempty"`
	Urgent48hRemain  int64         `json:"urgent_48h_remain,omitempty"`
	Urgent72hRemain  int64         `json:"urgent_72h_remain,omitempty"`
	Packages         []PackageItem `json:"packages,omitempty"`
	LastResourceSync time.Time     `json:"last_resource_sync,omitempty"`
	Cooling          bool          `json:"cooling"`
	CoolKind         string        `json:"cool_kind,omitempty"`
	CoolRemaining    int64         `json:"cool_remaining_sec,omitempty"`
	Until            time.Time     `json:"until,omitempty"`
	Reason           string        `json:"reason,omitempty"`
	SoftStreak       int           `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	// RateLimitedModels 当前仍在限额的模型列表（issue #36 限额台账）。
	// 仅「带解析时间 6004」触发的模型级独立冷却（modelCooldowns 未到期条目）时非空，
	// 每模型一行；运维据此看到"账号 A 的模型 X 还在限额中，预计 Z 时间恢复"。到期即消失（零回归）。
	RateLimitedModels []RateLimitedModel `json:"rate_limited_models,omitempty"`
	Disabled          bool               `json:"disabled"`
	// DisabledReason 仅 disabled 账号：禁用原因（运维可见）。
	DisabledReason string `json:"disabled_reason,omitempty"`
	// Paused 暂停选号（上游 fd835df 移植）：退出选号候选，但**照常**参与签到 / 活跃上报 /
	// 保活 / 余额刷新四类保号任务（以及旅行、成长任务；唯一跳过夜猫子真实对话）。
	// 与 disabled 正交：disabled 是更强终态（禁用时顺带清 paused）。
	Paused          bool      `json:"paused,omitempty"`
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`

	// 本地改造接轨字段（运行态，展示调度与积分状态）
	Pinned             bool    `json:"pinned"`                         // 是否被强制指定为路由账户（重启后重置）
	IsActiveChunk      bool    `json:"is_active_chunk"`                // 是否为当前主刷分块账号
	ActiveChunkReqs    int     `json:"active_chunk_reqs,omitempty"`    // 活跃分块已跑轮数
	ActiveChunkCredits float64 `json:"active_chunk_credits,omitempty"` // 活跃分块已消耗积分
	Tier               string  `json:"tier,omitempty"`                 // 调度梯队说明
	InBreathing        bool    `json:"in_breathing,omitempty"`         // 是否处于柔性透气让渡中
	// 最近一次签到结果（运行态，不持久化；面板卡片展示"今日是否签到"）。
	LastCheckinAt     time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinStatus string    `json:"last_checkin_status,omitempty"`
	// DailyMarks 每账号的"某类每日动作已在哪个 CST 自然日完成"标记（**持久化**，
	// stateAccount.DailyMarks）。用于给排程的每日动作加客户端幂等：补签卡、连登兑换/补偿、
	// 国际版每日活跃等 —— 这些动作会**消耗消耗品或真实额度**，而此前只有内存去重（甚至没有），
	// 重启/手动与定时撞车/崩溃重放都会重复执行（第十轮审计 F2/F8/F9）。
	DailyMarks map[string]string `json:"daily_marks,omitempty"`
}

// IsEnterprise 判定"企业版账号"。规则（§T.34.15）：**只认非空字符串**，且空白-only 视为非企业号。
// 为什么单独抽函数：企业号会被用来跳过个人成长类任务（S5 第 2 步），一旦把"空/空白/异形"
// 误判为企业号，就会**静默停掉**该号成长体系（只表现为"这个号不涨了"、无任何报错）。
func (s Status) IsEnterprise() bool { return strings.TrimSpace(s.EnterpriseID) != "" }

// RateLimitedModel 单个被限流模型的台账行（issue #36）。
type RateLimitedModel struct {
	Model string `json:"model"`
	// Kind 区分限流与模型不可用：6004 是 rate_limit，11102 是 model_unavailable。
	Kind string `json:"kind"`
	// Until 冷却到期时刻 = 该模型的独立冷却截止（modelCooldowns[m].Until，截断后），
	// 多模型限流时不再等于 Status.Until（账号级）。
	Until time.Time `json:"until,omitempty"`
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断，跟 softRateReset）；
	// 截断后 Until==ResetAt，省略 ResetAt 让台账自然减少一列。
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string `json:"reason,omitempty"`
}

type entry struct {
	a                 *auth.Auth
	credits           int64
	creditsTotal      int64         // 积分总额度（UserResource 聚合；0 = 未知）
	creditsExpiring   int64         // 快到期积分量
	creditsUsed       float64       // 累计消耗官方积分
	dailyCreditsUsed  float64       // 今日消耗官方积分（0点刷新）
	dailyTokensUsed   int64         // 今日消耗 token 数（0点刷新）
	dailyDate         string        // 今日用量所属自然日（CST 格式 2006-01-02）
	totalTokens       int64         // 累计消耗 token 总数
	earliestExpire    time.Time     // 最近有余量套餐的过期时间
	urgent24hRemain   int64         // 24小时内即将过期的积分量
	urgent72hRemain   int64         // 72小时内即将过期的积分量
	packages          []PackageItem // 多包资产管道（Phase 3 引入，按 Expire 升序排列）
	lastResourceSync  time.Time     // 最近一次与上游同步资源的时间
	lastCheckinAt     time.Time     // 最近一次签到时刻（运行态，不持久化）
	lastCheckinStatus string        // 最近一次签到结果（ok/already/fail/skipped，运行态）
	successCount      int64         // 累计成功
	errTotal          int64         // 累计错误（供成功率权重，不清零）；权重用 Laplace 平滑 (ok+1)/(ok+err+2)×3，见 successWeightTerm
	lastErr           time.Time     // 最近一次错误时间
	lastSuccess       time.Time     // 最近一次成功时间
	tokenUsage        TokenUsage    // 聊天请求 token 用量摘要（持久化）
	coolKind          CoolKind
	until             time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled          bool
	paused            bool // 暂停选号：落盘持久化（state.json），跨重启不丢
	reason            string
	lastUsed          time.Time // 最近被选中时刻（防并发撞号）
	// breakerUntil / fails / retryCount 为熔断器运行态（不持久化）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 重置点只有两处（都是账号被证明恢复的时刻）：NoteSuccess、reviveCoolingLocked。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// dailyMarks 每账号的"某类每日动作已在哪个 CST 自然日完成"（**持久化**：
	// stateAccount.DailyMarks）。给会消耗消耗品/真实额度的排程动作加客户端幂等，
	// 见 Pool.NoteDailyMark / DailyMarkDoneToday（第十轮审计 F2/F8/F9）。
	dailyMarks map[string]string
	// modelCooldowns 6004 模型级 limit 的**独立**冷却表：model → 该模型的冷却截止/重置。
	// 与 until（全账号级）正交：6004 只写本表、不写 until，因此多个模型同时 6004 时
	// 各自独立计时，互不覆盖（A 触发后 B 再触发，A 的冷却截止不被 B 覆盖——这是
	// 单 until 字段做不到的）。only 6004 触发时记录；空 map = 无模型级限流（不豁免）。
	// 运行态语义（不持久化）：重启清零，退化为仅账号级 until 冷却的现状。
	modelCooldowns map[string]modelCooldown
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 运行态语义（不持久化，与 inFlight 同语义）：重启清零可接受——重启后首个 keepalive
	// 成功即清计数，误判号不会因重启前的历史累积被继续追杀。
	sessionDeadFails int
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// modelCost 实测扣费账本：model → 观测（运行态，不持久化）。
	// 由每次成功请求的 usage.credit 折算而来（上游没有"按模型的用量"接口，
	// get-user-resource 只给套餐级积分汇总，只能实测）。选号时据此把
	// 「该模型上免费/便宜的号」排在前面。
	modelCost map[string]modelCostEntry
}

// modelCostOf 返回该账号在指定 model 上的有效成本观测；无观测或观测过期返回 ok=false。
func (e *entry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if model == "" {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok || mc.LastSeen.IsZero() {
		return modelCostEntry{}, false
	}
	if now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false // 过期：时段性优惠（夜间免费）不得跨时段生效
	}
	return mc, true
}

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断期）。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled || e.paused {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// modelExempt 报告账号是否处于「6004 模型级软冷却」形态：存在任一有效的 6004
// 模型级冷却（modelCooldowns 非空），且尚未禁用、未熔断。
// 此形态下账号仅对限流中的模型不可用，对其他模型仍可选（issue #31）。
// 本谓词仅供探活侧使用（ServableNow/ServableForRealm）：/healthz 无请求模型
// 上下文，用「存在豁免形态」表达"该账号还有别的模型可服务"；
// chat 侧按请求模型细粒度判定（healthyForModel：全账号健康且该模型不在独立
// 冷却内才放行），探活存在性语义与选号在豁免账号上口径一致。
// 调用方负责 now 与冷却有效性的判断（本方法只看形态，不看冷却是否已过期）。
func (e *entry) modelExempt() bool {
	if e.disabled || e.paused || !e.breakerUntil.IsZero() {
		return false
	}
	for _, mc := range e.modelCooldowns {
		if !mc.AuditOnly && !mc.Until.IsZero() {
			return true
		}
	}
	return false
}

// modelCooled 报告账号对指定 model 是否正处 6004 模型级冷却（该模型的独立冷却未过期）。
// 空 reqModel / 未记录 → false（不因模型级维度限制账号）。
func (e *entry) modelCooled(now time.Time, reqModel string) bool {
	if reqModel == "" {
		return false
	}
	mc, ok := e.modelCooldowns[reqModel]
	if !ok || mc.AuditOnly {
		return false
	}
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// healthyForModel 报告账号对指定 model 是否可选（含 6004 模型级独立冷却判定）。
//
// 优先级（全账号级先判，模型级后判）：
//   - 全账号不可用（disabled / 账号级 until / breakerUntil，见 healthy）→ 永不可选；
//     账号整体不可用时查该模型的独立冷却没有意义，直接短路返回 false。
//   - 仅全账号健康时，才查该模型是否正处 6004 独立冷却
//     （modelCooldowns[reqModel] 未过期）→ 不可选；
//   - 否则可选。
//
// 模型级维度只锁定触发模型：多模型同时 6004 时各自独立，被 B 限流的账号对 A 请求
// 仍可选（A 不在 modelCooldowns 拦截且账号级 healthy 成立）。空 reqModel /
// 未记录模型 → 等价 healthy。6004 从不写账号级 until（见 CooldownSoftForModel），
// 因此不存在「账号级冷却因病 6004 而起、应豁免其他模型」的形态。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) { // 全账号级（disabled/until/breakerUntil）先判
		return false
	}
	if e.modelCooled(now, reqModel) { // 全账号健康时再查该模型的 6004 独立冷却
		return false
	}
	return true
}

// pruneExpiredModelCooldowns 删除 modelCooldowns 中已过期的条目（惰性清理）。
// pick 写锁路径与 revive 调用，防止 map 无限膨胀；status 只读遍历天然跳过过期项，
// 无需清理。调用方必须已持有 p.mu 写锁。
func (e *entry) pruneExpiredModelCooldowns(now time.Time) {
	if len(e.modelCooldowns) == 0 {
		return
	}
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCooldowns, m)
		}
	}
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（两个截止取较早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits          int64         `json:"credits"`
	CreditsTotal     int64         `json:"credits_total,omitempty"`
	CreditsUsed      float64       `json:"credits_used,omitempty"`
	DailyCreditsUsed float64       `json:"daily_credits_used,omitempty"`
	DailyTokensUsed  int64         `json:"daily_tokens_used,omitempty"`
	DailyDate        string        `json:"daily_date,omitempty"`
	TotalTokens      int64         `json:"total_tokens,omitempty"`
	TokenUsage       TokenUsage    `json:"token_usage,omitempty"`
	EarliestExpire   time.Time     `json:"earliest_expire,omitempty"`
	Urgent24hRemain  int64         `json:"urgent_24h_remain,omitempty"`
	Urgent48hRemain  int64         `json:"urgent_48h_remain,omitempty"`
	Urgent72hRemain  int64         `json:"urgent_72h_remain,omitempty"`
	Packages         []PackageItem `json:"packages,omitempty"`
	LastResourceSync time.Time     `json:"last_resource_sync,omitempty"`
	Disabled         bool          `json:"disabled"`
	Paused           bool          `json:"paused,omitempty"`
	Reason           string        `json:"reason,omitempty"`
	Until            time.Time     `json:"until,omitempty"`
	CoolKind         CoolKind      `json:"cool_kind"`
	SuccessCount     int64         `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak,omitempty"`
	// DailyMarks 见 entry.DailyMarks：键是动作名（makeup/streak_claim/daily_chat…），值是 CST 日期。
	DailyMarks map[string]string `json:"daily_marks,omitempty"`

	// ModelCost 实测扣费账本（model → 观测）。仅内存态，重启后重新学习：
	// 成本会随上游活动（限免期/夜间免费/折扣）变化，持久化旧值反而是脏数据。
	ModelCost map[string]stateModelCost `json:"-"`
}

// stateModelCost 单个 (账号, 模型) 的实测成本观测。
type stateModelCost struct {
	// CostPer1k 每千 token 的 credit 消耗（EMA 平滑）。0 = 免费。
	// 用"每千 token"归一而非"单次 credit"：扣费随请求长度变化，
	// 不同长度的请求之间不可比。
	CostPer1k float64
	// LastSeen 最近观测时刻，超过 modelCostTTL 视为失效。
	LastSeen time.Time
	// Samples 观测次数（供排查）。
	Samples int
}

// modelCostTTL 成本观测的有效期。取 6 小时：既覆盖"夜间免费"这类时段性优惠的
// 单次会话，又不至于让昨天的价格决定今天的选择——过期的免费观测若永久有效，
// 白天会把已开始收费的号继续当成免费。
const modelCostTTL = 6 * time.Hour

// modelCostEntry 运行时成本账本（与 stateModelCost 同构，独立于持久化结构，
// 避免账本污染 state.json）。
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// modelCooldown 单个 (账号, 模型) 的 6004 独立冷却记录（运行态，不持久化）。
type modelCooldown struct {
	// Until 该模型的冷却截止（= now+min(resetAt-now, soft_rate_max)，截断后）。
	Until time.Time
	// ResetAt 上游「将在 … 重置」的原始墙钟（未经 soft_rate_max 截断）。
	// 与 Until 的区别同旧 softRateReset：Until 可能截断，ResetAt 是上游权威恢复时刻。
	ResetAt time.Time
	// Reason 触发原因（透出运维可读文案，同 Status.Reason）。
	Reason string
	// AuditOnly 为 true 时仅用于状态展示（例如无重置时间的 6004），
	// healthyForModel 与 modelExempt 必须忽略它，避免改变选号行为。
	AuditOnly bool
	// Hits 11102 负缓存的累计命中次数（驱动指数退避 6h→24h）。6004 条目 Hits 恒 0。
	Hits int
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
