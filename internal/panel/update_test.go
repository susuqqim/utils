package panel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 面板徽标是否显示"可更新"完全依赖它；必须按数字比较，不能按字符串。
func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.11.9", "1.11.8", true},
		{"1.11.8", "1.11.9", false},
		{"1.11.9", "1.11.9", false},
		{"1.12.0", "1.11.99", true},
		{"2.0.0", "1.99.99", true},
		{"1.11.10", "1.11.9", true},
		{"1.11.9", "1.11.9-panel", false}, // 后缀不属于数值版本（QA 实测会误报）
		{"1.11.10", "1.11.9-panel", true},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// B9 回归：更新检查缓存必须绑定 current 版本。
// 旧实现只看时间窗 ⇒ 刚更新完的 5 分钟内，面板仍用旧 current 的结果显示"可更新"。
func TestUpdateCacheBoundToCurrent(t *testing.T) {
	// 直接摆弄包级缓存：模拟"刚为新版本检查过"的状态。
	updateCacheMu.Lock()
	updateCache = updateCheckResult{
		Current: "1.11.14", Latest: "1.11.15",
		Available: true, CheckedAt: time.Now().Format(time.RFC3339),
	}
	updateCacheAt = time.Now()
	updateCacheMu.Unlock()

	// 同版本：应命中缓存（不联网）。用已取消的 ctx 保证"若真的去拉清单就一定失败"。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := CheckUpdateFor(ctx, "1.11.14", false)
	if got.Current != "1.11.14" || !got.Available {
		t.Fatalf("同版本应命中缓存，实得 %+v", got)
	}

	// 不同版本（比如刚装完新版）：不得复用旧结果，必须重新检查。
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	got2 := CheckUpdateFor(ctx2, "1.11.15", false)
	if got2.Current != "1.11.15" {
		t.Fatalf("current 变了必须重新检查（缓存未绑定版本），实得 %+v", got2)
	}
	if got2.Available {
		t.Fatalf("重新检查失败时不应声称可更新，实得 %+v", got2)
	}

	// 清理，避免污染其它用例
	updateCacheMu.Lock()
	updateCache = updateCheckResult{}
	updateCacheAt = time.Time{}
	updateCacheMu.Unlock()
}

// B10 回归：Go 侧清单源必须含代理 fallback（与 PowerShell updater 同集合）。
func TestUpdateManifestSourcesIncludeProxies(t *testing.T) {
	if len(updateManifestProxies) == 0 {
		t.Fatal("代理前缀列表为空：裸连 GitHub 不通时面板检查更新会失败")
	}
	var haveRelease bool
	for _, s := range updateManifestSources {
		if s == updateGhReleaseManifest {
			haveRelease = true
		}
	}
	if !haveRelease {
		t.Fatalf("直连源里缺少 release 清单 %q", updateGhReleaseManifest)
	}
	for _, px := range updateManifestProxies {
		if !strings.HasPrefix(px, "https://") || !strings.HasSuffix(px, "/") {
			t.Errorf("代理前缀格式可疑: %q（需要 https:// 且以 / 结尾，才能直接拼完整 URL）", px)
		}
	}
}
