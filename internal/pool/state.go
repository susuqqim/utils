// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"math"
	"sort"
	"strings"
	"time"
	"workbuddy2api/internal/cstime"

	"workbuddy2api/internal/auth"
)

// Restrict 上游风控限制（11140 账号画像风控）：禁用 **ttl 时长**，到期自动恢复可选。
//
// 【P1-7】与 Disable 的区别：Disable 是永久禁用（手工/session 死亡，restrictedUntil 为零）；
// Restrict 记录到期时间，因为上游这类限制会自行解除（社区实证：数小时~次日），
// 永久禁用会让恢复后的健康号白白浪费，且诱导用户去点"解冻"。
func (p *Pool) Restrict(uid string, ttl time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.paused = false
		e.reason = reason
		if ttl > 0 {
			e.restrictedUntil = p.now().Add(ttl)
		}
		p.invalidateChunkLocked(uid)
		p.dirty.Store(true)
	}
}

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.paused = false                // 禁用是更强终态：不叠加暂停选号
		e.restrictedUntil = time.Time{} // 永久禁用：显式清掉限制到期时间，避免被误判为"到期自动恢复"
		e.reason = reason
		p.invalidateChunkLocked(uid)
		p.dirty.Store(true)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.paused = false // 禁用是更强终态：不叠加暂停选号
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.invalidateChunkLocked(uid)
	p.dirty.Store(true)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.paused = false // 与 Revive 统一：解冻是全清，避免"已解冻却永不被选"的幽灵号
		e.restrictedUntil = time.Time{}
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// Revive 运维口径的"无条件恢复"：清禁用、冷却（含软退避计数）与熔断运行态。
// 与 ReviveDisabled（只清禁用）和 ReenableIfCredits（只清冷却、不动熔断）的区别：
// 本方法清除全部惩罚状态，供管理面板"解冻"按钮使用——人工判断该号可用时一键恢复。
// uid 不存在返回 false（供调用方区分"账号不存在"与"已复活"）。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.paused = false // 解冻是全清：暂停选号一并解除
	e.restrictedUntil = time.Time{}
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil
	e.sessionDeadFails = 0
	e.fails = 0
	e.retryCount = 0
	e.breakerUntil = time.Time{}
	p.dirty.Store(true)
	return true
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain int64, total ...int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if len(total) > 0 {
			e.creditsTotal = total[0]
		}
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain)
		} else {
			e.credits = remain
		}
		p.dirty.Store(true)
	}
}

// SetCreditsDetailed 更新账号余额/总额 + 快过期子集。
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsTotal = total
		e.creditsExpiring = expiring
		p.dirty.Store(true)
	}
}

// UpdateResource 更新账号的上游资产感知信息（余额、最早到期时间）。
func (p *Pool) UpdateResource(uid string, remain int64, earliestExpire time.Time) {
	now := time.Now()
	var u24, u72 int64
	if !earliestExpire.IsZero() && earliestExpire.After(now) {
		if earliestExpire.Before(now.Add(24 * time.Hour)) {
			u24 = remain
		}
		if earliestExpire.Before(now.Add(72 * time.Hour)) {
			u72 = remain
		}
	}
	p.UpdateResourceDetail(uid, remain, earliestExpire, u24, u72)
}

// UpdateResourceDetail 更新账号的详细上游资产感知信息（余额、最早到期时间、24h/72h临期额度，以及可选的多包资产管道）。
func (p *Pool) UpdateResourceDetail(uid string, remain int64, earliestExpire time.Time, urgent24h, urgent72h int64, packages ...PackageItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if !earliestExpire.IsZero() {
			e.earliestExpire = earliestExpire
		}
		e.urgent24hRemain = urgent24h
		e.urgent72hRemain = urgent72h
		e.lastResourceSync = time.Now()
		if len(packages) > 0 {
			e.packages = make([]PackageItem, len(packages))
			copy(e.packages, packages)
			sort.Slice(e.packages, func(i, j int) bool {
				if e.packages[i].Expire.IsZero() {
					return false
				}
				if e.packages[j].Expire.IsZero() {
					return true
				}
				return e.packages[i].Expire.Before(e.packages[j].Expire)
			})
			var merged []PackageItem
			for _, pkg := range e.packages {
				if len(merged) > 0 && merged[len(merged)-1].Expire.Equal(pkg.Expire) {
					merged[len(merged)-1].Remain += pkg.Remain
				} else {
					merged = append(merged, pkg)
				}
			}
			e.packages = merged
			p.recomputeFromPackagesLocked(e, time.Now())
		} else {
			e.packages = nil
		}
		if remain > 0 && !e.disabled && e.coolKind == CoolHard {
			p.reviveCoolingLocked(e, remain)
		}
		p.dirty.Store(true)
	}
}

// recomputeFromPackagesLocked 根据多包管道重新计算账号的 earliestExpire、urgent24hRemain (<=48h) 和 urgent72hRemain (<=72h)。
// 调用方必须已持有 p.mu。
func (p *Pool) recomputeFromPackagesLocked(e *entry, now time.Time) {
	if len(e.packages) == 0 {
		return
	}
	critDur := time.Duration(p.criticalExpireHours) * time.Hour
	if critDur <= 0 {
		critDur = 48 * time.Hour
	}
	urgDur := time.Duration(p.urgentExpireHours) * time.Hour
	if urgDur <= 0 {
		urgDur = 72 * time.Hour
	}

	valid := e.packages[:0]
	for _, pkg := range e.packages {
		if pkg.Remain > 0 {
			valid = append(valid, pkg)
		}
	}
	e.packages = valid

	var firstExp time.Time
	var u24, u72 int64
	for _, pkg := range e.packages {
		if pkg.Expire.IsZero() {
			continue
		}
		if pkg.Expire.After(now) {
			if firstExp.IsZero() || pkg.Expire.Before(firstExp) {
				firstExp = pkg.Expire
			}
			if pkg.Expire.Before(now.Add(critDur)) {
				u24 += pkg.Remain
			}
			if pkg.Expire.Before(now.Add(urgDur)) {
				u72 += pkg.Remain
			}
		}
	}
	e.earliestExpire = firstExp
	e.urgent24hRemain = u24
	e.urgent72hRemain = u72
}

// NoteUsage 记录单次调用的资源消耗（扣费与 token 数），实时扣减本地积分并累计。
func (p *Pool) NoteUsage(uid string, credit float64, tokens int) {
	p.NoteUsageForModel(uid, credit, tokens, "")
}

// NoteUsageForModel 记录单次调用的资源消耗（扣费与 token 数，附带模型标识），
// 实时扣减本地积分、累加请求轮数与分块额度，驱动自适应分块与交棒让渡。
func (p *Pool) NoteUsageForModel(uid string, credit float64, tokens int, model string) {
	if uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		today := currentDayCST(time.Now())
		if e.dailyDate != today {
			e.dailyCreditsUsed = 0
			e.dailyTokensUsed = 0
			e.dailyDate = today
		}
		if credit > 0 {
			e.creditsUsed += credit
			e.dailyCreditsUsed += credit
			deduct := int64(math.Ceil(credit))
			if e.credits > deduct {
				e.credits -= deduct
			} else {
				e.credits = 0
			}
			if len(e.packages) > 0 {
				remDeduct := deduct
				for i := range e.packages {
					if remDeduct <= 0 {
						break
					}
					if e.packages[i].Remain >= remDeduct {
						e.packages[i].Remain -= remDeduct
						remDeduct = 0
					} else {
						remDeduct -= e.packages[i].Remain
						e.packages[i].Remain = 0
					}
				}
				p.recomputeFromPackagesLocked(e, time.Now())
			} else {
				if e.urgent24hRemain > deduct {
					e.urgent24hRemain -= deduct
				} else {
					e.urgent24hRemain = 0
				}
				if e.urgent72hRemain > deduct {
					e.urgent72hRemain -= deduct
				} else {
					e.urgent72hRemain = 0
				}
			}
		}
		p.noteChunkUsageLocked(uid, credit)
		if tokens > 0 {
			e.totalTokens += int64(tokens)
			e.dailyTokensUsed += int64(tokens)
		}
		p.dirty.Store(true)
	}
}

// AccountCredits 返回指定账号当前剩余积分与累计已消耗积分。
func (p *Pool) AccountCredits(uid string) (remain int64, used float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.credits, e.creditsUsed
	}
	return 0, 0
}

// AccountCreditsDaily 返回指定账号当前剩余积分、今日已消耗积分与累计已消耗积分。
func (p *Pool) AccountCreditsDaily(uid string) (remain int64, dailyUsed float64, totalUsed float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		today := currentDayCST(time.Now())
		daily := e.dailyCreditsUsed
		if e.dailyDate != today {
			daily = 0
		}
		return e.credits, daily, e.creditsUsed
	}
	return 0, 0, 0
}

// UsageTotals 返回账号池当前（今日 + 累计）的消耗汇总。
func (p *Pool) UsageTotals() (dailyCredits, totalCredits float64, dailyTokens, totalTokens int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	today := currentDayCST(time.Now())
	for _, e := range p.byUID {
		totalCredits += e.creditsUsed
		totalTokens += e.totalTokens
		if e.dailyDate == today {
			dailyCredits += e.dailyCreditsUsed
			dailyTokens += e.dailyTokensUsed
		}
	}
	return
}

// ResetDailyUsageIfExpired 检查并主动刷新跨日用量（由定时调度器调用，或按需主动调用）。
func (p *Pool) ResetDailyUsageIfExpired() {
	p.mu.Lock()
	defer p.mu.Unlock()
	today := currentDayCST(time.Now())
	changed := false
	for _, e := range p.byUID {
		if e.dailyDate != today {
			e.dailyCreditsUsed = 0
			e.dailyTokensUsed = 0
			e.dailyDate = today
			changed = true
		}
	}
	if changed {
		p.dirty.Store(true)
	}
}

// noteChunkUsageLocked 累加分块消耗与请求轮数，若达到分块额度、轮数上限或 24h 临期积分耗尽则结束该分块并交棒。调用方必须已持有 p.mu。
func (p *Pool) noteChunkUsageLocked(uid string, credit float64) {
	if p.chunkQuotaCredits <= 0 {
		return
	}
	for realm, chunk := range p.activeChunks {
		if chunk != nil && chunk.uid == uid {
			chunk.requestCount++
			if credit > 0 {
				chunk.creditsUsed += credit
			}
			e := p.byUID[uid]
			// 如果该分块是 24h 临期任务，且 24h 临期积分已耗尽，立即结束分块以将流量让渡给其他 24h 临期账号
			if chunk.isUrgent24h && (e == nil || e.urgent24hRemain <= 0) {
				p.retireChunkLocked(realm, uid)
				continue
			}
			quota := chunk.creditQuota
			if quota <= 0 {
				quota = p.chunkQuotaCredits
			}
			// 达到单账号分块上限（如 25 / 50 / 100 credits），结束本轮分块并交棒
			if quota > 0 && chunk.creditsUsed >= quota {
				p.retireChunkLocked(realm, uid)
				continue
			}
			// 达到单账号最大请求轮数封顶（如极省档 60 轮退出保护），结束本轮分块并交棒
			if chunk.maxReqs > 0 && chunk.requestCount >= chunk.maxReqs {
				p.retireChunkLocked(realm, uid)
				continue
			}
		}
	}
}

// retireChunkLocked 正常结束活跃分块并记录该分块 UID，以便下一轮选举优先交棒让渡。调用方必须已持有 p.mu。
func (p *Pool) retireChunkLocked(realm, uid string) {
	delete(p.activeChunks, realm)
	if p.lastChunkFinishedUID == nil {
		p.lastChunkFinishedUID = make(map[string]string)
	}
	p.lastChunkFinishedUID[realm] = uid

	// Direction 2: 柔性透气轮转计数（Paced Tier Rotation）
	if p.tier2ConsecutiveChunks == nil {
		p.tier2ConsecutiveChunks = make(map[string]int)
	}
	if e := p.byUID[uid]; e != nil {
		if e.urgent24hRemain > 0 {
			// Tier 1（<=48h 极度紧急）不参与 Tier 2 计数，且重置 Tier 2 连续计数
			p.tier2ConsecutiveChunks[realm] = 0
		} else if e.urgent72hRemain > 0 {
			// Tier 2（48h~72h 临期缓冲），连续服务分块数 +1
			p.tier2ConsecutiveChunks[realm]++
		} else {
			// Tier 3/4/5 健康账号消费完一个透气分块，透气完成，重置 Tier 2 连续计数为 0
			p.tier2ConsecutiveChunks[realm] = 0
		}
	}
}

// invalidateChunkLocked 账号不可用（禁用/冷却/熔断）时清除引用该账号的活跃分块。调用方必须已持有 p.mu。
func (p *Pool) invalidateChunkLocked(uid string) {
	for realm, chunk := range p.activeChunks {
		if chunk != nil && chunk.uid == uid {
			delete(p.activeChunks, realm)
			if p.lastChunkFinishedUID != nil && p.lastChunkFinishedUID[realm] == uid {
				delete(p.lastChunkFinishedUID, realm)
			}
		}
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		for realm, chunk := range p.activeChunks {
			if chunk != nil && chunk.uid == uid {
				chunk.requestCount++
				if chunk.maxReqs > 0 && chunk.requestCount >= chunk.maxReqs {
					p.retireChunkLocked(realm, uid)
				}
			}
		}
		p.dirty.Store(true)
	}
}

// ModelCost 读取账号在某模型上的实测扣费观测（CostPer1k 与是否存在有效观测）。
// 供测试/运维断言成本账本内容；无观测或观测过期（modelCostTTL）时 ok=false。
func (p *Pool) ModelCost(uid, model string) (per1k float64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, false
	}
	mc, ok := e.modelCostOf(model, time.Now())
	if !ok {
		return 0, false
	}
	return mc.CostPer1k, true
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本。
// credit 为上游 usage.credit（本次真实扣费），tokens 为本次请求的 token 总数
// （prompt+completion，用于折算单位成本）。tokens<=0 时不记录：无法折算单价，
// 记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本仅内存态——成本随上游活动（限免期/夜间免费/折扣）变化，持久化旧值
// 反而是脏数据；重启后重新学习，代价只是前几次请求无偏好。
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 {
		return
	}
	// 单价按每千 token 归一，消除请求长度差异。
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
// **不碰 modelCooldowns**：6004 模型级 limit 每模型独立计时，其他模型成功不得抹掉
// 本模型的冷却截止（这正是"每模型独立"的语义）。模型级冷却只由到期/复活/账号级
// 冷却（Cooldown/reviveCoolingLocked）清除。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// NoteClientAbort 客户端中途取消（499）：清掉"账号正在连续失败"的运行态，但**不累加
// 成功计数**。
//
// 为什么需要它（而不是直接复用 NoteSuccess）：NoteSuccess 会 `successCount++`，而成功率项
// 直接进调度权重（满分 3.0，见 successWeightTerm）——把"用户/Agent 主动按了停止"算成账号
// 业绩，会让"响应慢到被客户端掐断"的账号反而越来越受青睐。
//
// 为什么必须清账：`softDurationLocked` 对连续软限流做**指数退避**（d << softStreak-1）。
// 499 路径本身不惩罚账号（无 fail/applyErrorPolicy），若也不清 `softStreak`，则该计数只增
// 不减：取消越频繁（Agent 超时、用户点停止）越没有机会归零，下一次真实 6004 会被按陈旧的
// streak 放大成远超必要的冷却。
//
// 刻意**不碰 breakerUntil**：熔断是"账号连续失败"的判定，客户端取消不构成恢复证据；
// 也**不碰 modelCooldowns / lastSuccess / successCount**（同 NoteSuccess 的口径）。
func (p *Pool) NoteClientAbort(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.fails = 0
		e.retryCount = 0
		e.softStreak = 0
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// RecordTokenUsage 记录一次实际发起的聊天账号尝试及上游返回的 usage 增量。
// usage 字段缺失时仍累计请求次数，但只累计明确存在的 token 字段。
func (p *Pool) RecordTokenUsage(uid string, delta TokenUsageDelta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	usage := &e.tokenUsage
	usage.RequestCount++
	usage.LastUsedAt = time.Now()
	if delta.Model != "" {
		usage.LastModel = delta.Model
	}
	known := false
	if delta.HasPromptTokens && delta.PromptTokens >= 0 {
		usage.PromptTokens += delta.PromptTokens
		known = true
	}
	if delta.HasCompletionTokens && delta.CompletionTokens >= 0 {
		usage.CompletionTokens += delta.CompletionTokens
		known = true
	}
	if delta.HasTotalTokens && delta.TotalTokens >= 0 {
		usage.TotalTokens += delta.TotalTokens
		known = true
	}
	if known {
		usage.UsageCount++
	}
	if delta.HasLatencyMs && delta.LatencyMs >= 0 {
		usage.LastLatencyMs = delta.LatencyMs
	}
	if delta.HasTokensPerSecond && delta.TokensPerSecond >= 0 {
		speed := delta.TokensPerSecond
		usage.LastTokensPerSecond = &speed
	} else {
		// 失败或缺少 completion_tokens 时不展示上一次请求的旧吞吐速度。
		usage.LastTokensPerSecond = nil
	}
	p.dirty.Store(true)
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入
// （issue #31 模型豁免）。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 同 PickByUID，但用 healthyForModel 校验：绑定号在当前模型被
// 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换。
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
// 注：返回的 disabled 口径**含 paused**（上游语义，见 case e.disabled || e.paused）；
// 逐账号 Status.Disabled/Paused 仍严格分离。需要区分时用 /status 的独立 paused 键。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm 同 CountsDetailed，但仅统计 Realm()==realm 的账号；
// realm=="" 退化为全池（现状语义，走同一遍历 helper 避免重复代码）。
// 双 realm 共存时供 /status 按域分组暴露 CN/global 各自可用性。
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm 是两函数共用的遍历实现；realm=="" 不加谓词。
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		case e.disabled && !e.restrictionExpired(p.now()) || e.paused:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个（对任意模型）healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
//
// 模型级豁免（issue #31 的探活侧补齐）：6004 模型级软冷却中的账号（modelExempt 形态）
// 对触发模型不可用、对其他模型仍可选，探活与 chat 必须同口径，否则"全号被某模型限流
// 但换模型可用"时 chat 实际 200 而 /healthz 误报 503。chat 侧按请求模型细粒度判定
// （healthyForModel：全账号健康且该模型不在独立冷却内才放行，模型豁免作用于选号），
// 探活侧没有请求模型上下文，取「存在豁免形态」的存在性语义——豁免账号（未禁用、
// 未熔断、存在模型级冷却条目）至少还剩触发模型之外的模型可用，ServableNow 计入。
// 注意与 chat 判定在"账号级 until 冷却 + 模型豁免并存"时并不完全重合：modelExempt
// 不检查 until，而 healthyForModel 会先判 until 再查模型冷却；该混合形态现实中不可达
// （plain Cooldown 会清空 modelCooldowns，6004 不写 until），此处仅为探活存在性语义，
// 不构成 chat 选号路径。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// ServableForRealm 报告某 realm 是否可服务：存在至少一个该 realm 的 healthy 且未占满在途名额的账号。
// 与 ServableNow 同口径（healthy 或模型豁免、排除 inFlightFull），仅叠加 Realm()==realm 谓词。
// realm=="" 退化为 ServableNow（现状语义）。供 /healthz 按 realm 暴露 CN/global 各自可达性。
func (p *Pool) ServableForRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

// NoteCheckin 记录账号最近一次签到结果（运行态，不持久化）。
// 仅供面板展示"今日是否签到"，不参与任何调度决策。
func (p *Pool) NoteCheckin(uid, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e != nil {
		e.lastCheckinAt = time.Now()
		e.lastCheckinStatus = status
	}
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	today := currentDayCST(now)
	dailyCredits := e.dailyCreditsUsed
	dailyTokens := e.dailyTokensUsed
	if e.dailyDate != today {
		dailyCredits = 0
		dailyTokens = 0
	}
	st := Status{
		UID: uid,
		// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空，
		// 每模型一行（modelCooldowns 内未到期的条目），多模型同时限流全部展示。
		// 到期判据 = 该模型的独立冷却 until 未过；条件满足才输出，随到期自然消失，
		// 普通软冷却（无模型级表）/硬冷却不产生台账（零回归）。
		RateLimitedModels: p.rateLimitedModelsLocked(e, now),
		Realm:             e.a.Realm(),
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		CreditsTotal:      e.creditsTotal,
		CreditsUsed:       e.creditsUsed,
		DailyCreditsUsed:  dailyCredits,
		DailyTokensUsed:   dailyTokens,
		DailyDate:         today,
		TotalTokens:       e.totalTokens,
		TokenUsage:        e.tokenUsage,
		EarliestExpire:    e.earliestExpire,
		Urgent24hRemain:   e.urgent24hRemain,
		Urgent48hRemain:   e.urgent24hRemain,
		Urgent72hRemain:   e.urgent72hRemain,
		Packages:          e.packages,
		LastResourceSync:  e.lastResourceSync,
		Cooling:           now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:            e.reason,
		Disabled:          e.disabled && !e.restrictionExpired(p.now()),
		Paused:            e.paused,
		SuccessCount:      e.successCount,
		ErrTotal:          e.errTotal,
		LastSuccessTime:   e.lastSuccess,
		LastErrTime:       e.lastErr,
		Until:             e.until,
		SoftStreak:        e.softStreak,
		InFlight:          int(e.inFlight.Load()),
		BreakerFails:      e.fails,
		BreakerUntil:      e.breakerUntil,
		LastCheckinAt:     e.lastCheckinAt,
		LastCheckinStatus: e.lastCheckinStatus,
		EnterpriseID:      e.a.EnterpriseID,
	}
	if !e.earliestExpire.IsZero() {
		days := time.Until(e.earliestExpire).Hours() / 24.0
		st.DaysUntilExpire = math.Round(days*10) / 10
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		// 常规冷却（until）与熔断期（breakerUntil）可能只有其一在生效：
		//   - until 生效：按 until 计剩余并保持原 kind（本仓 Cooldown 入口同时喂熔断器，
		//     若此处取 breaker 会让 soft_rate 恒显示成 breaker）；
		//   - 仅熔断期生效：此前误报 0 / unknown，这里按 breakerUntil 计剩余并标 breaker。
		switch {
		case now.Before(e.until):
			st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
			st.CoolKind = e.coolKind.String()
		case now.Before(e.breakerUntil):
			st.CoolRemaining = int64(time.Until(e.breakerUntil).Seconds() + 0.999)
			st.CoolKind = "breaker"
		default:
			st.CoolRemaining = 0
		}
	}

	// 强制路由状态
	st.Pinned = (p.pinnedUIDLocked(e.a.Realm()) == uid)

	// 活跃分块状态
	if chunk, ok := p.activeChunks[e.a.Realm()]; ok && chunk != nil && chunk.uid == uid {
		st.IsActiveChunk = true
		st.ActiveChunkReqs = chunk.requestCount
		st.ActiveChunkCredits = chunk.creditsUsed
	}

	// 调度梯队判定
	if e.urgent24hRemain > 0 {
		st.Tier = "Tier 1: 48h紧急清仓"
	} else if e.urgent72hRemain > 0 {
		st.Tier = "Tier 2: 72h临期清仓"
	} else if !e.earliestExpire.IsZero() && e.earliestExpire.Before(now.Add(time.Duration(p.nearExpireDays*24)*time.Hour)) && e.credits > 0 {
		st.Tier = "Tier 3: 7天近期临期"
	} else if !e.earliestExpire.IsZero() && e.earliestExpire.Before(now.Add(time.Duration(p.safeReserveDays*24)*time.Hour)) && e.credits > 0 {
		st.Tier = "Tier 4: 10天过渡梯队"
	} else {
		st.Tier = "Tier 5: 安全底仓"
	}

	// 柔性透气让渡判定
	if p.pacedBreathingEnabled && p.tier2ConsecutiveChunks != nil && p.tier2ConsecutiveChunks[e.a.Realm()] >= p.pacedTier2ConsecutiveMax {
		if st.Tier == "Tier 2: 72h临期清仓" {
			st.InBreathing = true
		}
	}

	return st
}

// rateLimitedModelsLocked 构建单账号的限额台账行，从 modelCooldowns 遍历输出——
// 每模型一行（含该模型的独立 until + 上游原始 resetAt），多模型同时 6004 全部展示。
// 有效期判据 = 该模型的独立冷却 until 未过；随到期自然消失（与 /status 观感一致）。
// 无模型级冷却（普通软冷却/硬冷却）→ nil（零回归）。调用方必须已持有 p.mu。
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// 先排序模型名，保证 /status 输出稳定（map 遍历无序）。
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			kind := "rate_limit"
			if strings.HasPrefix(mc.Reason, "11102") {
				kind = "model_unavailable"
			}
			row := RateLimitedModel{
				Model:  m,
				Kind:   kind,
				Until:  mc.Until,
				Reason: mc.Reason,
			}
			// 上游原始重置墙钟（台账 ResetAt）**始终透出**：issue #36 的产品意图是
			// "运维按真实恢复时刻观察"，故只要上游给了时间就必须可见。
			//
			// 历史坑：此前写成 `!mc.ResetAt.Equal(mc.Until)` 才输出，试图在"未被
			// soft_rate_max 截断"（此时 Until==ResetAt）时省掉一列冗余。但 6004 带解析
			// 时间的场景**绝大多数都没被截断**（上游给的重置点通常就在几十分钟内，
			// 远小于默认 2h 封顶），于是该字段在最常见的情况下**恒为空**，产品意图落空；
			// 且两个既有测试（截断场景 TestRateLimitedModelsRetainsUncappedResetAt、
			// 未截断场景 TestRateLimitedModelsInStatus）都要求它非空，实现与之矛盾。
			// 现改为有值即输出 —— Until 表示"本网关实际冷却到何时"（可能被截断），
			// ResetAt 表示"上游说何时恢复"，两者语义不同，都值得透出。
			if !mc.ResetAt.IsZero() {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// NoteDailyMark 记录"某类每日动作已在 day（CST 自然日）完成"（持久化，重启后仍有效）。
// 用于给会消耗消耗品/真实额度的排程动作加客户端幂等（第十轮审计 F2/F8/F9）：
// 补签卡、连登兑换与补偿、国际版每日活跃等。调用方传 cstime.Today() 作为 day。
func (p *Pool) NoteDailyMark(uid, key, day string) {
	if uid == "" || key == "" || day == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e != nil {
		if e.dailyMarks == nil {
			e.dailyMarks = map[string]string{}
		}
		e.dailyMarks[key] = day
		p.dirty.Store(true) // 立即置脏，让落盘循环尽快持久化（崩溃窗口最小化）
	}
}

// DailyMark 返回某账号某类每日动作最近一次完成的 CST 日期（无记录返回 ""）。
func (p *Pool) DailyMark(uid, key string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok && e != nil {
		return e.dailyMarks[key]
	}
	return ""
}

// DailyMarkDoneToday 报告某账号某类每日动作今天（CST）是否已完成。
func (p *Pool) DailyMarkDoneToday(uid, key string) bool {
	return p.DailyMark(uid, key) == cstime.Today()
}

// Pause 暂停选号（上游 fd835df 移植）：账号退出选号候选，但**照常**参与保号任务
// （签到 / 活跃上报 / 保活 / 余额刷新 / 旅行 / 成长任务），唯一跳过的是夜猫子
// （RunNightChats 逐条真实 glm-5.2 对话，与"让位防风控"正面冲突，见 b1a2284）。
// 与 Disable 的关键区别：不碰 until/coolKind/reason/softStreak/modelCooldowns，
// 因此暂停不会清掉既有冷却，也不会阻止冷却自行到期。
// 返回 false 表示 uid 不存在。
func (p *Pool) Pause(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.disabled {
		// 禁用是更强终态：不允许叠加 paused，否则会产生"已解冻却永不被选"的幽灵号
		// （ReviveDisabled 不清 paused —— 审查 B 的 P2-1）。
		return false
	}
	pauseLocked(e, p)
	return true
}

// Resume 恢复选号（幂等）。
func (p *Pool) Resume(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	resumeLocked(e, p)
	return true
}

func pauseLocked(e *entry, p *Pool) {
	e.paused = true
	// 与 Disable/冷却一致：把该号的活跃分块作废，否则面板会长期显示"已暂停选号 + 主刷中"，
	// 且分块退休会走"正常交棒"分支、写 lastChunkFinishedUID 并改 tier2ConsecutiveChunks
	// （审查 B 的 P1-3）。
	p.invalidateChunkLocked(e.a.UID)
	p.dirty.Store(true)
}

func resumeLocked(e *entry, p *Pool) {
	e.paused = false
	p.dirty.Store(true)
}
