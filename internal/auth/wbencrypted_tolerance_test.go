package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A4：非凭据字段的信封坏了，不该让整个账号文件不可解析（否则账号在池里静默消失）。
// 凭据字段坏了必须报错（fail-closed）。
func TestCorruptNicknameEnvelopeDoesNotKillAccount(t *testing.T) {
	goodAT, _ := sealWBValue("AT-GOOD")
	goodRT, _ := sealWBValue("RT-GOOD")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": goodAT["envelope"]},
			"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": goodRT["envelope"]},
			"expiresAt":    1800000000,
			"domain":       "workbuddy.cn",
		},
		"account": map[string]any{
			"uid": "uid-a4",
			// 伪造一串坏信封（base64 合法但 JSON 不合法）
			"nickname": map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString([]byte("not-json"))},
		},
	}
	raw, _ := json.Marshal(doc)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("非凭据字段坏掉不该让整个账号加载失败: %v", err)
	}
	if a.AccessToken != "AT-GOOD" {
		t.Errorf("凭据应正常解出，实得 %q", a.AccessToken)
	}
	if a.Nickname != "" {
		t.Errorf("坏字段应留空，实得 %q", a.Nickname)
	}
}

// A5：明文字段恰好是数字形态的字符串（"13800000000"）时，不能被 JSON 猜测改成 number。
func TestNumericStringPlaintextStaysString(t *testing.T) {
	env, _ := sealWBValue("13800000000")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{
			"uid":      "uid-a5",
			"nickname": map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
		},
	}
	raw, _ := json.Marshal(doc)
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("数字形态字符串被改成 number 会让 string 字段解析失败: %v", err)
	}
	if got.Nickname != "13800000000" {
		t.Errorf("Nickname=%q want 13800000000（类型收敛后应保持字符串形态）", got.Nickname)
	}
}

// A6：suite != 1 的信封（未来格式）不能按当前套件静默降级，必须显式拒绝。
func TestUnsupportedSuiteRejected(t *testing.T) {
	key := wbKey()
	env, _ := json.Marshal(map[string]any{
		"suite":      2,
		"keyId":      wbKeyID(key),
		"nonce":      base64.StdEncoding.EncodeToString(make([]byte, 12)),
		"authTag":    base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"ciphertext": base64.StdEncoding.EncodeToString([]byte("abcd")),
	})
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString(env)},
			"expiresAt":   1800000000,
		},
		"account": map[string]any{"uid": "uid-a6"},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	_ = os.WriteFile(p, raw, 0o600)
	if _, err := Parse(raw); err == nil {
		t.Error("未知 suite 应显式报错，而不是按 suite=1 硬解/静默降级")
	}
}
