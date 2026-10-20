package server

import (
	"errors"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 回归（2026-10）：/v1/models 的动态目录拉取曾无界阻塞（FetchModels 内 chan receive），
// 网络卡住时 handler 永久挂起。fetchModelsBounded 必须有上界，超时返回错误而不是阻塞。
func TestFetchModelsBoundedTimesOut(t *testing.T) {
	old := modelsFetchTimeout
	modelsFetchTimeout = 50 * time.Millisecond
	defer func() { modelsFetchTimeout = old }()

	block := make(chan struct{})
	defer close(block) // 放行被卡的 goroutine，避免测试结束时它仍在 sleep
	fn := func(_ *auth.Auth) ([]upstream.ModelInfo, error) { <-block; return nil, nil }

	start := time.Now()
	infos, err := fetchModelsBounded(nil, fn)
	el := time.Since(start)
	if el > time.Second {
		t.Fatalf("超时上界失效：耗时 %v", el)
	}
	if err == nil {
		t.Fatalf("超时应返回错误，实际 err=nil infos=%v", infos)
	}
	if infos != nil {
		t.Fatalf("超时应返回 nil infos，实际 %v", infos)
	}
}

// 正常路径：fn 立即返回时原样传递结果。
func TestFetchModelsBoundedPassesResult(t *testing.T) {
	old := modelsFetchTimeout
	modelsFetchTimeout = time.Second
	defer func() { modelsFetchTimeout = old }()

	want := []upstream.ModelInfo{{ID: "x"}}
	infos, err := fetchModelsBounded(nil, func(_ *auth.Auth) ([]upstream.ModelInfo, error) {
		return want, nil
	})
	if err != nil || len(infos) != 1 || infos[0].ID != "x" {
		t.Fatalf("透传失败: infos=%v err=%v", infos, err)
	}
	_, err = fetchModelsBounded(nil, func(_ *auth.Auth) ([]upstream.ModelInfo, error) {
		return nil, errors.New("boom")
	})
	if err == nil {
		t.Fatal("应透传错误")
	}
}
