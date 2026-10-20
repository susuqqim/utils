package panel

import (
	"os"
	"strings"
	"testing"
)

// 守卫（2026-10）：配置页曾出现"前端写了后端不认识的字段 ⇒ 整页保存失败"的历史缺陷
// （credit_floor 被误入 JS 时长字段）。本用例锁定「自适应三档分块」的 10 个字段必须
// 出现在配置页，且用 input name=，不允许只留一个开关。
func TestConfigPageExposesAdaptiveTierFields(t *testing.T) {
	b, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html: %v", err)
	}
	html := string(b)
	fields := []string{
		"tier_cheap_quota", "tier_cheap_max_reqs", "tier_cheap_cost_cutoff",
		"tier_normal_quota", "tier_normal_max_reqs",
		"tier_flagship_cost_cutoff", "tier_flagship_quota", "tier_flagship_max_reqs",
		"tier_unknown_quota", "tier_unknown_max_reqs",
	}
	for _, f := range fields {
		if !strings.Contains(html, `name="`+f+`"`) {
			t.Errorf("配置页缺少三档字段 %q（应作为 input name 出现）", f)
		}
	}
	if !strings.Contains(html, `name="adaptive_chunking"`) {
		t.Error("配置页缺少 adaptive_chunking 开关")
	}
	if !strings.Contains(html, "三档分块参数") {
		t.Error("配置页缺少「三档分块参数」说明区块")
	}
}
