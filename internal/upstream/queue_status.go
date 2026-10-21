package upstream

// queue_status.go 上游「排队/繁忙程度」查询（POST /v2/chat/queue/status）。
//
// 来源：官方客户端 `resources/app.asar` 里的 `createCapacityQueueApiClient`
// （@tencent-ai/codebuddy 的 capacity queue 实现，2026-10 反查）：
//
//	POST {endpoint}/v2/chat/queue/status   body: {"model": "<模型名>"}
//	headers: Authorization, Content-Type, X-Conversation-Request-ID[, X-Conversation-ID]
//	timeout: 15s
//
// 官方状态机（客户端 `handleQueueUpdate` 的分支）：
//
//	waiting    排队中（有队列位置，可继续等待）
//	full       队列已满 ← 这就是"繁忙"
//	user_limit 该用户额度受限
//	ready      就绪，可以重发
//	timeout    超时
//	expired    已过期 / 当前没有任何排队（**空闲时的常态**）
//	cancelled  已取消
//
// 官方错误码（随帧下发，不是 HTTP 码）：
//
//	6020 WAITING / 6021 FULL / 6022 USER_LIMIT
//
// 设计取舍：
//   - 只用**只读**语义（查询状态），绝不调用 /v2/chat/queue/cancel；
//   - 字段防御式解析：已知 status/message 提出来，其余原样保留（Extra），
//     上游随时可能加字段（如队列位置/预估等待），不让解析失败连累整体；
//   - 失败不重试（这是页面展示用的辅助信息，重试只会放大上游请求）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// QueueStatus 一次排队状态查询的结果。
type QueueStatus struct {
	// Status 七个官方状态之一：waiting/full/user_limit/ready/timeout/expired/cancelled；
	// 未知取值原样透出（上游新增状态时不至于丢信息）。
	Status string `json:"status"`
	// Message 上游给的提示（可能是 i18n key，如 "queue.expired.title"）。
	Message string `json:"message,omitempty"`
	// Extra 其余字段原样保留（防御式：队列位置/预估等待等未来字段）。
	Extra map[string]any `json:"extra,omitempty"`
}

// Busy 报告该状态是否属于「繁忙/不可立即发」。
//
// expired / ready / cancelled 都不算繁忙：
//   - expired 是**空闲时的常态**（实测空闲返回 expired），不能当成"拥堵"；
//   - ready 表示可以立即发；
//   - cancelled 是主动取消，与上游负载无关。
func (q *QueueStatus) Busy() bool {
	if q == nil {
		return false
	}
	switch q.Status {
	case "waiting", "full", "user_limit":
		return true
	}
	return false
}

// queueStatusPath 排队状态查询路径（与官方客户端一致）。
const queueStatusPath = "/v2/chat/queue/status"

// queueStatusTimeout 官方客户端用 15s；这里给 8s——页面展示用，不值得为它占满连接。
const queueStatusTimeout = 8 * time.Second

// FetchQueueStatus 查询「该账号 + 该模型」当前是否需要排队。
//
// 返回的 error 仅用于日志/诊断；调用方（面板）应把 nil 结果当作"未知"展示。
func (c *Client) FetchQueueStatus(a *auth.Auth, model string) (*QueueStatus, error) {
	if a == nil {
		return nil, fmt.Errorf("queue status: nil account")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("queue status: empty model")
	}
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+queueStatusPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// X-Conversation-Request-ID 用一次性 32hex：官方每次查询都带一个新的本轮 id。
	req.Header.Set("X-Conversation-Request-ID", session.NewMessageID())
	req.Header.Set("User-Agent", c.userAgent(a))
	c.injectCodeBuddyRequest(req)

	// 只读查询走独立短超时 client，避免和 chat 的长连接抢配额。
	client := &http.Client{Timeout: queueStatusTimeout, Transport: c.HTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if readErr != nil {
		return nil, fmt.Errorf("queue status read: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 160)}
	}

	// 信封：{"code":0,"msg":"OK","data":{...}}；data 缺失时把整个 body 当数据（防御）。
	var env struct {
		Code *int            `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("queue status parse: %w", err)
	}
	if env.Code != nil && *env.Code != 0 {
		// 业务错误码（6020/6021/6022 等）——把它们翻译成对应状态，仍返回可用结果。
		return &QueueStatus{Status: queueStatusFromBizCode(*env.Code), Message: env.Msg}, nil
	}
	payload := env.Data
	if len(payload) == 0 {
		payload = raw
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, fmt.Errorf("queue status payload: %w", err)
	}
	out := &QueueStatus{}
	if v, ok := fields["status"].(string); ok {
		out.Status = v
	}
	if v, ok := fields["message"].(string); ok {
		out.Message = v
	}
	// 其余字段保留（可能含队列位置、预估等待）
	rest := make(map[string]any, len(fields))
	for k, v := range fields {
		if k == "status" || k == "message" {
			continue
		}
		rest[k] = v
	}
	if len(rest) > 0 {
		out.Extra = rest
	}
	if out.Status == "" {
		out.Status = "unknown"
	}
	return out, nil
}

// queueStatusFromBizCode 把官方队列错误码翻译成状态字面量。
func queueStatusFromBizCode(code int) string {
	switch code {
	case 6020:
		return "waiting"
	case 6021:
		return "full"
	case 6022:
		return "user_limit"
	}
	return fmt.Sprintf("code_%d", code)
}
