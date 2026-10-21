//go:build windows

// 单实例守卫（2026-10-20 用户报障：连点两次 exe 会起两个进程）。
// ====================================================================
//
// 为什么不用 Wails 的 SingleInstanceLock：v2.16 的实现有 bug ——
// SetupSingleInstance 创建的事件窗口是 HWND_MESSAGE 消息专用窗口，
// 但查找用 FindWindowW，它只搜顶层窗口、搜不到消息专用窗口（Win32
// API 规定，必须用 FindWindowExW 并以 HWND_MESSAGE 为父窗口）。
// 于是第二个实例永远找不到第一个 → 锁形同虚设（实测双开，且两个
// WebView2 抢同一用户数据目录互杀）。
//
// 自己实现，三步：
//   1. guardSingleInstance 在 main() 最开头调用（先于 drv.Open）——
//      CreateMutex 判重；已有实例则 FindWindowExW(HWND_MESSAGE,…)
//      定位其消息窗口、WM_COPYDATA 转交通知，然后退出；
//   2. 第一实例的接收窗口（本文件 siWndProc）收到后唤出主界面
//      （appCtx 就绪前收到就只置标志，OnStartup 后统一补一次）；
//   3. 接收窗口跑在独立 LockOSThread goroutine 的消息泵里。

package main

import (
	"log"
	"runtime"
	"syscall"
	"unsafe"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
)

const (
	siMutex    = `Local\iKF-Audio-Center-sim`
	siClass    = `iKF-Audio-Center-sic`
	siWindow   = `iKF-Audio-Center-siw`
	wmCOPYDATA = 0x004A
	siMagic    = 0x494B4653 // "IKFS" —— 自定义 COPYDATA 标识
)

var siHwnd uintptr

// guardSingleInstance 返回 true = 已有实例在运行，本进程应立即退出。
func guardSingleInstance() bool {
	mu, err := windows.CreateMutex(nil, true, windows.StringToUTF16Ptr(siMutex))
	if err == nil {
		runtime.KeepAlive(mu) // 句柄必须持有到进程退出，防 GC 触发释放
		// 第一实例：持有互斥锁（句柄不关，进程退出自动释放），建接收窗口
		siStartReceiver()
		return false
	}
	if err != windows.ERROR_ALREADY_EXISTS {
		// 拿不到锁也不是「已存在」（罕见），如实放行，别把应用锁死
		log.Println("[单实例] 互斥锁异常（放行）：", err)
		return false
	}
	// 已有实例：定位其消息窗口并转交
	hwnd := siFindReceiver()
	if hwnd == 0 {
		// 上一实例正在退出（锁残留但窗口已销毁）：当作可启动处理，
		// 互斥锁句柄随本进程退出释放，不会永久卡死
		log.Println("[单实例] 上一实例窗口已销毁（退出中），本次放行")
		return false
	}
	siNotify(hwnd)
	return true
}

// siFindReceiver 用 FindWindowExW 找消息专用窗口 —— 关键差异：
// FindWindowW 不搜 HWND_MESSAGE 窗口（Wails v2.16 的 bug 根源）。
func siFindReceiver() uintptr {
	user32 := syscall.NewLazyDLL("user32.dll")
	find := user32.NewProc("FindWindowExW")
	hwndMessage := ^uintptr(2) // HWND_MESSAGE = (HWND)-3
	hwnd, _, _ := find.Call(hwndMessage, 0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(siClass))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(siWindow))))
	return hwnd
}

// siNotify 给第一实例发 WM_COPYDATA（无 payload，纯通知）。
func siNotify(hwnd uintptr) {
	type copyDataStruct struct {
		dwData uintptr
		cbData uint32
		lpData uintptr
	}
	cd := copyDataStruct{dwData: siMagic}
	user32 := syscall.NewLazyDLL("user32.dll")
	send := user32.NewProc("SendMessageW")
	send.Call(hwnd, wmCOPYDATA, 0, uintptr(unsafe.Pointer(&cd)))
}

// siStartReceiver 注册消息窗口类、建隐藏窗口，并在独立 OS 线程上泵消息。
// （GetMessageW 有线程亲和性——与托盘消息循环同一条铁律。）
func siStartReceiver() {
	go func() {
		runtime.LockOSThread()
		user32 := syscall.NewLazyDLL("user32.dll")
		register := user32.NewProc("RegisterClassExW")
		create := user32.NewProc("CreateWindowExW")

		// wndClassExW / msg 结构复用 tray.go 的定义（同包）
		wcex := wndClassExW{
			CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
			LpfnWndProc:   syscall.NewCallback(siWndProc),
			HInstance:     hInstOf(),
			HbrBackground: 6, // COLOR_WINDOW+1
			LpszClassName: uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(siClass))),
		}
		register.Call(uintptr(unsafe.Pointer(&wcex)))

		const hwndMessage = ^uintptr(2) // HWND_MESSAGE = (HWND)-3
		hwnd, _, err := create.Call(0,
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(siClass))),
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(siWindow))),
			0, 0, 0, 0, 0,
			// 第 9 参 = hWndParent 必须是 HWND_MESSAGE（消息专用窗口）；
			// hMenu 位置必须为 NULL（否则 Invalid menu handle）
			hwndMessage, 0,
			hInstOf(), 0)
		if hwnd == 0 {
			log.Println("[单实例] 接收窗口创建失败（二次启动将各起各的）：", err)
			return
		}
		siHwnd = hwnd

		getMessage := user32.NewProc("GetMessageW")
		dispatch := user32.NewProc("DispatchMessageW")
		var m msg
		for {
			r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 {
				return // WM_QUIT
			}
			dispatch.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
}

// siWndProc 接收窗口过程：收到转交通知就唤出主界面。
func siWndProc(hWnd, msgID, wParam, lParam uintptr) uintptr {
	if msgID == wmCOPYDATA {
		siShowMainWindow()
		return 0
	}
	return defWindowProc(hWnd, msgID, wParam, lParam)
}

// siShowMainWindow 唤出主界面。appCtx 未就绪（OnStartup 未跑）时挂起标志，
// 由 startTray 之后的 OnStartup 收尾 —— 不丢唤出请求。
func siShowMainWindow() {
	if appCtx == nil {
		winShown = true // OnStartup 后 StartHidden 之外都会显示，天然补上
		return
	}
	wruntime.WindowUnminimise(appCtx)
	wruntime.WindowShow(appCtx)
	winShown = true
}

func hInstOf() uintptr {
	h, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	return h
}
