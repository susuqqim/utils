// Package selfupdate 实现"程序内自更新（MSI 驱动）"的核心机制。
//
// 为什么走 MSI（方案 C，2026-10-22 决策）：
//   - 改文件的是 msiexec.exe（微软签名），不是我们未签名的二进制自我替换；
//   - 失败有 Windows Installer 原生事务回滚，不用自己写 supervisor；
//   - 火绒看到的是"装了个安装包"，而不是"无签名程序替换自身"。
//
// 本包只负责机制：校验 → 下载 → SHA256 → 落盘状态 → 派生 msiexec。
// 版本判定/清单来源/是否允许更新由调用方（panel/main）决定，便于单测注入。
package selfupdate

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 状态机取值。写进 update-state.json，也是面板进度条的取值来源。
const (
	StageIdle     = "idle"
	StageDownload = "download"
	StageVerify   = "verify"
	StageInstall  = "install" // 已派生 msiexec，等待重启后对账
	StageDone     = "done"
	StageFailed   = "failed"
)

var (
	ErrBusy          = errors.New("selfupdate: 已有更新在进行中")
	ErrNoMSI         = errors.New("selfupdate: 清单缺少 msi 段（该版本不支持自更新）")
	ErrBadMSI        = errors.New("selfupdate: msi 段字段不完整（需要 url/sha256/name）")
	ErrScheme        = errors.New("selfupdate: 更新地址必须是 https")
	ErrChecksum      = errors.New("selfupdate: SHA256 校验失败")
	ErrNoSpace       = errors.New("selfupdate: 磁盘剩余空间不足")
	ErrNotRunnable   = errors.New("selfupdate: 当前平台不支持派生安装程序")
	ErrInstallFailed = errors.New("selfupdate: 安装程序立即失败")
)

// Waiter 等待安装程序退出并返回退出码（必须幂等：多次调用返回同一结果）。
// 为什么要等：msiexec 对坏包/被策略拦截会在 1 秒内以非 0 退出，
// 此时若网关已退出，就没人把服务拉起来（2026-10-22 实测 1620）。
type Waiter interface {
	Wait() int
}

// MSI 是 latest.json 的 `msi` 段。
type MSI struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// State 是落盘的更新状态（面板据此显示进度；进程重启后仍可读）。
type State struct {
	Stage     string    `json:"stage"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Percent   int       `json:"percent"`
	MSIPath   string    `json:"msi_path,omitempty"`
	LogPath   string    `json:"log_path,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Options 是 Applier 的依赖注入点（测试全部替换，生产用 main 装配）。
type Options struct {
	DataDir    string // update-state.json / update.lock 落在这里
	InstallDir string // msi 下载目录与安装目录（exe 所在目录）

	Now      func() time.Time
	HTTP     *http.Client
	Spawn    func(msiPath, logPath string) (Waiter, error) // 派生安装程序；nil = 平台不支持
	DiskFree func(dir string) (int64, error)

	MinFree   int64         // 额外保留空间（默认 256MB）
	LockStale time.Duration // 超过该时长的锁视为陈旧（默认 30min）
	AllowHTTP bool          // 仅供测试：允许 http 源
	// EarlyExitWindow 派生安装程序后观察"是否立刻失败"的窗口（默认 1.2s）。
	// 覆盖：坏包 1620、被 HIPS/策略拦截、权限不足 —— 这些都在 1s 内返回非 0。
	EarlyExitWindow time.Duration
	// Mirrors 下载镜像前缀（如 https://ghproxy.net/），在主地址失败/校验不符时依次尝试。
	Mirrors []string
	// DownloadAttempts 每个地址的最大尝试次数（含续传重试），默认 3。
	DownloadAttempts int
	ProgressEvery    int // 每上涨多少个百分点落一次状态（默认 10）
}

// Applier 是自更新的执行器。并发安全：Apply 之间用 busy 标志互斥。
type Applier struct {
	opt Options

	busy atomic.Bool
	mu   sync.Mutex
	st   State
}

// New 构造 Applier 并填充默认值。
func New(opt Options) *Applier {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if opt.DiskFree == nil {
		opt.DiskFree = diskFree
	}
	if opt.MinFree <= 0 {
		opt.MinFree = 256 << 20
	}
	if opt.LockStale <= 0 {
		opt.LockStale = 30 * time.Minute
	}
	if opt.ProgressEvery <= 0 {
		opt.ProgressEvery = 10
	}
	if opt.EarlyExitWindow <= 0 {
		opt.EarlyExitWindow = 1200 * time.Millisecond
	}
	if opt.DownloadAttempts <= 0 {
		opt.DownloadAttempts = 3
	}
	return &Applier{opt: opt, st: State{Stage: StageIdle, Percent: 0}}
}

// StatePath 返回状态文件路径。
func (a *Applier) StatePath() string { return filepath.Join(a.opt.DataDir, "update-state.json") }

func (a *Applier) lockPath() string { return filepath.Join(a.opt.DataDir, "update.lock") }

// State 返回内存中的最新状态。
func (a *Applier) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

func (a *Applier) setState(s State) {
	s.UpdatedAt = a.opt.Now()
	a.mu.Lock()
	a.st = s
	a.mu.Unlock()
	_ = a.persist(s)
}

// persist 原子写状态文件（tmp + rename），失败只记录不阻断（状态是观测用的）。
func (a *Applier) persist(s State) error {
	if a.opt.DataDir == "" {
		return nil
	}
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.StatePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.StatePath())
}

// readStateFile 读取磁盘上的状态；不存在/损坏时返回 idle。
func (a *Applier) readStateFile() State {
	b, err := os.ReadFile(a.StatePath())
	if err != nil {
		return State{Stage: StageIdle}
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.Stage == "" {
		return State{Stage: StageIdle}
	}
	return s
}

// Reconcile 在进程启动时调用：把"上次安装已派生但没对账"的状态收尾。
//
//	stage=install 且 current==To → done（安装成功，新版本已在跑）
//	stage=install 且 current!=To → failed（MSI 失败并回滚，或版本未变）
//	其他非终态（download/verify）→ failed（上次更新被中断）
//
// 同时清理上次残留的锁文件（进程已重启，锁必然失效）。
// versionEqual 按点分段比较前导数字，忽略 -panel/-beta 之类后缀。
// 为什么需要：进程内的 appVersion 是 "1.11.20-panel"，而 latest.json 的 version 是 "1.11.20"，
// 直接字符串比较会把"更新成功"误判成"版本没变 → 回滚"。
func versionEqual(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na = leadingNum(pa[i])
		}
		if i < len(pb) {
			nb = leadingNum(pb[i])
		}
		if na != nb {
			return false
		}
	}
	return true
}

// leadingNum 取字符串开头的连续数字（"20-panel" → 20）。
func leadingNum(s string) int {
	n, got := 0, false
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

func (a *Applier) Reconcile(current string) State {
	s := a.readStateFile()
	switch s.Stage {
	case StageInstall:
		if current != "" && versionEqual(current, s.To) {
			s.Stage, s.Percent, s.Error = StageDone, 100, ""
		} else {
			s.Stage = StageFailed
			s.Error = fmt.Sprintf("安装后版本未变化（可能已回滚）：当前 %s，目标 %s", current, s.To)
		}
		_ = a.persist(s)
	case StageDownload, StageVerify:
		s.Stage = StageFailed
		s.Error = "上次更新在下载/校验阶段被中断"
		_ = a.persist(s)
	}
	_ = os.Remove(a.lockPath())
	a.mu.Lock()
	a.st = s
	a.mu.Unlock()
	return s
}

// acquireLock 拿到独占锁；陈旧锁（进程崩溃遗留）自动接管一次。
func (a *Applier) acquireLock() (func(), error) {
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		return nil, err
	}
	create := func() (*os.File, error) {
		return os.OpenFile(a.lockPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	f, err := create()
	if err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
		// 陈旧锁接管：只在确实过期时删，避免两个进程互删对方的新锁。
		if fi, statErr := os.Stat(a.lockPath()); statErr == nil && a.opt.Now().Sub(fi.ModTime()) > a.opt.LockStale {
			_ = os.Remove(a.lockPath())
			f, err = create()
		}
		if err != nil {
			return nil, ErrBusy
		}
	}
	_, _ = fmt.Fprintf(f, "pid=%d at=%s\n", os.Getpid(), a.opt.Now().Format(time.RFC3339))
	_ = f.Close()
	return func() { _ = os.Remove(a.lockPath()) }, nil
}

// Apply 执行一次更新：校验 → 下载 → SHA256 → 落盘状态 → 派生 msiexec。
//
// 返回 nil error 时状态为 StageInstall，调用方**必须**在响应写出后让本进程退出，
// 由 msiexec 完成安装并由 MSI 的 CaStartApp 拉起新版本（见 wb2api.wxs）。
func (a *Applier) Apply(ctx context.Context, current, target string, m MSI) (State, error) {
	if !a.busy.CompareAndSwap(false, true) {
		return a.State(), ErrBusy
	}
	defer a.busy.Store(false)

	to := strings.TrimSpace(target)
	if to == "" {
		to = versionOf(m.Name, m.URL)
	}
	st := State{Stage: StageDownload, From: current, To: to, Percent: 0}
	if err := validateMSI(m, a.opt.AllowHTTP); err != nil {
		st.Stage, st.Error = StageFailed, err.Error()
		a.setState(st)
		return a.State(), err
	}

	release, err := a.acquireLock()
	if err != nil {
		st.Stage, st.Error = StageFailed, err.Error()
		a.setState(st)
		return a.State(), err
	}
	// 成功路径**保留**锁：进程马上退出，若此时释放，另一个并发请求可能再派发一次 msiexec。
	// 锁由下次启动的 Reconcile 清理（那时安装事务已结束）。
	keepLock := false
	defer func() {
		if !keepLock {
			release()
		}
	}()

	// 磁盘空间：包体 3 倍 + 保留余量（MSI 解包会临时翻倍）。
	if free, derr := a.opt.DiskFree(a.opt.InstallDir); derr == nil {
		need := m.Size*3 + a.opt.MinFree
		if free < need {
			st.Stage, st.Error = StageFailed, fmt.Sprintf("%v：需要 %d MB，可用 %d MB", ErrNoSpace, need>>20, free>>20)
			a.setState(st)
			return st, ErrNoSpace
		}
	}

	a.setState(st)
	// 多源下载：主地址失败/校验不符就换镜像；同一地址允许续传重试。
	// 国内直连 GitHub Release 经常几 MB 都下不完，没有这层就永远更新不了。
	msiPath := ""
	var lastErr error
	for _, u := range a.candidateURLs(m.URL) {
		for attempt := 1; attempt <= a.opt.DownloadAttempts; attempt++ {
			p, _, derr := a.downloadFrom(ctx, u, m, &st)
			if derr == nil {
				msiPath = p
				break
			}
			lastErr = derr
			if errors.Is(derr, ErrChecksum) {
				break // 内容不对，换下一个源（.part 已删除）
			}
			if attempt < a.opt.DownloadAttempts {
				select {
				case <-ctx.Done():
					lastErr = ctx.Err()
				case <-time.After(400 * time.Millisecond):
				}
			}
		}
		if msiPath != "" {
			break
		}
	}
	if msiPath == "" {
		if lastErr == nil {
			lastErr = ErrChecksum
		}
		st.Stage, st.Error = StageFailed, lastErr.Error()
		a.setState(st)
		return a.State(), lastErr
	}
	st.Stage, st.Percent = StageVerify, 100
	a.setState(st)

	if a.opt.Spawn == nil {
		st.Stage, st.Error = StageFailed, ErrNotRunnable.Error()
		a.setState(st)
		return st, ErrNotRunnable
	}

	logPath := filepath.Join(a.opt.DataDir, fmt.Sprintf("update-%s.log", sanitize(st.To)))
	st.Stage, st.MSIPath, st.LogPath, st.Percent = StageInstall, msiPath, logPath, 100
	a.setState(st)
	w, err := a.opt.Spawn(msiPath, logPath)
	if err != nil {
		st.Stage, st.Error = StageFailed, "派生安装程序失败: "+err.Error()
		a.setState(st)
		return a.State(), err
	}
	// 早失败保护：msiexec 对坏包/被策略拦截会在 1 秒内非 0 退出。
	// 若此时网关已退出，就没人把服务拉起来 —— 所以先观察 EarlyExitWindow，
	// 立刻失败就收尾为 failed 并**继续服务**（不是让它停在那）。
	if code, exited := waitWithin(w, a.opt.EarlyExitWindow); exited && code != 0 {
		st.Stage = StageFailed
		st.Error = fmt.Sprintf("安装程序立即失败（exit=%d），当前版本未受影响；详见 %s", code, logPath)
		a.setState(st)
		return a.State(), fmt.Errorf("%w: exit=%d", ErrInstallFailed, code)
	}
	keepLock = true
	return a.State(), nil
}

// downloadFrom 从单个 URL 流式下载到 .update/<name>，边下边算 SHA256。
// 支持**断点续传**：已有 .part 时发 Range 请求（206 追加、200 从头再来），
// 失败时保留 .part 供下一次尝试继续 —— 本国网络下 3MB 常常一次下不完。
func (a *Applier) downloadFrom(ctx context.Context, urlStr string, m MSI, st *State) (string, string, error) {
	dir := filepath.Join(a.opt.InstallDir, ".update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	name := filepath.Base(m.Name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "update.msi"
	}
	dst := filepath.Join(dir, name)
	part := dst + ".part"

	var start int64
	if fi, err := os.Stat(part); err == nil {
		start = fi.Size()
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha256.New()
	if start > 0 {
		// 先把已落盘部分喂给哈希，保证最终哈希覆盖整份文件。
		if _, err := io.Copy(h, io.LimitReader(f, start)); err != nil {
			_ = os.Remove(part)
			return "", "", err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", "", err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := a.opt.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// 续传成功，保留已写入部分
	case http.StatusOK:
		if start > 0 { // 服务器不支持 Range：整份重下
			h.Reset()
			if err := f.Truncate(0); err != nil {
				_ = os.Remove(part)
				return "", "", err
			}
			start = 0
		}
	default:
		return "", "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", "", err
	}

	total := m.Size
	if resp.ContentLength > 0 {
		total = start + resp.ContentLength
	}
	buf := make([]byte, 64<<10)
	written := start
	lastPct := -1
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", "", werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct >= lastPct+a.opt.ProgressEvery || pct >= 100 {
					lastPct = pct
					st.Percent = pct
					a.setState(*st)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// 保留 .part，下一次尝试从这里续传
			return "", "", fmt.Errorf("下载中断（已存 %d 字节）: %w", written, rerr)
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(m.SHA256))
	if subtle.ConstantTimeCompare([]byte(sum), []byte(want)) != 1 {
		_ = os.Remove(part)
		return "", sum, fmt.Errorf("%w（实际 %s）", ErrChecksum, sum[:12])
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return "", "", err
	}
	if err := os.Rename(part, dst); err != nil {
		return "", "", err
	}
	return dst, sum, nil
}

// candidateURLs 返回下载候选：主地址 + 各镜像前缀（顺序即优先级）。
func (a *Applier) candidateURLs(primary string) []string {
	out := []string{primary}
	for _, px := range a.opt.Mirrors {
		px = strings.TrimSpace(px)
		if px == "" {
			continue
		}
		out = append(out, strings.TrimRight(px, "/")+"/"+primary)
	}
	return out
}

// validateMSI 校验清单字段；scheme 默认强制 https（AllowHTTP 仅供测试）。
func validateMSI(m MSI, allowHTTP bool) error {
	if m.URL == "" || m.SHA256 == "" || m.Name == "" {
		if m.URL == "" && m.SHA256 == "" && m.Name == "" {
			return ErrNoMSI
		}
		return ErrBadMSI
	}
	u, err := url.Parse(m.URL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadMSI, err)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return ErrScheme
	}
	if len(m.SHA256) != 64 {
		return fmt.Errorf("%w: sha256 长度 %d", ErrBadMSI, len(m.SHA256))
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return fmt.Errorf("%w: sha256 不是十六进制", ErrBadMSI)
	}
	return nil
}

// verifyFile 复算下载文件的 SHA256（防"边下边算"与实际落盘不一致）。
func verifyFile(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(strings.TrimSpace(want)))) != 1 {
		return fmt.Errorf("%w（实际 %s）", ErrChecksum, got[:12])
	}
	return nil
}

// versionOf 从 msi 文件名里取版本（workbuddy2api-1.11.20-x64.msi → 1.11.20），取不到则空串。
func versionOf(name, rawurl string) string {
	s := name
	if s == "" {
		s = rawurl
	}
	s = filepath.Base(s)
	s = strings.TrimSuffix(s, filepath.Ext(s))
	parts := strings.Split(s, "-")
	for _, p := range parts {
		if strings.Count(p, ".") >= 2 && strings.Trim(p, "0123456789.") == "" {
			return p
		}
	}
	return ""
}

// sanitize 把版本号压成安全文件名片段。
func sanitize(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// waitWithin 在 d 内等待安装程序退出；返回 (退出码, 是否已退出)。
// 超时返回 (0,false) 表示"安装仍在进行"，调用方按成功路径继续（优雅退出由 MSI 接管）。
func waitWithin(w Waiter, d time.Duration) (int, bool) {
	ch := make(chan int, 1)
	go func() { ch <- w.Wait() }()
	select {
	case code := <-ch:
		return code, true
	case <-time.After(d):
		return 0, false
	}
}
