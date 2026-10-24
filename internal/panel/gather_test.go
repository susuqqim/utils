package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func manifestServer(t *testing.T, delay time.Duration, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		_, _ = w.Write([]byte(`{"version":"` + version + `","sha256":"x","size":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 全慢源：必须在 budget 内放弃（不能逐个等满各自超时）。
func TestGatherManifestBudget(t *testing.T) {
	slow := manifestServer(t, 3*time.Second, "1.0.0")
	start := time.Now()
	_, _, ok := gatherManifest(context.Background(), []string{slow.URL, slow.URL, slow.URL}, http.DefaultClient, 300*time.Millisecond)
	elapsed := time.Since(start)
	if ok {
		t.Fatal("全慢源应返回失败")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("应在 budget(~300ms) 内放弃，实际 %v", elapsed)
	}
}

// 关键语义（2026-10-24 真实审计教训）：取版本最高者，不是谁先回谁赢。
// 现实对应：jsDelivr 缓存滞后返回旧版、Release 直连返回新版 —— 必须选新版。
func TestGatherManifestPicksHighestVersion(t *testing.T) {
	stale := manifestServer(t, 0, "1.11.24")
	fresh := manifestServer(t, 300*time.Millisecond, "1.11.26")

	m, src, ok := gatherManifest(context.Background(), []string{stale.URL, fresh.URL}, http.DefaultClient, 3*time.Second)
	if !ok {
		t.Fatal("应至少有一个源成功")
	}
	if m.Version != "1.11.26" {
		t.Fatalf("必须取最高版本，实得 %s（源 %s）", m.Version, src)
	}
	if src != fresh.URL {
		t.Fatalf("source 应指向新版源，实得 %s", src)
	}
}

// 预算到期时：已拿到结果就返回其中最高的，而不是丢掉。
func TestGatherManifestReturnsBestOnBudget(t *testing.T) {
	fast := manifestServer(t, 0, "1.11.25")
	verySlow := manifestServer(t, 5*time.Second, "9.9.9")

	m, _, ok := gatherManifest(context.Background(), []string{verySlow.URL, fast.URL}, http.DefaultClient, 500*time.Millisecond)
	if !ok {
		t.Fatal("预算到期前已拿到结果，应返回")
	}
	if m.Version != "1.11.25" {
		t.Fatalf("应返回已拿到的最高版本 1.11.25，实得 %s", m.Version)
	}
}
