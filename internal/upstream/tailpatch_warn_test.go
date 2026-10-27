// tailpatch_warn_test.go 覆盖「补末帧」WARN 的节流判据（B3，2026-10-27）。
//
// 背景：上游抖动时会连续多轮缺规范收尾帧（finish_reason 贴在数据帧上），此前**每条流**
// 都打一条 WARN。实测同一时段可连刷数十条，把日志环里真正有用的错误挤掉。
// 节流口径与 noDoneWarnEvery 同构：首次必打（保留可观测性），此后每 N 次一条。
//
// 这里只钉判据（纯函数），不碰全局 log 输出与包级计数器 —— 并行用例之间会互相干扰，
// 见 AGENTS.md「测试约定 2」。
package upstream

import "testing"

func TestShouldWarnTailPatched(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{1, true},   // 首次必打：新进程第一次出现就要能看见
		{2, false},  // 其后静默
		{99, false}, //
		{100, true}, // 满一轮汇总一条
		{101, false},
		{200, true},
		{201, false},
	}
	for _, c := range cases {
		if got := shouldWarnTailPatched(c.n); got != c.want {
			t.Errorf("shouldWarnTailPatched(%d)=%v want %v（首条必打，其后每 %d 条一条）",
				c.n, got, c.want, tailPatchedWarnEvery)
		}
	}
}

// 节流窗口必须 >1，否则「节流」等于逐条打（把实现改回 n>=1 时本用例变红）。
func TestTailPatchedWarnEveryIsMeaningful(t *testing.T) {
	if tailPatchedWarnEvery <= 1 {
		t.Fatalf("tailPatchedWarnEvery=%d，节流窗口必须 >1", tailPatchedWarnEvery)
	}
}
