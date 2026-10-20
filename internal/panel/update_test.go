package panel

import "testing"

// 面板徽标是否显示"可更新"完全依赖它；必须按数字比较，不能按字符串。
func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.11.9", "1.11.8", true},
		{"1.11.8", "1.11.9", false},
		{"1.11.9", "1.11.9", false},
		{"1.12.0", "1.11.99", true},
		{"2.0.0", "1.99.99", true},
		{"1.11.10", "1.11.9", true},
		{"1.11.9", "1.11.9-panel", false}, // 后缀不属于数值版本（QA 实测会误报）
		{"1.11.10", "1.11.9-panel", true},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
