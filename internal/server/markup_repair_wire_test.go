// markup_repair_wire_test.go 端到端守「handler.go 把 DSML 标记修复挂上流式与非流式两条
// 路径」这条接线（见 upstream/dsml.go）：
//
//   - 客户端**声明了 tools** + 上游把工具调用吐成正文标记 → 出站必须有结构化
//     tool_calls（流式 delta.tool_calls / 非流式 message.tool_calls），正文不残留标记，
//     finish_reason 为 tool_calls；
//   - 客户端**没声明 tools** → 修复器压根不挂（upstream.WithMarkupRepair 不传），
//     标记原样透出、不产出任何 tool_calls。
//
// 夹具用转义拼全角分隔符（\uFF5C），不写死字面量：分隔符是全角字符，字面量容易被
// 编辑器/复制粘贴静默改坏（同 internal/upstream/dsml_test.go 的理由）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// dsmlWireBlock 拼一个完整的 DSML calls 块（单次 exec_command 调用）。
func dsmlWireBlock() string {
	const pipe = "\uFF5C\uFF5C"
	open := "<" + pipe + "DSML" + pipe + " calls>"
	closeTag := "</" + pipe + "DSML" + pipe + " calls>"
	invoke := "<" + pipe + "DSML" + pipe
	return open + "\n" +
		invoke + ` invoke name="exec_command">` + "\n" +
		invoke + ` parameter name="cmd" string="true">ls -la` + "</" + pipe + "DSML" + pipe + " parameter>\n" +
		"</" + pipe + "DSML" + pipe + " invoke>\n" +
		closeTag
}

// dsmlWireSSE 拼一条上游 SSE：role 首帧 + 一个正文帧（prose + 标记块）+ 收尾帧 + [DONE]。
func dsmlWireSSE(content string) string {
	chunk := func(delta map[string]any, finish string) string {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		b, err := json.Marshal(map[string]any{
			"id": "chatcmpl-wire", "object": "chat.completion.chunk", "created": 1753600000,
			"model": "glm-5.2", "choices": []any{choice},
		})
		if err != nil {
			panic(err)
		}
		return "data: " + string(b) + "\n\n"
	}
	return chunk(map[string]any{"role": "assistant"}, "") +
		chunk(map[string]any{"content": content}, "") +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// dsmlWireHandler 挂一个把给定上游 SSE 原样回放（并强制按流式处理）的 handler。
func dsmlWireHandler(t *testing.T, sse string) http.Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// dsmlWireBody 拼请求体；withTools 决定客户端是否声明 tools（修复器的启用开关）。
func dsmlWireBody(withTools, stream bool) string {
	req := map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if withTools {
		req["tools"] = []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "exec_command"},
		}}
	}
	if stream {
		req["stream"] = true
	}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestMarkupRepairWireNonStream 非流式：声明了 tools 时聚合响应里的标记被还原成
// message.tool_calls；没声明 tools 时聚合响应一个字节都不动。
func TestMarkupRepairWireNonStream(t *testing.T) {
	const prose = "跑一下目录列表。\n\n"
	sse := dsmlWireSSE(prose + dsmlWireBlock())

	t.Run("声明了 tools → 还原", func(t *testing.T) {
		rec := httptest.NewRecorder()
		dsmlWireHandler(t, sse).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(dsmlWireBody(true, false))))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("resp not json: %v body=%s", err, rec.Body)
		}
		choice := resp["choices"].([]any)[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		if got := msg["content"]; got != prose {
			t.Errorf("content=%q want %q（标记必须被摘掉）", got, prose)
		}
		if strings.Contains(rec.Body.String(), "DSML") {
			t.Errorf("响应里仍残留标记: %s", rec.Body)
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || len(calls) != 1 {
			t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
		}
		tc := calls[0].(map[string]any)
		if tc["type"] != "function" {
			t.Errorf("type=%v want function", tc["type"])
		}
		if id, _ := tc["id"].(string); id == "" {
			t.Errorf("tool_call 缺 id: %#v", tc)
		}
		fn, _ := tc["function"].(map[string]any)
		if fn["name"] != "exec_command" {
			t.Errorf("fn.name=%v want exec_command", fn["name"])
		}
		if args, _ := fn["arguments"].(string); !strings.Contains(args, "ls -la") {
			t.Errorf("fn.arguments=%q 丢了参数", args)
		}
		if choice["finish_reason"] != "tool_calls" {
			t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
		}
	})

	t.Run("没声明 tools → 零改写", func(t *testing.T) {
		rec := httptest.NewRecorder()
		dsmlWireHandler(t, sse).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(dsmlWireBody(false, false))))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("resp not json: %v body=%s", err, rec.Body)
		}
		choice := resp["choices"].([]any)[0].(map[string]any)
		msg := choice["message"].(map[string]any)
		if _, has := msg["tool_calls"]; has {
			t.Errorf("没声明 tools 时不得产出 tool_calls: %#v", msg["tool_calls"])
		}
		if got, _ := msg["content"].(string); !strings.Contains(got, "DSML") {
			t.Errorf("标记应原样透出，实际 content=%q", got)
		}
		if choice["finish_reason"] != "stop" {
			t.Errorf("finish_reason=%v want stop", choice["finish_reason"])
		}
	})
}

// TestMarkupRepairWireStream 流式：声明了 tools 时正文标记在透传前被还原成
// delta.tool_calls 且收尾为 tool_calls；没声明 tools 时标记原样透出。
func TestMarkupRepairWireStream(t *testing.T) {
	const prose = "跑一下目录列表。\n\n"
	sse := dsmlWireSSE(prose + dsmlWireBlock())

	t.Run("声明了 tools → 还原", func(t *testing.T) {
		rec := httptest.NewRecorder()
		dsmlWireHandler(t, sse).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(dsmlWireBody(true, true))))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if strings.Contains(body, "DSML") {
			t.Fatalf("流式响应里仍残留标记: %s", body)
		}
		var content strings.Builder
		callFrames, callAt, finishAt, finishVal := 0, -1, -1, ""
		frameIdx := 0
		for _, ln := range strings.Split(body, "\n") {
			ln = strings.TrimSpace(ln)
			if !strings.HasPrefix(ln, "data: ") || strings.HasPrefix(ln, "data: [DONE]") {
				continue
			}
			var fr map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &fr); err != nil {
				t.Fatalf("bad frame %q: %v", ln, err)
			}
			chs, _ := fr["choices"].([]any)
			if len(chs) == 0 {
				continue
			}
			c := chs[0].(map[string]any)
			if d, ok := c["delta"].(map[string]any); ok {
				if s, ok := d["content"].(string); ok {
					content.WriteString(s)
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					callFrames++
					callAt = frameIdx
					tc := tcs[0].(map[string]any)
					fn, _ := tc["function"].(map[string]any)
					if fn["name"] != "exec_command" {
						t.Errorf("第 %d 个调用帧函数名=%v", callFrames, fn["name"])
					}
				}
			}
			if v, ok := c["finish_reason"].(string); ok && v != "" {
				finishAt, finishVal = frameIdx, v
			}
			frameIdx++
		}
		if content.String() != prose {
			t.Errorf("正文=%q want %q（标记必须被摘掉）", content.String(), prose)
		}
		if callFrames != 1 {
			t.Fatalf("应产出 1 个 delta.tool_calls 帧，实际 %d", callFrames)
		}
		if finishVal != "tool_calls" {
			t.Errorf("finish_reason=%q want tool_calls", finishVal)
		}
		if finishAt <= callAt {
			t.Errorf("收尾帧(%d)必须排在最后一个调用帧(%d)之后", finishAt, callAt)
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Error("缺 [DONE]")
		}
	})

	t.Run("没声明 tools → 零改写", func(t *testing.T) {
		rec := httptest.NewRecorder()
		dsmlWireHandler(t, sse).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(dsmlWireBody(false, true))))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "DSML") {
			t.Errorf("没声明 tools 时标记必须原样透出: %s", body)
		}
		if strings.Contains(body, `"tool_calls"`) {
			t.Errorf("没声明 tools 时不得产出 tool_calls: %s", body)
		}
	})
}
