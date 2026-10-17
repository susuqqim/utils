package server

import "testing"

// 隐藏模型（网关侧过滤）的纯函数契约：
//
//	① 支持 bare 与 cn:/global: 前缀**混写**（配置里写哪个都能匹配到两种目录形态）；
//	② 空配置 = 完全不改变行为（零回归）；
//	③ 过滤只剔除命中项、保持顺序。
func TestHiddenModelsHelpers(t *testing.T) {
	set := hiddenSet([]string{" glm-4.7 ", "cn:glm-4.6", "global:kimi-k2-thinking", ""})
	h := &Handler{hidden: set}
	cases := map[string]bool{
		"glm-4.7": true, "cn:glm-4.7": true, "global:glm-4.7": true, // bare 配置 ⇒ 两种前缀都命中
		"glm-4.6": true, "cn:glm-4.6": true, // 带前缀配置 ⇒ bare 形态也命中
		"kimi-k2-thinking": true, "global:kimi-k2-thinking": true,
		"glm-5.2": false, "cn:glm-5.2": false, "": false,
	}
	for m, want := range cases {
		if got := h.isHidden(m); got != want {
			t.Errorf("isHidden(%q)=%v want %v", m, got, want)
		}
	}
	// 空配置（默认）⇒ 一律不隐藏
	empty := &Handler{hidden: hiddenSet(nil)}
	if empty.isHidden("glm-4.7") {
		t.Error("未配置 hidden_models 时不得隐藏任何模型（零回归）")
	}
	// 过滤：保持顺序、只剔命中项
	items := []map[string]any{{"id": "glm-5.2"}, {"id": "glm-4.7"}, {"id": "cn:glm-4.6"}, {"id": "deepseek-v4.1-flash"}}
	got := filterHidden(items, func(m map[string]any) string { id, _ := m["id"].(string); return id }, set)
	if len(got) != 2 || got[0]["id"] != "glm-5.2" || got[1]["id"] != "deepseek-v4.1-flash" {
		t.Errorf("过滤结果错误: %+v", got)
	}
	if same := filterHidden(items, func(m map[string]any) string { id, _ := m["id"].(string); return id }, nil); len(same) != len(items) {
		t.Error("未配置时必须原样返回（含 nil 集合路径）")
	}
}
