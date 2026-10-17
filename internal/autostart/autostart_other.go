//go:build !windows

package autostart

import "errors"

// ErrUnsupported 非 Windows 平台不支持本机自启注册。
var ErrUnsupported = errors.New("autostart is only supported on Windows")

// Enable 在非 Windows 平台恒返回 ErrUnsupported。
func Enable(corePath, dataDir string, useStartupFolder bool) (Info, error) {
	return Info{}, ErrUnsupported
}

// Disable 在非 Windows 平台恒返回 ErrUnsupported。
func Disable() (Info, error) { return Info{}, ErrUnsupported }

// Status 在非 Windows 平台恒返回 ErrUnsupported。
func Status() (Info, error) { return Info{}, ErrUnsupported }
