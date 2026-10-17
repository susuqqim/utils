package main

import (
	"os"
	"strings"
	"testing"
)

// 装配一致性守卫（审查 E 的 P1-3）：cmd/activity 是文档里"部署后验证活跃上报闭环"的工具，
// 与 cmd/server 必须共用同一份 schedule 语义（issue #49 的立项理由）。漏装配
// IncludeDisabledInTasks 会让"开了开关、守护进程覆盖禁用号、而该工具仍跳过"，
// 用户按文档验证新特性会得到"没生效"的假结论。
func TestActivityWiresIncludeDisabledInTasks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "IncludeDisabledInTasks:") {
		t.Error("cmd/activity 的 scheduler.Config 未装配 IncludeDisabledInTasks（与 cmd/server 语义分叉）")
	}
}
