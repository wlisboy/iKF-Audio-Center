//go:build windows

package main

// 拉起驱动管理程序
// ================
//
// 主程序恒定 asInvoker —— 一次 UAC 都不弹。只有用户明确点了「安装/卸载驱动组件」，
// 才用 ShellExecuteEx(runas) 把同目录的 iKF-驱动管理.exe 拉起来，并且等它结束。
//
// 为什么非要等：装/卸驱动会让 PnP 重建设备实例，本进程手里那份 osConfLib 的
// 设备上下文随之作废。不等的话，界面会接着读出一堆空值，看着像「装坏了」。
// 等它退出、重新 Open 一次，交到界面上的才是干净状态。
//
// 用 ShellExecuteEx 而不是 ShellExecute：只有前者给子进程句柄，能 Wait；
// 也只有前者能分辨「用户取消了 UAC」和「安装器自己失败」。
// 提权走 runas 动词，所以驱动管理程序不需要 requireAdministrator 清单。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setupExeName 必须与 tools/build_portable.py 的 SETUP_NAME 逐字一致
const setupExeName = "iKF-驱动管理.exe"

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskFlagNoUI       = 0x00000400
	errCancelled          = 1223
)

var pShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

type shellExecInfoW struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

// SetupPath 返回同目录下的驱动管理程序；没带就返回空串（绿色包被拆散了）。
func SetupPath() string {
	p := filepath.Join(appDir(), setupExeName)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// DriverInfo 是主程序眼里的驱动现状，/api/driver 直接返回它。
//
// 注意 ready 与 setup_ok 是两件事：前者是「能不能跑」（端点枚举得出来吗），
// 后者是「有没有带安装器」。理论上有安装器 ≠ 驱动已装。
func (d *Driver) DriverInfo() map[string]interface{} {
	setup := SetupPath()
	return map[string]interface{}{
		"ok":       true,
		"ready":    d.ready,
		"devices":  d.devCnt,
		"capture":  d.capCnt,
		"cpl":      d.cpl,
		"dll":      d.dll,
		"error":    d.errMsg,
		"setup":    setup,
		"setup_ok": setup != "",
	}
}

// RunSetup 拉起驱动管理程序并等它结束，然后重新连一次驱动。
//
// action 取 "install" / "uninstall"；空串 = 不带参数，由用户在那边自己选。
// 会阻塞几秒到几十秒 —— 前端要转圈等，别把它当秒回的接口。
func (d *Driver) RunSetup(action string) map[string]interface{} {
	setup := SetupPath()
	if setup == "" {
		return map[string]interface{}{
			"ok": false,
			"error": "同目录下找不到 " + setupExeName + "，无法安装或卸载驱动组件。",
		}
	}

	params := ""
	switch action {
	case "install":
		params = "--install"
	case "uninstall":
		params = "--uninstall"
	case "":
		// 让用户在安装器界面里选
	default:
		return map[string]interface{}{
			"ok": false, "error": "action 只能是 install 或 uninstall",
		}
	}

	// 先松手：装驱动会重启设备实例，握着旧上下文等它回来只会读出错值
	d.Close()
	code, err := runElevated(setup, params)
	d.Open()

	info := d.DriverInfo()
	if err != nil {
		info["ok"] = false
		info["error"] = "无法启动驱动管理程序：" + err.Error()
		return info
	}
	info["action"] = action
	info["exit_code"] = code
	if code != 0 {
		info["ok"] = false
		switch code {
		case errCancelled:
			info["error"] = "已取消：没有授予管理员权限，驱动组件未改动。"
		default:
			info["error"] = fmt.Sprintf("驱动管理程序以退出码 %d 结束，请查看它的界面提示。", code)
		}
	}
	return info
}

// runElevated 用 runas 拉起 exe 并等它退出，返回退出码。
//
// 提权后子进程的当前目录会变成 System32，所以工作目录必须显式给 ——
// 驱动管理程序自己也是按 exe 路径推目录的，这里是替它把环境摆正。
func runElevated(exe, params string) (int, error) {
	verb := windows.StringToUTF16Ptr("runas")
	file := windows.StringToUTF16Ptr(exe)
	dir := windows.StringToUTF16Ptr(filepath.Dir(exe))
	var par *uint16
	if strings.TrimSpace(params) != "" {
		par = windows.StringToUTF16Ptr(params)
	}

	var sei shellExecInfoW
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	sei.fMask = seeMaskNoCloseProcess | seeMaskFlagNoUI
	sei.lpVerb = verb
	sei.lpFile = file
	sei.lpParameters = par
	sei.lpDirectory = dir
	sei.nShow = windows.SW_SHOWNORMAL

	r, _, e := pShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		return 0, e
	}
	if sei.hProcess == 0 {
		return 0, nil // 理论上不会发生（带了 NOCLOSEPROCESS），保守当成功
	}
	defer windows.CloseHandle(sei.hProcess)
	windows.WaitForSingleObject(sei.hProcess, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(sei.hProcess, &code); err != nil {
		return 0, nil
	}
	return int(code), nil
}
