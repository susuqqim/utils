package upstream

import "testing"

// error 帧分类（hint.go 早已实现，本用例把它钉住）：必须与"收到 4xx 响应体"同一套判据，
// 否则流尾的账号处置（applyErrorPolicy）与传输层会各判一套、互相漂移。
func TestFrameKind(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    ErrKind
	}{
		{"6004 模型级限流", `{"error":{"code":6004,"message":"model rate limit, will reset at 12:00"}}`, ErrSoftRate},
		{"11102 模型不存在", `{"error":{"code":11102,"message":"model [x] service info not found"}}`, ErrModelBlocked},
		{"余额不足", `{"error":{"code":402,"message":"insufficient credits"}}`, ErrHardCredit},
		// 既有实现（hint.go）对"解析不出 error.message"返回 ErrNone（不是 ErrClient）：
		// 语义是"这条帧给不出分类"，交给调用方按中性处理。
		{"无码无文案命中", `{"error":{"message":"something odd"}}`, ErrClient},
		{"空", ``, ErrNone},
		{"纯文本帧", `upstream exploded`, ErrNone},
	}
	for _, c := range cases {
		if got := FrameKind(c.payload); got != c.want {
			t.Errorf("%s: FrameKind=%v want %v", c.name, got, c.want)
		}
	}
}
