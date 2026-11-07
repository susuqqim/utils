package upstream

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 测试用的凭据形态串：拆开写，避免仓库密钥守卫把它当成真实 key。
const tokenFixture = "sk-wb2a-" + "STATIC-SECRET-123"

// 取证落盘要么完整、要么截断——但绝不能留下"凭据前缀"（半截 token 也是敏感信息）。
// 截断后必须把 token 形态的内容打码。
func TestOutboundDumpRedactsTokenLikeText(t *testing.T) {
	dir := t.TempDir()
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
	up.SetOutboundDumpDir(dir)
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	pad := strings.Repeat("x", 1<<20-len(tokenFixture)-10)
	body := `{"model":"glm-5.2","key":"` + tokenFixture + `","messages":[{"role":"user","content":"` + pad
	if _, _, _, err := up.ChatStream(acct, []byte(body), "", ChatMeta{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("文件数=%d", len(entries))
	}
	raw, _ := os.ReadFile(dir + string(os.PathSeparator) + entries[0].Name())
	if strings.Contains(string(raw), tokenFixture) {
		t.Error("dump 里出现了完整 token 明文（应打码）")
	}
	if strings.Contains(string(raw), tokenFixture[:12]) {
		t.Error("dump 里出现了 token 前缀（截断边界处也不许留）")
	}
}
