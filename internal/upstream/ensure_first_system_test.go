package upstream

import (
	"encoding/json"
	"testing"
)

// ensureFirstSystem 的回退即变红用例集。
// 背景：上游 2026-11 起对「首条消息必须是 system」做结构校验，
// 违反即 400 code=11128 "first message is not system prompt"（非内容指纹）。
// 历史坑：该兜底此前只对 global 生效，CN 直通 ⇒ passthrough 模式下客户端首条非 system 就整单挂。
func TestEnsureFirstSystem(t *testing.T) {
	roles := func(t *testing.T, body []byte) []string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, body)
		}
		msgs, _ := obj["messages"].([]any)
		out := make([]string, 0, len(msgs))
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			r, _ := mm["role"].(string)
			out = append(out, r)
		}
		return out
	}

	// 1) 完全没有 system → 前置中性 fallback，原消息顺序不变
	out := ensureFirstSystem([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`))
	if got := roles(t, out); len(got) != 3 || got[0] != "system" || got[1] != "user" || got[2] != "assistant" {
		t.Fatalf("no-system case roles=%v", got)
	}

	// 2) 首条是 assistant（prefill 形态）→ 同样前置
	out = ensureFirstSystem([]byte(`{"messages":[{"role":"assistant","content":"a"},{"role":"user","content":"hi"}]}`))
	if got := roles(t, out); got[0] != "system" || len(got) != 3 {
		t.Fatalf("assistant-first roles=%v", got)
	}

	// 3) 后面已有 system → 前移而不是新造一条（内容必须一字不动）
	out = ensureFirstSystem([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"SYS-X"},{"role":"assistant","content":"a"}]}`))
	if got := roles(t, out); len(got) != 3 || got[0] != "system" || got[1] != "user" || got[2] != "assistant" {
		t.Fatalf("move-system roles=%v", got)
	}
	if s := string(out); !contains(s, `"SYS-X"`) {
		t.Fatalf("前移后原 system 内容丢失: %s", s)
	}
	if contains(string(out), "You are a helpful assistant.") {
		t.Fatalf("已有 system 时不应再补 fallback: %s", out)
	}

	// 4) 首条已是 system → 字节级零改写（保护前缀缓存）
	in := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}]}`)
	if out4 := ensureFirstSystem(in); string(out4) != string(in) {
		t.Fatalf("首条 system 时被改写: %s -> %s", in, out4)
	}

	// 5) 坏 body / 空 messages → 原样返回，不二次错误化
	bad := []byte(`{not json`)
	if string(ensureFirstSystem(bad)) != string(bad) {
		t.Fatal("坏 body 不应被改写")
	}
	empty := []byte(`{"messages":[]}`)
	if string(ensureFirstSystem(empty)) != string(empty) {
		t.Fatal("空 messages 不应被改写")
	}
}

// 小工具：避免额外 import strings（本包其它文件已大量使用，但保持本文件自洽）。
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
