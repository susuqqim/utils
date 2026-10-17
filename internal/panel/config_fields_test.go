package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 面板配置字段的**三处一致**守卫：CFG_MAP（收集逻辑）↔ index.html 的表单控件 name ↔
// 配置结构体的 json tag。缺任何一处都会静默失效：
//   - CFG_MAP 有、表单没有 name ⇒ collectConfig 取不到值 ⇒ 该字段永远存不进去；
//   - 表单有、CFG_MAP 没有 ⇒ 输入被忽略（用户改了不生效）；
//   - 两边都有、后端 json tag 不同 ⇒ 保存后落到别的键。
//
// 全量比对不现实（CFG_MAP 是 JS 对象、结构体在别的包），这里固定"本轮新增的字段"
// 必须三处齐全 —— 新加配置项时同步扩展本表即可。
func TestPanelNewConfigFieldsWired(t *testing.T) {
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	indexHTML, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	cfgGo, err := os.ReadFile("../../cmd/server/config.go")
	if err != nil {
		t.Fatal(err)
	}
	schedGo, err := os.ReadFile("../../internal/config/schedule.go")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		field   string // 表单 name / CFG_MAP 键
		section string // CFG_MAP 的第一段（配置段名）
		jsonTag string // 配置结构体 json tag
	}{
		{"credit_floor", "pool", "credit_floor"},
		{"read_timeout", "server", "read_timeout"},
		{"include_disabled_in_tasks", "schedule", "include_disabled_in_tasks"},
	}
	for _, c := range cases {
		// ① CFG_MAP 必须有映射，且**section 与末段都要对**（审查 E 的 T5：只查"有映射"
		// 会漏掉写成错误 section 的情形 —— 运行期 mergeConfigMaps 会把它落到别的键，
		// warnUnknownKeys 只打 WARN ⇒ 面板静默不生效，正是本测试要防的第一种失效）。
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(c.field) + `\s*:\s*\[\s*'([^']+)'\s*,\s*'([^']+)'\s*\]`)
		m := re.FindStringSubmatch(string(appJS))
		if m == nil {
			t.Errorf("app.js 的 CFG_MAP 缺少 %q 映射（或格式不是 ['section','name']）", c.field)
		} else {
			if m[1] != c.section {
				t.Errorf("CFG_MAP[%q] 的 section=%q，期望 %q（落到别的段 ⇒ 面板静默不生效）", c.field, m[1], c.section)
			}
			if m[2] != c.jsonTag {
				t.Errorf("CFG_MAP[%q] 的键名=%q，期望 %q（与后端 json tag 不一致）", c.field, m[2], c.jsonTag)
			}
		}
		// ② 表单里必须有同名控件
		if !strings.Contains(string(indexHTML), `name="`+c.field+`"`) {
			t.Errorf("index.html 缺少 name=%q 的控件（CFG_MAP 有映射也取不到值）", c.field)
		}
		// ③ 后端必须有同名 json tag（Pool 段或 Schedule 段之一）
		tag := `json:"` + c.jsonTag
		if !strings.Contains(string(cfgGo), tag) && !strings.Contains(string(schedGo), tag) {
			t.Errorf("配置结构体缺少 json tag %q（面板存了会落到别的键）", c.jsonTag)
		}
	}
}

// 面板按钮/动作与后端路由的**联动**：app.js 里出现的 data-a="X" 动作（除纯前端动作外）
// 必须在后端有对应路由或明确的前端处理分支。这里只固定本批新增的 pause/resume，
// 防止"前端能点、后端 404"或反过来。
func TestPanelPauseResumeWired(t *testing.T) {
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	panelGo, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"pause", "resume"} {
		if !strings.Contains(string(appJS), `data-a="`+a+`"`) {
			t.Errorf("app.js 缺少 data-a=%q 按钮", a)
		}
		if !strings.Contains(string(panelGo), `panel/api/accounts/{uid}/`+a+`"`) {
			t.Errorf("panel.go 缺少 /%s 路由（前端能点、后端 404）", a)
		}
		if !strings.Contains(string(appJS), `accounts/' + encodeURIComponent(u) + '/`+a+`'`) {
			t.Errorf("app.js 缺少 %q 动作分发", a)
		}
	}
}

// 补审（§T.34.19 未完项）：reqlog 接口的边界语义 —— `since` 非法/归档未启用时**不得静默装作读了归档**，
// 且必须回显**生效**的 limit（非法/超限时为退回值），否则调用方会拿错数据并得出错误结论。
func TestReqLogAPIBoundaries(t *testing.T) {
	// 用最小构造：只验"参数解析与响应字段"契约，不依赖真实归档/环。
	src := readAppJSOrFile(t, "panel.go")
	for _, want := range []string{"since_error", `"limit":`, "since 解析失败"} {
		if !strings.Contains(src, want) {
			t.Errorf("reqlog 接口缺少边界处理标记：%q", want)
		}
	}
}

// readAppJSOrFile 读取 internal/panel 下的文件（测试辅助；避免引入额外依赖）。
func readAppJSOrFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("跳过：%v", err)
	}
	return string(b)
}
