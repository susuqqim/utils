package upstream

import "testing"

// 真实命中在 prompt_tokens_details.cached_tokens，同时存在 0 值别名时取最大值。
func TestUsageCacheHitTokens(t *testing.T) {
	u := map[string]any{
		"prompt_tokens":           float64(1000),
		"cached_tokens":           float64(0),
		"cache_read_input_tokens": float64(0),
		"prompt_tokens_details":   map[string]any{"cached_tokens": float64(640)},
	}
	n, ok := UsageCacheHitTokens(u)
	if !ok || n != 640 {
		t.Fatalf("got %d,%v want 640,true", n, ok)
	}
	if _, ok := UsageCacheHitTokens(map[string]any{"cached_tokens": float64(0)}); ok {
		t.Fatal("零值别名不应计为命中")
	}
}
