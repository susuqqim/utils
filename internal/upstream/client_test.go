package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		{400, `{"code":11133,"msg":"too many tokens in prompt"}`, ErrBadParams},
		{400, `too many tokens`, ErrBadParams},
		{400, `maximum context length exceeded`, ErrBadParams},
		// 11102「该后端无此模型」：账号/后端维度的模型不可用（非请求体畸形），
		// 从兜底 ErrClient 显式分出 → 终态可回 400 model_unavailable 而不是误导性 503。
		{400, `{"code":11102,"msg":"该后端无此模型"}`, ErrModelBlocked},
		{400, `{"code": 11102, "msg":"no such model"}`, ErrModelBlocked},
		{400, `no such model: glm-9.9`, ErrModelBlocked},
		// name/id 形态的模型不存在（轮次 3 审计：收窄裸串后不能把这一族一起丢掉）。
		{400, `{"msg":"invalid model name: glm-9.9"}`, ErrModelBlocked},
		{400, `{"msg":"invalid model id"}`, ErrModelBlocked},
		{400, `{"msg":"unsupported model name: x"}`, ErrModelBlocked},
		// 反向保护：参数类错误必须先被 badParams 吃掉，不能被模型族误吸成"换模型"。
		{400, `{"code":11133,"msg":"invalid model parameters"}`, ErrBadParams},
		{400, `invalid model parameters for this request`, ErrClient},
		{400, `{"code":11133,"msg":"x"}`, ErrBadParams},
		// 未知模型也可能回 404（实测形态未知、但代码路径必须覆盖）：必须在 ErrNotFound
		// 之前判成 ErrModelBlocked —— 否则会白冷却一个健康号（60s 软冷却）且终态仍是 503。
		{404, `{"code":11102,"msg":"no such model"}`, ErrModelBlocked},
		{404, `{"msg":"model not found"}`, ErrModelBlocked},
		{404, `no available channel for model glm-9.9`, ErrModelBlocked},
		// 业务码用真 JSON 遍历（空格/引号形态无关）
		{400, `{"code" : 11102 ,"msg":"x"}`, ErrModelBlocked},
		{400, `{"code":"11102"}`, ErrModelBlocked},
		{400, `{"code" : 11101 ,"msg":"Unmarshal chat params failed"}`, ErrBadParams},
		{400, `{"code":"11133"}`, ErrBadParams},
		// 反向保护：普通 404（路径缺失）仍必须是 ErrNotFound，不能被模型族误吸。
		{404, `{"code":404,"msg":"not found"}`, ErrNotFound},
		{404, ``, ErrNotFound},
		{400, `prompt is too long`, ErrPromptTooLong},
		{400, `{"code":11115,"msg":"prompt is too long: 120000 tokens"}`, ErrPromptTooLong},
		{413, `{"code":11115,"msg":"context length exceeded"}`, ErrPromptTooLong},
		// 图片格式/数据错误是确定性请求错误，分类后不轮转、不罚号。
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent"}`, ErrImageInvalid},
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		// 11135 业务码须容忍 JSON 空白。
		{400, `{"code": 11135, "msg":"image data invalid"}`, ErrImageInvalid},
		{400, `{"error":{"code": "11135", "message":"image invalid"}}`, ErrImageInvalid},
		// Issue #175：14018 明确表示账号积分耗尽，即使 HTTP 状态是 429 也必须
		// 走硬积分冷却；仅有相同文案而无该业务码的普通 429 仍保持软限流。
		{429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"error":{"data":{"code":"14018","msg":"Credits exhausted"}}}`, ErrHardCredit},
		{429, `{"requestId":"14018","msg":"Credits exhausted"}`, ErrSoftRate},
		{429, `{"code":1,"msg":"Credits exhausted"}`, ErrSoftRate},
		{200, `quota exceeded`, ErrHardCredit},
		// 账号级授权/配额故障（与 429 一起纳入轮换）：11140 request illegal = auth_forbidden
		// 风控（需重登），14017 = quota_not_activated（试用未激活，需完成 register）。修复前
		// 11140 走 4xx → ErrClient 只换号不罚，坏号留在池内被反复选中刷风控。
		// 注意：11140 不按 code 单独判定——该 code 也承载 rate-limiting 软限流文案
		// （上方 {200, "code":11140 rate-limiting} 必须仍是 ErrSoftRate），只能靠 msg 区分。
		{403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		// 2026-10-20（上游 PR #233）：裸 "request illegal"（无业务码）不再归属任何账号级判定——
		// 11128 通道校验失败与 11140 账号限制文案相同，必须 code + 文案双条件，否则误封健康号。
		{403, `request illegal`, ErrClient},
		{429, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated. Please log out of your current account and log in again to activate it immediately and start your free trial."}}}`, ErrAccountFault},
		{400, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestContentBlockedClientMessage 内容拦截把上游 body 改写成防火墙口径：
// 括号填分类关键词（由 body 抽出，抽不到回「违禁词」），绝不泄露上游 code/账号/upstream 字样。
func TestContentBlockedClientMessage(t *testing.T) {
	cases := []struct {
		body    string
		keyword string
	}{
		{`{"code":11128,"msg":"blocked by security policy"}`, "违禁词"},
		{`{"code":"11128","msg":"blocked by security policy"}`, "违禁词"},
		{`Illegal API invocation from an unapproved channel`, "违禁词"},
		{`{"code":11128,"msg":"content contains NSFW material"}`, "nsfw"},
		{`{"msg":"命中色情内容"}`, "色情"},
		{`violence detected`, "violence"},
		{"", "违禁词"},
	}
	for _, c := range cases {
		got := ContentBlockedClientMessage(c.body)
		want := fmt.Sprintf("触发网站风控违禁词，无法调用模型：内容命中网关内容防火墙规则[%s]，已被拦截。请修改内容后重试。", c.keyword)
		if got != want {
			t.Errorf("ContentBlockedClientMessage(%q)=\n%q\nwant %q", c.body, got, want)
		}
		for _, leak := range []string{"11128", "account", "accounts", "账号", "upstream", "cooling", "no_healthy"} {
			if strings.Contains(strings.ToLower(got), leak) {
				t.Errorf("client message must not leak %q: %s", leak, got)
			}
		}
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseSoftRateReset 解析上游 429 6004 msg 里的「将在 … 重置」时间（## UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非 6004 但带时间（不是模型级）", `{"code":11140,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseSoftRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			if r.Body != nil {
				outbound, _ = io.ReadAll(r.Body)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

// TestResourceSummaryAggregation 断言 ResourceSummary 聚合口径：
// remain 取 Cycle 期剩余、size 取 CycleSize（TotalDosage 作 size 下限）、used 派生。
func TestResourceSummaryAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	remain, used, size, packs, err := c.ResourceSummary(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if remain != 1500 || size != 3000 {
		t.Errorf("summary remain=%d size=%d want 1500/3000 (TotalDosage 作 size 下限)", remain, size)
	}
	// used = TotalDosage(3000) - remain(1500) = 1500。
	if used != 1500 {
		t.Errorf("used=%d want 1500", used)
	}
	if packs != 2 {
		t.Errorf("packs=%d want 2", packs)
	}
}

// TestResourceDetailExpiry 断言 ResourceDetail 能正确解析最早套餐到期时间。
func TestResourceDetailExpiry(t *testing.T) {
	futureDate1 := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	futureDate2 := time.Now().Add(120 * time.Hour).Format("2006-01-02 15:04:05")
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, fmt.Sprintf(`{"code":0,"data":{"Response":{"Data":{"TotalDosage":3000,"Accounts":[
			{"PackageName":"快过期包","CapacitySize":1000,"CapacityRemain":500,"CapacityUsed":500,"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":500,"CycleEndTime":%q},
			{"PackageName":"远期包","CapacitySize":2000,"CapacityRemain":1000,"CapacityUsed":1000,"CycleCapacitySize":2000,"CycleCapacityRemain":1000,"CycleCapacityUsed":1000,"ExpireTime":%q}
		]}}}}`, futureDate1, futureDate2)), nil
	})
	detail, err := c.ResourceDetail(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("ResourceDetail error: %v", err)
	}
	if detail.Remain != 1500 {
		t.Errorf("detail.Remain=%d want 1500", detail.Remain)
	}
	expectedExpiry := parseBillingTime(futureDate1)
	if detail.EarliestExpire.Sub(expectedExpiry).Abs() > time.Second {
		t.Errorf("detail.EarliestExpire=%v want %v", detail.EarliestExpire, expectedExpiry)
	}
}

// TestResourceSummaryGlobalRealm 断言 global 账号走 global billing base + /billing/meter/*
// （无 /v2 前缀），且 404 时 fallback /v2——realm 感知双路径，供 cmd/credit 复用。
func TestResourceSummaryGlobalRealm(t *testing.T) {
	var billingCalls []string
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		billingCalls = append(billingCalls, r.URL.Path)
		if r.URL.Path == "/billing/meter/get-user-resource" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"g","CycleCapacitySize":500,"CycleCapacityRemain":200,"CycleCapacityUsed":300}
		]}}}}`))
	}))
	defer billSrv.Close()

	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	c := &Client{
		HTTP:              &http.Client{},
		BillingBaseCN:     "https://billing.cn",
		BillingBaseGlobal: strings.TrimSuffix(billSrv.URL, "/"),
		GlobalEnabled:     true,
	}
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	remain, used, size, packs, err := c.ResourceSummary(a)
	if err != nil {
		t.Fatalf("global summary: %v", err)
	}
	if remain != 200 || used != 300 || size != 500 || packs != 1 {
		t.Errorf("global summary=%d/%d/%d/%d want 200/300/500/1", remain, used, size, packs)
	}
	if len(billingCalls) != 2 ||
		billingCalls[0] != "/billing/meter/get-user-resource" ||
		billingCalls[1] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("global billing fallback calls=%v", billingCalls)
	}
}

// TestResourceSummaryCNUnchanged 零回归：CN 账号仍是 /v2/billing/meter/get-user-resource 单路径。
func TestResourceSummaryCNUnchanged(t *testing.T) {
	var calls []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	_, _, _, _, err := c.ResourceSummary(&auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatalf("cn summary: %v", err)
	}
	if len(calls) != 1 || calls[0] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("cn billing calls=%v want single /v2 path", calls)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, total, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if total != 3000 {
		t.Errorf("total=%d want 3000", total)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, total, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

// TestIsAlreadyCheckin "今天已签到"判定为幂等成功（中文/英文 markers 均命中）。
func TestIsAlreadyCheckin(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"今天已签到"}`), nil
	})
	if !IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("今天已签到 应判为 already")
	}

	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"Already checked in today"}`), nil
	})
	if !IsAlreadyCheckin(c2.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("already(英文) 应判为 already")
	}
}

// TestIsAlreadyCheckinRejectsNonUpstream 网络层/解析层错误不得当作幂等成功。
// 误判会让当天实际未签到的账号被标成正常（停机补签遇到抖动时尤其危险）。
func TestIsAlreadyCheckinRejectsNonUpstream(t *testing.T) {
	if IsAlreadyCheckin(errors.New("dial tcp: connection refused")) {
		t.Error("网络错误不得判为 already")
	}
	if IsAlreadyCheckin(errors.New("parse failed: unexpected EOF")) {
		t.Error("解析错误不得判为 already")
	}
	// 非"已签到"语义的上游业务错误（如余额不足）也不得判为 already。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10002,"msg":"积分不足，请充值"}`), nil
	})
	if IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("余额不足不得判为 already")
	}
	// nil 不判为 already。
	if IsAlreadyCheckin(nil) {
		t.Error("nil 不得判为 already")
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

// 并发刷新守卫回归：上游常常**不轮换 refreshToken**（响应里 refreshToken 为空/未变），
// 于是并发场景下先完成的那个 goroutine 只会改 AccessToken。此时较晚完成的一方必须丢弃
// 自己的结果——旧实现用 `&&`（两值都变才丢弃）判定"没人动过"，会把旧一代 accessToken
// 覆盖回去，该号随即 401 → 软冷却。
func TestRefreshDiscardsStaleResultWhenOnlyAccessTokenChanged(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 模拟"另一个 goroutine 在本次网络 I/O 期间完成刷新并写回"：只改 AccessToken。
		a.Lock()
		a.AccessToken = "winner"
		a.Unlock()
		return jsonResp(200, `{"code":0,"data":{"accessToken":"stale","expiresIn":3600}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "winner" {
		t.Fatalf("accessToken = %q, want %q（较晚完成的刷新不得覆盖已写回的新一代凭证）", a.AccessToken, "winner")
	}
}

// 反向保护：本次是唯一写入者（无并发改动）时，刷新结果必须正常写回。
func TestRefreshWritesWhenNoConcurrentWriter(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"fresh","expiresIn":3600}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "fresh" {
		t.Fatalf("accessToken = %q, want fresh（无并发写入时不得误判为并发冲突）", a.AccessToken)
	}
}

// 错误信息截断必须按 rune 边界：上游错误体含中文时，按字节切会产生非法 UTF-8
// （日志乱码 + 透传给客户端的 Msg 被 json 替换成 U+FFFD）。
func TestTruncateIsRuneSafe(t *testing.T) {
	s := "上下文超限：这是一段中文错误信息，需要被安全截断"
	for _, n := range []int{1, 2, 3, 5, 7, 11, 20, 33} {
		got := truncate(s, n)
		if len(got) > n {
			t.Fatalf("truncate(%d) = %q (%d bytes) 超长", n, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) = %q 不是合法 UTF-8", n, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("truncate(%d) = %q 不是原串前缀", n, got)
		}
	}
	if got := truncate("  abc  ", 10); got != "abc" {
		t.Fatalf("TrimSpace 行为变化: %q", got)
	}
}

// CN 侧 /v3/config 必须**双 UA 取并集**：实测该端点按 UA 下发不同模型集合
// （IDE UA 无 deepseek 系列、CLI UA 无 o4-mini/enhance/auto-chat），单路会少列在用模型，
// 而客户端配置正是按 /v1/models 生成的。
func TestFetchModelsUnionsBothUserAgents(t *testing.T) {
	var sawIDE, sawCLI bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			switch r.Header.Get("User-Agent") {
			case codeBuddyIDEUA:
				sawIDE = true
				// IDE UA：字段更全（带 supportedEfforts），但**没有** deepseek 系列。
				return jsonResp(200, `{"code":0,"data":{"models":[
					{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}},
					{"id":"o4-mini","name":"O4","maxInputTokens":131072,"maxOutputTokens":8192}
				]}}`), nil
			case codeBuddyCLIUA:
				sawCLI = true
				// CLI UA：多出 deepseek 系列与预览模型。
				return jsonResp(200, `{"code":0,"data":{"models":[
					{"id":"deepseek-v4.1-flash","name":"DS","maxInputTokens":131072,"maxOutputTokens":8192},
					{"id":"kimi-k2.8-preview","name":"Kimi","maxInputTokens":131072,"maxOutputTokens":8192}
				]}}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{}}`), nil
		}
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if !sawIDE || !sawCLI {
		t.Fatalf("两路 UA 都必须被探测: ide=%v cli=%v", sawIDE, sawCLI)
	}
	got := map[string]ModelInfo{}
	for _, mi := range infos {
		got[mi.ID] = mi
	}
	for _, want := range []string{"glm-5.2", "o4-mini", "deepseek-v4.1-flash", "kimi-k2.8-preview"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("并集缺少 %s: %v", want, infos)
		}
	}
	// IDE 条目优先：glm-5.2 的 supportedEfforts 来自 IDE 路（字段更全）。
	if len(got["glm-5.2"].Efforts) != 2 {
		t.Fatalf("IDE 条目应优先（保留 supportedEfforts）: %+v", got["glm-5.2"])
	}
}

// 单路失败必须降级（另一路结果照常返回），两路全失败才报错。
func TestFetchModelsDegradesWhenOneUAFails(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			if r.Header.Get("User-Agent") == codeBuddyCLIUA {
				return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192}]}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{}}`), nil
		}
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("单路失败应降级而非报错: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("应保留成功那一路的结果: %+v", infos)
	}
}

// 非对话模型过滤：实测 hunyuan-image-alpha-edit（图像编辑）没有 text-to-image 标签、
// maxOutputTokens 也缺失（0），旧判据全放过 → 混进 /v1/models，而真机实测该 id 调
// /v1/chat/completions 会被上游 400 拒绝（"列出来但用不了"）。新增名字级兜底（仅在
// token 数未知时生效），并钉住几个**不能误伤**的正常模型。
func TestNonChatModelFiltersImageLikeIDs(t *testing.T) {
	cases := []struct {
		id     string
		maxOut int64
		tags   []string
		want   bool // true = 非对话模型（应被过滤）
	}{
		{"hunyuan-image-alpha-edit", 0, nil, true},    // 实测被上游 400
		{"hunyuan-image-alpha-edit", 4096, nil, true}, // 显式 id 名单：即便给了 token 数也排除
		{"some-image-gen-v2", 0, nil, true},
		{"text-embedding-3-large", 0, nil, true},
		{"bge-rerank-v2", 0, nil, true},
		{"doubao-video-pro", 0, nil, true},
		{"gpt-tts-1", 0, nil, true},
		// 不能误伤：带 v/vision/多模态的**对话**模型，以及有 token 元数据时名字含 image 的
		{"glm-4.6v", 0, nil, false},
		{"glm-5v-turbo", 0, nil, false},
		{"glm-4.6v", 65536, nil, false},
		{"glm-image-chat", 131072, nil, false}, // 有 token 元数据 → 不启用名字兜底
		{"deepseek-v4.1-flash", 393216, nil, false},
		{"kimi-k2.5", 0, nil, false},
		{"default", 0, nil, false},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.maxOut, c.tags); got != c.want {
			t.Errorf("nonChatModel(%q, %d) = %v, want %v", c.id, c.maxOut, got, c.want)
		}
	}
}

// 同 ID 冲突时的字段级取优：primary(IDE) 的档位为空/更少时，用 secondary(CLI) 补，
// 否则会白丢 supportedEfforts（storeEfforts 桶与请求体降级都依赖它）。
func TestMergeModelInfosFillsMissingCapabilityFields(t *testing.T) {
	primary := []ModelInfo{{ID: "glm-5.2", Name: "GLM", ContextWindow: 131072}}
	secondary := []ModelInfo{{
		ID: "glm-5.2", Name: "GLM-CLI", ContextWindow: 262144, MaxTokens: 8192,
		Efforts: []string{"low", "high", "max"}, DefaultEffort: "high",
	}}
	out := mergeModelInfos(primary, secondary)
	if len(out) != 1 {
		t.Fatalf("同 ID 应合并为一条: %+v", out)
	}
	got := out[0]
	if len(got.Efforts) != 3 {
		t.Errorf("secondary 的 supportedEfforts 未被补上: %+v", got.Efforts)
	}
	if got.DefaultEffort != "high" {
		t.Errorf("secondary 的 defaultEffort 未被补上: %q", got.DefaultEffort)
	}
	if got.MaxTokens != 8192 {
		t.Errorf("secondary 的 MaxTokens 未被补上: %d", got.MaxTokens)
	}
	// primary 已有的字段不得被覆盖（IDE 单条字段更全、优先）。
	if got.ContextWindow != 131072 || got.Name != "GLM" {
		t.Errorf("primary 字段被覆盖: %+v", got)
	}
	// primary 档位非空时保持 primary（不被 secondary 覆盖）。
	out2 := mergeModelInfos(
		[]ModelInfo{{ID: "x", Efforts: []string{"low"}}},
		[]ModelInfo{{ID: "x", Efforts: []string{"low", "high", "max"}}},
	)
	if len(out2[0].Efforts) != 1 || out2[0].Efforts[0] != "low" {
		t.Errorf("primary 档位非空时应保持 primary: %+v", out2[0].Efforts)
	}
}

// 单路持续失败时目录**不得缩水**：与上一份"两路都成功"的目录取并集。
// （重试只能救瞬时抖动；上游持续拒某个 UA 时，若直接把部分并集当成功缓存，
// /v1/models 会在 10 分钟 TTL 内少列该 UA 独有模型。）
func TestFetchModelsKeepsCatalogWhenOneUAPersistentlyFails(t *testing.T) {
	cliFails := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			if r.Header.Get("User-Agent") == codeBuddyCLIUA {
				if cliFails {
					return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
				}
				return jsonResp(200, `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash","name":"DS","maxInputTokens":131072,"maxOutputTokens":8192}]}}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192}]}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{}}`), nil
		}
	})

	first, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("两路都成功时应拿到并集: %+v", first)
	}

	cliFails = true // CLI 路持续失败（非瞬时抖动）
	second, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("degraded fetch 不应报错: %v", err)
	}
	got := map[string]bool{}
	for _, mi := range second {
		got[mi.ID] = true
	}
	if !got["glm-5.2"] || !got["deepseek-v4.1-flash"] {
		t.Fatalf("单路失败时目录不得缩水（应与上一份取并集）: %+v", second)
	}
}

// 4xx（429/401/403）是确定性拒绝：**不得重试**（重试只会放大限流/风控）。
func TestFetchModelsDoesNotRetryOn4xx(t *testing.T) {
	var cliCalls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			if r.Header.Get("User-Agent") == codeBuddyCLIUA {
				cliCalls.Add(1)
				return jsonResp(429, `{"code":6004,"msg":"rate limited"}`), nil
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192}]}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{}}`), nil
		}
	})
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("单路 429 应降级而非报错: %v", err)
	}
	if n := cliCalls.Load(); n != 1 {
		t.Fatalf("4xx 不得重试：CLI 路被调用 %d 次（want 1）", n)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("应保留成功那一路: %+v", infos)
	}
}
