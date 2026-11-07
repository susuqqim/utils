package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R4-1：值级校验的两个 false negative——短值被长度豁免、含 < > & 的值被 JSON 转义失配。
func TestValueValidationCoversShortAndEscaped(t *testing.T) {
	cases := []struct{ name, secret string }{
		{"短值", "SHORT7!"},
		{"转义字符", "LONG<TOKEN>&0123456"},
	}
	for _, c := range cases {
		env, _ := sealWBValue(c.secret)
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  "at-real",
				"refreshToken": "rt",
				"expiresAt":    1800000000,
			},
			"account": map[string]any{"uid": "uid-v", "NickName": map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]}},
		}
		raw, _ := json.Marshal(doc)
		dir := t.TempDir()
		p := filepath.Join(dir, "v.json")
		_ = os.WriteFile(p, raw, 0o600)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s Parse: %v", c.name, err)
		}
		a.FilePath = p
		_ = a.SaveAtomic() // 允许 fail-closed
		saved, _ := os.ReadFile(p)
		if strings.Contains(string(saved), c.secret) {
			t.Errorf("%s：明文 %q 被写回：\n%s", c.name, c.secret, saved)
		}
	}
}

// R4-2：同一对象里 accessToken（旧信封）与 AccessToken 并存时，
// 新 token 必须被封成信封，不能明文落进另一个键。
func TestDuplicateCaseKeysDeterministic(t *testing.T) {
	env, _ := sealWBValue("AT-OLD-ENVELOPE")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": "uid-dup"},
	}
	raw, _ := json.Marshal(doc)
	for i := 0; i < 5; i++ {
		dir := t.TempDir()
		p := filepath.Join(dir, "dup.json")
		_ = os.WriteFile(p, raw, 0o600)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		a.FilePath = p
		a.AccessToken = "AT-NEW-PLAINTEXT"
		err = a.SaveAtomic()
		saved, _ := os.ReadFile(p)
		if strings.Contains(string(saved), "AT-NEW-PLAINTEXT") {
			t.Fatalf("第 %d 次：新 token 明文落盘（save_err=%v）：\n%s", i+1, err, saved)
		}
	}
}

// 信封标记 "1" / true 也要识别（历史文件形态）。
func TestEnvelopeMarkerStringAndBool(t *testing.T) {
	for _, marker := range []any{"1", true} {
		env, _ := sealWBValue("AT-MARKER-2")
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  map[string]any{"$wbEncrypted": marker, "envelope": env["envelope"]},
				"refreshToken": "rt",
				"expiresAt":    1800000000,
			},
			"account": map[string]any{"uid": "uid-m2"},
		}
		raw, _ := json.Marshal(doc)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("marker=%v Parse: %v", marker, err)
		}
		if a.AccessToken != "AT-MARKER-2" {
			t.Errorf("marker=%v 未识别为信封", marker)
		}
	}
}
