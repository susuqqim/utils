//go:build windows

package tray

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
	"workbuddy2api/internal/gosafe"

	"workbuddy2api/internal/autostart"
)

//go:embed icon.ico
var iconData []byte

// ---- 常量 ----

const (
	trayClassName   = "WorkBuddy2APITrayWnd"
	trayIconID      = 1
	trayCallbackMsg = wmApp + 1 // Shell_NotifyIcon 回调消息
	trayUpdateMsg   = wmApp + 2 // 内部：刷新 tooltip

	wmApp       = 0x8000
	wmNull      = 0x0000
	wmClose     = 0x0010
	wmDestroy   = 0x0002
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004

	idiApplication = 32512
	dibRGBColors   = 0 // DIB_RGB_COLORS

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfDisabled  = 0x0002
	mfChecked   = 0x0008
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	swHide       = 0
	swShowNormal = 1
)

// HWND_MESSAGE：message-only 窗口的父句柄（(HWND)-3）。
const hwndMessage = ^uintptr(2)

// ---- DLL 过程表（纯 stdlib 直呼 Win32，与 internal/autostart 同套路）----

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")

	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procGetCurrentThreadId    = kernel32.NewProc("GetCurrentThreadId")

	procCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap     = gdi32.NewProc("CreateBitmap")
	procDeleteObject     = gdi32.NewProc("DeleteObject")

	procDestroyIcon = user32.NewProc("DestroyIcon")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")
)

// ---- Win32 结构体（字段布局与对齐由 Go 编译器按同一套 C ABI 规则计算）----

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type guid [16]byte

// notifyIconDataW 是 NOTIFYICONDATAW 的完整（Vista+）布局。
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         guid
	HBalloonIcon     uintptr
}

type msgT struct {
	HWnd    uintptr
	Message uint32
	_       uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type point struct{ X, Y int32 }

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

// ---- 状态 ----

// genState 表示"一代"托盘的生命周期（Start 建一代、Stop 结束一代）。
// 刻意不用包级 sync.Once：Stop 必须能关闭**下一次 Start 新建的** stopCh，
// 否则 Start→Stop→Start→Stop 时第二个 healthLoop 永不退出（goroutine + ticker 泄漏）。
type genState struct {
	stopCh chan struct{}
	done   chan struct{} // 消息循环退出后 close：供 Stop 有界等待，避免进程先于 removeIcon 结束
	closed bool
}

var (
	stateMu    sync.Mutex
	curOpts    Options
	hwndTray   uintptr
	nid        notifyIconDataW
	lastTip    string
	lastStatus StatusInfo
	cur        *genState
	iconOwned  bool   // true = HICON 由 createHIcon 自造，cleanup 需 DestroyIcon（回退的系统图标不可销毁）
	trayTID    uint32 // 托盘线程 ID：Stop 绝不在该线程上等待，否则自锁
)

// wndProcPtr 是窗口过程函数指针。必须保存在包级变量，避免被 GC 回收。
var wndProcPtr = syscall.NewCallback(wndProc)

var httpClient = &http.Client{Timeout: 3 * time.Second}

// Supported 报告当前平台是否支持系统托盘。
func Supported() bool { return true }

// opts 返回 Options 的快照，避免跨线程无锁读 curOpts。
func opts() Options {
	stateMu.Lock()
	defer stateMu.Unlock()
	return curOpts
}

// Start 创建托盘图标并进入消息循环（非阻塞：图标就绪后即返回）。
func Start(o Options) error {
	stateMu.Lock()
	// 幂等保护：同进程重复 Start 会让上一代的 hwnd/nid 被覆盖，导致旧消息循环
	// 操作到新一代的资源。当前调用方只 Start 一次，这里显式拒绝而非静默串台。
	if cur != nil && !cur.closed {
		stateMu.Unlock()
		return errors.New("tray already started")
	}
	curOpts = o
	// 新一代开始前复位图标归属，避免上一代的 true 泄漏到本次失败路径后误销毁共享图标。
	iconOwned = false
	nid.HIcon = 0
	g := &genState{stopCh: make(chan struct{}), done: make(chan struct{})}
	cur = g
	stateMu.Unlock()

	ready := make(chan error, 1)
	go func() {
		// ⚠️ 必须兜住 panic：托盘线程上运行着 syscall.NewCallback 注册的窗口过程，
		// 而它同步调用网关注入的重启/关闭回调（会落盘、关 HTTP server），一旦 panic
		// 会终止**整个网关进程**，与"托盘失败即降级"的承诺相反。
		// 更关键的是：recover 之后**必须**把结果发给 ready——否则 Start 会永久阻塞在
		// <-ready 上，main 卡在启动阶段、ListenAndServe 永不执行（把崩溃换成静默挂起）。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERR: [tray] 托盘线程 panic（已隔离，网关不受影响）: %v", r)
				select {
				case ready <- fmt.Errorf("tray panic: %v", r):
				default: // initIcon 已发过结果则忽略，避免二次发送阻塞
				}
			}
			close(g.done) // 通知 Stop：本代已彻底退出
		}()
		runtime.LockOSThread()
		tid, _, _ := procGetCurrentThreadId.Call()
		stateMu.Lock()
		trayTID = uint32(tid)
		stateMu.Unlock()
		if err := initIcon(o); err != nil {
			ready <- err
			return
		}
		ready <- nil
		messageLoop()
	}()
	if err := <-ready; err != nil {
		return err
	}

	go healthLoop(g)
	return nil
}

// Stop 移除托盘图标并结束消息循环（幂等：对未 Start / 重复调用均安全）。
// 会**有界等待**消息循环退出，确保 removeIcon 跑完再返回——否则进程可能在图标
// 被移除前就退出，留下僵尸图标（重启时还会与新进程的图标并存）。
func Stop() {
	stateMu.Lock()
	h := hwndTray
	g := cur
	if g != nil && !g.closed {
		g.closed = true
		close(g.stopCh)
	}
	onTrayThread := g != nil && trayTID != 0 && func() bool {
		cur, _, _ := procGetCurrentThreadId.Call()
		return uint32(cur) == trayTID
	}()
	stateMu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, wmClose, 0, 0)
	}
	if g == nil || onTrayThread {
		// 未启动过，或正处在托盘线程上（回调里调 Stop）——后者等待会自锁。
		return
	}
	select {
	case <-g.done:
	case <-time.After(2 * time.Second):
		log.Printf("WARN: [tray] 等待消息循环退出超时（继续退出，图标可能残留至资源回收）")
	}
}

// ---- 初始化与消息循环 ----

func initIcon(o Options) error {
	// 图标在**内存中**创建：解析内嵌 .ico 的 BGRA 像素 → CreateDIBSection + CreateIconIndirect。
	// 刻意不落盘——旧写法"把图标写到 %TEMP% 再 LoadImageW"属于杀软重点监控的
	// "释放文件到临时目录"行为特征，本机火绒即因此把整个二进制误删（见路线图 §R）。
	hIcon, err := createHIcon(iconData, 32)
	owned := err == nil && hIcon != 0
	if !owned {
		// 回退系统默认应用图标（共享图标，**不可** DestroyIcon），保证托盘依然可见。
		hIcon, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	if hIcon == 0 {
		return errors.New("加载托盘图标失败")
	}

	hInst, _, _ := procGetModuleHandleW.Call(0)

	className, err := syscall.UTF16PtrFromString(trayClassName)
	if err != nil {
		return err
	}
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   wndProcPtr,
		HInstance:     hInst,
		LpszClassName: className,
	}
	// 类重复注册返回 0（ERROR_CLASS_ALREADY_EXISTS）对同进程二次 Start 是正常情形，
	// 不影响用同一类名建窗口，故此处不视为错误。
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, err := syscall.UTF16PtrFromString("WorkBuddy2API")
	if err != nil {
		return err
	}
	hwnd, _, cerr := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		hwndMessage, 0, hInst, 0)
	if hwnd == 0 {
		if owned {
			procDestroyIcon.Call(hIcon)
		}
		return fmt.Errorf("创建托盘消息窗口失败: %v", cerr)
	}

	// nid 只在本线程（托盘线程）读写：applyTooltip/removeIcon 均由 wndProc/消息循环调用。
	nid = notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(nid)),
		HWnd:             hwnd,
		UID:              trayIconID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: trayCallbackMsg,
		HIcon:            hIcon,
	}
	st := StatusInfo{Online: false}
	tip := FormatTooltip(o.Version, st)
	copyTip(&nid, tip)

	if r, _, nerr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); r == 0 {
		// 失败必须把窗口与自造图标收干净，且**不写** hwndTray——
		// 否则 Stop() 会向一个没有消息泵的窗口 PostMessage(WM_CLOSE)，永远无人处理。
		if owned {
			procDestroyIcon.Call(hIcon)
		}
		procDestroyWindow.Call(hwnd)
		return fmt.Errorf("Shell_NotifyIcon(NIM_ADD) 失败: %v", nerr)
	}

	stateMu.Lock()
	hwndTray = hwnd
	lastTip = tip
	lastStatus = st
	iconOwned = owned
	stateMu.Unlock()

	// 控制台隐藏放在**托盘确认可用之后**：若上面任何一步失败，控制台保持可见，
	// 用户不会陷入"双击后什么都没有"（无窗口、无托盘）的状态。
	if o.HideConsole {
		hideConsoleIfExclusive()
		// 启动后一段时间内持续兜底（见 keepConsoleHidden 注释：终端窗口可能晚于本进程出现）
		go gosafe.Do("tray keep-console-hidden", keepConsoleHidden)
	}
	return nil
}

// hideConsoleIfExclusive 隐藏本进程的控制台窗口，但**仅当该控制台只属于本进程**时。
// GetConsoleWindow 返回的是进程所附着的控制台；直接从终端运行时会与用户的 cmd/PowerShell
// 共享同一个控制台，此时隐藏会把用户自己的窗口一起藏掉。
func hideConsoleIfExclusive() {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		// n>1 = 与别的进程共享（例如用户在自己的终端里敲命令启动）；n==0 = 调用失败。
		// 两种情况都**不许动控制台** —— 否则会把用户自己的窗口一起关掉/藏掉。
		return
	}
	// 优先 **FreeConsole()**：直接释放本进程独占的控制台。
	// 为什么比 ShowWindow 更可靠（用户实测反馈）：
	//   Windows 11 默认终端为 Windows Terminal 时，控制台是 ConPTY 伪控制台，
	//   **可见窗口属于 WindowsTerminal.exe**（类名 CASCADIA_HOSTING_WINDOW_CLASS，
	//   标题=我们的 exe 路径）—— 按"归属本进程 PID"找窗口根本找不到它；
	//   而 FreeConsole 会结束这条 ConPTY 会话 ⇒ 那个只为我们开着的 Windows Terminal
	//   标签/窗口随之关闭（不会影响用户自己的其它标签：每个标签是独立 ConPTY 会话）。
	r, _, _ := procFreeConsole.Call()
	if r != 0 {
		// 控制台已释放：把标准流改指到 NUL，避免后续写 stderr/stdout 报错
		// （日志真正的落点是面板日志环，见 cmd/server/main.go 的 MultiWriter 接线）。
		if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			os.Stdout, os.Stderr = f, f
		}
		return
	}
	// 兜底：FreeConsole 失败（极少数场景）时，退回"隐藏自己名下的伪控制台窗口"。
	if h, _, _ := procGetConsoleWindow.Call(); h != 0 {
		procShowWindow.Call(h, swHide)
		return
	}
	hideOwnPseudoConsoleWindows()
}

// hideOwnPseudoConsoleWindows 隐藏"类名为 PseudoConsoleWindow 且属于本进程"的窗口（FreeConsole 的兜底）。
func hideOwnPseudoConsoleWindows() {
	self := uint32(os.Getpid())
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != self {
			return 1
		}
		var buf [64]uint16
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 && syscall.UTF16ToString(buf[:n]) == "PseudoConsoleWindow" {
			procShowWindow.Call(hwnd, swHide)
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

// keepConsoleHidden 在启动后的一小段时间里**反复**尝试隐藏控制台窗口。
//
// 为什么要重试：Windows Terminal 托管时，承载我们的那个终端窗口往往**晚于**本进程出现
// （实测：开机自启拉起时，托盘初始化那一刻窗口还没建好，一次性隐藏会漏掉，
// 于是用户看到屏幕上一直留着一个标题为 wb2api-core.exe 的终端窗口）。
// 隐藏到"连续几轮都看不到自己的控制台窗口"即提前收敛，最多跑约 12 秒。
func keepConsoleHidden() {
	for i := 0; i < 12; i++ {
		time.Sleep(time.Second)
		if !hasVisibleOwnConsoleWindow() {
			if i >= 3 {
				return // 连续几轮都干净 → 停
			}
			continue
		}
		hideOwnPseudoConsoleWindows()
	}
}

// hasVisibleOwnConsoleWindow 报告本进程名下是否还有可见的伪控制台窗口。
func hasVisibleOwnConsoleWindow() bool {
	self := uint32(os.Getpid())
	found := false
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != self {
			return 1
		}
		var buf [64]uint16
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 && syscall.UTF16ToString(buf[:n]) == "PseudoConsoleWindow" {
			if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
				found = true
				return 0 // 找到即停止枚举
			}
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return found
}

func messageLoop() {
	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) == 0 { // WM_QUIT
			break
		}
		if int32(r) < 0 { // -1 = 错误：不要静默，"托盘莫名消失"要能查
			log.Printf("WARN: [tray] GetMessage 返回错误，消息循环退出")
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	cleanup()
}

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	// ⚠️ Windows 回调里 panic 会**终止整个进程**（Go 的 callback 包装器没有 recover，
	// panic 也无法被任何 Go 调用者兜住），而本进程同时承载网关本体，故必须在此兜住。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERR: [tray] 窗口过程 panic（已隔离，网关不受影响）: %v", r)
		}
	}()
	switch uint32(msg) {
	case trayCallbackMsg:
		switch uint32(lparam) {
		case wmRButtonUp:
			showMenu(hwnd)
		case wmLButtonUp:
			openPanel()
		}
		return 0
	case trayUpdateMsg:
		applyTooltip()
		return 0
	case wmClose:
		removeIcon()
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// safeCall 执行网关注入的回调并隔离 panic（回调会落盘、关 HTTP server，不能让它带崩进程）。
func safeCall(name string, fn func() error) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERR: [tray] %s 回调 panic（已隔离，网关不受影响）: %v", name, r)
		}
	}()
	// 普通 error 也要留痕：重启/退出失败此前被 `_ = fn()` 静默吞掉，用户点了菜单却什么都没发生
	// 且日志里毫无线索（第九轮审计 C4）。
	if err := fn(); err != nil {
		log.Printf("ERR: [tray] %s 回调失败: %v", name, err)
	}
}

// escMenu 转义菜单标签里的 '&'（Win32 把它当快捷键前缀）。
func escMenu(s string) string { return strings.ReplaceAll(s, "&", "&&") }

// ---- 菜单与动作 ----

func showMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	o := opts()
	stateMu.Lock()
	st := lastStatus
	stateMu.Unlock()

	items := MenuStatusItems(st)
	for _, it := range items {
		appendItem(menu, mfString|mfDisabled|mfGrayed, 0, escMenu(it))
	}
	appendItem(menu, mfSeparator, 0, "")
	appendItem(menu, mfString, menuIDOpenPanel, "打开管理面板")
	if o.OnRestart != nil {
		appendItem(menu, mfString, menuIDRestart, "重启服务")
	} else {
		appendItem(menu, mfString|mfDisabled|mfGrayed, menuIDRestart, "重启服务")
	}
	asFlags := uintptr(mfString)
	if autostartOn() {
		asFlags |= mfChecked
	}
	appendItem(menu, asFlags, menuIDToggleAutostart, "开机自启")
	appendItem(menu, mfSeparator, 0, "")
	if o.OnShutdown != nil {
		appendItem(menu, mfString, menuIDExit, "退出")
	} else {
		appendItem(menu, mfString|mfDisabled|mfGrayed, menuIDExit, "退出")
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmRightButton|tpmReturnCmd,
		uintptr(int64(pt.X)), uintptr(int64(pt.Y)), 0, hwnd, 0)
	// 托盘菜单经典要求：补一条消息，否则点击别处菜单不会消失。
	procPostMessageW.Call(hwnd, wmNull, 0, 0)

	switch actionForMenuID(int(int32(cmd))) {
	case ActionOpenPanel:
		openPanel()
	case ActionRestart:
		safeCall("重启", o.OnRestart)
	case ActionToggleAutostart:
		safeCall("切换开机自启", func() error { toggleAutostart(); return nil })
	case ActionExit:
		safeCall("退出", o.OnShutdown)
	}
}

func appendItem(menu, flags uintptr, id int, label string) {
	if label == "" {
		procAppendMenuW.Call(menu, flags, uintptr(id), 0)
		return
	}
	p, err := syscall.UTF16PtrFromString(label)
	if err != nil {
		procAppendMenuW.Call(menu, flags, uintptr(id), 0)
		return
	}
	// 必须把转换**内联在调用语句里**：若先存进 uintptr 变量，GC 会认为该缓冲已死，
	// 调用点（safepoint）上可能已被回收 → AppendMenuW 读到已释放内存。
	procAppendMenuW.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}

func openPanel() {
	url := opts().PanelURL
	if url == "" {
		return
	}
	u, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return
	}
	op, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return
	}
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(u)), 0, 0, swShowNormal)
}

func autostartOn() bool {
	info, err := autostart.Status()
	return err == nil && info.Enabled
}

func toggleAutostart() {
	o := opts()
	if autostartOn() {
		_, _ = autostart.Disable()
		return
	}
	if o.CorePath == "" || o.DataDir == "" {
		return
	}
	_, _ = autostart.Enable(o.CorePath, o.DataDir, false)
}

// ---- tooltip 刷新 ----

// Notify 弹出 Windows 通知（Shell_NotifyIcon 的 NIF_INFO 气泡）。
// 可跨 goroutine 调用：只修改 nid 的**副本**，不动托盘线程共享的 nid。
// 用于"发现新版本"提示（2026-10 更新通路重设计）——全程在进程内，不弹控制台。
func Notify(title, text string) error {
	if !Supported() {
		return ErrUnsupported
	}
	stateMu.Lock()
	hwnd := hwndTray
	stateMu.Unlock()
	if hwnd == 0 {
		return ErrUnsupported
	}
	const (
		nifInfo  = 0x00000010 // NIF_INFO
		niifInfo = 0x00000001 // NIIF_INFO
	)
	n := notifyIconDataW{
		CbSize:      uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:        hwnd,
		UID:         trayIconID,
		UFlags:      nifInfo,
		DwInfoFlags: niifInfo,
	}
	copyTitle(&n, title)
	copyInfo(&n, text)
	if r, _, e := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n))); r == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_MODIFY/NIF_INFO) 失败: %v", e)
	}
	return nil
}

// copyTitle/copyInfo 把字符串截断后写入固定长度 UTF-16 数组（含结尾 NUL）。
func copyTitle(n *notifyIconDataW, s string) {
	u := utf16.Encode([]rune(TruncateUTF16(s, 63)))
	copy(n.SzInfoTitle[:], u)
}
func copyInfo(n *notifyIconDataW, s string) {
	u := utf16.Encode([]rune(TruncateUTF16(s, 255)))
	copy(n.SzInfo[:], u)
}

func applyTooltip() {
	stateMu.Lock()
	h := hwndTray
	tip := lastTip
	stateMu.Unlock()
	if h == 0 {
		return
	}
	copyTip(&nid, tip)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func healthLoop(g *genState) {
	// 健康探测是**长驻循环**：只在 goroutine 顶层兜底的话，一次 panic 会让探测永久停摆
	// （图标状态冻结、无任何迹象）。按 AGENTS.md 的约定：顶层 + 每轮迭代各兜一层
	// （第九轮审计 C5）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERR: [tray] 健康探测 panic（已隔离，网关不受影响）: %v", r)
		}
	}()
	// 托盘在 HTTP 监听之前启动，立即探测会短暂显示"未响应"，故首帧稍作延迟。
	select {
	case <-g.stopCh:
		return
	case <-time.After(3 * time.Second):
	}
	gosafe.Loop("tray health probe", probe)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-tick.C:
			// 迭代级兜底：单次探测 panic 只丢这一轮，循环继续（否则图标状态永久冻结）。
			gosafe.Loop("tray health probe", probe)
		}
	}
}

func probe() {
	o := opts()
	var st StatusInfo
	if o.StatusProvider != nil {
		st = o.StatusProvider()
	} else if h, t, ok := fetchHealth(); ok {
		st = StatusInfo{
			Online:  true,
			Healthy: h,
			Total:   t,
		}
	} else {
		st = StatusInfo{Online: false}
	}
	tip := FormatTooltip(o.Version, st)
	stateMu.Lock()
	lastTip = tip
	lastStatus = st
	h := hwndTray
	stateMu.Unlock()
	if h != 0 {
		procPostMessageW.Call(h, trayUpdateMsg, 0, 0)
	}
}

func fetchHealth() (healthy, total int, ok bool) {
	url := opts().HealthURL
	if url == "" {
		return 0, 0, false
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, false
	}
	var v struct {
		Healthy int `json:"healthy"`
		Total   int `json:"total"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v); err != nil {
		return 0, 0, false
	}
	return v.Healthy, v.Total, true
}

// ---- 收尾 ----

func removeIcon() {
	stateMu.Lock()
	h := hwndTray
	stateMu.Unlock()
	if h == 0 {
		return
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func cleanup() {
	removeIcon()
	stateMu.Lock()
	// 自造的 HICON 必须销毁；回退用的 IDI_APPLICATION 是系统共享图标，不能销毁。
	if iconOwned && nid.HIcon != 0 {
		procDestroyIcon.Call(nid.HIcon)
	}
	nid.HIcon = 0
	iconOwned = false
	hwndTray = 0
	stateMu.Unlock()
}

func copyTip(d *notifyIconDataW, s string) {
	u := utf16.Encode([]rune(TruncateUTF16(s, tipMaxUnits)))
	n := copy(d.SzTip[:len(d.SzTip)-1], u)
	d.SzTip[n] = 0
}

// ---- 内存图标构建（全程不落盘）----

// iconPixels 从 ICO 字节流里选出最接近 want 的 32bpp 图像，解码为**自上而下**的
// 预乘 alpha BGRA 像素（CreateDIBSection + CreateIconIndirect 所需格式）。
func iconPixels(data []byte, want int) (w, h int, bgra []byte, err error) {
	if len(data) < 6 {
		return 0, 0, nil, errors.New("ico 数据过短")
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	type entry struct {
		w, h      int
		bits      uint16
		off, size uint32
	}
	ents := make([]entry, 0, count)
	for i := 0; i < count; i++ {
		base := 6 + i*16
		if base+16 > len(data) {
			break
		}
		ew, eh := int(data[base]), int(data[base+1])
		if ew == 0 {
			ew = 256
		}
		if eh == 0 {
			eh = 256
		}
		ents = append(ents, entry{
			w:    ew,
			h:    eh,
			bits: binary.LittleEndian.Uint16(data[base+6 : base+8]),
			size: binary.LittleEndian.Uint32(data[base+8 : base+12]),
			off:  binary.LittleEndian.Uint32(data[base+12 : base+16]),
		})
	}
	if len(ents) == 0 {
		return 0, 0, nil, errors.New("ico 无图像条目")
	}
	best := -1
	for i, e := range ents {
		if e.bits != 32 {
			continue
		}
		if best < 0 || absInt(e.w-want) < absInt(ents[best].w-want) {
			best = i
		}
	}
	if best < 0 {
		// 没有 32bpp 条目就明确失败（由调用方回退系统图标），
		// 不要按 32bpp 去解释 8bpp/调色板数据——那只会产出花屏或误判"截断"。
		return 0, 0, nil, errors.New("ico 无 32bpp 图像条目")
	}
	e := ents[best]
	// 全程用 uint64 做边界运算：e.off 是 uint32，32 位构建下 int(e.off) 可能为负，
	// 让下面的越界检查失效。
	if uint64(e.off)+40 > uint64(len(data)) {
		return 0, 0, nil, errors.New("ico 偏移越界")
	}
	bi := data[e.off:]
	if binary.LittleEndian.Uint32(bi[0:4]) != 40 {
		// 只支持标准 BITMAPINFOHEADER（biSize=40）：像素起点固定为 off+40，
		// 若接受 V4/V5 头（108/124）会把头部当像素读成花屏。宁可失败回退系统图标。
		return 0, 0, nil, errors.New("ico 非标准 BITMAPINFOHEADER")
	}
	if binary.LittleEndian.Uint32(bi[16:20]) != 0 { // biCompression 必须是 BI_RGB
		return 0, 0, nil, errors.New("ico 压缩方式不支持")
	}
	biHeight := int(int32(binary.LittleEndian.Uint32(bi[8:12])))
	pixOff := uint64(e.off) + 40
	w, h = e.w, biHeight/2
	if w <= 0 || h <= 0 {
		return 0, 0, nil, errors.New("ico 尺寸非法")
	}
	// 用 uint64 计算总字节数：32 位构建下 w*h*4 可能溢出为负，令下面的检查失效。
	total := uint64(w) * uint64(h) * 4
	if pixOff+total > uint64(len(data)) {
		return 0, 0, nil, errors.New("ico 像素数据截断")
	}
	bgra = make([]byte, total)
	for y := 0; y < h; y++ {
		src := int(pixOff) + (h-1-y)*w*4 // 源为自下而上
		dst := y * w * 4
		for x := 0; x < w; x++ {
			b := uint16(data[src+x*4])
			g := uint16(data[src+x*4+1])
			r := uint16(data[src+x*4+2])
			a := uint16(data[src+x*4+3])
			bgra[dst+x*4] = byte(b * a / 255)
			bgra[dst+x*4+1] = byte(g * a / 255)
			bgra[dst+x*4+2] = byte(r * a / 255)
			bgra[dst+x*4+3] = byte(a)
		}
	}
	return w, h, bgra, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// createHIcon 用内存 DIB 造一个 HICON，全程不产生任何临时文件。
func createHIcon(data []byte, want int) (uintptr, error) {
	w, h, bgra, err := iconPixels(data, want)
	if err != nil {
		return 0, err
	}
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 0, errors.New("GetDC 失败")
	}
	defer procReleaseDC.Call(0, hdc)

	bih := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      -int32(h), // 负高度 = 自上而下 DIB
		Planes:      1,
		BitCount:    32,
		Compression: 0, // BI_RGB
	}
	// bits 用 unsafe.Pointer 承接（DIB 由系统分配，非 Go 堆，GC 视为外部地址）；
	// 避免 uintptr→unsafe.Pointer 往返（go vet unsafeptr 会告警）。
	var bits unsafe.Pointer
	hbmColor, _, _ := procCreateDIBSection.Call(hdc,
		uintptr(unsafe.Pointer(&bih)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmColor == 0 || bits == nil {
		return 0, errors.New("CreateDIBSection 失败")
	}
	defer procDeleteObject.Call(hbmColor)

	copy(unsafe.Slice((*byte)(bits), len(bgra)), bgra)

	// AND 掩码：1bpp、行按 WORD 对齐，全 0（透明由 alpha 决定）。mask 恒非空（h>=1）。
	maskRow := ((w + 15) / 16) * 2
	mask := make([]byte, maskRow*h)
	hbmMask, _, _ := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, uintptr(unsafe.Pointer(&mask[0])))
	if hbmMask == 0 {
		return 0, errors.New("CreateBitmap 失败")
	}
	defer procDeleteObject.Call(hbmMask)

	ii := iconInfo{FIcon: 1, HbmMask: hbmMask, HbmColor: hbmColor}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if hIcon == 0 {
		return 0, errors.New("CreateIconIndirect 失败")
	}
	return hIcon, nil
}
