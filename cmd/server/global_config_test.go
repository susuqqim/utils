package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// TestGlobalDefaults 断言 global 段缺省：enabled=true（默认开启）、base 空（回落默认）。
func TestGlobalDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Global.Enabled {
		t.Error("global.enabled default should be true")
	}
	if c.Global.ChatBase != "" || c.Global.BillingBase != "" {
		t.Errorf("global bases default should be empty (fallback upstream defaults), got %q/%q",
			c.Global.ChatBase, c.Global.BillingBase)
	}
}

// TestGlobalParsedFromFile 断言 global 段可从 config 文件解析并覆盖。
func TestGlobalParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"global":{
			"enabled":true,
			"chat_base":"https://workbuddy-api-qa.workbuddy.ai",
			"billing_base":"https://billing.workbuddy.ai"
		}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Global.Enabled {
		t.Error("global.enabled want true from file")
	}
	if c.Global.ChatBase != "https://workbuddy-api-qa.workbuddy.ai" {
		t.Errorf("chat_base=%q", c.Global.ChatBase)
	}
	if c.Global.BillingBase != "https://billing.workbuddy.ai" {
		t.Errorf("billing_base=%q", c.Global.BillingBase)
	}
}

// TestGlobalEnabledAbsentIsTrue 断言 global 段缺席（旧 config 文件）→ enabled=true（默认开启）。
func TestGlobalEnabledAbsentIsTrue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Global.Enabled {
		t.Error("absent global section must default to enabled (true)")
	}
}

// TestGlobalEnabledExplicitOffEscapeHatch 显式 "enabled": false → 关闭（逃生门保留）。
func TestGlobalEnabledExplicitOffEscapeHatch(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"global":{"enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.Enabled {
		t.Error("explicit global.enabled=false must disable")
	}
}

// TestSaveConfigHotReloadsGlobal 验证在线保存配置时，global 段的变更能够立即热生效。
func TestSaveConfigHotReloadsGlobal(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"listen":":17963",
		"global":{
			"enabled":true,
			"chat_base":"https://www.workbuddy.ai",
			"billing_base":"https://www.workbuddy.ai"
		}
	}`), 0o600)

	up := &upstream.Client{
		ChatBaseGlobal:    "https://www.workbuddy.ai",
		BillingBaseGlobal: "https://www.workbuddy.ai",
		GlobalEnabled:     true,
	}
	p := pool.New("")
	sch := scheduler.New(scheduler.Config{})
	live := livecfg.New(livecfg.Snapshot{})

	newCfgJSON := []byte(`{
		"global":{
			"enabled":true,
			"chat_base":"https://codebuddy.ai",
			"billing_base":"https://billing.codebuddy.ai"
		}
	}`)

	restartFields, err := saveConfig(newCfgJSON, fp, live, p, up, sch)
	if err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	for _, f := range restartFields {
		if strings.HasPrefix(f, "global.") {
			t.Errorf("global field %q should be hot-applied, not require restart", f)
		}
	}

	if up.ChatBaseGlobal != "https://codebuddy.ai" {
		t.Errorf("ChatBaseGlobal=%q want https://codebuddy.ai", up.ChatBaseGlobal)
	}
	if up.BillingBaseGlobal != "https://billing.codebuddy.ai" {
		t.Errorf("BillingBaseGlobal=%q want https://billing.codebuddy.ai", up.BillingBaseGlobal)
	}
}
