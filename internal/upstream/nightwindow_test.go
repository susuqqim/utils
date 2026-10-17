package upstream

import (
	"testing"
	"time"
)

// 钉住夜猫子计数窗口的**边界**（借鉴上游 cfec8f4 的"钉窗口时钟"做法）。
//
// 为什么必须钉：窗口是 CST 口径（23:00–08:00），而 gating 用例若依赖真实 time.Now()，
// 在每日 23:00/08:00 前后或跨 CST 午夜运行时会出现"偶发红/偶发绿"—— 这类 flaky 一旦出现，
// 团队往往会去调期望值而不是查逻辑，最终把真缺陷调没。本用例显式传入时钟，结论与运行时刻无关。
func TestInNightWindowBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		h, m int
		want bool
	}{
		{22, 59, false}, // 窗口前一刻
		{23, 0, true},   // 左闭
		{23, 59, true},
		{0, 30, true}, // 跨 CST 午夜
		{7, 59, true},
		{8, 0, false}, // 右开
		{12, 0, false},
	}
	for _, c := range cases {
		ts := time.Date(2026, 10, 9, c.h, c.m, 0, 0, cst)
		if got := InNightWindow(ts); got != c.want {
			t.Errorf("InNightWindow(%02d:%02d CST) = %v, want %v（窗口 23:00–08:00 左闭右开）", c.h, c.m, got, c.want)
		}
	}
	// 结论不得随宿主时区变化：同一瞬间用 UTC 表达，仍应入窗口
	utc := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC) // == 23:00 CST
	if !InNightWindow(utc) {
		t.Error("15:00Z（= 23:00 CST）应入窗口 —— 窗口是 CST 口径，不能随进程 TZ 漂移")
	}
}
