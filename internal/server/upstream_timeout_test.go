// upstream_timeout_test.go 钉住 isUpstreamTimeout 的三态判定口径（上游 55d1e4d）：
// 超时和"网络抖动"此前共用同一条换号路径 —— 注定超时的请求会轮转 MaxRotate 次，
// 期间还给一串健康号喂连败计数。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type timeoutErr struct{ timeout bool }

func (e timeoutErr) Error() string   { return "stub" }
func (e timeoutErr) Timeout() bool   { return e.timeout }
func (e timeoutErr) Temporary() bool { return e.timeout }

func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"net.Error 超时", timeoutErr{true}, false, true},
		{"net.Error 非超时（连接被拒）", timeoutErr{false}, false, false},
		{"显式 deadline(ctx)", fmt.Errorf("read: %w", context.DeadlineExceeded), false, true},
		{"显式 deadline(os.ErrDeadlineExceeded)", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), false, true},
		{"包装过的 net.Error 超时", fmt.Errorf("dial: %w", timeoutErr{true}), false, true},
		{"客户端断连（ctx 已取消）", context.Canceled, true, false},
		{"客户端仍在但 ctx 取消（自家看门狗 ⇒ 上游停滞）", context.Canceled, false, true},
		{"普通错误", errors.New("connection reset by peer"), false, false},
		{"nil", nil, false, false},
	}
	var _ net.Error = timeoutErr{}
	for _, c := range cases {
		if got := isUpstreamTimeout(c.err, c.clientGone); got != c.want {
			t.Errorf("%s: isUpstreamTimeout(%v, clientGone=%v)=%v want %v", c.name, c.err, c.clientGone, got, c.want)
		}
	}
}

// 集成：真超时必须**只打一次上游**（不轮转）、**不罚号**（无冷却/无连败计数），
// 并给出与"没号可用"可区分的 upstream_timeout 文案。这是本移植的收益点：
// 此前超时与网络抖动共用换号路径 ⇒ 慢上游会把健康号全喂上连败计数、客户端干等 MaxRotate 轮。
func TestChatUpstreamTimeoutStopsRotation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond) // 超过下面 client 的 80ms 超时
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer srv.Close()
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	up := &upstream.Client{HTTP: &http.Client{Timeout: 80 * time.Millisecond}, ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s（超时应 503）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("应给出可区分的 upstream_timeout 文案，实得：%s", rec.Body)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("超时必须止损：只该打 1 次上游，实得 %d 次（轮转就会给健康号喂连败计数）", n)
	}
	for _, uid := range []string{"u1", "u2", "u3"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.ErrTotal != 0 {
			t.Errorf("超时不该罚号：uid=%s %+v", uid, st)
		}
	}
}
