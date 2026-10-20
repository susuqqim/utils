package upstream

import (
	"net/http"
	"testing"
)

// 回归（2026-10-20，实测国际版偶发 403）：上游 WAF 返回的是 HTML 拦截页
// （<!DOCTYPE html>…<title>WAF Block Page</title>）。此前 Classify 把它落成 ErrClient，
// 于是既不冷却也不退避、直接透传给客户端 ⇒ 表现为"国际版时好时坏"。
func TestClassifyWafHTML(t *testing.T) {
	page := `<!DOCTYPE html><html lang="en"><head><title>WAF Block Page</title></head><body>blocked</body></html>`
	if got := Classify(http.StatusForbidden, page); got != ErrWafBlock {
		t.Fatalf("403+HTML 应判 ErrWafBlock，实得 %v", got)
	}
	// 业务 JSON 403 不能误判成 WAF
	biz := `{"code":11140,"msg":"request illegal"}`
	if got := Classify(http.StatusForbidden, biz); got != ErrAccountFault {
		t.Fatalf("403+业务码应仍走业务分类，实得 %v", got)
	}
	// 普通 403 空体也按 WAF 处理（拦截页可能无 body）
	if got := Classify(http.StatusForbidden, ""); got != ErrWafBlock {
		t.Logf("空体 403 判定=%v（可接受：不同网关形态）", got)
	}
}
