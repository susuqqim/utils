//go:build !windows

package tray

// Start 在非 Windows 平台恒返回 ErrUnsupported（静默降级，不影响网关运行）。
func Start(Options) error { return ErrUnsupported }

// Stop 在非 Windows 平台是空操作。
func Stop() {}

// Supported 报告当前平台是否支持系统托盘。
func Supported() bool { return false }
