package upstream

import "testing"

func TestNormalizeModelRate(t *testing.T) {
	cases := map[string]string{
		"x0.05":        "0.05",
		"0.50x":        "0.5",
		" 1.62 ":       "1.62",
		"1.62 credits": "1.62",
		"0x":           "0",
		"":             "",
		"x未知":          "x未知", // 解析失败原样返回（不编造倍率）
	}
	for in, want := range cases {
		if got := normalizeModelRate(in); got != want {
			t.Errorf("normalizeModelRate(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEffectiveModelRate(t *testing.T) {
	// 有限时免费折扣（PromoFactor 非 nil 且有 PromoCredits）→ 取折后价
	f := 0.0
	mi := ModelInfo{ID: "hy3", Credits: "x1.5", PromoFactor: &f, PromoCredits: "0x"}
	if got := effectiveModelRate(mi); got != "0" {
		t.Errorf("限时免费应给 0，实得 %q", got)
	}
	// 无机器可读折扣 → 牌价
	mi2 := ModelInfo{ID: "glm", Credits: "x1.62"}
	if got := effectiveModelRate(mi2); got != "1.62" {
		t.Errorf("无折扣应取牌价，实得 %q", got)
	}
}

// storeModelRates 按 realm 整体替换：旧倍率不得冒充当前价（模型下架 → 查不到）。
func TestStoreAndQueryModelRates(t *testing.T) {
	c := &Client{}
	c.storeModelRates("cn", []ModelInfo{
		{ID: "glm-5.2", Credits: "x1.62"},
		{ID: "hy3", Credits: "x0.0"},
		{ID: ""},
	}, true)
	if got := c.ModelRate("cn", "glm-5.2"); got != "1.62" {
		t.Errorf("cn glm-5.2 倍率=%q want 1.62", got)
	}
	if got := c.ModelRate("cn", "hy3"); got != "0" {
		t.Errorf("免费模型倍率=%q want 0", got)
	}
	// realm 隔离：global 桶未写 → 查不到
	if got := c.ModelRate("global", "glm-5.2"); got != "" {
		t.Errorf("realm 应隔离，global 实得 %q", got)
	}
	// 整体替换：刷新后旧模型消失
	c.storeModelRates("cn", []ModelInfo{{ID: "new-model", Credits: "x2"}}, true)
	if got := c.ModelRate("cn", "glm-5.2"); got != "" {
		t.Errorf("刷新后旧倍率应被替换掉，实得 %q", got)
	}
	if got := c.ModelRate("cn", "new-model"); got != "2" {
		t.Errorf("新倍率=%q want 2", got)
	}
	// nil Client / 空模型名不 panic
	var nilC *Client
	if got := nilC.ModelRate("cn", "x"); got != "" {
		t.Errorf("nil Client 应返回空，实得 %q", got)
	}
	if got := c.ModelRate("cn", ""); got != "" {
		t.Errorf("空模型名应返回空，实得 %q", got)
	}
}

// 降级目录（complete=false）必须与旧快照取并集，且"一个倍率都没解析出来"时**不写**：
// 否则"只在 /v3 出现"的收费模型会变成未知 ⇒ 积分保底放行（审查 D 的 P2-6）。
func TestStoreModelRatesDegradedKeepsPrev(t *testing.T) {
	c := &Client{}
	c.storeModelRates("cn", []ModelInfo{
		{ID: "paid-only-in-v3", Credits: "x1.62"},
		{ID: "free", Credits: "x0"},
	}, true)
	// 降级目录：只剩 free（v3 路挂了）
	c.storeModelRates("cn", []ModelInfo{{ID: "free", Credits: "x0"}}, false)
	if got := c.ModelRate("cn", "paid-only-in-v3"); got != "1.62" {
		t.Errorf("降级时旧倍率必须保留（未知不等于免费），实得 %q", got)
	}
	if got := c.ModelRate("cn", "free"); got != "0" {
		t.Errorf("降级目录里的新值应覆盖，实得 %q", got)
	}
	// 降级 + 一个都解析不出：不能把旧快照抹成空桶
	c.storeModelRates("cn", []ModelInfo{{ID: "paid-only-in-v3", Credits: "动态选模"}}, false)
	if got := c.ModelRate("cn", "paid-only-in-v3"); got != "1.62" {
		t.Errorf("降级且无可解析倍率时不该清空旧快照，实得 %q", got)
	}
	// complete=true + 无可解析倍率：有意写空桶（目录确实没下发价，旧价不得冒充现价）
	c.storeModelRates("cn", []ModelInfo{{ID: "paid-only-in-v3", Credits: "动态选模"}}, true)
	if got := c.ModelRate("cn", "paid-only-in-v3"); got != "" {
		t.Errorf("全路成功且无可解析倍率时应清空（旧价不冒充现价），实得 %q", got)
	}
}

// IsTransientModelBlock（审查 E 的 P1-2）：只有"路由/容量类"文案才算瞬时（跳过负缓存）；
// 显式 11102、以及 404/文案形态的"模型不存在"都必须落负缓存 —— 否则账号不被 modelCooled，
// 每次请求继续烧号，且 >MaxRotate 的池因 modelUnavailableFinal 恒 false 而对这些模型永久 503。
func TestIsTransientModelBlock(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11102,"msg":"model [x] service info not found"}`, false},
		{`{"code":11102,"msg":"no available channel"}`, false}, // 有码以码为准
		{`{"code":404,"msg":"no available channel for model x"}`, true},
		{`{"code":400,"msg":"model x not found"}`, false}, // 文案形态也必须落缓存
		{`{"msg":"the model does not exist on this backend"}`, false},
		{`{"msg":"服务繁忙，请稍后重试"}`, true},
		{``, false},
	}
	for _, c := range cases {
		if got := IsTransientModelBlock(c.body); got != c.want {
			t.Errorf("IsTransientModelBlock(%q)=%v want %v", c.body, got, c.want)
		}
	}
}
