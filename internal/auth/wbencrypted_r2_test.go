package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R2-1（P1）：扁平形态的加密文件（accessToken 直接封在顶层，没有 auth 容器）。
// 仅启动（LoadDir→BackfillRealm→SaveAtomic）就会把明文 token 写进新建的 auth 段 ——
// 因为正向校验只认 auth.* 路径。契约：扁平形态按扁平写回，且永不出现明文。
func TestFlatFormEnvelopeNeverPlaintext(t *testing.T) {
	at, _ := sealWBValue("AT-FLAT-SECRET")
	rt, _ := sealWBValue("RT-FLAT-SECRET")
	doc := map[string]any{
		"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": at["envelope"]},
		"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": rt["envelope"]},
		"expiresAt":    1800000000,
		"domain":       "workbuddy.cn",
		"realm":        "cn",
		"uid":          "uid-flat",
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-flat.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 扁平形态: %v", err)
	}
	if a.AccessToken != "AT-FLAT-SECRET" {
		t.Fatalf("解密失败: %q", a.AccessToken)
	}
	a.FilePath = p
	a.AccessToken = "AT-FLAT-ROTATED"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), "AT-FLAT-ROTATED") || strings.Contains(string(saved), "RT-FLAT-SECRET") {
		t.Fatalf("扁平形态把凭据写成了明文：\n%s", saved)
	}
	_, _, out, _, err := decryptWBEncrypted(saved)
	if err != nil {
		t.Fatalf("回写文件解密失败: %v", err)
	}
	if got, _ := out["accessToken"].(string); got != "AT-FLAT-ROTATED" {
		t.Errorf("扁平回写后 accessToken=%q want AT-FLAT-ROTATED", got)
	}
}

// 精度：超长数字形态的凭据不能经 float64 中转（会变成 ...789 → ...800，且不可逆）。
func TestBigIntegerCredentialKeepsPrecision(t *testing.T) {
	const big = "1234567890123456789"
	env, _ := sealWBValue(big)
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account": map[string]any{"uid": "uid-big"},
	}
	raw, _ := json.Marshal(doc)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != big {
		t.Fatalf("AccessToken=%q want %q（float64 中转丢精度）", a.AccessToken, big)
	}
}

// 数组/三段路径里的信封：SaveAtomic 不能因此永久失败（否则刷新后的 token 永不落盘）。
func TestArrayEnvelopeDoesNotBlockSave(t *testing.T) {
	env, _ := sealWBValue("inside-array")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]},
			"refreshToken": "rt",
			"expiresAt":    1800000000,
		},
		"account":  map[string]any{"uid": "uid-arr"},
		"accounts": []any{map[string]any{"token": map[string]any{"$wbEncrypted": 1, "envelope": env["envelope"]}}},
	}
	raw, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-arr.json")
	_ = os.WriteFile(p, raw, 0o600)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a.FilePath = p
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("数组内信封不应让保存失败: %v", err)
	}
	saved, _ := os.ReadFile(p)
	if strings.Contains(string(saved), "inside-array") {
		t.Fatalf("数组内信封被解密成明文写回：\n%s", saved)
	}
	var out map[string]any
	if err := json.Unmarshal(saved, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["accounts"].([]any); !ok {
		t.Fatalf("accounts 数组被整段丢弃: %#v", out["accounts"])
	}
}
