package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 并发竞速的边界（独立构造，不依赖 Update 主流程）：
// 所有源都慢时，必须在 budget 内放弃，而不是逐个等满各自的超时。
func TestRaceFetchManifestBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		_, _ = w.Write([]byte("{}"))
	}))
	defer slow.Close()

	start := time.Now()
	_, _, err := raceFetchManifest(context.Background(), []string{slow.URL, slow.URL, slow.URL}, http.DefaultClient, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("全慢源应返回错误")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("应在 budget(~300ms) 内放弃，实际 %v", elapsed)
	}
}

// 竞速语义：慢源存在时，快源必须立刻胜出（不必等慢源超时）。
func TestRaceFetchManifestFirstWins(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(500)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"9.9.9","sha256":"x","size":1}`))
	}))
	defer fast.Close()

	start := time.Now()
	m, src, err := raceFetchManifest(context.Background(), []string{slow.URL, fast.URL}, http.DefaultClient, 5*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("快源应胜出: %v", err)
	}
	if m.Version != "9.9.9" || src != fast.URL {
		t.Fatalf("命中错误源: %+v %s", m, src)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("不该等慢源: %v", elapsed)
	}
}
