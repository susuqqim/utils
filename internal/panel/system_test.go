package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

func TestPanelSystemRestartAndShutdown(t *testing.T) {
	po := pool.New("")
	var restartCalled int32
	var shutdownCalled int32

	p := New(Config{
		Pool:    po,
		Version: "1.11.7-test",
		APIKey:  "secret-key",
		Restart: func() error {
			atomic.AddInt32(&restartCalled, 1)
			return nil
		},
		Shutdown: func() error {
			atomic.AddInt32(&shutdownCalled, 1)
			return nil
		},
	})

	// 1. 无鉴权访问 restart -> 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/system/restart", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rec.Code)
	}

	// 2. 带鉴权访问 restart -> 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/system/restart", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok == true, got %+v", resp)
	}
	// 稍等异步 goroutine 触发
	time.Sleep(350 * time.Millisecond)
	if atomic.LoadInt32(&restartCalled) != 1 {
		t.Fatalf("expected restartCalled == 1, got %d", atomic.LoadInt32(&restartCalled))
	}

	// 3. 带鉴权访问 shutdown -> 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/system/shutdown", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	time.Sleep(350 * time.Millisecond)
	if atomic.LoadInt32(&shutdownCalled) != 1 {
		t.Fatalf("expected shutdownCalled == 1, got %d", atomic.LoadInt32(&shutdownCalled))
	}
}

func TestPanelSystemNotImplemented(t *testing.T) {
	po := pool.New("")
	p := New(Config{
		Pool:     po,
		Version:  "1.11.7-test",
		Restart:  nil,
		Shutdown: nil,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/system/restart", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/system/shutdown", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}
}
