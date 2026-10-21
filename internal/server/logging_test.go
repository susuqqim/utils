package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"context"
	"sync"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureStdout 重定向 os.Stdout（连同 chatLogOut，见 SetChatLogOutput 的注入点）
// 并捕获 fn 期间的全部输出。
//
// ⚠️ 它替换的是**包级**变量：与 t.Parallel() 的用例存在隐性耦合（stability_test.go 用了
// t.Parallel()）。当前不冲突（并行用例既不写 stdout 也不发 chat 行），但**新增并行用例时
// 不要在里面发 chat 行或断言 stdout**，否则会与本函数互相干扰。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	oldOut := chatLogOut
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	chatLogOut = w
	fn()
	os.Stdout = old
	chatLogOut = oldOut
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog 临时开启聊天表格日志（TestMain 默认关闭），测试结束后恢复。
// 仅供断言表格行输出的用例使用。
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	// TTFB 必须被**记录**（>=0 且非负），但不能断言严格 >0：
	// 本函数读取的是内存里的 strings.Reader，首帧解析耗时在纳秒级，
	// 而 Windows 上 Go 单调时钟分辨率粗到"两次连续 time.Now() 差值为 0"
	// （实测 immediate time.Since: zeros=300/300）。此处断言 >0 会在 Windows
	// 上恒失败，而失败与网关行为无关。
	// 真正要防的回归是"TTFB 变成负值"（计时起点被写反）——那才是缺陷。
	if r.TTFB() < 0 {
		t.Errorf("ttfb=%v want >=0（计时起点不得晚于首帧）", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

// TestChatStatsReaderCreditMissing (P0, RED): usage 存在但 credit 字段缺失时
// Credit() 必须返回 ok=false——缺失≠免费，不能把缺观测当 0 扣费记入账本
// （否则收费的 global 号可能被误判 tier0 免费被永久优先）。
func TestChatStatsReaderCreditMissing(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); !ok || toks != 5 {
		t.Fatalf("tokens=%d ok=%v want 5/true (usage still供 token)", toks, ok)
	}
	credit, ok := r.Credit()
	if ok {
		t.Errorf("Credit()=(%v,true) want ok=false: usage 无 credit 字段 ≠ 0 成本", credit)
	}
}

// TestChatStatsReaderCreditExplicitZero (P0, RED/GREEN): usage 显式 credit:0 是合法免费观测，
// Credit() 必须 ok=true 且 credit==0——真 0 不许丢（显式 0 与字段缺失语义不同）。
func TestChatStatsReaderCreditExplicitZero(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":500,\"credit\":0}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	credit, ok := r.Credit()
	if !ok || credit != 0 {
		t.Errorf("Credit()=(%v,%v) want (0,true): 显式 credit:0 是合法免费观测", credit, ok)
	}
}

// TestChatStatsReaderJSONNullCredit 回归保护：usage.credit 显式 null 也算缺失
// （null ≠ 0），不得被当作免费观测。
func TestChatStatsReaderJSONNullCredit(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"credit\":null}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if _, ok := r.Credit(); ok {
		t.Error("usage.credit=null 应视为缺失（ok=false）")
	}
}

// TestChatStatsReaderNoUsage 末帧完全无 usage → Credit() ok=false（现状已对，回归保护）。
func TestChatStatsReaderCreditNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if _, ok := r.Credit(); ok {
		t.Error("无 usage 帧 Credit() 应 ok=false")
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestUIDPrefix(t *testing.T) {
	if got := uidPrefix("00e26541abcdef012345"); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := uidPrefix("abc"); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := uidPrefix(""); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", "00e26541abcdef", http.StatusOK, 1234, -1, 0.08, true)
	})
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "uid=00e26541", "TTFB=412ms", "tok=1234", "tok/s(生成期) |", "credit=0.08", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "s1", http.StatusServiceUnavailable, -1, -1, 0, false)
	})
	for _, want := range []string{"TTFB=-", "tok=-", "-tok/s", "credit=-", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", 200, 1, -1, 0, false)
		logChatRow(0, time.Second, "m", "sync", "u", 200, 1, -1, 0, false)
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "uid=u1", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"uid=u1", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}

// 聊天表格行的模型名截断必须按 rune 边界：模型名来自客户端，可能是中文/emoji。
func TestLogChatRowModelTruncationIsRuneSafe(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(time.Millisecond, 2*time.Millisecond, "中文模型名称很长的模型", "stream", "uid-1234", 200, 10, -1, 0, false)
	})
	if !utf8.ValidString(out) {
		t.Fatalf("日志行不是合法 UTF-8（截断切开了多字节字符）: %q", out)
	}
	// 11 字节上限 + rune 边界 → "中文模"（3 个 3 字节字符 = 9 字节，第 4 个放不下）。
	if !strings.Contains(out, "中文模") {
		t.Fatalf("日志行缺少按 rune 边界截断的模型名: %q", out)
	}
}

// 早退路径上的统计读取竞态回归（用 -race 判定，无功能断言）：
// upstream.StreamContext 在客户端取消/下游写错误时**直接 return**，不等内部 SSE 读取
// 协程退出；handler 紧接着读 Tokens/Credit/TotalTokens/Usage，与仍在解析 usage 帧的
// 读取协程并发。只把 ttfb 原子化不够——撕裂读会得到 (hasCredit=true, credit=0)，
// 把 0 单价写进成本 EMA，进而影响自适应分块的档位判定。
// 本用例必须在 `-race` 下运行才有意义（CI/本地验收请跑 go test -race）。
func TestChatStatsReaderNoRaceOnEarlyReturn(t *testing.T) {
	pr, pw := io.Pipe()
	stats := newChatStatsReaderSince(pr, time.Now())
	ctx, cancel := context.WithCancel(context.Background())

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		_ = upstream.StreamContext(ctx, httptest.NewRecorder(), stats)
	}()

	// 持续投喂"带 usage 的帧"：让读取协程总有解析工作可做（与 handler 的读重叠）。
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		frame := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"credit\":0.01}}\n\n"
		for i := 0; i < 300; i++ {
			if _, err := io.WriteString(pw, frame); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		_ = pw.Close()
	}()

	time.Sleep(10 * time.Millisecond)
	cancel() // 早退路径：StreamContext 立即返回，读取协程可能仍在跑

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = stats.Tokens()
				_, _ = stats.Credit()
				_ = stats.TotalTokens()
				_ = stats.Usage()
				_ = stats.TTFB()
			}
		}()
	}
	wg.Wait()
	_ = pw.Close()
	<-streamDone
	<-writerDone
}

// 速率口径：生成期（总时长 - 首帧等待）。端到端口径会把提示词处理/排队算进分母，
// 短回答被低估数倍 —— 实测同一请求 58.6 vs 173.9 tok/s（3 倍差），
// 面板数字与 DSH 等客户端按"生成期"显示的速率因此对不上（用户实测反馈）。
func TestLogChatRowRateUsesGenerationWindow(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		// TTFB=1s、总时长=2s → 生成期 1s；completion=100 含推理 60
		logChatRow(time.Second, 2*time.Second, "m", "stream", "u", 200, 100, 60, 0.1, true)
	})
	if !strings.Contains(out, "tok=100(推60)") {
		t.Errorf("应拆出推理 token（tok=100(推60)），实得:\n%s", out)
	}
	if !strings.Contains(out, "100.0tok/s") {
		t.Errorf("速率应按生成期算（100 tok / 1s = 100.0），实得:\n%s", out)
	}
	if strings.Contains(out, "50.0tok/s") {
		t.Errorf("仍是端到端口径（100/2s=50.0）—— 口径未生效:\n%s", out)
	}
	if !strings.Contains(out, "tok/s(生成期)") {
		t.Errorf("速率缺少口径标注:\n%s", out)
	}
}

func TestTokensPerSecondSemantics(t *testing.T) {
	// 正常：减去首帧等待
	if got := tokensPerSecond(100, 2*time.Second, time.Second); got != 100 {
		t.Errorf("生成期速率=%v want 100", got)
	}
	// 未测到首帧（非流式）：回退端到端
	if got := tokensPerSecond(100, 2*time.Second, 0); got != 50 {
		t.Errorf("无 TTFB 时应回退端到端=%v want 50", got)
	}
	// 异常：TTFB≥总时长 → 回退端到端而不是除负数
	if got := tokensPerSecond(100, time.Second, 3*time.Second); got != 100 {
		t.Errorf("TTFB 超总时长时应回退=%v want 100", got)
	}
	// usage 缺失
	if got := tokensPerSecond(-1, time.Second, 0); got != 0 {
		t.Errorf("usage 缺失应得 0，实得 %v", got)
	}
	// 上游 e05edb7/issue #127：假流式/攒批下发——首帧与末帧几乎同时到，
	// total−ttfb 只剩毫秒级，不得除出上万 tok/s 的幻数，应退回端到端。
	if got := tokensPerSecond(500, 5*time.Second, 4950*time.Millisecond); got != 100 {
		t.Errorf("攒批下发（仅剩 50ms）应退回端到端=100，实得 %v", got)
	}
	// 扣除后不足 200ms 下限 → 同样退回端到端
	if got := tokensPerSecond(100, time.Second, 950*time.Millisecond); got != 100 {
		t.Errorf("不足 200ms 下限应退回端到端=100，实得 %v", got)
	}
	// 恰好达到 200ms 下限 → 照常扣除（1s−800ms=200ms → 100/0.2=500）
	if got := tokensPerSecond(100, time.Second, 800*time.Millisecond); got != 500 {
		t.Errorf("恰好 200ms 应照常扣除=500，实得 %v", got)
	}
	// 零时长不除零
	if got := tokensPerSecond(10, 0, 0); got != 0 {
		t.Errorf("零时长应得 0，实得 %v", got)
	}
}

// P2-8：HTTP 200 的流里出现上游 error 帧（如 6004 限流/审核拦截）时，stats reader 必须
// 标记出来 —— 否则这条流会被记成"这一轮成功"，成功路径的 BlockModelClear 会把该
// (账号,模型) 刚写下的 11102 负缓存误清（保护被自己人撤除）。
func TestChatStatsReaderFlagsUpstreamErrorFrame(t *testing.T) {
	mk := func(body string) *chatStatsReader {
		return newChatStatsReaderSince(strings.NewReader(body), time.Now())
	}
	// 纯 error 帧：既没有 usage 也要标记
	r1 := mk("data: {\"error\":{\"code\":11102,\"msg\":\"model not found\"}}\n\n")
	io.ReadAll(r1)
	if !r1.UpstreamErrorFrame() {
		t.Error("error 帧未被标记（会误清负缓存）")
	}
	// 正常帧：不得标记
	r2 := mk("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	io.ReadAll(r2)
	if r2.UpstreamErrorFrame() {
		t.Error("正常流被误标为 error 帧")
	}
	// error:null（部分上游会带空值占位）：不算错误帧
	r3 := mk("data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	io.ReadAll(r3)
	if r3.UpstreamErrorFrame() {
		t.Error("error:null 不该被当作错误帧")
	}
	// 先正常后报错（边生成边限流）：也必须标记，且 usage 仍可解析
	r4 := mk("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"error\":{\"code\":6004,\"msg\":\"rate limit\"},\"usage\":{\"completion_tokens\":7}}\n\n")
	io.ReadAll(r4)
	if !r4.UpstreamErrorFrame() {
		t.Error("中途错误帧未被标记")
	}
}
