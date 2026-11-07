package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// fakeUpstreamHdr 与 newFakeUpstream 同形，但允许自定义响应头 ——
// 网关层 401 的死因只在 WWW-Authenticate 里，正文是同一页 HTML。
func fakeUpstreamHdr(t *testing.T, behavior func(r *http.Request) (int, string, http.Header)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body, hdr := behavior(r)
			if hdr == nil {
				hdr = http.Header{}
			}
			if hdr.Get("Content-Type") == "" {
				hdr.Set("Content-Type", "application/json")
			}
			return &http.Response{
				StatusCode: status,
				Header:     hdr,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const gwHTML401 = "<html><head><title>401 Authorization Required</title></head><body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>"

// 中间代理/边缘回的 401 HTML（无挑战头）不得停用账号：号是健康的，只是这次请求被挡。
// 修复前会走 ErrClient —— 不停用但也不冷却，同一请求反复撞同一节点；这里要求 30s 软冷却。
func TestGateway401PageDoesNotDisable(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		return http.StatusUnauthorized, gwHTML401, http.Header{"Content-Type": []string{"text/html"}}
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不见了")
	}
	if st.Disabled {
		t.Fatal("代理页 401 不得停用账号")
	}
	if !st.Cooling {
		t.Error("应给短冷却，避免同一请求反复撞同一节点")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", rec.Code)
	}
}

// 网关点名 invalid_token：先刷新一次。刷新成功则该号继续服务下一个请求，
// 不得像修复前那样把可自愈的号直接停用（hub #795 的教训）。
func TestInvalidTokenRefreshesInsteadOfDisabling(t *testing.T) {
	refreshed := false
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, "token/refresh") {
			refreshed = true
			return 200, `{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresAt":1999999999}}`, nil
		}
		return http.StatusUnauthorized, gwHTML401, http.Header{
			"Content-Type":     []string{"text/html"},
			"Www-Authenticate": []string{`Bearer realm="copilot", error="invalid_token", error_description="invalid token"`},
		}
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if !refreshed {
		t.Fatal("invalid_token 必须先尝试刷新")
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatal("刷新成功时不得停用账号")
	}
}

// 刷新也失败（凭证真死）才停用 —— 这才是需要人工重登的情况。
func TestInvalidTokenDisablesWhenRefreshFails(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		if strings.Contains(r.URL.Path, "token/refresh") {
			return http.StatusUnauthorized, gwHTML401, http.Header{
				"Content-Type":     []string{"text/html"},
				"Www-Authenticate": []string{`Bearer realm="copilot", error="invalid_token"`},
			}
		}
		return http.StatusUnauthorized, gwHTML401, http.Header{
			"Content-Type":     []string{"text/html"},
			"Www-Authenticate": []string{`Bearer realm="copilot", error="invalid_token"`},
		}
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("刷新失败应停用账号，实得 %+v", st)
	}
	_ = rec
}
