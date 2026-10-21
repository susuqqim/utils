package upstream

import (
	"os"
	"strings"
	"testing"
)

// 【2026-10-22 回归】DSH 新版 system prompt 的 "whose URL the runtime context names"
// 是 CN 域稳定复现的 11128 指纹（生产抓包逐字节二分 + 5/5 重复确认）。
func TestRuntimeContextPhraseScrubbed(t *testing.T) {
	bad := "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to..."
	out := sanitizeText(bad)
	if strings.Contains(out, "whose URL the runtime context names") {
		t.Fatalf("指纹未被改写：%q", out)
	}
	if !strings.Contains(out, "at the local address shown above") {
		t.Errorf("改写结果不符合预期：%q", out)
	}
	if !hasFingerprint(bad) {
		t.Error("预检未命中该指纹 → 整单跳过净化")
	}
	t.Logf("%q\n  → %q", bad, out)
}

// 用生产抓到的真实失败请求，验证净化后指纹消失。
func TestRealFailingRequestScrubbed(t *testing.T) {
	raw, err := os.ReadFile("/tmp/keep_20261022-000406.153.json")
	if err != nil {
		t.Skip("无真实样本（/tmp 已被清理）")
	}
	s := string(raw)
	if !strings.Contains(s, "whose URL the runtime context names") {
		t.Skip("样本里不含该指纹")
	}
	out := string(PrepareBodyOpt(raw, true))
	if strings.Contains(out, "whose URL the runtime context names") {
		t.Error("净化后仍残留 runtime context 指纹 —— 上游会返回 11128")
	} else {
		t.Logf("✓ 真实请求净化后指纹已消除（%d B → %d B）", len(s), len(out))
	}
}
