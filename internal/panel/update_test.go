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
	for _, s := range manifestSourcesFor(ChannelStable) {
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

// 预览通道：语义化比较（预发布 < 同号正式版；构建变体 -panel 与正式版同序）。
func TestVersionComparePrerelease(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // a 是否比 b 新
	}{
		{"1.11.25-preview.1", "1.11.24", true},
		{"1.11.24", "1.11.24-preview.1", true},
		{"1.11.24-preview.1", "1.11.24", false},
		{"1.11.24-preview.2", "1.11.24-preview.1", true},
		{"1.11.24-preview.1", "1.11.24-preview.1", false},
		{"1.11.24", "1.11.24-panel", false}, // 构建变体后缀 = 同版
		{"1.11.24-panel", "1.11.24", false},
		{"1.11.24-rc.2", "1.11.24-preview.9", true}, // 同号预发布按字典序：preview < rc，故 rc 更新
	}
	for _, c := range cases {
		if got := VersionNewer(c.a, c.b); got != c.want {
			t.Errorf("VersionNewer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// 通道 → 清单文件名 / 源集合。
func TestManifestSourcesByChannel(t *testing.T) {
	t.Setenv("WB2A_UPDATE_BASE", "")
	if got := CurrentChannel(); got != ChannelStable {
		t.Fatalf("默认通道应为 stable，实得 %q", got)
	}
	t.Setenv("WB2A_UPDATE_CHANNEL", "preview")
	if got := CurrentChannel(); got != ChannelPreview {
		t.Fatalf("env 应切到 preview，实得 %q", got)
	}
	t.Setenv("WB2A_UPDATE_CHANNEL", "garbage")
	if got := CurrentChannel(); got != ChannelStable {
		t.Fatalf("非法通道应回落 stable，实得 %q", got)
	}

	var stableHasRelease, previewHasRelease, previewHasPreviewJSON bool
	for _, u := range manifestSourcesFor(ChannelStable) {
		if u == updateGhReleaseManifest {
			stableHasRelease = true
		}
	}
	for _, u := range manifestSourcesFor(ChannelPreview) {
		if u == updateGhReleaseManifest {
			previewHasRelease = true
		}
		if strings.Contains(u, "preview.json") {
			previewHasPreviewJSON = true
		}
	}
	if !stableHasRelease {
		t.Error("stable 通道应含 release/latest 直连源")
	}
	if previewHasRelease {
		t.Error("preview 通道不应使用 releases/latest（它只指向最新非预发布）")
	}
	if !previewHasPreviewJSON {
		t.Error("preview 通道的源里应包含 preview.json")
	}

	// WB2A_UPDATE_BASE 覆盖（隔离测试/内网镜像）必须按通道选文件名。
	t.Setenv("WB2A_UPDATE_BASE", "http://127.0.0.1:18080/")
	src := manifestSourcesFor(ChannelPreview)
	if len(src) == 0 || src[0] != "http://127.0.0.1:18080/preview.json" {
		t.Fatalf("覆盖源应为 preview.json，实得 %v", src[:1])
	}
	srcStable := manifestSourcesFor(ChannelStable)
	if len(srcStable) == 0 || srcStable[0] != "http://127.0.0.1:18080/latest.json" {
		t.Fatalf("stable 覆盖源应为 latest.json，实得 %v", srcStable[:1])
	}
}
