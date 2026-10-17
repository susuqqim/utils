// nilpool_test.go Pool 依赖缺失时的可诊断失败回归。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestNilPoolFailsCleanly 依赖缺失必须回可诊断的 500，而不是 nil 解引用 panic。
// 回归背景：ServeHTTP 此前直通 mux，chatCompletions 里 `Pool == nil ||` 的条件
// 会让粘性分支恒真并解引用 nil Pool → panic；/healthz 与 /status 也会 panic。
func TestNilPoolFailsCleanly(t *testing.T) {
	h := NewHandler(Config{Upstream: upstream.New(), APIKey: ""})
	for _, tc := range []struct {
		method, path, body string
	}{
		{"POST", "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`},
		{"GET", "/v1/models", ""},
		{"GET", "/status", ""},
		{"GET", "/healthz", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		// 不得 panic；应回 500 且带可诊断错误体
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: code=%d want 500", tc.method, tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "server_misconfigured") {
			t.Errorf("%s %s: body=%s want server_misconfigured", tc.method, tc.path, rec.Body.String())
		}
	}
	// OPTIONS 预检不应受依赖缺失影响（CORS 语义保持）
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("OPTIONS: code=%d want 200", rec.Code)
	}
}

// Upstream 缺失（NewHandler 依赖缺项）时，**依赖上游的端点**必须是可诊断的 500，
// 而不是让 /v1/models 在上游探测 goroutine 里 nil 解引用 —— goroutine 的 panic 不被
// handler recover 兜住，会直接终止整个网关进程。
// 同时 /healthz 作为**存活**探针必须保持可用（不因缺依赖被判死）：编排层据此决定是否重启。
func TestNilUpstreamFailsCleanly(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")}) // 故意不传 Upstream

	// 只有**真正解引用 Upstream** 的路由才 500；/status 只读账号池，缺上游客户端也应正常服务。
	for _, path := range []string{"/v1/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 500 {
			t.Fatalf("%s: status=%d want 500 body=%s", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "server_misconfigured") {
			t.Fatalf("%s: body=%s", path, rec.Body)
		}
	}

	// /status 不依赖上游客户端：必须 200（原实现把它也算作依赖，属过严兜底）。
	recStatus := httptest.NewRecorder()
	h.ServeHTTP(recStatus, httptest.NewRequest("GET", "/status", nil))
	if recStatus.Code != 200 {
		t.Fatalf("/status 只读账号池，不应因缺 Upstream 依赖失败: %d %s", recStatus.Code, recStatus.Body)
	}

	// /healthz 不受影响：空池 → 503（服务不可用）而不是 500（进程/依赖故障）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code == 500 {
		t.Fatalf("/healthz 是存活探针，不应因缺 Upstream 依赖返回 500: %s", rec.Body)
	}
}
