package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeWaiter struct {
	code  int
	delay time.Duration
}

func (f *fakeWaiter) Wait() int {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.code
}

// spawnOK 模拟"安装仍在进行"（窗口内不退出）。
func spawnOK(string, string) (Waiter, error) { return &fakeWaiter{code: 0, delay: time.Hour}, nil }

func newApplier(t *testing.T, dir string, spawn func(string, string) (Waiter, error)) *Applier {
	t.Helper()
	return New(Options{
		DataDir:         filepath.Join(dir, "data"),
		InstallDir:      dir,
		Now:             func() time.Time { return time.Unix(1700000000, 0) },
		AllowHTTP:       true,
		Spawn:           spawn,
		EarlyExitWindow: 40 * time.Millisecond,
		DiskFree:        func(string) (int64, error) { return 1 << 40, nil },
	})
}

// 主路径：下载 → 校验 → 落状态(install) → 派生安装程序。
func TestApplyDownloadsVerifiesAndSpawns(t *testing.T) {
	body := []byte("fake-msi-payload")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	var gotMSI, gotLog string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := newApplier(t, dir, func(msi, log string) (Waiter, error) {
		mu.Lock()
		gotMSI, gotLog = msi, log
		mu.Unlock()
		return spawnOK(msi, log)
	})
	st, err := a.Apply(context.Background(), "1.11.19", "1.11.20", MSI{
		Name: "workbuddy2api-1.11.20-x64.msi", URL: srv.URL + "/x.msi", SHA256: sha, Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st.Stage != StageInstall || st.Percent != 100 {
		t.Fatalf("state=%+v want stage=install/percent=100", st)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("返回状态必须带 UpdatedAt（响应里不能是零值）")
	}
	if st.To != "1.11.20" {
		t.Errorf("To=%q want 1.11.20（从文件名解析）", st.To)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotMSI == "" || gotLog == "" {
		t.Fatalf("spawn 未被调用: msi=%q log=%q", gotMSI, gotLog)
	}
	if _, err := os.Stat(gotMSI); err != nil {
		t.Errorf("下载的文件不存在: %v", err)
	}
	if !strings.Contains(gotLog, "update-1.11.20.log") {
		t.Errorf("日志路径=%q 未含版本", gotLog)
	}
	raw, err := os.ReadFile(a.StatePath())
	if err != nil || !strings.Contains(string(raw), "install") {
		t.Errorf("状态文件未落盘: %v %s", err, raw)
	}
	// 成功路径**保留**锁：进程即将退出，锁由下次启动的 Reconcile 清理。
	if _, err := os.Stat(a.lockPath()); err != nil {
		t.Errorf("成功路径应保留锁文件，实际: %v", err)
	}
}

// SHA256 不匹配：必须失败、不派生、且删掉半成品。
func TestApplyRejectsChecksumMismatch(t *testing.T) {
	body := []byte("payload")
	bad := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	called := false
	a := newApplier(t, t.TempDir(), func(string, string) (Waiter, error) { called = true; return spawnOK("", "") })
	st, err := a.Apply(context.Background(), "1.11.19", "1.11.20", MSI{Name: "x.msi", URL: srv.URL, SHA256: bad, Size: int64(len(body))})
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err=%v want ErrChecksum", err)
	}
	if called {
		t.Error("校验失败后不得派生安装程序")
	}
	if st.Stage != StageFailed {
		t.Errorf("stage=%q want failed", st.Stage)
	}
}

// 非 https（且未开 AllowHTTP）必须拒绝：防止面板被改成 http 源。
func TestApplyRejectsPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	a := New(Options{DataDir: dir, InstallDir: dir, Spawn: spawnOK, EarlyExitWindow: 40 * time.Millisecond,
		DiskFree: func(string) (int64, error) { return 1 << 40, nil }})
	_, err := a.Apply(context.Background(), "1.11.19", "1.11.20", MSI{Name: "x.msi", URL: "http://example.com/x.msi", SHA256: strings.Repeat("a", 64), Size: 1})
	if !errors.Is(err, ErrScheme) {
		t.Fatalf("err=%v want ErrScheme", err)
	}
}

// 磁盘不足：在下载前就拒绝。
func TestApplyRejectsInsufficientSpace(t *testing.T) {
	dir := t.TempDir()
	a := New(Options{DataDir: dir, InstallDir: dir, AllowHTTP: true, EarlyExitWindow: 40 * time.Millisecond,
		Spawn:    spawnOK,
		DiskFree: func(string) (int64, error) { return 1024, nil },
		MinFree:  1 << 20})
	_, err := a.Apply(context.Background(), "1.11.19", "1.11.20", MSI{Name: "x.msi", URL: "http://x/y.msi", SHA256: strings.Repeat("a", 64), Size: 1 << 30})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err=%v want ErrNoSpace", err)
	}
}

// 并发：已有更新在跑时第二次调用必须被拒。
func TestApplyRejectsConcurrent(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, spawnOK)
	a.busy.Store(true)
	if _, err := a.Apply(context.Background(), "1", "1.11.20", MSI{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err=%v want ErrBusy", err)
	}
}

// 清单缺 msi 段：明确报 ErrNoMSI（老版本 latest.json 兼容路径）。
func TestApplyRejectsMissingMSI(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, spawnOK)
	if _, err := a.Apply(context.Background(), "1", "1.11.20", MSI{}); !errors.Is(err, ErrNoMSI) {
		t.Fatalf("err=%v want ErrNoMSI", err)
	}
}

// Reconcile：安装成功对账为 done；版本没变则 failed（回滚/未生效）。
func TestReconcileOutcomes(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, nil)
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	a.setState(State{Stage: StageInstall, From: "1.11.19", To: "1.11.20"})
	if s := a.Reconcile("1.11.20"); s.Stage != StageDone {
		t.Fatalf("同版本应判 done，得到 %+v", s)
	}

	a.setState(State{Stage: StageInstall, From: "1.11.19", To: "1.11.20"})
	s := a.Reconcile("1.11.19")
	if s.Stage != StageFailed || !strings.Contains(s.Error, "回滚") {
		t.Fatalf("版本未变应判 failed 且说明回滚，得到 %+v", s)
	}

	a.setState(State{Stage: StageDownload, Percent: 40})
	if s := a.Reconcile("1.11.19"); s.Stage != StageFailed {
		t.Fatalf("中断的下载应判 failed，得到 %+v", s)
	}
}

// Reconcile 必须清掉上次残留的锁（否则新进程永远 ErrBusy）。
func TestReconcileClearsStaleLock(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, nil)
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.lockPath(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Reconcile("1.11.19")
	if _, err := os.Stat(a.lockPath()); !os.IsNotExist(err) {
		t.Fatalf("锁文件应被清理，实际 err=%v", err)
	}
}

// 陈旧锁可被接管；新锁必须拒绝。
func TestLockStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, nil)
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.lockPath(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.acquireLock(); !errors.Is(err, ErrBusy) {
		t.Fatalf("新锁应 ErrBusy，得到 %v", err)
	}
	old := a.opt.Now().Add(-2 * time.Hour) // 用注入时钟，否则与固定 Now 比较会得到负值
	if err := os.Chtimes(a.lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	release, err := a.acquireLock()
	if err != nil {
		t.Fatalf("陈旧锁应被接管，得到 %v", err)
	}
	release()
}

func TestVersionOf(t *testing.T) {
	cases := map[string]string{
		"workbuddy2api-1.11.20-x64.msi":          "1.11.20",
		"workbuddy2api-1.11.20-x64-selftest.msi": "1.11.20",
		"update.msi":                             "",
	}
	for in, want := range cases {
		if got := versionOf(in, ""); got != want {
			t.Errorf("versionOf(%q)=%q want %q", in, got, want)
		}
	}
}

// 进程内版本带 -panel 后缀，而清单版本不带：必须判成功（否则每次更新都被误判回滚）。
func TestReconcileIgnoresVersionSuffix(t *testing.T) {
	dir := t.TempDir()
	a := newApplier(t, dir, nil)
	if err := os.MkdirAll(a.opt.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a.setState(State{Stage: StageInstall, From: "1.11.19-panel", To: "1.11.20"})
	if s := a.Reconcile("1.11.20-panel"); s.Stage != StageDone {
		t.Fatalf("1.11.20-panel 应判 done，得到 %+v", s)
	}
}

func TestVersionEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.11.20-panel", "1.11.20", true},
		{"1.11.20", "1.11.20-panel", true},
		{"1.11.20", "1.11.21", false},
		{"1.11.9", "1.11.10", false},
		{"", "1.11.20", false},
	}
	for _, c := range cases {
		if got := versionEqual(c.a, c.b); got != c.want {
			t.Errorf("versionEqual(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// 早失败保护：msiexec 立刻以非 0 退出（坏包 1620 / 被 HIPS 拦截）时，
// 必须收尾为 failed、释放锁（可重试），而**不是**让网关退出。
func TestApplyEarlyInstallFailureKeepsService(t *testing.T) {
	body := []byte("ok")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()

	dir := t.TempDir()
	a := newApplier(t, dir, func(string, string) (Waiter, error) {
		return &fakeWaiter{code: 1620}, nil // 立刻失败
	})
	st, err := a.Apply(context.Background(), "1.11.19", "1.11.20", MSI{
		Name: "x.msi", URL: srv.URL, SHA256: sha, Size: int64(len(body)),
	})
	if !errors.Is(err, ErrInstallFailed) {
		t.Fatalf("err=%v want ErrInstallFailed", err)
	}
	if st.Stage != StageFailed {
		t.Fatalf("stage=%q want failed", st.Stage)
	}
	if !strings.Contains(st.Error, "exit=1620") {
		t.Errorf("错误信息应含退出码，得到 %q", st.Error)
	}
	// 早失败必须释放锁：否则用户重试永远 409。
	if _, statErr := os.Stat(a.lockPath()); !os.IsNotExist(statErr) {
		t.Errorf("早失败后锁应释放，实际 %v", statErr)
	}
}
