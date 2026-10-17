// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// creditFloor 积分保底（SetCreditFloor 注入；0 = 关闭，缺省即现状）。账号 credits 低于
	// 该值时对**实测收费**模型不再参与选号（判据 floorBlockedForRealmModel）；全池触底且
	// 无免费可接时选号返回 nil（硬语义：宁 503 不打穿余额）。
	creditFloor int64
	// modelRateOf 按 (realm, 模型) 查上游目录积分倍率（"" = 未知）。由 main 注入
	// （pool 不依赖 upstream 包）；仅在持 p.mu 时调用，回调不得反向调用 Pool 方法。
	modelRateOf func(realm, model string) string
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// nowFn 仅供测试注入确定性时钟（SetClock）；nil = time.Now。
	// 生产代码不应设置此字段。动机：调度是时间敏感的（闲置补偿按 lastUsed 与 now 的差值），
	// 用真实时钟做分布类回归会随机器快慢漂移（实测同一实现 11%~23% 波动）。
	nowFn func() time.Time
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int

	// Phase 3 加权调度与分块策略
	chunkQuotaCredits      float64
	criticalExpireHours    float64
	urgentExpireHours      float64
	nearExpireDays         float64
	safeReserveDays        float64
	lowCreditCutoff        float64
	expireUrgencyWeight    float64
	expirePower            float64
	adaptiveChunking       bool
	tierUnknownQuota       float64
	tierUnknownMaxReqs     int
	tierCheapCostCutoff    float64
	tierFlagshipCostCutoff float64
	tierCheapQuota         float64
	tierCheapMaxReqs       int
	tierNormalQuota        float64
	tierNormalMaxReqs      int
	tierFlagshipQuota      float64
	tierFlagshipMaxReqs    int
	activeChunks           map[string]*chunkState // realm -> 活跃分块
	lastChunkFinishedUID   map[string]string      // realm -> 上一次刚结束分块的账号 UID（交棒让渡用）

	// Direction 2: 柔性透气轮转（Paced Tier Rotation）
	pacedBreathingEnabled    bool
	pacedTier2ConsecutiveMax int
	tier2ConsecutiveChunks   map[string]int // realm -> Tier 2 连续服务分块数

	// 强制指定路由账户（Pinned / Forced Routing，仅保存在内存中，重启后重置）
	pinnedUIDs map[string]string // realm -> UID
}

// chunkState 单 realm 活跃分块状态（运行态）。
type chunkState struct {
	uid          string
	model        string
	creditQuota  float64
	maxReqs      int
	creditsUsed  float64
	requestCount int
	isUrgent24h  bool
	createdAt    time.Time
}

// ActiveChunkInfo 对外暴露的单 realm 活跃分块状态摘要。
type ActiveChunkInfo struct {
	UID          string  `json:"uid"`
	Model        string  `json:"model,omitempty"`
	CreditQuota  float64 `json:"credit_quota"`
	MaxReqs      int     `json:"max_reqs"`
	CreditsUsed  float64 `json:"credits_used"`
	RequestCount int     `json:"request_count"`
	IsUrgent24h  bool    `json:"is_urgent_24h"`
}

// SchedulePolicy 调度策略参数（Phase 3）。
type SchedulePolicy struct {
	ChunkQuotaCredits   float64 // 单账户连续消耗分块额度（默认 25.0 credit，<=0 禁用分块）
	CriticalExpireHours float64 // 紧急清仓层到期小时数（默认 48.0 小时，优先跨号接力）
	UrgentExpireHours   float64 // 临期清仓层到期小时数（默认 72.0 小时）
	NearExpireDays      float64 // 近期临期层天数（默认 7.0 天）
	SafeReserveDays     float64 // 安全底仓层天数（默认 10.0 天）
	LowCreditCutoff     float64 // 枯竭断崖保护阈值（默认 800.0 credit）
	ExpireUrgencyWeight float64 // 到期紧迫度权重加成满分（默认 15.0）
	ExpirePower         float64 // 到期天数非线性加速指数（默认 1.5）

	// 自适应分块参数
	AdaptiveChunking       bool    // 是否开启根据 CostPer1k 自适应三档分块（默认为 true，只要 ChunkQuotaCredits > 0）
	DisableAdaptive        bool    // 显式禁用自适应分块（测试用，固定采用 ChunkQuotaCredits）
	TierUnknownQuota       float64 // 冷启动未知单价模型的分块额度（默认 15.0 credit）
	TierUnknownMaxReqs     int     // 冷启动未知单价模型最大轮数（默认 0 不限）
	TierCheapCostCutoff    float64 // 极省档单价阈值（默认 0.08 credit/1k tokens）
	TierFlagshipCostCutoff float64 // 旗舰档单价阈值（默认 0.80 credit/1k tokens）
	TierCheapQuota         float64 // 极省档分块额度（默认 25.0 credit）
	TierCheapMaxReqs       int     // 极省档最大轮数（默认 60 轮退出保护）
	TierNormalQuota        float64 // 常规档分块额度（默认 50.0 credit）
	TierNormalMaxReqs      int     // 常规档最大轮数（默认 200 轮）
	TierFlagshipQuota      float64 // 旗舰档分块额度（默认 100.0 credit）
	TierFlagshipMaxReqs    int     // 旗舰档最大轮数（默认 100 轮）
	// 柔性透气轮转参数（Paced Tier Rotation）
	PacedBreathingEnabled    bool // 是否开启柔性透气轮转（默认 true，只要 ChunkQuotaCredits > 0）
	DisablePacedBreathing    bool // 显式禁用柔性透气轮转（测试用）
	PacedTier2ConsecutiveMax int  // Tier 2 连续服务分块上限（默认 3）
}

const (
	defaultChunkQuotaCredits        = 25.0
	defaultCriticalExpireHours      = 48.0
	defaultUrgentExpireHours        = 72.0
	defaultNearExpireDays           = 7.0
	defaultSafeReserveDays          = 10.0
	defaultLowCreditCutoff          = 800.0
	defaultExpireUrgencyWeight      = 15.0
	defaultExpirePower              = 1.5
	defaultTierUnknownQuota         = 15.0
	defaultTierUnknownMaxReqs       = 0
	defaultTierCheapCostCutoff      = 0.08
	defaultTierFlagshipCostCutoff   = 0.80
	defaultTierCheapQuota           = 25.0
	defaultTierCheapMaxReqs         = 60 // 仅针对 0 费模型生效的退出保护轮数（收费模型不限轮数）
	defaultTierNormalQuota          = 50.0
	defaultTierNormalMaxReqs        = 0 // 收费模型不限轮数，纯由积分配额驱动
	defaultTierFlagshipQuota        = 100.0
	defaultTierFlagshipMaxReqs      = 0 // 收费模型不限轮数，纯由积分配额驱动
	defaultPacedTier2ConsecutiveMax = 3 // Tier 2 连续服务分块默认上限
)

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:                    map[string]*entry{},
		stateFp:                  stateFp,
		breakerThreshold:         defaultBreakerThreshold,
		breakerCooldown:          defaultBreakerCooldown,
		breakerCooldownMax:       defaultBreakerCooldownMax,
		idleWeightPerHour:        defaultIdleWeightPerHour,
		idleWeightMax:            defaultIdleWeightMax,
		chunkQuotaCredits:        0, // 默认 0（未启用分块，每次独立抽签；服务启动由 SetSchedulePolicy 注入 25.0）
		criticalExpireHours:      defaultCriticalExpireHours,
		urgentExpireHours:        defaultUrgentExpireHours,
		nearExpireDays:           defaultNearExpireDays,
		safeReserveDays:          defaultSafeReserveDays,
		lowCreditCutoff:          defaultLowCreditCutoff,
		expireUrgencyWeight:      defaultExpireUrgencyWeight,
		expirePower:              defaultExpirePower,
		adaptiveChunking:         true,
		tierUnknownQuota:         defaultTierUnknownQuota,
		tierUnknownMaxReqs:       defaultTierUnknownMaxReqs,
		tierCheapCostCutoff:      defaultTierCheapCostCutoff,
		tierFlagshipCostCutoff:   defaultTierFlagshipCostCutoff,
		tierCheapQuota:           defaultTierCheapQuota,
		tierCheapMaxReqs:         defaultTierCheapMaxReqs,
		tierNormalQuota:          defaultTierNormalQuota,
		tierNormalMaxReqs:        defaultTierNormalMaxReqs,
		tierFlagshipQuota:        defaultTierFlagshipQuota,
		tierFlagshipMaxReqs:      defaultTierFlagshipMaxReqs,
		activeChunks:             map[string]*chunkState{},
		lastChunkFinishedUID:     map[string]string{},
		pacedBreathingEnabled:    true,
		pacedTier2ConsecutiveMax: defaultPacedTier2ConsecutiveMax,
		tier2ConsecutiveChunks:   map[string]int{},
		pinnedUIDs:               map[string]string{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// SetSchedulePolicy 注入 Phase 3 调度策略。
func (p *Pool) SetSchedulePolicy(pol SchedulePolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chunkQuotaCredits = pol.ChunkQuotaCredits
	if pol.CriticalExpireHours > 0 {
		p.criticalExpireHours = pol.CriticalExpireHours
	}
	if pol.UrgentExpireHours > 0 {
		p.urgentExpireHours = pol.UrgentExpireHours
	}
	if pol.NearExpireDays > 0 {
		p.nearExpireDays = pol.NearExpireDays
	}
	if pol.SafeReserveDays > 0 {
		p.safeReserveDays = pol.SafeReserveDays
	}
	if pol.LowCreditCutoff > 0 {
		p.lowCreditCutoff = pol.LowCreditCutoff
	}
	if pol.ExpireUrgencyWeight > 0 {
		p.expireUrgencyWeight = pol.ExpireUrgencyWeight
	}
	if pol.ExpirePower > 0 {
		p.expirePower = pol.ExpirePower
	}
	if pol.DisableAdaptive {
		p.adaptiveChunking = false
	} else if pol.AdaptiveChunking || pol.ChunkQuotaCredits > 0 {
		p.adaptiveChunking = true
	}
	if pol.TierUnknownQuota > 0 {
		p.tierUnknownQuota = pol.TierUnknownQuota
	}
	if pol.TierUnknownMaxReqs > 0 {
		p.tierUnknownMaxReqs = pol.TierUnknownMaxReqs
	}
	if pol.TierCheapCostCutoff > 0 {
		p.tierCheapCostCutoff = pol.TierCheapCostCutoff
	}
	if pol.TierFlagshipCostCutoff > 0 {
		p.tierFlagshipCostCutoff = pol.TierFlagshipCostCutoff
	}
	if pol.TierCheapQuota > 0 {
		p.tierCheapQuota = pol.TierCheapQuota
	}
	if pol.TierCheapMaxReqs > 0 {
		p.tierCheapMaxReqs = pol.TierCheapMaxReqs
	}
	if pol.TierNormalQuota > 0 {
		p.tierNormalQuota = pol.TierNormalQuota
	}
	if pol.TierNormalMaxReqs > 0 {
		p.tierNormalMaxReqs = pol.TierNormalMaxReqs
	}
	if pol.TierFlagshipQuota > 0 {
		p.tierFlagshipQuota = pol.TierFlagshipQuota
	}
	if pol.TierFlagshipMaxReqs > 0 {
		p.tierFlagshipMaxReqs = pol.TierFlagshipMaxReqs
	}
	if pol.DisablePacedBreathing {
		p.pacedBreathingEnabled = false
	} else if pol.PacedBreathingEnabled || pol.ChunkQuotaCredits > 0 {
		p.pacedBreathingEnabled = true
	}
	if pol.PacedTier2ConsecutiveMax > 0 {
		p.pacedTier2ConsecutiveMax = pol.PacedTier2ConsecutiveMax
	}
}

// SetChunkQuotaCredits 动态设置单账户分块额度（<=0 禁用分块）。
func (p *Pool) SetChunkQuotaCredits(q float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chunkQuotaCredits = q
}

// SchedulePolicy 获取当前调度策略参数。
func (p *Pool) SchedulePolicy() SchedulePolicy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return SchedulePolicy{
		ChunkQuotaCredits:        p.chunkQuotaCredits,
		CriticalExpireHours:      p.criticalExpireHours,
		UrgentExpireHours:        p.urgentExpireHours,
		NearExpireDays:           p.nearExpireDays,
		SafeReserveDays:          p.safeReserveDays,
		LowCreditCutoff:          p.lowCreditCutoff,
		ExpireUrgencyWeight:      p.expireUrgencyWeight,
		ExpirePower:              p.expirePower,
		AdaptiveChunking:         p.adaptiveChunking,
		TierUnknownQuota:         p.tierUnknownQuota,
		TierUnknownMaxReqs:       p.tierUnknownMaxReqs,
		TierCheapCostCutoff:      p.tierCheapCostCutoff,
		TierFlagshipCostCutoff:   p.tierFlagshipCostCutoff,
		TierCheapQuota:           p.tierCheapQuota,
		TierCheapMaxReqs:         p.tierCheapMaxReqs,
		TierNormalQuota:          p.tierNormalQuota,
		TierNormalMaxReqs:        p.tierNormalMaxReqs,
		TierFlagshipQuota:        p.tierFlagshipQuota,
		TierFlagshipMaxReqs:      p.tierFlagshipMaxReqs,
		PacedBreathingEnabled:    p.pacedBreathingEnabled,
		PacedTier2ConsecutiveMax: p.pacedTier2ConsecutiveMax,
	}
}

// ActiveChunk 查询指定 realm 的当前活跃分块信息（若无活跃分块返回 ok=false）。
func (p *Pool) ActiveChunk(realm string) (ActiveChunkInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	chunk, ok := p.activeChunks[realm]
	if !ok || chunk == nil {
		return ActiveChunkInfo{}, false
	}
	return ActiveChunkInfo{
		UID:          chunk.uid,
		Model:        chunk.model,
		CreditQuota:  chunk.creditQuota,
		MaxReqs:      chunk.maxReqs,
		CreditsUsed:  chunk.creditsUsed,
		RequestCount: chunk.requestCount,
		IsUrgent24h:  chunk.isUrgent24h,
	}, true
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// SetClock 仅供测试注入确定性时钟（nil = time.Now）；生产代码不应调用。
//
// 动机：调度是**时间敏感**的（闲置补偿按 lastUsed 与 now 的差值计算），用真实时钟做
// 分布类回归会让结果随机器/race 检测器的快慢漂移（实测同一实现 11%~23% 波动）。
// 注入虚拟时钟后，分布用例完全可复现。
//
// ⚠️ 只影响 **pick 决策路径**：冷却（cooldown.go）、熔断、到期与日切仍用 `time.Now()`。
// 因此注入值必须**锚定真实 now 附近**（同一时刻量级），否则会出现"半虚拟时间线"：
// 例如把时钟拨到 +2h，刚写入的 1 分钟软冷却在 `Pick` 眼里已过期 → 冷却被静默无视，
// 相关用例假绿。
func (p *Pool) SetClock(fn func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nowFn = fn
}

// now 取当前时间。调用方均在持锁路径上（pick/weightOf/pickWeightedTier），
// 而 nowFn 只在测试初始化时写入，故读它无需额外同步。
func (p *Pool) now() time.Time {
	if fn := p.nowFn; fn != nil {
		return fn()
	}
	return time.Now()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		if !p.saveLocked() {
			// 落盘失败：回置 dirty 交给后台 flusher 重试。否则"已删账号"只存在于内存，
			// 下次启动 load() 会把它们从 state.json 里复活（幽灵账号）。
			p.dirty.Store(true)
		}
	}
}

// Remove 从池中移除指定 uid 账号并立即持久化；返回被移除的凭证（不存在返回 nil）。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// PinAccount 强制指定路由账户（内存存储，重启后重置）。
// 指定后，该 realm 的后续选号将无视加权与分块直接路由给该账号。
func (p *Pool) PinAccount(uid string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return "", fmt.Errorf("账号不存在: %s", uid)
	}
	if p.pinnedUIDs == nil {
		p.pinnedUIDs = make(map[string]string)
	}
	realm := e.a.Realm()
	p.pinnedUIDs[realm] = uid
	log.Printf("[pool] [%s] 账号已被强制指定路由: %s", realm, logfmt.Label(uid, e.a.Nickname))
	return realm, nil
}

// UnpinAccount 取消强制指定路由账户。uid 为 "" 或 "all" 时清除全部强制指定。
func (p *Pool) UnpinAccount(uid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pinnedUIDs == nil {
		return nil
	}
	if uid == "" || uid == "all" {
		p.pinnedUIDs = make(map[string]string)
		log.Printf("[pool] 已清除所有强制路由账户，恢复自动智能调度")
		return nil
	}
	for realm, u := range p.pinnedUIDs {
		if u == uid {
			delete(p.pinnedUIDs, realm)
			log.Printf("[pool] [%s] 账号已取消强制路由: %s", realm, logfmt.UID8(uid))
		}
	}
	return nil
}

// PinnedUIDs 返回当前所有域的强制指定路由账号（只读快照）。
func (p *Pool) PinnedUIDs() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(p.pinnedUIDs))
	for k, v := range p.pinnedUIDs {
		out[k] = v
	}
	return out
}

// PinnedUIDForRealm 返回指定 realm 的强制指定账号 UID（空表示无强制指定）。
func (p *Pool) PinnedUIDForRealm(realm string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pinnedUIDLocked(realm)
}

// pinnedUIDLocked 内部在已持锁状态下获取强制账号 UID。
func (p *Pool) pinnedUIDLocked(realm string) string {
	if p.pinnedUIDs == nil || len(p.pinnedUIDs) == 0 {
		return ""
	}
	if realm != "" {
		return p.pinnedUIDs[realm]
	}
	for _, u := range p.pinnedUIDs {
		if u != "" {
			return u
		}
	}
	return ""
}

// CreditFloor 当前积分保底（0 = 关闭）。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloor 设置积分保底。负值视为不修改（0 合法 = 关闭），与 SetMaxInFlight 同款语义。
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetModelRateOf 注入 (realm, 模型) → 目录积分倍率 的查询函数（"" = 未知）。
// 供积分保底做目录兜底：本地尚无实测成本时用目录价判"是否收费"；未注入则退化为
// "只认实测"（等价于上游 d19add4 的 tier1 语义）。
func (p *Pool) SetModelRateOf(fn func(realm, model string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelRateOf = fn
}
