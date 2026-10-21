package panel

// 更新检查（2026-10 重设计，用户需求）：
//   · 控制台左下角「检查更新」按钮 → GET /panel/api/update/check（本文件）
//   · 安装版（MSI）由网关进程自己通过 Windows 通知（托盘气泡）提示，不弹 cmd 窗口
//
// 关键约束：**全部在 Go 进程内用 net/http 完成**，不 spawn powershell/cmd ⇒ 不会闪出控制台窗口。
// 清单来源与安装包更新器保持一致：GitHub raw（susuqqim/utils）→ jsDelivr → Release 直连。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// updateGhReleaseManifest GitHub Release 的 latest 清单地址（会再套代理前缀）。
const updateGhReleaseManifest = "https://github.com/susuqqim/utils/releases/latest/download/latest.json"

// updateManifest 是 latest.json 的结构（与 scripts/update/*.ps1 一致）。
type updateManifest struct {
	Version string   `json:"version"`
	SHA256  string   `json:"sha256"`
	Size    int64    `json:"size"`
	URLs    []string `json:"urls"`
}

// updateCheckResult 是给面板的响应。
type updateCheckResult struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	URL       string `json:"url,omitempty"` // 新版本发布页/下载地址
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
	CheckedAt string `json:"checked_at"`
	Error     string `json:"error,omitempty"` // 取清单失败的原因（面板显示"检查失败"）
}

var (
	// B10：清单源必须与 scripts/update/wb2api-update.ps1 **同序同集合**。
	// 此前 Go 侧只有 3 个直连源、没有代理前缀 ⇒ 裸连 GitHub 不通时面板"检查更新"
	// 失败，而同一台机器上的 updater 却能成功（实测：curl raw 失败 / updater OK）。
	updateManifestSources = []string{
		"https://raw.githubusercontent.com/susuqqim/utils/main/latest.json",
		"https://cdn.jsdelivr.net/gh/susuqqim/utils@main/latest.json",
		updateGhReleaseManifest,
	}
	// 与 PowerShell 侧相同的三个代理前缀（pp 前缀 + 完整 release URL）
	updateManifestProxies = []string{
		"https://ghproxy.net/",
		"https://gh-proxy.com/",
		"https://mirror.ghproxy.com/",
	}
	updateHTTPClient = &http.Client{Timeout: 8 * time.Second}

	updateCacheMu sync.Mutex
	updateCache   updateCheckResult
	updateCacheAt time.Time
)

// leadingDigits 取字符串开头连续数字；无数字返回 0。
// 关键：本项目版本号形如 "1.11.9-panel"，后缀（-panel/-beta）不属于数值版本，
// 否则 versionNewer("1.11.9","1.11.9-panel") 会误判为"有更新"（QA 实测）。
func leadingDigits(s string) int {
	n := 0
	got := false
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		got = true
	}
	if !got {
		return 0
	}
	return n
}

// versionNewer 判断 a 是否比 b 新（按点分段、比较每段前导数字）。
func versionNewer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na = leadingDigits(pa[i])
		}
		if i < len(pb) {
			nb = leadingDigits(pb[i])
		}
		if na != nb {
			return na > nb
		}
	}
	return false
}

// fetchUpdateManifest 依次尝试各源；全部失败返回错误。
func fetchUpdateManifest(ctx context.Context) (updateManifest, string, error) {
	// 直连源 + 代理源（代理只对 GitHub Release URL 有意义）
	sources := append([]string{}, updateManifestSources...)
	for _, px := range updateManifestProxies {
		sources = append(sources, px+updateGhReleaseManifest)
	}
	var lastErr error
	for _, u := range sources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := updateHTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body := json.NewDecoder(resp.Body)
		var m updateManifest
		err = body.Decode(&m)
		resp.Body.Close()
		if err != nil || m.Version == "" {
			if err == nil {
				err = errUpdateManifestEmpty
			}
			lastErr = err
			continue
		}
		return m, u, nil
	}
	if lastErr == nil {
		lastErr = errUpdateManifestEmpty
	}
	return updateManifest{}, "", lastErr
}

// CheckUpdateFor 供非面板入口（托盘启动自检）使用：不依赖 Panel 实例。
func CheckUpdateFor(ctx context.Context, current string, force bool) updateCheckResult {
	updateCacheMu.Lock()
	// B9：缓存必须绑定 current 版本。旧实现只看时间窗 ⇒ 刚更新完的 5 分钟内，
	// 面板仍用旧 current 的结果显示"可更新"（用户看到提示却已经是最新）。
	if !force && time.Since(updateCacheAt) < 5*time.Minute &&
		updateCache.CheckedAt != "" && updateCache.Current == current {
		cached := updateCache
		updateCacheMu.Unlock()
		return cached
	}
	updateCacheMu.Unlock()

	res := updateCheckResult{Current: current, CheckedAt: time.Now().Format(time.RFC3339)}
	m, _, err := fetchUpdateManifest(ctx)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Latest = m.Version
		res.SHA256 = m.SHA256
		res.Size = m.Size
		res.Available = versionNewer(m.Version, current)
		if len(m.URLs) > 0 {
			res.URL = m.URLs[0]
		}
	}
	updateCacheMu.Lock()
	updateCache, updateCacheAt = res, time.Now()
	updateCacheMu.Unlock()
	return res
}

// CheckUpdate 是面板实例的入口（用其 Version 作为当前版本）。
func (p *Panel) CheckUpdate(ctx context.Context, force bool) updateCheckResult {
	return CheckUpdateFor(ctx, p.cfg.Version, force)
}

// handleUpdateCheck：GET /panel/api/update/check[?force=1]
func (p *Panel) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, p.CheckUpdate(ctx, force))
}

var errUpdateManifestEmpty = errors.New("update manifest: 清单为空或缺少 version")
