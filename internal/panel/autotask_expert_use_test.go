package panel

import "testing"

// 2026-11 新一批「体验某个专家」任务必须登记进自动化白名单，且标为尝试型。
// 这些任务只在 jump_url 里给 expertId，没有现成的按名匹配逻辑，漏登记会让面板显示
// “该任务需要客户端内交互”，用户只能手动点。
func TestExpertUseTasksAreAutoActions(t *testing.T) {
	cases := []struct {
		code     string
		expertID string
	}{
		{"expert_use_maidehao", "ex_GDl31WahyIH0"},
		{"expert_use_douyinyin", "ex_FbY5nlWTqDod"},
		{"expert_use_shangjia_xuanchuan", "ex_Weo3x2EZuSDm"},
		{"expert_use_bowangwang", "ex_WZY136wV5aMt"},
	}
	for _, c := range cases {
		a := autoActionFor(c.code)
		if a == nil {
			t.Errorf("%s 未登记：面板会显示需要客户端交互", c.code)
			continue
		}
		if a.run == nil {
			t.Errorf("%s 的 run 为空", c.code)
		}
		if !a.Attempt {
			t.Errorf("%s 未点亮验证前应标记 Attempt（尝试型），否则面板会当成稳过", c.code)
		}
		if a.Desc == "" {
			t.Errorf("%s 缺少展示说明", c.code)
		}
	}
}

// 每个 autoAction 都必须有 code/desc/run，避免半截登记（漏 run 会在执行时 panic）。
func TestAllAutoActionsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range autoActions {
		if a.TaskCode == "" || a.Desc == "" || a.run == nil {
			t.Errorf("autoAction 不完整: %+v", a)
		}
		if seen[a.TaskCode] {
			t.Errorf("重复登记: %s", a.TaskCode)
		}
		seen[a.TaskCode] = true
	}
}
