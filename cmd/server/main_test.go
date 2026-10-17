package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// 日志镜像的容错：控制台 writer 失败（托盘 FreeConsole 之后 stderr/stdout 已失效）时，
// 不能让 io.MultiWriter 短路掉它后面的面板日志环 —— 否则面板从此收不到任何日志
// （2026-10-05 自查发现的真实回归：环里只剩启动时那一条）。
func TestIgnoreWriteErrDoesNotShortCircuit(t *testing.T) {
	var ring strings.Builder
	mw := io.MultiWriter(&ring, ignoreWriteErr(failingWriter{}))
	n, err := mw.Write([]byte("hello\n"))
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if n != 6 {
		t.Errorf("应报告全部写入，得到 %d", n)
	}
	if ring.String() != "hello\n" {
		t.Errorf("环应拿到完整内容，实得 %q", ring.String())
	}
	// 反向对照：不包裹时 MultiWriter 会因第一个 writer 失败而短路（这正是回归的机制）
	var ring2 strings.Builder
	mw2 := io.MultiWriter(failingWriter{}, &ring2)
	if _, err := mw2.Write([]byte("x")); err == nil {
		t.Error("未包裹的失败 writer 应让 MultiWriter 返回错误（机制前提）")
	}
	if ring2.Len() != 0 {
		t.Error("短路时后续 writer 拿不到内容（机制前提）")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("console gone") }

// 面板热开保底（credit_floor 0 → 正值）必须补一次倍率预热，否则"倍率表为空"的空窗期
// 重现（此窗口内触底号打收费模型照旧归零）。审查 E 的 P1-1：早期版本先 Set 后判
// `p.CreditFloor() <= 0 && newCfg… > 0`，条件恒假 = 死守卫；抽成纯函数后本用例把它钉死。
func TestShouldWarmOnHotEnable(t *testing.T) {
	cases := []struct {
		old, new int64
		want     bool
	}{
		{0, 150, true},    // 热开：需要补预热
		{0, 1, true},      // 同上（边界）
		{150, 150, false}, // 已是开启状态改数值：无需重复预热
		{150, 0, false},   // 关闭保底：不需要
		{0, 0, false},     // 保持关闭
		{-5, 150, true},   // 负值会被 config 钳 0，这里按"<=0"处理
	}
	for _, c := range cases {
		if got := shouldWarmOnHotEnable(c.old, c.new); got != c.want {
			t.Errorf("shouldWarmOnHotEnable(%d, %d)=%v want %v（恒假即死守卫）", c.old, c.new, got, c.want)
		}
	}
}
