package upstream

import (
	"strings"
	"testing"
)

// 【2026-10-21 自伤回归】sanitizeBrandRe 的产出不得再是上游指纹。
//
// 上游 issue #18 实测：CN 域逐字匹配 `You are an AI agent running on a compatible client`
// （**不带结尾句号**即 400/11128；带句号反而放行）。
// 而原替换串 "$1 running on a compatible client" 产出的正是这句话 —— 净化规则自己在制造指纹。
func TestBrandRewriteNoLongerProducesFingerprint(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by DeepSeek",
		"You are an AI agent powered by DeepSeek Harness",
		"You are a coding agent powered by OpenCode",
		"You are an AI assistant powered by Cline",
	}
	for _, in := range cases {
		out := sanitizeText(in)
		if strings.Contains(out, "running on a compatible client") {
			t.Errorf("输入 %q → %q：改写结果仍含上游指纹形态", in, out)
		}
		if strings.Contains(out, "powered by") {
			t.Errorf("输入 %q → %q：品牌组合未被去掉", in, out)
		}
		t.Logf("%q → %q", in, out)
	}
}

// 客户端历史里已残留的指纹串（不带句号）必须被改写掉。
func TestExistingFingerprintIsScrubbed(t *testing.T) {
	fp := "You are an AI agent running on a compatible client"
	out := sanitizeText(fp)
	if out == fp {
		t.Fatalf("历史指纹串未被改写: %q", out)
	}
	if strings.Contains(out, "running on a compatible client") {
		t.Errorf("改写结果仍含指纹: %q", out)
	}
	t.Logf("%q → %q", fp, out)
}

// 预检必须能命中该指纹（否则 sanitizeText 直接原样返回，改写表根本不会执行）。
func TestFingerprintPrecheckHits(t *testing.T) {
	if !hasFingerprint("You are an AI agent running on a compatible client") {
		t.Fatal("预检未命中上游指纹 → 整单跳过净化")
	}
}
