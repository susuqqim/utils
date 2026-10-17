package pool

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 模型级阻塞：号都在、只是都被同一模型限流时，必须能与「真没号」区分
// （上游 44d6e04 / issue #102）。判据走 modelCooled（AuditOnly 不算阻塞）。
func TestPoolModelBlocked(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	now := time.Now()

	// 无任何模型冷却 → 不阻塞
	if st := p.ModelBlocked("glm-5.2", ""); st.Blocked {
		t.Fatalf("无冷却不该判阻塞: %+v", st)
	}
	// 只有一个号对该模型冷却 → 另一个号还能服务 → 不阻塞
	p.CooldownSoftForModel("u1", 600*time.Second, now.Add(30*time.Minute), "glm-5.2", "6004 model rate limit")
	if st := p.ModelBlocked("glm-5.2", ""); st.Blocked {
		t.Fatalf("仍有号可服务该模型时不该判阻塞: %+v", st)
	}
	// 两个号都对该模型冷却 → 阻塞；Count=2、Until 取最早、Reason 带原文
	p.CooldownSoftForModel("u2", 600*time.Second, now.Add(10*time.Minute), "glm-5.2", "6004 model rate limit")
	st := p.ModelBlocked("glm-5.2", "")
	if !st.Blocked {
		t.Fatalf("全池该模型冷却应判阻塞: %+v", st)
	}
	if st.Count != 2 {
		t.Errorf("Count=%d want 2", st.Count)
	}
	if st.Reason == "" {
		t.Errorf("Reason 应带冷却原因: %+v", st)
	}
	if st.Until.IsZero() || st.Until.After(now.Add(11*time.Minute)) {
		t.Errorf("Until 应取最早解封（≈+10min）：%v", st.Until)
	}
	// 只锁了 glm-5.2 → 别的模型不受影响
	if st := p.ModelBlocked("hy3", ""); st.Blocked {
		t.Errorf("只锁了 glm-5.2，不该影响 hy3: %+v", st)
	}
	// 空模型名不判阻塞（无成本维度）
	if st := p.ModelBlocked("", ""); st.Blocked {
		t.Errorf("空模型名不该判阻塞")
	}
	// 池内只有禁用号 = 「真的没号」，不是模型问题
	p2 := New(filepath.Join(t.TempDir(), "state2.json"))
	p2.Add(&auth.Auth{UID: "d1", AccessToken: "a", ExpiresAt: 9999999999})
	p2.Disable("d1", "test")
	if st := p2.ModelBlocked("glm-5.2", ""); st.Blocked {
		t.Errorf("池内无非禁用账号时应判「真没号」而非模型阻塞: %+v", st)
	}
}

// realm 维度（审查 A 的 P1-1）：单次请求只走一个 realm，跨 realm 的号不能用来否定
// "本 realm 全被挡"，否则模型级文案在多 realm 池里恒不生效。
func TestPoolModelBlockedRealmScoped(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "a", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "b", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	// cn 号被 6004 模型级冷却挡住；global 号健康
	p.CooldownSoftForModel("cn1", 600*time.Second, time.Now().Add(20*time.Minute), "glm-5.2", "6004 model rate limit")
	if st := p.ModelBlocked("glm-5.2", "cn"); !st.Blocked || st.Count != 1 {
		t.Errorf("cn 域全被挡应判阻塞（跨 realm 的 global 号不该否定它）: %+v", st)
	}
	if st := p.ModelBlocked("glm-5.2", "global"); st.Blocked {
		t.Errorf("global 域健康不该判阻塞: %+v", st)
	}
	// 暂停号不能作为"还有号能服务"的证据（审查 B/4 号 P1/P2）
	p.Pause("g1")
	p.CooldownSoftForModel("g1", 600*time.Second, time.Now().Add(5*time.Minute), "glm-5.2", "6004 model rate limit")
	if st := p.ModelBlocked("glm-5.2", "global"); st.Blocked {
		t.Errorf("该域只有暂停号+冷却号时，暂停号不算候选: %+v", st)
	}
}

// AuditOnly 条目（只审计不拦截）不得被判成阻塞（审查 A/C 的突变实验点）。
func TestPoolModelBlockedAuditOnlyNotBlocking(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	p.RecordModelRateLimitAudit("u1", "glm-5.2", "6004 without reset")
	if st := p.ModelBlocked("glm-5.2", ""); st.Blocked {
		t.Errorf("AuditOnly 不该判阻塞: %+v", st)
	}
}

// ModelExemptForTest 仅供测试：暴露 modelExempt 的探活口径（生产无导出必要）。
func (p *Pool) ModelExemptForTest(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if !e.modelExempt() {
		return false
	}
	// 探活口径还要求账号整体可用（paused/disabled 已被 modelExempt 排除）
	return e.healthy(time.Now())
}

// 11102 负缓存通道（上游 44d6e04 的 BlockModelBackoff/Clear）：写入即判阻塞，
// 退避有界递增，清除后恢复 —— 覆盖真实写入/清除通道而不是手搓 map。
func TestPoolBlockModelBackoffAndClear(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	reason := "11102 model not found on this backend (negative cache)"
	p.BlockModelBackoff("u1", "glm-5.9", reason)
	st := p.ModelBlocked("glm-5.9", "")
	if !st.Blocked || st.Count != 1 {
		t.Fatalf("写入 11102 负缓存后应判阻塞: %+v", st)
	}
	if !strings.HasPrefix(st.Reason, "11102") {
		t.Errorf("Reason 必须以 11102 开头（BlockModelClear 与 handler 分流都依赖该前缀）：%q", st.Reason)
	}
	// 指数退避：首次 6h，第二次 12h（封顶 24h）
	first := st.Until.Sub(time.Now())
	p.BlockModelBackoff("u1", "glm-5.9", reason)
	st2 := p.ModelBlocked("glm-5.9", "")
	second := st2.Until.Sub(time.Now())
	if second <= first {
		t.Errorf("第二次退避应更长：first=%v second=%v", first, second)
	}
	// 成功路径清除（只清 11102 条目）
	p.BlockModelClear("u1", "glm-5.9")
	if st := p.ModelBlocked("glm-5.9", ""); st.Blocked {
		t.Errorf("清除后不该再判阻塞: %+v", st)
	}
}

// 承诺守卫（审查 D 的 P2-11 缺口①）：BlockModelClear 只清 11102、**不碰** 6004 冷却表
// （6004 有自身上游重置墙钟语义，一次成功不该抹掉它）。
func TestBlockModelClearKeeps6004(t *testing.T) {
	// 注意：同一个 (账号,模型) 在 map 里只有一条条目（后写覆盖先写），所以"11102 与 6004
	// 并存"必须用**两个账号**表达：u1 被 6004 限流、u2 拿到 11102 负缓存。
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "b", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.2", "6004 model rate limit")
	p.BlockModelBackoff("u2", "glm-5.2", "11102 model not found on this backend (negative cache)")
	p.BlockModelClear("u2", "glm-5.2")
	// u2 已被清 ⇒ 它又能服务该模型 ⇒ 整体不再"全被挡"（Blocked 的语义是"所有号都被挡"）。
	if st := p.ModelBlocked("glm-5.2", ""); st.Blocked {
		t.Errorf("u2 清除后不该再判模型级阻塞：%+v", st)
	}
	// 而 u1 的 6004 必须仍在 ⇒ 选号时被排除 ⇒ 只有 u2 可选（行为断言，不依赖 Reason 文案）。
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a == nil || a.UID != "u2" {
		t.Fatalf("u1 仍被 6004 挡，应只有 u2 可选，实得 %v", a)
	}
	// 账号级冷却清理也不能把 11102 负缓存一起清掉（审查 D 的 P2-5）
	p2 := New(filepath.Join(t.TempDir(), "state.json"))
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	p2.BlockModelBackoff("u1", "glm-5.2", "11102 model not found on this backend (negative cache)")
	p2.Cooldown("u1", CoolSoft, 60*time.Second, "429 too many requests") // 账号级冷却 → clearRoutingModelCooldownsLocked
	if st := p2.ModelBlocked("glm-5.2", ""); !st.Blocked || !st.AllNotFound {
		t.Errorf("账号级冷却不该清掉 11102 负缓存：%+v", st)
	}
}

// 混合态（审查 D 的 P1-3）：部分号 11102、部分号 6004 ⇒ AllNotFound=false（handler 给
// 可重试的 503，而不是"换模型、别重试"的 400），且 Reason 确定性优先取 11102。
func TestModelBlockedMixedReasonsDeterministic(t *testing.T) {
	for i := 0; i < 20; i++ { // 反复跑：原实现按 map 迭代序取 Reason，会随机翻转
		p := New(filepath.Join(t.TempDir(), "state.json"))
		p.Add(&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999})
		p.Add(&auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999})
		p.BlockModelBackoff("a", "glm-5.2", "11102 model not found on this backend (negative cache)")
		p.CooldownSoftForModel("b", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.2", "6004 model rate limit")
		st := p.ModelBlocked("glm-5.2", "")
		if !st.Blocked || st.Count != 2 {
			t.Fatalf("两个号都被挡，应判阻塞且 Count=2：%+v", st)
		}
		if st.AllNotFound {
			t.Fatalf("混合态不该判 AllNotFound（会给出不可重试的 400）：%+v", st)
		}
		if !strings.HasPrefix(st.Reason, "11102") {
			t.Fatalf("Reason 应确定性优先 11102，实得 %q（第 %d 次）", st.Reason, i)
		}
	}
}

// 退避封顶（审查 D 的 P2-11 缺口⑥）：无论命中多少次，TTL 不得超过 24h，
// 且 Hits 单调递增（驱动 6h→12h→24h→24h）。
func TestBlockModelBackoffCapsAt24h(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	reason := "11102 model not found on this backend (negative cache)"
	for i := 0; i < 12; i++ {
		p.BlockModelBackoff("u1", "glm-5.2", reason)
	}
	st := p.ModelBlocked("glm-5.2", "")
	ttl := time.Until(st.Until)
	if ttl > 24*time.Hour+time.Minute {
		t.Errorf("退避封顶 24h 失效：TTL=%v", ttl)
	}
	if ttl < 23*time.Hour {
		t.Errorf("多次命中后应达到封顶（≥23h），实得 %v", ttl)
	}
	mc := p.byUID["u1"].modelCooldowns["glm-5.2"]
	if mc.Hits != 12 {
		t.Errorf("Hits 应累计到 12，实得 %d", mc.Hits)
	}
}

// S1（模型锁池视图）：ModelLocks 聚合"当前锁了哪些 (realm,model)、各几个号、最早何时解封、
// 是否含 11102"。只读快照，不改变选号；台账（AuditOnly）与已过期条目不参与。
func TestPoolModelLocksAggregation(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g", AccessToken: "c", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.CooldownSoftForModel("a", 600*time.Second, time.Now().Add(10*time.Minute), "glm-5.2", "6004 model rate limit")
	p.CooldownSoftForModel("b", 600*time.Second, time.Now().Add(30*time.Minute), "glm-5.2", "6004 model rate limit")
	p.BlockModelBackoff("g", "gpt-6-sol", "11102 model not found on this backend (negative cache)")
	p.CooldownSoftForModel("a", 600*time.Second, time.Now().Add(5*time.Minute), "hy3", "6004 model rate limit")
	p.RecordModelRateLimitAudit("b", "audit-only", "6004 no reset")

	rows := p.ModelLocks()
	if len(rows) != 3 {
		t.Fatalf("应有 3 行（cn×2 + global×1；台账不计），实得 %d：%+v", len(rows), rows)
	}
	if rows[0].Model != "hy3" || rows[1].Model != "glm-5.2" {
		t.Errorf("应按最早解封升序，实得 %s, %s", rows[0].Model, rows[1].Model)
	}
	if rows[1].Accounts != 2 {
		t.Errorf("cn/glm-5.2 应有 2 个号被挡，实得 %d", rows[1].Accounts)
	}
	if rows[1].NotFound {
		t.Error("6004 行不该标 not_found")
	}
	if last := rows[2]; last.Realm != "global" || last.Model != "gpt-6-sol" || !last.NotFound {
		t.Errorf("global/gpt-6-sol 应为 11102 负缓存行，实得 %+v", last)
	}
}
