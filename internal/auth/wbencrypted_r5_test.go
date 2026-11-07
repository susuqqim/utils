package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 同一对象里「规范键=旧明文」+「大小写变体=新密文」时，必须用**密文那份**作为权威值，
// 否则会拿旧 token 覆盖新 token（可用性事故），且新值丢失。
func TestCanonicalizePrefersSealedValue(t *testing.T) {
	env, _ := sealWBValue("AT-NEW-FROM-ENVELOPE")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "AT-OLD-PLAINTEXT",
			"AccessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": "uid-canon"},
	}
	raw, _ := json.Marshal(doc)
	if got, err := Parse(raw); err != nil || got.AccessToken != "AT-NEW-FROM-ENVELOPE" {
		t.Fatalf("权威值应取自信封：got=%q err=%v", func() string {
			if got != nil {
				return got.AccessToken
			}
			return ""
		}(), err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "canon.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, _ := Parse(raw)
	a.FilePath = p
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), "AT-OLD-PLAINTEXT") || strings.Contains(string(saved), "AT-NEW-FROM-ENVELOPE") {
		t.Fatalf("明文落盘：%s", saved)
	}
}

// R5-1（core）：信封在变体键、明文在规范键的畸形文件——删重复键时必须转移 sealed 标记，
// 否则轮换后的新 token 会明文写回。
func TestCanonicalizeTransfersSealedMark(t *testing.T) {
	env, _ := sealWBValue("AT-FROM-VARIANT")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "AT-OLD-PLAIN-CANONICAL",
			"AccessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": "uid-tr"},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	path := filepath.Join(dir, "tr.json")
	_ = os.WriteFile(path, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = path
	a.AccessToken = "AT-ROTATED-NEW"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "AT-ROTATED-NEW") || strings.Contains(string(saved), "AT-OLD-PLAIN-CANONICAL") {
		t.Fatalf("明文落盘：%s", saved)
	}
}
