package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/reqlog"
)

// 请求记录必须能区分「客户端要的模型/档位」与「实际出站的模型/档位」：
// 带 cn: 前缀、或 reasoning_effort 被上游能力表降到更低档时，两者不同。
// 没有这两个字段，面板上只能看到请求模型，降级是隐形的（排查"为什么这次输出差"只能靠猜）。
func TestReqLogRecordsEffectiveModelAndEffort(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/personal/models"):
			return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`, nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return 200, `{"code":0,"data":{"models":[]}}`, nil
		default:
			return 200, sseOK, http.Header{"Content-Type": []string{"text/event-stream"}}
		}
	})
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	if _, err := up.FetchModels(acct); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	h := NewHandler(Config{Pool: testPoolWith(acct), Upstream: up})
	ring := reqlog.NewRing(16)
	handler := ReqLogMiddleware(h, &ReqLogSink{Ring: ring})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"cn:glm-5.2","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)))

	var row *reqlog.Entry
	for _, e := range ring.Snapshot() {
		if e.Path == "/v1/chat/completions" {
			c := e
			row = &c
		}
	}
	if row == nil {
		t.Fatal("没采到 chat 的请求记录")
	}
	if row.Model != "cn:glm-5.2" {
		t.Errorf("Model=%q want 客户端原文 cn:glm-5.2", row.Model)
	}
	if row.ModelEff != "glm-5.2" {
		t.Errorf("ModelEff=%q want 出站裸名 glm-5.2", row.ModelEff)
	}
	if row.EffortEff != "high" {
		t.Errorf("EffortEff=%q want 降级后档位 high（请求 max，上游只支持 low/high）", row.EffortEff)
	}
}
