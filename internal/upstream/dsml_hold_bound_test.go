package upstream

import (
	"strings"
	"testing"
)

// 对抗审计发现（P1）：标记修复器的 hold / inBlock 缓冲此前**没有上界** —— 超长的"疑似标记"
// 前缀（如持续 `<`）或永不闭合的标记块会让内存随流长度线性增长，代理网关存在 DoS 面。
//
// 本用例守护修复后的契约：
//
//	① 单流缓冲严格有界（<= maxMarkupHold）；
//	② 超限后**放弃修复**（Enabled 转 false），已缓冲内容当正文原样透出，不丢字节；
//	③ 放弃之后继续 Feed 也不再累积（后续文本直接原样返回）。
func TestMarkupRepairHoldIsBounded(t *testing.T) {
	r := NewMarkupRepair(nil, true) // allowUnknown=true ⇒ 启用
	if !r.Enabled() {
		t.Fatal("前置：应处于启用状态")
	}
	// 未闭合的标记块：markupOpen 之后是无尽正文（永不出现 markupClose）
	huge := markupOpen + strings.Repeat("x", maxMarkupHold*3)
	emit, calls := r.Feed(huge)
	if len(calls) != 0 {
		t.Errorf("未闭合块不应还原出调用，实得 %d 个", len(calls))
	}
	if got := len(r.hold); got > maxMarkupHold {
		t.Errorf("缓冲无界：hold=%d > maxMarkupHold=%d", got, maxMarkupHold)
	}
	if r.Enabled() {
		t.Error("缓冲超限后应放弃修复（Enabled 应为 false），否则后续仍会无界累积")
	}
	if len(emit) != len(huge) {
		t.Errorf("放弃修复时必须原文全透出，emit=%d want=%d（不得吞字节）", len(emit), len(huge))
	}
	// 放弃后继续喂：不累积、原样返回
	more := strings.Repeat("y", 1<<20)
	emit2, calls2 := r.Feed(more)
	if len(calls2) != 0 || emit2 != more {
		t.Errorf("放弃后应原样透出后续文本（emit=%d want=%d，calls=%d）", len(emit2), len(more), len(calls2))
	}
	if r.hold != "" {
		t.Errorf("放弃后不得继续缓冲，hold=%d", len(r.hold))
	}
	if tail := r.Flush(); tail != "" {
		t.Errorf("放弃后 Flush 不应再吐出缓冲，实得 %d 字节", len(tail))
	}
}
