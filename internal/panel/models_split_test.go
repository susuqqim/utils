package panel

import (
	"os"
	"strings"
	"testing"
)

// 守卫（2026-10）：模型页必须按 realm 分成「国内版 / 国际版」两块。
// 做法是表内分组标题行（不动列结构），因此这里断言 app.js 里存在分组渲染与两个标题。
func TestModelsPageSplitsByRealm(t *testing.T) {
	b, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"__modelsSplitByRealm",
		"cnModels", "glModels",
		"startsWith('global:')",
		"国内版", "国际版",
		"sectRow(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js 缺少模型页分区要素: %q", want)
		}
	}
	// 分组后仍必须把两组都渲染出来（防止只渲染一组）
	if strings.Count(src, "map(rowOf)") < 2 {
		t.Error("app.js 应分别渲染 cnModels 与 glModels 两组（map(rowOf) 至少 2 处）")
	}
}
