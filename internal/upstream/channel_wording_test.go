package upstream

import (
	"net/http"
	"testing"
)

// 回归（2026-10-21 上游收紧）：11128 的 msg 从 "request illegal" 改成
// "Illegal API invocation from an unapproved channel"（displayTips 明说"渠道校验"）。
// 旧实现只认 "request illegal"，且这两句曾被放进 contentBlockedMarkers ⇒
// 新文案被判成 content_blocked，触发"降级提示词重试"（凭空多发一次上游请求）。
func TestChannelRefusedNewWording(t *testing.T) {
	const newBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"01a15d01071071b2ab33d2d7ccdc4d11","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"}}`
	if got := Classify(http.StatusBadRequest, newBody); got != ErrChannelRefused {
		t.Fatalf("新文案应为 ErrChannelRefused，实得 %v", got)
	}
	if got := Classify(http.StatusBadRequest, `{"code":11128,"msg":"request illegal"}`); got != ErrChannelRefused {
		t.Fatalf("旧文案仍应兼容，实得 %v", got)
	}
	// 11128 的内容审核变体必须仍是 ErrContentBlocked（不能被渠道校验抢走）
	if got := Classify(http.StatusBadRequest, `{"code":11128,"msg":"blocked by security policy"}`); got != ErrContentBlocked {
		t.Fatalf("内容审核变体应为 ErrContentBlocked，实得 %v", got)
	}
	// 无 11128 业务码、只有文案 → 不判渠道校验
	if got := Classify(http.StatusBadRequest, `{"msg":"Illegal API invocation from an unapproved channel"}`); got == ErrChannelRefused {
		t.Error("无业务码不应判 ErrChannelRefused")
	}
}

// 渠道校验**不得**再触发"降级提示词重试"（那条路径会多打一次上游）。
func TestChannelRefusedNotContentBlocked(t *testing.T) {
	body := `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	if Classify(http.StatusBadRequest, body) == ErrContentBlocked {
		t.Fatal("渠道校验被判成内容审核 ⇒ 会触发降级重试，凭空多发上游请求")
	}
}
