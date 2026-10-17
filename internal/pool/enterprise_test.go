package pool

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// S5 第 1 步验证 + 第 2 步前置安全网（§T.34.15）：
//
//	① 企业号必须被透出到对外 Status（面板/只读展示依赖它）；
//	② 判定**只认非空字符串** —— 缺失/空串/纯空白都必须是"个人号"。
//
// ② 尤其关键：若把空白误判为企业号，第 2 步门控会静默停掉该号的成长体系（无报错、只表现为"不涨了"）。
func TestStatusEnterpriseIDExposureAndJudgment(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "ent", AccessToken: "x", ExpiresAt: 9999999999, EnterpriseID: "E-123"})
	p.Add(&auth.Auth{UID: "per", AccessToken: "y", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "blank", AccessToken: "z", ExpiresAt: 9999999999, EnterpriseID: "   "})

	byUID := map[string]Status{}
	for _, st := range p.List() {
		byUID[st.UID] = st
	}
	if len(byUID) != 3 {
		t.Fatalf("应有 3 个账号，实得 %d", len(byUID))
	}
	// ① 透出
	if got := byUID["ent"].EnterpriseID; got != "E-123" {
		t.Errorf("企业号 enterpriseId 必须原样透出，实得 %q", got)
	}
	if got := byUID["per"].EnterpriseID; got != "" {
		t.Errorf("个人号不该有 enterpriseId，实得 %q", got)
	}
	// ② 判定（只认非空字符串）
	if !byUID["ent"].IsEnterprise() {
		t.Error("带非空 enterpriseId 应判为企业号")
	}
	if byUID["per"].IsEnterprise() {
		t.Error("缺失 enterpriseId 必须判为个人号")
	}
	if byUID["blank"].IsEnterprise() {
		t.Error("纯空白 enterpriseId 必须判为个人号（否则会静默停掉该号成长体系）")
	}
}
