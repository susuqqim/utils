package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 更新检查在 HTTP 层的契约测试（seam = GET /panel/api/update/check 的 JSON 响应）。
// 只断言调用方看得见的东西：latest / sha256 / notes / available。
// 不 mock 内部函数、不测私有实现细节 —— 实现改了这些用例应当仍然成立。

// fakeRepo 模拟一个清单源 + 同 Release 的说明资产。
// delay 用来制造「旧清单先返回、新清单后返回」的竞速。
type fakeRepo struct {
	srv   *httptest.Server
	sha   string
	notes string
	asset string // RELEASE-NOTES.md 内容；空串表示 404
	delay time.Duration
}

func newFakeRepo(t *testing.T, sha, notes, asset string, delay time.Duration) *fakeRepo {
	t.Helper()
	f := &fakeRepo{sha: sha, notes: notes, asset: asset, delay: delay}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			m := map[string]any{
				"version":   "9.9.9",
				"sha256":    f.sha,
				"size":      int64(1234),
				"generated": "2026-11-04T15:40:00",
				"urls":      []string{f.srv.URL + "/wb2api-core.exe"},
			}
			if f.notes != "" {
				m["notes"] = f.notes
			}
			json.NewEncoder(w).Encode(m)
		case strings.HasSuffix(r.URL.Path, "/"+releaseNotesAsset):
			if f.asset == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, f.asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// checkOnce 走真实端点，返回解码后的响应。
func checkOnce(t *testing.T, current string) map[string]any {
	t.Helper()
	p := New(Config{Version: current, APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/update/check?force=1", nil)
	p.handleUpdateCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("端点应返回 200，实得 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

// isolateUpdateState 把包级缓存与源列表钉在本用例内，结束后还原。
func isolateUpdateState(t *testing.T, sources []string) {
	t.Helper()
	oldSources := updateSourcesOverride
	updateSourcesOverride = sources
	oldDirect, oldProxy := updateDirectClient, updateProxyClient
	fast := &http.Client{Timeout: 3 * time.Second}
	updateDirectClient, updateProxyClient = fast, fast
	updateCacheMu.Lock()
	oldCache, oldAt := updateCache, updateCacheAt
	oldGood, oldGoodAt := updateLastGood, updateLastGoodAt
	updateCache, updateCacheAt = updateCheckResult{}, time.Time{}
	updateLastGood, updateLastGoodAt = updateCheckResult{}, time.Time{}
	updateCacheMu.Unlock()
	t.Cleanup(func() {
		updateSourcesOverride = oldSources
		updateDirectClient, updateProxyClient = oldDirect, oldProxy
		updateCacheMu.Lock()
		updateCache, updateCacheAt = oldCache, oldAt
		updateLastGood, updateLastGoodAt = oldGood, oldGoodAt
		updateCacheMu.Unlock()
	})
}

// 行为：发版后旧清单（无 notes、sha 是旧包）可能先返回；端点必须给调用方**新的** sha。
// 这条对应 2026-11-04 第三轮审计 F1 的真实故障：旧清单抢先返回 → 立即更新校验失败。
func TestUpdateCheckPrefersFreshManifestOnSameVersion(t *testing.T) {
	stale := newFakeRepo(t, "oldsha", "", "", 0)
	fresh := newFakeRepo(t, "newsha", "- 新增更新内容展示", "", 80*time.Millisecond)
	isolateUpdateState(t, []string{stale.srv.URL + "/latest.json", fresh.srv.URL + "/latest.json"})

	res := checkOnce(t, "1.0.0")
	if got := res["sha256"]; got != "newsha" {
		t.Fatalf("同版本竞速必须返回新清单的 sha256，实得 %v（旧清单抢赢了）", got)
	}
	if notes, _ := res["notes"].(string); !strings.Contains(notes, "更新内容") {
		t.Fatalf("新清单的 notes 应随响应返回，实得 %q", notes)
	}
}

// 行为：清单里自带 notes 时，端点直接返回它，不再去拉说明资产（可由资产 404 证明）。
func TestUpdateCheckUsesManifestNotesWithoutFetchingAsset(t *testing.T) {
	repo := newFakeRepo(t, "newsha", "清单内联说明", "", 0)
	isolateUpdateState(t, []string{repo.srv.URL + "/latest.json"})

	res := checkOnce(t, "1.0.0")
	if got, _ := res["notes"].(string); got != "清单内联说明" {
		t.Fatalf("notes=%q want 清单内联说明", got)
	}
}

// 行为：清单没有 notes（旧版清单）时，端点回退拉同 Release 的说明资产。
func TestUpdateCheckFallsBackToNotesAsset(t *testing.T) {
	repo := newFakeRepo(t, "newsha", "", "## 9.9.9\n- 回退说明", 0)
	isolateUpdateState(t, []string{repo.srv.URL + "/latest.json"})

	res := checkOnce(t, "1.0.0")
	if got, _ := res["notes"].(string); !strings.Contains(got, "回退说明") {
		t.Fatalf("notes 未回退到 RELEASE-NOTES.md，实得 %q", got)
	}
}

// 行为：说明读不到（404）不能让检查失败 —— 仍要返回 200/available，只是没有说明。
func TestUpdateCheckDegradesWhenNotesUnavailable(t *testing.T) {
	repo := newFakeRepo(t, "newsha", "", "", 0) // asset 空 ⇒ 404
	isolateUpdateState(t, []string{repo.srv.URL + "/latest.json"})

	res := checkOnce(t, "1.0.0")
	if avail, _ := res["available"].(bool); !avail {
		t.Fatalf("说明缺失不应影响 available，响应=%v", res)
	}
	if got, _ := res["notes"].(string); got != "" {
		t.Fatalf("说明缺失时 notes 应为空，实得 %q", got)
	}
}

// 行为：超大说明被截断到 64KB，响应不会被异常源打爆。
func TestUpdateCheckTruncatesHugeNotes(t *testing.T) {
	huge := strings.Repeat("A", maxNotesBytes+500)
	repo := newFakeRepo(t, "newsha", "", huge, 0)
	isolateUpdateState(t, []string{repo.srv.URL + "/latest.json"})

	res := checkOnce(t, "1.0.0")
	got, _ := res["notes"].(string)
	if len(got) != maxNotesBytes {
		t.Fatalf("notes 长度=%d want %d（应截断）", len(got), maxNotesBytes)
	}
}
