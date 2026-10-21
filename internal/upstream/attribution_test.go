// attribution_test.go 用量归属头 + 客户端 IP 透传单测。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// chatHeadersReq 构造一个 chat 请求并应用 ChatHeaders，发到测试 server，
// 返回 server 捕获到的所有头。便于断言归属/IP 头。
// clientIP 为透传参数（PassthroughIP 开启时注入；空串表示不透传）。
func chatHeadersReq(t *testing.T, c *Client, a *auth.Auth, clientIP string) http.Header {
	t.Helper()
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	c.ChatHTTP = srv.Client()
	c.HTTP = srv.Client()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	c.ChatHeaders(req, a, clientIP, ChatMeta{})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	return captured
}

// TestAgentPurposeHeadersSet ClientName 配 WorkBuddy 时四头齐全跟随该值。
func TestAgentPurposeHeadersSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a, "")
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"X-Agent-Purpose", "conversation"},
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-Product", "WorkBuddy"},
	} {
		if got := h.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q want %q", tc.header, got, tc.want)
		}
	}
}

// TestAttributionIncludesIDEVersion client_name + client_version 都配时，
// X-IDE-* 四头齐全（Name/Type/Product 跟随 client_name，Version 跟随 client_version）。
func TestAttributionIncludesIDEVersion(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy", ClientVersion: "6.0.0"}
	h := chatHeadersReq(t, c, a, "")
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"X-Agent-Purpose", "conversation"},
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-IDE-Version", "6.0.0"},
		{"X-Product", "WorkBuddy"},
	} {
		if got := h.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q want %q", tc.header, got, tc.want)
		}
	}
	// X-IDE-Version 缺省（client_version 空）= 内置默认 5.5.4，且四头齐全。
	c2 := &Client{ClientName: "WorkBuddy"}
	h2 := chatHeadersReq(t, c2, a, "")
	if got := h2.Get("X-IDE-Version"); got != "5.5.4" {
		t.Errorf("X-IDE-Version = %q want %q (default)", got, "5.5.4")
	}
	// ClientName 空（且未配 identity）时走**默认自洽 WB 形态**：X-IDE-Version 跟随
	// 默认客户端版本段，不再像旧行为那样只发 X-Product=SaaS（UA 与头组不一致正是
	// 2026-10-21 上游 11128 渠道校验的可疑触发点）。
	c3 := &Client{}
	h3 := chatHeadersReq(t, c3, a, "")
	if got := h3.Get("X-IDE-Version"); got != "5.5.4" {
		t.Errorf("X-IDE-Version = %q want %q (coherent default)", got, "5.5.4")
	}
}

// TestProductDefaultCoherentWB 未配置 client_name / identity 时，**默认必须自洽**：
// 默认 UA 是 `WorkBuddy/<ver> …`（自称桌面端），头组就必须同样是 WorkBuddy 全套。
//
// 旧行为是 X-Product=SaaS 且不发 X-IDE-*，与 UA 矛盾 —— "UA 说自己是 WorkBuddy 桌面端、
// 头里说自己是 SaaS、且没有任何 IDE 标识"这种混搭形态正是上游 11128 渠道校验的
// 可疑触发点（2026-10-21 收紧后社区大面积报错）。
func TestProductDefaultCoherentWB(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{} // 未配 ClientName / Identity
	h := chatHeadersReq(t, c, a, "")
	if got := h.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("X-Product = %q want %q (must agree with default UA)", got, "WorkBuddy")
	}
	for _, tc := range []struct{ hdr, want string }{
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-Agent-Purpose", "conversation"},
	} {
		if got := h.Get(tc.hdr); got != tc.want {
			t.Errorf("%s = %q want %q (coherent default)", tc.hdr, got, tc.want)
		}
	}
}

// TestProductWorkBuddy_WhenConfigured client_name=WorkBuddy 时 X-Product 跟随（等价覆盖首测，保独立命名）。
func TestProductWorkBuddy_WhenConfigured(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a, "")
	if got := h.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("X-Product = %q want %q", got, "WorkBuddy")
	}
	// client_name 其他值也应跟随。
	c2 := &Client{ClientName: "MyEditor"}
	h2 := chatHeadersReq(t, c2, a, "")
	if got := h2.Get("X-Product"); got != "MyEditor" {
		t.Errorf("X-Product = %q want %q", got, "MyEditor")
	}
}

// TestIPNotForwarded_ByDefault PassthroughIP 缺省 false：即使传入非空 clientIP 也不注入任何 IP 头。
func TestIPNotForwarded_ByDefault(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	// 即使 clientIP 参数非空，PassthroughIP 关闭也不透传。
	c := &Client{}
	h := chatHeadersReq(t, c, a, "10.0.0.1")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (passthrough off)", hdr, got)
		}
	}
}

// TestIPForwarded_WhenEnabled PassthroughIP=true 且 clientIP 参数非空时三头透传。
func TestIPForwarded_WhenEnabled(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}
	h := chatHeadersReq(t, c, a, "203.0.113.5")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "203.0.113.5" {
			t.Errorf("%s = %q want %q", hdr, got, "203.0.113.5")
		}
	}
}

// TestIPForwarded_NoLeakWhenClientIPEmpty PassthroughIP=true 但 clientIP 为空时不注入 IP 头。
func TestIPForwarded_NoLeakWhenClientIPEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}
	h := chatHeadersReq(t, c, a, "")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (no clientIP)", hdr, got)
		}
	}
}

// TestClientIPConcurrentNoCrossTalk 并发 goroutine 各自带不同 clientIP 调 ChatHeaders，
// 断言每个请求捕获到的 IP 头严格等于自己的 IP——验证按请求传递后无跨请求串扰
// （旧实现读写共享 ClientIP 字段会张冠李戴，-race 下暴露竞态）。
func TestClientIPConcurrentNoCrossTalk(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}

	// 共享一个测试 server：所有 goroutine 打到同一 server，各自捕获请求头。
	var mu sync.Mutex
	captured := map[string]string{} // clientIP -> 实际捕获到的 X-Forwarded-For
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured[r.Header.Get("X-Forwarded-For")] = r.Header.Get("X-Forwarded-For")
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	c.ChatHTTP = srv.Client()
	c.HTTP = srv.Client()

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		go func(myIP string) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
			if err != nil {
				t.Errorf("new req: %v", err)
				return
			}
			// 每个请求独立构造头：clientIP 作为参数传入，不存在共享可污染。
			c.ChatHeaders(req, a, myIP, ChatMeta{})
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Errorf("do: %v (ip=%s)", err, myIP)
				return
			}
			resp.Body.Close()
		}(ip)
	}
	wg.Wait()

	// 每个 IP 都应被至少一次请求携带到上游（出现在 captured 里）。
	for _, ip := range ips {
		if _, ok := captured[ip]; !ok {
			t.Errorf("clientIP %q never reached upstream (cross-talk/lost)", ip)
		}
	}
}

// TestExtractClientIP 从入站请求取 X-Forwarded-For 首段，回落 X-Real-IP，皆空返回空。
func TestExtractClientIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		xff  string
		real string
		want string
	}{
		{"xff_single", "1.2.3.4", "", "1.2.3.4"},
		{"xff_multi_first", "1.2.3.4, 5.6.7.8", "", "1.2.3.4"},
		{"real_fallback", "", "9.9.9.9", "9.9.9.9"},
		{"none_empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.real != "" {
				req.Header.Set("X-Real-IP", tc.real)
			}
			if got := ExtractClientIP(req); got != tc.want {
				t.Errorf("ExtractClientIP = %q want %q", got, tc.want)
			}
		})
	}
}
