package upstream

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// C2：2026-10-28 起的新变体是 {"code":11128,"msg":"first message is not system prompt"}，
// 它不含 channelRefusedMarkers，分类落到 ErrClient —— 但取证必须照样触发，
// 否则本功能恰好漏掉最主要的排障场景。
func TestOutboundDumpCoversFirstMessageVariant(t *testing.T) {
	dir := t.TempDir()
	up := &Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"first message is not system prompt"}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SetOutboundDumpDir(dir)
	acct := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	if _, _, _, err := up.ChatStream(acct, []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("新变体（first message is not system prompt）没有落盘，文件数=%d", len(entries))
	}
}
