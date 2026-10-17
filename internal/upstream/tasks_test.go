package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestClaimRewardWebEndpoint 领奖走 Web 域（workbuddy.cn）、任务码在路径里、无 body。
// 这是与 CLI 域（copilot.tencent.com/v2/.../reward/claim，task_code 在 body）的关键区别——
// 后者路径不存在，曾导致长期 400 "task not completed" 误判为"上游不支持领取"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	var gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（任务码必须在路径里）", gotPath, want)
	}
	if gotBody != "" {
		t.Errorf("claim 不应携带 body，got %q", gotBody)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}
	if !strings.Contains(gotReferer, "workbuddy.cn") {
		t.Errorf("Referer=%q 应指向 workbuddy.cn", gotReferer)
	}
}

// TestClaimRewardAlreadyClaimed 重复领取：上游返回 already_claimed=true，
// 本地应视为"无新增奖励但不报错"（幂等语义）。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted 未达标：上游 400 + task not completed 应作为错误透出。
func TestClaimRewardNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}

// 国际版任务列表：同一路径/信封，但**字段名不同**（code 而非 task_code、没有 credit/progress）。
// 回归点：不认别名时国际版任务码会全部解析成空串，面板只能显示标题、无法按码自动化。
func TestParseGrowthTasksGlobalSchema(t *testing.T) {
	// 真实负载（2026-10-08 探针原样截取，仅取前两条）
	raw := json.RawMessage(`{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"使用 WorkBuddy 完成至少一次对话交互","icon_url":"","level_name":"养虾尝试","status":"available","url":""},
		{"task_id":2,"code":"skill_installed","title":"完成技能安装","description":"成功安装至少一个 WorkBuddy 技能","icon_url":"","level_name":"养虾入门","status":"available","url":"workbuddy://skills"}]}`)
	tasks, err := parseGrowthTasks(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("任务数=%d，期望 2", len(tasks))
	}
	if tasks[0].TaskCode != "first_chat" || tasks[1].TaskCode != "skill_installed" {
		t.Errorf("国际版任务码未取到：%q / %q", tasks[0].TaskCode, tasks[1].TaskCode)
	}
	if tasks[0].Title != "完成一次对话" {
		t.Errorf("标题解析异常：%q", tasks[0].Title)
	}
	if tasks[1].JumpURL != "workbuddy://skills" {
		t.Errorf("url 别名未取到：%q", tasks[1].JumpURL)
	}
	if tasks[0].Status != "available" {
		t.Errorf("status 解析异常：%q", tasks[0].Status)
	}
	// 默认（CN）口径不能被别名破坏
	cnRaw := json.RawMessage(`{"tasks":[{"task_code":"chat_5","title":"和 AI 聊天 5 次","reward_credit":100,"reward_energy":5,"current":5,"target":5,"accept_status":"accepted"}]}`)
	cn, err := parseGrowthTasks(cnRaw)
	if err != nil || len(cn) != 1 {
		t.Fatalf("CN 负载解析失败: %v / %d", err, len(cn))
	}
	if cn[0].TaskCode != "chat_5" || cn[0].Credit != 100 || !cn[0].Claimable || cn[0].Claimed {
		t.Errorf("CN 口径解析被破坏：%+v", cn[0])
	}
}
