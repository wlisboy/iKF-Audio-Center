//go:build windows

// 系统托盘（第 64 轮，B4）—— 纯 Win32 syscall，不引入任何新依赖。
// ====================================================================
//
// 为什么不用 systray 库：构建是离线的（GOPROXY=off，依赖全在本地 module
// cache），systray 库不在 cache 里；而 Shell_NotifyIconW 这套 API 面
// （注册窗口类 → 建隐藏窗口 → 加托盘图标 → 消息循环）用 syscall 直写
// 只有 ~200 行，不值得为它破坏离线构建。
//
// 行为对照原面板（SystemTrayLClick/RClick.cml）：
//   - 左键托盘图标：显示 / 隐藏主窗口（原面板是音量弹窗；第 67 轮曾还原
//     为音量滑杆弹窗，2026-10-20 连静音一起精简后整个移除，回到开关主窗）
//   - 右键托盘图标：菜单 = 显示主界面 / 静音切换 / 修复开机入口 / 退出
//     （2026-10-20 精简：录音机与产品定位无关，菜单项移除；exe 仍捆绑在
//     便携包 SoundRecorder\，需要时可从目录手动打开。静音保留在右键，
//     左键音量弹窗里的静音勾选框与它重复，一并移除——弹窗纯做音量滑杆。）
//   - 点窗口 ✕：不退出，隐藏进托盘（main.go 的 OnBeforeClose 拦截）；
//     「退出」菜单项才真退出（置 quitRequested 后调 runtime.Quit）
//
// 线程模型：托盘消息循环（GetMessageW）有线程亲和性，startTray 在独立
// goroutine 里 LockOSThread 独占一线程；菜单动作通过 Wails 运行时
// （WindowShow/Hide/Quit 线程安全，内部投递回主线程）执行。
//
// 静音走驱动 MuteControl（渲染端点）—— 与界面「静音」开关是同一个参数、
// 同一个写回主人（驱动），托盘只是另一个入口，不违反「一个交互一个主人」。
//

package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ★ 托盘图标（第 65 轮定稿）：直接用**官方原面板的托盘图标**
//
//	`Skin\FaceLift\Main\SystemTray.ico`（单帧 16×16/32bpp，原面板托盘
//	显示的就是它，用户要求沿用）。已拷入 build/windows/tray.ico；
//	加载时按 SM_CXSMICON 精确命中 16×16 帧（多帧大图会发糊的坑见
//	loadTrayIcon 注释）。窗口标题栏/任务栏/安装包仍用 icon.ico。
//
//go:embed build/windows/tray.ico
var iconIco []byte

// ---- Win32 常量 ----

const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	imageIcon      = 1
	lrLoadFromFile = 0x10
	wmApp          = 0x8000
	wmTrayCallback = wmApp + 1
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmNull         = 0x0000
	wmDestroy      = 0x0002
	tpmRightButton = 0x0002
	tpmReturnCMD   = 0x0100
	mfString       = 0x00000000
	mfGrayed       = 0x00000001
	mfSeparator    = 0x00000800
	csHredraw      = 0x0002
	csVredraw      = 0x0001
	smCxSmIcon     = 49 // GetSystemMetrics：小图标宽（随 DPI）
	smCySmIcon     = 50 // GetSystemMetrics：小图标高
)

// 托盘菜单命令 ID
const (
	cmdShowHide = 1
	cmdMute     = 2
	cmdQuit     = 4
	cmdEntryFix = 5
	cmdEntryDel = 6
)

// ---- 音量弹窗（第 67 轮，还原原面板左键行为：音量滑杆；2026-10-20 起
//      纯做音量——静音勾选框与右键菜单「静音」重复，已移除）----
//
// 本弹窗走与主界面「音量」拨盘同一个参数（VolumeScalarControl 0..1，
// 界面刻度 /100）、同一个写回主人（驱动）。
//
// 自动收起：WM_ACTIVATE 收到 WA_INACTIVE（点弹窗外面）就藏 —— 不能用
// WM_KILLFOCUS，点弹窗自己的滑杆会把焦点交给孩子，父窗口立刻
// 失焦就会被误藏。

const (
	wmHScroll   = 0x0114
	wmCommand   = 0x0111
	wmActivate  = 0x0006
	wmKillFocus = 0x0008
	wmSetFont   = 0x0030
	waInactive  = 0

	tbGetPos   = 0x0400 // TBM_GETPOS  = WM_USER+0
	tbSetPos   = 0x0405 // TBM_SETPOS  = WM_USER+5
	tbSetRange = 0x0406 // TBM_SETRANGE = WM_USER+6
	swHide     = 0
	swShow     = 5
	iccBars    = 0x00000004

	volW, volH = 250, 58
	volTrackID = 1
	volLabelID = 3
)

var (
	volHWnd  uintptr
	volTrack uintptr
	volLabel uintptr
	volShown bool
)

// ---- Win32 结构体（字段顺序即 x64 内存布局，Go 自动按对齐规则填充）----

type point struct{ X, Y int32 }

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

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
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

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
	GuidItem         syscall.GUID
}

// ---- 包级状态 ----

var (
	appCtx        context.Context // OnStartup 注入的 Wails 上下文
	quitRequested bool            // 「退出」菜单置位后 OnBeforeClose 才放行关窗
	winShown      = true          // 主窗口当前可见性（托盘左键切换用）
	trayHWnd      uintptr         // 托盘消息窗口句柄
)

// ---- 供 main.go 调用的入口 ----

// startTray 起托盘 goroutine。ctx 必须是 OnStartup 给的 Wails 上下文。
func startTray(ctx context.Context) {
	appCtx = ctx
	go trayLoop()
}

// quitWanted 由 main.go 的 OnBeforeClose 查询：true = 托盘点了退出，放行关窗。
func quitWanted() bool { return quitRequested }

// ---- 图标：把内嵌 .ico 落临时文件再用 LoadImage 读（自包含，无文件依赖）----

func loadTrayIcon() uintptr {
	user32 := syscall.NewLazyDLL("user32.dll")
	loadImage := user32.NewProc("LoadImageW")
	getMetrics := user32.NewProc("GetSystemMetrics")
	tmp, err := os.CreateTemp("", "ikf-*.ico")
	if err != nil {
		log.Println("[托盘] 写临时图标失败：", err)
		return 0
	}
	path := tmp.Name()
	if _, err := tmp.Write(iconIco); err != nil {
		tmp.Close()
		log.Println("[托盘] 写临时图标失败：", err)
		return 0
	}
	tmp.Close()
	// LoadImage 已把位图读进内存，临时文件立即可删
	defer os.Remove(path)
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	// ★ 显式按「小图标」系统尺寸加载：icon.ico 有 256→16 六帧，
	//   cxDesired=0 时 LoadImage 会挑最大帧（256px），托盘再硬缩到
	//   16px 就发糊发白（第 64 轮实测）。SM_CXSMICON/CYSMICON 随 DPI
	//   缩放（高 DPI 下自动取 20/24 等），托盘处无需再缩。
	cx, _, _ := getMetrics.Call(smCxSmIcon)
	cy, _, _ := getMetrics.Call(smCySmIcon)
	h, _, _ := loadImage.Call(
		0, uintptr(unsafe.Pointer(pathPtr)),
		imageIcon, cx, cy, lrLoadFromFile)
	if h == 0 {
		log.Println("[托盘] LoadImageW 加载图标失败（GetLastError 见系统日志）")
	}
	return h
}

// ---- 托盘图标注册 / 注销 ----

func trayOp(op uintptr, nid *notifyIconDataW) bool {
	shell32 := syscall.NewLazyDLL("shell32.dll")
	r, _, _ := shell32.NewProc("Shell_NotifyIconW").Call(op, uintptr(unsafe.Pointer(nid)))
	return r != 0
}

// trayBalloon 从托盘弹一条系统气泡提示（静音切换的回执 —— 托盘上没有
// 界面 toast 可用，气泡就是托盘的等价物）。
func trayBalloon(title, text string) {
	if trayHWnd == 0 {
		return
	}
	nid := notifyIconDataW{
		CbSize:      uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:        trayHWnd,
		UID:         1,
		UFlags:      nifInfo,
		DwInfoFlags: 0, // NIIF_NONE
	}
	copy(nid.SzInfo[:], utf16Of(text))
	copy(nid.SzInfoTitle[:], utf16Of(title))
	trayOp(nimModify, &nid)
}

func utf16Of(s string) []uint16 {
	u, _ := syscall.UTF16FromString(s)
	return u
}

// ---- 动作 ----

// toggleWindow 左键：显示 / 隐藏主窗。
func toggleWindow() {
	if appCtx == nil {
		return
	}
	if winShown {
		wruntime.WindowHide(appCtx)
		winShown = false
	} else {
		wruntime.WindowUnminimise(appCtx)
		wruntime.WindowShow(appCtx)
		winShown = true
	}
}

// startParamWatch 外部参数轮询（2026-10-20 用户报障：按系统音量键调音量，
// 主界面拨盘不动）。系统音量/静音可以被 Windows 随时改（音量键、系统
// 混音器、其它应用），应用内任何写入路径都收不到通知 —— 与托盘同理，
// 只能靠轮询。每 1s 读一次 VolumeScalarControl / MuteControl，与上次
// 不同才发 tray:sync（前端防抖 300ms 后整体重拉，与场景应用同路）。
// ★ 首轮只记基线不触发；驱动读失败（设备不在）记空串视为基线。
// ReadEp 跨线程调用与 --serve 的 HTTP 路径相同，线程安全已由 serve 验证。
// 检测到变化打一行日志（--serve 排障可见；窗口模式日志同理）。
func startParamWatch() {
	first := true // 首轮只记基线，不触发重拉
	lastVol, lastMute := "", ""
	for {
		time.Sleep(250 * time.Millisecond)
		vol, mute := "", ""
		if ret, val := drv.ReadEp(EpRender, "VolumeScalarControl"); ret == 0 {
			vol = strings.TrimSpace(val)
		}
		if ret, val := drv.ReadEp(EpRender, "MuteControl"); ret == 0 {
			mute = strings.TrimSpace(val)
		}
		if vol == lastVol && mute == lastMute {
			continue
		}
		wasFirst := first
		first = false
		lastVol, lastMute = vol, mute
		if wasFirst {
			continue
		}
		log.Printf("[参数watch] 外部变化：Volume=%s Mute=%s → 已通知主界面重同步", vol, mute)
		notifyFrontend()
	}
}

// notifyFrontend 托盘侧改了驱动参数后通知主界面重同步（2026-10-20 用户报障：
// 托盘静音变了主界面纹丝不动）。根因：托盘走 drv.WriteEp 直写驱动，绕过了
// 前端 post() 通道，主界面无从得知。修复 = Wails 事件推送 tray:sync，前端
// （build_center.py 桥接层）监听后整体重拉 /api/params（与 __resync 同路）。
// ★ 音量滑杆已于同日移除（左键改为开关主窗），目前唯一触发点是静音切换。
// --serve 诊断模式没有 appCtx/主界面，静默跳过。
func notifyFrontend() {
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "tray:sync")
}

// toggleMute 静音切换：读当前值 → 写反值 → 气泡回执。
func toggleMute() {
	ret, val := drv.ReadEp(EpRender, "MuteControl")
	if ret != 0 {
		trayBalloon("iKF Audio Center", "静音状态读取失败（驱动未就绪）")
		return
	}
	cur := strings.TrimSpace(val) == "1"
	wr := drv.WriteEp(EpRender, "MuteControl", !cur)
	if ok, _ := wr["ok"].(bool); !ok {
		trayBalloon("iKF Audio Center", "静音设置失败")
		return
	}
	notifyFrontend()
	if !cur {
		trayBalloon("iKF Audio Center", "已静音")
	} else {
		trayBalloon("iKF Audio Center", "已取消静音")
	}
}

// quitApp 真退出：置位 → 注销托盘图标 → Quit（OnBeforeClose 会放行）。
func quitApp() {
	quitRequested = true
	if trayHWnd != 0 {
		nid := notifyIconDataW{CbSize: uint32(unsafe.Sizeof(notifyIconDataW{})), HWnd: trayHWnd, UID: 1}
		trayOp(nimDelete, &nid)
	}
	if appCtx != nil {
		wruntime.Quit(appCtx)
	}
}

// ---- 菜单 ----

func showTrayMenu() {
	user32 := syscall.NewLazyDLL("user32.dll")
	createPopup := user32.NewProc("CreatePopupMenu")
	appendMenu := user32.NewProc("AppendMenuW")
	trackPopup := user32.NewProc("TrackPopupMenu")
	getCursorPos := user32.NewProc("GetCursorPos")
	setForeground := user32.NewProc("SetForegroundWindow")
	postMsg := user32.NewProc("PostMessageW")

	hMenu, _, _ := createPopup.Call()
	if hMenu == 0 {
		return
	}
	item := func(flags uintptr, id uintptr, text string) {
		p, _ := syscall.UTF16PtrFromString(text)
		appendMenu.Call(hMenu, flags, id, uintptr(unsafe.Pointer(p)))
	}
	item(mfString, cmdShowHide, "显示主界面")
	item(mfString, cmdMute, "静音 / 取消静音")
	item(mfSeparator, 0, "")
	// 入口状态（P5-1'）：便携包被移动后，「修复开机入口」把自启与面板项
	// 拉回当前目录；入口已存在时（2026-10-20 用户要求「可写也要可删」）
	// 提供删除 —— Run 值与 CLSID 命令值一并删掉，不再开机自启。
	// 隐藏启动（/h /d）时不弹提示框，增删都靠这里兜底。
	if st := readEntryStatus(); st.OK() {
		item(mfString, cmdEntryDel, "禁止开机启动")
	} else {
		item(mfString, cmdEntryFix, "开机自启")
	}
	item(mfSeparator, 0, "")
	item(mfString, cmdQuit, "退出")

	var pt point
	getCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	setForeground.Call(trayHWnd) // 前台化：点菜单外才会自动收起
	cmd, _, _ := trackPopup.Call(hMenu, tpmRightButton|tpmReturnCMD,
		uintptr(pt.X), uintptr(pt.Y), 0, trayHWnd, 0)
	postMsg.Call(trayHWnd, wmNull, 0, 0) // 让菜单销毁（SDK 文档要求）

	switch cmd {
	case cmdShowHide:
		if appCtx != nil {
			wruntime.WindowUnminimise(appCtx)
			wruntime.WindowShow(appCtx)
			winShown = true
		}
	case cmdMute:
		toggleMute()
	case cmdEntryFix:
		// 提权会阻塞到子进程退出，放到独立 goroutine，别卡住托盘消息循环
		go func() {
			if _, err := entryFix(); err != nil {
				trayBalloon("iKF Audio Center", "入口修复未完成："+err.Error())
				return
			}
			trayBalloon("iKF Audio Center", "已允许开机启动")
		}()
	case cmdEntryDel:
		go func() {
			if _, err := entryDelete(); err != nil {
				trayBalloon("iKF Audio Center", "禁止开机启动未完成："+err.Error())
				return
			}
			trayBalloon("iKF Audio Center",
				"已禁止开机启动")
		}()
	case cmdQuit:
		quitApp()
	}
}

// ---- 消息循环 ----

func trayWndProc(hWnd, msgID, wParam, lParam uintptr) uintptr {
	switch msgID {
	case wmTrayCallback:
		// 旧版 NOTIFYICON 行为：wParam=图标 ID，lParam=鼠标事件
		switch lParam & 0xFFFF {
		case wmLButtonUp:
			toggleWindow() // 2026-10-20：音量弹窗移除，左键 = 显示/隐藏主窗
		case wmRButtonUp:
			showTrayMenu()
		}
		return 0
	case wmDestroy:
		syscall.NewLazyDLL("user32.dll").NewProc("PostQuitMessage").Call(0)
		return 0
	}
	return defWindowProc(hWnd, msgID, wParam, lParam)
}

// defWindowProc 单独包一层：syscall.NewCallback 只允许有限的回调槽位，
// DefWindowProcW 不需要回调，直接拿过程地址调用。
func defWindowProc(hWnd, msgID, wParam, lParam uintptr) uintptr {
	p := syscall.NewLazyDLL("user32.dll").NewProc("DefWindowProcW")
	r, _, _ := p.Call(hWnd, msgID, wParam, lParam)
	return r
}

func trayLoop() {
	// DLL 句柄/消息窗口/消息循环都必须固定在这一个 OS 线程上。
	// 注意：drv 的驱动线程是另一条（driver.go 自己 LockOSThread），互不干扰。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	user32 := syscall.NewLazyDLL("user32.dll")
	getModuleHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	registerClass := user32.NewProc("RegisterClassExW")
	createWnd := user32.NewProc("CreateWindowExW")
	getMessage := user32.NewProc("GetMessageW")
	translate := user32.NewProc("TranslateMessage")
	dispatch := user32.NewProc("DispatchMessageW")

	clsName, _ := syscall.UTF16PtrFromString("IKFTrayWnd")
	winTitle, _ := syscall.UTF16PtrFromString("iKF")
	hInst, _, _ := getModuleHandle.Call(0)
	wcex := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		Style:         csHredraw | csVredraw,
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     hInst,
		LpszClassName: uintptr(unsafe.Pointer(clsName)),
	}
	if r, _, _ := registerClass.Call(uintptr(unsafe.Pointer(&wcex))); r == 0 {
		log.Println("[托盘] RegisterClassExW 失败，托盘不可用")
		return
	}

	// CreateWindowExW 全部 12 参：exStyle, 类名, 窗口名, style,
	// X/Y/W/H, 父窗口, 菜单, 实例, 附加参 —— style=0 即隐藏窗口。
	hWnd, _, _ := createWnd.Call(
		0, uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(winTitle)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hWnd == 0 {
		log.Println("[托盘] CreateWindowExW 失败，托盘不可用")
		return
	}
	trayHWnd = hWnd

	hIcon := loadTrayIcon()
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             trayHWnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayCallback,
		HIcon:            hIcon,
	}
	copy(nid.SzTip[:], utf16Of("iKF Audio Center"))
	if !trayOp(nimAdd, &nid) {
		log.Println("[托盘] Shell_NotifyIconW(NIM_ADD) 失败，托盘不可用")
		return
	}
	log.Println("[托盘] 托盘图标已注册")

	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0)
		if r == 0 { // WM_QUIT
			break
		}
		translate.Call(uintptr(unsafe.Pointer(&m)))
		dispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}
