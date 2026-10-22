package upstream

import (
	"strings"
	"testing"
)

// 【2026-10-22 回归】改写表必须真的改写：曾出现 from/to 被写成同一串（或 from 混入形近字符），
// 导致规则恒不命中、指纹原样上行 —— 而单测因为断言写反（既要求"不含"又要求"含"）照样绿。
func TestRewriteTableIsEffective(t *testing.T) {
	for i, rw := range sanitizeRewrites {
		if rw[0] == rw[1] {
			t.Errorf("pair%d 是空操作：from==to=%q", i, rw[0])
		}
		if rw[0] == "" || rw[1] == "" {
			t.Errorf("pair%d 有空串", i)
		}
	}
}

// 规则字符串里除「11‑128」（刻意用 U+2011 形近连字符，网关入站计数用）外，
// 不允许出现任何非 ASCII 字符：形近字符会让规则看起来一样、实际永不命中。
func TestRewriteRulesAreAsciiOnly(t *testing.T) {
	const allow = "11‑128"
	for i, rw := range sanitizeRewrites {
		for _, s := range rw {
			if s == allow {
				continue
			}
			for _, r := range s {
				if r > 127 {
					t.Errorf("pair%d 含非 ASCII 字符 %U：%q", i, r, s)
					break
				}
			}
		}
	}
}

// DSH 的 GUI 描述段必须被真正改写掉（这是 CN 域 11128 的稳定指纹）。
func TestGUISentenceRewritten(t *testing.T) {
	in := "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", they mean this GUI."
	if !hasFingerprint(in) {
		t.Fatal("预检必须命中该指纹，否则整单跳过净化")
	}
	out := sanitizeText(in)
	if out == in {
		t.Fatal("改写后内容未变化 —— 规则没生效")
	}
	if strings.Contains(out, "whose URL the runtime context names") {
		t.Fatalf("改写后仍残留 GUI 指纹：%q", out)
	}
	if !strings.Contains(out, "whose URL is named in the runtime context") {
		t.Fatalf("改写结果不是中性表述：%q", out)
	}
	// 幂等：二次净化不再变化
	if again := sanitizeText(out); again != out {
		t.Fatalf("非幂等：%q -> %q", out, again)
	}
}
