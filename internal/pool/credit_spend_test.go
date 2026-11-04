package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func spendTestAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "t", Domain: "www.codebuddy.cn"}
}

func spendCreditsOf(t *testing.T, p *Pool, uid string) int64 {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("no status for %s", uid)
	}
	return st.Credits
}

// 单请求实测积分常只有 0.02~0.09。旧实现用 math.Ceil ⇒ 每条至少扣 1 分，
// 本地余额飞快见底，5 分钟刷新又把它拉回真实值，面板表现就是忽降忽升/忽而不动。
// 这条锁住「小数累计、满 1 才扣、余数保留」。
func TestNoteUsageDeductsFractionally(t *testing.T) {
	p := New("")
	p.Add(spendTestAuth("u1"))
	p.SetCredits("u1", 100)

	p.NoteUsage("u1", 0.4, 10)
	if got := spendCreditsOf(t, p, "u1"); got != 100 {
		t.Fatalf("0.4 分不应扣整分，credits=%d", got)
	}
	p.NoteUsage("u1", 0.7, 10)
	if got := spendCreditsOf(t, p, "u1"); got != 99 {
		t.Fatalf("累计 1.1 分应扣 1 分，credits=%d", got)
	}
	p.NoteUsage("u1", 200, 10)
	if got := spendCreditsOf(t, p, "u1"); got != 0 {
		t.Fatalf("超额消耗应归零而不是负数，credits=%d", got)
	}
}

// 刷新写回绝对值时必须丢掉本地小数余数，否则校正后又会多扣一分。
func TestNoteUsageFractionResetOnRefresh(t *testing.T) {
	p := New("")
	p.Add(spendTestAuth("u1"))
	p.SetCredits("u1", 100)
	p.NoteUsage("u1", 0.9, 10)
	p.UpdateResourceDetail("u1", 100, time.Time{}, 0, 0)
	p.NoteUsage("u1", 0.5, 10)
	if got := spendCreditsOf(t, p, "u1"); got != 100 {
		t.Fatalf("刷新后余数应清零：0.5 分不足以扣整分，credits=%d", got)
	}
}

// 大额消耗仍应立即扣（一次 60 分不能靠累计）。
func TestNoteUsageLargeSpendImmediate(t *testing.T) {
	p := New("")
	p.Add(spendTestAuth("u1"))
	p.SetCredits("u1", 100)
	p.NoteUsage("u1", 60, 1000)
	if got := spendCreditsOf(t, p, "u1"); got != 40 {
		t.Fatalf("60 分应扣 60，credits=%d", got)
	}
}

// 积分消耗统计（credits_used / 今日）仍按真实小数累计，不受扣减粒度影响。
func TestNoteUsageKeepsExactConsumption(t *testing.T) {
	p := New("")
	p.Add(spendTestAuth("u1"))
	p.SetCredits("u1", 100)
	p.NoteUsage("u1", 0.02, 10)
	p.NoteUsage("u1", 0.03, 10)
	st, _ := p.Status("u1")
	if st.CreditsUsed < 0.049 || st.CreditsUsed > 0.051 {
		t.Fatalf("credits_used=%v want 0.05", st.CreditsUsed)
	}
}
