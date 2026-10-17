package tray

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestActionForMenuID(t *testing.T) {
	cases := map[int]MenuAction{
		menuIDOpenPanel:       ActionOpenPanel,
		menuIDRestart:         ActionRestart,
		menuIDToggleAutostart: ActionToggleAutostart,
		menuIDExit:            ActionExit,
		0:                     ActionNone,
		999:                   ActionNone,
	}
	for id, want := range cases {
		if got := actionForMenuID(id); got != want {
			t.Errorf("actionForMenuID(%d) = %v, want %v", id, got, want)
		}
	}
}

func TestTruncateUTF16(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 127, "hello"},
		{"abcdef", 3, "abc"},
		{"abcdef", 6, "abcdef"},
		{"中文abc", 2, "中文"},
		{"中文abc", 1, "中"},
		{"", 5, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"😀", 2, "😀"},
		{"😀", 1, ""},   // 不能把代理对切成一半
		{"😀😀", 3, "😀"}, // 第二个代理对不完整，丢掉
		{"a😀", 1, "a"},
	}
	for _, c := range cases {
		got := TruncateUTF16(c.in, c.max)
		if got != c.want {
			t.Errorf("TruncateUTF16(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		// 不变式：结果不超限，且本身是合法 UTF-8（无孤立代理）
		if n := len(utf16.Encode([]rune(got))); n > c.max && c.max > 0 {
			t.Errorf("TruncateUTF16(%q, %d) 结果 %d 个 UTF-16 单元，超出上限", c.in, c.max, n)
		}
	}
}

func TestTooltipText(t *testing.T) {
	got := TooltipText("1.11.6", 5, 5, true)
	if !strings.Contains(got, "1.11.6") || !strings.Contains(got, "5/5") {
		t.Errorf("健康 tooltip 应含版本与账号数，得到 %q", got)
	}

	got = TooltipText("", 0, 0, false)
	if !strings.Contains(got, "未响应") {
		t.Errorf("不健康 tooltip 应提示未响应，得到 %q", got)
	}

	// 超长版本号必须被截断到 szTip 容量内。
	long := strings.Repeat("v", 500)
	got = TooltipText(long, 1, 2, true)
	if n := len(utf16.Encode([]rune(got))); n > tipMaxUnits {
		t.Errorf("tooltip 截断失败：%d 个 UTF-16 单元 > %d", n, tipMaxUnits)
	}
}

func TestFormatTooltip(t *testing.T) {
	st := StatusInfo{
		Online:        true,
		Healthy:       4,
		Total:         5,
		Cooling:       1,
		TotalCredits:  25872,
		DailyCredits:  14.20,
		Urgent48h:     86,
		CNHealthy:     3,
		CNTotal:       4,
		GlobalHealthy: 1,
		GlobalTotal:   1,
	}
	tip := FormatTooltip("1.11.6", st)
	if !strings.Contains(tip, "4/5 账号在线") {
		t.Errorf("expected account counts in tip, got: %q", tip)
	}
	if !strings.Contains(tip, "1冷却") {
		t.Errorf("expected cooling count in tip, got: %q", tip)
	}
	if !strings.Contains(tip, "25,872") {
		t.Errorf("expected credits in tip, got: %q", tip)
	}
	if !strings.Contains(tip, "14.20") {
		t.Errorf("expected daily usage in tip, got: %q", tip)
	}
	if n := len(utf16.Encode([]rune(tip))); n > tipMaxUnits {
		t.Errorf("tip length exceeds UTF-16 limit: %d > %d", n, tipMaxUnits)
	}

	offline := FormatTooltip("1.11.6", StatusInfo{Online: false})
	if !strings.Contains(offline, "未响应") {
		t.Errorf("expected offline message in tip, got: %q", offline)
	}
}

func TestMenuStatusItems(t *testing.T) {
	st := StatusInfo{
		Online:        true,
		Healthy:       5,
		Total:         5,
		CNHealthy:     4,
		CNTotal:       4,
		GlobalHealthy: 1,
		GlobalTotal:   1,
		TotalCredits:  25872,
		DailyCredits:  14.20,
		Urgent48h:     86,
	}
	items := MenuStatusItems(st)
	if len(items) != 3 {
		t.Fatalf("expected 3 status items, got %d: %v", len(items), items)
	}
	if !strings.Contains(items[0], "服务正常") || !strings.Contains(items[0], "CN 4/4") {
		t.Errorf("item 0 unexpected: %q", items[0])
	}
	if !strings.Contains(items[1], "25,872") || !strings.Contains(items[1], "14.20") {
		t.Errorf("item 1 unexpected: %q", items[1])
	}
	if !strings.Contains(items[2], "48h 内到期 86") {
		t.Errorf("item 2 unexpected: %q", items[2])
	}

	// 0 临期时不展示临期行
	stNoUrgent := st
	stNoUrgent.Urgent48h = 0
	items2 := MenuStatusItems(stNoUrgent)
	if len(items2) != 2 {
		t.Fatalf("expected 2 status items when urgent is 0, got %d", len(items2))
	}

	// 离线时只返回离线行
	itemsOffline := MenuStatusItems(StatusInfo{Online: false})
	if len(itemsOffline) != 1 || !strings.Contains(itemsOffline[0], "离线") {
		t.Errorf("unexpected offline items: %v", itemsOffline)
	}
}

func TestFormatCredits(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-10, "0"},
		{0, "0"},
		{5, "5"},
		{123, "123"},
		{1000, "1,000"},
		{25872, "25,872"},
		{1234567, "1,234,567"},
	}
	for _, c := range cases {
		if got := formatCredits(c.in); got != c.want {
			t.Errorf("formatCredits(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
