package upstream

import (
	"net/http"
	"testing"
)

// 回归（2026-10-20，上游 PR #233）：WorkBuddy 的 11128（通道校验失败）与 11140（账号被限制）
// msg 都是 "request illegal"，文案完全相同。旧实现按裸文案判定，会把一次通道校验失败
// 误判成账号封禁并硬禁用健康账号。必须按业务码分流，且都要求 code + 文案双条件。
func TestClassifyChannelRefusedVsAccountRestricted(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"11128 通道校验（400）", 400,
			`{"code":11128,"msg":"request illegal","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`,
			ErrChannelRefused},
		{"11128 通道校验（403）", 403, `{"code":11128,"msg":"request illegal"}`, ErrChannelRefused},
		{"11140 账号限制", 403, `{"code":11140,"msg":"request illegal"}`, ErrAccountFault},
		{"11140 账号限制（400）", 400, `{"code":11140,"msg":"request illegal"}`, ErrAccountFault},
		// 11140 的模型级限流变体：文案不是 request illegal，必须仍是软限流
		{"11140 限流变体", 200,
			`{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`,
			ErrSoftRate},
		// 裸文案（无业务码）不得归属任何账号级判定
		{"无码裸文案", 400, `{"msg":"request illegal"}`, ErrClient},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d)=%v want %v", c.name, c.status, got, c.want)
		}
	}
}

func TestIsAccountRestrictedAndChannelRefused(t *testing.T) {
	if !IsAccountRestricted(`{"code":11140,"msg":"request illegal"}`) {
		t.Error("11140+request illegal 应判 IsAccountRestricted")
	}
	if IsAccountRestricted(`{"code":11128,"msg":"request illegal"}`) {
		t.Error("11128 不得判 IsAccountRestricted（健康账号）")
	}
	if IsAccountRestricted(`{"code":11140,"msg":"The model provider is rate-limiting requests."}`) {
		t.Error("11140 限流变体不得判 IsAccountRestricted")
	}
	if !isChannelRefused(`{"code":11128,"msg":"request illegal"}`) {
		t.Error("11128+request illegal 应判 isChannelRefused")
	}
	if isChannelRefused(`{"code":11128,"msg":"blocked by security policy"}`) {
		t.Error("11128 内容审核变体不得判通道校验失败")
	}
}

// X-Conversation-ID 必须始终发送：国际端点收紧校验后，缺失会直接 400 / 11128。
func TestConversationIDAlwaysSent(t *testing.T) {
	c := New()
	// 空 meta（客户端没带会话 ID）
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	c.injectConversationHeaders(req, ChatMeta{})
	if got := req.Header.Get("X-Conversation-ID"); got == "" {
		t.Fatal("空 meta 时也必须发送 X-Conversation-ID（回退生成的 32 hex）")
	} else if len(got) != 32 {
		t.Errorf("回退会话 ID 应为 32 位 hex，实得 %q", got)
	}
	// 入站带值则透传
	req2, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	c.injectConversationHeaders(req2, ChatMeta{ConversationID: "abc123"})
	if got := req2.Header.Get("X-Conversation-ID"); got != "abc123" {
		t.Errorf("应透传入站会话 ID，实得 %q", got)
	}
	// 两次独立请求的回退值不得相同（不跨请求复用）
	req3, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	c.injectConversationHeaders(req3, ChatMeta{})
	if req3.Header.Get("X-Conversation-ID") == req.Header.Get("X-Conversation-ID") {
		t.Error("回退会话 ID 必须每次新生成")
	}
}
