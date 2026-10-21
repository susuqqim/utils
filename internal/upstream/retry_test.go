package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TLS 握手超时必须被识别为"可重试的网络层错误"（请求未到达上游）。
func TestIsTLSHandshakeErr(t *testing.T) {
	yes := []string{
		`Post "https://www.workbuddy.ai/console/chat/completions": net/http: TLS handshake timeout`,
		`remote error: tls: handshake failure`,
		`tls: first record does not look like a TLS handshake`,
	}
	for _, s := range yes {
		if !isTLSHandshakeErr(errors.New(s)) {
			t.Errorf("应识别为握手错误: %s", s)
		}
	}
	no := []string{
		`connection reset by peer`,
		`context canceled`,
		`unexpected end of JSON input`,
	}
	for _, s := range no {
		if isTLSHandshakeErr(errors.New(s)) {
			t.Errorf("不应识别为握手错误: %s", s)
		}
	}
}

func TestIsTruncatedJSONErr(t *testing.T) {
	if !isTruncatedJSONErr(errors.New("unexpected end of JSON input")) {
		t.Error("截断应可重试")
	}
	if isTruncatedJSONErr(errors.New("invalid character 'x'")) {
		t.Error("格式错误不应重试")
	}
}

// 响应体被截断时，幂等 RPC 应自动重试一次并成功（模拟"第一次返回半截 JSON"）。
func TestDoJSONRetriesTruncatedBody(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// 第一次：声明 Content-Length 但只写一半就断开
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			b, _ := io.ReadAll(r.Body)
			_ = b
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Total`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// 提前关闭：返回的 body 不完整
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"ok":true}}`))
	}))
	defer srv.Close()

	cl := New()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{"q":1}`))
	data, err := cl.doJSON(req)
	if err != nil {
		t.Fatalf("应重试成功，实得 err=%v (calls=%d)", err, calls)
	}
	if calls != 2 {
		t.Errorf("应恰好重试一次，calls=%d", calls)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Errorf("拿到错误数据: %s", data)
	}
}

// 非幂等/不可重放的 body 不得静默成功：保留原错误。
func TestDoJSONNonRetryableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":11133,"msg":"model_param_invalid"}`))
	}))
	defer srv.Close()

	cl := New()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if _, err := cl.doJSON(req); err == nil {
		t.Fatal("400 不应被重试成功")
	}
}

// 重试必须有上限，不能把持续失败的请求拖成无限循环。
func TestDoJSONBoundedAttempts(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Total`))
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	cl := New()
	cl.HTTP.Timeout = 3 * time.Second
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if _, err := cl.doJSON(req); err == nil {
		t.Fatal("持续截断应返回错误")
	}
	if calls > 2 {
		t.Errorf("重试应有上限，calls=%d", calls)
	}
}
