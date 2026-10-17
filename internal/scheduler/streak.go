// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"encoding/json"
	"log"
	"time"
	"workbuddy2api/internal/gosafe"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/cstime"
	"workbuddy2api/internal/logfmt"
)

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// force 可选：为 true 时强制执行，忽略 CheckinDisabled 开关。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isCheckinDisabled() {
		return
	}
	// 并发保护（第十轮审计 F2）：连登兑换/抽奖/补签都会真实消耗资源，而此前**无任何互斥** ——
	// 面板连点两次、或手动与定时撞车，就会有两个协程给同一批账号并发跑完整套。
	if !s.streakMu.TryLock() {
		// 拿不到锁**不能就这么算了**（第十二轮审计 F7）：TryLock 是"立即返回不排队"，
		// 若同一天的 09:00 与 21:00 两次触发都撞上正在跑的长任务（手动全量签到几百账号
		// 可跑数分钟），当天的连登兑换/补签/抽奖就全丢了 —— daily_chat 有持久化标记可自愈，
		// streak 没有。这里安排一次"空闲后补跑"兜底。
		log.Printf("WARN: streak-bonus 已有一次在跑，本次跳过（将安排空闲后补跑，避免当天漏跑）")
		s.scheduleStreakCatchUp(isForce)
		return
	}
	defer s.streakMu.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		s.streakBonusAccount(a)
	}
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
func (s *Scheduler) streakBonusAccount(a *auth.Auth) {
	// 0. 补签保连登：昨日漏签且有补签卡则补上（连续天数一断就要重攒 7 天）。
	s.makeupYesterday(a)
	// 0.5 礼包/补偿（每号一次，无则业务错误静默跳过）。
	// 幂等（第十轮审计 F8）：旧实现**每次运行都无条件调用** —— 一天至少 2 次（9/21 点）
	// 外加每次重启与每次手动触发，上游若不严格幂等就是重复领取。这里在**成功领取后**
	// 置持久化标记（`gift_ok`/`comp_ok`，永不再试）；失败不置位，保留重试能力。
	// ⚠️ 只有**真的领到**（credit > 0）才置永久标记（第十一轮审计 F2）：
	// ClaimGift/ClaimCompensation 的成功判据只是 err == nil，而 `{"code":0}` 无 data、
	// 或 data 形状漂移导致 Unmarshal 失败（代码自己 WARN"按 0 处理"）都会得到
	// credit=0, err=nil —— 那时若置位"永不再试"，一次解析抖动就**永久丢掉该账号的礼包**。
	if s.cfg.Pool.DailyMark(a.UID, "gift_ok") == "" {
		if credit, err := s.cfg.Upstream.ClaimGift(a); err == nil && credit > 0 {
			s.cfg.Pool.NoteDailyMark(a.UID, "gift_ok", cstime.Today())
			log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc", logfmt.Label(a.UID, a.Nickname), credit)
		}
	}
	if s.cfg.Pool.DailyMark(a.UID, "comp_ok") == "" {
		if credit, err := s.cfg.Upstream.ClaimCompensation(a); err == nil && credit > 0 {
			s.cfg.Pool.NoteDailyMark(a.UID, "comp_ok", cstime.Today())
			log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc", logfmt.Label(a.UID, a.Nickname), credit)
		}
	}

	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		log.Printf("streak-bonus %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := s.cfg.Upstream.GrowthRedeemTier(a, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("streak-bonus %s: redeem %s: %v", logfmt.Label(a.UID, a.Nickname), tier.Tier, err)
			continue
		}
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			a.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := s.cfg.Upstream.LotteryChances(a)
	if err != nil {
		log.Printf("streak-bonus %s: lottery summary: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	for i := 0; i < chances; i++ {
		raw, err := s.cfg.Upstream.LotteryDraw(a)
		if err != nil {
			log.Printf("streak-bonus %s: draw: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", logfmt.Label(a.UID, a.Nickname), i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", logfmt.Label(a.UID, a.Nickname), chances)
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 无卡 / 无漏签 / 查询失败均静默（不影响主流程）。
func (s *Scheduler) makeupYesterday(a *auth.Auth) {
	// **消耗品幂等**（第十轮审计 F2）：补签卡是消耗品，而此前零客户端去重 ——
	// 启动自动签到、每日 9/21 点、面板"全部签到"、重试/重启都会走到这里，
	// 且写上游成功与"记录已完成"之间被中断就会重复补一次。
	// 现按 CST 自然日持久化标记（stateAccount.DailyMarks，重启后仍有效）。
	if s.cfg.Pool.DailyMarkDoneToday(a.UID, "makeup") {
		return
	}
	missed, err := s.cfg.Upstream.HeatmapYesterdayMissed(a)
	if err != nil || !missed {
		return
	}
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	yesterday := cstime.Yesterday() // 必须是**上游自然日**的昨天：本地时区错一天会把补签卡用在错误日期
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("streak-bonus %s: 补签 %s 失败: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return
	}
	// 先记标记再打日志：即使随后进程被杀，重启也不会重复消耗补签卡。
	s.cfg.Pool.NoteDailyMark(a.UID, "makeup", cstime.Today())
	log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", logfmt.Label(a.UID, a.Nickname), yesterday)
}

// streakCatchUpStep / streakCatchUpTries：补跑节奏（每 3 分钟试一次，最多 30 分钟）。
// 只在本轮"因撞锁被跳过"时武装，且同一时刻只允许一个补跑者在排队。
const (
	streakCatchUpStep  = 3 * time.Minute
	streakCatchUpTries = 10
)

// scheduleStreakCatchUp 安排一次"锁空闲后补跑连登任务"（第十二轮审计 F7）。
//
// 语义：这不是"重试失败的请求"，而是"补上被并发挤掉的那一轮" —— 内容与正常一轮完全一致
// （各自幂等：locked/无次数自动跳过），因此多跑一次的代价只是少量只读请求。
// 进程退出即随之结束（不落盘、不跨重启；跨重启的补跑由启动巡检兜住）。
func (s *Scheduler) scheduleStreakCatchUp(force bool) {
	s.retryMu.Lock()
	if s.streakCatchUpArmed {
		s.retryMu.Unlock()
		return
	}
	s.streakCatchUpArmed = true
	s.retryMu.Unlock()

	go gosafe.Do("streak catch-up", func() {
		defer func() {
			s.retryMu.Lock()
			s.streakCatchUpArmed = false
			s.retryMu.Unlock()
		}()
		for i := 0; i < streakCatchUpTries; i++ {
			time.Sleep(streakCatchUpStep)
			if !s.streakMu.TryLock() {
				continue // 还占着，下一轮再试
			}
			s.streakMu.Unlock()
			log.Printf("streak-bonus: 补跑此前被挤掉的一轮（第 %d 次探测后空闲）", i+1)
			s.RunStreakBonusNow(force)
			return
		}
		log.Printf("WARN: streak-bonus 补跑放弃（%v 内始终被占用）—— 当天连登任务可能未执行",
			time.Duration(streakCatchUpTries)*streakCatchUpStep)
	})
}
