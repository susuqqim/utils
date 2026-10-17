package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestAdaptiveChunkingCheapTierAndMaxReqsExit 验证极省/免费档（CostPer1k < 0.08）的 25 credit 配额与 60 轮退出保护：
// 即使单次扣费恒为 0 credit（如免费模型 hy4-preview），达到 60 轮后分块必须正常结束并交棒，彻底解决 0 费率死锁霸占。
func TestAdaptiveChunkingCheapTierAndMaxReqsExit(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})

	p.Add(&auth.Auth{UID: "free_bot"})
	p.Add(&auth.Auth{UID: "backup_bot"})
	p.UpdateResourceDetail("free_bot", 2000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("backup_bot", 2000, now.Add(30*24*time.Hour), 0, 0)

	// 记录免费模型的实测成本：CostPer1k = 0.0（两个账号均确认为免费号）
	p.NoteModelCost("free_bot", "hy4-preview", 0.0, 1000)
	p.NoteModelCost("backup_bot", "hy4-preview", 0.0, 1000)

	// 首次按 hy4-preview 选号
	pick1 := p.PickExcludingForModel(nil, "hy4-preview")
	if pick1 == nil {
		t.Fatal("expected a bot to win")
	}
	firstUID := pick1.UID
	otherUID := "backup_bot"
	if firstUID == "backup_bot" {
		otherUID = "free_bot"
	}

	// 检查分块信息：额度应为 25.0，最大轮数应为 60
	chunk, ok := p.ActiveChunk("")
	if !ok {
		t.Fatal("expected active chunk to be established")
	}
	if chunk.CreditQuota != 25.0 {
		t.Fatalf("expected cheap tier credit quota 25.0, got %f", chunk.CreditQuota)
	}
	if chunk.MaxReqs != 60 {
		t.Fatalf("expected cheap tier max reqs 60, got %d", chunk.MaxReqs)
	}

	// 模拟 59 次 0 credit 请求：分块必须保持有效（粘性保护）
	for i := 0; i < 59; i++ {
		p.NoteUsageForModel(firstUID, 0.0, 100, "hy4-preview")
		got := p.PickExcludingForModel(nil, "hy4-preview")
		if got == nil || got.UID != firstUID {
			t.Fatalf("chunk should remain sticky at request %d, got %v", i+1, got)
		}
	}

	// 第 60 次请求：达到 60 轮封顶
	p.NoteUsageForModel(firstUID, 0.0, 100, "hy4-preview")

	// 活跃分块必须已被清除，且记录交棒让渡
	_, stillActive := p.ActiveChunk("")
	if stillActive {
		t.Fatal("chunk should have retired after reaching 60 max reqs")
	}

	// 下一次选号必须交棒让渡给另一个账号！
	next := p.PickExcludingForModel(nil, "hy4-preview")
	if next == nil || next.UID != otherUID {
		t.Fatalf("expected baton handover to %s after 60 reqs, got %v", otherUID, next)
	}
}

// TestAdaptiveChunkingFlagshipTier 验证旗舰/深度推理档（CostPer1k >= 0.80）的 100 credit 配额：
// 覆盖 Claude 3.5 Sonnet 等昂贵模型，支撑长会话连续 Prompt Cache，不频繁换号。
func TestAdaptiveChunkingFlagshipTier(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0, // 基础分块开启，自适应扩展到 100
	})

	p.Add(&auth.Auth{UID: "sonnet_user"})
	p.UpdateResourceDetail("sonnet_user", 5000, now.Add(30*24*time.Hour), 0, 0)

	// 记录 Claude 3.5 Sonnet 的高额单价（例如 2.0 credit / 1k tokens）
	p.NoteModelCost("sonnet_user", "claude-3-5-sonnet", 2.0, 1000)

	got := p.PickExcludingForModel(nil, "claude-3-5-sonnet")
	if got == nil || got.UID != "sonnet_user" {
		t.Fatalf("expected sonnet_user, got %v", got)
	}

	chunk, ok := p.ActiveChunk("")
	if !ok {
		t.Fatal("expected active chunk")
	}
	if chunk.CreditQuota != 100.0 {
		t.Fatalf("expected flagship tier credit quota 100.0, got %f", chunk.CreditQuota)
	}
	// 用户明确策略：最大请求轮数仅针对 0 费模型生效，收费模型不限轮数（MaxReqs == 0）
	if chunk.MaxReqs != 0 {
		t.Fatalf("expected flagship tier max reqs 0 (unlimited for paid models), got %d", chunk.MaxReqs)
	}

	// 消耗 60 credits（已超过普通 25/50 额度，但未达到 100 credit 旗舰额度）
	p.NoteUsageForModel("sonnet_user", 60.0, 30000, "claude-3-5-sonnet")

	// 分块必须依然存续！Prompt Cache 不被打碎！
	got2 := p.PickExcludingForModel(nil, "claude-3-5-sonnet")
	if got2 == nil || got2.UID != "sonnet_user" {
		t.Fatalf("flagship chunk should remain active at 60 credits, got %v", got2)
	}

	// 再消耗 40.5 credits（累计 100.5 >= 100）
	p.NoteUsageForModel("sonnet_user", 40.5, 20000, "claude-3-5-sonnet")

	_, stillActive := p.ActiveChunk("")
	if stillActive {
		t.Fatal("flagship chunk should retire after consuming 100.5 credits")
	}
}

// TestAdaptiveChunkingNormalTier 验证常规主力档（0.08 <= CostPer1k < 0.80）的 50 credit 配额：
// 覆盖 Kimi、GLM 等主流模型，收费模型不限轮数。
func TestAdaptiveChunkingNormalTier(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})

	p.Add(&auth.Auth{UID: "kimi_agent"})
	p.UpdateResourceDetail("kimi_agent", 5000, now.Add(30*24*time.Hour), 0, 0)

	// 记录 Kimi 命中态单价：0.2 credit / 1k tokens
	p.NoteModelCost("kimi_agent", "kimi-k3-1", 0.2, 1000)

	got := p.PickExcludingForModel(nil, "kimi-k3-1")
	if got == nil || got.UID != "kimi_agent" {
		t.Fatalf("expected kimi_agent, got %v", got)
	}

	chunk, ok := p.ActiveChunk("")
	if !ok {
		t.Fatal("expected active chunk")
	}
	if chunk.CreditQuota != 50.0 {
		t.Fatalf("expected normal tier credit quota 50.0, got %f", chunk.CreditQuota)
	}
	// 收费模型不限轮数（MaxReqs == 0）
	if chunk.MaxReqs != 0 {
		t.Fatalf("expected normal tier max reqs 0 (unlimited for paid models), got %d", chunk.MaxReqs)
	}

	// 消耗 35 credits（未达 50）
	p.NoteUsageForModel("kimi_agent", 35.0, 15000, "kimi-k3-1")
	got2 := p.PickExcludingForModel(nil, "kimi-k3-1")
	if got2 == nil || got2.UID != "kimi_agent" {
		t.Fatalf("chunk should remain active at 35 credits, got %v", got2)
	}

	// 再消耗 16 credits（累计 51 >= 50）
	p.NoteUsageForModel("kimi_agent", 16.0, 10000, "kimi-k3-1")
	_, stillActive := p.ActiveChunk("")
	if stillActive {
		t.Fatal("normal chunk should retire after exceeding 50 credits")
	}
}

// TestAdaptiveChunkingPaidCheapModelNotCappedByReqs 验证极省但收费的模型（如 0 < CostPer1k < 0.08）
// 绝不被 60 轮封顶提前退出，只有当 25 credit 额度耗尽才退出换号。
func TestAdaptiveChunkingPaidCheapModelNotCappedByReqs(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})

	p.Add(&auth.Auth{UID: "cheap_paid_bot"})
	p.UpdateResourceDetail("cheap_paid_bot", 3000, now.Add(30*24*time.Hour), 0, 0)

	// 单价为 0.02 credit/1k tokens（收费模型，但属于极省档 < 0.08）
	p.NoteModelCost("cheap_paid_bot", "deepseek-flash", 0.02, 1000)

	got := p.PickExcludingForModel(nil, "deepseek-flash")
	if got == nil || got.UID != "cheap_paid_bot" {
		t.Fatalf("expected cheap_paid_bot, got %v", got)
	}

	chunk, ok := p.ActiveChunk("")
	if !ok {
		t.Fatal("expected active chunk")
	}
	if chunk.CreditQuota != 25.0 {
		t.Fatalf("expected credit quota 25.0, got %f", chunk.CreditQuota)
	}
	if chunk.MaxReqs != 0 {
		t.Fatalf("paid cheap model should have MaxReqs=0 (unlimited), got %d", chunk.MaxReqs)
	}

	// 连续执行 70 次调用（超过 60 轮，每次扣费 0.05 credit，累计 3.5 credits < 25.0）
	for i := 0; i < 70; i++ {
		p.NoteUsageForModel("cheap_paid_bot", 0.05, 1000, "deepseek-flash")
		picked := p.PickExcludingForModel(nil, "deepseek-flash")
		if picked == nil || picked.UID != "cheap_paid_bot" {
			t.Fatalf("paid cheap model should NOT be retired by request count at turn %d, got %v", i+1, picked)
		}
	}

	// 确认在 70 轮后分块依然健康有效（Prompt Cache 完整保留）
	chunkAfter, ok := p.ActiveChunk("")
	if !ok || chunkAfter.UID != "cheap_paid_bot" {
		t.Fatal("chunk should remain active after 70 requests because 3.5 credits < 25.0")
	}

	// 消耗剩余额度至 25.1 credits -> 正常因额度耗尽退出！
	p.NoteUsageForModel("cheap_paid_bot", 21.6, 50000, "deepseek-flash")
	_, stillActive := p.ActiveChunk("")
	if stillActive {
		t.Fatal("chunk should retire after reaching 25.1 credits")
	}
}

// TestBatonHandoverSequence 验证多账号交棒让渡机制：
// A 结束分块后，下一次选举必让渡给 B；B 结束后让渡给 C；杜绝单号连续连任。
func TestBatonHandoverSequence(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
		DisableAdaptive:   true, // 固定 25 credit 测试纯交棒序列
	})

	p.Add(&auth.Auth{UID: "acct_1"})
	p.Add(&auth.Auth{UID: "acct_2"})
	p.Add(&auth.Auth{UID: "acct_3"})

	// 三账号积分相同、到期时间相同
	p.UpdateResourceDetail("acct_1", 3000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("acct_2", 3000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("acct_3", 3000, now.Add(30*24*time.Hour), 0, 0)

	// 第一次选出活跃账号
	first := p.Pick()
	if first == nil {
		t.Fatal("pick failed")
	}
	u1 := first.UID

	// 消耗完 u1 的 25 credit 分块
	p.NoteUsage(u1, 26.0, 1000)

	// 第二次选举：u1 刚结束，交棒机制必须避让 u1，选出另两个号之一
	second := p.Pick()
	if second == nil {
		t.Fatal("second pick failed")
	}
	u2 := second.UID
	if u2 == u1 {
		t.Fatalf("baton handover failed: u1 consecutive re-elected, want other than %s", u1)
	}

	// 消耗完 u2 的分块
	p.NoteUsage(u2, 26.0, 1000)

	// 第三次选举：u2 刚结束，交棒机制必须避让 u2
	third := p.Pick()
	if third == nil {
		t.Fatal("third pick failed")
	}
	u3 := third.UID
	if u3 == u2 {
		t.Fatalf("baton handover failed: u2 consecutive re-elected, want other than %s", u2)
	}
}

// TestSafeReserveBurnRateBalancingAndSoftLanding 验证安全底仓层的时间负重与额度负重平衡（BurnRate）及 800 软着陆：
//   - acct_urgent: 2400 credits, 20 天到期 -> BurnRate = (2400-800)/20 = 80 credit/day
//   - acct_calm:   1800 credits, 50 天到期 -> BurnRate = (1800-800)/50 = 20 credit/day
//
// 1. acct_urgent 的日均负重需求为 80，远高于 acct_calm 的 20，故优先消耗 acct_urgent。
// 2. 当 acct_urgent 消耗到 850 积分时，其 BurnRate 骤降至 (850-800)/20 = 2.5，
//
//	此时 acct_calm（BurnRate=20）接棒主刷，acct_urgent 自然在 800 断崖线前平滑软着陆。
func TestSafeReserveBurnRateBalancingAndSoftLanding(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		LowCreditCutoff: 800.0,
		SafeReserveDays: 10.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 稳定选取最高分

	p.Add(&auth.Auth{UID: "acct_urgent"})
	p.Add(&auth.Auth{UID: "acct_calm"})

	p.UpdateResourceDetail("acct_urgent", 2400, now.Add(20*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("acct_calm", 1800, now.Add(50*24*time.Hour), 0, 0)

	// 1. acct_urgent 日消耗需求大 (80 > 20)，必须优先当选
	got1 := p.Pick()
	if got1 == nil || got1.UID != "acct_urgent" {
		t.Fatalf("expected acct_urgent with higher burn rate (80 vs 20), got %v", got1)
	}

	// 2. 模拟消耗 acct_urgent 至 850 积分（靠近 800 保护线）
	p.SetCredits("acct_urgent", 850)

	// 此时 acct_urgent BurnRate = (850-800)/20 = 2.5
	// acct_calm BurnRate = (1800-800)/50 = 20.0 (20 > 2.5)
	// 权重倒转，acct_calm 必须接棒成为首选！acct_urgent 成功实现软着陆！
	got2 := p.Pick()
	if got2 == nil || got2.UID != "acct_calm" {
		t.Fatalf("expected acct_calm to take over (burn rate 20 vs 2.5), got %v", got2)
	}
}

// TestAdaptiveChunkingUnknownModelDefaultQuota 验证冷启动/未知单价模型的分块额度默认为 15.0 credit：
// 在没有观测到实测扣费数据时，采用保守的 15 积分分块，既快速建立模型单价画像，又避免冷启动过度霸占单号。
func TestAdaptiveChunkingUnknownModelDefaultQuota(t *testing.T) {
	withNoPickGap(t)
	now := time.Now()
	p := New("")
	p.SetSchedulePolicy(SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})

	p.Add(&auth.Auth{UID: "fresh_bot"})
	p.UpdateResourceDetail("fresh_bot", 3000, now.Add(30*24*time.Hour), 0, 0)

	// 不记录任何成本数据，直接发起未知模型的选号
	got := p.PickExcludingForModel(nil, "unknown-model-xyz")
	if got == nil || got.UID != "fresh_bot" {
		t.Fatalf("expected fresh_bot, got %v", got)
	}

	chunk, ok := p.ActiveChunk("")
	if !ok {
		t.Fatal("expected active chunk")
	}
	if chunk.CreditQuota != 15.0 {
		t.Fatalf("expected unknown model chunk quota 15.0, got %f", chunk.CreditQuota)
	}
	if chunk.MaxReqs != 0 {
		t.Fatalf("expected unknown model max reqs 0, got %d", chunk.MaxReqs)
	}

	// 消耗 14 credits（未达 15）
	p.NoteUsageForModel("fresh_bot", 14.0, 1000, "unknown-model-xyz")
	got2 := p.PickExcludingForModel(nil, "unknown-model-xyz")
	if got2 == nil || got2.UID != "fresh_bot" {
		t.Fatalf("chunk should remain active at 14 credits, got %v", got2)
	}

	// 达到 15.2 credits（超过 15） -> 分块退役
	p.NoteUsageForModel("fresh_bot", 1.2, 500, "unknown-model-xyz")
	_, stillActive := p.ActiveChunk("")
	if stillActive {
		t.Fatal("chunk should retire after reaching 15.2 credits")
	}
}
