// Package tray 提供 Windows 系统托盘图标（与网关同进程）。
//
// 设计取舍（见路线图 §R）：
//
//   - **与网关同进程**：`wb2api stop` 靠 `taskkill /IM wb2api-core.exe` 按映像名杀进程，
//     若托盘单独起一个 exe，stop 会漏杀它。同进程也天然让托盘的"重启/退出"复用
//     网关既有的生命周期回调（落盘、优雅停机、Docker 特判）。
//   - **纯 syscall 直呼 Win32，零第三方依赖**：仓库有成文约束（离线环境不可拉取第三方
//     模块，见 internal/autostart 与 internal/logfmt），且已有 `syscall.NewLazyDLL`
//     直呼 shell32/user32 的成熟先例。故不引 systray/walk/fyne 等 GUI 库。
//   - **失败即降级**：托盘是增强能力，任何一步失败都只应由调用方打 WARN，绝不阻断网关。
//
// 非 Windows 平台由 tray_other.go 提供 `ErrUnsupported` 桩。
package tray

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// ErrUnsupported 非 Windows 平台不支持系统托盘。
var ErrUnsupported = errors.New("tray is only supported on Windows")

// StatusInfo 提供托盘悬浮提示与菜单展示所需的实时状态与积分概况。
type StatusInfo struct {
	Online        bool    // 网关服务是否在线
	Healthy       int     // 正常可用账号数
	Total         int     // 总账号数
	Cooling       int     // 冷却中账号数
	Disabled      int     // 禁用账号数
	CNHealthy     int     // CN 域可用数
	CNTotal       int     // CN 域总数
	GlobalHealthy int     // Global 域可用数
	GlobalTotal   int     // Global 域总数
	TotalCredits  int64   // 剩余可用积分总额
	DailyCredits  float64 // 今日已消耗积分
	Urgent48h     int64   // 48h 临期紧急积分
}

// Options 托盘启动参数。
type Options struct {
	// PanelURL 左键单击 / 菜单"打开管理面板"要打开的地址。
	PanelURL string
	// HealthURL 健康探针地址（如 http://127.0.0.1:17963/healthz），用于刷新 tooltip。
	HealthURL string
	// Version 网关版本，用于 tooltip 前缀展示。
	Version string
	// HideConsole 启动时隐藏控制台窗口（进程仍为 console 子系统，stdout 与日志照常
	// 写入面板环形缓冲与日志文件）。为 true 时仅隐藏窗口，不改变进程属性。
	HideConsole bool
	// CorePath 网关可执行文件绝对路径（用于菜单里的开机自启注册）。
	CorePath string
	// DataDir 数据目录绝对路径（自启注册的 -config 基准）。
	DataDir string
	// OnRestart / OnShutdown 复用网关的生命周期回调；nil 时对应菜单项不生效。
	OnRestart  func() error
	OnShutdown func() error
	// StatusProvider 实时状态与积分查询回调（nil 时降级为通过 HealthURL 做基础探测）。
	StatusProvider func() StatusInfo
}

// 菜单项 ID。Windows 实现按这些 ID 建菜单，actionForMenuID 负责映射回语义动作。
const (
	menuIDOpenPanel       = 1
	menuIDRestart         = 2
	menuIDToggleAutostart = 3
	menuIDExit            = 4
)

// MenuAction 菜单项语义。
type MenuAction int

const (
	// ActionNone 未知 / 无动作（也是分隔符与状态行的归宿）。
	ActionNone MenuAction = iota
	// ActionOpenPanel 打开管理面板。
	ActionOpenPanel
	// ActionRestart 重启网关服务。
	ActionRestart
	// ActionToggleAutostart 切换开机自启。
	ActionToggleAutostart
	// ActionExit 退出网关服务。
	ActionExit
)

// actionForMenuID 把 Win32 菜单命令 ID 映射为语义动作。纯函数，便于单测。
func actionForMenuID(id int) MenuAction {
	switch id {
	case menuIDOpenPanel:
		return ActionOpenPanel
	case menuIDRestart:
		return ActionRestart
	case menuIDToggleAutostart:
		return ActionToggleAutostart
	case menuIDExit:
		return ActionExit
	default:
		return ActionNone
	}
}

// tipMaxUnits 是 NOTIFYICONDATAW.szTip 的容量上限（WCHAR[128] 含结尾 NUL，故有效 127 个 UTF-16 单元）。
const tipMaxUnits = 127

// TooltipText 生成托盘悬浮提示（兼容既有接口）。
func TooltipText(version string, healthy, total int, healthyOK bool) string {
	return FormatTooltip(version, StatusInfo{
		Online:  healthyOK,
		Healthy: healthy,
		Total:   total,
	})
}

// FormatTooltip 生成托盘多行悬浮提示（含在线状态、多域账号与简易积分）。
func FormatTooltip(version string, s StatusInfo) string {
	prefix := "WorkBuddy2API"
	if version != "" {
		prefix += " " + version
	}
	if !s.Online {
		return TruncateUTF16(prefix+" · 网关未响应", tipMaxUnits)
	}

	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("\n● ")
	b.WriteString(fmt.Sprintf("%d/%d 账号在线", s.Healthy, s.Total))
	if s.Cooling > 0 {
		b.WriteString(fmt.Sprintf(" (%d冷却)", s.Cooling))
	}
	if s.TotalCredits > 0 || s.DailyCredits > 0 {
		b.WriteString(fmt.Sprintf("\n💎 剩余 %s | 今日 %.2f", formatCredits(s.TotalCredits), s.DailyCredits))
	}
	return TruncateUTF16(b.String(), tipMaxUnits)
}

// MenuStatusItems 生成右键菜单顶部的状态与简易积分展示行（作为置灰信息展示）。
func MenuStatusItems(s StatusInfo) []string {
	if !s.Online {
		return []string{"● 网关状态：离线未响应"}
	}
	var items []string

	statusLine := fmt.Sprintf("● 状态：服务正常 (%d/%d 账号在线", s.Healthy, s.Total)
	if s.CNTotal > 0 && s.GlobalTotal > 0 {
		statusLine += fmt.Sprintf(" · CN %d/%d · GL %d/%d)", s.CNHealthy, s.CNTotal, s.GlobalHealthy, s.GlobalTotal)
	} else {
		statusLine += ")"
	}
	if s.Cooling > 0 {
		statusLine += fmt.Sprintf(" [%d个冷却中]", s.Cooling)
	}
	items = append(items, statusLine)

	creditLine := fmt.Sprintf("💎 积分：剩余 %s credits | 今日消耗 %.2f", formatCredits(s.TotalCredits), s.DailyCredits)
	items = append(items, creditLine)

	if s.Urgent48h > 0 {
		items = append(items, fmt.Sprintf("⏳ 临期：48h 内到期 %s credits", formatCredits(s.Urgent48h)))
	}
	return items
}

func formatCredits(n int64) string {
	if n < 0 {
		return "0"
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var res []byte
	rem := len(s) % 3
	if rem > 0 {
		res = append(res, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(res) > 0 {
			res = append(res, ',')
		}
		res = append(res, s[i:i+3]...)
	}
	return string(res)
}

// TruncateUTF16 把 s 截断到不超过 maxUnits 个 UTF-16 单元，且不会把一个代理对切成两半。
// 用于满足 Win32 定长 WCHAR 缓冲区的写入约束。
func TruncateUTF16(s string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	u := utf16.Encode([]rune(s))
	if len(u) <= maxUnits {
		return s
	}
	u = u[:maxUnits]
	// 末位若是高代理（未配对），去掉它，避免产出非法 UTF-16。
	if last := u[len(u)-1]; last >= 0xD800 && last <= 0xDBFF {
		u = u[:len(u)-1]
	}
	return string(utf16.Decode(u))
}
