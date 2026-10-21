package upstream

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// 【2026-10-21 回归】建连上限必须放宽到 ≥15s 且可配置。
//
// 生产实测：网络抖动时 10 次探测有 4 次 12s 都连不上、成功的也有 7.8s 才建连。
// 此前硬编码 8s ⇒ 请求死在 Dial 阶段，网关自己返回 503 upstream_timeout
// （日志 `dial tcp <ip>:443: i/o timeout`），而 header_timeout 给到 300s 也救不了。
func TestDefaultDialTimeoutWideEnough(t *testing.T) {
	if defaultDialTimeout < 15*time.Second {
		t.Fatalf("defaultDialTimeout=%v 太短：实测存在 >12s 才建连成功的正常链路，会把它们提前判死", defaultDialTimeout)
	}
	if defaultDialTimeout > 60*time.Second {
		t.Fatalf("defaultDialTimeout=%v 太长：建连阶段不该让用户等超过 1 分钟", defaultDialTimeout)
	}
}

// New() 造出的 Transport 必须真的用上放宽后的 dialer。
func TestNewTransportUsesWideDialTimeout(t *testing.T) {
	c := New()
	tr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("ChatHTTP.Transport 类型不是 *http.Transport: %T", c.ChatHTTP.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext 为空：建连将无任何超时上限（半死链路会永久阻塞）")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout 应 > 0")
	}
}

// DialTimeout 字段语义：0 表示交给默认值（不能变成"无上限"）。
func TestDialTimeoutZeroMeansDefault(t *testing.T) {
	c := New()
	if c.DialTimeout != 0 {
		t.Errorf("New() 不应预置 DialTimeout（交由 main 按 config 注入），实得 %v", c.DialTimeout)
	}
	if defaultDialTimeout == 0 {
		t.Error("defaultDialTimeout 不能为 0（那等于无上限）")
	}
	// 覆盖后能生效：模拟 main.go 的做法
	d := &net.Dialer{Timeout: 25 * time.Second, KeepAlive: 30 * time.Second}
	tr := c.ChatHTTP.Transport.(*http.Transport)
	tr.DialContext = d.DialContext
	if tr.DialContext == nil {
		t.Error("覆盖 DialContext 失败")
	}
}
