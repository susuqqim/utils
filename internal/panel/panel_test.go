package panel

import (
	"os"
	"regexp"
	"testing"
)

// TestPanelRouteInventory 路由清单守卫（第九轮审计 F1）。
//
// 背景：keyless 模式下"改状态的请求必须带自定义头"这条 CSRF 防护依赖
// `server.isPanelStateChanging` 里的**手工白名单** —— 将来新增一条"会改状态的 GET"
// 会静默绕过它（`login/poll` 正是先例：GET 但会写 auths + 入池 + 触发签到/试用）。
//
// 本用例把**当前路由清单冻结**：新增/删除任何路由都会让它变红，从而强制改动者显式复核
// "这条路由会不会改状态、要不要加进白名单"，并同步更新本清单。
func TestPanelRouteInventory(t *testing.T) {
	raw, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatalf("读 panel.go: %v", err)
	}
	re := regexp.MustCompile(`p\.mux\.HandleFunc\("([A-Z]+) ([^"]*)"`)
	got := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		got[m[1]+" "+m[2]] = true
	}
	want := []string{
		"GET /panel/{$}", "GET /panel/app.js",
		"GET /panel/api/overview", "GET /panel/api/logs", "GET /panel/api/models",
		"GET /panel/api/usage", "GET /panel/api/packages", "GET /panel/api/config",
		"GET /panel/api/model_probes", "GET /panel/api/login/regions",
		"GET /panel/api/login/poll", // ← 唯一改状态的 GET：必须留在 server.isPanelStateChanging
		"GET /panel/api/tasks/queue", "GET /panel/api/school/vouchers",
		"GET /panel/api/accounts/{uid}/tasks",
		"POST /panel/api/login/start", "POST /panel/api/import/cockpit",
		"POST /panel/api/accounts/{uid}/revive", "POST /panel/api/accounts/{uid}/disable",
		"POST /panel/api/accounts/{uid}/pause",
		"POST /panel/api/accounts/{uid}/resume",
		"POST /panel/api/accounts/{uid}/pin", "POST /panel/api/accounts/{uid}/unpin",
		"POST /panel/api/accounts/unpin_all", "POST /panel/api/accounts/{uid}/checkin",
		"POST /panel/api/accounts/{uid}/balance", "POST /panel/api/accounts/{uid}/remove",
		"POST /panel/api/accounts/{uid}/tasks/accept", "POST /panel/api/accounts/{uid}/tasks/accept_all",
		"POST /panel/api/accounts/{uid}/tasks/claim", "POST /panel/api/accounts/{uid}/tasks/auto",
		"POST /panel/api/accounts/{uid}/tasks/auto_all",
		"POST /panel/api/tasks/scan_all", "POST /panel/api/tasks/run_queue",
		"POST /panel/api/checkin_all",
		"GET /panel/api/model-locks",
		"GET /panel/api/reqlog", "POST /panel/api/travel_all",
		"POST /panel/api/activity_all", "POST /panel/api/balance_all",
		"POST /panel/api/keepalive_all", "POST /panel/api/usage/save",
		"POST /panel/api/config",
		"POST /panel/api/system/restart", "POST /panel/api/system/shutdown",
	}
	seen := map[string]bool{}
	for _, w := range want {
		if !got[w] {
			t.Errorf("路由清单少了 %q（删除路由请同步更新 want；若是有意删除，也请复核 isPanelStateChanging）", w)
		}
		seen[w] = true
	}
	for g := range got {
		if !seen[g] {
			t.Errorf("新增路由 %q：请复核它是否**会改状态**——若是且方法为 GET，"+
				"必须加进 server.isPanelStateChanging（keyless CSRF 白名单），然后把本用例的 want 补上", g)
		}
	}
}
