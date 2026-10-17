//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// launchRestart 在 Windows 宿主环境启动一个完全独立的新实例。
//
// 控制台窗口策略：非托盘模式沿用 CREATE_NEW_CONSOLE（保证前台用法能看到新进程输出）；
// 托盘模式（restartHidden，见 main.go）改用 CREATE_NO_WINDOW，避免每次重启弹出新控制台
// ——托盘会在启动时自行隐藏窗口，但"先弹出再隐藏"仍会闪窗。
func launchRestart(exe string, args []string, wd string) error {
	flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP | 0x00000010) // CREATE_NEW_CONSOLE
	if restartHidden {
		flags = uint32(syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000) // CREATE_NO_WINDOW
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = wd
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	return cmd.Start()
}
