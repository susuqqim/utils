package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 出站身份预设：三套身份的 UA 与头组必须**自洽**（同属一套），否则上游会按
// "UA 与头组不一致"判非官方客户端（2026-10-21 渠道校验收紧）。
func TestIdentityPresetsCoherent(t *testing.T) {
	cases := []struct {
		identity  string
		wantUA    string
		wantName  string
		wantProd  string
		noIDEHead bool
	}{
		{IdentityVSCode, "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0", "CodeBuddyIDE", "CodeBuddy", false},
		{IdentityCLI, "CLI/2.63.2 CodeBuddy/2.63.2", "", "CodeBuddy", true},
	}
	for _, c := range cases {
		cl := New()
		cl.Identity = c.identity
		if got := cl.userAgent(nil); got != c.wantUA {
			t.Errorf("%s UA=%q want %q", c.identity, got, c.wantUA)
		}
		req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
		cl.injectAttribution(req)
		if got := req.Header.Get("X-Product"); got != c.wantProd {
			t.Errorf("%s X-Product=%q want %q", c.identity, got, c.wantProd)
		}
		if got := req.Header.Get("X-IDE-Name"); got != c.wantName {
			t.Errorf("%s X-IDE-Name=%q want %q", c.identity, got, c.wantName)
		}
		if c.noIDEHead && req.Header.Get("X-IDE-Name") != "" {
			t.Errorf("%s 不应发 X-IDE-*", c.identity)
		}
		if req.Header.Get("X-Agent-Purpose") != "conversation" {
			t.Errorf("%s 缺 X-Agent-Purpose", c.identity)
		}
	}
}

// wb 预设：UA 走默认三段式，头组为 WorkBuddy（与 UA 自洽）。
func TestIdentityWB(t *testing.T) {
	cl := New()
	cl.Identity = IdentityWB
	if got := cl.userAgent(nil); !strings.HasPrefix(got, "WorkBuddy/") {
		t.Errorf("wb UA 应走默认三段式，实得 %q", got)
	}
	req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
	cl.injectAttribution(req)
	if req.Header.Get("X-Product") != "WorkBuddy" || req.Header.Get("X-IDE-Name") != "WorkBuddy" {
		t.Errorf("wb 头组不一致: %v", req.Header)
	}
	if req.Header.Get("X-IDE-Version") == "" {
		t.Error("wb 应带 X-IDE-Version")
	}
}

// 默认（不配 identity）：完全保持历史行为 —— X-Product: SaaS、不发 X-IDE-*。
func TestIdentityDefaultKeepsLegacyBehavior(t *testing.T) {
	cl := New()
	req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
	cl.injectAttribution(req)
	if req.Header.Get("X-Product") != "SaaS" {
		t.Errorf("默认应为 SaaS，实得 %q", req.Header.Get("X-Product"))
	}
	if req.Header.Get("X-IDE-Name") != "" {
		t.Error("默认不应发 X-IDE-Name")
	}
}

func TestNormalizeIdentity(t *testing.T) {
	cases := map[string]string{"": IdentityDefault, "wb": IdentityWB, "WorkBuddy": IdentityWB, "VSC": IdentityVSCode, "vscode": IdentityVSCode, "ide": IdentityVSCode, " CLI ": IdentityCLI}
	for in, want := range cases {
		got, ok := NormalizeIdentity(in)
		if !ok || got != want {
			t.Errorf("NormalizeIdentity(%q)=(%q,%v) want %q", in, got, ok, want)
		}
	}
	if _, ok := NormalizeIdentity("nope"); ok {
		t.Error("未知取值应返回 ok=false")
	}
}
