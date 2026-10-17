package usage

import (
	"testing"
	"time"
)

// 陷阱一：本地双域存在**同名模型** —— 按模型聚合必须按 realm 分开，否则账目错配且不报错。
func TestModelKeySeparatesRealms(t *testing.T) {
	cn := ModelKey("cn", "gpt-6-sol")
	gl := ModelKey("global", "gpt-6-sol")
	if cn == gl {
		t.Fatal("双域同名模型必须是不相同的分组键（否则会把两域用量合并成一个数字）")
	}
	if cn.String() == gl.String() {
		t.Fatalf("键字符串也必须可区分：%q vs %q", cn.String(), gl.String())
	}
	if cn.String() != "cn|gpt-6-sol|" {
		t.Errorf("键表示应稳定可读，实得 %q", cn.String())
	}
	// 同域同模型必须相等（可作 map 键）
	if ModelKey("cn", "gpt-6-sol") != cn {
		t.Error("同域同模型应为同一键")
	}
}

// 陷阱二：按天必须用 **CST 自然日**（UTC+8），宿主 UTC 时区下用本地日历日会差一天。
func TestCSTDayBoundary(t *testing.T) {
	// 2026-10-09 15:59:59Z = 2026-10-09 23:59:59 CST ⇒ 仍是 10-09
	a := time.Date(2026, 10, 9, 15, 59, 59, 0, time.UTC)
	if got := CSTDay(a); got != "2026-10-09" {
		t.Errorf("15:59:59Z 应属 CST 10-09，实得 %s", got)
	}
	// 2026-10-09 16:00:00Z = 2026-10-10 00:00:00 CST ⇒ 翻到 10-10
	b := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	if got := CSTDay(b); got != "2026-10-10" {
		t.Errorf("16:00:00Z 应属 CST 10-10，实得 %s", got)
	}
	// 用非 UTC 的输入时区也要返回同一 CST 日（不随宿主时区变化）
	loc := time.FixedZone("Kathmandu", 5*3600+45*60)
	if got := CSTDay(a.In(loc)); got != "2026-10-09" {
		t.Errorf("换宿主时区后 CST 日必须不变，实得 %s", got)
	}
}

func TestDayKeyForCombination(t *testing.T) {
	ts := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	k := DayKeyFor(ts, "global", "gpt-6-sol")
	if k.Day != "2026-10-10" || k.Realm != "global" || k.Model != "gpt-6-sol" {
		t.Errorf("组合键错误：%+v", k)
	}
	if k.String() != "2026-10-10|global|gpt-6-sol|" {
		t.Errorf("组合键表示：%q", k.String())
	}
	if AccountKey("u1").String() != "u1|" || (GroupKey{}).String() != "" {
		t.Error("账号键/空键表示不符合约定")
	}
}

// 全仓守卫（S4 方案 A 后 usage.go 也走此入口）：身份串格式稳定 + **双域同名模型不得同键**。
func TestBucketKeyIsRealmAware(t *testing.T) {
	a := BucketKey("h:2026-10-09T15", "cn", "u1", "gpt-6-sol")
	b := BucketKey("h:2026-10-09T15", "global", "u1", "gpt-6-sol")
	if a != "h:2026-10-09T15|cn|u1|gpt-6-sol" {
		t.Errorf("身份串格式被改动（会破坏肉眼比对习惯）：%q", a)
	}
	if a == b {
		t.Fatal("双域同名模型必须不同键 —— 否则两域用量会被合并成一个数字且不报错")
	}
	if BucketKey("h:1", "cn", "u1", "m") == BucketKey("h:2", "cn", "u1", "m") {
		t.Error("不同小时片必须不同键")
	}
}
