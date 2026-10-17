package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPhase3TierSelection 验证 5 级临期梯队的严格优先级：
// Tier 1 (24h) > Tier 2 (72h) > Tier 3 (7d) > Tier 4 (10d) > Tier 5 (底仓 >10d)。
func TestPhase3TierSelection(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")

	p.Add(&auth.Auth{UID: "crit24"})
	p.Add(&auth.Auth{UID: "urg72"})
	p.Add(&auth.Auth{UID: "near7d"})
	p.Add(&auth.Auth{UID: "trans10d"})
	p.Add(&auth.Auth{UID: "safe"})

	// crit24: 12h 到期, 24h 临期积分 500
	p.UpdateResourceDetail("crit24", 500, now.Add(12*time.Hour), 500, 500)
	// urg72: 48h 到期, 72h 临期积分 1000
	p.UpdateResourceDetail("urg72", 1000, now.Add(48*time.Hour), 0, 1000)
	// near7d: 5 天到期
	p.UpdateResourceDetail("near7d", 2000, now.Add(5*24*time.Hour), 0, 0)
	// trans10d: 8.5 天到期
	p.UpdateResourceDetail("trans10d", 3000, now.Add(8*24*time.Hour+12*time.Hour), 0, 0)
	// safe: 30 天到期
	p.UpdateResourceDetail("safe", 5000, now.Add(30*24*time.Hour), 0, 0)

	// 1. 最高优先级：Tier 1 (24h) 必须当选
	got := p.Pick()
	if got == nil || got.UID != "crit24" {
		t.Fatalf("expected crit24 from Tier 1, got %v", got)
	}

	// 2. 禁用 crit24 后，Tier 2 (72h) 必须当选
	p.Disable("crit24", "test")
	got = p.Pick()
	if got == nil || got.UID != "urg72" {
		t.Fatalf("expected urg72 from Tier 2, got %v", got)
	}

	// 3. 禁用 urg72 后，Tier 3 (7d) 必须当选
	p.Disable("urg72", "test")
	got = p.Pick()
	if got == nil || got.UID != "near7d" {
		t.Fatalf("expected near7d from Tier 3, got %v", got)
	}

	// 4. 禁用 near7d 后，Tier 4 (10d) 必须当选
	p.Disable("near7d", "test")
	got = p.Pick()
	if got == nil || got.UID != "trans10d" {
		t.Fatalf("expected trans10d from Tier 4, got %v", got)
	}

	// 5. 禁用 trans10d 后，Tier 5 (safe) 当选
	p.Disable("trans10d", "test")
	got = p.Pick()
	if got == nil || got.UID != "safe" {
		t.Fatalf("expected safe from Tier 5, got %v", got)
	}
}

// TestPhase3Urgent24hHandoff 验证 24 小时临期积分接力消费：
// "在到期24小时积分消耗完时优先消耗其他账户的24小时积分"
func TestPhase3Urgent24hHandoff(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits:   25.0,
		CriticalExpireHours: 48.0,
		UrgentExpireHours:   72.0,
		NearExpireDays:      7.0,
		SafeReserveDays:     10.0,
		LowCreditCutoff:     800.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	p.Add(&auth.Auth{UID: "acct_a"})
	p.Add(&auth.Auth{UID: "acct_b"})
	p.Add(&auth.Auth{UID: "safe_reserve"})

	// acct_a: 20h到期，24h临期积分 20 credit，总额 1000
	p.UpdateResourceDetail("acct_a", 1000, now.Add(20*time.Hour), 20, 20)
	// acct_b: 18h到期，24h临期积分 30 credit，总额 2000 (18h更临期，到期日紧迫度与负重更高)
	p.UpdateResourceDetail("acct_b", 2000, now.Add(18*time.Hour), 30, 30)
	// safe_reserve: 30天到期，总额 5000
	p.UpdateResourceDetail("safe_reserve", 5000, now.Add(30*24*time.Hour), 0, 0)

	// 首选应该是 acct_b（18h 比 20h 更临期，且负重更高）
	pick1 := p.Pick()
	if pick1 == nil || pick1.UID != "acct_b" {
		t.Fatalf("expected acct_b to be picked first, got %v", pick1)
	}

	// 消费 20 credit：acct_b 还有 10 credit 的 24h 额度，仍在分块内（分块额度25，且24h仍有余量）
	p.NoteUsage("acct_b", 20.0, 1000)
	pick2 := p.Pick()
	if pick2 == nil || pick2.UID != "acct_b" {
		t.Fatalf("expected acct_b to stay in active chunk, got %v", pick2)
	}

	// 再消费 10 credit：acct_b 的 24h 临期额度彻底归零 (urgent24hRemain = 0)
	p.NoteUsage("acct_b", 10.0, 500)

	// 核心断言：acct_b 的 24h 临期积分耗尽，虽然 acct_b 总积分仍有 1970，
	// 但必须立即跨号接力切换到依然持有 24h 临期积分的 acct_a！
	pick3 := p.Pick()
	if pick3 == nil || pick3.UID != "acct_a" {
		t.Fatalf("urgent 24h handoff failed: expected acct_a with remaining 24h credits, got %v", pick3)
	}

	// 消费 acct_a 的 20 credit 临期积分
	p.NoteUsage("acct_a", 20.0, 1000)

	// acct_a 与 acct_b 的 24h 临期积分均已耗尽，此时才能降级到底仓层 safe_reserve
	pick4 := p.Pick()
	if pick4 == nil || pick4.UID != "safe_reserve" {
		t.Fatalf("expected safe_reserve after all 24h credits exhausted, got %v", pick4)
	}
}

// TestPhase3SafeReserve800CutoffAndSmoothDegrade 验证 800 积分断崖保护与全池平滑降级：
// 底仓层中，>=800 账号优先被消耗，<800 账号作为应急储备受到保护。
// 当全员均 <800 时，平滑降级，不阻断请求。
func TestPhase3SafeReserve800CutoffAndSmoothDegrade(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		LowCreditCutoff: 800.0,
	})

	p.Add(&auth.Auth{UID: "low1"})
	p.Add(&auth.Auth{UID: "low2"})
	p.Add(&auth.Auth{UID: "high"})

	p.UpdateResourceDetail("low1", 300, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("low2", 600, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("high", 1500, now.Add(30*24*time.Hour), 0, 0)

	// 1. 存在 >=800 积分账号时，必须命中 high，low1 和 low2 受保护
	for i := 0; i < 20; i++ {
		got := p.Pick()
		if got == nil || got.UID != "high" {
			t.Fatalf("expected high (>=800), got %v", got)
		}
	}

	// 2. 将 high 的余额扣减至低于 800（如 500）
	p.SetCredits("high", 500)

	// 此时全池全员低于 800 (low1: 300, low2: 600, high: 500)
	// 验证平滑降级：服务不中断，加权选取（high与low2均有机会）
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("smooth degrade failed: Pick returned nil when all accounts below cutoff")
		}
		seen[got.UID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("smooth degrade should allow multiple low-credit accounts, seen=%v", seen)
	}
}

// TestPhase3ExpiringRescueOverrides800Cutoff 验证临期抢救压倒 800 积分断崖：
// 即将过期的积分（无论总额多小，如 100 credits）必须全力优先消耗，不因低于 800 而被封锁。
func TestPhase3ExpiringRescueOverrides800Cutoff(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		LowCreditCutoff: 800.0,
	})

	p.Add(&auth.Auth{UID: "expiring_low"})
	p.Add(&auth.Auth{UID: "safe_high"})

	// expiring_low: 18h 后到期，但只有 100 积分 (< 800 断崖)
	p.UpdateResourceDetail("expiring_low", 100, now.Add(18*time.Hour), 100, 100)
	// safe_high: 30 天后到期，有 5000 积分 (>= 800)
	p.UpdateResourceDetail("safe_high", 5000, now.Add(30*24*time.Hour), 0, 0)

	// 临期抢救压倒断崖：必须命中 expiring_low！
	for i := 0; i < 20; i++ {
		got := p.Pick()
		if got == nil || got.UID != "expiring_low" {
			t.Fatalf("expiring rescue should override 800 cutoff: want expiring_low, got %v", got)
		}
	}
}

// TestPhase3ChunkingLifecycle 验证 25 credit 单账号分块粘性与额度消耗轮转：
func TestPhase3ChunkingLifecycle(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})

	p.Add(&auth.Auth{UID: "c1"})
	p.Add(&auth.Auth{UID: "c2"})

	p.UpdateResourceDetail("c1", 1000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("c2", 1000, now.Add(30*24*time.Hour), 0, 0)

	// 第一次选出优胜者作为 active chunk
	first := p.Pick()
	if first == nil {
		t.Fatal("pick failed")
	}
	winnerUID := first.UID

	// 分块生效中：多次 Pick 必须稳定返回同一账号
	for i := 0; i < 10; i++ {
		got := p.Pick()
		if got == nil || got.UID != winnerUID {
			t.Fatalf("chunking sticky broken at turn %d: want %s, got %v", i, winnerUID, got)
		}
	}

	// 消耗 15 credits (累积 15 < 25)
	p.NoteUsage(winnerUID, 15.0, 1000)
	got := p.Pick()
	if got == nil || got.UID != winnerUID {
		t.Fatalf("chunk should still be active at 15 credits: want %s, got %v", winnerUID, got)
	}

	// 再消耗 10.5 credits (累积 25.5 >= 25) -> 分块结束！
	p.NoteUsage(winnerUID, 10.5, 500)

	// 此时分块已结束，检查池状态中活跃分块已被清除
	p.mu.RLock()
	chunk := p.activeChunks[""]
	p.mu.RUnlock()
	if chunk != nil {
		t.Fatal("chunk should be deleted after exceeding 25 credits")
	}
}

// TestPhase3InFlightBorrowingDoesNotBreakChunk 验证在途打满时的临时借号不破坏活跃分块：
func TestPhase3InFlightBorrowingDoesNotBreakChunk(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetMaxInFlight(2)
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	p.Add(&auth.Auth{UID: "primary"})
	p.Add(&auth.Auth{UID: "backup"})

	p.UpdateResourceDetail("primary", 5000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("backup", 3000, now.Add(30*24*time.Hour), 0, 0)

	// 建立 primary 为 active chunk
	got := p.Pick()
	if got == nil || got.UID != "primary" {
		t.Fatalf("want primary, got %v", got)
	}

	// 模拟并发打满 primary 的 2 个在途名额
	p.Acquire("primary")
	p.Acquire("primary")

	// 第 3 个请求到来：primary 在途已满，临时借用 backup，但不重置活跃分块
	borrowed := p.Pick()
	if borrowed == nil || borrowed.UID != "backup" {
		t.Fatalf("in-flight full should borrow backup, got %v", borrowed)
	}

	// 检查 primary 依然是 active chunk
	p.mu.RLock()
	chunk := p.activeChunks[""]
	p.mu.RUnlock()
	if chunk == nil || chunk.uid != "primary" {
		t.Fatalf("active chunk should remain primary, got %v", chunk)
	}

	// 释放 primary 的租约
	p.Release("primary")
	p.Release("primary")

	// 在途释放后，后续请求继续回到 primary 消费分块
	resumed := p.Pick()
	if resumed == nil || resumed.UID != "primary" {
		t.Fatalf("after releasing in-flight, should resume primary chunk, got %v", resumed)
	}
}

// TestExpiryWeightingAndUrgencyBonus 验证到期日紧迫度权重加成与超线性时间衰减：
// 即便远期账号积分更多，更早到期的账号依然能凭借时间紧迫度加权脱颖而出。
func TestExpiryWeightingAndUrgencyBonus(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		NearExpireDays:      7.0,
		SafeReserveDays:     10.0,
		ExpireUrgencyWeight: 15.0,
		ExpirePower:         1.5,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	p.Add(&auth.Auth{UID: "acct_far"})
	p.Add(&auth.Auth{UID: "acct_near"})

	// acct_far: 6 天后到期，积分 3000
	p.UpdateResourceDetail("acct_far", 3000, now.Add(6*24*time.Hour), 0, 0)
	// acct_near: 3.5 天后到期，积分仅 1000
	p.UpdateResourceDetail("acct_near", 1000, now.Add(84*time.Hour), 0, 0)

	// acct_near 虽只有 1000 积分（acct_far 的 1/3），但因到期时间更近（3.5d vs 6d），
	// 在 1.5 幂次与 15 分紧迫度加成下，权重超过 acct_far
	got := p.Pick()
	if got == nil || got.UID != "acct_near" {
		t.Fatalf("expected acct_near to win due to expiry urgency, got %v", got)
	}
}

// TestBatonHandoverExemptionForUrgent 验证紧急临期号的分块交棒让渡豁免机制：
// 1. Tier 1 (<=24h) 账号在分块结束后豁免让渡，全力抢救临期积分。
// 2. 账号到期时间显著早于候选账号（差距 >= 24h）时豁免让渡，避免向远期账号无效让渡。
// 3. 候选账号到期时间相近（差距 < 24h）时正常触发让渡。
func TestBatonHandoverExemptionForUrgent(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits:   25.0,
		CriticalExpireHours: 48.0,
		UrgentExpireHours:   72.0,
		NearExpireDays:      7.0,
		SafeReserveDays:     10.0,
		ExpireUrgencyWeight: 15.0,
		ExpirePower:         1.5,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// --- 场景 1：Tier 1 且仍有 24h 额度，豁免让渡 ---
	p.Add(&auth.Auth{UID: "u24_1"})
	p.Add(&auth.Auth{UID: "u24_2"})
	p.UpdateResourceDetail("u24_1", 1000, now.Add(12*time.Hour), 80, 80)
	p.UpdateResourceDetail("u24_2", 1000, now.Add(20*time.Hour), 80, 80)

	// u24_1 当选并完成一个 25 credit 的分块
	got := p.Pick()
	if got == nil || got.UID != "u24_1" {
		t.Fatalf("expected u24_1 first, got %v", got)
	}
	p.NoteUsage("u24_1", 25.0, 1000) // 分块结束，记录 lastChunkFinishedUID = u24_1

	// 因为 u24_1 是 Tier 1 且 urgent24hRemain 仍有 55 credit，触发 Tier 1 临期豁免，继续连选 u24_1
	got2 := p.Pick()
	if got2 == nil || got2.UID != "u24_1" {
		t.Fatalf("expected u24_1 to retain chunk due to Tier 1 handover exemption, got %v", got2)
	}

	// --- 场景 2：到期时间相差 >= 24h，豁免向远期号让渡 ---
	p2 := New("")
	p2.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
		NearExpireDays:    7.0,
		SafeReserveDays:   10.0,
	})
	p2.SetRandomSource(func(n int64) int64 { return 0 })
	p2.Add(&auth.Auth{UID: "near_account"})
	p2.Add(&auth.Auth{UID: "far_account"})
	// near_account: 4 天后到期 (96h)
	p2.UpdateResourceDetail("near_account", 2000, now.Add(4*24*time.Hour), 0, 0)
	// far_account: 6 天后到期 (144h，比 near_account 晚 48h >= 24h)
	p2.UpdateResourceDetail("far_account", 2000, now.Add(6*24*time.Hour), 0, 0)

	// near_account 当选并跑完分块
	g1 := p2.Pick()
	if g1 == nil || g1.UID != "near_account" {
		t.Fatalf("expected near_account, got %v", g1)
	}
	p2.NoteUsage("near_account", 25.0, 1000)

	// 此时 near_account 比 far_account 早到期 48h (>= 24h)，豁免让渡，继续选用 near_account
	g2 := p2.Pick()
	if g2 == nil || g2.UID != "near_account" {
		t.Fatalf("expected near_account to keep serving due to >=24h gap exemption, got %v", g2)
	}

	// --- 场景 3：到期时间相近（差距 < 24h），正常交棒让渡 ---
	p3 := New("")
	p3.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
		NearExpireDays:    7.0,
		SafeReserveDays:   10.0,
	})
	p3.SetRandomSource(func(n int64) int64 { return 0 })
	p3.Add(&auth.Auth{UID: "peer_a"})
	p3.Add(&auth.Auth{UID: "peer_b"})
	// peer_a 4 天后到期，peer_b 4 天加 6 小时后到期（差距仅 6h < 24h）
	p3.UpdateResourceDetail("peer_a", 2000, now.Add(4*24*time.Hour), 0, 0)
	p3.UpdateResourceDetail("peer_b", 2000, now.Add(4*24*time.Hour+6*time.Hour), 0, 0)

	// peer_a 当选并完成分块
	pa1 := p3.Pick()
	if pa1 == nil || pa1.UID != "peer_a" {
		t.Fatalf("expected peer_a, got %v", pa1)
	}
	p3.NoteUsage("peer_a", 25.0, 1000)

	// 两账号到期时间相近，正常让渡给 peer_b
	pa2 := p3.Pick()
	if pa2 == nil || pa2.UID != "peer_b" {
		t.Fatalf("expected handover to peer_b for peer accounts with <24h expiry gap, got %v", pa2)
	}
}

// TestTier1ExpandedTo48Hours 验证 Tier 1 紧急到期层扩充至 48 小时：
// 36 小时（1.5 天）到期的账号被纳入 Tier 1，优先于 60 小时（2.5 天，Tier 2）与远期账号被调度。
func TestTier1ExpandedTo48Hours(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		CriticalExpireHours: 48.0,
		UrgentExpireHours:   72.0,
		NearExpireDays:      7.0,
		SafeReserveDays:     10.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	p.Add(&auth.Auth{UID: "acct_36h"})
	p.Add(&auth.Auth{UID: "acct_60h"})
	p.Add(&auth.Auth{UID: "acct_5d"})

	// acct_36h: 36h到期 (1.5天)，48h临期积分 100
	p.UpdateResourceDetail("acct_36h", 1000, now.Add(36*time.Hour), 100, 100)
	// acct_60h: 60h到期 (2.5天)，72h临期积分 200
	p.UpdateResourceDetail("acct_60h", 2000, now.Add(60*time.Hour), 0, 200)
	// acct_5d: 5天到期，总额 5000
	p.UpdateResourceDetail("acct_5d", 5000, now.Add(5*24*time.Hour), 0, 0)

	// acct_36h 属于 Tier 1 (<=48h)，必须首选
	got := p.Pick()
	if got == nil || got.UID != "acct_36h" {
		t.Fatalf("expected acct_36h in Tier 1 to be picked first, got %v", got)
	}

	// 消耗完 acct_36h 的 100 临期额度
	p.NoteUsage("acct_36h", 100.0, 1000)

	// acct_36h 临期额度耗尽后，瀑布降级到 Tier 2 (acct_60h)
	got2 := p.Pick()
	if got2 == nil || got2.UID != "acct_60h" {
		t.Fatalf("expected acct_60h in Tier 2 to be picked next, got %v", got2)
	}
}

// TestPackagePipelineAdvancement 验证本地多包资产管道（Package Pipeline）特性：
//  1. 支持同时感知多套餐包的余额与不同到期日，并按 Expire 升序排队；
//  2. NoteUsage 消费积分时，优先从最早到期的包划扣；
//  3. 当最早的小额包耗尽时，本地毫秒级自动将 earliestExpire 滑动至下一个未耗尽的包，
//     并实时重算 Tier 1/2 临期额度，使账号在 Tier 1 -> Tier 2 -> Tier 3 优雅瀑布降级，
//     彻底杜绝因过期时间停留在旧包导致的"幽灵坠落"（直接跌入 Tier 5 底仓）问题。
func TestPackagePipelineAdvancement(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		CriticalExpireHours: 48.0,
		UrgentExpireHours:   72.0,
		NearExpireDays:      7.0,
		SafeReserveDays:     10.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	p.Add(&auth.Auth{UID: "pipeline_acct"})
	p.Add(&auth.Auth{UID: "safe_reserve"})

	// safe_reserve: 30天后到期，底仓 10000 积分
	p.UpdateResourceDetail("safe_reserve", 10000, now.Add(30*24*time.Hour), 0, 0)

	// pipeline_acct 包含 3 个套餐包：
	// 包 1: 3 credit，32h 到期 (Tier 1, <=48h)
	// 包 2: 100 credit，68h 到期 (Tier 2, <=72h)
	// 包 3: 4055 credit，127h 到期 (~5.3天，Tier 3, 72h~7d)
	// 总额 4158 credit
	exp1 := now.Add(32 * time.Hour)
	exp2 := now.Add(68 * time.Hour)
	exp3 := now.Add(127 * time.Hour)

	pkgs := []PackageItem{
		{Remain: 3, Expire: exp1},
		{Remain: 100, Expire: exp2},
		{Remain: 4055, Expire: exp3},
	}
	p.UpdateResourceDetail("pipeline_acct", 4158, exp1, 3, 103, pkgs...)

	st, _ := p.Status("pipeline_acct")
	if st.Urgent24hRemain != 3 || st.Urgent72hRemain != 103 {
		t.Fatalf("unexpected urgent remain: 24h=%d, 72h=%d", st.Urgent24hRemain, st.Urgent72hRemain)
	}
	if !st.EarliestExpire.Equal(exp1) {
		t.Fatalf("expected earliestExpire=%v, got %v", exp1, st.EarliestExpire)
	}

	// 阶段 1: 账号处于 Tier 1 (48h 临期，包 1 有 3 credit)
	pick1 := p.Pick()
	if pick1 == nil || pick1.UID != "pipeline_acct" {
		t.Fatalf("expected pipeline_acct in Tier 1 to be picked first, got %v", pick1)
	}

	// 阶段 2: 消费 3 credit，包 1 耗尽
	p.NoteUsage("pipeline_acct", 3.0, 100)

	st2, _ := p.Status("pipeline_acct")
	if st2.Urgent24hRemain != 0 {
		t.Fatalf("expected Urgent24hRemain=0 after consuming package 1, got %d", st2.Urgent24hRemain)
	}
	if st2.Urgent72hRemain != 100 {
		t.Fatalf("expected Urgent72hRemain=100 (package 2), got %d", st2.Urgent72hRemain)
	}
	// 核心断言：earliestExpire 自动前移滑动至包 2 (68h)！
	if !st2.EarliestExpire.Equal(exp2) {
		t.Fatalf("expected earliestExpire to advance to package 2 (%v), got %v", exp2, st2.EarliestExpire)
	}

	// 下一次 Pick: 账号优雅降级到 Tier 2 (72h 临期)，依然优先于 safe_reserve (Tier 5)
	pick2 := p.Pick()
	if pick2 == nil || pick2.UID != "pipeline_acct" {
		t.Fatalf("expected pipeline_acct in Tier 2 to be picked, got %v", pick2)
	}

	// 阶段 3: 消费 100 credit，包 2 耗尽
	p.NoteUsage("pipeline_acct", 100.0, 500)

	st3, _ := p.Status("pipeline_acct")
	if st3.Urgent24hRemain != 0 || st3.Urgent72hRemain != 0 {
		t.Fatalf("expected all urgent counters=0, got 24h=%d, 72h=%d", st3.Urgent24hRemain, st3.Urgent72hRemain)
	}
	// 核心断言：earliestExpire 自动前移滑动至包 3 (127h ~ 5.3天)！
	if !st3.EarliestExpire.Equal(exp3) {
		t.Fatalf("expected earliestExpire to advance to package 3 (%v), got %v", exp3, st3.EarliestExpire)
	}

	// 核心断言：账号顺利进入 Tier 3 (近期临期层 72h~7d)！彻底杜绝幽灵坠落到底仓！
	pick3 := p.Pick()
	if pick3 == nil || pick3.UID != "pipeline_acct" {
		t.Fatalf("expected pipeline_acct in Tier 3 to be picked (avoid ghost falling!), got %v", pick3)
	}

	// 阶段 4: 消费完剩下的 4055 credit
	p.NoteUsage("pipeline_acct", 4055.0, 1000)
	st4, _ := p.Status("pipeline_acct")
	if st4.Credits != 0 {
		t.Fatalf("expected 0 credits remaining, got %d", st4.Credits)
	}
	if len(st4.Packages) != 0 {
		t.Fatalf("expected 0 packages remaining, got %d", len(st4.Packages))
	}

	// 此时 pipeline_acct 耗尽，只能选中 safe_reserve
	pick4 := p.Pick()
	if pick4 == nil || pick4.UID != "safe_reserve" {
		t.Fatalf("expected safe_reserve after pipeline_acct exhausted, got %v", pick4)
	}
}

// TestPackagePipelinePersistence 验证多包管道状态的落盘与恢复：
// 重启加载 state.json 后，套餐包管道与滑动进度完整保留。
func TestPackagePipelinePersistence(t *testing.T) {
	withNoPickGap(t)
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	now := time.Now()

	p1 := New(stateFp)
	p1.Add(&auth.Auth{UID: "persist_bot"})

	exp1 := now.Add(30 * time.Hour)
	exp2 := now.Add(60 * time.Hour)
	p1.UpdateResourceDetail("persist_bot", 500, exp1, 200, 500,
		PackageItem{Remain: 200, Expire: exp1},
		PackageItem{Remain: 300, Expire: exp2},
	)

	// 消费 200 credit，使首包耗尽并滑动至次包
	p1.NoteUsage("persist_bot", 200.0, 100)
	p1.Flush()

	// 重新构建 Pool 模拟重启
	p2 := New(stateFp)
	p2.Add(&auth.Auth{UID: "persist_bot"})

	st, _ := p2.Status("persist_bot")
	if st.Credits != 300 {
		t.Fatalf("expected credits=300 after reload, got %d", st.Credits)
	}
	if len(st.Packages) != 1 {
		t.Fatalf("expected 1 package remaining after reload, got %d", len(st.Packages))
	}
	if !st.EarliestExpire.Equal(exp2) {
		t.Fatalf("expected earliestExpire=%v after reload, got %v", exp2, st.EarliestExpire)
	}
	if st.Urgent72hRemain != 300 {
		t.Fatalf("expected urgent72hRemain=300 after reload, got %d", st.Urgent72hRemain)
	}
}

// TestPacedTier2BreathingRotation 验证 Tier 2 柔性透气轮转：
// Tier 2（48h~72h）临期账号连续完成 3 个分块后，强制交棒给 Tier 3/4/5 健康账号消费 1 个分块散热透气，
// 透气完成后无缝切回 Tier 2 继续抢救。
func TestPacedTier2BreathingRotation(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits:        25.0,
		CriticalExpireHours:      48.0,
		UrgentExpireHours:        72.0,
		PacedBreathingEnabled:    true,
		PacedTier2ConsecutiveMax: 3,
	})

	p.Add(&auth.Auth{UID: "t2_expiring"})
	p.Add(&auth.Auth{UID: "t3_healthy"})

	// t2_expiring: 60h 到期（Tier 2，48h~72h），1000 积分
	p.UpdateResourceDetail("t2_expiring", 1000, now.Add(60*time.Hour), 0, 1000)
	// t3_healthy: 5 天后到期（Tier 3），2000 积分
	p.UpdateResourceDetail("t3_healthy", 2000, now.Add(5*24*time.Hour), 0, 0)

	// 分块 1：应命中 t2_expiring
	pick1 := p.Pick()
	if pick1 == nil || pick1.UID != "t2_expiring" {
		t.Fatalf("chunk 1 expected t2_expiring, got %v", pick1)
	}
	p.NoteUsage("t2_expiring", 25.0, 100) // 完成分块 1

	// 分块 2：应继续命中 t2_expiring
	pick2 := p.Pick()
	if pick2 == nil || pick2.UID != "t2_expiring" {
		t.Fatalf("chunk 2 expected t2_expiring, got %v", pick2)
	}
	p.NoteUsage("t2_expiring", 25.0, 100) // 完成分块 2

	// 分块 3：应继续命中 t2_expiring
	pick3 := p.Pick()
	if pick3 == nil || pick3.UID != "t2_expiring" {
		t.Fatalf("chunk 3 expected t2_expiring, got %v", pick3)
	}
	p.NoteUsage("t2_expiring", 25.0, 100) // 完成分块 3，达到连续 3 块上限！

	// 透气分块：应触发柔性透气让渡，命中健康号 t3_healthy
	pickBreath := p.Pick()
	if pickBreath == nil || pickBreath.UID != "t3_healthy" {
		t.Fatalf("breathing chunk expected t3_healthy after 3 consecutive t2 chunks, got %v", pickBreath)
	}
	p.NoteUsage("t3_healthy", 25.0, 100) // 健康号完成透气分块，重置连续计数

	// 透气完成后的下一分块：应无缝切回 t2_expiring 继续抢救！
	pickBack := p.Pick()
	if pickBack == nil || pickBack.UID != "t2_expiring" {
		t.Fatalf("resumed chunk expected t2_expiring after breathing, got %v", pickBack)
	}
}

// TestTier1NoBreathing 验证 Tier 1（<=48h 极度紧急）不受透气轮转打断：
// 100% 绝对最高优先级全力抢救，连续服务多个分块绝不主动让渡。
func TestTier1NoBreathing(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits:        25.0,
		CriticalExpireHours:      48.0,
		UrgentExpireHours:        72.0,
		PacedBreathingEnabled:    true,
		PacedTier2ConsecutiveMax: 3,
	})

	p.Add(&auth.Auth{UID: "t1_critical"})
	p.Add(&auth.Auth{UID: "t3_healthy"})

	// t1_critical: 30h 到期（Tier 1，<=48h），1000 积分
	p.UpdateResourceDetail("t1_critical", 1000, now.Add(30*time.Hour), 1000, 1000)
	// t3_healthy: 5 天后到期（Tier 3），2000 积分
	p.UpdateResourceDetail("t3_healthy", 2000, now.Add(5*24*time.Hour), 0, 0)

	// 连续 5 个分块，必须全部命中 t1_critical，不透气！
	for i := 1; i <= 5; i++ {
		pick := p.Pick()
		if pick == nil || pick.UID != "t1_critical" {
			t.Fatalf("chunk %d expected t1_critical (no breathing for Tier 1), got %v", i, pick)
		}
		p.NoteUsage("t1_critical", 25.0, 100)
	}
}
