package scheduler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"workbuddy2api/internal/cstime"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestGlobalAccountsSkipCheckinTravel gating（活动已放开，见下方两个 activity 用例）：
// 池内 global 账号在 checkin/travel 两类循环中**零上游调用**；activity 已不再跳过
// global（PR #45 实测 /v2/report 在 workbuddy.ai code=0 OK）。
//
// fake upstream 全路径统计调用数：global 账号若被误放行，会打到 fake server
// 的 /daily-checkin /buddy/info 等任意路径，调用数即 >0。
func TestGlobalAccountsSkipCheckinTravel(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	// 显式 realm=global（下行 Domain 兜底场景在 auth 包单测覆盖，这里直接构造 global）。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})

	// 签到/旅行循环：global 账号不应发起任何上游调用。
	s.CheckinAll()
	s.RunTravelNow()

	if n := calls.Load(); n != 0 {
		t.Errorf("global account checkin/travel upstream calls=%d want 0（checkin/travel 仍跳过）", n)
	}
}

// TestGlobalAccountSkippedListedWithStatus 门控跳过的 global 账号在 CheckinAll
// 回执里以 skipped(global) 呈现——手动触发时结果可读，不再是"未覆盖"的空白。
func TestGlobalAccountSkippedListedWithStatus(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].UID != "g1" {
		t.Fatalf("out=%+v want 1 row for g1", out)
	}
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped（global 门控回执）", out[0].Status)
	}
	if out[0].Detail != "global" {
		t.Errorf("detail=%q want global", out[0].Detail)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0", calls.Load())
	}
}

// TestGlobalAndCNMixedPoolServed 混池：activity **两类都上报**（global 不再跳过，
// PR #45 实测 workbuddy.ai /v2/report code=0 OK）；travel 仍只服务 CN（global 跳过）。
// 一次 fake upstream 同时统计两类账号真正打到的请求——区隔"放行"不是池空。
func TestGlobalAndCNMixedPoolServed(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	stub := &reportStub{} // /v2/report 统计 + X-User-Id 判定
	growth := &travelStub{buddy: "null"}
	// 合并 growth/billing 端点：activity 的 report + travel 的 buddy 都要能被两类账号打到。
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			stub.handler().ServeHTTP(w, r)
			return
		}
		growth.handler().ServeHTTP(w, r)
	}))
	defer both.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: both.Client(), ChatBaseCN: both.URL, ChatBaseGlobal: both.URL, BillingBaseCN: both.URL,
		BillingBaseGlobal: both.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()
	// CN + global 各上报一次（global 不再跳过）。
	if n := stub.calls.Load(); n != 2 {
		t.Errorf("activity report calls=%d want 2（CN + global 各 1）", n)
	}
	// activity 阶段会顺带领猫（travelAdoptForce），把 buddy/info 计数打进快照；
	// 旅行检验只数 RunTravelNow 这段的增量（global 被 travel 跳过，只有 CN 走）。
	infoBefore := growth.infoCalls.Load()

	s.RunTravelNow()
	infoDelta := growth.infoCalls.Load() - infoBefore
	if infoDelta != 1 {
		t.Errorf("CN travel buddy-info delta=%d want 1（仅 CN 账号旅行）", infoDelta)
	}
}

// TestRunActivityNowReportsGlobalAccounts 核心验收：global 账号的 report 上报**不再跳过**
// （PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK）。fake upstream 断言
// global 账号真的发出 /v2/report 请求；且按 realm 路由到 global billing base。
func TestRunActivityNowReportsGlobalAccounts(t *testing.T) {
	fastActivity(t)

	var globalCalls, cnCalls atomic.Int32
	reportOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		case "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"name":"cat"}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			globalCalls.Add(1)
		}
		reportOK.ServeHTTP(w, r)
	}))
	defer globalSrv.Close()
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			cnCalls.Add(1)
		}
		reportOK.ServeHTTP(w, r)
	}))
	defer cnSrv.Close()

	p := pool.New("")
	// 纯 global 池：若 report 仍被 IsGlobal() 跳过，globalSrv 计数保持 0。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: globalSrv.Client(), ChatBaseCN: cnSrv.URL, ChatBaseGlobal: globalSrv.URL, BillingBaseCN: cnSrv.URL,
		BillingBaseGlobal: globalSrv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()

	if n := globalCalls.Load(); n != 1 {
		t.Errorf("global account report calls=%d want 1（global 上报不再跳过）", n)
	}
	if n := cnCalls.Load(); n != 0 {
		t.Errorf("global account 不应打到 CN base: cn_calls=%d", n)
	}
}

// TestRunActivityNowGlobalReportFailWarnsButOthersProceed global report 失败（如上游
// 不认 /v2/report）→ 记 WARN 不影响其他账号：同一趟里 CN 账号照常上报。
func TestRunActivityNowGlobalReportFailWarnsButOthersProceed(t *testing.T) {
	fastActivity(t)

	var cnCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			uid := r.Header.Get("X-User-Id")
			if uid == "g1" {
				http.Error(w, "no such report endpoint on global", 404)
				return
			}
			cnCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL, BillingBaseCN: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow() // 不应 panic，g1 的 report 失败只记 WARN

	if n := cnCalls.Load(); n != 1 {
		t.Errorf("CN account report calls=%d want 1（global 失败不影响 CN）", n)
	}
}

// TestRunKeepaliveStillRefreshesGlobal keepalive 不拦 global：token refresh 端点
// 对 global 存在，刷新调用照发。
func TestRunKeepaliveStillRefreshesGlobal(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "g1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1,
		Domain: "www.workbuddy.ai"}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("global keepalive refresh calls=%d want 1（keepalive 不拦 global）", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("global token 未刷新: %s", a.AccessToken)
	}
}

// TestRunCheckinNowGating 签到开关门控：
// CheckinDisabled=true 时 RunCheckinNow 默认跳过；force=true 时放行。
func TestRunCheckinNowGating(t *testing.T) {
	var checkinCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/daily-checkin") {
			checkinCalls.Add(1)
		}
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	s := New(Config{
		Pool:            p,
		Upstream:        up,
		CheckinDisabled: true,
	})

	// CheckinDisabled 时默认跳过
	s.RunCheckinNow()
	if n := checkinCalls.Load(); n != 0 {
		t.Errorf("CheckinDisabled 时 calls=%d want 0", n)
	}

	// force=true 时放行
	s.RunCheckinNow(true)
	if n := checkinCalls.Load(); n != 1 {
		t.Errorf("RunCheckinNow(true) calls=%d want 1", n)
	}
}

// TestSchedulerReconfigureHotUpdate 热配置更新验证：
// Reconfigure 应能无缝更新 GrowthDisabled 与 GrowthHours 并通知 schedule 重算。
func TestSchedulerReconfigureHotUpdate(t *testing.T) {
	s := New(Config{
		GrowthDisabled: true,
		GrowthHours:    []int{1},
	})
	if !s.isGrowthDisabled() {
		t.Fatal("isGrowthDisabled want true")
	}

	// 热更新开启
	s.Reconfigure(
		[]int{9, 21}, []int{9, 21}, []int{10}, []int{22}, []int{1}, []int{23}, []int{2},
		false, false, false, false, false, false, false,
	)

	if s.isGrowthDisabled() {
		t.Fatal("isGrowthDisabled after reconfigure want false")
	}

	s.schedMu.Lock()
	hours := s.cfg.GrowthHours
	s.schedMu.Unlock()
	if len(hours) != 1 || hours[0] != 2 {
		t.Errorf("growthHours=%v want [2]", hours)
	}
}

// 热重载必须各自写各自的字段：旧实现对 cat/blackcat 都写同一组值，导致面板保存配置
// 会把 cat_hours / cat_enabled 静默改写成 blackcat_* 的值（夜猫子时点无声漂移）。
func TestSchedulerReconfigureKeepsCatAndBlackcatSeparate(t *testing.T) {
	s := New(Config{CatHours: []int{3}, BlackcatHours: []int{23}})

	s.Reconfigure(
		nil, nil, nil, nil, []int{3}, []int{23}, nil,
		false, false, false, false, false, false, false,
	)

	s.schedMu.Lock()
	cat, black := s.cfg.CatHours, s.cfg.BlackcatHours
	catOff, blackOff := s.cfg.CatDisabled, s.cfg.BlackcatDisabled
	s.schedMu.Unlock()
	if len(cat) != 1 || cat[0] != 3 {
		t.Errorf("catHours=%v want [3]（不得被 blackcat_hours 覆盖）", cat)
	}
	if len(black) != 1 || black[0] != 23 {
		t.Errorf("blackcatHours=%v want [23]", black)
	}
	if catOff || blackOff {
		t.Errorf("两者都应保持启用: catDisabled=%v blackcatDisabled=%v", catOff, blackOff)
	}

	// 与 New 同口径的"或"归一：只关 cat 也必须整体关（否则 dispatch 仍会排夜猫子）。
	s.Reconfigure(
		nil, nil, nil, nil, nil, nil, nil,
		false, false, false, false, true, false, false,
	)
	if !s.isCatDisabled() {
		t.Fatal("cat_enabled=false 必须整体停掉夜猫子（isCatDisabled 应 true）")
	}
}

// 补签卡幂等：**已置当日标记时一次上游请求都不该发**（消耗品，第十轮审计 F2）。
// 判别方式：假上游记请求数 —— 回退掉标记检查，请求数立刻从 0 变成 ≥1。
func TestMakeupYesterdaySkippedWhenMarkedToday(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Nickname: "n1"}
	p.Add(a)
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	sch := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}})

	// 未置标记：会去查热力图（≥1 次请求）
	sch.makeupYesterday(a)
	before := calls.Load()
	if before == 0 {
		t.Fatal("未置标记时应至少查一次热力图（否则本用例失去判别力）")
	}

	// 置当日标记后：一次都不该再发
	p.NoteDailyMark("u1", "makeup", cstime.Today())
	sch.makeupYesterday(a)
	if got := calls.Load(); got != before {
		t.Fatalf("已置当日标记后仍发了 %d 次上游请求（补签卡可能被重复消耗）", got-before)
	}
}

// 限流/风控错误要能翻译成"暂停该账号"（第十轮审计 F3）：
// 429 软限流 → 30 分钟；账号级风控/授权 → 6 小时；其它错误 → 不停（不误伤）。
func TestUpstreamAccountPenaltyClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantDur bool
	}{
		{"429 软限流", &upstream.Error{Kind: upstream.ErrSoftRate, Msg: "rate limited"}, true},
		{"账号级风控 11140", &upstream.Error{Kind: upstream.ErrAccountFault, Msg: "request illegal"}, true},
		{"业务参数错误不停手", &upstream.Error{Kind: upstream.ErrBadParams, Msg: "bad params"}, false},
		{"模型不可用不停手", &upstream.Error{Kind: upstream.ErrModelBlocked, Msg: "no model"}, false},
		{"普通错误不停手", errors.New("connection reset"), false},
	}
	for _, c := range cases {
		d, kind := upstreamAccountPenalty(c.err)
		if c.wantDur && d <= 0 {
			t.Errorf("%s: 应有惩罚期，实得 0", c.name)
		}
		if !c.wantDur && d > 0 {
			t.Errorf("%s: 不应有惩罚期（会误伤），实得 %v (%s)", c.name, d, kind)
		}
	}
	// 惩罚期生效后 penalized 为真，到期自然失效
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	sch := New(Config{Pool: p, Upstream: upstream.New()})
	if sch.penalized("u1") {
		t.Fatal("初始不应处于惩罚期")
	}
	sch.notePenalty("u1", &upstream.Error{Kind: upstream.ErrSoftRate, Msg: "429"})
	if !sch.penalized("u1") {
		t.Fatal("记惩罚后应处于惩罚期")
	}
	sch.mu.Lock()
	sch.penaltyUntil["u1"] = time.Now().Add(-time.Second) // 手工到期
	sch.mu.Unlock()
	if sch.penalized("u1") {
		t.Fatal("过期后不应再判为惩罚期")
	}
}

// 撞锁被跳过时必须**安排补跑**（第十二轮审计 F7）：TryLock 是立即返回不排队，
// 若当天两次触发都撞上长任务，rebuild 前的实现会让连登兑换/补签/抽奖整天不跑。
func TestStreakCatchUpArmedWhenLocked(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	sch := New(Config{Pool: p, Upstream: upstream.New(), CheckinHours: []int{9, 21}})

	// 模拟"已有一次在跑"：手工持锁
	sch.streakMu.Lock()
	sch.RunStreakBonusNow() // 应走 catch-up 分支而不是静默返回
	sch.retryMu.Lock()
	armed := sch.streakCatchUpArmed
	sch.retryMu.Unlock()
	sch.streakMu.Unlock()

	if !armed {
		t.Fatal("撞锁后未武装补跑：当天可能整天漏跑连登任务")
	}
	// 再撞一次不应重复武装（同一时刻只允许一个补跑者）
	sch.streakMu.Lock()
	sch.RunStreakBonusNow()
	sch.streakMu.Unlock()
	sch.retryMu.Lock()
	stillOne := sch.streakCatchUpArmed
	sch.retryMu.Unlock()
	if !stillOne {
		t.Fatal("重复武装/状态被清空")
	}
}
