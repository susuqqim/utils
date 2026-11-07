package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/reqlog"
)

// B1：没有任何上游交互（池空）时，model_eff 必须为空 —— 面板列名是"实际出站"，
// 空池却显示模型名会把排查带偏（"到底发出去没有"）。
func TestNoUpstreamCallMeansEmptyModelEff(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		return 200, sseOK, http.Header{"Content-Type": []string{"text/event-stream"}}
	})
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up}) // 空池
	ring := reqlog.NewRing(8)
	handler := ReqLogMiddleware(h, &ReqLogSink{Ring: ring})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	rows := ring.Snapshot()
	if len(rows) == 0 {
		t.Fatal("没采到记录")
	}
	for _, e := range rows {
		if e.ModelEff != "" {
			t.Errorf("池空未发请求却记录了 model_eff=%q（应为空）", e.ModelEff)
		}
	}
}

// B4：面板需要"请求档位"才能判断是否真的降级。中间件要从 body 里把 effort 读出来。
func TestReqLogRecordsRequestedEffort(t *testing.T) {
	up := fakeUpstreamHdr(t, func(r *http.Request) (int, string, http.Header) {
		return 200, sseOK, http.Header{"Content-Type": []string{"text/event-stream"}}
	})
	a := authForTest()
	h := NewHandler(Config{Pool: testPoolWith(&a), Upstream: up})
	ring := reqlog.NewRing(8)
	handler := ReqLogMiddleware(h, &ReqLogSink{Ring: ring})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"glm-5.2","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)))

	rows := ring.Snapshot()
	if len(rows) == 0 {
		t.Fatal("没采到记录")
	}
	if got := rows[len(rows)-1].EffortReq; got != "max" {
		t.Errorf("EffortReq=%q want max", got)
	}
}
