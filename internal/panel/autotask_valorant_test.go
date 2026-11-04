package panel

import "testing"

// WorkBuddy_Valorant：专家没进公开检索（keyword 通道需客户端 searchHeaders），
// 只能靠翻市场列表定位。ID 一旦失效，这条用例至少保证登记还在、Attempt 标记还在，
// 让人知道它属于“尝试型”而不是稳定可点。
func TestValorantTaskRegistered(t *testing.T) {
	a := autoActionFor("WorkBuddy_Valorant")
	if a == nil {
		t.Fatal("WorkBuddy_Valorant 未登记：面板会显示需要客户端交互")
	}
	if !a.Attempt {
		t.Error("应标记 Attempt（依赖市场检索到的 expertId，判据未长期验证）")
	}
	if a.run == nil || a.Desc == "" {
		t.Error("run/Desc 不能为空")
	}
}
