package upstream

import (
	"strings"
	"testing"
)

// 回归（2026-10-21 上游收紧渠道校验 11128）：提示词里的「身份句 + 品牌」组合会被
// 判定为非官方客户端。用最小改写（只换 "powered by …" 半句）保住角色定义。
func TestSanitizeBrandIdentitySentence(t *testing.T) {
	cases := []struct{ in, wantNot string }{
		{"You are an AI agent powered by DeepSeek Harness.", "DeepSeek Harness"},
		{"You are a coding agent powered by the deepseek-v4.1-flash model.", "deepseek-v4.1-flash"},
		{"You are an AI assistant powered by OpenCode.", "OpenCode"},
		{"You are an AI agent powered by DeepSeek Harness.\n接下来是用户指令。", "DeepSeek Harness"},
	}
	for _, c := range cases {
		got := sanitizeText(c.in)
		if strings.Contains(got, c.wantNot) {
			t.Errorf("未中性化：in=%q got=%q", c.in, got)
		}
		if !strings.Contains(got, "You are a") {
			t.Errorf("角色定义被误删：in=%q got=%q", c.in, got)
		}
		// 【2026-10-21 修正】旧断言要求结果含 "running on a compatible client"，
		// 但那正是上游 CN 域身份句指纹（issue #18：不带结尾句号即 400/11128）。
		// 断言 bug 为「期望行为」等于把自伤固化，改为断言绝不落入指纹形态。
		if strings.Contains(got, "compatible client") {
			t.Errorf("改写结果落入上游指纹形态（会触发 11128）：in=%q got=%q", c.in, got)
		}
		if !strings.Contains(got, "general-purpose assistant") {
			t.Errorf("未换成中性表述：in=%q got=%q", c.in, got)
		}
	}
}

// 普通文本里的 "powered by" 不得被改写（零回归）。
func TestSanitizeBrandNotTouchedForPlainText(t *testing.T) {
	cases := []string{
		"This server is powered by renewable energy.",
		"The model was powered by DeepSeek Harness internally.",
		"powered by",
	}
	for _, in := range cases {
		if got := sanitizeText(in); got != in {
			t.Errorf("不应改写：in=%q got=%q", in, got)
		}
	}
}

// 幂等：重复净化不得二次改写或破坏语义。
func TestSanitizeBrandIdempotent(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness."
	once := sanitizeText(in)
	twice := sanitizeText(once)
	if once != twice {
		t.Errorf("非幂等：once=%q twice=%q", once, twice)
	}
}
