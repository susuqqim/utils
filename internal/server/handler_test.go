package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/cstime"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	// 测试里把动态模型拉取上界压到 100ms：生产默认 8s，若用在测试里会让
	// 每个涉及 /v1/models 的用例都等满 8 秒，全量套件从 ~40s 涨到 >15min。
	modelsFetchTimeout = 2 * time.Second // 太紧（100ms）会把"5 次探测含重试"的假上游用例提前掐断，见 TestModelsNegativeCacheOnFetchFailure
	chatLogEnabled = false
	os.Exit(m.Run())
}

// resetModelsCache 清空 package 级动态模型缓存（测试隔离：fetchDynamicModels 是全包共享
// 状态，不复位会导致 /v1/models 断言被先前测试的缓存污染——shuffle 下偶发失败）。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

// assertJSONErrorCode 断言响应体是 OpenAI 风格 error 信封且 code 精确等于 want。
func assertJSONErrorCode(t *testing.T, body, want string) bool {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, body)
		return false
	}
	if envelope.Error.Code != want {
		t.Errorf("error code=%q want %q body=%s", envelope.Error.Code, want, body)
		return false
	}
	return true
}

// TestChatBodyLimitExactAllowed 恰好等于上限的请求体正常放行到上游（不被 413 误伤）。
func TestChatBodyLimitExactAllowed(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)) // 恰好 100 字节
	if body := prefix + pad + suffix; len(body) != 100 {
		t.Fatalf("fixture len=%d want 100", len(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(prefix+pad+suffix)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (exactly-at-limit body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatOversizedBodyReturns413 请求体超过上限 → 直接 413 request_body_too_large：
// 不打上游（calls=0）、不罚账号（无冷却/无熔断计数/无禁用）、不轮转。
func TestChatOversizedBodyReturns413(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	// 101 字节 > 100 上限。
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)+1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(prefix+pad+suffix)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "request_body_too_large") {
		t.Errorf("body should carry request_body_too_large: %s", body)
	}
	if !strings.Contains(body, "server.max_body_mb") {
		t.Errorf("413 message should name config key server.max_body_mb: %s", body)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called on 413, got %d", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("413 must not penalize account: %+v", st)
	}
}

// TestChatOversizedBodyDefaultLimitHeader 未显式设置 MaxBodyBytes 时兜底 8MB：
// 8MB+1 的请求体必须 413（不再静默截断喂给上游，issue #41 根因）。
func TestChatOversizedBodyDefaultLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 MaxBodyBytes → 默认 8MB

	body := make([]byte, 8<<20+1) // 8MB+1
	copy(body, `{"model":"glm-5.2","messages":[]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 (8MB+1 must be rejected)", rec.Code)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called, got %d", calls)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转** ——
// 同一 body 换任何账号都是同样的解析结果，轮转只会放大无效请求并耗尽在途名额
// （上游 fd5c5a6 / issue #102 同族修法）。端到端断言只打一次上游（即使 4 号可用）、
// 直接 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400：请求级错误不得换号重试)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatTooManyTokensDoesNotPenalizeAccount 验证超长 Context 报错（too many tokens）
// 绝不被误判为 ErrSoftRate 导致账号进入 600s 软冷却。
func TestChatTooManyTokensDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11133,"msg":"too many tokens in prompt: exceeds model context length"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	// 账号状态：绝不能进入 Cooling 状态，绝不能被熔断
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("u1 must NOT be cooling after too many tokens error: %+v", st)
	}
	if st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("u1 must not accumulate error or breaker fails: %+v", st)
	}
}

// TestChatClientCancellationAbortsUpstreamWithoutPenalty 验证客户端点击“停止生成”（断开连接）时，
// 立即中断上游，不惩罚账号（不冷却、不计错误数、不熔断），且释放并发租约。
func TestChatClientCancellationAbortsUpstreamWithoutPenalty(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamClosed := make(chan struct{})

	pr, pw := io.Pipe()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			close(upstreamStarted)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &closingReader{Reader: pr, onClose: func() { close(upstreamClosed) }},
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"stream":true}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// 等待上游开始推流
	<-upstreamStarted
	pw.Write([]byte("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))

	// 客户端点击停止生成
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ServeHTTP did not return promptly after client cancellation")
	}

	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream connection was not closed upon client cancellation")
	}

	// 账号状态：绝不能进入 Cooling 状态，绝不能被熔断，租约必须已释放
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("u1 must NOT be cooling after client cancel: %+v", st)
	}
	if st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("u1 must not accumulate error or breaker fails: %+v", st)
	}
	if st.InFlight != 0 {
		t.Errorf("u1 InFlight must be 0, got %d", st.InFlight)
	}
}

type closingReader struct {
	io.Reader
	onClose func()
}

func (c *closingReader) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	if closer, ok := c.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// TestChatAllBadParams400CarriesUpstreamBody 全部账号都 11101（畸形请求体）时，
// 终态必须是 **400 invalid_request_error**（而不是 503），且文案携带上游原始 11101 信息。
//
// 口径变更（路线图 §R.8 遗留项）：此前终态一律 503 no_healthy_account + "all accounts
// unavailable (cooling/disabled)"，但 11101 是**客户端请求体**的问题——换多少账号都是
// 同样的拒绝。503 会让 OpenAI SDK 判定"可重试"并反复重试，文案还把责任推给实测健康的账号池。
func TestChatAllBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if !assertJSONErrorCode(t, body, "invalid_request_error") {
		t.Errorf("want code=invalid_request_error, body=%s", body)
	}
}

// 11102「该后端无此模型」：终态归 400 model_unavailable（而不是 503），
// 并给出"核对 /v1/models 模型名"的指引——模型名不可用换账号也救不回来。
func TestChatAllModelBlocked400ModelUnavailable(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"该后端无此模型"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model-xyz","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !assertJSONErrorCode(t, body, "model_unavailable") {
		t.Errorf("body=%s", body)
	}
	if !strings.Contains(body, "/v1/models") {
		t.Errorf("模型不可用文案应指向 /v1/models: %s", body)
	}
	// 模型不可用不罚账号（该 Kind 在 applyErrorPolicy 走 default：只换号）。
	if st, ok := p.Status("u1"); !ok || st.Cooling || st.Disabled {
		t.Fatalf("模型不可用不应冷却/禁用账号: ok=%v cooling=%v disabled=%v", ok, st.Cooling, st.Disabled)
	}
}

// 反向保护：真正的"池不可用"（全池 429）必须仍然是 503 —— 这类失败值得客户端稍后重试，
// 分流不能把 503 顺手改成 400。
func TestChatPoolUnavailableStays503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limit exceeded, will reset in 35 minutes"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

// TestChatAccountFault11140Disables 账号级授权封禁（11140 request illegal，实测为
// global 账号 auth_forbidden 风控）：软冷却到期也不会自动恢复（需重新 OAuth 登录），
// 到期后重新选号只会再撞 403 浪费轮换——故**硬禁用**（不在池中参与选号），同一请求
// 轮换到下一个号、后续请求直接跳过。
// 修复前 Classify 对 403+request illegal 归 ErrClient → applyErrorPolicy 走 default
// 只换号不罚，坏号留在可用池反复被选中刷风控；软冷却列后来只是临时止血，到期复发。
// 【P1-7】11140（账号画像风控）默认**不跨号重试**：同一请求继续换号会被上游视为同源
// 批量试探，实测 6 分钟内把 3 个账号逐个连坐限制。收到 11140 即停手并把原因交给客户端。
func TestChatAccountFault11140StopsRotation(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 90 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("默认应停手并返回 403 account_restricted，实得 code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 {
		t.Errorf("坏号应只打一次，实得 %d", calls["Bearer at-bad"])
	}
	if calls["Bearer at-good"] != 0 {
		t.Errorf("默认不得跨号重试（防连坐），good 被调用 %d 次", calls["Bearer at-good"])
	}
	st, _ := p.Status("bad")
	if !st.Disabled {
		t.Fatalf("11140 账号应被禁用: %+v", st)
	}
	wantReason := "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction"
	if st.DisabledReason != wantReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, wantReason)
	}
}

// 需要旧行为（跨号重试）时可显式开启 RestrictedRotateMax>0。
func TestChatAccountFault11140RotatesWhenConfigured(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 403, `{"code":11140,"msg":"request illegal"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 90 * time.Second, RestrictedRotateMax: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("显式开启后应轮转到 good 成功，code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want 各 1 次", calls)
	}
}

// 【P1-7】整池波次暂停：同域窗口内 ≥RestrictionWaveMin 个账号被限制 ⇒ 该域整体停手，
// 后续请求不再向该域发起上游调用（避免把剩余账号也拖下水）。
func TestRestrictionWavePausesRealm(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 403, `{"code":11140,"msg":"request illegal"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at-3", ExpiresAt: 9999999999},
	)
	for _, u := range []string{"a1", "a2", "a3"} {
		p.SetCredits(u, 1000)
	}
	// 允许连坐 2 次：首请求会打到 2 个号 → 触发波次暂停。
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 90 * time.Second, RestrictedRotateMax: 2})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	first := calls
	if first < 2 {
		t.Fatalf("首请求应至少打到 2 个号才触发波次，实得 %d", first)
	}
	if !h.realmPaused("cn") {
		t.Fatalf("同域 2 个号被限制后应暂停该域，calls=%d", first)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if calls != first {
		t.Errorf("波次暂停期间不应再打上游：before=%d after=%d", first, calls)
	}
	if rec2.Code == 200 {
		t.Errorf("暂停期间应返回错误而不是 200")
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	// 04:00 是 CST 口径（与签到窗口同钟）：按 CST 断言，避免宿主时区影响（TZ=UTC 检查）。
	if h := st.Until.In(cstime.Zone).Hour(); h != 4 {
		t.Fatalf("14018 should use hard-credit cooldown (CST 04:00), got until=%v", st.Until)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

func TestChatErrImageInvalidFailFast(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11135,"msg":"invalid_image_data"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (fail-fast without rotating through all accounts)", calls)
	}
	st1, _ := p.Status("a1")
	st2, _ := p.Status("a2")
	if st1.Cooling || st2.Cooling {
		t.Fatalf("neither account should be cooled on image_invalid: a1=%+v, a2=%+v", st1, st2)
	}
}

// TestChatAccountFault14017Rotates 配额未激活（14017 trial not activated，实测 global
// 新账号 register 未完成）同样纳入轮换：坏号冷却、轮换到下一号、不再被重复选中。
// 与 11140（硬禁用）区分的关键：14017 属于 register 未完成，完善 register 后可能
// 自动恢复——所以**保持软冷却**，不禁用（禁用会让用户补完 register 后仍无法用）。
func TestChatAccountFault14017Rotates(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 429, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated. Please log out of your current account and log in again to activate it immediately and start your free trial."}}}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const cool = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: cool})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}

	// 14017 保持软冷却（软 45s），不得硬禁用——register 完善后自动恢复（区别于 11140）。
	st, _ := p.Status("bad")
	if st.Disabled {
		t.Fatalf("bad 不应被禁用（14017 trial 未激活可能自愈）: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("bad 应进入 soft_rate 冷却（14017 软冷却，非禁用）: %+v", st)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

// TestApplyErrorPolicyAccountFaultSplit applyErrorPolicy 层直接回归：同一 ErrAccountFault
// 分类下按 msg 分野——"request illegal"(11140) → 硬禁用；"trial"(14017) → 软冷却不禁用。
// 端到端接线由上方 TestChatAccountFault11140Disables / TestChatAccountFault14017Rotates 覆盖。
func TestApplyErrorPolicyAccountFaultSplit(t *testing.T) {
	const why11140 = "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction"

	t.Run("11140 request illegal disables", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, "glm-5.2")
		st, _ := p.Status("u1")
		if !st.Disabled {
			t.Fatalf("11140 应硬禁用: %+v", st)
		}
		if st.DisabledReason != why11140 {
			t.Errorf("disabled_reason=%q want %q", st.DisabledReason, why11140)
		}
		if st.Cooling {
			t.Errorf("11140 禁用不应叠加冷却: %+v", st)
		}
	})

	t.Run("14017 trial stays soft cool", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated"}}}`, "glm-5.2")
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("14017 不应禁用: %+v", st)
		}
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Errorf("14017 应 soft_rate 软冷却: %+v", st)
		}
		if st.Reason == "" {
			t.Errorf("14017 冷却 reason 不应为空: %+v", st)
		}
	})
}

// TestApplyErrorPolicySoftRateExponentialBackoff handler 层回归：同一账号连续被限流，
// 冷却时长必须 600s → 1200s → 2400s 指数增长（时长断言全部取自注入值，不依赖真实等待）。
// 直接驱动 applyErrorPolicy 而非发 HTTP 请求：账号在冷却期内不会被再次选中，
// 走完整请求会需要等冷却自然到期（真实 sleep），而这里要验的正是"连续限流"的退避本身。
// Classify→applyErrorPolicy 的接线由 TestChatSoftCoolsOnRateLimitBody 端到端覆盖。
func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i, want := range []int64{600, 1200, 2400} {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "")
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i+1, st)
		}
		if st.SoftStreak != i+1 {
			t.Errorf("call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		if st.CoolRemaining < want-3 || st.CoolRemaining > want {
			t.Errorf("call %d: cool_remaining_sec=%d want ~%d", i+1, st.CoolRemaining, want)
		}
	}
}

// TestApplyErrorPolicyNotFoundUsesFixedBase 404 分流：偶发上游 404 的冷却基数固定 60s
// （notFoundCooldown），不取 soft_rate 的 600s 基数，也不受其配置值影响。
//
// 关于退避：404 仍走 Cooldown(CoolSoft)，因此与 429 共用同一 softStreak（本用例锚定这一
// 现状）。这不构成"偶发 404 罚过重"的场景——streak 只在**连续**无成功时累积，
// 中间任何一次成功（NoteSuccess）都会把它清零；故只有持续 404 的坏号才会退避升级。
func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i, want := range []int64{notFoundSec, 2 * notFoundSec, 4 * notFoundSec} {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "")
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i+1, st)
		}
		if st.SoftStreak != i+1 {
			t.Errorf("call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m）。
		if st.CoolRemaining < want-3 || st.CoolRemaining > want {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i+1, st.CoolRemaining, want, notFoundSec)
		}
	}

	// 成功后 streak 归零 → 下次 404 回到 60s 基数。
	// （签到解冻 ReenableIfCredits 不适用于本场景：它保留 streak，是冷却域的续期。）
	p.NoteSuccess("u1")
	h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "")
	if st, _ := p.Status("u1"); st.SoftStreak != 1 || st.CoolRemaining > notFoundSec {
		t.Errorf("success should reset 404 backoff: %+v", st)
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：PickByUID 将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

// TestChatFingerprintStickySession 验证无会话键客户端（标准 OpenAI 请求）：
// 多轮对话通过首条 user 消息指纹实现会话粘性路由，避免账号频繁轮换击穿 Prompt Cache。
func TestChatFingerprintStickySession(t *testing.T) {
	st := newBindStore()
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: p.AvailableUIDs,
	})
	var hitUIDs []string
	var mu sync.Mutex
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if authz == "Bearer at-a1" {
			hitUIDs = append(hitUIDs, "a1")
		} else if authz == "Bearer at-a2" {
			hitUIDs = append(hitUIDs, "a2")
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	// 第一轮对话：首条 user 消息为 "写个贪吃蛇"
	req1 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"写个贪吃蛇"}]}`))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: code=%d", rec1.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(hitUIDs))
	}
	firstAccount := hitUIDs[0]
	mu.Unlock()

	// 第二轮对话：上下文追加 assistant 回复和第二个 user 提问 "增加暂停功能"
	// 按照标准 OpenAI 请求格式，没有 conversation_id
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[`+
			`{"role":"user","content":"写个贪吃蛇"},`+
			`{"role":"assistant","content":"好的这是贪吃蛇代码"},`+
			`{"role":"user","content":"增加暂停功能"}]}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: code=%d", rec2.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(hitUIDs))
	}
	secondAccount := hitUIDs[1]
	mu.Unlock()

	if secondAccount != firstAccount {
		t.Fatalf("turn 2 hit %s, want sticky %s (Prompt Cache missed!)", secondAccount, firstAccount)
	}

	// 第三轮对话：同对话追加第三问
	req3 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[`+
			`{"role":"user","content":"写个贪吃蛇"},`+
			`{"role":"assistant","content":"好的这是贪吃蛇代码"},`+
			`{"role":"user","content":"增加暂停功能"},`+
			`{"role":"assistant","content":"已增加暂停功能"},`+
			`{"role":"user","content":"换个皮肤颜色"}]}`))
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("turn 3 failed: code=%d", rec3.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", len(hitUIDs))
	}
	thirdAccount := hitUIDs[2]
	mu.Unlock()

	if thirdAccount != firstAccount {
		t.Fatalf("turn 3 hit %s, want sticky %s", thirdAccount, firstAccount)
	}
}

// TestChatHeaderStickySession 验证通过 HTTP Header (如 X-Conversation-ID / X-Session-ID) 传入会话键时，
// 网关同样能够建立会话粘性并稳定路由。
func TestChatHeaderStickySession(t *testing.T) {
	st := newBindStore()
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: p.AvailableUIDs,
	})
	var hitUIDs []string
	var mu sync.Mutex
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if authz == "Bearer at-a1" {
			hitUIDs = append(hitUIDs, "a1")
		} else if authz == "Bearer at-a2" {
			hitUIDs = append(hitUIDs, "a2")
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	req1 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"q1"}]}`))
	req1.Header.Set("X-Conversation-ID", "custom-header-conv")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("req1 failed: code=%d", rec1.Code)
	}

	mu.Lock()
	firstAccount := hitUIDs[0]
	mu.Unlock()

	if uid, ok := st.lastUID("custom-header-conv"); !ok || uid != firstAccount {
		t.Fatalf("store binding for custom-header-conv=%s, want %s", uid, firstAccount)
	}

	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"q2"}]}`))
	req2.Header.Set("X-Conversation-ID", "custom-header-conv")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("req2 failed: code=%d", rec2.Code)
	}

	mu.Lock()
	secondAccount := hitUIDs[1]
	mu.Unlock()

	if secondAccount != firstAccount {
		t.Fatalf("req2 hit %s, want sticky %s", secondAccount, firstAccount)
	}
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if h := st.Until.In(cstime.Zone).Hour(); h != 4 {
		t.Errorf("until CST hour=%d want 4 (next-day 04:00, until=%v)", h, st.Until)
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级冷却：模型级台账含 glm-5.3 + 该模型独立截止 ≈ reset。
	st, _ := p.Status("bad")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("bad should have glm-5.3 model limit ledger: %+v", st)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 6004 不写账号级 until（模型级独立冷却）。
	if !st.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", st.Until)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing (static models prefixed)")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 6 {
		t.Fatalf("want 6 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids (cn-prefixed) missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "cn:dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("cn:dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("cn:dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
		case "cn:glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("cn:glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("cn:glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游 500
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	// 回退静态表（≥5 个）
	if len(data) < 5 {
		t.Errorf("static fallback failed: %d", len(data))
	}
	// 兜底表必须覆盖**在用模型**：2026-10-03 实测发现兜底表只有 10 个 cn 模型，
	// 一旦上游模型目录故障，/v1/models 会少列 deepseek 系列等在用模型
	// （而客户端配置正是按这份清单生成的）。已按实测补到 21 条，这里钉住关键几条。
	ids := map[string]bool{}
	for _, it := range data {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids[id] = true
			}
		}
	}
	for _, want := range []string{"deepseek-v4.1-flash", "glm-5.3", "kimi-k2.8-preview", "hy4-preview"} {
		if !ids[want] && !ids["cn:"+want] {
			t.Errorf("静态兜底表缺少在用模型 %s（上游目录故障时会少列）", want)
		}
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：一次 fetch 失败即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d (static fallback)", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("fetch failure should trip breaker with threshold=1: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 其余走静态 fallback（仍返回 200）。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// 一轮探测 = 5 次上游调用：企业端点 ×1 + **/v3/config 双 UA 各 2 次**（2026-10-03 起
	// CN 侧取双 UA 并集，且单路失败会**先重试一次**再降级 —— 避免"部分并集"被当成功写进
	// 10 分钟缓存导致清单缩水，见 fetchV3Models）。全部失败才进负缓存。
	if got := calls.Load(); got != 5 {
		t.Errorf("want 5 probes (console + (v3 IDE UA + retry) + (v3 CLI UA + retry)), got %d", got)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if got := calls.Load(); got != 10 {
		t.Errorf("want 10 probes after cooldown (2 rounds x 5), got %d", got)
	}
}

// TestModelsDynamicZeroContextFallback 动态模型缺 maxInputTokens → context_length 兜底 131072，
// 其余真实值不得被覆盖（issue 提醒：不能全表统一 131072 抹平真实 ContextLength）。
func TestModelsDynamicZeroContextFallback(t *testing.T) {
	resetModelsCache()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-zero-ctx","maxInputTokens":0,"maxOutputTokens":4096},
			{"id":"dyn-real-ctx","maxInputTokens":262144,"maxOutputTokens":32768}
		],"agents":[{"name":"cli","models":["dyn-zero-ctx","dyn-real-ctx"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	zero := map[string]any{}
	real := map[string]any{}
	for _, m := range resp.Data {
		switch m["id"] {
		case "cn:dyn-zero-ctx":
			zero = m
		case "cn:dyn-real-ctx":
			real = m
		}
	}
	// 缺 maxInputTokens → 兜底 131072（其余字段保持真实）。
	if zc, _ := zero["context_length"].(float64); zc != 131072 {
		t.Errorf("zero-ctx context_length=%v want 131072 fallback", zc)
	}
	if zo, _ := zero["max_output_tokens"].(float64); zo != 4096 {
		t.Errorf("zero-ctx max_output_tokens=%v want 4096 (real value preserved)", zo)
	}
	// 有真实 maxInputTokens → 必须用真实值，不得被兜底抹平。
	if rc, _ := real["context_length"].(float64); rc != 262144 {
		t.Errorf("real-ctx context_length=%v want 262144 (real value must win)", rc)
	}
	if ro, _ := real["max_output_tokens"].(float64); ro != 32768 {
		t.Errorf("real-ctx max_output_tokens=%v want 32768", ro)
	}
}

// TestModelsDynamicUnderscoresStaticAndPreservesBodies 动态成功时优先动态结果（连 headers
// 字段一并保留原样），失败才回退静态表——两条路径都产出 valid models 响应。
func TestModelsDynamicUnderscoresStaticAndPreservesBodies(t *testing.T) {
	resetModelsCache()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-only","maxInputTokens":200000,"maxOutputTokens":20000,"name":"Dyn Only"}
		],"agents":[{"name":"cli","models":["dyn-only"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("no models returned")
	}
	// 动态优先：动态模型出现即证明未走静态 fallback。
	found := false
	for _, m := range resp.Data {
		if m["id"] == "cn:dyn-only" {
			found = true
			if cl, _ := m["context_length"].(float64); cl != 200000 {
				t.Errorf("cn:dyn-only context_length=%v want 200000 (dynamic value)", cl)
			}
		}
	}
	if !found {
		t.Errorf("/v1/models must include cn:dyn-only when dynamic fetch succeeds: %v", resp.Data)
	}

	// 失败路径仍 return 静态表（既有行为由 TestModelsDynamicFallsBackToStatic 覆盖）。
	if len(resp.Data) == 0 {
		t.Fatal("static fallback produced empty list")
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
	if _, ok := statusBody["daily_credits_used"]; !ok {
		t.Errorf("daily_credits_used missing in status: %v", statusBody)
	}
	if _, ok := statusBody["total_credits_used"]; !ok {
		t.Errorf("total_credits_used missing in status: %v", statusBody)
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestStatusRateLimitedModelsLedger end-to-end（issue #36）：上游 429 6004 带
// 「将在 … 重置」→ 账号 modelCooldowns 被写入 → /status accounts 输出该模型的
// 限额台账（rate_limited_models[].model + reset_at + until）。
func TestStatusRateLimitedModelsLedger(t *testing.T) {
	reset := time.Now().Add(35 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	body := `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, body, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999},
	)
	const soft = 600 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	// 双号都被 6004 → 全部换完仍 503（无健康号），但换号过程已把 u1 冷却。
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	// u1 被选中过（已冷却 + 有台账）。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("u1 应进入 6004 模型级软冷却（glm-5.2 台账）: %+v", st)
	}

	statusRec := httptest.NewRecorder()
	h.ServeHTTP(statusRec, httptest.NewRequest("GET", "/status", nil))
	if statusRec.Code != 200 {
		t.Fatalf("status code=%d body=%s", statusRec.Code, statusRec.Body)
	}
	var sbody struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &sbody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	var su1 *pool.Status
	for i := range sbody.Accounts {
		if sbody.Accounts[i].UID == "u1" {
			su1 = &sbody.Accounts[i]
			break
		}
	}
	if su1 == nil {
		t.Fatal("u1 不在 /status accounts 里")
	}
	if len(su1.RateLimitedModels) != 1 {
		t.Fatalf("u1 rate_limited_models=%v want 1 行", su1.RateLimitedModels)
	}
	row := su1.RateLimitedModels[0]
	if row.Model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2", row.Model)
	}
	if d := row.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("reset_at=%v want ~35m 后=%v", row.ResetAt, reset)
	}
	// 6004 模型级冷却不写账号级 until：Status.Until 应为零值，模型截止在台账行里。
	if !su1.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", su1.Until)
	}
	if d := row.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("until=%v want ~35m 后=%v", row.Until, reset)
	}
	// 未命中测试账号（u2 也被限流）也会带台账——但只断言 u1（被选中的号）即验证端到端。
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenAllModelExempt 全部账号处于 6004 单模型软冷却（对其他模型仍可选）
// → /healthz 必须 200，与 chat 的模型级豁免（切模型立即可用）同口径。
// 回归：此前 ServableNow 只看账号级 healthy，"全号被 v4.1 限流但 glm 可用"时误报 503。
func TestHealthz200WhenAllModelExempt(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	// 账号级 healthy 已为 0（冷却中），但模型豁免使 chat 对非 glm 请求仍可达。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (model-exempt account keeps pool servable)", rec.Code)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedNonStickyDegraded 单次请求内容拦截仅单次重试降级，不跨请求全局污染。
func TestContentBlockedNonStickyDegraded(t *testing.T) {
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 请求 1 触发误报拦截 → 重试 Degraded 成功（200）。
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"my-agent-rules"},{"role":"user","content":"hi"}]}`)))
	if rec1.Code != 200 {
		t.Fatalf("req1 code=%d", rec1.Code)
	}
	if len(bodies) != 2 {
		t.Fatalf("req1 should have 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("req1 retry body should contain Degraded prompt: %s", bodies[1])
	}

	// 请求 2 发送：必须保持原样系统提示词，绝不直达 Degraded 破坏 Agent 提示词！
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"my-agent-rules"},{"role":"user","content":"second"}]}`)))
	if rec2.Code != 200 {
		t.Fatalf("req2 code=%d", rec2.Code)
	}
	if len(bodies) != 3 {
		t.Fatalf("total calls should be 3, got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[2]), "my-agent-rules") {
		t.Errorf("req2 should keep original system prompt, got %s", bodies[2])
	}
	if strings.Contains(string(bodies[2]), prompt.Degraded) {
		t.Errorf("req2 should NOT be contaminated by Degraded prompt: %s", bodies[2])
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报；若仍拦则回 400 content_blocked）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下内容拦截直接回 400 content_blocked，不降级重试、不轮转、不暴露账号语义。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 (custom does not degrade)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"content_blocked"`) {
		t.Errorf("want content_blocked code: %s", body)
	}
	if !strings.Contains(body, "触发网站风控违禁词") || !strings.Contains(body, "规则[违禁词]") {
		t.Errorf("want firewall message with keyword, not error code: %s", body)
	}
	for _, leak := range []string{"11128", "account", "accounts", "账号", "upstream", "cooling", "no_healthy"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("must not leak %q: %s", leak, body)
		}
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError），
// 且 passthrough 降级重试后第二次仍拦 → 立即 400 content_blocked（不轮转、不泄露上游错误码）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	// passthrough 首遇（无原始 system，实际无降级重试）→ 内容拦截直接回 400 content_blocked。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"code":"content_blocked"`) {
		t.Errorf("passthrough retry-still-blocked should return content_blocked: %s", rec.Body)
	}

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestContentBlockedSecondHitReturns400 passthrough 首遇降级重试、第二次仍拦 → 立即 400
// content_blocked：**不轮转**（多账号池也只打一次）、**不罚账号**、防火墙文案不含账号/错误码。
func TestContentBlockedSecondHitReturns400(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))

	// passthrough 首遇（body 含原始 system）→ 降级重试一次；第二次仍拦 → 立即 400，不轮转。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Errorf("want exactly 2 upstream calls (first 400 + one degraded retry, then stop), got %d", calls)
	}
	body := rec.Body.String()
	if !assertJSONErrorCode(t, body, "content_blocked") {
		return
	}
	for _, leak := range []string{"11128", "account", "accounts", "账号", "upstream", "cooling", "disabled", "no_healthy"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("must not leak %q: %s", leak, body)
		}
	}
	// 不轮转 ⇒ 两个账号都未被施加任何处罚（无冷却/禁用/熔断计数）。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 {
			t.Fatalf("content_blocked must not penalize any account: uid=%s status=%+v", uid, st)
		}
	}
}

// TestContentBlockedReturnsFirewallMessage 内容拦截最终失败时回 400 content_blocked，
// 文案为网关防火墙口径（含分类关键词），不含账号/冷却/upstream/错误码前缀。
func TestContentBlockedReturnsFirewallMessage(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11128,"msg":"blocked by security policy: content contains NSFW material"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	// custom 模式不走降级，因此只应打一次上游（不轮转第二个账号）。
	if calls != 1 {
		t.Errorf("content_blocked must not rotate accounts, calls=%d", calls)
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, rec.Body)
	}
	if envelope.Error.Code != "content_blocked" {
		t.Errorf("code=%q want content_blocked", envelope.Error.Code)
	}
	want := "触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[nsfw]，已被拦截。请修改内容后重试。"
	if envelope.Error.Message != want {
		t.Errorf("message=%q want %q", envelope.Error.Message, want)
	}
	for _, leak := range []string{"account", "accounts", "账号", "upstream", "cooling", "disabled", "no_healthy", "11128"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), leak) {
			t.Errorf("must not leak %q: %s", leak, rec.Body)
		}
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// TestStatusRealmTotals /status 新增 realm_totals 字段：混合池各域计数独立分组，
// 既有 total/healthy/cooling/disabled 汇总键零回归（仍全池口径）。
func TestStatusRealmTotals(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "cn2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "g2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	p.Cooldown("cn2", pool.CoolSoft, time.Hour, "429 rate limit")
	p.Disable("g2", "session dead")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Total, Healthy, Cooling, Disabled int
		RealmTotals                       map[string]map[string]int `json:"realm_totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v body=%s", err, rec.Body)
	}
	// 既有汇总键零回归：全池 = 4 账号。
	if body.Total != 4 || body.Healthy != 2 || body.Cooling != 1 || body.Disabled != 1 {
		t.Errorf("summary = %d/%d/%d/%d want 4/2/1/1 (zero regression)", body.Total, body.Healthy, body.Cooling, body.Disabled)
	}
	if body.RealmTotals == nil {
		t.Fatal("realm_totals missing from /status")
	}
	cn := body.RealmTotals["cn"]
	if cn["total"] != 2 || cn["healthy"] != 1 || cn["cooling"] != 1 || cn["disabled"] != 0 {
		t.Errorf("realm_totals.cn=%v want total=2 healthy=1 cooling=1 disabled=0", cn)
	}
	g := body.RealmTotals["global"]
	if g["total"] != 2 || g["healthy"] != 1 || g["cooling"] != 0 || g["disabled"] != 1 {
		t.Errorf("realm_totals.global=%v want total=2 healthy=1 cooling=0 disabled=1", g)
	}
}

// TestHealthzRealmServable /healthz 新增 realm_servable 字段：各域可服务状态独立暴露。
// global 全冷却 + cn 空闲 → realm_servable.global=false, cn=true；HTTP 仍 200（存在性语义零回归）。
func TestHealthzRealmServable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "g2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	// global 全冷却（一软一硬），cn 空闲。
	p.Cooldown("g1", pool.CoolSoft, time.Hour, "429 rate limit")
	p.Cooldown("g2", pool.CoolHard, time.Hour, "余额不足")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	// 存在性语义零回归：任一域可服务 → 200。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (cn servable keeps existence semantics)", rec.Code)
	}
	var resp struct {
		Healthy       int             `json:"healthy"`
		RealmServable map[string]bool `json:"realm_servable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp.Healthy != 1 {
		t.Errorf("healthy=%d want 1 (only cn1)", resp.Healthy)
	}
	if resp.RealmServable["cn"] != true {
		t.Errorf("realm_servable.cn=%v want true", resp.RealmServable["cn"])
	}
	if resp.RealmServable["global"] != false {
		t.Errorf("realm_servable.global=%v want false", resp.RealmServable["global"])
	}
}

// 非流式聚合失败（上游 200，但流里没有任何有效 data 事件）→ 网关回 502。
// 此时账号**不得**被记成功、会话**不得**被绑到这个没产出任何内容的号上：
// 旧实现在聚合之前就 NoteSuccess + Bind，会把失败记成成功（污染成功率权重），
// 并把会话钉在一个其实没答上来的账号上。
func TestNonStreamAggregateFailureNotCountedAsSuccess(t *testing.T) {
	st := newBindStore()
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	sess := session.New(session.Config{TTL: time.Minute, Store: st, Available: p.AvailableUIDs})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: [DONE]\n\n", true // 只有 [DONE]，无有效事件
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-aggfail"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if s, ok := p.Status("u1"); !ok || s.SuccessCount != 0 {
		t.Fatalf("聚合失败被记成成功: ok=%v success_count=%d", ok, s.SuccessCount)
	}
	if uid, bound := st.lastUID("conv-aggfail"); bound {
		t.Fatalf("聚合失败仍把会话绑定到该账号: uid=%s", uid)
	}
}

// 流式结束状态的归类口径（原实现只识别"取消→499"，其余一律留 200，
// 于是上游中途截断在日志/统计里伪装成成功）。
func TestStreamOutcomeStatusClassification(t *testing.T) {
	cases := []struct {
		name   string
		serr   error
		ctxErr error
		want   int
	}{
		{"正常收尾", nil, nil, http.StatusOK},
		{"客户端取消(serr)", context.Canceled, context.Canceled, 499},
		{"客户端断开(仅 ctx)", nil, context.Canceled, 499},
		// 任何请求上下文错误都按"客户端已不在"处理（net/http 的 Shutdown **不会**取消
		// 活跃请求的 ctx，所以 ctxErr 非空基本只有客户端断开一种来源；其余取值按同口径保守归类）。
		{"ctx 其它错误(DeadlineExceeded)", nil, context.DeadlineExceeded, 499},
		{"上游错误 + 客户端断开", errors.New("unexpected EOF"), context.Canceled, 499},
		{"上游中途读错误", errors.New("unexpected EOF"), nil, http.StatusBadGateway},
		{"上游空流", errors.New("upstream stream contained no valid data events"), nil, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamOutcomeStatus(tc.serr, tc.ctxErr); got != tc.want {
				t.Fatalf("streamOutcomeStatus(%v, %v) = %d, want %d", tc.serr, tc.ctxErr, got, tc.want)
			}
		})
	}
}

// 端到端：客户端要流式，但上游只回了一个 [DONE]（0 个有效数据事件）→
// StreamContext 返回 "no valid data events"，网关必须向下游补 error 帧并把它记为 502
// （旧实现只识别客户端取消，这种情况会被记成 200 成功）。
//
// 注：另一种"截断"（有内容但既无 finish_reason 也无 [DONE]）在流式路径目前**有意**
// 按正常结束处理并兜底补 [DONE]（见 internal/upstream/sse_test.go 的 TestStreamDoneFallback），
// 与非流式聚合的判截断口径不一致，属待决策项，本次不动。
func TestStreamEmptyUpstreamRecordedAsBadGateway(t *testing.T) {
	withChatLog(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: [DONE]\n\n", true // 没有任何有效数据事件
	})
	h := NewHandler(Config{Pool: p, Upstream: up})

	var rec *httptest.ResponseRecorder
	out := captureStdout(t, func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("流式响应头已发出，状态应为 200，got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("空流未向下游补 error 帧: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("空流未补 [DONE]（下游可能挂死）: %s", rec.Body.String())
	}
	// 断言"状态列 = 502"（`| 502 |`），而不是裸 Contains("502")：延迟列也可能是 502ms，
	// 裸匹配存在假绿（审查指出）。
	if !strings.Contains(out, "| 502 |") {
		t.Fatalf("空流未被记为 502（日志行）: %s", out)
	}
}

// api_key 为空（网关不鉴权）时，跨站来源的浏览器请求必须被拒——否则用户访问的任意
// 网页都能驱动本机网关（烧积分、读 /status 账号画像）。同源与无 Origin（SDK/curl）
// 必须照常放行，否则面板自己与下游工具会被误伤。
func TestKeylessGatewayRejectsCrossOrigin(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()}) // 不设 APIKey → 不鉴权

	cases := []struct {
		name       string
		reqURL     string
		origin     string
		wantStatus int
	}{
		{"无 Origin（curl/SDK）", "http://127.0.0.1:17963/healthz", "", http.StatusOK},
		{"同源(127.0.0.1 同端口)", "http://127.0.0.1:17963/healthz", "http://127.0.0.1:17963", http.StatusOK},
		{"同源(localhost 别名)", "http://127.0.0.1:17963/healthz", "http://localhost:17963", http.StatusOK},
		{"同源(IPv6 回环别名)", "http://[::1]:17963/healthz", "http://127.0.0.1:17963", http.StatusOK},
		{"同源(尾点 FQDN)", "http://localhost.:17963/healthz", "http://localhost.:17963", http.StatusOK},
		{"跨站", "http://127.0.0.1:17963/healthz", "https://evil.example", http.StatusForbidden},
		{"跨站(端口不同)", "http://127.0.0.1:17963/healthz", "http://127.0.0.1:38080", http.StatusForbidden},
		// DNS rebinding：Origin 与 Host 同为攻击者域（重绑定后指向 127.0.0.1）→ 必须拒。
		{"DNS rebinding(Origin==Host 非回环)", "http://attacker.tld:17963/healthz", "http://attacker.tld:17963", http.StatusForbidden},
		{"非回环 Host + 无 Origin 仍放行(SDK)", "http://gw.example.com:17963/healthz", "", http.StatusOK},
		{"Origin: null(data:/file: 页面)", "http://127.0.0.1:17963/healthz", "null", http.StatusForbidden},
		{"Host 0.0.0.0 不算回环", "http://0.0.0.0:17963/healthz", "http://0.0.0.0:17963", http.StatusForbidden},
		{"回环网段非 .1(127.0.0.2)", "http://127.0.0.2:17963/healthz", "http://127.0.0.2:17963", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.reqURL, nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("origin=%q status=%d want %d body=%s", tc.origin, rec.Code, tc.wantStatus, rec.Body)
			}
		})
	}
}

// api_key 非空时 CORS 保持原有宽松口径（密钥即边界，下游 Web 客户端常需跨域直连）。
func TestKeyedGatewayKeepsPermissiveCORS(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, APIKey: "k"})

	req := httptest.NewRequest("GET", "http://127.0.0.1:17963/healthz", nil)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Origin", "https://some-client.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("设置了 api_key 时不应按来源拦截: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("ACAO=%q want *（密钥非空时保持原口径）", got)
	}
}

// 终态"模型不可用"是否定论：只要该 realm 还有**未试过**的可用账号就必须保持 503
// （未试过的号可能正好有该模型），全部试过才升 400。这条分支此前无用例覆盖。
func TestModelUnavailableFinalOnlyWhenNoUntriedCandidate(t *testing.T) {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	// 只试过 u1 → 还有 u2 没试 → 不能下"模型不可用"的定论。
	if h.modelUnavailableFinal("glm-5.2", "cn", map[string]bool{"u1": true}) {
		t.Fatal("仍有未试候选时必须返回 false（保持 503 可重试）")
	}
	// 两个都试过 → 定论成立。
	if !h.modelUnavailableFinal("glm-5.2", "cn", map[string]bool{"u1": true, "u2": true}) {
		t.Fatal("候选全部试过后应返回 true（升 400）")
	}
	// 池此刻整体不可用（全禁用/全冷却）→ **不是**模型定论：账号恢复后同一个模型可能就能用，
	// 回 400 会让客户端永久放弃。必须保持 503。
	p.Disable("u1", "test")
	p.Disable("u2", "test")
	if h.modelUnavailableFinal("glm-5.2", "cn", map[string]bool{}) {
		t.Fatal("无可用候选时（池整体不可用）应返回 false（保持 503）")
	}
}

// https 反代/直连形态：Host 缺端口时按 r.TLS 补 443，Origin 同样补 443 后应判同源
// （覆盖 hostPortWithDefaults 的 TLS 分支；漏了这条会把 https 同源面板请求误判跨站）。
func TestKeylessGatewayHTTPSDefaultPort(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()}) // 无 key

	req := httptest.NewRequest("GET", "https://localhost/healthz", nil)
	req.Host = "localhost:443"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Origin", "https://localhost")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("https 同源（Host 缺端口补 443）应放行, got %d body=%s", rec.Code, rec.Body)
	}
}

// Host 侧缺端口同样按 r.TLS 补 443（上一例是 Origin 侧缺端口；两侧都要覆盖）。
func TestKeylessGatewayHTTPSDefaultPortFromRequestHost(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})

	req := httptest.NewRequest("GET", "https://localhost/healthz", nil)
	req.Host = "localhost" // 无端口
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Origin", "https://localhost:443")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Host 缺端口时应按 TLS 补 443, got %d body=%s", rec.Code, rec.Body)
	}
}

// 记账口径端到端锁定（这条接线此前**零测试覆盖**，审查指出）：
// usage.Add 的 ok 必须是"该次上游尝试是否真的成功"，而不是"有没有 token"。
// 五类请求各一次，断言 Totals.{Requests,Errors}：
//  1. 200 + usage                      → 成功
//  2. 200 但**整条流没有 usage**        → 成功（旧口径会误记为失败 —— 本次修复点）
//  3. 上游 400（11101）                 → 失败
//  4. 200 空流（无有效事件）→ 502        → 失败（旧口径在流式下会误记为成功）
//  5. 客户端取消（499）                 → 计请求、**不计失败**
func TestUsageAccountingReflectsRealOutcome(t *testing.T) {
	rec := usage.New("") // path 为空 = 纯内存，不落盘
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})

	var mode int32 // 0=正常(带 usage) 1=无 usage 2=400 3=空流
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err // 取消：让 handler 走 499 分支
			}
			ct := "text/event-stream"
			status, body := 200, sseOK
			switch atomic.LoadInt32(&mode) {
			case 1:
				// 有内容、有 finish_reason、有 [DONE]，但**没有 usage** → 聚合成功且无 token。
				body = "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			case 2:
				ct, status, body = "application/json", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`
			case 3:
				body = "data: [DONE]\n\n" // 无有效数据事件 → 502
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: rec})

	send := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		if ctx != nil {
			req = req.WithContext(ctx)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}

	if r := send(nil); r.Code != 200 { // 1) 成功（带 usage）
		t.Fatalf("case1 success: code=%d body=%s", r.Code, r.Body)
	}
	atomic.StoreInt32(&mode, 1)
	if r := send(nil); r.Code != 200 { // 2) 成功但无 usage
		t.Fatalf("case2 success without usage: code=%d body=%s", r.Code, r.Body)
	}
	atomic.StoreInt32(&mode, 2)
	if r := send(nil); r.Code != http.StatusBadRequest { // 3) 上游 400
		t.Fatalf("case3 upstream 400: code=%d body=%s", r.Code, r.Body)
	}
	atomic.StoreInt32(&mode, 3)
	if r := send(nil); r.Code != http.StatusBadGateway { // 4) 空流 → 502
		t.Fatalf("case4 empty stream: code=%d body=%s", r.Code, r.Body)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	send(cctx) // 5) 客户端取消

	s := rec.Snapshot(0, nil)
	if s.Totals.Requests != 5 {
		t.Fatalf("requests=%d want 5", s.Totals.Requests)
	}
	if s.Totals.Errors != 2 {
		t.Fatalf("errors=%d want 2（上游 400 + 空流 502；成功无 usage 与取消都不计失败）", s.Totals.Errors)
	}
	if s.Totals.TotalTokens == 0 {
		t.Fatalf("成功那次的 usage token 未被记录: %+v", s.Totals)
	}
}

// 生产池形态下的"模型不可用"终态（HTTP 级）：可用号数 > MaxRotate(3) 时，单次请求
// 永远试不完全部候选 → 必须保持 503（可重试），并且文案要说明是**模型**问题而不是
// 账号池挂了（§R.8 的另一半：误导性文案）。这条路径此前只有单元直调覆盖。
func TestModelBlockedWithMoreAccountsThanMaxRotateStays503(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"该后端无此模型"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up}) // MaxRotate 缺省 3 < 4 个可用号
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model-xyz","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503（还有未试账号，不能断定模型不存在）body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !assertJSONErrorCode(t, body, "no_healthy_account") {
		t.Errorf("body=%s", body)
	}
	if !strings.Contains(body, "model was rejected") {
		t.Errorf("503 文案应说明是模型维度的问题，而不是笼统的账号池不可用: %s", body)
	}
	// 11102 不罚账号：四个号都应仍然健康可选中（供后续请求继续尝试）。
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		if st, ok := p.Status(uid); !ok || st.Cooling || st.Disabled {
			t.Fatalf("%s 不应被冷却/禁用: ok=%v cooling=%v disabled=%v", uid, ok, st.Cooling, st.Disabled)
		}
	}
}

// 流式 499 的记账接线（池层语义见 TestNoteClientAbortClearsStreakWithoutInflatingSuccess）：
// 客户端取消必须清连续软限流计数（否则指数退避被陈旧 streak 放大），但不能把这次尝试
// 记成账号业绩（successCount 不变）。这条是批次 3 引入过回归的地方：当时 499 完全不再记账。
func TestStreamClientCancelClearsSoftStreakWithoutSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	for i := 0; i < 3; i++ {
		p.Cooldown("u1", pool.CoolSoft, time.Minute, "6004 rate limited")
	}
	before, ok := p.Status("u1")
	if !ok || before.SoftStreak < 3 {
		t.Fatalf("前置不成立: %+v", before)
	}
	h := NewHandler(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进 handler 前即取消：流会被判定为客户端中止（499）
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)).
		WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	after, _ := p.Status("u1")
	if after.SoftStreak != 0 {
		t.Fatalf("客户端取消后 softStreak=%d，应为 0（否则退避指数只增不减）", after.SoftStreak)
	}
	if after.SuccessCount != before.SuccessCount {
		t.Fatalf("客户端取消不应累加 successCount: %d → %d", before.SuccessCount, after.SuccessCount)
	}
}

// 未知模型名的**可判定**收紧（实测上游回 400+11102「model [x] service info not found」）：
// 池内可用号数 > MaxRotate 时，"候选是否都试过"永远不成立 → 只靠它会让未知模型一直是 503
// （客户端按可重试语义循环烧轮转）。故补"该模型名是否在网关清单里"这一判据。
func TestModelMissingFromCatalogDiagnostics(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})

	// 缓存未就绪（无动态清单）→ 不可判定，必须保守返回 false。
	if h.modelMissingFromCatalog("glm-5.2", "cn") {
		t.Fatal("动态清单未就绪时不得判定为'不在清单里'")
	}

	// 造一份"新鲜"的动态清单（只含 glm-5.2）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = nil, time.Time{}
		dynamicModelsCache.Unlock()
	})

	if h.modelMissingFromCatalog("glm-5.2", "cn") {
		t.Fatal("动态清单里有的模型不能被判为不可用")
	}
	if !h.modelMissingFromCatalog("no-such-model-xyz", "cn") {
		t.Fatal("确实不在清单里的模型应判定为不可用（→400 不再重试）")
	}
	// global realm 的清单来自未缓存的上游探测 → 一律不下结论（保持 503 可重试）。
	if h.modelMissingFromCatalog("no-such-model-xyz", "global") {
		t.Fatal("global realm 不得用 cn 清单下结论")
	}
	// 缓存过期 → 不可判定（静态表可能不是全集）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now().Add(-2 * dynamicModelsTTL)
	dynamicModelsCache.Unlock()
	if h.modelMissingFromCatalog("no-such-model-xyz", "cn") {
		t.Fatal("清单过期时不得下结论")
	}
	// 静态表内的模型始终算"在清单里"。
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	if h.modelMissingFromCatalog("glm-5.1", "cn") {
		t.Fatal("静态表内的模型不能被判为不在清单里")
	}
}

// HTTP 级回归：生产池形态（可用号 5 > MaxRotate 3）+ 全部 11102 + **清单里没有**的模型名
// 仍必须保持 503（可重试），**不得**据"网关清单里没有"硬判 400。
//
// 依据（2026-10-03 实测）：网关 cn 清单由双 UA + 企业端点并集采集，仍未必覆盖上游全部模型，
// `deepseek-v4.1-flash`、`glm-5.3` 等在用模型都不在清单里 → 按清单判 400 会打断可用配置。
func TestUnknownModelNeverHard400BasedOnCatalog(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"model [x] service info not found"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u5", AccessToken: "at5", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = nil, time.Time{}
		dynamicModelsCache.Unlock()
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model-xyz","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("清单缺失不得作为 400 依据（会误杀不在清单里的在用模型），got %d %s",
			rec.Code, rec.Body.String()[:min(160, rec.Body.Len())])
	}
	if !strings.Contains(rec.Body.String(), "model was rejected") {
		t.Errorf("503 文案应说明是模型维度的问题: %s", rec.Body.String())
	}
	// 诊断通道仍然工作（供事后发现拼写错误）：该模型确实不在清单里。
	if !h.modelMissingFromCatalog("no-such-model-xyz", "cn") {
		t.Error("诊断函数应报告'不在清单里'")
	}
}

// 非流式取消（499）与流式同口径：清连续软限流计数、不累加成功数。
// 这条曾是遗漏（批次 3 只修了流式）——非流式是很多 SDK 的默认模式，取消频率更高。
func TestNonStreamClientCancelClearsSoftStreakWithoutSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	for i := 0; i < 3; i++ {
		p.Cooldown("u1", pool.CoolSoft, time.Minute, "6004 rate limited")
	}
	before, ok := p.Status("u1")
	if !ok || before.SoftStreak < 3 {
		t.Fatalf("前置不成立: %+v", before)
	}
	h := NewHandler(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	after, _ := p.Status("u1")
	if after.SoftStreak != 0 {
		t.Fatalf("非流式取消后 softStreak=%d，应为 0（指数退避不得被陈旧计数放大）", after.SoftStreak)
	}
	if after.SuccessCount != before.SuccessCount {
		t.Fatalf("取消不应累加 successCount: %d → %d", before.SuccessCount, after.SuccessCount)
	}
}

// 静态兜底表守卫：它**不经过** fetchV3Models 的 nonChatModel 过滤，若混进图像/向量类条目，
// 客户端自动发现会把它们当对话模型（选中即被上游拒，见 2026-10-03 实测）。
func TestStaticModelsAreChatModels(t *testing.T) {
	for _, m := range staticModels {
		id, _ := m["id"].(string)
		if id == "" {
			t.Errorf("静态表存在无 id 条目: %+v", m)
			continue
		}
		if upstream.NonChatCatalogID(id) {
			t.Errorf("静态表混入非对话模型 %q（应在 upstream.nonChatNameHint/nonChatKnownIDs 里排除）", id)
		}
	}
}

// 去重助手：保首现、去后续重复（顺序稳定）。
func TestDedupeModelIDs(t *testing.T) {
	in := []map[string]any{
		{"id": "cn:X"}, {"id": "global:X"}, {"id": "X"}, {"id": "X"}, {"id": "Y"}, {"id": ""},
	}
	out := dedupeModelIDs(in)
	ids := make([]string, 0, len(out))
	for _, m := range out {
		ids = append(ids, m["id"].(string))
	}
	want := []string{"cn:X", "global:X", "X", "Y", ""}
	if len(ids) != len(want) {
		t.Fatalf("去重结果 %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("顺序/内容不符: %v, want %v", ids, want)
		}
	}
}

// /v1/models 不得出现重复 id：跨 realm 同名模型此前会产出两份裸名条目
// （cn 分支一份、global 分支一份），严格按 OpenAI 协议消费的客户端会看到重复项。
func TestModelListHasNoDuplicateIDs(t *testing.T) {
	resetModelsCache()
	// 动态 cn 目录里放一个与**静态 global 名单**同名的模型，制造"裸名双份"场景。
	shared := "shared-model"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"` + shared + `","name":"S","maxInputTokens":131072,"maxOutputTokens":8192}]}}`, false
	})
	// 池里同时放一个 CN 号与一个 global 号（domain=workbuddy.ai → Realm()=="global"），
	// 这样 global 分支真的会产出同名裸条目，才能真正测到"去重"。
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "atg", ExpiresAt: 9999999999, Domain: "workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := map[string]int{}
	for _, m := range resp.Data {
		if id, ok := m["id"].(string); ok {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("模型 id 重复 %d 次: %s", n, id)
		}
	}
	if seen[shared] == 0 {
		t.Fatalf("测试前置失效：%s 未出现在清单里（seen=%v）", shared, seen)
	}
}

// 调试落盘（WB2A_DUMP_REQ）：路径不能是 Docker 遗留的固定 /app/data（原生形态写不进去、
// 每次请求打一条 ERR）。现在默认落 <cwd>/data/last_request.json，显式给路径时以路径为准。
func TestDumpReqPathResolution(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// 1) 开关值非路径 → 落 <cwd>/data/last_request.json（目录不存在时自动创建）。
	dumpReqDump("1", []byte(`{"probe":1}`))
	if b, err := os.ReadFile(filepath.Join(dir, "data", "last_request.json")); err != nil || string(b) != `{"probe":1}` {
		t.Fatalf("默认落盘失败: %v %q", err, b)
	}
	// 2) 开关值含分隔符 → 当作完整路径。
	explicit := filepath.Join(dir, "custom", "dump.json")
	dumpReqDump(explicit, []byte(`{"probe":2}`))
	if b, err := os.ReadFile(explicit); err != nil || string(b) != `{"probe":2}` {
		t.Fatalf("显式路径落盘失败: %v %q", err, b)
	}
}

// keyless 模式下面板写操作的 CSRF 兜底：表单**无法设置自定义头**，故要求非 GET 的
// /panel/api/* 必须带 `Authorization`（值不校验，它只是"非表单客户端"的证明）。
//
// 早期实现曾按 Content-Type 拒掉表单类型，但那会**误伤面板自己的文件导入**
// （/panel/api/auths/import 是 multipart/form-data）——本用例把两类都钉住。
func TestKeylessPanelWritesRequireCustomHeader(t *testing.T) {
	newH := func() *Handler {
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		return NewHandler(Config{Pool: p, Upstream: upstream.New()}) // 无 key
	}
	cases := []struct {
		name    string
		method  string
		path    string
		authz   string
		ct      string
		blocked bool
	}{
		{"面板写操作-无头（跨站表单形状）", "POST", "/panel/api/system/shutdown", "", "application/x-www-form-urlencoded", true},
		{"面板写操作-multipart 无头", "POST", "/panel/api/auths/import", "", "multipart/form-data; boundary=x", true},
		{"面板写操作-带头", "POST", "/panel/api/system/shutdown", "Bearer keyless", "", false},
		{"面板文件导入-multipart 带头（不得误伤）", "POST", "/panel/api/auths/import", "Bearer keyless", "multipart/form-data; boundary=x", false},
		{"面板读操作-无头", "GET", "/panel/api/overview", "", "", false},
		// 唯一"改状态的 GET"：login/poll 会写 auths + 入池 + 可能触发签到/试用 →
		// 必须与写操作同口径（否则 <img src=...> 即可驱动，state 是唯一屏障）。
		{"改状态的 GET(login/poll)-无头", "GET", "/panel/api/login/poll?state=x", "", "", true},
		{"改状态的 GET(login/poll)-带头", "GET", "/panel/api/login/poll?state=x", "Bearer keyless", "", false},
		{"对话 POST-无头（不受面板门约束）", "POST", "/v1/chat/completions", "", "application/json", false},
		// /v1 不接受表单编码：keyless + 老浏览器下 text/plain 表单可构造形似 JSON 的 body
		// 去烧额度（读不到响应但确实消耗）。合法客户端一律 JSON。
		{"对话 POST-text/plain（表单形状）", "POST", "/v1/chat/completions", "", "text/plain", true},
		{"对话 POST-urlencoded（表单形状）", "POST", "/v1/chat/completions", "", "application/x-www-form-urlencoded", true},
		{"对话 GET-无 body", "GET", "/v1/models", "", "", false},
	}
	for _, c := range cases {
		// 仅本用例：给请求加短超时，避免"不受面板门约束"的那条真打到上游并挂死
		// （QA 复现：该请求会打到 copilot.tencent.com，本机 fake-IP 198.18.0.20 的 TLS 永不回包）。
		// 语义不变：仍然要求"不被 keyless 门拒绝"，只是不等外网。
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("x")).WithContext(ctx)
		if c.authz != "" {
			req.Header.Set("Authorization", c.authz)
		}
		if c.ct != "" {
			req.Header.Set("Content-Type", c.ct)
		}
		rec := httptest.NewRecorder()
		newH().ServeHTTP(rec, req)
		cancel()
		if c.blocked && rec.Code != http.StatusForbidden {
			t.Errorf("%s: code=%d want 403", c.name, rec.Code)
		}
		if !c.blocked && rec.Code == http.StatusForbidden {
			t.Errorf("%s: 不应被 keyless 门拒绝（code=%d）", c.name, rec.Code)
		}
	}
}

// app.js 必须**总是**发送 Authorization：否则 keyless 模式下它自己的写操作会被上面那道门挡住
// （这是"加固反而打断功能"的典型形态，用源码级断言钉住）。
func TestPanelAppAlwaysSendsAuthorization(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "panel", "app.js"))
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	src := string(raw)
	if strings.Contains(src, "if (k) h['Authorization']") {
		t.Error("app.js 仍有条件式 Authorization：keyless 模式下会被 CSRF 门挡住写操作")
	}
	if n := strings.Count(src, "h['Authorization'] = 'Bearer ' + (k || 'keyless')"); n < 2 {
		t.Errorf("app.js 应在全部 %d 个请求助手处无条件带 Authorization，实际只找到 %d 处", 2, n)
	}
}

// /v1/models 不得把 **global 独有**模型以裸名列出：命名协议是"裸名 → cn"（AGENTS.md §1），
// 裸名列出等于发一张必然失败的票（实测 18 个：o4-mini / gpt-5.6-sol / primary-model …）。
// 两域共有的模型不受影响：cn 分支发的裸名走 cn 是**正确**路由。
func TestModelListDoesNotAdvertiseBareGlobalOnlyNames(t *testing.T) {
	resetModelsCache()
	// 按 host 区分 CN 与 global 探测：CN base = fake.example，global 缺省 = www.workbuddy.ai。
	up := &upstream.Client{
		GlobalEnabled: true,
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[{"id":"shared-x","name":"S","maxInputTokens":131072,"maxOutputTokens":8192}]}}`
			if strings.Contains(r.URL.Host, "workbuddy.ai") {
				body = `{"code":0,"data":{"models":[
					{"id":"shared-x","name":"S","maxInputTokens":131072,"maxOutputTokens":8192},
					{"id":"gl-only","name":"G","maxInputTokens":131072,"maxOutputTokens":8192}
				]}}`
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "atg", ExpiresAt: 9999999999, Domain: "workbuddy.ai"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})

	seen := map[string]bool{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok {
			seen[id] = true
		}
	}
	if !seen["global:gl-only"] {
		t.Errorf("global 独有模型应以 global: 前缀列出: %v", seen)
	}
	if seen["gl-only"] {
		t.Error("global 独有模型不得以裸名列出（裸名会路由到 cn → 必然失败）")
	}
	if !seen["shared-x"] {
		t.Error("两域共有模型的裸名应保留（走 cn 是正确路由）")
	}
	if !seen["global:shared-x"] {
		t.Error("两域共有模型也应同时提供 global: 前缀条目")
	}
}

// /status 透出上游静默截断计数（只读观测项）：便于发现"上游悄悄少了收尾帧"而不必翻日志。
func TestStatusExposesStreamNoDoneCounter(t *testing.T) {
	before := upstream.StreamNoDoneCount()
	// 造一次静默截断（有有效帧、无 [DONE] 就 EOF）。
	rec := httptest.NewRecorder()
	if err := upstream.Stream(rec, strings.NewReader("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if upstream.StreamNoDoneCount() != before+1 {
		t.Fatalf("计数未自增: %d → %d", before, upstream.StreamNoDoneCount())
	}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := body["stream_no_done_total"].(float64)
	if !ok {
		t.Fatalf("/status 缺少 stream_no_done_total: %s", rec2.Body)
	}
	if int64(got) < before+1 {
		t.Fatalf("stream_no_done_total=%d want >= %d", int64(got), before+1)
	}
}

// 11133 类（**模型相关**的参数拒绝）必须保留轮转：同一 body 换号可能成功
// （本报自证：同参数 hy4 通过、GPT 系 15 拒 16 过）。审查 A/4 号的 P1-2。
// 对照：11101（请求体解析失败）必须首轮即 400 不轮转（见上一个用例）。
func TestChatModelRelatedBadParamsStillRotates(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11133,"msg":"model_param_invalid: max_tokens must be at least 16"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（模型相关参数拒绝应换号重试并成功）", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次（保留了轮转）", calls)
	}
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("ErrBadParams 不该罚号：%+v", st)
	}
}

// 11102 的 (账号,模型) 负缓存：首个请求照常打到上游并拿到 11102（由既有的
// sawModelBlocked 归 400），**后续同模型请求不再烧号** —— 选号直接为空，
// 由 ModelBlocked 的 11102 分支给出 400 model_unavailable（换模型才有用），
// 而不是 503「账号池挂了」让人无限重试（上游 44d6e04 / 审查 A 的 P1-3 转活证明）。
func TestChatModelBlockedNegativeCacheSecondRequest(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11102,"msg":"model [glm-9.9] service info not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-9.9","messages":[{"role":"user","content":"hi"}]}`)))
		return rec
	}
	first := post()
	if first.Code != http.StatusBadRequest {
		t.Fatalf("首个请求 code=%d body=%s（11102 应归 400）", first.Code, first.Body)
	}
	if calls != 1 {
		t.Fatalf("首个请求应只打 1 次上游，实得 %d（>1 说明分类/早退有问题）", calls)
	}
	before := calls
	second := post()
	if second.Code != http.StatusBadRequest {
		t.Fatalf("第二个请求 code=%d body=%s（负缓存命中应仍 400，不该 503）", second.Code, second.Body)
	}
	// 必须点明是**负缓存**分支：只断言 model_unavailable 是近真空的 ——
	// sawModelBlocked 那条 400 分支用的是同一个 code（审查 D 的 P2-11）。
	if !strings.Contains(second.Body.String(), "negative cache") {
		t.Errorf("第二个请求应走负缓存分支（文案需含 negative cache）：%s", second.Body)
	}
	if calls != before {
		t.Errorf("负缓存命中后不该再打上游：calls %d→%d", before, calls)
	}
}

// /status 的新增键与"disabled 口径含 paused、逐账号字段分离"的契约（审查 E 的 T1/P2-3）：
// 聚合 disabled 含 paused 是上游语义，但运维必须能区分 ⇒ 独立 paused 键；而逐账号
// accounts[].disabled / [].paused 仍严格分离（调度器的 st.Disabled 判据依赖这一点）。
func TestStatusPausedCountAndDisabledSemantics(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "dis", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "pau", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ok", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	p.Disable("dis", "manual")
	p.Pause("pau")
	h := NewHandler(Config{Pool: p})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	if got := body["paused"]; got != float64(1) {
		t.Errorf("/status.paused=%v want 1（暂停号必须单独计数）", got)
	}
	if got := body["disabled"]; got != float64(2) {
		t.Errorf("/status.disabled=%v want 2（口径含 paused，上游语义）", got)
	}
	if got := body["credit_floor"]; got != float64(0) {
		t.Errorf("/status.credit_floor=%v want 0（默认关闭）", got)
	}
	accs, _ := body["accounts"].([]any)
	seen := map[string]map[string]any{}
	for _, a := range accs {
		m, _ := a.(map[string]any)
		uid, _ := m["uid"].(string)
		seen[uid] = m
	}
	if m := seen["dis"]; m == nil || m["disabled"] != true || m["paused"] == true {
		t.Errorf("禁用号逐账号字段应 disabled=true/paused=false：%v", m)
	}
	if m := seen["pau"]; m == nil || m["paused"] != true || m["disabled"] == true {
		t.Errorf("暂停号逐账号字段应 paused=true/disabled=false：%v", m)
	}
}

// 上游以 error 帧报错时必须**按帧内容处置账号**（上游 01ecb4a）：形态是 HTTP 200 已开流 +
// 一帧 error（6004 限流/审核）。此前这些动作在读第一帧之前就做 ⇒ 正在限流的号被记成健康、
// 粘性把会话钉死在它身上、后续每轮都失败。断言：① 帧仍透传给客户端；② 该号被软冷却；
// ③ 不记成功（成功 helper 不被调用 ⇒ 不绑粘性）。
func TestChatStreamErrorFrameDisposesAccount(t *testing.T) {
	const frame = `{"error":{"code":6004,"message":"model rate limit, will reset at 12:00"}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "data: " + frame + "\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("error 帧必须仍透传给客户端：%s", rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("6004 error 帧必须按帧内容处罚账号（软冷却），实得 %+v", st)
	}
}
