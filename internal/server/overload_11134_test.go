package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

// 11134 = 供应商侧过载（不是账号问题）：换号撞的是同一个过载供应商，
// 只会多烧一份上下文/积分，还会丢掉会话粘性。约定：
//   - 本轮只打一次上游，不轮转其它账号；
//   - 不罚号（不停用、不熔断）；
//   - 终态可重试（503），让客户端退避重试而不是换号。
func TestProviderOverload11134DoesNotRotate(t *testing.T) {
	var calls int32
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		atomic.AddInt32(&calls, 1)
		return http.StatusServiceUnavailable, `{"code":11134,"msg":"model provider is overloaded"}`, nil
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("上游被打了 %d 次；11134 过载不应轮转其它账号（期望 1 次）", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503（可重试终态）", rec.Code)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("%s 不在池里", uid)
		}
		if st.Disabled {
			t.Errorf("%s 被停用：11134 是供应商过载，不应罚号", uid)
		}
		if st.BreakerFails > 0 {
			t.Errorf("%s 熔断计数=%d：过载不应喂熔断", uid, st.BreakerFails)
		}
	}
}
