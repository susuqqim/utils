package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R3-1：键名大小写变体（AccessToken / ACCESSTOKEN / Device_Token）绕过按路径名白名单的
// 校验 → 明文 token 写回加密文件。校验必须**按值**：解出的明文一个都不许出现在输出里。
func TestCaseVariantKeysCannotLeakPlaintext(t *testing.T) {
	secret := "SECRET-VALUE-XYZ"
	nested := []string{"AccessToken", "ACCESSTOKEN"}
	for _, key := range nested {
		env, _ := sealWBValue(secret)
		authObj := map[string]any{
			"accessToken":  "at-real", // 规范键：保证账号可解析
			"refreshToken": "rt",
			"expiresAt":    1800000000,
			key:            map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
		}
		doc := map[string]any{"auth": authObj, "account": map[string]any{"uid": "uid-case"}}
		raw, _ := json.Marshal(doc)
		dir := t.TempDir()
		p := filepath.Join(dir, "case.json")
		_ = os.WriteFile(p, raw, 0o600)

		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("%s Parse: %v", key, err)
		}
		a.FilePath = p
		_ = a.SaveAtomic() // 允许 fail-closed，但不允许写明文
		saved, _ := os.ReadFile(p)
		if strings.Contains(string(saved), secret) {
			t.Errorf("%s：明文被写回加密文件：\n%s", key, saved)
		}
	}

	// 顶层 device token 的大小写变体
	env, _ := sealWBValue(secret)
	doc := map[string]any{
		"auth":         map[string]any{"accessToken": "at-real", "refreshToken": "rt", "expiresAt": 1800000000},
		"account":      map[string]any{"uid": "uid-case"},
		"Device_Token": map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "case2.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Device_Token Parse: %v", err)
	}
	a.FilePath = p
	_ = a.SaveAtomic()
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), secret) {
		t.Errorf("Device_Token：明文被写回：\n%s", saved)
	}
}

// R3-2：wbDecodePlain 不能把带尾部残留的文本截断成前缀数字
// （"7abc123"→7、"2026-11-07T…"→2026，且会被封进信封永久化）。
func TestDecodePlainRejectsPartialJSON(t *testing.T) {
	for _, s := range []string{"7abc123", "2026-11-07T00:00:00Z", "1.2.3"} {
		if got := wbDecodePlain([]byte(s)); got != s {
			t.Errorf("wbDecodePlain(%q)=%#v want 原样字符串", s, got)
		}
	}
	if v, ok := wbDecodePlain([]byte(`{"a":1}`)).(map[string]any); !ok || v["a"] == nil {
		t.Errorf("合法 JSON 对象应被解出")
	}
}
