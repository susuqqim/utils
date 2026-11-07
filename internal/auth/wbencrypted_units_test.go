package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// A1：桌面端 expiresAt 是毫秒。回写若统一写成秒，桌面端会把凭证读成"1970 年已过期"
// → 要求重新登录（网关却还在用刷新后的 token），属于跨端兼容性回归。
// 契约：**保持原单位**回写。
func TestEncryptedAuthPreservesExpiresAtUnit(t *testing.T) {
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
		t.Fatal(err)
	}
	if a.ExpiresAt != 1800000000 {
		t.Fatalf("内部应为 Unix 秒 1800000000，实得 %d", a.ExpiresAt)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(path)
	plainSaved, _, doc, _, err := decryptWBEncrypted(saved)
	if err != nil {
		t.Fatalf("回写文件解密失败: %v", err)
	}
	if len(plainSaved) == 0 || doc == nil {
		t.Fatal("回写文件不含信封（应保持加密）")
	}
	authObj, _ := doc["auth"].(map[string]any)
	exp := numOf(authObj["expiresAt"])
	if exp == "" {
		t.Fatalf("auth.expiresAt 缺失或类型异常: %#v", authObj["expiresAt"])
	}
	if exp != "1800000000000" {
		t.Fatalf("回写成了 %s：桌面端按毫秒解释会读成 1970 年过期", exp)
	}
}
