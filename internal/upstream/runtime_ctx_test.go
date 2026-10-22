package upstream

import (
	"os"
	"strings"
	"testing"
)

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
