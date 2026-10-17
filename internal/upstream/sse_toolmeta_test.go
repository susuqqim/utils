package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenAI 流式规范：tool_calls 的 `id`/`type`/`function.name` **只在首帧**出现，
// 后续增量帧只带 `index` + `function.arguments`（客户端据此按 index 追加拼接）。
//
// 上游实测形态就是"首帧带元数据、后续只带 arguments"，而网关为了"防止客户端状态机解析崩溃"
// 给**每一个**续帧都补回了 id/type/name —— 这会让按"带 id 即新调用"解析的客户端（如 Codex）
// 把一个工具调用拆成几百个碎片（实测一次 write_file 被拆成 327 帧，逐帧 1~11 字符）。
func TestStreamToolCallMetaOnlyOnFirstDelta(t *testing.T) {
	in := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\""}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\"a.py\","}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"x\"}"}}]}}]}`,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(l, "data: ") || strings.TrimSpace(l[6:]) == "[DONE]" {
			continue
		}
		var o map[string]any
		if err := json.Unmarshal([]byte(l[6:]), &o); err != nil {
			t.Fatalf("帧不是合法 JSON: %q", l)
		}
		frames = append(frames, o)
	}
	// 逐帧检查：第 2 个及以后的 tool_calls 帧不得再带 id/type/function.name
	count := 0
	for _, o := range frames {
		ch, _ := o["choices"].([]any)
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]any)["delta"].(map[string]any)
		if d == nil {
			continue
		}
		tcs, ok := d["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		count++
		cur, _ := tcs[0].(map[string]any)
		fn, _ := cur["function"].(map[string]any)
		if count == 1 {
			if cur["id"] != "call_1" {
				t.Errorf("首帧应带 id，实得 %v", cur["id"])
			}
			if fn["name"] != "write_file" {
				t.Errorf("首帧应带 name，实得 %v", fn["name"])
			}
			continue
		}
		if _, has := cur["id"]; has {
			t.Errorf("第 %d 个 tool_calls 帧不该再带 id（规范只在首帧给）: %v", count, cur["id"])
		}
		if _, has := cur["type"]; has {
			t.Errorf("第 %d 个 tool_calls 帧不该再带 type", count)
		}
		if _, has := fn["name"]; has {
			t.Errorf("第 %d 个 tool_calls 帧不该再带 function.name", count)
		}
		if _, has := fn["arguments"]; !has {
			t.Errorf("第 %d 个 tool_calls 帧应带 arguments", count)
		}
	}
	if count != 3 {
		t.Fatalf("tool_calls 帧数=%d want 3", count)
	}
}

// 上游怪癖兼容：它把 finish_reason 重复贴在**每个**增量帧上（实测 331 个工具帧全带
// finish_reason="tool_calls"），而规范只在末帧给非 null。客户端（Codex）见到带
// finish_reason 的帧就认为该次调用结束 → 参数逐字符碎裂、无法发送代码。
// 网关必须归一化：数据帧上的值摘掉，只在收尾帧发出（恰好一次）。
func TestStreamFinishReasonOnlyOnTerminalFrame(t *testing.T) {
	// 上游形态：每个工具帧都带 finish_reason="tool_calls"；末帧是 usage + role(空 delta)
	raw := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frames := framesFromBody(t, rec.Body.String())
	finishCount := 0
	for i, fr := range frames {
		chs, _ := fr["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c := chs[0].(map[string]any)
		got, _ := c["finish_reason"].(string)
		if got == "" {
			continue
		}
		finishCount++
		d, _ := c["delta"].(map[string]any)
		// 非 null 的 finish_reason 只能出现在**空数据**的收尾帧上
		if len(d) > 0 {
			if _, onlyRole := d["role"]; !onlyRole && len(d) == 1 {
				t.Errorf("帧 %d：带 finish_reason 的帧仍有实质 delta：%#v", i, d)
			}
			if deltaHasSubstance(d) {
				t.Errorf("帧 %d：数据帧不得带 finish_reason（实测 Codex 逐字符碎裂的根因）", i)
			}
		}
	}
	if finishCount != 1 {
		t.Errorf("非 null finish_reason 出现 %d 次（规范应恰好 1 次）", finishCount)
	}
}

// 若上游把 finish_reason 只贴在**带内容**的帧上（没有规范收尾帧），网关必须补一个空 delta 末帧。
func TestStreamSynthesizesTerminalFrameWhenMissing(t *testing.T) {
	raw := "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frames := framesFromBody(t, rec.Body.String())
	last := frames[len(frames)-1]
	c := last["choices"].([]any)[0].(map[string]any)
	if c["finish_reason"] != "stop" {
		t.Fatalf("末帧应带 finish_reason=stop，实得 %v", c["finish_reason"])
	}
	if d, _ := c["delta"].(map[string]any); len(d) != 0 {
		t.Errorf("合成的末帧 delta 应为空，实得 %#v", d)
	}
	// 原数据帧里的 finish_reason 应已被摘掉（避免"内容帧即结束"的误判）
	for i, fr := range frames[:len(frames)-1] {
		chs, _ := fr["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		if v, ok := chs[0].(map[string]any)["finish_reason"].(string); ok && v != "" {
			t.Errorf("帧 %d 不该带 finish_reason=%q", i, v)
		}
	}
}

// 续帧只允许 index + function.arguments —— id/type/name 一律删除（非 repeat 模式）。
// 上游实测每帧都带 type（部分还带 id/name），而规范只在首帧给；多余元数据正是
// Codex"工具调用被逐字符拆碎"的第二类诱因。
func TestStreamContinuationFramesOnlyCarryArguments(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\"}}]}}]}\n\n" +
		// 续帧：上游**故意**重复带 id/type/name（真实上游就这样）
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"\\\"a\\\":1}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	frames := framesFromBody(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	get := func(i int) (map[string]any, map[string]any) {
		tc := frames[i]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		return tc, fn
	}
	// 首帧：元数据齐
	tc0, fn0 := get(0)
	if tc0["id"] != "c1" || tc0["type"] != "function" || fn0["name"] != "write_file" {
		t.Errorf("首帧应带完整元数据，实得 %#v / %#v", tc0, fn0)
	}
	// 续帧：只允许 index + arguments
	tc1, fn1 := get(1)
	for _, k := range []string{"id", "type"} {
		if v, has := tc1[k]; has {
			t.Errorf("续帧不得带 %s（实得 %v）", k, v)
		}
	}
	if v, has := fn1["name"]; has {
		t.Errorf("续帧不得带 function.name（实得 %v）", v)
	}
	if fn1["arguments"] != `"a":1}` {
		t.Errorf("续帧 arguments 必须原样透传，实得 %v", fn1["arguments"])
	}
	if _, has := tc1["index"]; !has {
		t.Error("续帧必须保留 index")
	}
}
