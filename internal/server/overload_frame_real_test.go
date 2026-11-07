package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
)

// 真实 SSE error 帧形态（{"error":{"code":11134,...}}）连打两次：
// 第一次不得写 (账号,模型) 负缓存，第二次仍必须打到上游。
func TestProviderOverloadRealErrorFrameTwice(t *testing.T) {
	const errFrame = "event: error\ndata: {\"error\":{\"code\":11134,\"message\":\"model provider is overloaded\"}}\n\n"
	var calls int32
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		atomic.AddInt32(&calls, 1)
		return 200, errFrame, http.Header{"Content-Type": []string{"text/event-stream"}}
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Fatalf("第 %d 次请求被判 model_unavailable(400)：11134 帧不该写负缓存", i+1)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("上游被打了 %d 次 want 2（第二次不该被本地负缓存挡住）", got)
	}
	if st, ok := p.Status("u1"); ok && st.Cooling {
		t.Fatalf("11134 帧让账号进入冷却（%s，剩余 %v 秒）：过载不该罚号", st.Reason, st.CoolRemaining)
	}
	_ = io.Discard
}
