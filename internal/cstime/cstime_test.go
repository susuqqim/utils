package cstime

import (
	"testing"
	"time"
)

// 关键：CST 与宿主时区不同日时必须按 **CST** 算。
// UTC 2026-10-03 17:00 == CST 2026-10-04 01:00 → "今天"是 10-04，昨天是 10-03。
func TestDayUsesCSTRegardlessOfHostZone(t *testing.T) {
	utc := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	if got := Day(utc); got != "2026-10-04" {
		t.Fatalf("Day(UTC 17:00) = %q, want 2026-10-04（CST 已是次日 01:00）", got)
	}
	// CST 边界：UTC 15:59:59 → 仍是 CST 当日 23:59:59。
	if got := Day(time.Date(2026, 10, 3, 15, 59, 59, 0, time.UTC)); got != "2026-10-03" {
		t.Fatalf("CST 日界前应为 10-03, got %q", got)
	}
	// 跨年/闰月末：CST 2027-01-01 00:30（UTC 2026-12-31 16:30）→ 昨天 = 2026-12-31。
	prev := Day(time.Date(2026, 12, 31, 16, 30, 0, 0, time.UTC).In(Zone).AddDate(0, 0, -1))
	if prev != "2026-12-31" {
		t.Fatalf("跨年昨天 = %q, want 2026-12-31", prev)
	}
	// 闰年 2 月末
	if got := Day(time.Date(2028, 2, 29, 20, 0, 0, 0, time.UTC)); got != "2028-03-01" {
		t.Fatalf("闰月末 = %q, want 2028-03-01", got)
	}
}

// Today / Yesterday 与 Day 同口径（相对关系恒成立）。
func TestTodayYesterdayAreConsistent(t *testing.T) {
	today := Today()
	y := Day(time.Now().In(Zone).AddDate(0, 0, -1))
	if y == today {
		t.Fatalf("昨天不应等于今天（%s）", today)
	}
	if Yesterday() != y {
		// 允许跨零点：两次调用之间若刚过 CST 零点，Yesterday() 会前进一天。
		if Yesterday() != Day(time.Now().In(Zone).AddDate(0, 0, -2)) {
			t.Fatalf("Yesterday() 口径不一致")
		}
	}
}
