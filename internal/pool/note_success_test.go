package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 回归（2026-10-20 生产实测）：账号一次 429 进入软冷却后，后续请求其实都成功了，
// 但 NoteSuccess 不清 until ⇒ 每个请求都走「全冷却兜底」路径（fallback_earliest_expiry），
// 既刷 WARN 噪音，又让调度丢失"该号健康"的信息。
func TestNoteSuccessClearsSoftCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000)

	p.Cooldown("u1", CoolSoft, 10*time.Minute, "429 rate limit")
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("应处于软冷却")
	}

	p.NoteSuccess("u1")

	st, _ = p.Status("u1")
	if st.Cooling {
		t.Errorf("成功后应清除软冷却，实得 cooling=%v until=%v", st.Cooling, st.Until)
	}
	if a := p.PickExcluding(nil); a == nil {
		t.Error("清除软冷却后应能正常选中（不再走兜底路径）")
	}
}

// 余额耗尽（CoolHard）不能被"成功"清掉：那种号调了必 402，必须等签到恢复。
func TestNoteSuccessKeepsHardCreditCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u2", 0)

	p.Cooldown("u2", CoolHard, 10*time.Minute, "credits exhausted")
	p.NoteSuccess("u2")

	st, _ := p.Status("u2")
	if !st.Cooling {
		t.Error("CoolHard 不得被成功清除")
	}
}
