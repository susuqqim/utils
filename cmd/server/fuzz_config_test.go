package main

import (
	"encoding/json"
	"testing"
	"time"
)

// 配置解析必须能扛住任意输入：`config.json` 是人手编辑的、面板也直接 POST 任意 JSON 进来，
// 而解析发生在**启动路径**上 —— 一次 panic 就是"网关起不来"，比请求级 panic 更严重。
// 用法： go test ./cmd/server/ -run FuzzParseConfig -fuzz FuzzParseConfig -fuzztime 60s
func FuzzParseConfig(f *testing.F) {
	seeds := []string{
		`{}`,
		`{"api_key":"k","listen":"127.0.0.1:17963"}`,
		`null`, `[]`, `"str"`, `123`, `true`, ``,
		`{"server":{"max_body_mb":0}}`,
		`{"server":{"max_body_mb":-1}}`,
		`{"server":{"max_body_mb":99999999999999999999}}`,
		`{"cooldown":{"soft_rate":""}}`,
		`{"cooldown":{"soft_rate":"abc"}}`,
		`{"cooldown":{"soft_rate":"-1h"}}`,
		`{"pool":{"breaker_cooldown":"0s","breaker_threshold":-5}}`,
		`{"session_sticky":{"ttl":"1ns","gc_interval":"0"}}`,
		`{"schedule":{"checkin_hours":[25,-1,0,23],"activity_report_count":-3}}`,
		`{"schedule":{"checkin_hours":null}}`,
		`{"upstash":{"url":"://x","token":""}}`,
		`{"global":{"chat_base":"http://x","enabled":true}}`,
		"{\"prompt\":{\"mode\":\"file\"},\"prompt_file\":\"\u0000\"}",
		`{"unknown_key":{"deep":{"nested":[1,2,3]}}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		cfg, err := ParseConfig(raw)
		if err != nil {
			if cfg != nil {
				t.Fatalf("ParseConfig 出错时不应返回非 nil 配置")
			}
			return
		}
		if cfg == nil {
			t.Fatal("ParseConfig 成功却返回 nil")
		}
		// 不变量（normalize 的契约）：解析成功即字段可用。
		if cfg.Server.MaxBodyMB <= 0 {
			t.Fatalf("成功解析却留下非法 max_body_mb=%d", cfg.Server.MaxBodyMB)
		}
		if cfg.SoftRateDur < 0 || cfg.SoftRateMaxDur < 0 || cfg.BreakerCooldownDur < 0 || cfg.BreakerCooldownMaxD < 0 {
			t.Fatalf("成功解析却留下负时长: soft=%v max=%v breaker=%v/%v",
				cfg.SoftRateDur, cfg.SoftRateMaxDur, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
		}
		if cfg.SessionTTL < 0 || cfg.SessionGCInterval < 0 {
			t.Fatalf("成功解析却留下负 session 时长: ttl=%v gc=%v", cfg.SessionTTL, cfg.SessionGCInterval)
		}
		// 解析成功 → 再喂给 ParseConfigInto（面板保存路径用它做热重载），同样不得 panic。
		// 注意契约：dst 必须已初始化（覆盖语义，不补默认值）→ 这里按调用方口径传 Default()。
		if _, err := ParseConfigInto(raw, Default()); err != nil {
			t.Fatalf("ParseConfig 成功但 ParseConfigInto(Default()) 失败: %v", err)
		}
		// nil dst 也必须有确定行为（不再是含糊的 Unmarshal(nil) 错误）。
		if _, err := ParseConfigInto(raw, nil); err != nil {
			t.Fatalf("ParseConfig 成功但 ParseConfigInto(nil) 失败: %v", err)
		}
		// 序列化回去必须仍是合法 JSON（面板 GET 会 marshal 给前端）。
		if _, err := json.Marshal(cfg); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	})
}

// 面板保存路径会先 merge 再 ParseConfig：这里单独验证 merge 不因任意输入 panic。
func FuzzMergeConfigMaps(f *testing.F) {
	f.Add([]byte(`{"a":1}`), []byte(`{"b":2}`))
	f.Add([]byte(`{"a":{"b":1}}`), []byte(`{"a":{"c":2}}`))
	f.Add([]byte(`null`), []byte(`{"x":[1]}`))
	f.Add([]byte(`{"a":[1,2]}`), []byte(`{"a":null}`))
	f.Fuzz(func(t *testing.T, cur, in []byte) {
		var a, b map[string]any
		_ = json.Unmarshal(cur, &a)
		_ = json.Unmarshal(in, &b)
		if a == nil {
			a = map[string]any{}
		}
		merged := mergeConfigMaps(a, b)
		if merged == nil {
			t.Fatal("merge 返回 nil（调用方会立即 deref）")
		}
		_ = mergedJSON(merged)
	})
}

var _ = time.Second
