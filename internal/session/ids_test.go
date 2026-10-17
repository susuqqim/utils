package session

import (
	"strings"
	"testing"
)

// TestResolveConversationID 覆盖 conversationId 提取的 snake/camel/缺失三态：
//   - metadata.conversation_id / metadata.conversationId → 取值
//   - 顶层 conversation_id / conversationId → 取值（snake 优先同 ExtractKey）
//   - 缺失 / 只有 user_id → ""（会话头族语义只认对话 ID，绝不回落 user_id）
func TestResolveConversationID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"metadata session_id", `{"metadata":{"session_id":"sess-1"}}`, "sess-1"},
		{"metadata sessionId", `{"metadata":{"sessionId":"sess-2"}}`, "sess-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"top-level session_id", `{"session_id":"sess-3"}`, "sess-3"},
		{"top-level sessionId", `{"sessionId":"sess-4"}`, "sess-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"metadata userId only", `{"metadata":{"userId":"u1"}}`, ""},
		{"top-level userId only", `{"userId":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveConversationID([]byte(c.body)); got != c.want {
				t.Errorf("ResolveConversationID(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestNewMessageIDFormat 32 位 hex 且不为空、两次调用大概率不同（随机性冒烟）。
func TestNewMessageIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

// TestRequestIDForKeyStability 同 key 恒稳定、异 key 各不同、空 key 每次新值。
func TestRequestIDForKeyStability(t *testing.T) {
	// 预热清空包级缓存，避免其他测试污染 key（测试隔离）。
	k1a := RequestIDForKey("conv-a")
	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}
	k2 := RequestIDForKey("conv-b")
	if k1a == k2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// 空 key：每次调用生成新值（无会话则无"会话内稳定"语义）。
	e1 := RequestIDForKey("")
	e2 := RequestIDForKey("")
	if e1 == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", e1)
	}
	// 稳定值自身也须是 32 hex（可作 B3 TraceId 直接使用）。
	for _, id := range []string{k1a, k2, e1} {
		if len(id) != 32 {
			t.Errorf("RequestIDForKey value %q len=%d want 32", id, len(id))
		}
	}
}

// TestTurnKeyExtraction 轮级兜底键的提取：取**最后一条** user 消息的「序号+文本」，
// 轮内追加 assistant/tool 消息不改变键；无 user / 无文本 / 坏 JSON 一律空串。
func TestTurnKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"你好"}]}`, "u0:你好"},
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "u2:第二问"},
		// agent 多步：轮内追加 assistant/tool 消息，末条 user 位置与内容不变 → 同键。
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"任务"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"结果"}]}`, "u0:任务"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:看图"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation 轮级 ID 是纯派生：同键恒同值、异键异值、空键每次新值、
// 形态恒 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	a1 := TurnRequestID("u0:同一个问题")
	a2 := TurnRequestID("u0:同一个问题")
	if a1 != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", a1, a2)
	}
	if b := TurnRequestID("u0:另一个问题"); b == a1 {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// 同文本但序号不同（不同轮里内容相同的提问）也要分开。
	if c := TurnRequestID("u2:同一个问题"); c == a1 {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// 空键：无轮可聚合 → 每次新值（保持原有请求级独立行为）。
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}
	for _, id := range []string{a1, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}

// TestConversationFingerprint 多轮会话指纹测试：
//   - 单轮与多轮追加（assistant / 后续 user 提问）提取到的指纹恒定不变；
//   - 系统提示词（system / developer）在前不影响首条 user 消息的提取；
//   - 多模态 parts 文本能够被正确提取拼接成指纹；
//   - 首条 user 消息不同，产生不同指纹；
//   - 无 user 消息 / 空文本 / 坏 JSON 返回 ""；
//   - 指纹形态恒为 "fp:" + 32 位 hex。
func TestConversationFingerprint(t *testing.T) {
	turn1 := `{"messages":[{"role":"user","content":"帮我写个贪吃蛇"}]}`
	turn2 := `{"messages":[{"role":"user","content":"帮我写个贪吃蛇"},{"role":"assistant","content":"好的，这是贪吃蛇代码..."},{"role":"user","content":"增加暂停功能"}]}`
	turn3 := `{"messages":[{"role":"system","content":"你是一名资深程序员"},{"role":"user","content":"帮我写个贪吃蛇"},{"role":"assistant","content":"好的"},{"role":"user","content":"继续"}]}`
	diff := `{"messages":[{"role":"user","content":"今天天气怎么样"}]}`

	fp1 := ConversationFingerprint([]byte(turn1))
	fp2 := ConversationFingerprint([]byte(turn2))
	fp3 := ConversationFingerprint([]byte(turn3))
	fpDiff := ConversationFingerprint([]byte(diff))

	if fp1 == "" {
		t.Fatal("turn1 fingerprint should not be empty")
	}
	if !strings.HasPrefix(fp1, "fp:") || len(fp1) != 35 { // "fp:" (3) + 32 hex = 35 chars
		t.Fatalf("unexpected format: %q len=%d", fp1, len(fp1))
	}
	if fp1 != fp2 {
		t.Errorf("multi-turn dialogue should keep same fingerprint across turns: turn1=%q turn2=%q", fp1, fp2)
	}
	if fp1 != fp3 {
		t.Errorf("presence of system message before user message should not alter first user fingerprint: turn1=%q turn3=%q", fp1, fp3)
	}
	if fp1 == fpDiff {
		t.Errorf("different first message should produce different fingerprint: %q", fp1)
	}

	// 多模态测试
	mm := `{"messages":[{"role":"user","content":[{"type":"text","text":"帮我写个贪吃蛇"}]}]}`
	if fpMM := ConversationFingerprint([]byte(mm)); fpMM != fp1 {
		t.Errorf("multimodal parts text should match equivalent string content: %q vs %q", fpMM, fp1)
	}

	// 边界与非法输入
	emptyCases := []string{
		"",
		"not-json",
		"{}",
		`{"messages":[]}`,
		`{"messages":[{"role":"system","content":"sys only"}]}`,
		`{"messages":[{"role":"user","content":""}]}`,
		`{"messages":[{"role":"user","content":"   "}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
	}
	for _, c := range emptyCases {
		if got := ConversationFingerprint([]byte(c)); got != "" {
			t.Errorf("ConversationFingerprint(%q) = %q, want empty", c, got)
		}
	}
}
