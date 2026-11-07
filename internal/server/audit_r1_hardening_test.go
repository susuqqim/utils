package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqlog"
)

// C-1：model 由客户端控制，没有长度上限时一个 8MB 的 model 字段会进内存环/归档/面板序列化。
// 采集层必须截断（保留前缀，便于识别）。
func TestReqLogTruncatesOversizedModel(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		return 200, sseOK, http.Header{"Content-Type": []string{"text/event-stream"}}
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	ring := reqlog.NewRing(8)
	handler := ReqLogMiddleware(h, &ReqLogSink{Ring: ring})

	huge := strings.Repeat("m", 5000)
	body := `{"model":"` + huge + `","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	rows := ring.Snapshot()
	if len(rows) == 0 {
		t.Fatal("没采到记录")
	}
	for _, e := range rows {
		if len(e.Model) > 200 {
			t.Errorf("Model 长度 %d 未截断（会撑爆内存环/面板）", len(e.Model))
		}
		if len(e.ModelEff) > 200 {
			t.Errorf("ModelEff 长度 %d 未截断", len(e.ModelEff))
		}
	}
}

// D-1：11134 与限流文案同时出现时（上游真实形态），也必须按"供应商过载"处理：
// 只打一次上游、不罚号。修复前会落 ErrSoftRate → 轮转 + 冷却账号。
func TestProviderOverloadWithRateLimitTextDoesNotRotate(t *testing.T) {
	var calls int32
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		atomic.AddInt32(&calls, 1)
		return http.StatusTooManyRequests, `{"code":11134,"msg":"rate limit exceeded: model provider is overloaded"}`, nil
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
		t.Fatalf("上游被打了 %d 次；11134+限流文案也不应轮转（期望 1）", got)
	}
}

// D-2：11134 只出现在**嵌套的非 code 字段**里（{"code":11102,"extra":{"code":11134}}）时，
// 不能把整单判成过载而放弃轮转 —— 11102 是"该后端无此模型"，其它账号可能正好有。
func TestNested11134DoesNotSuppressRotation(t *testing.T) {
	var calls int32
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		atomic.AddInt32(&calls, 1)
		return http.StatusNotFound, `{"code":11102,"msg":"no such model on this backend","extra":{"code":11134}}`, nil
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("上游只打了 %d 次：嵌套 11134 不应抑制轮转（11102 需要换号再试）", got)
	}
}
