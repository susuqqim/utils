package main

import (
	"encoding/json"
	"testing"
)

// 回归（2026-10，VM 验收）：config.example.json 的占位符曾被当**真密钥**，
// 新装用户既不知道密钥、控制台又被 401 拦住（彻底卡死）。占位符必须按"未设置"处理。
func TestPlaceholderAPIKeyTreatedAsUnset(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"api_key":"CHANGE_ME_BEFORE_USE"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := c.normalize(); err != nil {
		// 其它字段缺失可能导致校验失败；归一化应已发生，先断言结果
		t.Logf("normalize 返回（可接受）: %v", err)
	}
	if c.APIKey != "" {
		t.Fatalf("占位符应归一化为空（等价于未设置），实得 %q", c.APIKey)
	}
}

// 真实密钥绝不能被归一化掉。
func TestRealAPIKeyKept(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"api_key":"sk-wb2a-YourSecretTokenHere"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_ = c.normalize()
	if c.APIKey == "" {
		t.Fatal("真实密钥不应被清空")
	}
}
