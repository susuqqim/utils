package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"glm-5.2","messages":[]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
}

func TestPrepareBodyToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"function","function":{"name":"get_weather"}},"tools":[{"type":"function"}]}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools should be kept for function choice")
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	for _, in := range []string{
		`{"tool_choice":"none","tools":[{}],"functions":[{}]}`,
		`{"tool_choice":{"type":"none"},"tools":[{}]}`,
	} {
		out := PrepareBodyOpt([]byte(in), true)
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, ok := m["tool_choice"]; ok {
			t.Errorf("%s: tool_choice should be deleted", in)
		}
		if _, ok := m["tools"]; ok {
			t.Errorf("%s: tools should be deleted", in)
		}
		if _, ok := m["functions"]; ok {
			t.Errorf("%s: functions should be deleted", in)
		}
	}
}

func TestPrepareBodyToolChoiceAuto(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"tool_choice":{"type":"auto"}}`), true)
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
}

func TestPrepareBodyInvalidJSON(t *testing.T) {
	in := []byte(`{broken`)
	out := PrepareBodyOpt(in, true)
	if string(out) != string(in) {
		t.Error("invalid json should pass through unchanged")
	}
}

const sseFixture = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func TestAggregate(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v", resp["model"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role=%v", msg["role"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", usage)
	}
}

func TestAggregateSkipsNonDataLines(t *testing.T) {
	raw := ": comment\n\n" + sseFixture
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestAggregateToolCalls(t *testing.T) {
	// 流式 tool_calls：首片带 id/type/name + 空 arguments，后续只带 arguments 片段
	raw := `data: {"id":"x1","model":"deepseek-v4-pro","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""},"index":0}]}}],"usage":null}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"北京\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":11}}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if calls[0]["id"] != "call_a" || calls[0]["type"] != "function" {
		t.Errorf("call meta=%v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn.name=%v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("fn.arguments=%q", fn["arguments"])
	}
}

// TestBackfillToolCallNames 直测跨帧 name 缓存：首 chunk 带 name 时建缓存，
// 后续 chunk 缺省/置空 name 时从缓存回填（hawklithm/workbuddy2api issue#2）。
func TestBackfillToolCallNames(t *testing.T) {
	var mkFrame = func(name, args string) map[string]any {
		fn := map[string]any{}
		if name != "" {
			fn["name"] = name
		}
		if args != "" {
			fn["arguments"] = args
		}
		return map[string]any{"choices": []any{
			map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0.0, "function": fn},
			}}},
		}}
	}
	getFn := func(frame map[string]any) map[string]any {
		tc := frame["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		return fn
	}
	names := map[int]map[int]ToolCallMeta{}

	// 首帧带 name：进缓存，且**只在首帧**对外发出（OpenAI 规范）
	f0 := mkFrame("lookup", "")
	backfillToolCallMeta(f0, names, false)
	if names[0][0].Name != "lookup" {
		t.Fatalf("cache after first chunk=%v want lookup", names)
	}

	// 续帧 name 为空：**不再回填**（旧行为会把 name 重复到每个续帧 → 严格客户端把一次
	// 工具调用拆成几百个碎片；见 backfillToolCallMeta 的注释与 Codex 实测）
	f1 := mkFrame("", `{"term":"x"}`)
	backfillToolCallMeta(f1, names, false)
	if v, has := getFn(f1)["name"]; has {
		t.Errorf("续帧不该再补 name（规范只在首帧给），实得 %v", v)
	}

	// 兼容开关 repeat=true 时恢复旧行为（为"状态机需要重复元数据"的客户端保留）
	f1b := mkFrame("", `{"term":"x"}`)
	backfillToolCallMeta(f1b, names, true)
	if getFn(f1b)["name"] != "lookup" {
		t.Errorf("repeat=true 时应回填 name，实得 %v", getFn(f1b)["name"])
	}

	// 不同 index 互不串扰：index 1 无缓存 → 不得凭空造出 name
	f3 := map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 1.0, "function": map[string]any{}},
		}}},
	}}
	backfillToolCallMeta(f3, names, false)
	if _, ok := names[0][1]; ok {
		t.Error("index 1 should not get a cached name")
	}
	f3call := f3["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if _, hasFn := f3call["function"].(map[string]any)["name"]; hasFn {
		t.Error("index 1 with no cache should leave function without name (no phantom name)")
	}

	// 非 tool_calls 帧（content only）零影响
	f5 := map[string]any{"choices": []any{
		map[string]any{"delta": map[string]any{"content": "hi"}},
	}}
	backfillToolCallMeta(f5, names, false)
	if got := f5["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any); len(got) != 1 || got["content"] != "hi" {
		t.Errorf("content-only frame altered: %#v", got)
	}
}

// TestStreamToolCallNameOnlyOnFirstFrame 端到端：上游首 chunk 带 id/name，后续 chunk 的
// name 为空串且无 id —— 透传后**只有首帧**带元数据，续帧只带 arguments（OpenAI 规范）。
//
// 历史：旧实现（为 hawklithm/workbuddy2api issue#2）给每个续帧都补回 name/id，导致
// 按"带 id 即新调用"解析的客户端（实测 Codex）把一次工具调用拆成几百个碎片、无法发送代码。
// 需要旧行为的可用 config `features.tool_call_meta_repeat=true` 恢复（见下一个用例）。
func TestStreamToolCallNameOnlyOnFirstFrame(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"{\\\"term\\\":\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"\",\"arguments\":\"\\\"北京\\\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	var args strings.Builder
	toolFrames := 0
	for i, fr := range frames {
		chs, ok := fr["choices"].([]any)
		if !ok {
			t.Fatalf("frame %d choices missing", i)
		}
		d, _ := chs[0].(map[string]any)["delta"].(map[string]any)
		tcs, ok := d["tool_calls"].([]any)
		if !ok {
			continue // 末帧只有 finish_reason，无 tool_calls 属正常
		}
		toolFrames++
		tc := tcs[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		if a, _ := fn["arguments"].(string); a != "" {
			args.WriteString(a)
		}
		if toolFrames == 1 {
			if fn["name"] != "lookup" || tc["id"] != "call_a" {
				t.Errorf("首帧应带 name/id，实得 name=%v id=%v", fn["name"], tc["id"])
			}
			continue
		}
		// 续帧：不得再出现元数据（这是 Codex 碎裂问题的根因）
		if _, has := tc["id"]; has {
			t.Errorf("frame %d 续帧不该带 id（实得 %v）", i, tc["id"])
		}
		if _, has := tc["type"]; has {
			t.Errorf("frame %d 续帧不该带 type", i)
		}
		if v, has := fn["name"]; has {
			t.Errorf("frame %d 续帧不该带 name（实得 %v）", i, v)
		}
	}
	if toolFrames != 3 {
		t.Fatalf("tool_calls 帧数=%d want 3", toolFrames)
	}
	// arguments 必须跨 chunk 完整保流（本次改动只动元数据，不动 arguments）
	if got := args.String(); got != `{"term":"北京"}` {
		t.Errorf("arguments 跨 chunk 拼接=%q want %q", got, `{"term":"北京"}`)
	}
}

// 兼容开关 repeat=true：恢复"每个续帧都带元数据"的旧行为（config features.tool_call_meta_repeat）。
func TestStreamToolCallMetaRepeatCompat(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamContextOpt(context.Background(), rec, strings.NewReader(raw), true); err != nil {
		t.Fatal(err)
	}
	frames := framesFromBody(t, rec.Body.String())
	last := frames[len(frames)-1]
	tc := last["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_a" || fn["name"] != "lookup" {
		t.Errorf("repeat=true 时续帧应带 id/name，实得 id=%v name=%v", tc["id"], fn["name"])
	}
}

// streamFrames 把原始 SSE 输入经 Stream 处理后解析出所有 JSON 帧及 [DONE] 计数。
// framesFromBody 从已写好的 SSE 响应体解析数据帧（供需要指定 Stream 变体的用例复用）。
func framesFromBody(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.HasPrefix(ln, "data: [DONE]") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ln, err)
		}
		frames = append(frames, obj)
	}
	return frames
}

func streamFrames(t *testing.T, raw string) (frames []map[string]any, doneCount int) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "data: [DONE]") {
			doneCount++
			continue
		}
		if strings.HasPrefix(ln, "data: ") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
				t.Fatalf("bad frame %q: %v", ln, err)
			}
			frames = append(frames, obj)
		}
	}
	return frames, doneCount
}

func TestNormalizeFrame(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string // 规范化后 marshal 的期望 JSON（Go map 键按字典序输出）
	}{
		{"empty content/refusal and finish_reason empty string",
			map[string]any{"id": "x", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"content": "", "refusal": ""}, "finish_reason": ""},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"x","object":"chat.completion.chunk","usage":null}`},
		{"non-empty tool_calls kept",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"id": "c1", "type": "function"}}}},
			}},
			`{"choices":[{"delta":{"tool_calls":[{"id":"c1","type":"function"}]},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"empty tool_calls list dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{}, "content": "hi"}},
			}},
			`{"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"empty placeholder function_call dropped",
			map[string]any{"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"function_call": map[string]any{"name": "", "arguments": ""}}},
			}},
			`{"choices":[{"delta":{},"finish_reason":null,"index":0}],"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}`},
		{"top-level unknown fields dropped, usage null when absent",
			map[string]any{"id": "x", "object": "chat.completion.chunk", "created": 1, "junk": "noise", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
			}},
			`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1,"id":"x","object":"chat.completion.chunk","usage":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(normalizeFrame(c.in, nil))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != c.want {
				t.Errorf("got  %s\nwant %s", raw, c.want)
			}
		})
	}
}

func TestStreamNormalizesFrames(t *testing.T) {
	// 混合噪声帧：空 content/reasoning/refusal/function_call + 空 tool_calls + 顶层非标字段，
	// 随后非空 content + tool_calls 帧，最后 finish/usage 帧。
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"\",\"refusal\":\"\",\"tool_calls\":[],\"function_call\":{\"name\":\"\",\"arguments\":\"\"}},\"finish_reason\":\"\"}],\"extra_field\":\"junk\"}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\",\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"},\"index\":0}]},\"finish_reason\":\"\"}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done frames=%d want 1", done)
	}
	if len(frames) != 3 {
		t.Fatalf("frames=%d want 3", len(frames))
	}

	// 帧 1：噪声全剔除，finish_reason ""→null，usage 缺失→null，顶层非标字段剥除
	f0 := frames[0]
	if _, ok := f0["extra_field"]; ok {
		t.Error("top-level extra_field should be dropped")
	}
	if f0["usage"] != nil {
		t.Errorf("usage should be null when absent, got %v", f0["usage"])
	}
	ch0 := f0["choices"].([]any)[0].(map[string]any)
	if ch0["finish_reason"] != nil {
		t.Errorf("frame1 finish_reason=%v want null", ch0["finish_reason"])
	}
	d := ch0["delta"].(map[string]any)
	// role 是合法白名单键保留；空 content/reasoning/refusal/tool_calls/function_call 噪声全剔除
	if len(d) != 1 || d["role"] != "assistant" {
		t.Errorf("frame1 delta should only keep role, got %#v", d)
	}
	for _, noise := range []string{"content", "reasoning_content", "refusal", "tool_calls", "function_call"} {
		if _, ok := d[noise]; ok {
			t.Errorf("frame1 delta should drop %q, got %#v", noise, d)
		}
	}

	// 帧 2：非空 content 与 tool_calls 保留，finish_reason ""→null
	f1 := frames[1]
	ch1 := f1["choices"].([]any)[0].(map[string]any)
	d1 := ch1["delta"].(map[string]any)
	if d1["content"] != "hello" {
		t.Errorf("frame2 content=%v", d1["content"])
	}
	tcs, ok := d1["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("frame2 tool_calls=%#v", d1["tool_calls"])
	}
	// 数据帧上的 finish_reason 一律摘下（规范：只有末帧非 null）—— 上游把
	// finish_reason 重复贴在每个增量帧上，是 Codex"工具调用逐字符拆碎"的根因。
	if ch1["finish_reason"] != nil {
		t.Errorf("frame2（数据帧）finish_reason=%v want nil", ch1["finish_reason"])
	}

	// 帧 3：finish_reason 非空保留，usage 保留
	f2 := frames[2]
	ch2 := f2["choices"].([]any)[0].(map[string]any)
	if ch2["finish_reason"] != "tool_calls" {
		t.Errorf("frame3 finish_reason=%v want tool_calls (input stop but stream has tool_calls)", ch2["finish_reason"])
	}
	if f2["usage"].(map[string]any)["total_tokens"].(float64) != 7 {
		t.Errorf("frame3 usage=%v", f2["usage"])
	}
}

// TestStreamFirstIdPassthrough 帧混合（首帧有 id / 中间帧无 id / 空串 id）：输出每帧 id
// 必须连续一致（取首帧真实值），不再一律 chatcmpl-wb2api（issue #35 后台聚合：透传流里
// 每帧同 id 才能按消息归并）。
func TestStreamFirstIdPassthrough(t *testing.T) {
	raw := "data: {\"id\":\"chatcmpl-upstream-9\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		// 中间帧无 id：应复用首帧 id。
		"data: {\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n" +
		// 中间帧 id 为空串：同样复用首帧 id。
		"data: {\"id\":\"\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	frames, done := streamFrames(t, raw)
	if done != 1 {
		t.Fatalf("done=%d want 1", done)
	}
	// 第 3 帧带内容 + finish_reason=stop → 其 finish_reason 被摘下，网关按规范**补一个
	// 空 delta 的收尾帧**（所以是 4 帧而不是 3 帧）。
	if len(frames) != 4 {
		t.Fatalf("frames=%d want 4（含网关补的规范末帧）", len(frames))
	}
	for i, fr := range frames {
		if got := fr["id"]; got != "chatcmpl-upstream-9" {
			t.Errorf("frame %d id=%v want chatcmpl-upstream-9 (首帧真实 id 续传)", i, got)
		}
	}
	tail := frames[3]["choices"].([]any)[0].(map[string]any)
	if tail["finish_reason"] != "stop" {
		t.Errorf("补的末帧 finish_reason=%v want stop", tail["finish_reason"])
	}
	if d, _ := tail["delta"].(map[string]any); len(d) != 0 {
		t.Errorf("补的末帧 delta 应为空，实得 %#v", d)
	}
	if frames[2]["choices"].([]any)[0].(map[string]any)["finish_reason"] != nil {
		t.Error("数据帧不该再带 finish_reason")
	}
}

// TestStreamNoIdFallsBackToSentinel 全流无任何真实 id → 兜底 chatcmpl-wb2api
// （整流无 id 时的既有哨兵，帧与帧之间仍一惯性存在）。
func TestStreamNoIdFallsBackToSentinel(t *testing.T) {
	raw := "data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{}," +
		"\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	frames, _ := streamFrames(t, raw)
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	for i, fr := range frames {
		if got := fr["id"]; got != "chatcmpl-wb2api" {
			t.Errorf("frame %d id=%v want sentinel chatcmpl-wb2api (无真实 id)", i, got)
		}
	}
}

func TestStreamDoneFallback(t *testing.T) {
	// 上游流在无 [DONE] 时 EOF，Stream 必须兜底写一个 [DONE]
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), "data: [DONE]") {
		t.Errorf("missing [DONE] fallback: %q", body)
	}

	// 已有 [DONE] 时只写一次，不重复
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(rec2.Body.String(), "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, rec2.Body.String())
	}
}

func TestStreamPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing chunks: %q", body)
	}
	// 逐行仍是合法 SSE（每行以 data: 开头或是空行）
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("bad line: %q", ln)
		}
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

// TestAggregateEmptyStreamCases 覆盖空流检测：0 有效事件必须报错、[DONE] 即 break、
// [DONE] 后垃圾不进聚合、正常聚合回归。
func TestAggregateEmptyStreamCases(t *testing.T) {
	// 正常回归基流：content + finish_reason + usage，[DONE] 收尾。
	valid := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	cases := []struct {
		name    string
		raw     string
		wantErr bool
		// 回归断言（仅在 wantErr=false 时校验）
		wantContent string
		wantUsage   float64
	}{
		{
			name:    "空流（EOF 即止）",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "只有注释行和空行加 DONE",
			raw:     ": comment\n\n: another comment\n\ndata: [DONE]\n\n",
			wantErr: true,
		},
		{
			name:    "DONE 后跟垃圾帧不进聚合",
			raw:     valid[:len(valid)-len("data: [DONE]\n\n")] + "data: [DONE]\n\ndata: {\"junk\":\"should not aggregate\"}\n\n",
			wantErr: false,
			// 与 valid 基流一致的聚合期望
			wantContent: "hi",
			wantUsage:   7,
		},
		{
			name:        "正常流回归",
			raw:         valid,
			wantErr:     false,
			wantContent: "hi",
			wantUsage:   7,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := Aggregate(strings.NewReader(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (resp=%v)", resp)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
			if msg["content"] != c.wantContent {
				t.Errorf("content=%q want %q", msg["content"], c.wantContent)
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				if u["total_tokens"].(float64) != c.wantUsage {
					t.Errorf("usage=%v want %v", u["total_tokens"], c.wantUsage)
				}
			} else {
				t.Errorf("usage missing")
			}
		})
	}
}

// TestAggregateEmptyStreamError 校验空流错误信息形如约定文案。
func TestAggregateEmptyStreamError(t *testing.T) {
	_, err := Aggregate(strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no valid data events") {
		t.Fatalf("err=%v", err)
	}
}

// TestStreamEmptyFramesCase 覆盖流式空流检测：0 有效帧时写 error 帧（error 字段存活）,
// 恰好一个 [DONE]，并返回非 nil error。
func TestStreamEmptyFramesCase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空流", ""},
		{"只有注释行", ": comment\n\n"},
		{"只有 DONE", "data: [DONE]\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := Stream(rec, strings.NewReader(c.raw))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			body := rec.Body.String()
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Errorf("[DONE] count=%d want 1: %q", n, body)
			}
			// error 帧必须原样保留 error 字段（未被 normalizeFrame 白名单剥掉）
			var e map[string]any
			found := false
			for _, ln := range strings.Split(body, "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, "data: ") {
					payload := strings.TrimPrefix(ln, "data: ")
					if payload == "[DONE]" {
						continue
					}
					if json.Unmarshal([]byte(payload), &e) == nil {
						if em, ok := e["error"].(map[string]any); ok && em["message"] == "empty upstream stream" && em["type"] == "upstream_error" {
							found = true
						}
					}
				}
			}
			if !found {
				t.Errorf("error frame absent or error field stripped: %q", body)
			}
		})
	}
}

// TestStreamGarbageAfterDone 校验 DONE 之后的垃圾帧不出现在响应里。
func TestStreamGarbageAfterDone(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"should\":\"not appear\"}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "should") {
		t.Errorf("garbage after DONE leaked into response: %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	// 有效帧仍被透传
	if !strings.Contains(body, "hello") {
		t.Errorf("valid frame missing: %q", body)
	}
}

// TestStreamNormalPassthroughRegression 校验正常透传回归：帧被 normalize 后透传、
// 末尾恰好一个 [DONE]、无 error 帧；上游漏发 DONE 时自动补。
func TestStreamNormalPassthroughRegression(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"带 DONE 的正常流", sseFixture},
		{"漏发 DONE 自动补", "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(c.raw)); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()
			if strings.Contains(body, `"error"`) {
				t.Errorf("unexpected error frame: %q", body)
			}
			if n := strings.Count(body, "data: [DONE]"); n != 1 {
				t.Errorf("[DONE] count=%d want 1: %q", n, body)
			}
			// 帧被规范化：含 "id" 且有标准 object 字段
			if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
				t.Errorf("frame not normalized: %q", body)
			}
		})
	}
}

// TestStreamContextCancellation 验证当客户端主动取消（点击停止生成）时，
// StreamContext 立即退出并返回 context.Canceled，不卡顿、不泄漏 goroutine。
func TestStreamContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()

	errCh := make(chan error, 1)
	rec := httptest.NewRecorder()

	go func() {
		errCh <- StreamContext(ctx, rec, pr)
	}()

	// 写入首帧
	pw.Write([]byte("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	time.Sleep(20 * time.Millisecond)

	// 客户端点击“停止生成”
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StreamContext failed to abort promptly on client cancellation")
	}
	pw.Close()
}

// TestStreamMidStreamErrorFramePassthrough error-passthrough：SSE 流中带上游 error 帧
// （如 6004 限流/审核拦截）时，该帧**原样透传**（error 字段不被 normalizeFrame 白名单
// 剥掉），message/code/requestId 原文可见；且干净帧仍规范化透传、恰好一个 [DONE]。
func TestStreamMidStreamErrorFramePassthrough(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-42"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	// 错误帧原文存续：message/code/requestId 都在。
	if !strings.Contains(body, "您的使用量已超出频率限制") ||
		!strings.Contains(body, `"code":"6004"`) ||
		!strings.Contains(body, `"requestId":"req-rl-42"`) {
		t.Fatalf("error frame must pass through verbatim (message/code/requestId): %q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] count=%d want 1: %q", n, body)
	}
	// 干净帧仍被规范化透传（error 帧不计入规范化路径，不影响普通帧）。
	if !strings.Contains(body, `"role":"assistant"`) || !strings.Contains(body, `"content":"hello"`) {
		t.Errorf("clean frame missing or not normalized: %q", body)
	}
}

// TestNormalizeUsageCacheAliasesMirrorsNestedHit / PreservesZeroResult 验证
// 缓存命中别名字段镜像同步逻辑。
func TestNormalizeUsageCacheAliasesMirrorsNestedHit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            21041.0,
		"completion_tokens":        8.0,
		"total_tokens":             21049.0,
		"cache_read_input_tokens":  0.0,
		"cached_tokens":            0.0,
		"prompt_cache_hit_tokens":  0.0,
		"prompt_cache_miss_tokens": 177.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 20864.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	for _, key := range []string{
		"cache_read_input_tokens",
		"cached_tokens",
		"prompt_cache_hit_tokens",
	} {
		if got[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, got[key])
		}
	}
	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 20864.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 20864", details["cached_tokens"])
	}
}

func TestNormalizeUsageCacheAliasesPreservesZeroResult(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           35.0,
		"completion_tokens":       2.0,
		"total_tokens":            37.0,
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 0.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 0", details["cached_tokens"])
	}
}

// 静默截断（有有效帧、无读错误、但上游没发 [DONE]）必须留痕：非流式聚合对同一形态判
// truncation 报错，流式这里按"正常结束"兜底补 [DONE]（语义不变），但至少要能观测到。
func TestStreamWithoutDoneIsCounted(t *testing.T) {
	before := noDoneCount.Load()
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")); err != nil {
		t.Fatal(err)
	}
	if noDoneCount.Load() != before+1 {
		t.Fatalf("未记录静默截断: before=%d after=%d", before, noDoneCount.Load())
	}
	// 上游显式发 [DONE] 时不得计数。
	rec2 := httptest.NewRecorder()
	if err := Stream(rec2, strings.NewReader("data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if got := noDoneCount.Load(); got != before+1 {
		t.Fatalf("显式 [DONE] 不应计入静默截断: %d", got)
	}
}

// SSE 规范口径的显式记录：`data:` 之后**至多剥掉一个空格**（spec: "If the value starts
// with a U+0020 SPACE, remove it"）。`data:  x` 因此得到 " x" —— 下游 JSON 解析容忍前导空白，
// 而 `data: [DONE]`（标准写法）必须被识别为结束标记。
// 这条由 fuzz 目标 FuzzSSEDataPayload 逼出来：我最初的断言要求"payload 无前导空白"，
// 属于比规范更严的误报 —— 结论是**改断言，不是改实现**。
func TestSSEDataPayloadSpecSemantics(t *testing.T) {
	cases := []struct {
		line    string
		payload string
		ok      bool
	}{
		{"data: [DONE]", "[DONE]", true},
		{"data:[DONE]", "[DONE]", true}, // 无空格（部分中间层）
		{"data:{\"a\":1}", "{\"a\":1}", true},
		{"data: {\"a\":1}", "{\"a\":1}", true},
		{"data:  x", " x", true}, // 两个空格 → 只剥一个（规范）
		{"data:", "", true},
		{"data: ", "", true},
		{" data: x", "", false}, // 前导空格 → 不是 data 行
		{"event: error", "", false},
		{": keep-alive", "", false},
		{"data: [DONE]\r\n", "[DONE]", true},
	}
	for _, c := range cases {
		got, ok := SSEDataPayload(c.line)
		if ok != c.ok || got != c.payload {
			t.Errorf("SSEDataPayload(%q) = (%q, %v), want (%q, %v)", c.line, got, ok, c.payload, c.ok)
		}
	}
}

// 第 ① 步回归（上游 01ecb4a）：error 帧观察者必须拿到**原文**，且正常流不触发；
// 无选项时不 panic、行为与以前完全一致。
func TestStreamErrorFrameObserver(t *testing.T) {
	const errPayload = `{"error":{"code":6004,"msg":"rate limit","requestId":"r1"}}`
	var got string
	rec := httptest.NewRecorder()
	raw := "data: " + errPayload + "\n\n"
	if err := StreamContextOpt(context.Background(), rec, strings.NewReader(raw), false,
		WithErrorFrameObserver(func(p string) { got = p })); err != nil {
		t.Fatalf("StreamContextOpt: %v", err)
	}
	if got != errPayload {
		t.Fatalf("观察者未拿到 error 帧原文：%q", got)
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("error 帧仍必须透传给客户端：%q", rec.Body.String())
	}
	// 正常数据帧不得触发观察者
	got = ""
	rec2 := httptest.NewRecorder()
	if err := StreamContextOpt(context.Background(), rec2,
		strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), false,
		WithErrorFrameObserver(func(p string) { got = p })); err != nil {
		t.Fatalf("StreamContextOpt: %v", err)
	}
	if got != "" {
		t.Errorf("正常帧不该触发观察者：%q", got)
	}
	// 无选项：沿用原签名/行为
	rec3 := httptest.NewRecorder()
	if err := StreamContextOpt(context.Background(), rec3,
		strings.NewReader("data: "+errPayload+"\n\n"), false); err != nil {
		t.Fatalf("无选项时不应报错: %v", err)
	}
}
