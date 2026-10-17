package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPromoProbeBothRealms 探针：核对**原始** /v3/config 里 data.modelPromotions
// 在 cn / global 两个 realm 各自是否存在、以及当前生效情况。
//
// 为什么要它（2026-10-05）：面板清单里 promo 只出现在 cn 模型上，需要区分
// 「上游没下发」与「我们的解析/路径漏了」。结论（实测）：
//
//	· cn      → 7 条（含 hy3「限时免费」factor=0、hy4-preview「夜间免费」factor=0 但当前不在时段）
//	· global  → **键存在、值为空数组 []** ⇒ 国际版 schema 支持该字段，但腾讯当前未下发优惠。
//
// 两个 realm 共用 fetchV3ConfigModelMap → applyModelPromotions（client.go:1645），
// 所以一旦国际版开始下发，面板无需改代码即可显示。
//
// 需手动运行（会真实访问上游）：
//
//	WB2A_PROMO_PROBE=1 [WB2A_AUTH_DIR=<auths 目录>] go test ./internal/upstream/ -run TestPromoProbeBothRealms -v
func TestPromoProbeBothRealms(t *testing.T) {

	if os.Getenv("WB2A_PROMO_PROBE") == "" {
		t.Skip("set WB2A_PROMO_PROBE=1 to run")
	}
	dir := os.Getenv("WB2A_AUTH_DIR")
	if dir == "" {
		dir = "/mnt/c/Users/GG/.workbuddy2api/auths"
	}
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	c := &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, ChatHTTP: &http.Client{Timeout: 60 * time.Second}, GlobalEnabled: true}
	c.ChatBaseCN = "https://copilot.tencent.com" // 让 CN 对照也能跑（正常由 main 注入）
	c.ChatBaseGlobal = ""                        // 空 = 默认 workbuddy.ai
	pick := map[string]*auth.Auth{}
	for _, a := range auths {
		r := strings.ToLower(a.Realm())
		if r == "" {
			r = "cn"
		}
		if pick[r] == nil {
			pick[r] = a
		}
	}
	for _, realm := range []string{"cn", "global"} {
		a := pick[realm]
		if a == nil {
			fmt.Printf("── realm=%s：无可用账号\n", realm)
			continue
		}
		base := c.chatBase(a)
		req, _ := http.NewRequest(http.MethodGet, base+"/v3/config", nil)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		if a.UID != "" {
			req.Header.Set("X-User-Id", a.UID)
		}
		req.Header.Set("X-Domain", v3ConfigDomain(a, base))
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("User-Agent", codeBuddyIDEUA)
		c.injectCodeBuddyRequest(req)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			fmt.Printf("── realm=%s base=%s → 请求失败: %v\n", realm, base, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var env struct {
			Code int `json:"code"`
			Data struct {
				Models          []map[string]any   `json:"models"`
				ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
				Keys            map[string]any     `json:"-"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		// 也统计原始 JSON 里 modelPromotions 键是否出现（防结构体漏字段）
		var generic map[string]any
		_ = json.Unmarshal(raw, &generic)
		rawHasKey := false
		if d, ok := generic["data"].(map[string]any); ok {
			_, rawHasKey = d["modelPromotions"]
		}
		fmt.Printf("── realm=%-6s base=%-28s HTTP=%d code=%d models=%d\n", realm, base, resp.StatusCode, env.Code, len(env.Data.Models))
		var snippet string
		var dataKeys []string
		if d, ok := generic["data"].(map[string]any); ok {
			for k := range d {
				dataKeys = append(dataKeys, k)
			}
			if v, ok := d["modelPromotions"]; ok {
				b, _ := json.Marshal(v)
				snippet = string(b)
				if len(snippet) > 220 {
					snippet = snippet[:220] + "…"
				}
			}
		}
		fmt.Printf("   原始 JSON 里存在 data.modelPromotions 键: %v ；解析出条目 %d 个\n", rawHasKey, len(env.Data.ModelPromotions))
		fmt.Printf("   modelPromotions 原始值: %s\n", snippet)
		fmt.Printf("   data 顶层键: %v\n", dataKeys)
		for i, p := range env.Data.ModelPromotions {
			if i >= 6 {
				fmt.Printf("   … 另有 %d 个\n", len(env.Data.ModelPromotions)-i)
				break
			}
			label, factor, disc := "", "nil", ""
			if p.Badge != nil {
				label = p.Badge.Label
			}
			if p.Discount != nil {
				factor = fmt.Sprintf("%v", p.Discount.Factor)
				disc = p.Discount.DiscountedCredits
			}
			ids := p.ModelIDs
			if len(ids) > 3 {
				ids = ids[:3]
			}
			fmt.Printf("     enabled=%v prio=%d label=%-10s factor=%-5s discountedCredits=%-6s active=%v modelIds=%v\n",
				p.Enabled, p.Priority, label, factor, disc, promoActive(&env.Data.ModelPromotions[i], time.Now()), ids)
		}
	}
}
