package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 【P1-7】上游风控限制（11140）必须带到期时间：
//   - 未到期 ⇒ 不可选、Status 显示 disabled；
//   - 到期后 ⇒ 自动恢复可选（上游限制会自行解除），无需人工"解冻"；
//   - 手工 Disable（永久）不受到期时间影响。
func TestRestrictExpiresAutomatically(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000)

	p.Restrict("u1", 50*time.Millisecond, "account restricted by upstream (11140 request illegal)")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("未到期时应显示 disabled")
	}
	if a := p.PickExcluding(nil); a != nil {
		t.Fatalf("未到期时不应被选中，实得 %s", a.UID)
	}

	time.Sleep(80 * time.Millisecond)

	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatal("到期后应自动恢复（显示不再 disabled）")
	}
	if a := p.PickExcluding(nil); a == nil {
		t.Fatal("到期后应可被选中")
	}
}

// 手工 Disable 是永久禁用：即使曾经设过限制到期时间，也不得被"到期自动恢复"误放行。
func TestDisableClearsRestrictionExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u2", 1000)

	p.Restrict("u2", 30*time.Millisecond, "restricted")
	p.Disable("u2", "manual disable")
	time.Sleep(60 * time.Millisecond)

	if st, _ := p.Status("u2"); !st.Disabled {
		t.Fatal("手工禁用的账号不得因限制到期而自动恢复")
	}
	if a := p.PickExcluding(nil); a != nil {
		t.Fatalf("手工禁用账号不应被选中，实得 %s", a.UID)
	}
}
