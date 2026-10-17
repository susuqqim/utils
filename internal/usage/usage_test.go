package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// 积分用量累计测试：验证单次请求 credit 正确累加到 totals、by_account 与 series 中。
func TestCreditsTracking(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 50, HasPromptTokens: true, Credit: 2.5, HasCredit: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Credits != 4.0 {
		t.Fatalf("totals.credits = %v, want 4.0", s.Totals.Credits)
	}
	if len(s.Series) != 1 || s.Series[0].Credits != 4.0 {
		t.Fatalf("series[0].credits = %v, want 4.0", s.Series[0].Credits)
	}
	if len(s.ByAccount) != 1 || s.ByAccount[0].Credits != 4.0 {
		t.Fatalf("by_account[0].credits = %v, want 4.0", s.ByAccount[0].Credits)
	}
}

// 缓存命中 token 的累计与聚合（totals + by_account）；缺失 HasCached 的尝试不累加。
func TestCachedTokensAggregate(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.3", Delta{PromptTokens: 1000, HasPromptTokens: true, CompletionTokens: 20, HasCompletion: true, CachedTokens: 800, HasCached: true}, true)
	r.Add(now, "cn", "uid1", "glm-5.3", Delta{PromptTokens: 500, HasPromptTokens: true, CachedTokens: 200, HasCached: true}, true)
	r.Add(now, "cn", "uid2", "glm-5.3", Delta{PromptTokens: 300, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.CachedTokens != 1000 {
		t.Fatalf("totals cached = %d, want 1000", s.Totals.CachedTokens)
	}
	var got int64
	for _, a := range s.ByAccount {
		if a.Key == "uid1" {
			got = a.CachedTokens
		}
	}
	if got != 1000 {
		t.Fatalf("by_account uid1 cached = %d, want 1000", got)
	}
}

// 客户端取消（499）计入请求数但不计失败：账号无过错，不应拉低成功率。
func TestCanceledAttemptNotCountedAsError(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.3", Delta{PromptTokens: 100, HasPromptTokens: true}, true)
	r.Add(now, "cn", "uid1", "glm-5.3", Delta{Canceled: true}, false)
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 2 {
		t.Fatalf("requests=%d want 2", s.Totals.Requests)
	}
	if s.Totals.Errors != 0 {
		t.Fatalf("errors=%d want 0（499 不计失败）", s.Totals.Errors)
	}
}

// 折叠到**同一个日桶**的多个小时桶，缓存命中量必须累加（回归：else 分支曾漏加 CC）。
// 只测"同一天第二个及以后的小时桶"这条路径——第一个小时桶走结构体拷贝，掩盖了缺陷。
func TestRollupSumsCachedTokensIntoExistingDayBucket(t *testing.T) {
	r := New("")
	// 同一天的两个不同小时，都在 90 天保留窗口之外 → 折叠进同一个 d: 桶。
	// 显式取"100 天前的 12:00 与 13:00"：直接 Truncate(time.Hour) 再 +1h 会在本地
	// 23:00 时跨到次日（每天 23:00–23:59 必红的时间炸弹，审查已证实）。
	day := time.Now().AddDate(0, 0, -100)
	h1 := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, day.Location())
	h2 := h1.Add(time.Hour)
	if h1.Format(dayLayout) != h2.Format(dayLayout) {
		t.Fatalf("测试前提不成立：%s 与 %s 不在同一天", h1, h2)
	}
	r.Add(h1, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, CachedTokens: 30, HasCached: true}, true)
	r.Add(h2, "cn", "u", "m", Delta{PromptTokens: 200, HasPromptTokens: true, CachedTokens: 70, HasCached: true}, true)

	r.Rollup(time.Now())

	all := r.Snapshot(0, nil)
	if all.Totals.CachedTokens != 100 {
		t.Fatalf("折叠后 cached = %d, want 100（同一天多个小时桶须累加）", all.Totals.CachedTokens)
	}
	if all.Totals.PromptTokens != 300 {
		t.Fatalf("折叠后 prompt = %d, want 300", all.Totals.PromptTokens)
	}
	// 折叠必须真的合并成一个日桶（否则本用例没覆盖到 else 分支）。
	var dayBuckets int
	for _, b := range r.buckets {
		if b.Scope == "d:"+h1.Format(dayLayout) {
			dayBuckets++
		}
	}
	if dayBuckets != 1 {
		t.Fatalf("日桶数 = %d, want 1", dayBuckets)
	}
}

// 落盘失败必须**回置 dirty**，让下一轮 flush 重试，而不是静默丢掉这一轮增量。
func TestFlushFailureKeepsDirtyForRetry(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备占位文件失败: %v", err)
	}
	// 父路径是普通文件 → MkdirAll/WriteFile 必然失败（跨平台稳定）。
	r := New(filepath.Join(blocker, "usage.json"))
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)

	r.Save() // 强制落盘一次（必然失败）

	if !r.dirty {
		t.Fatalf("落盘失败后 dirty = false：这一轮增量会在下次成功落盘前丢失")
	}
	if r.flushFails != 1 {
		t.Fatalf("flushFails = %d, want 1", r.flushFails)
	}
	// 已存在的数据不能因为落盘失败而在内存里消失。
	if s := r.Snapshot(0, nil); s.Totals.PromptTokens != 5 {
		t.Fatalf("内存数据丢失: prompt = %d, want 5", s.Totals.PromptTokens)
	}

	// 端到端证明"重试"：把路径改成可写后，下一轮 flush 必须把保住的增量真的写下去。
	good := filepath.Join(dir, "usage.json")
	r.path = good // 包内私有字段：模拟运维修好目录/权限
	r.Save()
	if r.dirty {
		t.Fatal("成功落盘后 dirty 应为 false")
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("重试未落盘: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Buckets) != 1 {
		t.Fatalf("重试落盘内容异常: err=%v buckets=%d", err, len(f.Buckets))
	}
	if r.flushFails != 0 {
		t.Fatalf("成功后应清零 flushFails, got %d", r.flushFails)
	}
}

// flush 并发安全（single-flight）：定时器与面板「刷新」会同时调 flush，两者共用
// <path>.tmp —— 交错会让 rename 搬走半截文件，load() 解析失败即"从零开始"覆盖历史。
// 本用例并发压同一 Recorder，断言每次落盘产物始终是**完整可解析**的 JSON。
func TestConcurrentFlushKeepsFileParseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
				r.Save() // 强制落盘（force=true）
			}
		}(i)
	}
	wg.Wait()
	r.Save()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘文件不是完整 JSON（并发 flush 交错）: %v", err)
	}
	if len(f.Buckets) != 1 {
		t.Fatalf("桶数 = %d, want 1", len(f.Buckets))
	}
	// 内存与落盘总量必须一致（没有半截快照把数据写没）。
	if s := r.Snapshot(0, nil); s.Totals.PromptTokens != f.Buckets[0].PT {
		t.Fatalf("内存 %d != 落盘 %d（丢更新）", s.Totals.PromptTokens, f.Buckets[0].PT)
	}
}

// Start() 必须幂等：重复调用会二次 close(r.done) → panic（goroutine 内 panic = 进程退出）。
func TestStartIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Start() // 第二次必须是无害的 no-op
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	r.Stop() // 能正常停下即说明 done 没有被提前/重复关闭
	r.Stop() // Stop 本身也应幂等
}

// 桶键必须按 **CST** 口径（产品"自然日"= 中国区自然日），而不是宿主机本地时区：
// Docker 默认 UTC 时，用本地时区会让同一个"今天"在面板上被劈成两天（差 8 小时）。
func TestBucketsUseCSTRegardlessOfHostZone(t *testing.T) {
	// 用 UTC 时间构造 23:30 —— CST 视角已是次日 07:30。
	utcLate := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	r := New("")
	r.Add(utcLate, "cn", "u1", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.mu.Lock()
	var scope string
	for _, b := range r.buckets {
		scope = b.Scope
	}
	r.mu.Unlock()
	if scope != "h:2026-03-11T07" {
		t.Fatalf("桶键 %q，want h:2026-03-11T07（CST 口径）", scope)
	}
	// 折叠也必须落在 CST 日界上。
	r.Rollup(time.Now())
	found := false
	r.mu.Lock()
	for _, b := range r.buckets {
		if b.Scope == "d:2026-03-11" {
			found = true
		}
	}
	r.mu.Unlock()
	if !found {
		t.Fatal("折叠后的日桶不是 CST 日界（d:2026-03-11）")
	}
}

// 回归守卫（§T.34.13）：`hours=0` 必须是**全部历史**，不得退化成默认窗口 ——
// 这正是上游 dc19443 修掉的缺陷形态（把 0 当假值 ⇒ 数字静默变小）。
// 构造"窗口外旧桶 + 窗口内新桶"，分别断言 Snapshot(0) 计全部、Snapshot(24) 只计窗口内。
func TestSnapshotAllHistoryNotDegraded(t *testing.T) {
	r := New("")
	now := time.Now()
	d := func(pt int64) Delta {
		return Delta{PromptTokens: pt, HasPromptTokens: true, TotalTokens: pt, HasTotal: true}
	}
	r.Add(now.Add(-72*time.Hour), "cn", "u1", "m", d(100), true) // 窗口外
	r.Add(now.Add(-1*time.Hour), "cn", "u1", "m", d(7), true)    // 窗口内

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 {
		t.Errorf("hours=0 必须计全部历史：requests=%d want 2（退化成默认窗口即上游 dc19443 的缺陷）", all.Totals.Requests)
	}
	if all.Totals.PromptTokens != 107 {
		t.Errorf("hours=0 应计入窗口外 token：prompt_tokens=%d want 107", all.Totals.PromptTokens)
	}
	w := r.Snapshot(24, nil)
	if w.Totals.Requests != 1 || w.Totals.PromptTokens != 7 {
		t.Errorf("hours=24 只应计窗口内：requests=%d prompt_tokens=%d want 1/7", w.Totals.Requests, w.Totals.PromptTokens)
	}
}
