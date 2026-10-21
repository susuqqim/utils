package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 【F1 回归，2026-10-21】畸形请求必须在**本地 400**，绝不能打到上游。
//
// 修复前实测（生产）：缺 messages / 空 messages / 缺 model 的请求会真的发到上游
// （耗时 1.2~1.8s），拿回 11101/11133 再转成 invalid_request_error。每浪费一次就是
// 一次上游调用 —— 既烧账号配额，又在风控眼里多一条异常请求。
func TestMalformedRequestsNeverReachUpstream(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"非法 JSON", `not-json`, "invalid_json"},
		{"空对象", `{}`, "missing_model"},
		{"缺 messages", `{"model":"glm-5.2"}`, "missing_messages"},
		{"messages 为 null", `{"model":"glm-5.2","messages":null}`, "missing_messages"},
		{"messages 为空数组", `{"model":"glm-5.2","messages":[]}`, "empty_messages"},
		{"messages 不是数组", `{"model":"glm-5.2","messages":"x"}`, "invalid_messages"},
		{"缺 model", `{"messages":[{"role":"user","content":"hi"}]}`, "missing_model"},
		{"model 为纯空白", `{"model":"   ","messages":[{"role":"user","content":"hi"}]}`, "missing_model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstreamCalls := 0
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				upstreamCalls++
				return 200, sseOK, true
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			p.SetCredits("u1", 1000)
			h := NewHandler(Config{Pool: p, Upstream: up})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), c.wantCode) {
				t.Errorf("body=%s 应包含 code=%s", rec.Body.String(), c.wantCode)
			}
			if upstreamCalls != 0 {
				t.Errorf("畸形请求不得打到上游，实得 %d 次", upstreamCalls)
			}
		})
	}
}

// 合法的最小请求仍应正常转发（防止校验过严误伤）。
func TestLegalMinimalRequestStillForwarded(t *testing.T) {
	upstreamCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		upstreamCalls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("最小合法请求应成功，实得 code=%d body=%s", rec.Code, rec.Body)
	}
	if upstreamCalls == 0 {
		t.Error("合法请求应转发到上游")
	}
}

// 【D2 回归，2026-10-21】503（暂时不可用，可重试）必须带 Retry-After，
// 否则客户端会盲目立刻重试，把"暂时不可用"放大成持续上游压力。
func Test503CarriesRetryAfter(t *testing.T) {
	// 无可用账号 ⇒ 503 no_healthy_account
	p := testPoolWith()
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("空池应 503，实得 %d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("503 必须带 Retry-After 退避提示")
	}
}
