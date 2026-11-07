package upstream

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func newDumpClient(t *testing.T, dir string) *Client {
	t.Helper()
	return &Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"request illegal"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// B-1：uid 不可信（来自配置/上游），带路径分隔符就能把取证文件写到目录之外。
func TestOutboundDumpSanitizesUID(t *testing.T) {
	dir := t.TempDir()
	up := newDumpClient(t, dir)
	up.SetOutboundDumpDir(dir)
	acct := &auth.Auth{UID: "../../evil", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	if _, _, _, err := up.ChatStream(acct, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("目录内文件数=%d want 1", len(entries))
	}
	if entries[0].IsDir() {
		t.Fatal("uid 里的 ../ 让落盘变成了目录穿越")
	}
	if strings.Contains(entries[0].Name(), "..") || strings.ContainsAny(entries[0].Name(), "/\\") {
		t.Fatalf("文件名未消毒: %s", entries[0].Name())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.json")); err == nil {
		t.Fatal("文件被写到了配置目录之外")
	}
}

// B-5：取证是排障用的，不能把几十 MB 的长上下文整份写盘。
func TestOutboundDumpCapsBytes(t *testing.T) {
	dir := t.TempDir()
	up := newDumpClient(t, dir)
	up.SetOutboundDumpDir(dir)
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	huge := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + strings.Repeat("x", 3<<20) + `"}]}`
	if _, _, _, err := up.ChatStream(acct, []byte(huge), "", ChatMeta{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("目录内文件数=%d want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	const maxDump = 1<<20 + 1024
	if info.Size() > maxDump {
		t.Fatalf("落盘 %d 字节，超过上限（应 ≤ %d）", info.Size(), maxDump)
	}
}
