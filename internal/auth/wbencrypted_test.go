package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 新版桌面端把 accessToken/refreshToken/nickname 等字段换成 $wbEncrypted 信封
// （AES-256-GCM，见 wbencrypted.go）。用户直接把桌面端的 auth 文件丢进 auths/ 时，
// 网关必须能读；读不懂就表现为"添加账号后立刻 missing accessToken"。
//
// 期望值来自独立实现（Node 的 aes-256-gcm 手工解同一份夹具），不是拿本仓代码反推。
func TestParseEncryptedDesktopAuthFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "wbencrypted-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != "at-from-wbencrypted" {
		t.Errorf("AccessToken=%q want at-from-wbencrypted", a.AccessToken)
	}
	if a.RefreshToken != "rt-from-wbencrypted" {
		t.Errorf("RefreshToken=%q want rt-from-wbencrypted", a.RefreshToken)
	}
	if a.Nickname != "小凛" {
		t.Errorf("Nickname=%q want 小凛", a.Nickname)
	}
	if a.UID != "uid-wb-1" {
		t.Errorf("UID=%q want uid-wb-1", a.UID)
	}
	// 桌面端 expiresAt 是毫秒；本仓内部一律用 Unix 秒。不归一化会把有效期算到 5 万年以后。
	if a.ExpiresAt != 1800000000 {
		t.Errorf("ExpiresAt=%d want 1800000000（毫秒已归一化为秒）", a.ExpiresAt)
	}
}

// 回写必须保持加密：否则一次 token 刷新就把桌面端的加密文件变成明文（安全问题），
// 或丢掉桌面端自己写的字段（uin 等）。要求：仍是信封 + 不出现明文 token + 保留 uin + 可再次解析。
func TestSaveEncryptedAuthKeepsEnvelope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "wbencrypted-auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-uid-wb-1.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = path
	a.AccessToken = "at-rotated-by-gateway"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(saved)
	if !strings.Contains(s, "$wbEncrypted") {
		t.Error("回写后丢了 $wbEncrypted 信封：等于把桌面端加密文件降级成明文")
	}
	if strings.Contains(s, "at-rotated-by-gateway") {
		t.Error("回写后出现明文 access token")
	}
	if !strings.Contains(s, "\"uin\"") {
		t.Error("回写丢了桌面端自己写的 uin 字段")
	}

	// 独立核对：直接解密落盘文件的信封，看里面是不是新 token（不依赖 Parse→Save 自身往返）。
	_, _, doc, _, err := decryptWBEncrypted(saved)
	if err != nil {
		t.Fatalf("回写文件解密失败: %v", err)
	}
	authDoc, _ := doc["auth"].(map[string]any)
	if got, _ := authDoc["accessToken"].(string); got != "at-rotated-by-gateway" {
		t.Errorf("信封内 accessToken=%q want at-rotated-by-gateway", got)
	}
	if got := numOf(authDoc["expiresAt"]); got != "1800000000000" {
		t.Errorf("信封内 expiresAt=%v want 1800000000000（毫秒）", authDoc["expiresAt"])
	}
}

// numOf 把 JSON 数值的任意内部表示（json.Number / float64 / string）统一成数字文本。
func numOf(v any) string {
	switch t := v.(type) {
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	}
	return ""
}
