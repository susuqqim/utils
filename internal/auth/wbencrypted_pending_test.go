package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 混合形态：顶层 accessToken 信封 + auth 段同时存在（第三方工具/历史文件很常见）。
// 之前会"永久拒绝保存"，导致刷新后的 token 写不回文件。契约：能安全写回。
func TestMixedFormSaveSucceeds(t *testing.T) {
	atTop, _ := sealWBValue("AT-TOP-OLD")
	atNested, _ := sealWBValue("AT-NESTED-OLD")
	doc := map[string]any{
		"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": atTop["envelope"]},
		"refreshToken": "rt-plain",
		"expiresAt":    1800000000,
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": atNested["envelope"]},
			"refreshToken": "rt-plain-2",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": "uid-mixed"},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "mixed.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = p
	a.AccessToken = "AT-MIXED-NEW"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("混合形态不该拒绝保存: %v", err)
	}
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), "AT-MIXED-NEW") {
		t.Fatalf("新 token 被明文写回：\n%s", saved)
	}
	if strings.Contains(string(saved), "AT-TOP-OLD") || strings.Contains(string(saved), "AT-NESTED-OLD") {
		t.Fatalf("旧 token 明文残留：\n%s", saved)
	}
}

// 扁平形态 + 数字 uid：既要能解析，也不能把 uid 改写成字符串。
func TestFlatFormNumericUIDParses(t *testing.T) {
	at, _ := sealWBValue("AT-FLAT-NUM")
	doc := map[string]any{
		"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": at["envelope"]},
		"refreshToken": "rt",
		"expiresAt":    1800000000,
		"uid":          json.Number("1234567890123456789"),
	}
	raw, _ := json.Marshal(doc)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("扁平+数字 uid 解析失败: %v", err)
	}
	if a.AccessToken != "AT-FLAT-NUM" {
		t.Fatalf("AccessToken=%q", a.AccessToken)
	}
	if a.UID != "1234567890123456789" {
		t.Fatalf("UID=%q", a.UID)
	}
}
