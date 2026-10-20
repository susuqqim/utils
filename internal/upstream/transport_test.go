package upstream

import (
	"net/http"
	"testing"
)

// 回归（2026-10，QA 发现）：本机 DNS fake-IP 场景下域名可解析、TCP 可连，但 TLS 永不回包；
// 旧 Transport 未设 TLSHandshakeTimeout / DialContext 超时，导致请求永久阻塞（曾把测试拖到 10 分钟）。
// 本用例钉住硬化后的关键字段，防止有人"顺手简化"回去。
func TestNewClientTransportHardened(t *testing.T) {
	c := New()
	if c.HTTP == nil || c.ChatHTTP == nil {
		t.Fatal("HTTP/ChatHTTP client 未初始化")
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP.Transport 类型=%T，期望 *http.Transport", c.HTTP.Transport)
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout 未设置：TLS 永不回包时会永久阻塞")
	}
	if tr.DialContext == nil {
		t.Error("DialContext 未设置：TCP 连接无超时")
	}
	if tr.Proxy == nil {
		t.Error("Proxy 未设置：不尊重 HTTPS_PROXY")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout 未设置：响应头前无上限")
	}
	if tr.ExpectContinueTimeout <= 0 {
		t.Error("ExpectContinueTimeout 未设置")
	}
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Error("ChatHTTP 应与 HTTP 共享同一 Transport（连接池复用）")
	}
}
