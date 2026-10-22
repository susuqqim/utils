//go:build windows

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

// createNoWindow = CREATE_NO_WINDOW：不弹控制台（与托盘模式一致，避免闪窗）。
const createNoWindow = 0x08000000

// SpawnMSI 用系统自带的 msiexec 执行安装包。
//
// 参数选择（见 工作区文档/自更新方案-20261022.md §7）：
//   - /passive：显示进度条但零交互 —— 火绒的"软件安装拦截"对全静默 /qn 更敏感；
//   - /norestart：绝不重启系统（本产品不需要）；
//   - /l*v：完整日志，失败时运维可查（路径与 progress 端点返回的一致）。
func SpawnMSI(msiPath, logPath string) (Waiter, error) {
	msiexec := filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe")
	if _, err := os.Stat(msiexec); err != nil {
		msiexec = "msiexec.exe" // 极端情况退回 PATH 查找
	}
	cmd := exec.Command(msiexec, "/i", msiPath, "/passive", "/norestart", "/l*v", logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdWaiter{cmd: cmd}, nil
}

// cmdWaiter 让 Wait() 幂等：多个观察者（早失败窗口 + 后续）拿到同一退出码。
type cmdWaiter struct {
	cmd  *exec.Cmd
	once sync.Once
	code int
}

func (w *cmdWaiter) Wait() int {
	w.once.Do(func() {
		_ = w.cmd.Wait()
		w.code = w.cmd.ProcessState.ExitCode()
	})
	return w.code
}
