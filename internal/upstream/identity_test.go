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
		cl.injectAttribution(req, nil)
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
	cl.injectAttribution(req, nil)
	if req.Header.Get("X-Product") != "WorkBuddy" || req.Header.Get("X-IDE-Name") != "WorkBuddy" {
		t.Errorf("wb 头组不一致: %v", req.Header)
	}
	if req.Header.Get("X-IDE-Version") == "" {
		t.Error("wb 应带 X-IDE-Version")
	}
}

// 默认（不配 identity / client_name）必须与默认 UA **自洽**，而不是旧版的 SaaS 混搭。
func TestIdentityDefaultIsCoherent(t *testing.T) {
	cl := New()
	req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
	cl.injectAttribution(req, nil)
	if got := req.Header.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("默认 X-Product 应为 WorkBuddy（与 UA 自洽），实得 %q", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "WorkBuddy" {
		t.Errorf("默认应发 X-IDE-Name=WorkBuddy，实得 %q", got)
	}
	// UA 里的版本号与 X-IDE-Version 必须同值（官方头组形状）
	ua := cl.userAgent(nil)
	ver := req.Header.Get("X-IDE-Version")
	if ver == "" || !strings.Contains(ua, ver) {
		t.Errorf("X-IDE-Version=%q 必须出现在 UA %q 中", ver, ua)
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
