// Package autostart 管理 Windows 开机自启，供 `wb2api autostart on|off|status` 使用。
//
// 设计取舍（见路线图 §N）：旧实现是 wb2api.cmd -> powershell -ExecutionPolicy Bypass
// -> autostart.ps1，命中杀软"启动项防护"启发式（cmd -> powershell Bypass -> 写启动项
// 是恶意持久化的经典形态，火绒规则 fp_name_touch_startup_malware 即由此触发）。现改为：
//
//   - 由 Go 二进制**直接写注册表**（HKCU\Software\Microsoft\Windows\CurrentVersion\Run），
//     不再经过 cmd/powershell，也不需要 .ps1 脚本文件；
//   - **默认只写 Run 键**（免提权、免额外文件）；启动文件夹 .cmd 仅作显式 opt-in 兜底
//     （--startup-folder），因为它本身就是杀软重点监控的启动项形态。
//
// 非 Windows 平台（含 Linux 交叉编译）由 autostart_other.go 提供占位实现。
package autostart

import (
	"strings"
)

// RunValueName 是 HKCU\...\Run 下的值名（也是历史实现的持久化项名）。
const RunValueName = "WorkBuddy2API"

// Info 描述当前自启状态。
type Info struct {
	// Enabled 是否有任一自启机制生效。
	Enabled bool
	// Method 生效方式："run-key" / "startup-folder" / ""（未启用）。
	Method string
	// RunValue HKCU Run 中的原始值（空 = 未设置）。
	RunValue string
	// StartupFile 启动文件夹兜底文件路径（用于展示）。
	StartupFile string
	// StartupSet 启动文件夹文件是否存在。
	StartupSet bool
}

// RunValue 描述写成 Run 键的完整命令行：绝对路径 + 绝对 config 路径。
// Run 键不改变工作目录，故 config 必须给绝对路径（旧实现靠 wrapper .cmd 里的 cd 兜底）。
func RunValue(corePath, dataDir, cfgFile string) string {
	core := strings.Trim(strings.TrimSpace(corePath), "\"")
	cfg := strings.Trim(strings.TrimSpace(cfgFile), "\"")
	return "\"" + core + "\" -config \"" + cfg + "\""
}
