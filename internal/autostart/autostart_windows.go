//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const runKeyPath = "Software\\Microsoft\\Windows\\CurrentVersion\\Run"

// startupFileName 是启动文件夹兜底文件名（沿用历史命名，便于用户识别）。
const startupFileName = "WorkBuddy2API.cmd"

// scheduledTaskName 是历史实现可能留下的计划任务名，Disable 时尽力清理。
const scheduledTaskName = "WorkBuddy2API"

// Enable 启用开机自启。默认写 HKCU Run 键；useStartupFolder=true 时写启动文件夹（写 .cmd）。
// 启用一种方式时，自动清理另一种方式的残留，防止开机同时启动两个实例造成端口冲突。
func Enable(corePath, dataDir string, useStartupFolder bool) (Info, error) {
	if corePath == "" || dataDir == "" {
		return Info{}, errors.New("core path / data dir must not be empty")
	}
	info := Info{}
	if sf, err := startupFilePath(); err == nil {
		info.StartupFile = sf
	}

	if useStartupFolder {
		// 显式指定启动文件夹：写入 .cmd 并清理 Run 键避免双重自启
		if err := writeStartupCmd(corePath, dataDir); err != nil {
			return info, fmt.Errorf("write Startup folder failed: %w", err)
		}
		_ = deleteRunValue(runKeyPath, RunValueName)
		info.Enabled = true
		info.Method = "startup-folder"
		info.StartupSet = true
		return info, nil
	}

	// 默认模式：写 HKCU Run 键；成功后清理可能遗留的启动文件夹 .cmd 避免双重自启
	value := RunValue(corePath, dataDir, filepath.Join(dataDir, "config.json"))
	if err := setRunValue(runKeyPath, RunValueName, value); err != nil {
		return info, fmt.Errorf("write HKCU Run failed: %w", err)
	}
	if info.StartupFile != "" {
		_ = os.Remove(info.StartupFile)
	}
	info.Enabled = true
	info.Method = "run-key"
	info.RunValue = value
	return info, nil
}

// Disable 关闭自启：删除 Run 键值、启动文件夹兜底文件，并尽力清理历史实现留下的
// wrapper 与计划任务。
func Disable() (Info, error) {
	var errs []error
	if err := deleteRunValue(runKeyPath, RunValueName); err != nil {
		errs = append(errs, err)
	}
	if p, err := startupFilePath(); err == nil {
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			errs = append(errs, rerr)
		}
	}
	// 历史实现产物：%LOCALAPPDATA%\workbuddy2api\wb2api-autorun.cmd 与同名计划任务。
	if ad := os.Getenv("LOCALAPPDATA"); ad != "" {
		_ = os.Remove(filepath.Join(ad, "workbuddy2api", "wb2api-autorun.cmd"))
	}
	_ = exec.Command("schtasks", "/delete", "/tn", scheduledTaskName, "/f").Run()
	info, _ := Status()
	if len(errs) > 0 {
		return info, errors.Join(errs...)
	}
	return info, nil
}

// Status 读取当前自启状态（Run 键 + 启动文件夹）。
func Status() (Info, error) {
	info := Info{}
	if sf, err := startupFilePath(); err == nil {
		info.StartupFile = sf
	}
	value, ok, err := queryRunValue(runKeyPath, RunValueName)
	if err != nil {
		return info, err
	}
	if ok {
		info.Enabled = true
		info.Method = "run-key"
		info.RunValue = value
	}
	if info.StartupFile != "" {
		if _, serr := os.Stat(info.StartupFile); serr == nil {
			info.StartupSet = true
			if !info.Enabled {
				info.Enabled = true
				info.Method = "startup-folder"
			}
		}
	}
	return info, nil
}

// ---- 注册表（直呼 advapi32.dll，纯 stdlib、零外部依赖）----
//
// 说明：Go 标准库 syscall 未导出注册表 API（在 golang.org/x/sys/windows/registry 中，
// 而本仓库离线环境不可拉取该模块），故用 LazyDLL 直接调用 Win32 的 Reg*W。

const (
	hkeyCurrentUser      = 0x80000001 // HKEY_CURRENT_USER
	keyQueryValue        = 0x0001
	keySetValue          = 0x0002
	regSZ                = 1
	regOptionNonVolatile = 0
	errorSuccess         = 0
	errorFileNotFound    = 2
)

var (
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

// errnoIfNotSuccess 把 0 以外的返回码转为 error（ERROR_FILE_NOT_FOUND 由调用方特判）。
func errnoIfNotSuccess(r1 uintptr) error {
	if r1 == errorSuccess {
		return nil
	}
	return syscall.Errno(r1)
}

func setRunValue(keyPath, name, value string) error {
	sub, err := syscall.UTF16PtrFromString(keyPath)
	if err != nil {
		return err
	}
	var h syscall.Handle
	var disp uint32
	r1, _, _ := procRegCreateKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(sub)), 0, 0,
		regOptionNonVolatile, keySetValue, 0, uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&disp)))
	if err := errnoIfNotSuccess(r1); err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(h))
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	data := utf16.Encode([]rune(value))
	data = append(data, 0)
	r1, _, _ = procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)), 0, regSZ,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	return errnoIfNotSuccess(r1)
}

func queryRunValue(keyPath, name string) (string, bool, error) {
	sub, err := syscall.UTF16PtrFromString(keyPath)
	if err != nil {
		return "", false, err
	}
	var h syscall.Handle
	r1, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(sub)), 0,
		keyQueryValue, uintptr(unsafe.Pointer(&h)))
	if r1 != errorSuccess {
		if syscall.Errno(r1) == syscall.Errno(errorFileNotFound) {
			return "", false, nil
		}
		return "", false, syscall.Errno(r1)
	}
	defer procRegCloseKey.Call(uintptr(h))
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return "", false, err
	}
	var valType, size uint32
	r1, _, _ = procRegQueryValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)), 0,
		uintptr(unsafe.Pointer(&valType)), 0, uintptr(unsafe.Pointer(&size)))
	if r1 != errorSuccess {
		if syscall.Errno(r1) == syscall.Errno(errorFileNotFound) {
			return "", false, nil
		}
		return "", false, syscall.Errno(r1)
	}
	if size == 0 {
		return "", true, nil
	}
	buf := make([]uint16, size/2+1)
	r1, _, _ = procRegQueryValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)), 0,
		uintptr(unsafe.Pointer(&valType)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 != errorSuccess {
		return "", false, syscall.Errno(r1)
	}
	return strings.TrimRight(string(utf16.Decode(buf[:size/2])), "\x00"), true, nil
}

func deleteRunValue(keyPath, name string) error {
	sub, err := syscall.UTF16PtrFromString(keyPath)
	if err != nil {
		return err
	}
	var h syscall.Handle
	r1, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(sub)), 0,
		keySetValue, uintptr(unsafe.Pointer(&h)))
	if r1 != errorSuccess {
		if syscall.Errno(r1) == syscall.Errno(errorFileNotFound) {
			return nil
		}
		return syscall.Errno(r1)
	}
	defer procRegCloseKey.Call(uintptr(h))
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r1, _, _ = procRegDeleteValueW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)))
	if r1 != errorSuccess && syscall.Errno(r1) != syscall.Errno(errorFileNotFound) {
		return syscall.Errno(r1)
	}
	return nil
}

// ---- 启动文件夹（仅 opt-in 兜底）----

func startupFilePath() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("APPDATA not set")
	}
	return filepath.Join(appData, "Microsoft\\Windows\\Start Menu\\Programs\\Startup", startupFileName), nil
}

func writeStartupCmd(corePath, dataDir string) error {
	p, err := startupFilePath()
	if err != nil {
		return err
	}
	core := strings.Trim(strings.TrimSpace(corePath), "\"")
	dir := strings.Trim(strings.TrimSpace(dataDir), "\"")
	cfgFile := filepath.Join(dir, "config.json")
	content := "@echo off\r\n" +
		"cd /d \"" + dir + "\"\r\n" +
		"start \"WorkBuddy2API\" /min \"" + core + "\" -config \"" + cfgFile + "\"\r\n"
	return os.WriteFile(p, []byte(content), 0o644)
}
