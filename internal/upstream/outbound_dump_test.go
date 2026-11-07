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

// 11‑128（渠道校验）只在"我们发出去的 body 长什么样"这一层能查清：
// 上游只说 request illegal，不回显它看到了什么。取证开关打开时把**出站 body**落盘，
// 关掉时零开销、零落盘（用户内容敏感，默认必须关）。
func TestOutboundDumpOnChannelRefused(t *testing.T) {
	const refuse = `{"code":11128,"msg":"request illegal, first message is not system prompt"}`
	up := &Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(refuse)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	dir := t.TempDir()
	up.SetOutboundDumpDir(dir)
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"取证用的用户内容"}]}`)
	_, status, _, err := up.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStream err=%v", err)
	}
	if status != 400 {
		t.Fatalf("status=%d want 400", status)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("落盘文件数=%d want 1（11128 时应在取证目录留下一条）", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "取证用的用户内容") {
		t.Error("dump 里应包含真实出站 body（否则无法比对上游到底看到了什么）")
	}
	if !strings.Contains(s, "glm-5.2") {
		t.Error("dump 里应包含模型名")
	}
	if strings.Contains(s, "at1") {
		t.Error("dump 不得包含 access token")
	}
}

// 开关关闭时不得产生任何文件（默认行为）。
func TestOutboundDumpDisabledByDefault(t *testing.T) {
	up := &Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"request illegal"}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	dir := t.TempDir()
	up.SetOutboundDumpDir(dir)
	up.SetOutboundDumpDir("") // 显式关闭
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	if _, _, _, err := up.ChatStream(acct, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("关闭取证后仍落盘 %d 个文件", len(entries))
	}
}
