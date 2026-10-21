package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// retry.go 传输层容错：只处理"请求还没真正被上游处理"或"响应不完整"的**幂等**场景。
//
// 背景（2026-10-21 实测）：www.workbuddy.ai 出现**间歇性 TLS 握手卡死**——
// TCP 连接 0.28s 就建立，但 TLS 握手偶尔 17s 不返回（3 次里 1 次），叠加我们的
// TLSHandshakeTimeout=10s ⇒ 直接 503；同时资产同步出现 `unexpected end of JSON
// input`（响应体被提前断开，JSON 截断）。
//
// 判定与边界：
//   - **TLS 握手超时**：TCP 已连上但 TLS 未完成 ⇒ 一个字节的 HTTP 请求都没发出去，
//     对任何接口（含 chat）重试都安全，不会产生重复生成/重复计费；
//   - **响应体截断**：上游已开始返回但连接被掐断 ⇒ 仅对**幂等 RPC**（查询/刷新/上报）
//     重试；流式 chat 绝不在此重试（会产生重复生成）。
//
// 重试次数固定为 2（1 次原请求 + 1 次重试），间隔 150ms：足以吸收瞬时抖动，
// 又不会把上游限流放大成风暴。

// isTLSHandshakeErr 报告错误是否发生在 TLS 握手阶段（请求未到达上游）。
func isTLSHandshakeErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "TLS handshake timeout") ||
		strings.Contains(s, "tls: handshake") ||
		strings.Contains(s, "tls: first record does not look like a TLS handshake")
}

// isTruncatedJSONErr 报告 JSON 解析失败是否源于**响应体被截断**（而非内容格式错误）。
func isTruncatedJSONErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unexpected end of JSON input")
}

// cloneRequest 复制一个可重放的请求（body 从 GetBody 重新取）。
// GetBody 为空（如 stream body）时返回 error ⇒ 调用方放弃重试。
func cloneRequest(req *http.Request) (*http.Request, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	r := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = body
		return r, nil
	}
	if req.Body == nil {
		return r, nil
	}
	return nil, errors.New("request body is not replayable")
}

// doRetryTLS 发送请求；**仅**在 TLS 握手超时时重试一次。
//
// 供 chat 流式路径使用：握手都没完成，重试不会造成重复生成，纯网络层重试。
func (c *Client) doRetryTLS(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = c.HTTP
	}
	resp, err := client.Do(req)
	if err == nil || !isTLSHandshakeErr(err) {
		return resp, err
	}
	r, cerr := cloneRequest(req)
	if cerr != nil {
		return nil, err
	}
	time.Sleep(150 * time.Millisecond)
	return client.Do(r)
}

// drainAndClose 丢弃并关闭响应体（重试前清理，避免连接泄漏）。
func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}
