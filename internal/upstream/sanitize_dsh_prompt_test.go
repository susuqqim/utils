// sanitize_dsh_prompt_test.go —— DSH 提示词身份句改写的回归。
//
// 夹具 testdata/dsh_prompt_paragraphs.txt 直接来自 DSH 客户端 app.asar 的三个提示词常量
// （HARNESS_SOURCE_PROMPT / WEB_SURFACE_PROMPT / LEGACY 前缀），是**上游真实收到的原文**。
//
// 守三条不变量：
//  1. 改写后不残留 DeepSeek 产品标识（DeepSeek Harness / DSH 自称 / __DSH_BOOT__）；
//  2. 语义骨架必须保留（checkout / Web GUI / runtime context / boot payload / 同义句）；
//  3. 二次净化幂等。
package upstream

import (
	"os"
	"strings"
	"testing"
)

func dshPromptFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/dsh_prompt_paragraphs.txt")
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	return string(b)
}

// 每条规则都必须真的改写，且 from 必须真的出现在夹具里（防止形近字符/空操作）。
func TestDSHPromptRulesAreEffective(t *testing.T) {
	fixture := dshPromptFixture(t)
	for i, rw := range dshPromptRewrites {
		if rw[0] == rw[1] {
			t.Errorf("pair%d 是空操作: %q", i, rw[0])
		}
		if !strings.Contains(fixture, rw[0]) {
			t.Errorf("pair%d 的 from 不在夹具里（规则永远不会命中）: %q", i, rw[0])
		}
	}
}

// 夹具整段净化后：产品标识清零、语义骨架保留、幂等。
func TestDSHPromptFingerprintsRemoved(t *testing.T) {
	in := dshPromptFixture(t)
	if !hasFingerprint(in) {
		t.Fatal("预检必须命中 DSH 提示词，否则整单跳过净化")
	}
	out := sanitizeText(in)

	for _, banned := range []string{"DeepSeek Harness", "DSH itself", "window.__DSH_BOOT__", "dsh web"} {
		if strings.Contains(out, banned) {
			t.Errorf("改写后仍残留产品标识 %q", banned)
		}
	}

	for _, keep := range []string{
		"implementation checkout",
		"Web GUI",
		"runtime context",
		"boot payload",
		"inspect or extend this implementation itself",
		"current working directory",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("语义骨架丢了 %q（改写过度）", keep)
		}
	}

	if again := sanitizeText(out); again != out {
		t.Fatalf("非幂等：改写结果再次净化仍变化")
	}
}

// 只出现锚点（没有整句）时也必须进入净化路径，不能因预检漏判而原样上行。
func TestDSHPromptProbeAlone(t *testing.T) {
	for _, probe := range dshPromptProbes {
		if !hasFingerprint("prefix " + probe + " suffix") {
			t.Errorf("锚点 %q 未触发预检", probe)
		}
	}
}
