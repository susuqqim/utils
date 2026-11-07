package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R3 回归（P1）：三段以上路径（auth.tokens.accessToken 这类）此前被就地解密、
// 回写时又因"路径太深"跳过封装 → 明文落盘。契约：深路径和数组一样**不解密**，
// 原样保留信封。
func TestDeepEnvelopeNotDecryptedNorLeaked(t *testing.T) {
	deep, _ := sealWBValue("DEEP-SECRET-AT")
	at, _ := sealWBValue("AT-TOP")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": at["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
			"tokens": map[string]any{
				"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": deep["envelope"]},
			},
		},
		"account": map[string]any{
			"uid": "uid-deep",
			"profile": map[string]any{
				"nickname": map[string]any{"$wbEncrypted": 1, "envelope": deep["envelope"]},
			},
		},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-deep.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != "AT-TOP" {
		t.Fatalf("顶层凭据应正常解出，实得 %q", a.AccessToken)
	}
	a.FilePath = p
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), "DEEP-SECRET-AT") {
		t.Fatalf("深路径信封的明文被写回：\n%s", saved)
	}
	if !strings.Contains(string(saved), "$wbEncrypted") {
		t.Error("回写后丢了信封")
	}
	// 深路径那一份必须还是信封（没被解成明文，也没被删）
	var out map[string]any
	if err := json.Unmarshal(saved, &out); err != nil {
		t.Fatal(err)
	}
	authObj, _ := out["auth"].(map[string]any)
	tokens, _ := authObj["tokens"].(map[string]any)
	if tokens == nil || tokens["accessToken"] == nil {
		t.Fatalf("深路径 tokens 被丢弃: %#v", authObj["tokens"])
	}
	if _, isEnv := tokens["accessToken"].(map[string]any); !isEnv {
		t.Errorf("深路径应保持信封形态，实得 %T", tokens["accessToken"])
	}
}

// 信封标记的数值形态：1 / 1.0 / 1e0 都是"1"（UseNumber 下是 json.Number 文本）。
func TestEnvelopeMarkerNumericForms(t *testing.T) {
	for _, marker := range []string{"1.0", "1e0"} {
		env, _ := sealWBValue("AT-MARKER")
		doc := map[string]any{
			"auth": map[string]any{
				"accessToken":  map[string]any{"$wbEncrypted": json.RawMessage(marker), "envelope": env["envelope"]},
				"refreshToken": "rt",
				"expiresAt":    1800000000,
			},
			"account": map[string]any{"uid": "uid-marker"},
		}
		raw, _ := json.Marshal(doc)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("marker=%s Parse: %v", marker, err)
		}
		if a.AccessToken != "AT-MARKER" {
			t.Errorf("marker=%s 未被识别为信封（AccessToken=%q）", marker, a.AccessToken)
		}
	}
}

// 桌面端用数字形态的 uid 时，回写不能擅自改成字符串（那是改写别人的文件）。
func TestNumericUIDStaysNumeric(t *testing.T) {
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "at",
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": json.Number("1234567890123456789"), "nickname": "n"},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-numuid.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.UID != "1234567890123456789" {
		t.Fatalf("UID=%q want 数字串", a.UID)
	}
	a.FilePath = p
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(p)
	var out map[string]any
	if err := json.Unmarshal(saved, &out); err != nil {
		t.Fatal(err)
	}
	acct, _ := out["account"].(map[string]any)
	if _, ok := acct["uid"].(string); ok {
		t.Errorf("数字 uid 被写成字符串，擅自改写了桌面端文件: %#v", acct["uid"])
	}
}
