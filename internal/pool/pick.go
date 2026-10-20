// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域；PickExcluding 传 ""）。
// 注意：模型豁免只进 normal 选号（候选 healthy 判定）；全冷却兜底不参与模型豁免——
// 兜底本来就是在"无任何 direct 可用"时的降级，切模型可用性已在 normal 阶段体现。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// 惰性清理过期的 6004 模型级冷却（map 不无限膨胀；status 只读遍历跳过过期项）。
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
	}
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if p.floorBlockedForRealmModel(e, reqModel, realm, now) {
			continue // 积分保底：触底号不接实测收费模型
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now, realm, reqModel)
	}

	// 0. 强制指定路由账户（Pinned / Forced Routing）：重启后重置，最高优先级直通
	targetUID := p.pinnedUIDLocked(realm)
	if targetUID != "" {
		if pe, ok := p.byUID[targetUID]; ok && realmOK(pe) && (tried == nil || !tried[targetUID]) && !pe.blocked(now) {
			if healthyOf(pe) && !p.inFlightFull(pe) {
				pe.lastUsed = now
				return pe.a
			}
		}
	}

	// Phase 3：主刷分块与临期加权调度
	// 1. 检查是否存在当前 realm 的活跃分块
	if p.chunkQuotaCredits > 0 {
		if chunk, ok := p.activeChunks[realm]; ok && chunk != nil {
			chunkEntry, exists := p.byUID[chunk.uid]
			// 校验分块账号的有效性：账号存在、健康、未超额、未超最大请求轮数、24h临期额度未耗尽
			quota := chunk.creditQuota
			if quota <= 0 {
				quota = p.chunkQuotaCredits
			}
			chunkInvalid := !exists || !chunkEntry.healthy(now) ||
				(quota > 0 && chunk.creditsUsed >= quota) ||
				(chunk.maxReqs > 0 && chunk.requestCount >= chunk.maxReqs) ||
				(chunk.isUrgent24h && chunkEntry.urgent24hRemain <= 0)
			if chunkInvalid {
				// 分块已耗尽或账号失效，清除该分块并记录交棒让渡
				p.retireChunkLocked(realm, chunk.uid)
			} else {
				// 分块尚未耗尽。检查当前请求能否使用该分块账号（未被 tried 排除且在途未满且对该模型健康）
				// 本地补洞（上游没有）：分块路径**直取** activeChunks 里的账号，绕过 cands 过滤；
				// 分块内每笔按 usage 实扣，账号必然在中途跌破保底 —— 必须同判据，否则保底看似
				// 生效实则被这条旁路漏掉。不能用时走下方 borrowCands（分块留给免费模型）。
				canUse := healthyOf(chunkEntry) && (tried == nil || !tried[chunk.uid]) && !p.inFlightFull(chunkEntry) &&
					!p.floorBlockedForRealmModel(chunkEntry, reqModel, realm, now)
				if canUse {
					chunkEntry.lastUsed = now
					return chunkEntry.a
				}
				// 若当前请求无法使用该活跃分块（例如被本次请求的 tried 排除，或在途并发名额已满），
				// 则临时借用其他健康候选账号，但不重置活跃分块
				borrowCands := make([]*entry, 0, len(cands))
				for _, ce := range cands {
					if ce.a.UID != chunk.uid {
						borrowCands = append(borrowCands, ce)
					}
				}
				if len(borrowCands) > 0 {
					winner, _ := p.electWinnerLocked(borrowCands, reqModel, realm, now)
					if winner != nil {
						winner.lastUsed = now
						return winner.a
					}
				}
			}
		}
	}

	// 2. 无活跃分块或分块刚被清除：在所有 cands 中通过 Phase 3 加权体系选出优胜者
	winner, isUrgent24h := p.electWinnerLocked(cands, reqModel, realm, now)
	if winner == nil {
		return nil
	}

	// 若启用了分块调度，将新当选的优胜者注册为该 realm 的活跃分块
	if p.chunkQuotaCredits > 0 {
		quota, maxReqs := p.adaptiveQuotaLocked(reqModel, winner, now)
		p.activeChunks[realm] = &chunkState{
			uid:          winner.a.UID,
			model:        reqModel,
			creditQuota:  quota,
			maxReqs:      maxReqs,
			creditsUsed:  0,
			requestCount: 0,
			isUrgent24h:  isUrgent24h,
			createdAt:    now,
		}
	}

	winner.lastUsed = now
	return winner.a
}

// electWinnerLocked Phase 3 优胜者选举：
//
// 1. 成本分层（免费 > 未知 > 收费）：已确认免费号优先于收费号。
// 2. 临期分层（严格梯队淘汰）：
//   - Tier 1: 紧急到期层（<=48小时）：最高优先级，消耗完后立即跨号接力。
//   - Tier 2: 临期清仓层（<=72小时 / 3天）。
//   - Tier 3: 近期临期层（<=7天）。
//   - Tier 4: 过渡梯队层（<=10天）。
//   - Tier 5: 安全底仓层（>10天或无临期套餐）。在底仓层中应用枯竭断崖保护（800积分）：
//     若存在 >=800 积分的底仓号，则 <800 积分账号受保护不被消耗；
//     若全员均 <800 积分，则平滑降级（所有底仓号均可选，不中断服务）。
//     临期抢救（Tier 1~4）不受 800 积分断崖限制，即将过期的积分无论总额多少均全力抢救！
//
// 2.5 分块交棒让渡（Baton Handover）：当池中有多候选账号时，优先将令牌让渡给其他账号，避免连任垄断。
// 3. 时间负重与额度负重平衡（BurnRate = max(0, credits - 800) / D）+ 三因子积分加权 + Top5 防雪崩随机。
func (p *Pool) electWinnerLocked(cands []*entry, reqModel, realm string, now time.Time) (*entry, bool) {
	if len(cands) == 0 {
		return nil, false
	}

	// 1. 成本分层（免费 Tier 0 > 未知 Tier 1 > 收费 Tier 2）
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	costFiltered := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if ti, _ := costTier(e); ti == bestTier {
			costFiltered = append(costFiltered, e)
		}
	}

	// 2. 划分 5 个临期梯队
	critDur := time.Duration(p.criticalExpireHours) * time.Hour
	if critDur <= 0 {
		critDur = 48 * time.Hour
	}
	urgDur := time.Duration(p.urgentExpireHours) * time.Hour
	if urgDur <= 0 {
		urgDur = 72 * time.Hour
	}
	nearDays := p.nearExpireDays
	if nearDays <= 0 {
		nearDays = 7.0
	}
	safeDays := p.safeReserveDays
	if safeDays <= 0 {
		safeDays = 10.0
	}

	var tier1, tier2, tier3, tier4, tier5 []*entry
	for _, e := range costFiltered {
		if e.urgent24hRemain > 0 {
			tier1 = append(tier1, e)
			continue
		}

		if e.urgent72hRemain > 0 {
			tier2 = append(tier2, e)
			continue
		}

		nearDur := time.Duration(nearDays*24) * time.Hour
		if !e.earliestExpire.IsZero() && e.earliestExpire.After(now.Add(urgDur)) && e.earliestExpire.Before(now.Add(nearDur)) && e.credits > 0 {
			tier3 = append(tier3, e)
			continue
		}

		safeDur := time.Duration(safeDays*24) * time.Hour
		if !e.earliestExpire.IsZero() && e.earliestExpire.After(now.Add(nearDur)) && e.earliestExpire.Before(now.Add(safeDur)) && e.credits > 0 {
			tier4 = append(tier4, e)
			continue
		}

		tier5 = append(tier5, e)
	}

	filterTier5 := func(list []*entry) []*entry {
		cutoff := int64(p.lowCreditCutoff)
		if cutoff <= 0 {
			cutoff = 800
		}
		var aboveCutoff []*entry
		for _, e := range list {
			if e.credits >= cutoff {
				aboveCutoff = append(aboveCutoff, e)
			}
		}
		if len(aboveCutoff) > 0 {
			return aboveCutoff
		}
		return list
	}

	var selectedTier []*entry
	isUrgent24h := false
	tierLevel := 5
	if len(tier1) > 0 {
		selectedTier = tier1
		isUrgent24h = true
		tierLevel = 1
	} else if len(tier2) > 0 {
		isBreathing := false
		maxConsecutive := p.pacedTier2ConsecutiveMax
		if maxConsecutive <= 0 {
			maxConsecutive = defaultPacedTier2ConsecutiveMax
		}
		if p.pacedBreathingEnabled && p.tier2ConsecutiveChunks != nil && p.tier2ConsecutiveChunks[realm] >= maxConsecutive {
			if len(tier3) > 0 || len(tier4) > 0 || len(tier5) > 0 {
				isBreathing = true
			}
		}
		if !isBreathing {
			selectedTier = tier2
			tierLevel = 2
		} else {
			log.Printf("[pool] [%s] Tier 2 连续服务达 %d 个分块，触发柔性透气让渡（降级至健康梯队）", realm, p.tier2ConsecutiveChunks[realm])
			if len(tier3) > 0 {
				selectedTier = tier3
				tierLevel = 3
			} else if len(tier4) > 0 {
				selectedTier = tier4
				tierLevel = 4
			} else {
				selectedTier = filterTier5(tier5)
				tierLevel = 5
			}
		}
	} else if len(tier3) > 0 {
		selectedTier = tier3
		tierLevel = 3
	} else if len(tier4) > 0 {
		selectedTier = tier4
		tierLevel = 4
	} else {
		selectedTier = filterTier5(tier5)
		tierLevel = 5
	}

	// 2.5 分块交棒让渡（Baton Handover）：
	// 如果池中存在多个就绪候选账号，优先避让上一个刚结束分块的账号，确保各账号轮转交替接力。
	// 特殊保护（临期豁免让渡）：如果上一个刚结束分块的账号仍处于紧急临期（Tier 1/Tier 2），
	// 或者其到期时间显著早于同层其他候选（早于 >= 24h），则不强制向远期账号交棒，全力抢救临期积分。
	if len(selectedTier) > 1 && p.lastChunkFinishedUID != nil {
		lastUID := p.lastChunkFinishedUID[realm]
		if lastUID != "" {
			var lastEntry *entry
			for _, e := range selectedTier {
				if e.a.UID == lastUID {
					lastEntry = e
					break
				}
			}
			shouldHandover := true
			if lastEntry != nil {
				// 规则 1：处于 Tier 1 紧急到期层（<=48h）且仍有临期余量，不让渡
				if tierLevel == 1 && lastEntry.urgent24hRemain > 0 {
					shouldHandover = false
				}
				// 规则 2：若 lastEntry 到期时间比同层其他所有就绪候选至少早 24 小时，不让渡
				if shouldHandover && !lastEntry.earliestExpire.IsZero() {
					isSignificantlyEarlier := true
					for _, other := range selectedTier {
						if other.a.UID == lastUID {
							continue
						}
						if other.earliestExpire.IsZero() {
							continue
						}
						// 如果存在到期时间与 lastEntry 差距在 24h 以内的近邻账号，则正常交棒
						if other.earliestExpire.Sub(lastEntry.earliestExpire) < 24*time.Hour {
							isSignificantlyEarlier = false
							break
						}
					}
					if isSignificantlyEarlier {
						shouldHandover = false
					}
				}
			}
			if shouldHandover {
				var handoverTier []*entry
				for _, e := range selectedTier {
					if e.a.UID != lastUID {
						handoverTier = append(handoverTier, e)
					}
				}
				if len(handoverTier) > 0 {
					selectedTier = handoverTier
				}
			}
		}
	}

	// 3. 计算同梯队内的三因子权重与时间/额度负重（同梯队下按 BurnRate 或积分加权）
	var maxCredits int64
	var maxBurnRate float64
	var minDays float64 = -1
	for _, e := range selectedTier {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
		br := p.burnRateForTier(e, tierLevel, now)
		if br > maxBurnRate {
			maxBurnRate = br
		}
		if tierLevel <= 4 && !e.earliestExpire.IsZero() {
			d := e.earliestExpire.Sub(now).Hours() / 24.0
			if d < 0.05 {
				d = 0.05
			}
			if minDays < 0 || d < minDays {
				minDays = d
			}
		}
	}
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(selectedTier))
	for i, e := range selectedTier {
		ws[i] = weighted{e: e, w: p.weightOfTier(e, maxCredits, maxBurnRate, tierLevel, minDays, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		_, ci := costTier(ws[i].e)
		_, cj := costTier(ws[j].e)
		if ci != cj {
			return ci < cj // 同层且收费时：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w // 负重率/积分与三因子加权高的在前
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})

	top := make([]*entry, 0, len(ws))
	for _, c := range ws {
		top = append(top, c.e)
	}
	if len(top) > 5 {
		top = top[:5]
	}

	// 4. 防并发撞号过滤
	eligible := make([]*entry, 0, len(top))
	for _, e := range top {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var winner *entry
	if len(eligible) == 0 {
		winner = top[0]
		for _, c := range top[1:] {
			if c.lastUsed.Before(winner.lastUsed) {
				winner = c
			}
		}
	} else {
		winner = p.pickWeightedTier(eligible, maxCredits, maxBurnRate, tierLevel, minDays)
	}
	return winner, isUrgent24h
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, model string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.blocked(now) {
			continue // 禁用/暂停选号（含未到期的 11140 限制）永不参与兜底
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue // 兜底是最后一道选号路径：保底在它之前挡不住就等于没挡（上游 b98fb4f）
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry uid=%s until=%s kind=%s", logfmt.UID8(best.a.UID), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = p.now()
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读 p.maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// successWeightTerm 成功率项的权重贡献（满分 3.0）：
//
//	term = successCount/(successCount+errTotal) × 3；无样本 → 1.5（中性偏信任）
//
// ⚠️ **不要**把它改成"更平滑"的估计量（Laplace/Add-2 之类）。这看起来更"统计正确"，
// 但作为**调度权重**它是反向优化：实测（12 seed × 300 次 Pick，10 号同分同闲、每次成功回喂）
//
//	原始比率   最大份额 11.1%（均匀基准 10%）
//	Laplace   最大份额 23.3%   ← 集中度翻倍
//
// 机理：原始比率在**首次成功即饱和到 3.0**，所有"成功过的号"权重被拉平，只剩闲置项区分，
// 于是轮转接近均匀；平滑拆掉了这个"饱和均衡器"，让每多一次成功都继续加分 → 先富更富。
// 回归保护见 TestPickConcentrationStaysNearUniform（阈值 18%，两种实现可区分）。
func successWeightTerm(success, errTotal int64) float64 {
	total := success + errTotal
	if total <= 0 {
		return 1.5
	}
	return float64(success) / float64(total) * 3
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// burnRateForTier 计算候选账号在指定梯队的日均消耗负重率（支持超线性时间衰减）。
// Tier 1/2（紧急 48h / 临期 72h 清仓）：按即将到期的额度计算 BurnRate = urgentCredits / (D ^ power)，越临期负重越高。
// Tier 3/4（临期/过渡层）：全额面临到期，BurnRate = credits / (D ^ power)。
// Tier 5（安全底仓层）：计算高于 lowCreditCutoff（默认 800 积分）的富余积分消耗速率：
//
//	BurnRate = max(0, credits - cutoff) / (D ^ power) (天)
//
// 随着账号逼近 800 积分断崖线，富余积分趋近于 0，负重率自然降为 0 实现软着陆保护。
func (p *Pool) burnRateForTier(e *entry, tierLevel int, now time.Time) float64 {
	power := p.expirePower
	if power <= 0 {
		power = defaultExpirePower
	}

	if tierLevel == 1 || tierLevel == 2 {
		credits := float64(e.credits)
		if tierLevel == 1 && e.urgent24hRemain > 0 {
			credits = float64(e.urgent24hRemain)
		} else if tierLevel == 2 && e.urgent72hRemain > 0 {
			credits = float64(e.urgent72hRemain)
		}
		if credits <= 0 {
			return 0
		}
		days := 1.0
		if !e.earliestExpire.IsZero() {
			days = e.earliestExpire.Sub(now).Hours() / 24.0
			if days < 0.05 {
				days = 0.05
			}
		}
		return credits / math.Pow(days, power)
	} else if tierLevel == 3 || tierLevel == 4 {
		if e.credits <= 0 {
			return 0
		}
		days := 7.0
		if !e.earliestExpire.IsZero() {
			days = e.earliestExpire.Sub(now).Hours() / 24.0
			if days < 0.05 {
				days = 0.05
			}
		}
		return float64(e.credits) / math.Pow(days, power)
	} else if tierLevel == 5 {
		cutoff := int64(p.lowCreditCutoff)
		if cutoff <= 0 {
			cutoff = 800
		}
		excess := float64(e.credits - cutoff)
		if excess <= 0 {
			return 0
		}
		safeDays := p.safeReserveDays
		if safeDays <= 0 {
			safeDays = 10.0
		}
		days := 30.0
		if !e.earliestExpire.IsZero() {
			d := e.earliestExpire.Sub(now).Hours() / 24.0
			if d < safeDays {
				d = safeDays
			}
			days = d
		}
		return excess / days
	}
	return 0
}

// weightOfTier 计算单个账号在梯队内的综合权重（含到期时间紧迫度、时间/额度负重、闲置补偿与成功率）。
func (p *Pool) weightOfTier(e *entry, maxCredits int64, maxBurnRate float64, tierLevel int, minDays float64, now time.Time) float64 {
	w := 1.0

	// 1. 积分与时间负重项（满分 10 分）
	if maxBurnRate > 0 {
		br := p.burnRateForTier(e, tierLevel, now)
		w += (br / maxBurnRate) * 10.0
	} else if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10.0
	}

	// 2. 到期日专属紧迫度加权项（满分 expireUrgencyWeight 分，默认 15.0 分）：
	// 仅在临期抢救梯队（Tier 1~4）中生效。安全底仓层（Tier 5）账号均为远期/安全储备，不打紧迫度分。
	// 越逼近到期日，紧迫度得分越高；梯队中最先到期的账号获得满分，其他账号随相对天数超线性衰减。
	if tierLevel <= 4 && minDays > 0 && !e.earliestExpire.IsZero() {
		days := e.earliestExpire.Sub(now).Hours() / 24.0
		if days < 0.05 {
			days = 0.05
		}
		ratio := minDays / days
		if ratio > 1.0 {
			ratio = 1.0
		}
		power := p.expirePower
		if power <= 0 {
			power = defaultExpirePower
		}
		urgencyWeight := p.expireUrgencyWeight
		if urgencyWeight <= 0 {
			urgencyWeight = defaultExpireUrgencyWeight
		}
		w += math.Pow(ratio, power) * urgencyWeight
	}

	// 3. 闲置补偿
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0
		}
		w += idleW
	}

	// 4. 成功率 × 3（与 weightOf 同口径；不要改成平滑估计量，见 successWeightTerm 注释）
	w += successWeightTerm(e.successCount, e.errTotal)
	return w
}

// pickWeightedTier 基于梯队负重与三因子权重的加权随机抽签。
func (p *Pool) pickWeightedTier(cands []*entry, maxCredits int64, maxBurnRate float64, tierLevel int, minDays float64) *entry {
	now := p.now()
	const scale = 1_000_000
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOfTier(e, maxCredits, maxBurnRate, tierLevel, minDays, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// modelCostPer1kLocked 返回指定模型的有效 CostPer1k（千 token 单价）。
// 优先采用 preferred 账号自身的实测观测；若 preferred 无有效观测，
// 则在池内所有账号中查找该模型最近的有效观测（跨账号成本经验共享）。
// 无有效观测或观测已过期（modelCostTTL）返回 ok=false。调用方须持有 p.mu。
func (p *Pool) modelCostPer1kLocked(model string, preferred *entry, now time.Time) (float64, bool) {
	if model == "" {
		return 0, false
	}
	if preferred != nil {
		if mc, ok := preferred.modelCostOf(model, now); ok {
			return mc.CostPer1k, true
		}
	}
	var bestMC modelCostEntry
	var found bool
	for _, e := range p.byUID {
		if mc, ok := e.modelCostOf(model, now); ok {
			if !found || mc.LastSeen.After(bestMC.LastSeen) {
				bestMC = mc
				found = true
			}
		}
	}
	if found {
		return bestMC.CostPer1k, true
	}
	return 0, false
}

// adaptiveQuotaLocked 计算新分块的动态额度配额与最大请求轮数封顶。
// 根据模型千 Token 实测单价（CostPer1k）三档自适应：
// 用户明确策略：最大请求轮数仅针对 0 费模型（CostPer1k <= 0）生效，避免 0 费模型无限霸占单号；
// 任何有真实扣费的模型（CostPer1k > 0），均纯粹由积分配额驱动，不受轮数限制（maxReqs=0），充分保障 Prompt Cache 连续性。
func (p *Pool) adaptiveQuotaLocked(reqModel string, preferred *entry, now time.Time) (float64, int) {
	if !p.adaptiveChunking || reqModel == "" {
		quota := p.chunkQuotaCredits
		if quota <= 0 {
			quota = defaultChunkQuotaCredits
		}
		return quota, 0
	}

	costPer1k, ok := p.modelCostPer1kLocked(reqModel, preferred, now)
	if !ok {
		// 冷启动未知单价模型：采用未知模型探索档（默认 15.0 credit，快速测算单价并防单号过度霸占）
		quota := p.tierUnknownQuota
		if quota <= 0 {
			quota = defaultTierUnknownQuota
		}
		return quota, p.tierUnknownMaxReqs
	}

	// 0 费模型（CostPer1k <= 0，如 hy4-preview 等免费模型）：启用强制退出保护（默认 60 轮）
	if costPer1k <= 0 {
		return p.tierCheapQuota, p.tierCheapMaxReqs
	}

	// 以下均为收费模型（CostPer1k > 0）：轮数不设上限（maxReqs=0），纯由积分配额驱动
	if costPer1k < p.tierCheapCostCutoff {
		return p.tierCheapQuota, 0
	}
	if costPer1k >= p.tierFlagshipCostCutoff {
		return p.tierFlagshipQuota, p.tierFlagshipMaxReqs
	}
	return p.tierNormalQuota, p.tierNormalMaxReqs
}

// floorBlockedForRealmModel 报告账号是否应因「积分保底」被排除在该模型的候选之外。
//
// 两级判据（上游 d19add4 + 39af6b7）：① 本地实测成本（modelCostOf）最权威；
// ② 无实测时用上游目录倍率兜底（modelRateOf，由 main 注入）。任何一级判"收费"即拦；
// 无观测且目录未知 → 放行（未知不惩罚）。保底未开（creditFloor<=0）恒放行。
// 调用方必须已持 p.mu。
func (p *Pool) floorBlockedForRealmModel(e *entry, model, realm string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || e.credits >= p.creditFloor {
		return false
	}
	_ = realm // 本批判据与域无关；保留形参以便下一批接目录倍率（按 realm 查倍率表）
	// ① 实测成本最权威（本地观测到的真实收费）
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	// ② 目录倍率兜底（上游 39af6b7）：尚无实测时用目录价判断，堵住"全池无观测 → 全放行
	//    被高价新模型一笔打穿"。未注入回调 / 目录未覆盖 / 非数值一律放行（未知不惩罚）。
	if p.modelRateOf == nil {
		return false
	}
	rate := p.modelRateOf(realm, model)
	if rate == "" {
		return false
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false
	}
	return v > 0
}
