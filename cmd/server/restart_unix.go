//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// launchRestart 在 Unix/Linux 环境启动一个完全脱离当前会话的新实例。
func launchRestart(exe string, args []string, wd string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = wd
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	return cmd.Start()
}
