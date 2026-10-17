package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这些 Fuzz 目标覆盖**处理不可信上游数据**的入口：SSE 解析/非流式聚合/请求体改写。
// 上游（或中间代理）返回畸形数据时必须只是"报错/降级"，绝不能 panic ——
// 网关是长驻服务，parse 路径上的 panic 会直接带走全部在途请求。
// 用法： go test ./internal/upstream/ -run Fuzz -fuzz FuzzAggregate -fuzztime 60s

func FuzzAggregate(f *testing.F) {
	seeds := []string{
		"",
		"data: [DONE]\n\n",
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data:{}\n\n",
		"data: {\"choices\":[{\"index\":-1,\"delta\":{}}]}\n\n",
		"data: {\"choices\":[{\"index\":99999999999999999999}]}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":\"x\"}}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"a\",\"arguments\":\"{\"}}]}}]}\n\n",
		"data: {\"error\":{\"message\":\"boom\"}}\n\ndata: [DONE]\n\n",
		"data: " + strings.Repeat("a", 4096) + "\n\n",
		"event: error\ndata: {\"code\":6004}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":null}}]}\n\n\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// 允许的错误不算失败，只禁止 panic（Go 的 fuzz 会捕获 panic 并报告）。
		_, _ = Aggregate(strings.NewReader(raw))
		_, _ = AggregateContext(context.Background(), strings.NewReader(raw))
	})
}

func FuzzStream(f *testing.F) {
	seeds := []string{
		"",
		"data: [DONE]\n\n",
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"reasoning\":\"r\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n",
		"data: {\"choices\":[]}\n\n",
		": keep-alive\n\n",
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// 非流式聚合 + 流式透传两条路径都要能扛住任意输入。
		_, _ = Aggregate(strings.NewReader(raw))
		rec := httptest.NewRecorder()
		_ = Stream(rec, strings.NewReader(raw))
		_ = StreamContext(context.Background(), rec, strings.NewReader(raw))
	})
}

func FuzzSSEDataPayload(f *testing.F) {
	for _, s := range []string{"", "data:", "data: ", "data:[DONE]", "data: [DONE]", " data: x", "data: \x00", "\xff\xfe", "data:  [DONE]"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		payload, ok := SSEDataPayload(line)
		trimmed := strings.TrimRight(line, "\r\n")
		if !ok {
			// 非 data: 行必须判 false（注释/event 行由调用方另行处理）
			if strings.HasPrefix(trimmed, "data:") {
				t.Fatalf("data: 行被判为 false: %q", line)
			}
			return
		}
		if !strings.HasPrefix(trimmed, "data:") {
			t.Fatalf("非 data: 行被判为 true: %q", line)
		}
		// 规范行为（SSE spec）：剥掉 "data:"，且**仅当值以 SPACE 开头时再剥一个**。
		// 所以 `data:  x` 的 payload 是 " x"（不是 "x"）—— 下游 JSON 解析容忍前导空白。
		// 最初这里我断言"payload 不能有前导空白"，被 fuzz 判为误报；规范如此，改断言而非改实现。
		body := strings.TrimPrefix(trimmed, "data:")
		if want := strings.TrimPrefix(body, " "); payload != want {
			t.Fatalf("payload=%q want %q（源 %q）", payload, want, line)
		}
		// 关键语义：标准结束标记（data: 后恰好一个空格）必须被识别为 [DONE]。
		if body == " [DONE]" && payload != "[DONE]" {
			t.Fatalf("标准 [DONE] 未被识别: %q", line)
		}
	})
}

func FuzzPrepareBody(f *testing.F) {
	seeds := []string{
		`{"model":"m","messages":[]}`,
		`{}`,
		``,
		`null`,
		`[]`,
		`{"stream":true,"stream_options":{"include_usage":false}}`,
		`{"messages":[{"role":"tool","content":[{"type":"text","text":"a"}]}]}`,
		`{"functions":[{"name":"f"}]}`,
		`{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required"}`,
		`{"reasoning_effort":"high"}`,
		`{"messages":[{"role":"user","content":"x"}]}  trailing`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	efforts := map[string][]string{"m": {"low", "high"}}
	f.Fuzz(func(t *testing.T, raw string) {
		// 任意输入只允许被改写/透传，不允许 panic（sanitize 开关两条路径都跑）。
		_ = PrepareBodyOpt([]byte(raw), true)
		_ = PrepareBodyOpt([]byte(raw), false)
		_ = PrepareBodyOptWithEfforts([]byte(raw), true, efforts)
		// 契约：要么原样透传，要么产出合法 JSON（改写失败绝不能产出坏 JSON）。
		out := PrepareBodyOptWithEfforts([]byte(raw), false, efforts)
		if string(out) != raw && !json.Valid(out) {
			t.Fatalf("既非原样透传、又不是合法 JSON（源 %q）: %q", raw, out)
		}
	})
}

var _ = bytes.MinRead
var _ = io.Discard
