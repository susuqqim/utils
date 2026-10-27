package upstream

import (
	"strings"
	"testing"
)

// 【2026-10-27 生产回归】DSH 的 @ 路径说明段被上游新加入逐字指纹黑名单：
// 同一 prompt / 模型 / 网关版本，10-26 可过；10-27 原文稳定 11128；逐字最小改写后 200。
// 本用例同时钉：预检命中、4 个连续子串全部消失、中性语义保留、二次净化幂等。
func TestDSHAtPathInstructionsRewritten(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. " +
		"A trailing slash marks a directory: list it when its contents matter."

	if !hasFingerprint(in) {
		t.Fatal("预检必须命中 DSH @ 路径指纹，否则整单跳过净化")
	}
	out := sanitizeText(in)
	if out == in {
		t.Fatal("改写后内容未变化 —— 新规则未生效")
	}
	for _, banned := range []string{
		"Tokens prefixed with @",
		"the user explicitly referenced",
		"absolute paths identify files or directories",
		"list it when its contents matter",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("改写后仍残留 DSH 指纹 %q：%q", banned, out)
		}
	}
	for _, neutral := range []string{
		"Tokens beginning with @",
		"the user explicitly mentioned",
		"absolute paths point to files or directories",
		"list it when its contents are relevant",
	} {
		if !strings.Contains(out, neutral) {
			t.Errorf("改写后缺少中性表述 %q：%q", neutral, out)
		}
	}
	if again := sanitizeText(out); again != out {
		t.Fatalf("非幂等：%q -> %q", out, again)
	}
}

// 只出现该段首句也必须改写；真实请求可能被客户端裁剪，不能依赖后续三句存在。
func TestDSHAtPathFingerprintProbeAlone(t *testing.T) {
	in := "Tokens prefixed with @"
	if got := sanitizeText(in); got != "Tokens beginning with @" {
		t.Fatalf("sanitizeText(%q)=%q", in, got)
	}
}
