package upstream

import (
	"net/http"
	"testing"
)

// 上游 APISIX 拒掉凭证时回 HTML 401，死因只在 WWW-Authenticate 里。
// 只按正文分类会把「凭证真失效」和「中间代理页」混成一类：
// 前者会把号留在池里每次命中都 401，后者会把健康号停用。
func TestClassifyRespGateway401(t *testing.T) {
	const html401 = "<html><head><title>401 Authorization Required</title></head><body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>"
	const challenge = `Bearer realm="copilot", error="invalid_token", error_description="invalid token"`

	cases := []struct {
		name      string
		status    int
		body      string
		challenge string
		want      ErrKind
	}{
		{"点名 invalid_token → 真失效", http.StatusUnauthorized, html401, challenge, ErrAuthInvalid},
		{"无挑战头的 HTML 401 → 代理页", http.StatusUnauthorized, html401, "", ErrGatewayAuth},
		{"12153 仍是 session dead", http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`, "", ErrSessionDead},
		{"普通 JSON 401 不误判", http.StatusUnauthorized, `{"code":40001,"msg":"bad token"}`, "", ErrClient},
		{"403 WAF 页不受影响", http.StatusForbidden, html401, "", ErrWafBlock},
	}
	for _, c := range cases {
		if got := ClassifyResp(c.status, c.body, c.challenge); got != c.want {
			t.Errorf("%s: ClassifyResp=%v want %v", c.name, got, c.want)
		}
	}
}

// 只有网关层 401 值得「刷新后重试」；其余错误刷新无效。
func TestIsAuthRetryable(t *testing.T) {
	yes := []*Error{{Kind: ErrAuthInvalid}, {Kind: ErrGatewayAuth}}
	for _, e := range yes {
		if !IsAuthRetryable(e) {
			t.Errorf("%v 应可重试", e.Kind)
		}
	}
	no := []*Error{{Kind: ErrSessionDead}, {Kind: ErrClient}, {Kind: ErrSoftRate}, {Kind: ErrServer}}
	for _, e := range no {
		if IsAuthRetryable(e) {
			t.Errorf("%v 不应重试", e.Kind)
		}
	}
	if IsAuthRetryable(nil) {
		t.Error("nil 不应重试")
	}
}
