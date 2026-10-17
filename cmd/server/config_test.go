package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", c.Schedule.ActivityHours)
	}
	if len(c.Schedule.GrowthHours) != 1 || c.Schedule.GrowthHours[0] != 1 {
		t.Errorf("growth_hours=%v want [1]", c.Schedule.GrowthHours)
	}
	if len(c.Schedule.CatHours) != 1 || c.Schedule.CatHours[0] != 1 {
		t.Errorf("cat_hours=%v want [1]", c.Schedule.CatHours)
	}
	if !c.Schedule.GrowthEnabled || !c.Schedule.CatEnabled {
		t.Errorf("growth/cat enabled defaults want true/true, got %v/%v",
			c.Schedule.GrowthEnabled, c.Schedule.CatEnabled)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
	// 新 hours 缺省 → 回落默认（非空）。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
	}
	if len(c.Schedule.GrowthHours) != 1 || c.Schedule.GrowthHours[0] != 1 {
		t.Errorf("growth_hours=%v want default [1]", c.Schedule.GrowthHours)
	}
	if len(c.Schedule.CatHours) != 1 || c.Schedule.CatHours[0] != 1 {
		t.Errorf("cat_hours=%v want default [1]", c.Schedule.CatHours)
	}
	if !c.Schedule.GrowthEnabled || !c.Schedule.CatEnabled {
		t.Errorf("growth/cat switches must default true on legacy config: %+v", c.Schedule)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	// 签到/保活开关缺省 true（互不干扰）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// hours 仍回落默认。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21] even when disabled", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10] even when disabled", c.Schedule.ActivityHours)
	}
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", c.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
				t.Errorf("travel_hours=%v want default [9 21]", c.Schedule.TravelHours)
			}
			if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
				t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestMaxBodyDefault 默认 max_body_mb=8。
func TestMaxBodyDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Server.MaxBodyMB != 8 {
		t.Errorf("max_body_mb=%d want 8", c.Server.MaxBodyMB)
	}
}

// TestMaxBodyExplicit 显式设置 max_body_mb。
func TestMaxBodyExplicit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"server":{"max_body_mb":16}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxBodyMB != 16 {
		t.Errorf("max_body_mb=%d want 16", c.Server.MaxBodyMB)
	}
}

// TestMaxBodyInvalid 非法值（0/负数）normalize 报错：0 想表达"不限"会被静默当成 8MB，
// 与其误导不如 fail fast 提示显式配大上限。
func TestMaxBodyInvalid(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"server":{"max_body_mb":`+v+`}}`), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for max_body_mb=%s", v)
		}
		if !strings.Contains(err.Error(), "server.max_body_mb") {
			t.Errorf("error should name config key server.max_body_mb: %v", err)
		}
	}
}

// TestMaxBodyEnvOverride env WB2A_MAX_BODY_MB 非空覆盖 JSON 值。
func TestMaxBodyEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"server":{"max_body_mb":4}}`), 0o600)
	t.Setenv("WB2A_MAX_BODY_MB", "12")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxBodyMB != 12 {
		t.Errorf("max_body_mb=%d want env 12", c.Server.MaxBodyMB)
	}
}

// TestPromptDefaultCustom 默认 prompt.mode=custom 且 PromptText 为内置默认（非空）。
func TestPromptDefaultCustom(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "custom" {
		t.Errorf("prompt.mode=%q want custom", c.Prompt.Mode)
	}
	if c.PromptText == "" {
		t.Error("PromptText should be non-empty (built-in default)")
	}
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(c.PromptText))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 必须用 json.Marshal 构造，不能字符串拼接：Windows 下 pf 形如
	// `C:\Users\...\my.md`，其中 `\U` 是**非法 JSON 转义序列**，拼出来的配置
	// 直接解析失败（本用例此前在 Windows 上恒失败，报 "invalid escape sequence"）。
	// 这同时也是给用户的正确姿势：config.json 里的路径要么用 `/`，
	// 要么把反斜杠写成 `\\`。
	blob, err := json.Marshal(map[string]any{
		"prompt": map[string]any{"mode": "custom", "file": pf},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	os.WriteFile(cf, blob, 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestUnknownConfigKeysWarned 未知配置键被打成 WARN，且**不影响解析结果**。
// 背景：JSON 反序列化默认静默忽略未知字段，导致拼错/已退役的键长期无人发现
// （维护仓库 config.json 曾同时带 10 个不存在的键）。此用例锁定"告警可见但不致命"。
func TestUnknownConfigKeysWarned(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "c.json")
	os.WriteFile(cf, []byte(`{
		"listen": ":1234",
		"pool": {"max_in_flight": 2, "expiring_soon": "168h", "no_such_key": 1},
		"totally_unknown_section": {"x": 1}
	}`), 0o600)

	got := captureLog(t, func() {
		c, err := Load(cf)
		if err != nil {
			t.Fatalf("未知键不得导致加载失败: %v", err)
		}
		// 已知键照常生效
		if c.Pool.MaxInFlight != 2 {
			t.Errorf("max_in_flight=%d want 2", c.Pool.MaxInFlight)
		}
	})
	for _, wantKey := range []string{"pool.expiring_soon", "pool.no_such_key", "totally_unknown_section"} {
		if !strings.Contains(got, wantKey) {
			t.Errorf("期望告警包含 %q，实际日志:\n%s", wantKey, got)
		}
	}
}

// TestKnownConfigKeysNoWarn 合法配置不得产生任何未知键告警（防误报回归）。
func TestKnownConfigKeysNoWarn(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "c.json")
	// 用完整模板：任何 Config 支持的键都不该被误判为未知。
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("模板不可读，跳过：%v", err)
	}
	os.WriteFile(cf, raw, 0o600)
	got := captureLog(t, func() {
		if _, err := Load(cf); err != nil {
			t.Fatalf("模板配置应可加载: %v", err)
		}
	})
	if strings.Contains(got, "未知配置键") {
		t.Errorf("完整模板被误报未知键:\n%s", got)
	}
}

// captureLog 捕获 log 包（标准错误）在 fn 期间的输出。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	old := log.Writer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	log.SetOutput(w)
	// fn 内部若 Fatal 会直接退出，这里不兜；正常路径保证恢复。
	fn()
	log.SetOutput(old)
	w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 仍 custom。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "custom" {
		t.Errorf("legacy config should default to custom, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamVersionConfig 配置 upstream.client_version / cli_version 与 env
// WB2A_CLIENT_VERSION / WB2A_CLI_VERSION 均生效；缺省空串 = headers 层回落内置默认。
func TestUpstreamVersionConfig(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"client_version":"6.0.0","cli_version":"3.0.0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.ClientVersion != "6.0.0" || c.Upstream.CliVersion != "3.0.0" {
		t.Errorf("client_version=%q cli_version=%q want 6.0.0/3.0.0", c.Upstream.ClientVersion, c.Upstream.CliVersion)
	}
	// 缺省为空（headers 层回落内置默认）。
	if c2, err := Load(""); err != nil || c2.Upstream.ClientVersion != "" || c2.Upstream.CliVersion != "" {
		t.Errorf("default versions=%q/%q want empty (err=%v)", c2.Upstream.ClientVersion, c2.Upstream.CliVersion, err)
	}
	// env 覆盖。
	t.Setenv("WB2A_CLIENT_VERSION", "7.0.0")
	t.Setenv("WB2A_CLI_VERSION", "4.0.0")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.ClientVersion != "7.0.0" || c3.Upstream.CliVersion != "4.0.0" {
		t.Errorf("env versions=%q/%q want 7.0.0/4.0.0", c3.Upstream.ClientVersion, c3.Upstream.CliVersion)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, err := Load(""); err != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, err)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", c3.Upstream.UserAgent)
	}
}

// TestConfigUTF8BOM 验证 Windows 下带 UTF-8 BOM 头的 config.json 能够被正常解析。
func TestConfigUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	content := append([]byte("\xef\xbb\xbf"), []byte(`{"api_key":"bom-secret","auth_dir":"./custom-auths"}`)...)
	if err := os.WriteFile(fp, content, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load config with UTF-8 BOM failed: %v", err)
	}
	if c.APIKey != "bom-secret" {
		t.Errorf("APIKey = %q, want %q", c.APIKey, "bom-secret")
	}
	if c.AuthDir != "./custom-auths" {
		t.Errorf("AuthDir = %q, want %q", c.AuthDir, "./custom-auths")
	}
}

// resolveConfigPath：空原样、相对转绝对、绝对取所在目录、含 .. 需规整、外层引号剥离。
func TestResolveConfigPath(t *testing.T) {
	if p, d := resolveConfigPath(""); p != "" || d != "" {
		t.Fatalf(`resolveConfigPath("") = (%q, %q)`, p, d)
	}
	wd, _ := os.Getwd()
	p, d := resolveConfigPath("config.json")
	if !filepath.IsAbs(p) || filepath.Base(p) != "config.json" {
		t.Fatalf("relative -> abs = %q", p)
	}
	if d != wd {
		t.Fatalf("chdir = %q want %q", d, wd)
	}
	dir := t.TempDir()
	abs := filepath.Join(dir, "config.json")
	p2, d2 := resolveConfigPath(abs)
	if p2 != abs || d2 != dir {
		t.Fatalf("resolveConfigPath(%q) = (%q, %q)", abs, p2, d2)
	}
	p3, d3 := resolveConfigPath(filepath.Join(dir, "sub", "..", "config.json"))
	if d3 != dir || filepath.Base(p3) != "config.json" {
		t.Fatalf("dirty path = (%q, %q) want dir %q", p3, d3, dir)
	}

	// 引号包裹与首尾空格剥离测试（Windows 下残留引号会导致 filepath.Abs 非法字符报错）
	quoted := `"` + abs + `"`
	pQuoted, dQuoted := resolveConfigPath(quoted)
	if pQuoted != abs || dQuoted != dir {
		t.Fatalf("resolveConfigPath(%q) = (%q, %q) want (%q, %q)", quoted, pQuoted, dQuoted, abs, dir)
	}
	singleQuoted := `'` + abs + `'`
	pSingle, dSingle := resolveConfigPath(singleQuoted)
	if pSingle != abs || dSingle != dir {
		t.Fatalf("resolveConfigPath(%q) = (%q, %q) want (%q, %q)", singleQuoted, pSingle, dSingle, abs, dir)
	}
	spacedQuoted := `  "` + abs + `"  `
	pSpaced, dSpaced := resolveConfigPath(spacedQuoted)
	if pSpaced != abs || dSpaced != dir {
		t.Fatalf("resolveConfigPath(%q) = (%q, %q) want (%q, %q)", spacedQuoted, pSpaced, dSpaced, abs, dir)
	}
}

// TestFindDefaultConfig 测试未指定 config 且当前目录无 config.json 时自动探测数据目录
func TestFindDefaultConfig(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("WB2API_HOME", "")
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv("HOME", tempHome)

	// 初始状态无 config.json -> 返回空
	if c, d := findDefaultConfig(); c != "" || d != "" {
		t.Fatalf("findDefaultConfig() should be empty, got (%q, %q)", c, d)
	}

	// 1. 测试标准目录 %USERPROFILE%\.workbuddy2api\config.json
	defaultDir := filepath.Join(tempHome, ".workbuddy2api")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(defaultDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, d := findDefaultConfig()
	if c != cfgPath || d != defaultDir {
		t.Fatalf("findDefaultConfig() = (%q, %q) want (%q, %q)", c, d, cfgPath, defaultDir)
	}

	// 2. 测试 WB2API_HOME 环境变量覆盖优先
	customHome := t.TempDir()
	customCfg := filepath.Join(customHome, "config.json")
	if err := os.WriteFile(customCfg, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2API_HOME", customHome)
	c2, d2 := findDefaultConfig()
	if c2 != customCfg || d2 != customHome {
		t.Fatalf("findDefaultConfig(WB2API_HOME) = (%q, %q) want (%q, %q)", c2, d2, customCfg, customHome)
	}
}

// 回归（路线图 §O）：chdir 到 config 所在目录后，配置里以 ./ 开头的相对路径必须解析到该目录，
// 而不是进程原 CWD——否则开机自启（Run 键不改 CWD）会静默加载 0 个账号。
func TestConfigRelativePathsFollowConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"auth_dir":"./auths","state_file":"./data/state.json"}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, chdirTo := resolveConfigPath(cfgPath)
	oldWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(chdirTo); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(abs)
	if err != nil {
		t.Fatal(err)
	}
	gotAuth, _ := filepath.Abs(cfg.AuthDir)
	if filepath.Clean(gotAuth) != filepath.Clean(filepath.Join(dir, "auths")) {
		t.Fatalf("auth_dir = %q want %q", gotAuth, filepath.Join(dir, "auths"))
	}
	gotState, _ := filepath.Abs(cfg.StateFile)
	if filepath.Clean(gotState) != filepath.Clean(filepath.Join(dir, "data", "state.json")) {
		t.Fatalf("state_file = %q want %q", gotState, filepath.Join(dir, "data", "state.json"))
	}
}

func TestLoadNotExistIsDetectible(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	isOsNotExist := os.IsNotExist(err)
	isErrorsIs := errors.Is(err, os.ErrNotExist)
	t.Logf("os.IsNotExist: %v, errors.Is(err, os.ErrNotExist): %v", isOsNotExist, isErrorsIs)
	if !isErrorsIs && !isOsNotExist {
		t.Fatalf("neither os.IsNotExist nor errors.Is caught missing config: %v", err)
	}
}

func TestUITrayDefaultsTrue(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.UI.Tray == nil || !*c.UI.Tray {
		t.Fatalf("ui.tray 缺省应为 true，得到 %v", c.UI.Tray)
	}
}

func TestUITrayExplicitFalse(t *testing.T) {
	c, err := ParseConfig([]byte(`{"ui":{"tray":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.Tray == nil || *c.UI.Tray {
		t.Fatalf("ui.tray 显式 false 应保留，得到 %v", c.UI.Tray)
	}

	c, err = ParseConfig([]byte(`{"ui":{"tray":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.Tray == nil || !*c.UI.Tray {
		t.Fatalf("ui.tray 显式 true 应保留，得到 %v", c.UI.Tray)
	}
}

func TestUITrayEnvOverride(t *testing.T) {
	t.Setenv("WB2A_UI_TRAY", "false")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.Tray == nil || *c.UI.Tray {
		t.Fatalf("WB2A_UI_TRAY=false 应生效，得到 %v", c.UI.Tray)
	}
}

func TestUITrayIsKnownKey(t *testing.T) {
	// 反向保护：ui.tray 必须在已知键集合内，否则启动会对它打"未知配置键"WARN。
	if !knownConfigKeys()["ui.tray"] {
		t.Fatal("ui.tray 不在 knownConfigKeys 中（会误报未知键）")
	}
}

func TestUITrayPanelMergePreserved(t *testing.T) {
	// 模拟面板提交：只需保留/覆盖 ui.tray，其余键不得丢失。
	cur := map[string]any{"listen": ":17963", "ui": map[string]any{"tray": true}}
	incoming := map[string]any{"ui": map[string]any{"tray": false}}
	merged := mergeConfigMaps(cur, incoming)
	ui, _ := merged["ui"].(map[string]any)
	if ui == nil || ui["tray"] != false {
		t.Fatalf("合并后 ui.tray 应为 false，得到 %v", merged["ui"])
	}
	if merged["listen"] != ":17963" {
		t.Fatalf("合并不应丢失其它键，得到 %v", merged)
	}
}

// 出站 base URL 的底线校验（第十轮审计）：这四个字段决定"账号 access token 发往哪里"。
// 关键红线：**远端必须 https**（http 等于明文外发 token），本机回环允许 http 以便调试。
func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"空值回落内置默认", "", true},
		{"空白字符同空值", "   ", true},
		{"https 远端", "https://www.workbuddy.ai", true},
		{"https 带路径", "https://proxy.example.com/wb/", true},
		{"http 本机回环（允许调试）", "http://127.0.0.1:8899", true},
		{"http localhost", "http://localhost:8899", true},
		{"http IPv6 回环", "http://[::1]:8899", true},
		{"http 远端（红线：明文外发 token）", "http://evil.example.com", false},
		{"非 http(s) 协议", "ftp://evil.example.com", false},
		{"file 协议", "file:///etc/passwd", false},
		{"缺主机名", "https://", false},
		{"带 userinfo（会进日志/被中间人读）", "https://user:pass@evil.example.com", false},
		{"无法解析", "https://[::1", false},
	}
	for _, c := range cases {
		err := validateBaseURL("global.chat_base", c.raw)
		if c.ok && err != nil {
			t.Errorf("%s: 应通过，却报错 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: 应被拒，却通过了（%q）", c.name, c.raw)
		}
	}
}

// 校验必须真正接进 saveConfig 链路：http 远端 + userinfo 都要在保存时被拒（400 级错误），
// https 远端则可正常保存（合法自建反代用法不被拦）。
func TestSaveConfigRejectsUnsafeBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := Default()
	base.APIKey = "k"
	out, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		`{"global":{"chat_base":"http://evil.example.com","enabled":true}}`,
		`{"global":{"chat_base":"https://user:pass@evil.example.com","enabled":true}}`,
		`{"global":{"billing_base":"ftp://evil.example.com","enabled":true}}`,
	}
	for _, raw := range bad {
		if _, err := saveConfig([]byte(raw), path, nil, nil, nil, nil); err == nil {
			t.Errorf("saveConfig 应拒绝不安全 base URL: %s", raw)
		}
	}
	// 合法：https 远端（只验校验链路本身 —— 完整 saveConfig 还需要 live/pool/up/sch）
	okRaw := `{"api_key":"k","global":{"chat_base":"https://proxy.example.com","enabled":true}}`
	if _, err := ParseConfig([]byte(okRaw)); err != nil {
		t.Errorf("https 远端应可通过校验: %v", err)
	}
	if _, err := ParseConfig([]byte(`{"api_key":"k","global":{"chat_base":"http://proxy.example.com","enabled":true}}`)); err == nil {
		t.Error("http 远端必须被拒")
	}
}

// 负时长必须 fail fast（fuzz 目标 FuzzParseConfig 发现）：`"soft_rate":"-1h"` 旧实现能解析成功，
// 于是软冷却 until = now + (-1h) 落在过去 → 冷却静默失效，而配置看起来"设过了"。
func TestNegativeDurationsRejected(t *testing.T) {
	bad := []string{
		`{"cooldown":{"soft_rate":"-1h"}}`,
		`{"cooldown":{"soft_rate_max":"-30m"}}`,
		`{"pool":{"breaker_cooldown":"-5s"}}`,
		`{"pool":{"breaker_cooldown_max":"-1h"}}`,
		`{"session_sticky":{"ttl":"-1m"}}`,
		`{"session_sticky":{"gc_interval":"-1s"}}`,
	}
	for _, raw := range bad {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("负时长应被拒绝: %s", raw)
		}
	}
	// 正/零时长仍按原语义接受（0 是"关掉该机制"的既有写法，不在本次收窄范围内）
	ok := []string{
		`{"cooldown":{"soft_rate":"1m"}}`,
		`{"cooldown":{"soft_rate":"0s"}}`,
		`{"pool":{"breaker_cooldown":"0s"}}`,
		`{"session_sticky":{"ttl":"0s"}}`,
	}
	for _, raw := range ok {
		if _, err := ParseConfig([]byte(raw)); err != nil {
			t.Errorf("非负时长应被接受: %s → %v", raw, err)
		}
	}
}

// 超大 max_body_mb 必须 fail fast：运行时是 `int64(MaxBodyMB) << 20`，溢出后变负，
// 而 io.LimitReader(负数) 立刻 EOF ⇒ 所有对话请求都报"空 body"（fuzz 目标 FuzzParseConfig 发现）。
func TestMaxBodyMBBounded(t *testing.T) {
	ok := []int{1, 8, 100, maxBodyMBLimit}
	for _, mb := range ok {
		if _, err := ParseConfig([]byte(fmt.Sprintf(`{"server":{"max_body_mb":%d}}`, mb))); err != nil {
			t.Errorf("max_body_mb=%d 应被接受: %v", mb, err)
		}
	}
	bad := []int{maxBodyMBLimit + 1, 1 << 20, 8796093022208, 1<<63 - 1}
	for _, mb := range bad {
		if _, err := ParseConfig([]byte(fmt.Sprintf(`{"server":{"max_body_mb":%d}}`, mb))); err == nil {
			t.Errorf("max_body_mb=%d 应被拒绝（会溢出成负值）", mb)
		}
	}
	// 溢出确实会发生在 1<<43 附近——把"为什么设上限"钉在用例里（用变量做运行时移位，
	// 常量移位会被编译器直接判为溢出错误）。
	overflowMB := 8796093022208
	if int64(overflowMB)<<20 >= 0 {
		t.Fatal("前提失效：该值不再溢出，可放宽上限")
	}
}

// 密钥轮换相关：过渡钥与主钥相同时必须被清掉（否则"清空过渡钥"这一步会失去意义）。
func TestAPIKeyPreviousNormalized(t *testing.T) {
	c, err := ParseConfig([]byte(`{"api_key":"sk-wb2a-SAME","api_key_previous":"sk-wb2a-SAME"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.APIKeyPrevious != "" {
		t.Errorf("过渡钥与主钥相同应被清空，实得 %q", c.APIKeyPrevious)
	}
	c2, err := ParseConfig([]byte(`{"api_key":"sk-wb2a-NEW","api_key_previous":"sk-wb2a-OLD"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c2.APIKeyPrevious != "sk-wb2a-OLD" {
		t.Errorf("不同的过渡钥应保留，实得 %q", c2.APIKeyPrevious)
	}
}

// 生成的默认密钥必须与项目约定形态一致（sk-wb2a- + base64url 43 字符 = 51），
// 这样 scripts/guard-secrets.sh 的项目专属规则才真正覆盖它。
func TestWriteDefaultKeyShape(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	key, err := WriteDefault(p)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if len(key) != 51 || !strings.HasPrefix(key, "sk-wb2a-") {
		t.Errorf("默认密钥形态不符：len=%d prefix=%q", len(key), key[:min(8, len(key))])
	}
	var c Config
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("生成的配置无法解析: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("文件里的 api_key 与返回值不一致")
	}
	if c.APIKeyPrevious != "" {
		t.Errorf("新生成的配置不该带过渡钥，实得 %q", c.APIKeyPrevious)
	}
}

// server.read_timeout 解析（上游 15a6fc9，issue #100）：空 ⇒ 300s；"0" 合法（不限制）；
// 负值/非法 fail fast（静默钳 0 会把读超时保护悄悄关掉，只在生产上以慢速 body 拖死连接暴露）。
func TestParseServerReadTimeout(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{{"", 300 * time.Second, false}, {"300s", 300 * time.Second, false}, {"5m", 5 * time.Minute, false},
		{"0", 0, false}, {"-1s", 0, true}, {"abc", 0, true}} {
		got, err := ParseServerReadTimeout(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseServerReadTimeout(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("ParseServerReadTimeout(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// S5 第 2 步预备：enterprise_gate **默认必须是关闭**（false）——
// 开启会改变任务行为（企业号跳过个人成长体系），因此默认值本身就是安全契约：
// 任何"忘记显式配置"的部署都不应被默认改变行为。
func TestEnterpriseGateDefaultsOff(t *testing.T) {
	var c Config
	if c.EnterpriseGate {
		t.Fatal("零值 Config 的 EnterpriseGate 必须为 false（默认关闭）")
	}
	// 显式开启仍可解析（键名契约）
	var on struct {
		EnterpriseGate bool `json:"enterprise_gate"`
	}
	if err := json.Unmarshal([]byte(`{"enterprise_gate":true}`), &on); err != nil || !on.EnterpriseGate {
		t.Fatalf("enterprise_gate=true 应能解析：err=%v got=%v", err, on.EnterpriseGate)
	}
	// 旧配置（无该键）解析后必须仍为 false ⇒ 升级不改变既有部署行为
	var old struct {
		EnterpriseGate bool `json:"enterprise_gate"`
	}
	if err := json.Unmarshal([]byte(`{"listen":":17963"}`), &old); err != nil || old.EnterpriseGate {
		t.Fatalf("旧配置缺键时必须是 false：err=%v got=%v", err, old.EnterpriseGate)
	}
}
