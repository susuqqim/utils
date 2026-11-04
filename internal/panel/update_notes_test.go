package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// releaseNotesURL 只认 wb2api-core.exe 的同级资产；其它一律不猜。
func TestReleaseNotesURL(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"https://github.com/o/r/releases/download/v1/wb2api-core.exe"}, "https://github.com/o/r/releases/download/v1/RELEASE-NOTES.md"},
		{[]string{"https://gh-proxy.com/https://github.com/o/r/releases/download/v1/wb2api-core.exe"}, "https://gh-proxy.com/https://github.com/o/r/releases/download/v1/RELEASE-NOTES.md"},
		{[]string{"https://example.com/download/x.exe"}, ""},
		{nil, ""},
	}
	for i, c := range cases {
		if got := releaseNotesURL(c.in); got != c.want {
			t.Errorf("case%d got=%q want=%q", i, got, c.want)
		}
	}
}

// fetchReleaseNotes：成功、404、超大 body 截断、空 URL 四种形态。
func TestFetchReleaseNotes(t *testing.T) {
	huge := strings.Repeat("A", maxNotesBytes+500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.md":
			io.WriteString(w, "  ## 更新内容\n- 修复 A\n  ")
		case "/big.md":
			io.WriteString(w, huge)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if got := fetchReleaseNotes(context.Background(), srv.Client(), srv.URL+"/ok.md"); got != "## 更新内容\n- 修复 A" {
		t.Errorf("ok got=%q", got)
	}
	if got := fetchReleaseNotes(context.Background(), srv.Client(), srv.URL+"/missing.md"); got != "" {
		t.Errorf("404 应返回空串，got len=%d", len(got))
	}
	if got := fetchReleaseNotes(context.Background(), srv.Client(), srv.URL+"/big.md"); len(got) != maxNotesBytes {
		t.Errorf("超大说明应截断到 %d，got %d", maxNotesBytes, len(got))
	}
	if got := fetchReleaseNotes(context.Background(), srv.Client(), ""); got != "" {
		t.Errorf("空 URL 应返回空串，got=%q", got)
	}
}

// 清单带 notes 时直接用，且不再发请求（用一个必定 500 的服务器证明没被调用）。
func TestFillUpdateNotesPrefersManifestNotes(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()

	oldDirect, oldProxy := updateDirectClient, updateProxyClient
	updateDirectClient, updateProxyClient = srv.Client(), srv.Client()
	defer func() { updateDirectClient, updateProxyClient = oldDirect, oldProxy }()

	res := updateCheckResult{Available: true}
	m := updateManifest{Version: "9.9.9", Notes: "  inline notes  ", URLs: []string{srv.URL + "/wb2api-core.exe"}}
	fillUpdateNotes(context.Background(), &res, m)
	if res.Notes != "inline notes" {
		t.Fatalf("notes=%q", res.Notes)
	}
	if called {
		t.Fatal("清单已有 notes 时不应再发请求")
	}
}

// 清单无 notes 时回退拉同 Release 的 RELEASE-NOTES.md。
func TestFillUpdateNotesFallsBackToAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wb2api-core.exe" {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/"+releaseNotesAsset) {
			io.WriteString(w, "## 1.11.30\n- 新增更新内容展示")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	oldDirect, oldProxy := updateDirectClient, updateProxyClient
	updateDirectClient, updateProxyClient = srv.Client(), srv.Client()
	defer func() { updateDirectClient, updateProxyClient = oldDirect, oldProxy }()

	res := updateCheckResult{Available: true}
	fillUpdateNotes(context.Background(), &res, updateManifest{Version: "9.9.9", URLs: []string{srv.URL + "/wb2api-core.exe"}})
	if !strings.Contains(res.Notes, "新增更新内容展示") {
		t.Fatalf("fallback notes=%q", res.Notes)
	}
}

// 已经是最新（Available=false）时不抓说明，避免无谓请求。
func TestFillUpdateNotesSkipsWhenUpToDate(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	oldDirect, oldProxy := updateDirectClient, updateProxyClient
	updateDirectClient, updateProxyClient = srv.Client(), srv.Client()
	defer func() { updateDirectClient, updateProxyClient = oldDirect, oldProxy }()

	res := updateCheckResult{Available: false}
	fillUpdateNotes(context.Background(), &res, updateManifest{URLs: []string{srv.URL + "/wb2api-core.exe"}})
	if called || res.Notes != "" {
		t.Fatalf("called=%v notes=%q", called, res.Notes)
	}
}

// 清单 JSON 的 notes 字段必须能解析（发版侧会往 latest.json 里写）。
func TestManifestDecodesNotes(t *testing.T) {
	var m updateManifest
	if err := json.Unmarshal([]byte(`{"version":"1.2.3","notes":"hello"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Notes != "hello" {
		t.Fatalf("notes=%q", m.Notes)
	}
}

// 面板响应必须把 notes 序列化出去（前端读 d.notes）；漏 tag 会导致功能静默失效。
func TestUpdateCheckResultJSONHasNotes(t *testing.T) {
	b, err := json.Marshal(updateCheckResult{Notes: "- 修复 A"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"notes":`) {
		t.Fatalf("响应缺少 notes 字段: %s", b)
	}
}

// 端到端：清单源指向本地 httptest（WB2A_UPDATE_BASE），验证 CheckUpdateFor 真的把说明带回响应。
// 这条覆盖「取清单 → 发现新版 → 回退拉 RELEASE-NOTES.md → 放进结果」的整条链路。
func TestCheckUpdateForCarriesNotesEndToEnd(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			io.WriteString(w, `{"version":"9.9.9","sha256":"abc","size":1,"urls":["`+srv.URL+`/wb2api-core.exe"]}`)
		case strings.HasSuffix(r.URL.Path, "/"+releaseNotesAsset):
			io.WriteString(w, "## 9.9.9\n- 新增更新内容展示")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// 用注入点把候选源收敛成本地一条：既不触网，也不会等满 4s 预算。
	oldSources := updateSourcesOverride
	updateSourcesOverride = []string{srv.URL + "/latest.json"}
	defer func() { updateSourcesOverride = oldSources }()
	oldDirect, oldProxy := updateDirectClient, updateProxyClient
	updateDirectClient, updateProxyClient = srv.Client(), srv.Client()
	defer func() { updateDirectClient, updateProxyClient = oldDirect, oldProxy }()
	// 快照并清空包级缓存：本用例会写入 lastGood，若不还原会污染后续
	// TestUpdateCacheBoundToCurrent（它依赖"检查失败时不得声称可更新"）。
	updateCacheMu.Lock()
	oldCache, oldCacheAt := updateCache, updateCacheAt
	oldGood, oldGoodAt := updateLastGood, updateLastGoodAt
	updateCache, updateCacheAt = updateCheckResult{}, time.Time{}
	updateLastGood, updateLastGoodAt = updateCheckResult{}, time.Time{}
	updateCacheMu.Unlock()
	t.Cleanup(func() {
		updateCacheMu.Lock()
		updateCache, updateCacheAt = oldCache, oldCacheAt
		updateLastGood, updateLastGoodAt = oldGood, oldGoodAt
		updateCacheMu.Unlock()
	})

	res := CheckUpdateFor(context.Background(), "1.0.0", true)
	if !res.Available {
		t.Fatalf("应判定有新版本: %+v", res)
	}
	if !strings.Contains(res.Notes, "新增更新内容展示") {
		t.Fatalf("notes 未带回: %q (err=%q)", res.Notes, res.Error)
	}
}
