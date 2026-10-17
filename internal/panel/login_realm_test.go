package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLoginEndpoints realm→端点映射：global 三端点全在 workbuddy.ai 域，
// cn/空/非法值 → copilot.tencent.com（零回归兜底）。
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm      string
		wantState  string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginEndpoints(c.realm)
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			// token/account 端点必与 state 同 base。
			if c.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}

// loginPoll 必须自己判 TTL：回收只在 loginStart 顺手做，用户不再发起新的添加账号时
// 过期 state 会一直可用（与 panel.go 文档口径和"unknown or expired state"文案都不一致）。
func TestLoginPollRejectsExpiredState(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	const state = "expired-state-abc"

	p.loginMu.Lock()
	p.logins[state] = loginSession{created: time.Now().Add(-loginTTL - time.Minute), realm: "cn"}
	p.loginMu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/login/poll?state="+state, nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("过期 state 应回 404，got %d body=%s", rec.Code, rec.Body)
	}
	p.loginMu.Lock()
	_, stillThere := p.logins[state]
	p.loginMu.Unlock()
	if stillThere {
		t.Fatal("过期 state 应从 logins 里剔除（否则内存只增不减）")
	}
}
