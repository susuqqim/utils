package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
)

// TestHandlerPhase3SessionFollowsActiveChunk 端到端验证：
// "已建立的会话也服从 Phase 3 的主刷分块与临期加权调度"
// 当分块耗尽切换到新账号时，即使是老会话，也自动跟随新分块账号，
// 且会话粘性绑定自动收敛更新到新账号。
func TestHandlerPhase3SessionFollowsActiveChunk(t *testing.T) {
	st := newBindStore()
	p := pool.New("")
	p.SetSchedulePolicy(pool.SchedulePolicy{
		ChunkQuotaCredits: 25.0,
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	a1 := &auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999}
	a2 := &auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999}
	p.Add(a1)
	p.Add(a2)

	now := time.Now()
	// a1 有 24h 临期积分 20 credit（优先作为第一块）
	p.UpdateResourceDetail("a1", 1000, now.Add(18*time.Hour), 20, 20)
	// a2 无临期套餐（底仓）
	p.UpdateResourceDetail("a2", 3000, now.Add(30*24*time.Hour), 0, 0)

	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: p.AvailableUIDs,
	})

	var hitUIDs []string
	var mu sync.Mutex
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if authz == "Bearer at-a1" {
			hitUIDs = append(hitUIDs, "a1")
		} else if authz == "Bearer at-a2" {
			hitUIDs = append(hitUIDs, "a2")
		}
		return 200, sseOK, true
	})

	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	// 1. 发起第一轮对话（建立会话粘性，绑定 conv-phase3）
	req1 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"turn 1"}],"metadata":{"conversation_id":"conv-phase3"}}`))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("turn 1 failed: %d", rec1.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 1 || hitUIDs[0] != "a1" {
		t.Fatalf("turn 1 should hit a1 (Tier 1 24h expiring), got %v", hitUIDs)
	}
	mu.Unlock()

	// 检查会话已被绑定到 a1
	if uid, ok := st.lastUID("conv-phase3"); !ok || uid != "a1" {
		t.Fatalf("session should be bound to a1, got %s ok=%v", uid, ok)
	}

	// 2. 第二轮对话（a1 还在分块内，虽然会话已经建立，仍命中 a1）
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"turn 2"}],"metadata":{"conversation_id":"conv-phase3"}}`))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("turn 2 failed: %d", rec2.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 2 || hitUIDs[1] != "a1" {
		t.Fatalf("turn 2 should hit a1, got %v", hitUIDs)
	}
	mu.Unlock()

	// 3. 模拟 a1 的 24h 临期积分耗尽（消耗 20 credit）
	p.NoteUsage("a1", 20.0, 1000)

	// 4. 核心验证：第三轮对话（同一会话 conv-phase3 继续发消息）
	// 按照用户需求："已建立的会话也服从 Phase 3 的主刷分块与临期加权调度"
	// a1 临期任务完成切号，第三轮必须服从分块切换到 a2，而不是被死锁在 a1 上！
	req3 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"turn 3"}],"metadata":{"conversation_id":"conv-phase3"}}`))
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("turn 3 failed: %d", rec3.Code)
	}

	mu.Lock()
	if len(hitUIDs) != 3 || hitUIDs[2] != "a2" {
		t.Fatalf("turn 3 should follow active chunk to a2, got %v", hitUIDs)
	}
	mu.Unlock()

	// 检查会话粘性绑定自动更新收敛到 a2
	if uid, ok := st.lastUID("conv-phase3"); !ok || uid != "a2" {
		t.Fatalf("session binding should converge to a2, got %s ok=%v", uid, ok)
	}
}

// TestHandlerAdaptiveChunkingAndHandover 端到端验证：
// 免费模型在 handler 层达到 60 轮封顶后，自动结束分块并交棒到下一个账号。
func TestHandlerAdaptiveChunkingAndHandover(t *testing.T) {
	st := newBindStore()
	p := pool.New("")
	p.SetSchedulePolicy(pool.SchedulePolicy{
		ChunkQuotaCredits: 25.0,
		AdaptiveChunking:  true,
		TierCheapMaxReqs:  5, // 为了测试效率，将极省档封顶设为 5 轮
	})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	a1 := &auth.Auth{UID: "bot1", AccessToken: "at-bot1", ExpiresAt: 9999999999}
	a2 := &auth.Auth{UID: "bot2", AccessToken: "at-bot2", ExpiresAt: 9999999999}
	p.Add(a1)
	p.Add(a2)

	now := time.Now()
	p.UpdateResourceDetail("bot1", 2000, now.Add(30*24*time.Hour), 0, 0)
	p.UpdateResourceDetail("bot2", 2000, now.Add(30*24*time.Hour), 0, 0)
	p.NoteModelCost("bot1", "hy4-preview", 0.0, 1000)
	p.NoteModelCost("bot2", "hy4-preview", 0.0, 1000)

	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: p.AvailableUIDs,
	})

	var hits []string
	var mu sync.Mutex
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if authz == "Bearer at-bot1" {
			hits = append(hits, "bot1")
		} else if authz == "Bearer at-bot2" {
			hits = append(hits, "bot2")
		}
		return 200, sseOK, true
	})

	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	// 前 5 轮请求：处于 bot1 的活跃分块内，稳定返回 bot1
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"hy4-preview","messages":[{"role":"user","content":"ping"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("req %d failed: %d", i, rec.Code)
		}
	}

	mu.Lock()
	if len(hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(hits))
	}
	for i, h := range hits {
		if h != "bot1" {
			t.Fatalf("hit %d expected bot1, got %s", i, h)
		}
	}
	mu.Unlock()

	// 第 6 轮请求：bot1 已达 5 轮封顶，分块交棒给 bot2
	req6 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hy4-preview","messages":[{"role":"user","content":"ping 6"}]}`))
	rec6 := httptest.NewRecorder()
	h.ServeHTTP(rec6, req6)
	if rec6.Code != 200 {
		t.Fatalf("req 6 failed: %d", rec6.Code)
	}

	mu.Lock()
	if len(hits) != 6 || hits[5] != "bot2" {
		t.Fatalf("req 6 should handover to bot2, got %v", hits)
	}
	mu.Unlock()
}
