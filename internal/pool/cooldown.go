// 冷却与熔断：Cooldown/CooldownSoftForModel（软退避指数升级）、软冷却封顶、
// 熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"log"
	"sort"
	"strings"
	"time"
	"workbuddy2api/internal/cstime"
)

func (p *Pool) SetCredits(uid string, credits int64, total ...int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		if len(total) > 0 {
			e.creditsTotal = total[0]
		}
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
// 冷却入口同时是熔断器的失败信号：喂入 fails，达到阈值按指数退避熔断（与 until 正交）。
//
// CoolSoft 额外做**连续软限流指数退避**：同一账号连续触发软冷却时，实际时长按
// d << (softStreak-1) 逐次翻倍（封顶 softRateMax，见 softDurationLocked）。
// 首 streak=1 → 实际时长 = d，单次调用语义与旧行为一致。
// 这就是与熔断器并存的双重升级，且是**有意为之**：软退避管"近期被限流"，
// 在 600s 起按分钟~小时级放大；熔断管"病态反复失败"，按 30m→1h→2h→6h 长期封禁。
// 二者喂入路径共用本入口但计数器独立（softStreak vs fails），互不污染。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if kind == CoolSoft {
			e.softStreak++
			d = p.softDurationLocked(d, e.softStreak)
		}
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空会参与路由的模型级冷却，避免上一次
		// 模型豁免泄漏到账号级冷却上；AuditOnly 条目不影响路由，保留展示。
		clearRoutingModelCooldownsLocked(e)
		p.invalidateChunkLocked(uid)
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）。
// 区别于 Cooldown：当上游 6004 明说「将在 … 重置」时，把 **该模型** 的冷却截止精确
// 设为 resetAt（上游给定时间，不再靠固定基数+指数退避猜测），记录到独立的
// modelCooldowns[model]——每模型独立计时，多个模型同时 6004 互不覆盖（这是单 until
// 字段做不到的）。不写 until（全账号级冷却不受 6004 污染）。
//
// 收窄规则（与旧实现一致）：
//   - resetAt 非零（6004 带解析时间）→ modelCooldowns[model].Until = min(resetAt,
//     now+softRateMax)，ResetAt 记录上游原始墙钟（台账 ResetAt）。指数退避
//     **不适用**：重置时间已是上游权威，再指数放大会无视它明说的恢复时刻。
//   - resetAt 零值（6004 无时间文案 / 非 6004 的 soft）→ 完全退回 Cooldown 现状
//     （soft_streak 指数退避 + 封顶 soft_rate_max，写 until），不记录模型（不豁免）。
//
// 熔断信号照旧喂入（冷却与熔断正交，行为与 Cooldown 一致）；softStreak 仍递增
// （无论是否命中解析时间）——解析时间的冷却**不**参与指数退避，但 softStreak 计数
// 照常累加，后续无时间的 6004 从当前 streak 继续退避（与任务书口径一致）。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.softStreak++
		hasReset := !resetAt.IsZero()
		// now 只取一次：d 与 modelCooldowns[].Until 必须基于同一时刻推算，
		// 否则 Until = now₂+(resetAt-now₁) 比 ResetAt 恒略大（漂移量 = 两次取时之差），
		// 导致"未截断时 Until 应严格等于 ResetAt"不成立。
		now := time.Now()
		d := p.softDurationLocked(base, e.softStreak)
		if hasReset {
			// 有上游重置时间：直接取 min(重置墙钟, now+softRateMax)，不做指数放大。
			cap := now.Add(p.softRateMaxOr())
			if resetAt.After(cap) {
				d = cap.Sub(now)
			} else if resetAt.After(now) {
				d = resetAt.Sub(now)
			} else {
				// 重置时间已过（时钟偏移/文案过期）：冷却极短，立即恢复。
				d = time.Millisecond
			}
		}
		e.coolKind = CoolSoft
		e.reason = reason
		if hasReset {
			// 6004 带解析时间：写该模型的独立冷却表（不写 until）。until 截断到封顶，
			// ResetAt 保留上游原始墙钟（台账 ResetAt 呈现真实恢复时刻）。
			// 未截断时 Until 与 ResetAt 严格相等（同一 now + 同一 d 推算）。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:   now.Add(d),
				ResetAt: resetAt,
				Reason:  reason,
			}
		} else {
			// 无解析时间（普通软冷却/非 6004）：退回账号级 until 冷却，且清路由豁免。
			e.until = time.Now().Add(d)
			clearRoutingModelCooldownsLocked(e)
			p.invalidateChunkLocked(uid)
		}
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// RecordModelRateLimitAudit 记录无法参与模型路由的 6004 展示项。
// 典型场景是 6004 没有可解析重置时间：账号仍按原有有界退避冷却，
// 本方法只把模型名挂到 e.until 上供账号页展示，不影响 healthyForModel。
func (p *Pool) RecordModelRateLimitAudit(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	if old, exists := e.modelCooldowns[model]; exists && !old.AuditOnly && old.Until.After(now) {
		return // 已有真实模型冷却，审计记录不得覆盖路由截止
	}
	until := e.until
	if until.IsZero() || !until.After(now) {
		until = now.Add(p.softRateMaxOr())
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = make(map[string]modelCooldown)
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:     until,
		Reason:    reason,
		AuditOnly: true,
	}
	p.dirty.Store(true)
}

// clearRoutingModelCooldownsLocked 删除参与选号豁免的模型冷却，保留 AuditOnly 台账。
// 调用方必须已持有 p.mu 写锁。
func clearRoutingModelCooldownsLocked(e *entry) {
	for model, mc := range e.modelCooldowns {
		if mc.AuditOnly {
			continue // 台账：只审计不拦截
		}
		if strings.HasPrefix(strings.TrimSpace(mc.Reason), "11102") {
			// 11102 是**模型级负缓存**（"该后端没有这个模型"），与"模型豁免"是相反语义：
			// 账号在**别的**模型上遇一次 429/404 不该把它的 6h/24h 负缓存一起清掉
			// （审查 D 的 P2-5）。只有成功（BlockModelClear）或 TTL 到期才该失效。
			continue
		}
		delete(e.modelCooldowns, model)
	}
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
	p.invalidateChunkLocked(e.a.UID)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（**CST**，与全仓日界口径一致）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00 CST）恢复。
//
// 口径（第十二轮审计 F4）：锚点是"签到窗口前的静默时段"，而签到窗口本身是 CST；
// 旧实现按宿主本地钟算 → 非 CST 宿主上会与签到窗口错开（本机 CST 零变化）。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	now = now.In(cstime.Zone) // 见上方口径说明：与签到窗口同为 CST
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, cstime.Zone)
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, cstime.Zone)
}

// reviveCoolingLocked 余额恢复解冻：只解冻余额耗尽冷却（CoolHard 的 until/coolKind/reason）
// 并更新 credits。
//
// 不动 CoolSoft 软限流退避、softStreak 与 modelCooldowns（6004 模型级台账）：
// 后两者的恢复证据是上游重置墙钟到期或探测成功，不是「余额有钱」。余额刷新
// 周期任务（每 5 分钟）经 ReenableIfCredits 到达这里——若在此清空整个冷却域，
// 任何限流冷却的实际寿命都被压到一个刷新周期内：6004 台账被抹后撞限号被误判
// 健康、重新选中再撞 429，全池冷却保护形同虚设。softStreak 亦保留：
// 退避计数与余额无关，由 NoteSuccess（成功是最强恢复证据）或自然到期收敛。
// 硬冷却（CoolHard）的权威恢复证据正是余额恢复（remain>0），照旧解冻。
// 不动熔断器（fails/retryCount/breakerUntil）——签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	if e.coolKind == CoolHard {
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
	}
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，解冻余额耗尽冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。

// ModelBlockStatus 描述某模型在全池范围内因模型级冷却而不可选的情况（上游 44d6e04 移植）。
type ModelBlockStatus struct {
	Blocked bool // true = 每个非禁用账号都对该模型处于冷却中
	// AllNotFound 本 realm 内所有"被挡"条目都是 11102（确定性聚合，供 400/503 分流）。
	AllNotFound bool
	Reason      string // 冷却原因。注意：本仓库当前写入的是网关常量（如 "6004 model rate limit"），
	// 不是上游原文——上游 44d6e04 的对应字段存的是上游 body，移植时未一并搬（见 §T.34 待办）。
	Until time.Time // 最早解封时间（零值 = 上游未给重置时刻）
	Count int       // 因此被挡的账号数
}

// ModelBlocked 报告该模型是否在全池范围内被模型级冷却挡住。
//
// 为什么需要它：选号失败时客户端只会拿到 no_healthy_account（"没有可用账号"），
// 但真实原因常常是「号都在、只是都对这个模型关闭」。两者对调用方的处置完全不同
// ——前者该等，后者换个模型才有用——此前却无法区分：首次请求还能看到上游原文
// （lastErr 非空），一旦负缓存写入，后续请求 lastErr 为空，就只剩"池子没号"，
// 上游原文与解封时间在那一刻被丢掉（上游 issue #102 附带发现）。
//
// 跨 realm 判定：调用方失败前已依次尝试过各域，所以只要有**任意**账号还能服务该
// 模型，就不能算全池阻塞 —— 此时返回 Blocked=false，让上层继续用原有的
// no_healthy_account 文案（选号失败另有原因：在途占满/积分保底/账号级冷却）。
//
// 口径必须与选号一致：用 modelCooled 而非直接查 map，这样 AuditOnly 条目
// （只审计不拦截）不会被误报成阻塞。
func (p *Pool) ModelBlocked(model, realm string) ModelBlockStatus {
	if model == "" {
		return ModelBlockStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var st ModelBlockStatus
	// notFoundAll：本 realm 内**所有**被挡条目都是 11102（"这个后端没有这个模型"）才为真。
	// 混合态（部分号 11102、部分号 6004 带重置）必须给**可重试**的 503 —— 否则同一客户端
	// 同一请求体会因 map 迭代序随机收到"换模型、别重试"与"稍后重试"两种相反指引
	// （审查 D 的 P1-3：原实现取任意第一条 Reason 的前缀）。
	notFoundAll := true
	for _, e := range p.byUID {
		if e.blocked(p.now()) {
			// 禁用/暂停选号（含未到期的 11140 限制）都不在候选里，不能作为"还有号能服务"的证据
			// （审查 B 的 P1-2：暂停号会让 Blocked 恒 false）。
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			// 单次请求只走一个 realm（handler resolveModel + PickExcludingForRealm），
			// 从不跨域轮转 ⇒ 跨 realm 的号不能用来否定"本 realm 全被挡"（审查 A 的 P1-1）。
			continue
		}
		if !e.modelCooled(now, model) {
			return ModelBlockStatus{} // 还有号能服务该模型 → 不是模型级阻塞
		}
		st.Count++
		mc, ok := e.modelCooldowns[model]
		if !ok {
			notFoundAll = false
			continue
		}
		isNotFound := strings.HasPrefix(strings.TrimSpace(mc.Reason), "11102")
		if !isNotFound {
			notFoundAll = false
		}
		// Reason 的选择也确定化：11102（"换模型才有用"是更强结论）优先，其余保留首个。
		if st.Reason == "" || (isNotFound && !strings.HasPrefix(strings.TrimSpace(st.Reason), "11102")) {
			st.Reason = mc.Reason
		}
		// 取最早解封：那才是"再等多久值得重试"的答案。
		if !mc.Until.IsZero() && (st.Until.IsZero() || mc.Until.Before(st.Until)) {
			st.Until = mc.Until
		}
	}
	st.AllNotFound = st.Count > 0 && notFoundAll
	if st.Count == 0 {
		// 该 realm 里压根没有非禁用/非暂停账号：这是"真的没号"，不是模型问题。
		return ModelBlockStatus{}
	}
	st.Blocked = true
	return st
}

// 11102「该后端无此模型」负缓存的有界退避参数（上游 44d6e04）：
// 6h 起、每命中一次左移一位（上限 2^4 倍），封顶 24h —— 最多一天再试一次。
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
)

// BlockModelBackoff 记录一次"该账号对该模型不可用"（上游 11102 model not found /
// service info not found）的 (账号, 模型) 负缓存。
//
// 与 CooldownSoftForModel 的区别：11102 是**确定性**的"本后端没有这个模型"，
// 不带上游重置时间，因此用**有界指数退避**（6h→24h）而不是跟随 resetAt。作用有二：
//  1. 后续同模型请求不再反复烧同一个号（省上游调用，此前每次都要打一次才拿到 11102）；
//  2. 让 handler 的 Pool.ModelBlocked 能给出"换模型才有用"的 400（而不是 503 让客户端
//     无限重试）—— 这正是上游 44d6e04 要交付的语义。
//
// 实测又通则由 BlockModelClear 清除（成功路径调用）。
// 生命周期：modelCooldowns **不持久化**（entry 落盘/Redis 快照都不含它）⇒ Hits 退避与 TTL
// 只在单个进程生命周期内成立，重启即清零（审查 D 的 P2-10）。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	// 容量上限（审查 E 的 P2-7）：负缓存的键是**客户端可控的模型名**，不设上限时
	// 单请求即可为每账号各造 ≤MaxRotate 个存活 6h+ 的条目。先惰性清理过期项，仍满则放弃
	// 本次写入（宁可不缓存，也不让 map 无界增长）。
	const maxModelCooldowns = 64
	if len(e.modelCooldowns) >= maxModelCooldowns {
		e.pruneExpiredModelCooldowns(now)
		if len(e.modelCooldowns) >= maxModelCooldowns {
			log.Printf("WARN: [pool] model cooldown cache full for uid=%s (>=%d), skip caching model=%q", uid8(uid), maxModelCooldowns, model)
			return
		}
	}
	prev := e.modelCooldowns[model]
	hits := prev.Hits
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	// 保留 ResetAt（信息量），但**有意升级** AuditOnly 台账为路由条目：11102 是确定性结论，
	// 只审计不拦截会让"每次请求继续烧号"的洞留在原地（审查 E 的 P2-6）。
	e.modelCooldowns[model] = modelCooldown{Until: now.Add(ttl), Reason: reason, Hits: hits, ResetAt: prev.ResetAt}
	// 不置 dirty：该字段不落盘（见函数头注释），置了只会触发无实义的写盘/快照。
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。
// 只清 **11102 条目**、不碰 6004 独立冷却表 —— 6004 有自身上游重置墙钟语义，
// 一次成功不该抹掉它。区分方式：11102 条目的 Reason 恒以 "11102" 开头。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	// 不置 dirty：同 BlockModelBackoff（该字段不落盘）。
}

// uid8 仅用于日志的短 uid（与 handler/logfmt 口径一致：前 8 位）。
func uid8(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// ModelLockRow 一个 (realm, model) 的当前锁定聚合（面板「模型锁池」视图，上游 b3f92dd）。
type ModelLockRow struct {
	Realm    string    `json:"realm"`
	Model    string    `json:"model"`
	Accounts int       `json:"accounts"`  // 该模型当前被挡的账号数
	Until    time.Time `json:"until"`     // 最早解封时间（零值 = 相关条目均无解封时间）
	NotFound bool      `json:"not_found"` // 是否含 11102 负缓存（"换模型才有用"）
	Reason   string    `json:"reason"`    // 展示用原因（确定化：11102 优先）
}

// ModelLocks 汇总当前所有处于**模型级冷却/负缓存**的 (realm, model) 行：各自挡了几个号、
// 最早何时解封、是否含 11102。只读快照，不改变选号。与 ModelBlocked 的分工：那个回答
// "某模型是不是全被挡"，这个回答"当前锁了哪些模型"。
func (p *Pool) ModelLocks() []ModelLockRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	type key struct{ realm, model string }
	agg := map[key]*ModelLockRow{}
	for _, e := range p.byUID {
		if e.disabled || e.paused || len(e.modelCooldowns) == 0 {
			continue
		}
		realm := e.a.Realm()
		for model, mc := range e.modelCooldowns {
			if mc.AuditOnly || !mc.Until.After(now) {
				continue // 台账 / 已过期：不参与"当前锁池"
			}
			k := key{realm, model}
			row, ok := agg[k]
			if !ok {
				row = &ModelLockRow{Realm: realm, Model: model}
				agg[k] = row
			}
			row.Accounts++
			if row.Until.IsZero() || mc.Until.Before(row.Until) {
				row.Until = mc.Until
			}
			isNF := strings.HasPrefix(strings.TrimSpace(mc.Reason), "11102")
			if isNF {
				row.NotFound = true
			}
			if row.Reason == "" || isNF {
				row.Reason = mc.Reason
			}
		}
	}
	out := make([]ModelLockRow, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Until.IsZero() != out[j].Until.IsZero() {
			return !out[i].Until.IsZero()
		}
		if !out[i].Until.Equal(out[j].Until) {
			return out[i].Until.Before(out[j].Until)
		}
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Model < out[j].Model
	})
	return out
}
