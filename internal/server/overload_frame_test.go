package server

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 11134 以 SSE error 帧到达时，不得写 (账号,模型) 负缓存 ——
// 否则第二次同模型请求会被本地判成 model_unavailable(400)，不可重试，
// 与 HTTP 路径刚建立的"过载可重试"语义正好相反（审计 R2-core P1）。
func TestProviderOverloadFrameDoesNotBlockModel(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		return 200, sseOK, http.Header{"Content-Type": []string{"text/event-stream"}}
	})})

	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, `{"code":11134,"msg":"model provider is overloaded"}`, "glm-5.2")

	if st := p.ModelBlocked("glm-5.2", "cn"); st.Blocked {
		t.Fatalf("11134 不应写模型负缓存，实得 %+v", st)
	}
}
