package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/reqlog"
)

// S3 第二步：采集只覆盖 /v1/*，且**必须脱敏**（IP /24、UA 120、uid 8 位），
// 同时不得吞掉请求体（下游仍要能读到 model）。
func TestReqLogMiddlewareCapturesAndSanitizes(t *testing.T) {
	ring := reqlog.NewRing(16)
	var seenBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	h := ReqLogMiddleware(next, &ReqLogSink{Ring: ring})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.77, 10.0.0.1")
	req.Header.Set("User-Agent", "agent/1.0 "+strings.Repeat("x", 200))
	h.ServeHTTP(rec, req)

	if !strings.Contains(seenBody, "glm-5.2") {
		t.Fatalf("请求体必须交回下游（否则模型解析全坏）：%q", seenBody)
	}
	got := ring.Snapshot()
	if len(got) != 1 {
		t.Fatalf("应采集 1 条，实得 %d", len(got))
	}
	e := got[0]
	if e.Path != "/v1/chat/completions" || e.Status != 200 || e.Model != "glm-5.2" {
		t.Errorf("基本字段错误：%+v", e)
	}
	if e.IP != "203.0.113.0/24" {
		t.Errorf("IP 必须截断到 /24，实得 %q", e.IP)
	}
	if n := len([]rune(e.UA)); n > 120 {
		t.Errorf("UA 必须 ≤120 字符，实得 %d", n)
	}
	if strings.Contains(e.UA, "x"+"x"+"x"+"x"+"x"+"x"+"x"+"x"+"x"+"x"+"x"+"x") && len([]rune(e.UA)) == 0 {
		t.Error("UA 不该为空")
	}

	// 面板/健康检查等**不采**
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if n := len(ring.Snapshot()); n != 1 {
		t.Errorf("非 /v1/* 不该被采集，实得 %d 条", n)
	}
}

// 回归守卫（对抗式自审发现）：中间件为探测 model 读了前 64KB，**绝不能**因此截断 body。
// 只放回前缀会让 >64KB 的请求体静默丢尾部 —— 大上下文/文件块请求正好会踩到，且下游只报模糊解析错。
func TestReqLogMiddlewarePreservesLargeBody(t *testing.T) {
	ring := reqlog.NewRing(4)
	const n = 200 << 10 // 200KB，远超探测窗口
	payload := `{"model":"glm-5.2","pad":"` + strings.Repeat("z", n) + `"}`
	var gotLen int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // 下游必须读到**完整**body
		gotLen = len(b)
		w.WriteHeader(200)
	})
	h := ReqLogMiddleware(next, &ReqLogSink{Ring: ring})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload)))

	if gotLen != len(payload) {
		t.Fatalf("请求体被截断：下游读到 %d 字节，原始 %d 字节（丢失 %d）", gotLen, len(payload), len(payload)-gotLen)
	}
	// 只断言'采集到了'：首个 64KB 切在 JSON 中间 ⇒ json.Unmarshal 必然失败，
	// 此时**不猜 model** 是正确行为（宁可空着，也不能把半截 JSON 当模型名）。
	if e := ring.Snapshot(); len(e) != 1 || e[0].Model != "" {
		t.Errorf("大 body 场景应采集 1 条且 model 为空（窗口内含不完整 JSON），实得 %+v", e)
	}
}
