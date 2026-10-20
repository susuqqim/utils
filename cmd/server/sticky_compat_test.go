package main

import (
	"encoding/json"
	"testing"
)

// 回归（2026-10，VM 验收发现）：旧 config 里 session_sticky 是布尔 true，新版本严格解码会报
// "cannot unmarshal bool into ... struct" ⇒ 网关启动即失败（MSI 不覆盖已有 config）。必须兼容。
func TestSessionStickyAcceptsLegacyBool(t *testing.T) {
	var c struct {
		SessionSticky StickyCfg `json:"session_sticky"`
	}
	if err := json.Unmarshal([]byte(`{"session_sticky": true}`), &c); err != nil {
		t.Fatalf("旧的布尔写法应被接受: %v", err)
	}
	if !c.SessionSticky.Enabled || c.SessionSticky.TTL != "30m" || c.SessionSticky.GCInterval != "5m" {
		t.Fatalf("布尔 true 应等价于 enabled+默认 TTL/GC，实得 %+v", c.SessionSticky)
	}
	if err := json.Unmarshal([]byte(`{"session_sticky": false}`), &c); err != nil {
		t.Fatalf("false 也应被接受: %v", err)
	}
	if c.SessionSticky.Enabled {
		t.Fatal("false 应映射为 enabled=false")
	}
}

func TestSessionStickyAcceptsObject(t *testing.T) {
	var c struct {
		SessionSticky StickyCfg `json:"session_sticky"`
	}
	in := `{"session_sticky": {"enabled": true, "ttl": "1h", "gc_interval": "2m"}}`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("对象写法应被接受: %v", err)
	}
	if !c.SessionSticky.Enabled || c.SessionSticky.TTL != "1h" || c.SessionSticky.GCInterval != "2m" {
		t.Fatalf("对象字段未正确解码: %+v", c.SessionSticky)
	}
}
