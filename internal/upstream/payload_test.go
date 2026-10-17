package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	// 未带 stream_options → 注入
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	// 已带 stream_options → include_usage 强制为 true（网关依赖末帧 usage 做用量/
	// 成本/缓存命中记账；客户端显式 false 会让整条统计链失去观测），但保留其它键。
	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false,"other_key":123}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true (forced)", so2["include_usage"])
	}
	if so2["other_key"] != float64(123) {
		t.Errorf("other stream_options key lost: %v", so2)
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestNormalizeMaxTokens 验证 max_completion_tokens 映射到 max_tokens。
func TestNormalizeMaxTokens(t *testing.T) {
	// 仅传 max_completion_tokens → 自动映射到 max_tokens
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","max_completion_tokens":16384}`), false, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m["max_tokens"].(float64); !ok || v != 16384 {
		t.Errorf("max_tokens = %v want 16384", m["max_tokens"])
	}

	// 同时传 max_tokens 与 max_completion_tokens → max_completion_tokens 覆盖
	out2 := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","max_tokens":100,"max_completion_tokens":32768}`), false, nil)
	var m2 map[string]any
	if err := json.Unmarshal(out2, &m2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m2["max_tokens"].(float64); !ok || v != 32768 {
		t.Errorf("max_tokens = %v want 32768", m2["max_tokens"])
	}

	// 仅传 max_tokens → 保持原值
	out3 := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","max_tokens":4096}`), false, nil)
	var m3 map[string]any
	if err := json.Unmarshal(out3, &m3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m3["max_tokens"].(float64); !ok || v != 4096 {
		t.Errorf("max_tokens = %v want 4096", m3["max_tokens"])
	}
}

// TestNormalizeToolChoicePolicy 验证 tool_choice 改写策略：省略不改写（实测上游默认行为
// 与显式 auto 一致）、none 抑制 tools、function 对象扁平化、required 原样保留。
func TestNormalizeToolChoicePolicy(t *testing.T) {
	// 携带 tools 且未指定 tool_choice → 保持省略，不注入 "auto"
	bodyWithTools := `{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"search"}}]}`
	out := PrepareBodyOptWithEfforts([]byte(bodyWithTools), false, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc, ok := m["tool_choice"]; ok {
		t.Errorf("tool_choice should stay omitted, got %v", tc)
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools must be preserved when tool_choice is omitted")
	}

	// 显式 required → 原样保留（A/B 探测：上游支持，3/3 触发工具调用）
	bodyRequired := `{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"search"}}],"tool_choice":"required"}`
	reqOut := PrepareBodyOptWithEfforts([]byte(bodyRequired), false, nil)
	var mReq map[string]any
	if err := json.Unmarshal(reqOut, &mReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc, ok := mReq["tool_choice"].(string); !ok || tc != "required" {
		t.Errorf("tool_choice = %v want 'required'", mReq["tool_choice"])
	}

	// 显式 none → 删除 tool_choice 并清空 tools
	bodyNone := `{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"search"}}],"tool_choice":"none"}`
	outNone := PrepareBodyOptWithEfforts([]byte(bodyNone), false, nil)
	var mNone map[string]any
	if err := json.Unmarshal(outNone, &mNone); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := mNone["tool_choice"]; ok {
		t.Errorf("tool_choice should be deleted on 'none', got %v", mNone["tool_choice"])
	}
	if _, ok := mNone["tools"]; ok {
		t.Errorf("tools should be suppressed on 'none', got %v", mNone["tools"])
	}

	// 显式 function 对象 → 扁平化为名称字符串
	bodyFn := `{"model":"glm-5.2","messages":[],"tools":[{"type":"function","function":{"name":"search"}}],"tool_choice":{"type":"function","function":{"name":"search"}}}`
	outFn := PrepareBodyOptWithEfforts([]byte(bodyFn), false, nil)
	var mFn map[string]any
	if err := json.Unmarshal(outFn, &mFn); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc, ok := mFn["tool_choice"].(string); !ok || tc != "search" {
		t.Errorf("tool_choice = %v want 'search'", mFn["tool_choice"])
	}
}

// TestReasoningModelsDefaultMaxEffort 验证推理模型默认注入 max 并通过降级管线落到最高支持档。
func TestReasoningModelsDefaultMaxEffort(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.3":           {"low", "medium", "high", "max"},
		"kimi-k3-1":         {"low", "high", "max"},
		"deepseek-v4-flash": {"low", "high"},
		"glm-5.2":           {"off"},
	}

	// glm-5.3 支持 max → 默认落到 max
	outGLM := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.3","messages":[]}`), false, efforts)
	var mGLM map[string]any
	_ = json.Unmarshal(outGLM, &mGLM)
	if eff, _ := mGLM["reasoning_effort"].(string); eff != "max" {
		t.Errorf("glm-5.3 reasoning_effort = %q want 'max'", eff)
	}

	// kimi-k3-1 支持 max → 默认落到 max
	outKimi := PrepareBodyOptWithEfforts([]byte(`{"model":"kimi-k3-1","messages":[]}`), false, efforts)
	var mKimi map[string]any
	_ = json.Unmarshal(outKimi, &mKimi)
	if eff, _ := mKimi["reasoning_effort"].(string); eff != "max" {
		t.Errorf("kimi-k3-1 reasoning_effort = %q want 'max'", eff)
	}

	// deepseek-v4-flash 最高支持 high → max 经降级管线落到 high
	outDS := PrepareBodyOptWithEfforts([]byte(`{"model":"deepseek-v4-flash","messages":[]}`), false, efforts)
	var mDS map[string]any
	_ = json.Unmarshal(outDS, &mDS)
	if eff, _ := mDS["reasoning_effort"].(string); eff != "high" {
		t.Errorf("deepseek-v4-flash reasoning_effort = %q want 'high'", eff)
	}

	// glm-5.2 非推理模型 → 不注入 reasoning_effort
	outNonReasoning := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, efforts)
	var mNon map[string]any
	_ = json.Unmarshal(outNonReasoning, &mNon)
	if eff, ok := mNon["reasoning_effort"]; ok {
		t.Errorf("glm-5.2 should not have reasoning_effort, got %v", eff)
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// schema/参数级 400 的识别（用于快速失败、不轮转烧号）。
func TestIsParameterSchemaError(t *testing.T) {
	if !IsParameterSchemaError(`{"code":11129,"msg":"invalid_function_call_parameters"}`) {
		t.Fatal("11129 must be treated as parameter/schema error")
	}
	if !IsParameterSchemaError(`{"msg":"invalid function call parameters"}`) {
		t.Fatal("english marker must be recognized")
	}
	if IsParameterSchemaError(`{"code":1004,"msg":"rate limit"}`) {
		t.Fatal("rate limit must NOT be a schema error")
	}
	if IsParameterSchemaError("") {
		t.Fatal("empty body must not match")
	}
}

// `null` / 非对象 JSON 不得 panic：改写不了就原样透传（上游会以 11101 明确拒绝）。
// 背景：`null` 是合法 JSON，Unmarshal 成功但 map 为 nil → 旧实现在 obj["stream"]=true 处 panic，
// 请求体为 null 的客户端会看到连接被掐断（而不是 400）。由 fuzz 目标 FuzzPrepareBody 发现，
// 并在真机复现（http: panic serving ... assignment to entry in nil map）。
func TestPrepareBodyDoesNotPanicOnNonObjectJSON(t *testing.T) {
	for _, raw := range []string{"null", "null ", "\nnull\n", "true", "123", "1.5", `"str"`, "[]", "[1,2]", "  ", "\t"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("输入 %q 触发 panic: %v", raw, r)
				}
			}()
			out := PrepareBodyOpt([]byte(raw), true)
			out2 := PrepareBodyOptWithEfforts([]byte(raw), true, map[string][]string{"m": {"low"}})
			// 契约：非对象输入一律原样返回（不改写、不产坏 JSON）。
			if string(out) != raw && len(out) != 0 {
				t.Errorf("输入 %q：应原样透传，实得 %q", raw, out)
			}
			if string(out2) != raw && len(out2) != 0 {
				t.Errorf("输入 %q（带 efforts）：应原样透传，实得 %q", raw, out2)
			}
		}()
	}
}

// 上游 fd3e142：GPT 系 max_tokens < 16 会被上游 400（11133），Claude Code 切模型时
// 发的极小探针会导致"永远切不过去、全号 503"。发送前抬到下限；非 GPT / 非数值 /
// 已达下限一律不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		want   int64
		absent bool
	}{
		{"GPT 系过小 → 抬到 16", `{"model":"gpt-6-sol","max_tokens":4}`, 16, false},
		{"GPT 系边界 15 → 抬到 16", `{"model":"gpt-6-luna","max_tokens":15}`, 16, false},
		{"GPT 系 16 不动", `{"model":"gpt-6-sol","max_tokens":16}`, 16, false},
		{"GPT 系大值不动", `{"model":"gpt-5.6-sol","max_tokens":4096}`, 4096, false},
		{"非 GPT 模型不动", `{"model":"hy4-preview-f","max_tokens":4}`, 4, false},
		{"未携带字段不动", `{"model":"gpt-6-sol"}`, 0, true},
		{"非数值不动（缺省）", `{"model":"gpt-6-sol","max_tokens":"abc"}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(c.body), false)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
			}
			v, has := obj["max_tokens"]
			if c.absent {
				// 未携带字段 / 非数值：一律不改写（保持原样透传）。注意"被整体删除"也必须算失败
				// —— 原实现 `if has && …` 会在字段消失时静默通过（审查 4 号的 P2-7）。
				switch c.name {
				case "未携带字段不动":
					if has {
						t.Errorf("字段本就不该出现，实得 %#v", v)
					}
				default:
					if str, ok := v.(string); !ok || str != "abc" {
						t.Errorf("非数值应原样保留 %q，实得 %#v", "abc", v)
					}
				}
				return
			}
			if !has {
				t.Fatalf("max_tokens 丢失：%s", out)
			}
			var got int64
			switch n := v.(type) {
			case float64:
				got = int64(n)
			case int64:
				got = n
			default:
				t.Fatalf("max_tokens 类型异常 %T", v)
			}
			if got != c.want {
				t.Errorf("max_tokens=%d want %d", got, c.want)
			}
		})
	}
}

// clamp 的**边界**（审查 A 的 P1-3）：只抬 0<v<16；0/负数（客户端表"不限"）/越界一律不动，
// 否则会把"本该上游 400 明示"的请求变成 16 token 截断的"成功"。
func TestClampGPTMinMaxTokensBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"0 不动", `{"model":"gpt-6-sol","max_tokens":0}`, 0},
		{"-1 不动（不限语义）", `{"model":"gpt-6-sol","max_tokens":-1}`, -1},
		{"超大值不动（不降级成 16）", `{"model":"gpt-6-sol","max_tokens":1e19}`, 1e19},
		{"max_completion_tokens 先归一再抬", `{"model":"gpt-6-sol","max_completion_tokens":1}`, 16},
		{"global: 前缀同样生效", `{"model":"global:gpt-6-sol","max_tokens":4}`, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(c.body), false)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("输出非法 JSON: %v (%s)", err, out)
			}
			v, ok := obj["max_tokens"].(float64)
			if !ok {
				t.Fatalf("max_tokens 类型异常: %#v", obj["max_tokens"])
			}
			if v != c.want {
				t.Errorf("max_tokens=%v want %v（out=%s）", v, c.want, out)
			}
		})
	}
}
