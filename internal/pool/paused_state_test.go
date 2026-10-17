package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 暂停选号（上游 fd835df）：退出选号候选、但保号任务照常；与 disabled 正交。
func newPausedPool(t *testing.T, uids ...string) *Pool {
	t.Helper()
	p := New(filepath.Join(t.TempDir(), "state.json"))
	for _, u := range uids {
		p.Add(&auth.Auth{UID: u, AccessToken: "at-" + u, ExpiresAt: 9999999999})
	}
	return p
}

func TestPauseExitsRouting(t *testing.T) {
	p := newPausedPool(t, "u1")
	if a := p.PickExcluding(nil); a == nil {
		t.Fatal("暂停前应能选出账号")
	}
	if !p.Pause("u1") {
		t.Fatal("Pause 应成功")
	}
	if a := p.PickExcluding(nil); a != nil {
		t.Errorf("暂停后不该再被选中，实得 %s", a.UID)
	}
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Error("Status.Paused 应为 true")
	}
	if st.Disabled {
		t.Error("暂停不是禁用：Disabled 应保持 false")
	}
	// healthz 口径：暂停号不参与服务
	if p.ServableNow() {
		t.Error("暂停号不该参与服务（ServableNow 应为 false）")
	}
}

func TestResumeRestoresRouting(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Pause("u1")
	if !p.Resume("u1") {
		t.Fatal("Resume 应成功")
	}
	if a := p.PickExcluding(nil); a == nil {
		t.Error("恢复后应能选出账号")
	}
	st, _ := p.Status("u1")
	if st.Paused {
		t.Error("恢复后 Paused 应为 false")
	}
}

// 暂停号必须同样被"全冷却兜底"路径排除（否则会被捞回来）。
func TestPauseExcludesFromFallback(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Pause("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	if a := p.PickExcluding(nil); a != nil {
		t.Errorf("兜底路径也不该捞回暂停号，实得 %s", a.UID)
	}
}

// 暂停不得碰冷却域（否则会静默清掉既有冷却）。
func TestPauseKeepsCoolingDomain(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "soft rate")
	p.Pause("u1")
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Error("暂停不该清掉既有冷却")
	}
	if st.Reason != "soft rate" {
		t.Errorf("Reason=%q want %q", st.Reason, "soft rate")
	}
}

func TestPauseDisableReviveInterplay(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Pause("u1")
	p.Disable("u1", "manual")
	st, _ := p.Status("u1")
	if !st.Disabled || st.Paused {
		t.Errorf("禁用应清掉暂停（更强终态）：%+v", st)
	}
	p.Pause("u1") // 对禁用号也能暂停（正交）
	p.Revive("u1")
	st, _ = p.Status("u1")
	if st.Paused || st.Disabled {
		t.Errorf("解冻是全清（含暂停）：%+v", st)
	}
}

func TestPauseUnknownUID(t *testing.T) {
	p := newPausedPool(t, "u1")
	if p.Pause("nope") {
		t.Error("未知 uid 的 Pause 应返回 false")
	}
	if p.Resume("nope") {
		t.Error("未知 uid 的 Resume 应返回 false")
	}
	if !p.Resume("u1") {
		t.Error("Resume 应幂等（未暂停也返回 true）")
	}
}

// 暂停必须落盘（跨重启不丢）。
func TestPausePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	p.Pause("u1")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "a", ExpiresAt: 9999999999})
	if st, _ := p2.Status("u1"); !st.Paused {
		t.Errorf("重启后 Paused 应保持：%+v", st)
	}
}

// paused 的聚合口径与探活口径（审查 B 的 P1-2 / 4 号的 P1-3）：
// disabled 汇总含 paused（上游语义），但逐账号字段必须仍是"paused=true, disabled=false"，
// 且 modelExempt 不能把暂停号当"还有别的模型可服务"。
func TestPausedCountsAndProbeSemantics(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Pause("u1")
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 0 || cooling != 0 || disabled != 1 {
		t.Errorf("聚合口径：total=%d healthy=%d cooling=%d disabled=%d，期望 1/0/0/1（paused 并入 disabled）",
			total, healthy, cooling, disabled)
	}
	st, _ := p.Status("u1")
	if !st.Paused || st.Disabled {
		t.Errorf("逐账号字段必须区分暂停与禁用：%+v", st)
	}
	if p.ModelExemptForTest("u1") {
		t.Error("暂停号不该被当作 modelExempt（否则 6004 探活会把它算作可服务）")
	}
}

// Pause 对禁用号必须拒绝（审查 B 的 P2-1：禁用是更强终态，不叠加）；ReviveDisabled 与
// Revive 都要清 paused（语义统一）。
func TestPauseRejectsDisabledAndReviveClearsPaused(t *testing.T) {
	p := newPausedPool(t, "u1")
	p.Disable("u1", "manual")
	if p.Pause("u1") {
		t.Error("对已禁用号 Pause 应返回 false")
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("不该出现 disabled+paused 组合：%+v", st)
	}
	p.ReviveDisabled("u1")
	if st, _ := p.Status("u1"); st.Paused || st.Disabled {
		t.Errorf("ReviveDisabled 应同时清 paused 与 disabled：%+v", st)
	}
}
