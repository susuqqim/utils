package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

func TestPanelPinAndOverview(t *testing.T) {
	po := pool.New("")
	a1 := &auth.Auth{UID: "user-1", Nickname: "账号1", AccessToken: "t1"}
	a2 := &auth.Auth{UID: "user-2", Nickname: "账号2", AccessToken: "t2", Domain: "codebuddy.ai"}
	po.Add(a1)
	po.Add(a2)
	po.SetCredits("user-1", 500)
	po.SetCredits("user-2", 300)

	p := New(Config{
		Pool:    po,
		Version: "1.11.6-panel",
		APIKey:  "", // 无鉴权模式方便测试
	})

	// 1. 测试 pin user-1
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/accounts/user-1/pin", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var pinResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pinResp); err != nil {
		t.Fatal(err)
	}
	if pinResp["ok"] != true || pinResp["realm"] != "cn" {
		t.Fatalf("unexpected pinResp: %+v", pinResp)
	}

	// 2. 测试 overview 包含 pinned_uids 和统计汇总
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/overview", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var ov map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	pinnedUIDs, ok := ov["pinned_uids"].(map[string]any)
	if !ok || pinnedUIDs["cn"] != "user-1" {
		t.Fatalf("expected pinned_uids['cn'] == 'user-1', got %+v", ov["pinned_uids"])
	}
	if ov["total_credits"].(float64) != 800 {
		t.Fatalf("expected total_credits == 800, got %v", ov["total_credits"])
	}

	// 3. 测试 unpin user-1
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/accounts/user-1/unpin", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// 4. 测试 unpin_all
	po.PinAccount("user-2")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/panel/api/accounts/unpin_all", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(po.PinnedUIDs()) != 0 {
		t.Fatalf("expected all unpinned, got %+v", po.PinnedUIDs())
	}
}
