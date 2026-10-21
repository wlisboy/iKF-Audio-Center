//go:build windows

package main

// 驱动管理界面 —— 自绘的 320x240 单窗口，两种形态
// ==============================================
//
//	选择态：清单勾选要执行的组件（多选），右下角「取消」「安装」
//	进度态：装/卸期间同一窗口换成 标题 + 副标 + 来回滚的进度条，期间不许关窗
//
// 为什么自绘：不引 GUI 库、不用 comctl32 的控件与 marquee 进度条 ——
// marquee 是 comctl32 v6 才有的，要用 v6 就得带清单（带清单就得有 .syso）。
// 一张画布反而更短，顺带能把配色和主程序（#0B0B0D 底、#E60012 品牌红）对齐。

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW      = user32.NewProc("RegisterClassExW")
	pCreateWindowExW       = user32.NewProc("CreateWindowExW")
	pDefWindowProcW        = user32.NewProc("DefWindowProcW")
	pDestroyWindow         = user32.NewProc("DestroyWindow")
	pShowWindow            = user32.NewProc("ShowWindow")
	pUpdateWindow          = user32.NewProc("UpdateWindow")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pPeekMessageW          = user32.NewProc("PeekMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessageW      = user32.NewProc("DispatchMessageW")
	pInvalidateRect        = user32.NewProc("InvalidateRect")
	pBeginPaint            = user32.NewProc("BeginPaint")
	pEndPaint              = user32.NewProc("EndPaint")
	pGetClientRect         = user32.NewProc("GetClientRect")
	pAdjustWindowRectEx    = user32.NewProc("AdjustWindowRectEx")
	pGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	pSetTimer              = user32.NewProc("SetTimer")
	pKillTimer             = user32.NewProc("KillTimer")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pMessageBoxW           = user32.NewProc("MessageBoxW")
	pSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	pSetWindowPos          = user32.NewProc("SetWindowPos")
	pLoadCursorW           = user32.NewProc("LoadCursorW")
	pSetCursor             = user32.NewProc("SetCursor")
	pFillRect              = user32.NewProc("FillRect")
	pFrameRect             = user32.NewProc("FrameRect")
	pDrawTextW             = user32.NewProc("DrawTextW")
	pGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW") // 在 kernel32，不是 user32
	pCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	pCreatePen             = gdi32.NewProc("CreatePen")
	pMoveToEx              = gdi32.NewProc("MoveToEx")
	pLineTo                = gdi32.NewProc("LineTo")
	pCreateFontW           = gdi32.NewProc("CreateFontW")
	pSelectObject          = gdi32.NewProc("SelectObject")
	pDeleteObject          = gdi32.NewProc("DeleteObject")
	pSetTextColor          = gdi32.NewProc("SetTextColor")
	pSetBkMode             = gdi32.NewProc("SetBkMode")
	pGetDeviceCaps         = gdi32.NewProc("GetDeviceCaps")
	pDwmSetWindowAttribute = syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
)

// ---- Win32 常量（只列用到的） ----
const (
	wsPopup      = 0x80000000
	wsCaption    = 0x00C00000
	wsSysmenu    = 0x00080000
	wsVisible    = 0x10000000
	wsExDlgFrame = 0x00000001 // WS_EX_DLGMODALFRAME：细边框、无最大化
	swShow       = 5
	pmRemove     = 1

	wmDestroy    = 0x0002
	wmPaint      = 0x000F
	wmClose      = 0x0010
	wmEraseBkgnd = 0x0014
	wmSetCursor  = 0x0020
	wmKeyDown    = 0x0100
	wmTimer      = 0x0113
	wmMouseMove  = 0x0200
	wmLBtnDown   = 0x0201

	htClient = 1
	idcArrow = 32512
	idcHand  = 32649

	vkEscape = 0x1B
	vkReturn = 0x0D
	vkSpace  = 0x20

	transparent    = 1
	dtLeft         = 0x0000
	dtCenter       = 0x0001
	dtVCenter      = 0x0004
	dtWordBreak    = 0x0010
	dtSingleLine   = 0x0020
	dtNoPrefix     = 0x0800
	psSolid        = 0
	antialiasedQ   = 4
	defaultCharset = 1
	dwmDarkMode    = 20 // DWMWA_USE_IMMERSIVE_DARK_MODE（1903+ 之前是 19，两个都试）
)

// 配色取自设计稿：--bg / --card / --accent / 前景灰阶
const (
	colBg     = 0x000D0B0B // #0B0B0D
	colCard   = 0x001A1616 // #16161A 悬停底 / 次级按钮底
	colTrack  = 0x00231E1E // #1E1E23 分隔线 / 进度槽
	colBorder = 0x00312A2A // #2A2A31 描边
	colBrand  = 0x001200E6 // #E60012
	colTitle  = 0x00F4F2F2 // #F2F2F4
	colBody   = 0x00A29A9A // #9A9AA2
	colHint   = 0x00736B6B // #6B6B73
	colDim    = 0x00453F3F // #3F3F45 不可用行
	colWhite  = 0x00FFFFFF
)

// 画笔/画刷下标
const (
	brBg = iota
	brCard
	brTrack
	brBrand
	brBorder
	brWhite
	brCount
)

// 字体下标
const (
	fBig   = iota // 16/600 进度大标题
	fTitle        // 15/600 选择态标题
	fBody         // 13/400 行标题 / 按钮
	fHint         // 11/400 提示 / 状态 / 脚注
	fCount
)

// 逻辑像素（96 dpi）下的版面，实际坐标一律过 px()
// 2026-10-19 精简：抛弃官方兼容思路，清单 5 行 → 4 行（去掉「撤销接管」，
// 接管语义简化为「开机自启」一行），窗口 288 → 252。
const (
	lgW   = 360.0 // 2026-10-20：320 → 360，行距与状态区一起放大（用户反馈「显示乱」）
	lgPad = 16.0

	rowTop = 54.0 // 第一行清单的顶边
	rowH   = 40.0 // 每行高（标题 + 提示两行）

	// 进度态高度固定（paintProgress 的版面按这个高度排）。
	// 选择态高度随可见行数走 —— 见 choiceH/statusY/btnYOf。
	lgProgressH = 258.0
	lgBarY      = 160.0
	lgBarH      = 4.0

	btnW = 80.0
	btnH = 28.0
)

// visibleRows 当前可见的清单行（optRows 下标）。
// 2026-10-20（用户反馈「卸载时灰行多余」）：卸载模式下「重启音频设备」
// 与「开机自启」两行**不再变灰挂着**，直接隐藏 —— 卸载语境里它们没有
// 意义，清单只剩「音频驱动组件（必需）」+「卸载驱动」两行。
func visibleRows(uninst bool) []int {
	if uninst {
		return []int{0, 2}
	}
	return []int{0, 1, 2, 3}
}

// statusY 状态区顶边（分隔线下）。
func statusY(uninst bool) float64 {
	return rowTop + float64(len(visibleRows(uninst)))*rowH + 14
}

// btnYOf 按钮区顶边。
func btnYOf(uninst bool) float64 { return statusY(uninst) + 52 }

// choiceH 选择态窗口逻辑高度。
func choiceH(uninst bool) float64 { return btnYOf(uninst) + btnH + 14 }

// uiResizeTo 按新的逻辑高度缩放窗口（保持位置）。切模式/切相位时调用。
func uiResizeTo(h float64) {
	if uiHwnd == 0 {
		return
	}
	rc := rectT{0, 0, px(lgW), px(h)}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rc)),
		uintptr(wsPopup|wsCaption|wsSysmenu|wsVisible), 0, uintptr(wsExDlgFrame))
	pSetWindowPos.Call(uintptr(uiHwnd), 0, 0, 0,
		uintptr(rc.right-rc.left), uintptr(rc.bottom-rc.top), 0x2|0x4) // SWP_NOMOVE|SWP_NOZORDER
}

// ---- 选择态：清单内容 ----
//
// 只有 108B.Inf 一个驱动包，pnputil 是整包装，**分不出「只装内核不装 APO」** ——
// 签名 .cat 覆盖 INF 内容，改一个字就签名失效。所以清单里「驱动组件」是必需项：
// 勾选锁定、点不动，右侧标「必需」，用户仍能看清这一包装了什么。
// 独立开关只剩「重启设备」「装还是卸」「开机自启」三个，没有别的了 ——
// 新框架直接抛弃官方旧面板，官方兼容的补救动作（撤销接管）不进界面，
// 需要时用 --takeover-undo 命令行。
var optRows = []optRow{
	{"音频驱动组件", "内核驱动 + 音效 APO + 面板扩展", true},
	{"重启音频设备", "让音效立即生效，无需重新插拔", false},
	{"卸载驱动", "从系统移除，耳机将暂时不可用", false},
	{"开机自启", "开机自动运行 iKF Audio Center", false},
}

type optRow struct {
	label string
	hint  string
	req   bool
}

// optSel 是这次要对系统做的事。
type optSel struct {
	install    bool
	restart    bool
	uninstall  bool
	takeover   bool // P5-3：装完把 Run\Cm108BSound + CLSID {125C480D} 指向主程序
	untakeover bool // P5-3：反向操作，入口还原旧面板（与 takeover 互斥）
}

type uiRunCfg struct {
	state       *drvState                              // 选择态显示现状用（可空：开窗后由 probe 异步填充）
	needChoice  bool                                   // false = 跳过选择态，直接进进度
	presel      optSel                                 // 选择态初值 / 跳过选择态时的固定选择
	labels      func(optSel) (title, sub, foot string) // 进度态文案随选择变
	work        func(optSel) (string, bool)
	probe       func() *drvState // 开窗后异步探测（state 为 nil 时启用）
	autoConfirm time.Duration    // >0 时选择态停这么久后自动确认（--demo 用）
}

type uiOut struct {
	sel       optSel
	detail    string
	ok        bool
	cancelled bool
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type rectT struct{ left, top, right, bottom int32 }

type pointT struct{ x, y int32 }

type msgT struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      pointT
}

type paintStruct struct {
	hdc         syscall.Handle
	fErase      int32
	rcPaint     rectT
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

var (
	dpiScale = 1.0

	uiMu                   sync.Mutex
	uiHwnd                 syscall.Handle
	uiPhase                int32 // phaseChoice / phaseProgress
	uiUninst               bool  // 卸载模式（清单互斥状态）
	uiRestart              = true
	uiTakeover             bool // 接管（仅安装模式可点，与撤销互斥）
	uiUntake               bool // 撤销接管
	uiDrv                  *drvState
	uiTitle, uiSub, uiFoot string
	uiMarquee              float64
	uiTrackW               float64

	uiHotRow int32 = -1
	uiHotBtn int32 = -1
	uiMouseX int32
	uiMouseY int32

	uiConfirmed bool // 用户点了主按钮（只在选择态有效）
	uiCancelled bool

	uiBrushes  [brCount]syscall.Handle
	uiFonts    [fCount]syscall.Handle
	uiWndClass = windows.StringToUTF16Ptr("iKFSetupWnd")
	uiWndTitle = windows.StringToUTF16Ptr("iKF 驱动管理")
)

const (
	phaseChoice   = 0
	phaseProgress = 1
)

func px(v float64) int32 { return int32(v*dpiScale + 0.5) }

func lpX(l uintptr) int32 { return int32(int16(uint16(l & 0xFFFF))) }         //nolint
func lpY(l uintptr) int32 { return int32(int16(uint16((l >> 16) & 0xFFFF))) } //nolint

// initDPI 声明 DPI 感知并量出缩放比。
// 不声明的话高 DPI 屏上整个窗口会被系统位图拉伸，字发虚。
func initDPI() {
	pSetProcessDPIAware.Call()
	hdc, _, _ := pGetDC.Call(0)
	if hdc != 0 {
		if v, _, _ := pGetDeviceCaps.Call(hdc, 88 /*LOGPIXELSY*/); v > 0 {
			dpiScale = float64(v) / 96.0
		}
		pReleaseDC.Call(0, hdc)
	}
}

func makeFont(pxSize float64, weight int32) syscall.Handle {
	face := windows.StringToUTF16Ptr("Microsoft YaHei UI")
	h, _, _ := pCreateFontW.Call(
		uintptr(int32(-px(pxSize))), 0, 0, 0, uintptr(weight), 0, 0, 0,
		defaultCharset, 0, 0, antialiasedQ, 0, uintptr(unsafe.Pointer(face)))
	return syscall.Handle(h)
}

// ---- 命中测试 ----

// rowAt 返回鼠标下的清单行；-1 = 不在任何行上。
func rowAt(x, y int32, uninst bool) int {
	if x < px(lgPad-4) || x > px(lgW-lgPad+4) {
		return -1
	}
	for pos, i := range visibleRows(uninst) {
		top := px(rowTop + float64(pos)*rowH - 5)
		bot := px(rowTop + float64(pos)*rowH + rowH - 6)
		if y >= top && y <= bot {
			return i
		}
	}
	return -1
}

// btnAt 返回鼠标下的按钮：0 无 / 1 次按钮（取消） / 2 主按钮（安装·卸载）。
func btnAt(x, y int32, uninst bool) int {
	bY := btnYOf(uninst)
	if y < px(bY) || y > px(bY+btnH) {
		return 0
	}
	if x >= px(lgW-lgPad-btnW) {
		return 2
	}
	if x >= px(lgW-lgPad-btnW-btnW-8) {
		return 1
	}
	return 0
}

// rowLive 说这一行当前能不能点。
// 「驱动组件」是必需项，常态下点不动；只有在卸载模式下点它才有意义（= 退出卸载）。
// 「重启设备」「开机自启」只在安装模式有意义。
func rowLive(i int) bool {
	// 2026-10-20：行隐藏后，能被 rowAt 命中的行全部可交互
	// （必需行点了 = 退出卸载模式，语义仍在 toggleRow 里）。
	return i >= 0 && i < len(optRows)
}

func toggleRow(i int) {
	switch i {
	case 0: // 驱动组件：从卸载模式退回安装模式
		if uiUninst {
			uiUninst, uiRestart = false, true
		}
	case 1: // 重启设备
		if !uiUninst {
			uiRestart = !uiRestart
		}
	case 2: // 卸载：与上面各行互斥
		if uiUninst {
			uiUninst, uiRestart = false, true
		} else {
			uiUninst, uiRestart = true, false
		}
	case 3: // 开机自启（写官方入口 = 接管；GUI 不再提供撤销，CLI --takeover-undo 保留）
		if !uiUninst {
			uiTakeover = !uiTakeover
		}
	}
}

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmEraseBkgnd:
		return 1 // 全在 WM_PAINT 里画，不要系统先刷一遍白底
	case wmPaint:
		paint(hwnd)
		return 0
	case wmTimer:
		// ★ 界面闪烁修复（2026-10-19）：
		//   1. 选择态是**完全静态**的 —— 不失效任何区域，一颗像素都不重画
		//      （原实现选择态也 40 次/秒整窗重画，这就是闪烁）。
		//   2. 进度态只失效进度条那一小条：marquee 亮条在这里滚，其余文字
		//      与清单不动。BeginPaint 会把绘制裁剪到失效区，整窗重画消失。
		uiMu.Lock()
		if uiPhase != phaseProgress {
			uiMu.Unlock()
			return 0
		}
		uiMarquee += 5.5 * dpiScale
		if uiMarquee > uiTrackW+float64(px(96)) {
			uiMarquee = -float64(px(96))
		}
		h := uiHwnd
		trackW := int32(uiTrackW)
		uiMu.Unlock()
		if h != 0 {
			barY := px(lgBarY)
			barH := px(lgBarH)
			if barH < 2 {
				barH = 2
			}
			rc := rectT{px(24) - px(2), barY - px(2), px(24) + trackW + px(2), barY + barH + px(2)}
			pInvalidateRect.Call(uintptr(h), uintptr(unsafe.Pointer(&rc)), 0)
		}
		return 0
	case wmMouseMove:
		uiMouseX, uiMouseY = lpX(lparam), lpY(lparam)
		hotRow, hotBtn := int32(-1), int32(0)
		if uiPhase == phaseChoice {
			if r := rowAt(uiMouseX, uiMouseY, uiUninst); r >= 0 && rowLive(r) {
				hotRow = int32(r)
			}
			hotBtn = int32(btnAt(uiMouseX, uiMouseY, uiUninst))
		}
		uiMu.Lock()
		changed := hotRow != uiHotRow || hotBtn != uiHotBtn
		uiHotRow, uiHotBtn = hotRow, hotBtn
		uiMu.Unlock()
		if changed {
			pInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmSetCursor:
		if lpX(lparam) == htClient && (uiHotRow >= 0 || uiHotBtn != 0) {
			pSetCursor.Call(uintptr(loadCursor(idcHand)))
			return 1
		}
		return 0
	case wmLBtnDown:
		if uiPhase != phaseChoice {
			return 0
		}
		x, y := lpX(lparam), lpY(lparam)
		switch btnAt(x, y, uiUninst) {
		case 1:
			uiCancelled = true
		case 2:
			uiConfirmed = true
		default:
			if r := rowAt(x, y, uiUninst); r >= 0 && rowLive(r) {
				toggleRow(r)
				uiResizeTo(choiceH(uiUninst)) // 卸载/安装切换 → 行数变化 → 窗口高度跟随
				pInvalidateRect.Call(hwnd, 0, 0)
			}
		}
		return 0
	case wmKeyDown:
		if uiPhase != phaseChoice {
			return 0
		}
		switch wparam {
		case vkEscape:
			uiCancelled = true
		case vkReturn, vkSpace:
			uiConfirmed = true
		}
		return 0
	case wmClose:
		// 干活期间不许关，否则会留下装到一半的系统；选择态关窗 = 取消
		if uiPhase == phaseChoice {
			uiCancelled = true
		}
		return 0
	case wmDestroy:
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// ---- 绘制 ----

func paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc rectT
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	fill(hdc, 0, 0, rc.right, rc.bottom, uiBrushes[brBg])
	pSetBkMode.Call(hdc, transparent)

	uiMu.Lock()
	phase, uninst, restart := uiPhase, uiUninst, uiRestart
	takeover := uiTakeover
	title, sub, foot, pos, trackW := uiTitle, uiSub, uiFoot, uiMarquee, uiTrackW
	hotRow, hotBtn := uiHotRow, uiHotBtn
	uiMu.Unlock()

	if phase == phaseChoice {
		paintChoice(hdc, uninst, restart, takeover, hotRow, hotBtn)
		return
	}
	paintProgress(hdc, title, sub, foot, pos, trackW)
}

func paintChoice(hdc uintptr, uninst, restart, takeover bool, hotRow, hotBtn int32) {
	title := "选择要执行的组件"
	if uninst {
		title = "卸载驱动组件"
	}
	put(hdc, title, lgPad, 14, lgW-2*lgPad, 20, 15, 600, colTitle, uiFonts[fTitle],
		dtLeft|dtVCenter|dtSingleLine)
	hr(hdc, 40, colTrack)

	vis := visibleRows(uninst)
	for pos, i := range vis {
		r := optRows[i]
		y := rowTop + float64(pos)*rowH
		var on, off, lock bool
		switch i {
		case 0:
			on, lock = true, true // 必需：永远勾着，点不动
		case 1:
			on, off = !uninst && restart, uninst
		case 2:
			on = uninst
		case 3:
			on, off = !uninst && takeover, uninst
		}
		if hotRow == int32(i) {
			fill(hdc, px(lgPad-6), px(y-6), px(lgW-lgPad+6), px(y+rowH-8), uiBrushes[brCard])
		}
		// 复选方框。锁定态灰底暗勾（一眼是「固定的」）；普通勾选品牌红。
		cbX, cbY := px(lgPad), px(y+4)
		cbR, cbB := px(lgPad+13), px(y+17)
		if on {
			if lock {
				fill(hdc, cbX, cbY, cbR, cbB, uiBrushes[brTrack])
				frame(hdc, cbX, cbY, cbR, cbB, uiBrushes[brBorder])
				checkMark(hdc, cbX, cbY, colBody)
			} else {
				fill(hdc, cbX, cbY, cbR, cbB, uiBrushes[brBrand])
				checkMark(hdc, cbX, cbY, colWhite)
			}
		} else {
			frame(hdc, cbX, cbY, cbR, cbB, uiBrushes[brBorder])
		}
		// 必需项只是「不能改」，不是「不可用」—— 标题别跟真禁用一样灰
		tc, hc := uint32(colTitle), uint32(colHint)
		if lock {
			tc = colBody
		}
		if off {
			tc, hc = colDim, colDim
		}
		put(hdc, r.label, lgPad+24, y, 190, 18, 13, 400, tc, uiFonts[fBody],
			dtLeft|dtVCenter|dtSingleLine)
		put(hdc, r.hint, lgPad+24, y+18, 240, 16, 11, 400, hc, uiFonts[fHint],
			dtLeft|dtVCenter|dtSingleLine)
		if r.req {
			// 「必需」徽标：底色块 + 居中文字，比裸灰字更像状态标签
			bx, by := px(lgW-lgPad-46), px(y)
			fill(hdc, bx, by, px(lgW-lgPad), px(y+18), uiBrushes[brTrack])
			put(hdc, "必需", float64(bx)/dpiScale, float64(by)/dpiScale, 46, 18, 10, 400,
				colHint, uiFonts[fHint], dtCenter|dtVCenter|dtSingleLine)
		}
	}

	// 状态区：两行 label:value（2026-10-20 从单行长句拆开，不再挤一行）
	stY := statusY(uninst)
	hr(hdc, stY-10, colTrack)
	lib, dev := "检测中…", "检测中…"
	if uiDrv != nil {
		lib, dev = "未安装", "耳机未连接"
		if len(uiDrv.published) > 0 {
			lib = "已安装"
		}
		if uiDrv.deviceID != "" {
			dev = "耳机已连接"
		}
	}
	put(hdc, "驱动状态", lgPad, stY, 64, 18, 11, 400, colHint, uiFonts[fHint],
		dtLeft|dtVCenter|dtSingleLine)
	put(hdc, lib, lgPad+70, stY, lgW-lgPad*2-70, 18, 11, 400, colBody, uiFonts[fHint],
		dtLeft|dtVCenter|dtSingleLine)
	put(hdc, "设备连接", lgPad, stY+19, 64, 18, 11, 400, colHint, uiFonts[fHint],
		dtLeft|dtVCenter|dtSingleLine)
	put(hdc, dev, lgPad+70, stY+19, lgW-lgPad*2-70, 18, 11, 400, colBody, uiFonts[fHint],
		dtLeft|dtVCenter|dtSingleLine)

	// 按钮：右下角，取消在前、主按钮在后
	bY := btnYOf(uninst)
	cx, cy := px(lgW-lgPad-2*btnW-8), px(bY)
	cw, chh := px(btnW), px(btnH)
	bg := uiBrushes[brCard]
	if hotBtn == 1 {
		bg = uiBrushes[brBorder]
	}
	fill(hdc, cx, cy, cx+cw, cy+chh, bg)
	frame(hdc, cx, cy, cx+cw, cy+chh, uiBrushes[brBorder])
	put(hdc, "取消", float64(cx)/dpiScale, bY, btnW, btnH, 13, 400, colBody, uiFonts[fBody],
		dtCenter|dtVCenter|dtSingleLine)

	ox := px(lgW - lgPad - btnW)
	fill(hdc, ox, cy, ox+cw, cy+chh, uiBrushes[brBrand])
	label := "安装"
	if uninst {
		label = "卸载"
	}
	put(hdc, label, float64(ox)/dpiScale, bY, btnW, btnH, 13, 600, colWhite, uiFonts[fBody],
		dtCenter|dtVCenter|dtSingleLine)
}

func paintProgress(hdc uintptr, title, sub, foot string, pos, trackW float64) {
	put(hdc, title, 24, 76, lgW-48, 24, 16, 600, colTitle, uiFonts[fBig],
		dtLeft|dtVCenter|dtSingleLine)
	put(hdc, sub, 24, 104, lgW-48, 44, 13, 400, colBody, uiFonts[fBody], dtLeft|dtWordBreak)

	// 进度条：底槽 + 一段来回滚的亮条（自己裁剪到槽内）
	// 几何与 wmTimer 的失效区共用 lgBarY/lgBarH（两处不一致会漏画）。
	barY := px(lgBarY)
	barH := px(lgBarH)
	if barH < 2 {
		barH = 2
	}
	fill(hdc, px(24), barY, px(24)+int32(trackW), barY+barH, uiBrushes[brTrack])
	x0 := px(24) + int32(pos)
	x1 := x0 + px(96)
	if lim := px(24) + int32(trackW); x1 > lim {
		x1 = lim
	}
	if x0 < px(24) {
		x0 = px(24)
	}
	if x1 > x0 {
		fill(hdc, x0, barY, x1, barY+barH, uiBrushes[brBrand])
	}
	put(hdc, foot, 24, 176, lgW-48, 44, 11, 400, colHint, uiFonts[fHint], dtLeft|dtWordBreak)
}

// statusLine 是选择态底部那行现状。2026-10-19 用户定稿：极简 + 官方语气，
// 不显示发布名（oemNN.inf），连接状态统一「耳机已连接 / 耳机未连接」句式。
func statusLine(s *drvState) string {
	if s == nil {
		return "正在检测…"
	}
	lib := "驱动未安装"
	if len(s.published) > 0 {
		lib = "驱动已安装"
	}
	dev := "耳机未连接"
	if s.deviceID != "" {
		dev = "耳机已连接"
	}
	return lib + " · " + dev
}

func checkMark(hdc uintptr, x0, y0 int32, col uint32) {
	pen, _, _ := pCreatePen.Call(psSolid, uintptr(px(1.8)), uintptr(col))
	if pen == 0 {
		return
	}
	old, _, _ := pSelectObject.Call(hdc, pen)
	pMoveToEx.Call(hdc, uintptr(x0+px(3)), uintptr(y0+px(6.5)), 0)
	pLineTo.Call(hdc, uintptr(x0+px(5.5)), uintptr(y0+px(9)))
	pLineTo.Call(hdc, uintptr(x0+px(10)), uintptr(y0+px(3.5)))
	pSelectObject.Call(hdc, old)
	pDeleteObject.Call(uintptr(pen))
}

func fill(hdc uintptr, l, t, r, b int32, br syscall.Handle) {
	rc := rectT{l, t, r, b}
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), uintptr(br))
}

func frame(hdc uintptr, l, t, r, b int32, br syscall.Handle) {
	rc := rectT{l, t, r, b}
	pFrameRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), uintptr(br))
}

func hr(hdc uintptr, y float64, col uint32) {
	br, _, _ := pCreateSolidBrush.Call(uintptr(col))
	if br == 0 {
		return
	}
	fill(hdc, px(lgPad), px(y), px(lgW-lgPad), px(y)+1, syscall.Handle(br))
	pDeleteObject.Call(br)
}

// put 在给定逻辑矩形里画一段字；flags 决定单行/换行与对齐。
func put(hdc uintptr, s string, x, y, w, h, size float64, weight int32, col uint32,
	f syscall.Handle, flags uintptr) {
	if s == "" || f == 0 {
		return
	}
	old, _, _ := pSelectObject.Call(hdc, uintptr(f))
	pSetTextColor.Call(hdc, uintptr(col))
	rc := rectT{px(x), px(y), px(x + w), px(y + h)}
	u, _ := syscall.UTF16FromString(s)
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1),
		uintptr(unsafe.Pointer(&rc)), flags|dtNoPrefix)
	pSelectObject.Call(hdc, old)
}

// ---- 窗口生命周期 ----

// uiRun 建窗、维持消息循环、返回工作结果。
//
// 选择态由用户勾选并确认；确认后同一窗口切到进度态、后台跑 work。
// 建不出窗口时不弹窗也不放弃 —— 退回一个 Yes/No 消息框把活干完，
// 不能因为画不出来就不装驱动。
func uiRun(cfg uiRunCfg) uiOut {
	// ★「未响应」根因修复（2026-10-19）：Win32 窗口的消息队列绑在**创建它的
	//   OS 线程**上，而 Go 调度器会随意迁移 goroutine —— 这个 goroutine 一旦
	//   被挪去别的线程，PeekMessage 就永远读不到窗口队列，窗口进入 ghost
	//   「未响应」态。装驱动期间 pnputil 在后台跑、GC 一活跃，迁移概率陡增，
	//   与「时灵时不灵」的卡死完全吻合（tray.go 早就是 LockOSThread 同款，
	//   installer 漏了）。整个 UI 生命周期（建窗 → 消息循环 → 销窗）钉死线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	initBrushes()
	uiMu.Lock()
	uiPhase = phaseChoice
	uiUninst, uiRestart = cfg.presel.uninstall, cfg.presel.restart
	uiTakeover, uiUntake = cfg.presel.takeover, cfg.presel.untakeover
	uiDrv = cfg.state
	uiTitle, uiSub, uiFoot = "", "", ""
	uiHotRow, uiHotBtn, uiMouseX, uiMouseY = -1, 0, -999, -999
	uiConfirmed, uiCancelled = false, false
	uiMu.Unlock()

	ex, _, _ := pGetModuleHandleW.Call(0)
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(ex),
		hCursor:       loadCursor(idcArrow),
		hbrBackground: 0,
		lpszClassName: uiWndClass,
	}
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	style := uint32(wsPopup | wsCaption | wsSysmenu | wsVisible)
	exStyle := uint32(wsExDlgFrame)
	rc := rectT{0, 0, px(lgW), px(choiceH(cfg.presel.uninstall))}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rc)), uintptr(style), 0, uintptr(exStyle))
	cw := int(rc.right - rc.left)
	ch := int(rc.bottom - rc.top)
	sw, _, _ := pGetSystemMetrics.Call(0)
	sh, _, _ := pGetSystemMetrics.Call(1)

	hwnd, _, _ := pCreateWindowExW.Call(
		uintptr(exStyle), uintptr(unsafe.Pointer(uiWndClass)), uintptr(unsafe.Pointer(uiWndTitle)),
		uintptr(style),
		uintptr((int(sw)-cw)/2), uintptr((int(sh)-ch)/2), uintptr(cw), uintptr(ch),
		0, 0, ex, 0)
	if hwnd == 0 {
		sel := cfg.presel
		if cfg.needChoice {
			if a := fallbackChoice(); a != "" {
				sel = optSel{}
				if a == "uninstall" {
					sel.uninstall = true
				} else {
					sel.install, sel.restart = true, true
				}
			} else {
				return uiOut{cancelled: true}
			}
		}
		det, ok := cfg.work(sel)
		return uiOut{sel: sel, detail: det, ok: ok}
	}
	uiMu.Lock()
	uiHwnd = syscall.Handle(hwnd)
	uiTrackW = float64(px(lgW) - px(24)*2)
	uiMu.Unlock()

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pSetForegroundWindow.Call(hwnd)
	darkenTitleBar(hwnd)
	pSetTimer.Call(hwnd, 1, 25, 0)

	// 稳定性修复（2026-10-19）：探测放后台，窗口先弹 —— 开窗前那两三轮
	// pnputil 枚举（UAC 确认后黑窗干等好几秒）是「双击没反应」体感的
	// 主要来源。结果稍后填进底部状态行；用户在探测完成前就点确认也没
	// 关系，work 闭包会重新拿新鲜状态（见 main.go）。
	if cfg.probe != nil && cfg.state == nil {
		go func() {
			st := cfg.probe()
			note("后台探测完成: published=%v device=%q", st.published, st.deviceID)
			uiMu.Lock()
			uiDrv = st
			h := uiHwnd
			uiMu.Unlock()
			if h != 0 {
				pInvalidateRect.Call(uintptr(h), 0, 0)
			}
		}()
	}

	var m msgT
	start := time.Now()
	workStart := time.Time{}
	done := make(chan struct{})
	var sel optSel
	var detail string
	var ok bool

	for {
		for {
			if r, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove); r == 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}

		if uiPhase == phaseChoice {
			if cfg.autoConfirm > 0 && time.Since(start) >= cfg.autoConfirm {
				uiConfirmed = true
			}
			if uiCancelled {
				break
			}
			if uiConfirmed {
				uiMu.Lock()
				if uiUninst {
					sel = optSel{uninstall: true}
				} else {
					sel = optSel{install: true, restart: uiRestart,
						takeover: uiTakeover, untakeover: uiUntake}
				}
				t, s, f := cfg.labels(sel)
				uiTitle, uiSub, uiFoot = t, s, f
				uiPhase = phaseProgress
				uiHotRow, uiHotBtn = -1, 0
				uiMu.Unlock()
				uiResizeTo(lgProgressH) // 进度态版面按 258 高排，切回去
				workStart = time.Now()
				go func() {
					// 闪退不留痕的修复：work 里的任何 panic 不再让窗口无声
					// 消失 —— 落日志（失败时才写盘）+ 走失败弹框分支。
					defer func() {
						if r := recover(); r != nil {
							note("work panic: %v\n%s", r, debug.Stack())
							detail = "内部错误（详情已写入 iKF-驱动管理.log）：\n" + fmt.Sprint(r)
							ok = false
						}
						// 太快完成会让窗口一闪而过，看着像没跑；至少留 700ms
						if d := 700*time.Millisecond - time.Since(workStart); d > 0 {
							time.Sleep(d)
						}
						close(done)
					}()
					detail, ok = cfg.work(sel)
				}()
			}
			time.Sleep(8 * time.Millisecond)
			continue
		}

		select {
		case <-done:
			<-done
			pKillTimer.Call(hwnd, 1)
			pDestroyWindow.Call(hwnd)
			uiMu.Lock()
			uiHwnd = 0
			uiMu.Unlock()
			return uiOut{sel: sel, detail: detail, ok: ok}
		default:
			time.Sleep(8 * time.Millisecond)
		}
	}

	pKillTimer.Call(hwnd, 1)
	pDestroyWindow.Call(hwnd)
	uiMu.Lock()
	uiHwnd = 0
	uiMu.Unlock()
	return uiOut{cancelled: true}
}

// setStatus 改副标（唯一允许的跨线程写入口，后台 goroutine 用它报步骤）。
func setStatus(s string) {
	uiMu.Lock()
	prev := uiSub
	if s != "" {
		uiSub = s
	}
	h := uiHwnd
	uiMu.Unlock()
	if h != 0 && prev != uiSub {
		pInvalidateRect.Call(uintptr(h), 0, 0)
	}
}

func initBrushes() {
	uiMu.Lock()
	defer uiMu.Unlock()
	if uiBrushes[0] != 0 {
		return
	}
	for i, c := range []uint32{colBg, colCard, colTrack, colBrand, colBorder, colWhite} {
		h, _, _ := pCreateSolidBrush.Call(uintptr(c))
		uiBrushes[i] = syscall.Handle(h)
	}
	uiFonts[fBig] = makeFont(16, 600)
	uiFonts[fTitle] = makeFont(15, 600)
	uiFonts[fBody] = makeFont(13, 400)
	uiFonts[fHint] = makeFont(11, 400)
	uiMarquee = -float64(px(96))
}

func releaseBrushes() {
	uiMu.Lock()
	defer uiMu.Unlock()
	for _, h := range uiBrushes {
		if h != 0 {
			pDeleteObject.Call(uintptr(h))
		}
	}
	for _, h := range uiFonts {
		if h != 0 {
			pDeleteObject.Call(uintptr(h))
		}
	}
}

func loadCursor(id uintptr) syscall.Handle {
	h, _, _ := pLoadCursorW.Call(0, id)
	return syscall.Handle(h)
}

// attachConsole 在父进程有控制台时把自己接上去。
//
// 本程序是 GUI 子系统（-H windowsgui），默认不占控制台 —— 双击时不弹黑框。
// 但从终端跑 --status 这类诊断时，输出得能看见，否则主程序和构建脚本没法用。
// ATTACH_PARENT_PROCESS 失败就当没有，不影响界面模式。
func attachConsole() {
	// 已经被重定向到文件/管道就别动 —— 那种情况下句柄本来就是好的
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	kernel32.NewProc("AttachConsole").Call(^uintptr(0)) // (DWORD)-1
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
	}
}

// darkenTitleBar 把标题栏也刷成深色，跟客户区接得上。
// 只在 Win10 1903+ 有效，失败无所谓（就是根浅色标题栏）。
func darkenTitleBar(hwnd uintptr) {
	var on int32 = 1
	if r, _, _ := pDwmSetWindowAttribute.Call(hwnd, dwmDarkMode,
		uintptr(unsafe.Pointer(&on)), unsafe.Sizeof(on)); r != 0 {
		pDwmSetWindowAttribute.Call(hwnd, 19, uintptr(unsafe.Pointer(&on)), unsafe.Sizeof(on))
	}
}

// ---- 消息框 ----

const (
	mbOK           = 0x00000000
	mbYesNo        = 0x00000004
	mbIconError    = 0x00000010
	mbIconQuestion = 0x00000020
	mbIconWarning  = 0x00000030
	mbIconInfo     = 0x00000040
	mbDefButton2   = 0x00000100
	idYes          = 6
)

func msgBox(body, caption string, flags uint32) int {
	t, _ := syscall.UTF16FromString(body)
	c, _ := syscall.UTF16FromString(caption)
	r, _, _ := pMessageBoxW.Call(0, uintptr(unsafe.Pointer(&t[0])),
		uintptr(unsafe.Pointer(&c[0])), uintptr(flags))
	return int(int32(r))
}
