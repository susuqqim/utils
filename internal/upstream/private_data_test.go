package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

func pdReq(t *testing.T, c *Client, a *auth.Auth) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	return req
}

// X-Private-Data 的官方默认值必须按身份/站点正确（证据见 identity.go 注释）：
//   - WB 桌面端：无条件 true；
//   - VSCode：仅国际站发 true，国内站不发；
//   - CLI：企业号 true、个人号 false。
func TestPrivateDataIdentityDefaults(t *testing.T) {
	cn := &auth.Auth{UID: "cn", AccessToken: "at", Domain: "www.codebuddy.cn"}
	gl := &auth.Auth{UID: "gl", AccessToken: "at", Domain: "www.workbuddy.ai"}
	ent := &auth.Auth{UID: "ent", AccessToken: "at", Domain: "www.codebuddy.cn", EnterpriseID: "ent-1"}

	cases := []struct {
		name     string
		identity string
		a        *auth.Auth
		want     string // "" = 不发
	}{
		{"WB(cn)", IdentityWB, cn, "true"},
		{"WB(global)", IdentityWB, gl, "true"},
		{"默认=WB(cn)", "", cn, "true"},
		{"VSCode(cn) 不发", IdentityVSCode, cn, ""},
		{"VSCode(global) true", IdentityVSCode, gl, "true"},
		{"CLI 个人号 false", IdentityCLI, cn, "false"},
		{"CLI 企业号 true", IdentityCLI, ent, "true"},
	}
	for _, c := range cases {
		cl := New()
		cl.Identity = c.identity
		got := pdReq(t, cl, c.a).Header.Get("X-Private-Data")
		if got != c.want {
			t.Errorf("%s: X-Private-Data=%q want %q", c.name, got, c.want)
		}
	}
}

// 运维显式声明（true/false）必须逐字覆盖身份默认，包括 VSCode 国内站（默认不发）。
func TestPrivateDataOperatorOverride(t *testing.T) {
	cn := &auth.Auth{UID: "cn", AccessToken: "at", Domain: "www.codebuddy.cn"}
	for _, c := range []struct{ decl, want string }{
		{"true", "true"},
		{"false", "false"},
		{"auto", ""}, // VSCode 国内站默认不发
		{"", "true"}, // 空 = auto，但身份是 WB → true
	} {
		cl := New()
		cl.PrivateData = c.decl
		identity := IdentityVSCode
		if c.decl == "" {
			identity = IdentityWB
		}
		cl.Identity = identity
		got := pdReq(t, cl, cn).Header.Get("X-Private-Data")
		if got != c.want {
			t.Errorf("decl=%q identity=%s: got %q want %q", c.decl, identity, got, c.want)
		}
	}
}

func TestNormalizePrivateData(t *testing.T) {
	ok := map[string]string{
		"": PrivateDataAuto, "auto": PrivateDataAuto, "AUTO": PrivateDataAuto,
		"true": PrivateDataTrue, "on": PrivateDataTrue, "1": PrivateDataTrue,
		"false": PrivateDataFalse, "off": PrivateDataFalse, "0": PrivateDataFalse,
	}
	for in, want := range ok {
		got, valid := NormalizePrivateData(in)
		if !valid || got != want {
			t.Errorf("NormalizePrivateData(%q)=(%q,%v) want %q", in, got, valid, want)
		}
	}
	if _, valid := NormalizePrivateData("maybe"); valid {
		t.Error("未知取值应返回 false")
	}
}

// 语义方向守卫：true 是「数据私有 / 不参与优化」，不能写反（hub #528 曾把文案写反）。
func TestPrivateDataSemanticsTrueIsPrivate(t *testing.T) {
	cl := New()
	cl.PrivateData = PrivateDataTrue
	cl.Identity = IdentityWB
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	if got := pdReq(t, cl, a).Header.Get("X-Private-Data"); got != "true" {
		t.Fatalf("声明 true 必须原样发出 true，实得 %q", got)
	}
}
