package upstream

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestTasksProbeLive 只读探针：拉取**上游实时任务清单**（默认口径 + 小程序口径），
// 与「我们已知的任务码」求差集，用来发现腾讯新上的任务。
//
// 只调 ListTasks / ListTasksMP（GET），**不做 accept/claim**（不产生副作用/不领奖）。
// 手动运行：
//
//	WB2A_TASKS_PROBE=1 [WB2A_AUTH_DIR=<auths 目录>] [WB2A_KNOWN_CODES=<每行一个码的文件>] \
//	  go test ./internal/upstream/ -run TestTasksProbeLive -v
func TestTasksProbeLive(t *testing.T) {
	if os.Getenv("WB2A_TASKS_PROBE") == "" {
		t.Skip("set WB2A_TASKS_PROBE=1 to run")
	}
	dir := os.Getenv("WB2A_AUTH_DIR")
	if dir == "" {
		dir = "/mnt/c/Users/GG/.workbuddy2api/auths"
	}
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	c := &Client{
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		ChatHTTP:      &http.Client{Timeout: 60 * time.Second},
		GlobalEnabled: true,
	}
	c.ChatBaseCN = "https://copilot.tencent.com"
	c.BillingBaseCN = "https://copilot.tencent.com"
	var cn, gl *auth.Auth
	for _, a := range auths {
		r := strings.ToLower(a.Realm())
		if r == "" {
			r = "cn"
		}
		if r == "cn" && cn == nil {
			cn = a
		}
		if r == "global" && gl == nil {
			gl = a
		}
	}
	known := map[string]bool{}
	if fp := os.Getenv("WB2A_KNOWN_CODES"); fp != "" {
		if raw, err := os.ReadFile(fp); err == nil {
			for _, l := range strings.Split(string(raw), "\n") {
				l = strings.TrimSpace(l)
				if l != "" {
					known[l] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	report := func(realm, kind string, a *auth.Auth, list func(*auth.Auth) ([]Task, error)) {
		if a == nil {
			fmt.Printf("── %s/%s：无该 realm 账号\n", realm, kind)
			return
		}
		tasks, err := list(a)
		if err != nil {
			fmt.Printf("── %s/%s：拉取失败: %v\n", realm, kind, err)
			return
		}
		fmt.Printf("── %s/%s：上游下发 %d 个任务\n", realm, kind, len(tasks))
		codes := make([]string, 0, len(tasks))
		for _, tk := range tasks {
			codes = append(codes, tk.TaskCode)
			seen[tk.TaskCode] = true
		}
		sort.Strings(codes)
		for _, tk := range tasks {
			mark := "  "
			if len(known) > 0 && !known[tk.TaskCode] {
				mark = "🆕"
			}
			fmt.Printf("   %s %-34s 奖励=%d/%d 进度=%d/%d type=%-8s status=%-9s %s\n",
				mark, tk.TaskCode, tk.Credit, tk.Energy, tk.Current, tk.Target,
				tk.TaskType, tk.Status, truncateStr(tk.Title, 30))
		}
	}
	report("cn", "默认口径", cn, c.ListTasks)
	report("cn", "小程序口径", cn, c.ListTasksMP)
	report("global", "默认口径", gl, c.ListTasks)
	// 国际版任务码为空 ⇒ 抓原始 JSON 看真实字段名（只读）
	if gl != nil && os.Getenv("WB2A_TASKS_RAW") != "" {
		raw, err := c.growthJSON(gl, http.MethodGet, tasksListPath, nil)
		if err != nil {
			fmt.Printf("── global 原始拉取失败: %v\n", err)
		} else {
			fmt.Printf("\n=== global 任务列表原始 JSON（前 1200 字符）===\n%s\n", truncateStr(string(raw), 1200))
		}
	}
	if len(known) > 0 {
		var novel []string
		for code := range seen {
			if !known[code] {
				novel = append(novel, code)
			}
		}
		sort.Strings(novel)
		fmt.Printf("\n=== 与已知 %d 个任务码求差集 ===\n", len(known))
		if len(novel) == 0 {
			fmt.Println("  ✅ 上游没有我们未覆盖的任务码")
		} else {
			fmt.Printf("  🆕 上游新出现(或我们未登记) %d 个：%v\n", len(novel), novel)
		}
	}
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
