package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用真实密钥现场封一个值，构造各种信封形态的夹具（密钥固定，信封随机 nonce）。
func sealHelper(t *testing.T, v any) string {
	t.Helper()
	env, err := sealWBValue(v)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return env["envelope"].(string)
}

func writeAuth(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-container.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A-1：整段 auth 被封成一个信封（桌面端完全可能这么做）。启动路径会 LoadDir→BackfillRealm→SaveAtomic，
// 那时若不能原样重封，**绝不能**把 token 明文写回文件——必须报错并保持原文件不动。
func TestContainerEnvelopeNeverWrittenPlaintext(t *testing.T) {
	authObj := map[string]any{
		"accessToken":  "AT-CONTAINER-SECRET",
		"refreshToken": "RT-CONTAINER-SECRET",
		"expiresAt":    1800000000,
		"domain":       "workbuddy.cn",
		"realm":        "cn",
	}
	doc := map[string]any{
		"auth": map[string]any{
			"$wbEncrypted": 1,
			"envelope":     sealHelper(t, authObj),
		},
		"account": map[string]any{"uid": "uid-c1", "uin": "10001"},
	}
	path := writeAuth(t, doc)
	before, _ := os.ReadFile(path)

	a, err := Parse(before)
	if err != nil {
		t.Fatalf("Parse 容器级信封: %v", err)
	}
	if a.AccessToken != "AT-CONTAINER-SECRET" {
		t.Fatalf("解密失败 AccessToken=%q", a.AccessToken)
	}
	a.FilePath = path
	_ = a.SaveAtomic() // 允许返回错误（fail-closed），但不允许写出明文

	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "AT-CONTAINER-SECRET") || strings.Contains(string(after), "RT-CONTAINER-SECRET") {
		t.Fatalf("启动/保存把加密文件写成了明文 token：\n%s", after)
	}
}

// A-2：顶层 device_token 原本是信封时，回写也必须仍然是信封。
func TestEncryptedDeviceTokenStaysSealed(t *testing.T) {
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": sealHelper(t, "AT-DT-SECRET")},
			"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": sealHelper(t, "RT-DT-SECRET")},
			"expiresAt":    1800000000,
			"domain":       "workbuddy.cn",
		},
		"account":      map[string]any{"uid": "uid-dt"},
		"device_token": map[string]any{"$wbEncrypted": 1, "envelope": sealHelper(t, "DEVICE-SECRET-XYZ")},
	}
	path := writeAuth(t, doc)
	raw, _ := os.ReadFile(path)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.DeviceToken != "DEVICE-SECRET-XYZ" {
		t.Fatalf("device_token 解密失败: %q", a.DeviceToken)
	}
	a.FilePath = path
	a.DeviceToken = "DEVICE-ROTATED-ABC"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "DEVICE-ROTATED-ABC") {
		t.Error("device_token 被明文写回")
	}
	b, err := Parse(after)
	if err != nil {
		t.Fatalf("回写结果再解析失败: %v", err)
	}
	if b.DeviceToken != "DEVICE-ROTATED-ABC" {
		t.Errorf("回写后 device_token=%q want DEVICE-ROTATED-ABC", b.DeviceToken)
	}
}

// A-3：信封里的 nonce 长度异常（损坏/恶意文件）不能让 crypto/cipher panic 穿透，
// 否则 LoadDir 直接崩 → 启动崩溃循环。
func TestBadNonceLengthDoesNotPanic(t *testing.T) {
	key := wbKey()
	keyID := wbKeyID(key)
	badEnv, _ := json.Marshal(map[string]any{
		"suite":      1,
		"keyId":      keyID,
		"nonce":      base64.StdEncoding.EncodeToString([]byte("short")), // 5 字节，非 GCM nonce
		"authTag":    base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"ciphertext": base64.StdEncoding.EncodeToString([]byte("xxxx")),
	})
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": base64.StdEncoding.EncodeToString(badEnv)},
			"expiresAt":   1800000000,
		},
		"account": map[string]any{"uid": "uid-bad"},
	}
	path := writeAuth(t, doc)
	raw, _ := os.ReadFile(path)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("坏 nonce 触发 panic（会变成启动崩溃循环）：%v", r)
		}
	}()
	if _, err := Parse(raw); err == nil {
		t.Error("坏 nonce 应报错（fail-closed），而不是静默当成明文")
	}
}
