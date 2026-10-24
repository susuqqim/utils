package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 【2026-10-24 上游新语义】11140 是「账号级、需申诉、不会自愈」的限制：
// ttl<=0 必须表现为**永久不可选**（不因时间流逝自动恢复），只能人工 Revive 解除。
func TestRestrictPermanent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u9", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u9", 1000)

	p.Restrict("u9", 0, "upstream 11140: account restricted (unusual activity)")

	st, ok := p.Status("u9")
	if !ok || !st.Disabled {
		t.Fatalf("应显示 disabled，实得 %+v", st)
	}
	if st.Reason == "" {
		t.Error("reason 应保留上游原因，供面板展示")
	}
	if p.PickExcluding(nil) != nil {
		t.Fatal("永久限制期间不得被选中")
	}
	// 时间推进也不能恢复（restrictionExpired 只对「有限制到期时间」的条目为真）
	if e, ok := p.byUID["u9"]; ok {
		if e.restrictionExpired(time.Now().Add(100 * time.Hour)) {
			t.Fatal("永久限制不应被判为已到期")
		}
	}
	// 人工解除（面板「解冻」）后必须可再被选中
	if !p.Revive("u9") {
		t.Fatal("Revive 应返回 true")
	}
	if st, _ := p.Status("u9"); st.Disabled {
		t.Fatalf("解除后不应再 disabled，实得 %+v", st)
	}
	if p.PickExcluding(nil) == nil {
		t.Fatal("解除后应可被选中")
	}
}

// 兼容旧行为：显式给 ttl>0 时仍按限时处理（到期自动恢复）。
func TestRestrictWithTTLStillExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u10", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u10", 1000)

	p.Restrict("u10", 30*time.Millisecond, "upstream 11140 (limited ttl mode)")
	if a := p.PickExcluding(nil); a != nil {
		t.Fatal("限时窗口内不应被选中")
	}
	time.Sleep(50 * time.Millisecond)
	if a := p.PickExcluding(nil); a == nil {
		t.Fatal("ttl 到期后应恢复可选")
	}
}
