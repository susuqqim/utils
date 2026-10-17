package upstream

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// data: 后不带空格也要能提取负载（部分上游/中间层如此），注释/event 行不算数据。
func TestSSEDataPayloadToleratesMissingSpace(t *testing.T) {
	for _, in := range []string{`data: {"a":1}`, `data:{"a":1}`} {
		p, ok := SSEDataPayload(in)
		if !ok || p != `{"a":1}` {
			t.Fatalf("in=%q got %q,%v", in, p, ok)
		}
	}
	if _, ok := SSEDataPayload(": keep-alive"); ok {
		t.Fatal("comment line must not be data")
	}
	if _, ok := SSEDataPayload("event: foo"); ok {
		t.Fatal("event line must not be data")
	}
}

// HTTP 200 之后出现的 error 帧必须报错，不能被当成"成功但空回答"。
func TestAggregateErrorFrameReturnsError(t *testing.T) {
	stream := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"error":{"message":"6004 rate limit","code":6004}}` + "\n\n"
	if _, err := Aggregate(strings.NewReader(stream)); err == nil {
		t.Fatal("error frame should surface as an error")
	}
}

// n>1：每个 choice 独立聚合，不能揉成一条、也不能只回 choice 0。
func TestAggregateMultipleChoices(t *testing.T) {
	stream := `data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{"content":"B"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"},{"index":1,"delta":{"content":"b"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	chs, _ := resp["choices"].([]any)
	if len(chs) != 2 {
		t.Fatalf("choices=%d want 2", len(chs))
	}
	c0 := chs[0].(map[string]any)["message"].(map[string]any)["content"]
	c1 := chs[1].(map[string]any)["message"].(map[string]any)["content"]
	if c0 != "Aa" || c1 != "Bb" {
		t.Fatalf("choice content c0=%v c1=%v want Aa/Bb", c0, c1)
	}
}

// 既无 [DONE] 也无 finish_reason：截断必须报错，不能默认 stop 伪装完成。
func TestAggregateTruncatedStreamErrors(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"
	if _, err := Aggregate(strings.NewReader(stream)); err == nil {
		t.Fatal("truncated stream should error")
	}
}

// legacy function_call 在非流式聚合里也要保留（旧实现只处理 tool_calls）。
func TestAggregateLegacyFunctionCall(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"function_call":{"name":"do_thing","arguments":"{\"a\":"}},"finish_reason":"function_call"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	fc, ok := msg["function_call"].(map[string]any)
	if !ok || fc["name"] != "do_thing" {
		t.Fatalf("function_call=%v want name do_thing", msg["function_call"])
	}
}

// 流式：data: 缺空格仍走归一化；event:/retry: 等控制行不下发。
func TestStreamToleratesMissingSpaceAndDropsControlLines(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := `data:{"choices":[{"index":0,"delta":{"reasoning":"think"}}]}` + "\n\n" +
		"event: ping\n\n" +
		"data: [DONE]\n\n"
	if err := StreamContext(context.Background(), rec, strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"think"`) {
		t.Fatalf("missing-space frame not normalized: %s", body)
	}
	if strings.Contains(body, "event: ping") {
		t.Fatalf("control line forwarded to client: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no [DONE] written: %s", body)
	}
}

type failReader struct {
	s    string
	err  error
	done bool
}

func (f *failReader) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		return copy(p, f.s), nil
	}
	return 0, f.err
}

// 中途读错误：除 [DONE] 外必须补一个 error 帧，避免下游把截断当正常完成。
func TestStreamWritesErrorFrameOnMidStreamReadError(t *testing.T) {
	rec := httptest.NewRecorder()
	r := &failReader{s: `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n", err: errors.New("boom")}
	if err := StreamContext(context.Background(), rec, r); err == nil {
		t.Fatal("expected stream error to be reported")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("missing error frame on mid-stream failure: %s", body)
	}
}

// 待议2-BugA：n>1 时不同 choice 的同号 tool_call 不得共享 name/id（旧实现串台）。
//
// 口径更新（2026-10-03，Codex 工具调用碎裂问题）：元数据**只在首帧**发出（OpenAI 规范），
// 因此本用例把"串台"检查放在**首帧**上 —— 若缓存按 choice 之外的维度共享，choice1 就会
// 从 choice0 的缓存里凭空拿到 name/id。续帧只允许带 arguments。
func TestBackfillToolCallMetaPerChoiceNoCrossTalk(t *testing.T) {
	cache := map[int]map[int]ToolCallMeta{}
	get := func(frame map[string]any, ci int) map[string]any {
		return frame["choices"].([]any)[ci].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	}
	fnOf := func(tc map[string]any) map[string]any {
		fn, _ := tc["function"].(map[string]any)
		return fn
	}

	// 首帧：choice0 带完整元数据；choice1 **只有 arguments**（无 id/name）
	f1 := map[string]any{"choices": []any{
		map[string]any{"index": 0.0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0.0, "id": "c0", "type": "function",
				"function": map[string]any{"name": "alpha", "arguments": "A"}},
		}}},
		map[string]any{"index": 1.0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0.0, "function": map[string]any{"arguments": "B"}},
		}}},
	}}
	backfillToolCallMeta(f1, cache, false)
	c0, c1 := get(f1, 0), get(f1, 1)
	if c0["id"] != "c0" || fnOf(c0)["name"] != "alpha" {
		t.Fatalf("choice0 自身元数据丢失: %#v", c0)
	}
	if v, has := c1["id"]; has {
		t.Fatalf("choice1 拿到了 choice0 的 id（跨 choice 串台）: %v", v)
	}
	if v, has := fnOf(c1)["name"]; has {
		t.Fatalf("choice1 拿到了 choice0 的 name（跨 choice 串台）: %v", v)
	}

	// 续帧：两个 choice 都只带 arguments，**都不再补元数据**（规范）
	f2 := map[string]any{"choices": []any{
		map[string]any{"index": 0.0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0.0, "function": map[string]any{"arguments": "A2"}},
		}}},
		map[string]any{"index": 1.0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0.0, "function": map[string]any{"arguments": "B2"}},
		}}},
	}}
	backfillToolCallMeta(f2, cache, false)
	c0b, c1b := get(f2, 0), get(f2, 1)
	if _, has := c0b["id"]; has {
		t.Error("续帧不该再带 id")
	}
	if _, has := fnOf(c0b)["name"]; has {
		t.Error("续帧不该再带 name")
	}
	if fnOf(c0b)["arguments"] != "A2" || fnOf(c1b)["arguments"] != "B2" {
		t.Errorf("arguments 必须原样透传: %#v / %#v", fnOf(c0b), fnOf(c1b))
	}
	// 缓存按 choice 独立（这是串台修复的实质）
	if cache[0][0].Name != "alpha" || cache[1][0].Name != "" {
		t.Errorf("缓存串台: cache[0][0]=%+v cache[1][0]=%+v", cache[0][0], cache[1][0])
	}
}

// 待议2-BugB：choice0 有 tool_calls 时，纯文本 choice 的 finish_reason 不得被改写成 tool_calls。
func TestStreamPerChoiceFinishReasonIsolation(t *testing.T) {
	raw := `data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"f0","arguments":"{}"}}]},"finish_reason":""},{"index":1,"delta":{"content":"plain text"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	frames, done := streamFrames(t, raw)
	if done != 1 || len(frames) != 2 {
		t.Fatalf("done=%d frames=%d want 1/2", done, len(frames))
	}
	// 帧 0 是**数据帧**（两个 choice 都带内容）→ 规范要求 finish_reason 全为 null
	// （2026-10-03 起：数据帧上的 finish_reason 一律摘下，只在收尾帧发 —— 正是 Codex
	//  "工具调用被逐字符拆碎"的根因）。
	for i := range 2 {
		chs := frames[0]["choices"].([]any)
		if len(chs) != 2 {
			t.Fatalf("frame0 choices=%d want 2", len(chs))
		}
		if v := chs[i].(map[string]any)["finish_reason"]; v != nil {
			t.Fatalf("frame0 choice%d finish_reason=%v want nil（数据帧不得带）", i, v)
		}
	}
	// 帧 1 是收尾帧（空 delta）→ 纠偏只认本 choice：
	//   choice0 有 tool_calls 且上游报 stop → 纠正为 tool_calls
	//   choice1 纯文本 → 保持 stop（不得被 choice0 污染）
	chs := frames[1]["choices"].([]any)
	if c0 := chs[0].(map[string]any)["finish_reason"]; c0 != "tool_calls" {
		t.Fatalf("收尾帧 choice0 finish_reason=%v want tool_calls", c0)
	}
	if c1 := chs[1].(map[string]any)["finish_reason"]; c1 != "stop" {
		t.Fatalf("收尾帧 choice1 finish_reason=%v want stop（不得被 choice0 污染）", c1)
	}
}

// 待议3：`event: error` 携带错误语义必须透传；id:/retry:/其它 event 仍丢弃，注释保留。
func TestStreamForwardsErrorEventAndDropsOtherControlLines(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := "event: error\n" +
		`data: {"error":{"message":"6004 rate limit","code":6004}}` + "\n\n" +
		"event: ping\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n" +
		"id: 42\n" +
		"retry: 1000\n" +
		": keep-alive\n" +
		"data: [DONE]\n\n"
	if err := StreamContext(context.Background(), rec, strings.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("event: error not forwarded: %s", body)
	}
	if !strings.Contains(body, `"code":6004`) {
		t.Fatalf("error payload lost: %s", body)
	}
	if strings.Contains(body, "event: ping") || strings.Contains(body, "id: 42") || strings.Contains(body, "retry: 1000") {
		t.Fatalf("control line leaked: %s", body)
	}
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("comment not forwarded: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("no [DONE]: %s", body)
	}
}

// isErrorEventLine 边界：大小写、空白容忍，非 error 的 event 一律不算。
func TestIsErrorEventLine(t *testing.T) {
	cases := map[string]bool{
		"event: error":  true,
		"event:error":   true,
		"event:  ERROR": true,
		"event: ping":   false,
		"event:":        false,
		"data: error":   false,
		": error":       false,
	}
	for in, want := range cases {
		if got := isErrorEventLine(in); got != want {
			t.Errorf("isErrorEventLine(%q)=%v want %v", in, got, want)
		}
	}
}
