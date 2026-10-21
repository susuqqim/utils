package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// 上游 /v2/chat/queue/status 的响应解析：信封 code/data、未知字段保留、业务码翻译。
func TestQueueStatusParsing(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		wantStatus string
		wantBusy   bool
	}{
		{"空闲常态 expired", 200, `{"code":0,"msg":"OK","data":{"status":"expired","message":"queue.expired.title"}}`, "expired", false},
		{"就绪 ready", 200, `{"code":0,"msg":"OK","data":{"status":"ready"}}`, "ready", false},
		{"排队 waiting", 200, `{"code":0,"msg":"OK","data":{"status":"waiting"}}`, "waiting", true},
		{"满员 full", 200, `{"code":0,"msg":"OK","data":{"status":"full"}}`, "full", true},
		{"用户限流 user_limit", 200, `{"code":0,"msg":"OK","data":{"status":"user_limit"}}`, "user_limit", true},
		{"超时 timeout", 200, `{"code":0,"msg":"OK","data":{"status":"timeout"}}`, "timeout", false},
		{"业务码 6021 → full", 200, `{"code":6021,"msg":"queue full"}`, "full", true},
		{"业务码 6020 → waiting", 200, `{"code":6020,"msg":"queue waiting"}`, "waiting", true},
		{"业务码 6022 → user_limit", 200, `{"code":6022,"msg":"user limit"}`, "user_limit", true},
		{"无 status 字段 → unknown", 200, `{"code":0,"data":{}}`, "unknown", false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("应为 POST，实得 %s", r.Method)
			}
			if r.URL.Path != queueStatusPath {
				t.Errorf("路径 %s want %s", r.URL.Path, queueStatusPath)
			}
			// 请求体里必须带 model
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "" {
				t.Error("请求体缺 model")
			}
			if r.Header.Get("X-Conversation-Request-ID") == "" {
				t.Error("缺 X-Conversation-Request-ID")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.httpStatus)
			_, _ = w.Write([]byte(c.body))
		}))

		cl := New()
		cl.HTTP.Transport = &http.Transport{Proxy: nil}
		// 把 chatBase 指到测试服务器：用 GlobalEnabled + 域覆盖最省事的方式是直接构造账号
		a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "127.0.0.1"}
		_ = a
		// chatBase 依赖域名，测试直接替换 transport 后仍会请求真实域名 —— 改为直接调内部方法
		// 这里通过 monkey 方式不可行，故用 RoundTripper 拦截到本地 srv。
		cl.HTTP.Transport = roundTripTo(srv.URL)
		st, err := cl.FetchQueueStatus(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}, "glm-5.2")
		srv.Close()
		if err != nil {
			t.Errorf("%s: 意外错误 %v", c.name, err)
			continue
		}
		if st.Status != c.wantStatus {
			t.Errorf("%s: status=%q want %q", c.name, st.Status, c.wantStatus)
		}
		if st.Busy() != c.wantBusy {
			t.Errorf("%s: Busy()=%v want %v", c.name, st.Busy(), c.wantBusy)
		}
	}
}

// 未知字段必须保留在 Extra（上游随时可能加队列位置/预估等待）。
func TestQueueStatusKeepsUnknownFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"waiting","message":"m","position":7,"estimatedWaitSec":42}}`))
	}))
	defer srv.Close()
	cl := New()
	cl.HTTP.Transport = roundTripTo(srv.URL)
	st, err := cl.FetchQueueStatus(&auth.Auth{UID: "u1", AccessToken: "at"}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if st.Extra["position"] != float64(7) || st.Extra["estimatedWaitSec"] != float64(42) {
		t.Errorf("未知字段丢失: %#v", st.Extra)
	}
}

// 空模型名必须拒绝（避免无意义请求）。
func TestQueueStatusRejectsEmptyModel(t *testing.T) {
	cl := New()
	if _, err := cl.FetchQueueStatus(&auth.Auth{UID: "u1"}, "  "); err == nil {
		t.Error("空模型名应报错")
	}
}

// roundTripTo 把所有请求重定向到测试服务器（保留原路径），不触网。
func roundTripTo(base string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u := base + r.URL.Path
		nr, err := http.NewRequest(r.Method, u, r.Body)
		if err != nil {
			return nil, err
		}
		nr.Header = r.Header
		return http.DefaultTransport.RoundTrip(nr)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
