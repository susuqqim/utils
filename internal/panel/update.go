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
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/selfupdate"
)

// 清单文件名：稳定通道 latest.json；预览通道 preview.json（同一仓库 main 分支）。
const (
	ManifestStable  = "latest.json"
	ManifestPreview = "preview.json"
)

// ChannelStable / ChannelPreview 更新通道。
const (
	ChannelStable  = "stable"
	ChannelPreview = "preview"
)

// updateGhReleaseManifest GitHub Release 的 latest 清单地址（会再套代理前缀）。
const updateGhReleaseManifest = "https://github.com/susuqqim/utils/releases/latest/download/latest.json"

// CurrentChannel 返回当前更新通道：WB2A_UPDATE_CHANNEL=preview 才走预览通道，
// 其余（未设/写错）一律按 stable 处理 —— 预览通道必须显式开启，避免误吃预发布。
func CurrentChannel() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WB2A_UPDATE_CHANNEL")), ChannelPreview) {
		return ChannelPreview
	}
	return ChannelStable
}

// manifestNameFor 返回该通道对应的清单文件名。
func manifestNameFor(channel string) string {
	if channel == ChannelPreview {
		return ManifestPreview
	}
	return ManifestStable
}

// manifestSourcesFor 构造该通道的候选源（直连 + Release 直连 + 代理前缀）。
// 预览通道不加 Release 直连：github 的 releases/latest 只指向最新**非预发布**，
// 预发布清单放 main 分支（raw / jsDelivr 可取），Release 里只放资产。
func manifestSourcesFor(channel string) []string {
	name := manifestNameFor(channel)
	out := make([]string, 0, 8)
	for _, tpl := range updateManifestSourceTemplates {
		out = append(out, fmt.Sprintf(tpl, name))
	}
	if channel != ChannelPreview {
		out = append(out, updateGhReleaseManifest)
	}
	// 自更新源覆盖：只认环境变量（面板不可改），供隔离测试/内网镜像。
	if base := strings.TrimSpace(os.Getenv("WB2A_UPDATE_BASE")); base != "" {
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		out = append([]string{base + name}, out...)
	}
	for _, px := range updateManifestProxies {
		for _, u := range out[:len(out)] {
			if strings.HasPrefix(u, "https://github.com/") {
				out = append(out, px+u)
			}
		}
	}
	return out
}

// updateManifest 是 latest.json 的结构（与 scripts/update/*.ps1 一致）。
type updateManifest struct {
	Version string   `json:"version"`
	SHA256  string   `json:"sha256"`
	Size    int64    `json:"size"`
	URLs    []string `json:"urls"`
	// MSI 自更新段（2026-10-22 新增）：缺省时退化为"只提示 + 给下载链接"。
	MSI *selfupdate.MSI `json:"msi,omitempty"`
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

	// MSI 自更新（2026-10-22）：有 msi 段才能"一键更新"，否则面板只给下载链接。
	MSI      *selfupdate.MSI `json:"msi,omitempty"`
	CanApply bool            `json:"can_apply,omitempty"`

	// Channel 当前更新通道（stable / preview）；面板据此显示预览版徽标。
	Channel string `json:"channel,omitempty"`
}

var (
	// B10：清单源必须与 scripts/update/wb2api-update.ps1 **同序同集合**。
	// 此前 Go 侧只有 3 个直连源、没有代理前缀 ⇒ 裸连 GitHub 不通时面板"检查更新"
	// 失败，而同一台机器上的 updater 却能成功（实测：curl raw 失败 / updater OK）。
	// 通道无关的两个直连源（%s 处填清单文件名）。
	updateManifestSourceTemplates = []string{
		"https://raw.githubusercontent.com/susuqqim/utils/main/%s",
		"https://cdn.jsdelivr.net/gh/susuqqim/utils@main/%s",
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

// versionNewer 判断 a 是否比 b 新。
//
// 规则（语义化版本）：
//  1. 先按点比较数值段（1.11.24 < 1.11.25）；
//  2. 数值段相同再看预发布后缀：**带后缀的比不带后缀的旧**（1.11.24-preview.1 < 1.11.24）；
//  3. 两边都带后缀时逐段比较（preview.2 > preview.1，数字段按数值比）。
//
// 为什么要这么细：预览通道（preview.json）会给出 1.11.25-preview.1 这类版本号，
// 而稳定通道给 1.11.24；只比前导数字会把 1.11.24-preview.1 与 1.11.24 判成相等，
// 导致稳定版用户被误提示降级、或预览用户升不到同号稳定版。
func versionNewer(a, b string) bool { return compareVersion(a, b) > 0 }

// compareVersion 返回 -1/0/1。
func compareVersion(a, b string) int {
	ca, pa := splitPre(a)
	cb, pb := splitPre(b)
	va, vb := strings.Split(ca, "."), strings.Split(cb, ".")
	for i := 0; i < len(va) || i < len(vb); i++ {
		var na, nb int
		if i < len(va) {
			na = leadingDigits(va[i])
		}
		if i < len(vb) {
			nb = leadingDigits(vb[i])
		}
		if na != nb {
			if na > nb {
				return 1
			}
			return -1
		}
	}
	switch {
	case pa == "" && pb == "":
		return 0
	case pa == "":
		// a 无后缀；b 若是 -panel/-beta 这类**构建变体**后缀，视为同版；
		// 只有 -preview.N/-rc.N 这类才是「比正式版旧」的预发布。
		if !isPrereleaseSuffix(pb) {
			return 0
		}
		return 1
	case pb == "":
		if !isPrereleaseSuffix(pa) {
			return 0
		}
		return -1
	}
	ap, bp := isPrereleaseSuffix(pa), isPrereleaseSuffix(pb)
	switch {
	case !ap && !bp:
		return 0
	case ap && !bp:
		return -1
	case !ap && bp:
		return 1
	}
	return comparePre(pa, pb)
}

// isPrereleaseSuffix 判断后缀是否为**预发布**（比同号正式版旧）：
// preview / rc / alpha / beta。其它后缀（本项目惯用的 -panel）是构建变体，与正式版同序。
func isPrereleaseSuffix(pre string) bool {
	l := strings.ToLower(pre)
	for _, k := range []string{"preview", "rc", "alpha", "beta"} {
		if strings.HasPrefix(l, k) {
			return true
		}
	}
	return false
}

// splitPre 把 "1.11.24-preview.1" 拆成 ("1.11.24", "preview.1")。
func splitPre(v string) (core, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// comparePre 比较两个预发布串（点分段：数字段按数值，其余按字典序）。
func comparePre(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		if i >= len(pa) {
			return -1
		}
		if i >= len(pb) {
			return 1
		}
		na, aNum := parseAllDigits(pa[i])
		nb, bNum := parseAllDigits(pb[i])
		if aNum && bNum {
			if na != nb {
				if na > nb {
					return 1
				}
				return -1
			}
			continue
		}
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// parseAllDigits 判断整段是否纯数字。
func parseAllDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// fetchUpdateManifest 依次尝试各源；全部失败返回错误。
func fetchUpdateManifest(ctx context.Context) (updateManifest, string, error) {
	// 按通道生成候选源（stable=latest.json / preview=preview.json）。
	sources := manifestSourcesFor(CurrentChannel())

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

	res := updateCheckResult{Current: current, CheckedAt: time.Now().Format(time.RFC3339), Channel: CurrentChannel()}
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
		res.MSI = m.MSI
		res.CanApply = m.MSI != nil
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

// ErrUpdateInFlight 由 main 在"有在途请求"时返回：面板把它映射成 409，
// 提示用户稍后再更新（更新会重启网关，不能打断正在跑的对话）。
var ErrUpdateInFlight = errors.New("有请求正在处理中，请稍后再更新（会重启网关）")

// FetchForApply 为"立即更新"**强制**拉一次最新清单（不走 5 分钟缓存），
// 返回 msi 段与目标版本 —— 避免用户点了更新却拿到缓存里的旧包地址。
func FetchForApply(ctx context.Context) (selfupdate.MSI, string, error) {
	m, _, err := fetchUpdateManifest(ctx)
	if err != nil {
		return selfupdate.MSI{}, "", err
	}
	if m.MSI == nil {
		return selfupdate.MSI{}, m.Version, selfupdate.ErrNoMSI
	}
	return *m.MSI, m.Version, nil
}

// handleUpdateProgress：GET /panel/api/update/progress
func (p *Panel) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	if p.cfg.UpdateProgress == nil {
		writeJSON(w, http.StatusOK, selfupdate.State{Stage: selfupdate.StageIdle})
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.UpdateProgress())
}

// handleUpdateApply：POST /panel/api/update/apply
//
// 语义：成功即代表**已派生 msiexec**，网关随后退出，由 MSI 的 CaStartApp 拉起新版；
// 面板此时会断连，重连后读 /update/progress 续显（状态已落盘）。
func (p *Panel) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ApplyUpdate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"code": "not_applicable", "error": "当前形态不支持程序内自更新（Docker 请拉新镜像；非 Windows 平台不支持 MSI）",
			"message": "当前形态不支持程序内自更新（Docker 请拉新镜像；非 Windows 平台不支持 MSI）",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	st, err := p.cfg.ApplyUpdate(ctx)
	if err != nil {
		code, raw := http.StatusBadRequest, "update_failed"
		switch {
		case errors.Is(err, selfupdate.ErrBusy), errors.Is(err, ErrUpdateInFlight):
			code, raw = http.StatusConflict, "update_busy"
		case errors.Is(err, selfupdate.ErrNoMSI), errors.Is(err, selfupdate.ErrNotRunnable):
			code, raw = http.StatusNotImplemented, "not_applicable"
		case errors.Is(err, selfupdate.ErrScheme), errors.Is(err, selfupdate.ErrBadMSI):
			code, raw = http.StatusBadGateway, "bad_manifest"
		}
		writeJSON(w, code, map[string]any{"code": raw, "message": err.Error(), "error": err.Error(), "state": st})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": st.To, "state": st})
}

// VersionNewer 导出给 main 装配用（与面板徽标/更新检查同一套版本语义）。
func VersionNewer(a, b string) bool { return versionNewer(a, b) }
