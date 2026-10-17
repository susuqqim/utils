package upstream

import "testing"

// P2（对抗审计发现，批次44 修复）：超限放弃分支在 inBlock 时已消费起始标记，
// 若不补回就会净丢 len(markupOpen) 字节，违反"一个字节都不丢"的契约。
// 本用例用审计给的最小复现守护：先喂 markupOpen+16KB（进入块内），再喂 64KB（触发超限）。
func TestMarkupRepairGiveUpKeepsMarkupBytes(t *testing.T) {
	r := NewMarkupRepair(nil, true)
	first := markupOpen + "xxxxxxxxxxxxxxxx" // 16 字节正文，确保进入 inBlock 且不到上限
	e1, c1 := r.Feed(first)
	second := make([]byte, maxMarkupHold+1)
	for i := range second {
		second[i] = 'y'
	}
	e2, c2 := r.Feed(string(second))
	if len(c1)+len(c2) != 0 {
		t.Errorf("不应还原出调用，实得 %d+%d", len(c1), len(c2))
	}
	in := len(first) + len(second)
	out := len(e1) + len(e2)
	if out != in {
		t.Errorf("字节丢失: 输入 %d，透出 %d，差 %d（应为 0；markupOpen 长 %d）", in, out, in-out, len(markupOpen))
	}
	if r.Enabled() {
		t.Error("超限后应放弃修复")
	}
	if tail := r.Flush(); tail != "" {
		t.Errorf("放弃后 Flush 不应再吐缓冲，实得 %d 字节", len(tail))
	}
}
