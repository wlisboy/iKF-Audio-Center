//go:build windows

// iKF 驱动管理 —— 只负责驱动的装与卸
// ==================================
//
// 与主程序（iKF-Audio-Center.exe）的分工：
//
//	主程序 iKF-Audio-Center.exe   只管运行：读同目录 CPL\ 里的 osConfLib.dll，
//	                              驱动耳机。asInvoker，永不提权
//	本程序 iKF-驱动管理.exe        只管驱动：把 driver\ 装进系统 / 从系统卸掉。
//	                              需要管理员，只在被拉起或双击时申请
//
// 为什么拆成两个 exe：装驱动是写系统（DriverStore + 内核服务 + APO 注册），
// 必须提权；而主程序若带 requireAdministrator，一个绿色小工具每次启动都弹
// UAC。拆开之后权限只在本程序存活的那几秒里存在，主程序始终干净。
//
// 用法
// ----
//
//	iKF-驱动管理.exe              界面模式：320x252 窗口，4 行清单（精简版）
//	iKF-驱动管理.exe --install    静默安装（含重启设备让 APO 生效）
//	  --takeover                  装完顺带设置开机自启（接管官方入口）
//	  --takeover-undo             装完顺带还原官方入口（GUI 已去掉此项）
//	iKF-驱动管理.exe --uninstall  静默卸载
//	iKF-驱动管理.exe --status     打印现状（只读），退出码 0=已装 / 3=未装
//	iKF-驱动管理.exe --dry-run    预演：只打印将要执行的命令，不碰系统
//	  --no-restart                装完不重启设备
//	  --demo[=秒]                 界面自检：放选择态，默认 3 秒后自动确认进进度态
//
// 退出码
// ------
//
//	0 成功 · 1 包不完整/环境错误 · 3 未安装（仅 --status）· 4 pnputil 失败
//	1223 用户在 UAC 上点了「否」
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"ikfaudiocenter/internal/takeover"
)

// ---- 单实例互斥（稳定性修复 ----
//
// 装驱动是写 DriverStore 的全局操作，两个实例并发跑 pnputil 会互相踩
// （装到一半另一个来删），这是「时灵时不灵」的真源之一。用命名互斥体
// 挡住：--status / --dry-run / --demo 只读或演示，不占互斥。
//
// 互斥体在**提权之后**才拿：双击 → UAC → 提权子进程拿互斥。若在提权前
// 拿，等 UAC 的父进程会一直占着，提权子进程反而进不来。
const mutexName = `Global\iKF-DriverSetup-Mutex`

func acquireSingleInstance() bool {
	name, _ := syscall.UTF16FromString(mutexName)
	h, _, err := kernel32.NewProc("CreateMutexW").Call(
		0, 0, uintptr(unsafe.Pointer(&name[0])))
	if h == 0 {
		return true
	}
	return err != syscall.ERROR_ALREADY_EXISTS
}

func main() {
	if len(os.Args) > 1 {
		attachConsole()
	}

	if hasOpt("--demo") {
		initDPI()
		demoRun()
		releaseBrushes()
		return
	}

	if has("--status") {
		s := probe()
		printStatus(s)
		if len(s.published) > 0 {
			os.Exit(0)
		}
		os.Exit(3)
	}

	dry := has("--dry-run")

	if !dry && !windows.GetCurrentProcessToken().IsElevated() {
		code := elevate()
		if code == 1223 {
			msgBox("操作已取消，未做任何改动\n\n"+
				"如需继续，请重新运行本程序", "iKF-驱动管理", mbOK|mbIconInfo)
		}
		os.Exit(code)
	}

	// 单实例：提权完成后才检查（原因见 acquireSingleInstance 注释）。
	if !dry {
		if !acquireSingleInstance() {
			msgBox("iKF-驱动管理已经在运行。\n\n请先在已打开的窗口里完成操作，"+
				"不要重复启动 —— 两个实例同时装卸驱动会互相冲突。",
				"iKF-驱动管理", mbOK|mbIconWarning)
			return
		}
	}

	// 稳定性修复 ：界面模式**不再先探测再开窗** —— 开窗前那
	// 两三轮 pnputil 枚举加上杀软对未签名 exe 的首次扫描，UAC 确认后要
	// 干等好几秒，体感就是「双击没反应」。现在窗口立刻出，探测转后台，
	// 结果落在窗口底部状态行（探测期间显示「正在检测驱动状态…」）。
	// 静默模式没有界面可看，保持同步探测（要靠退出码说话）。
	action := ""
	switch {
	case has("--install"):
		action = "install"
	case has("--uninstall"):
		action = "uninstall"
	}

	var s *drvState
	if action != "" {
		s = probe()
		note("启动探测完成: inf=%s hwid=%s published=%v device=%q",
			s.infPath, s.hwid, s.published, s.deviceID)
	}

	if action != "" {
		sel := optSel{install: true, restart: true}
		if action == "uninstall" {
			sel = optSel{uninstall: true}
		}
		// 静默模式默认不碰入口 —— 主程序 /api/driver/setup 拉起的
		// --install 不带这些参数，行为不变；要接管需显式给开关。
		if has("--takeover") {
			sel.takeover = true
		}
		if has("--takeover-undo") {
			sel.takeover, sel.untakeover = false, true
		}
		detail, ok := runAction(s, dry, sel)
		if _, err := fmt.Println(detail); err != nil {
			fl := uint32(mbOK | mbIconInfo)
			if !ok {
				fl = mbOK | mbIconError
			}
			msgBox(detail, "iKF 驱动管理", fl)
		}
		if ok {
			return
		}
		os.Exit(4)
	}

	out := uiRun(uiRunCfg{
		// state 传 nil：状态由 probe 在开窗后异步填充（见 uiRun 注释）。
		// work 里重新 probe —— 拿点击那一刻的新鲜状态，也顺带把「勾选到
		// 执行之间驱动被装/卸过」的窗口期堵上。
		state:      nil,
		needChoice: true,
		presel:     optSel{install: true, restart: true, takeover: true},
		labels:     progressLabels,
		probe:      probe,
		work:       func(sel optSel) (string, bool) { return runAction(probe(), dry, sel) },
	})
	releaseBrushes()
	if out.cancelled {
		return
	}

	done := ""
	if !out.sel.uninstall {
		done = "\n\n现在可以打开 iKF Audio Center"
	}
	if !out.ok {
		if p := writeLog(); p != "" {
			out.detail += "\n\n原始输出已写入：\n" + p
		}
		msgBox("操作未成功：\n\n"+out.detail, "iKF 驱动管理", mbOK|mbIconError)
		os.Exit(4)
	}
	msgBox(out.detail+done, "iKF 驱动管理", mbOK|mbIconInfo)
}

func progressLabels(sel optSel) (string, string, string) {
	if sel.uninstall {
		return "正在卸载驱动组件", "正在移除中…",
			"此过程通常需要几秒，请勿关闭窗口。"
	}
	sub := "正在写入中…"
	return "正在安装驱动组件", sub,
		"此过程约需 10–30 秒，请勿关闭窗口。"
}

// runAction 把一次选择落成系统动作。装完按需重启设备，让 APO 立刻挂上端点。
func runAction(s *drvState, dry bool, sel optSel) (string, bool) {
	if dry {
		a := "install"
		if sel.uninstall {
			a = "uninstall"
		}
		return dryRunText(s, a), true
	}
	var detail string
	var ok bool
	if sel.uninstall {
		setStatus("正在移除中…")
		detail, ok = s.uninstall()
	} else {
		setStatus("正在写入中…")
		detail, ok = s.install()
	}
	if ok && !sel.uninstall && sel.restart && !has("--no-restart") {
		setStatus("正在重启音频设备…")
		if id := probe().restartDevice(); id != "" {
			detail += "\n已重启音频设备"
		} else {
			detail += "\n未重启音频设备"
		}
	}
	// 接管（仅安装模式）。本程序启动即提权，注册表直写即可。
	// 完成提示不再提「开机自启」，成败只落日志。
	if ok && !sel.uninstall && sel.takeover {
		setStatus("正在设置开机自启…")
		if _, err := takeover.Write(); err != nil {
			note("takeover.Write 失败: %v", err)
		}
	}
	if ok && !sel.uninstall && sel.untakeover {
		setStatus("正在撤销接管…")
		if _, err := takeover.UndoWrite(); err == nil {
			detail += "\n已撤销接管：开机自启与控制面板项还原旧面板。"
		} else {
			detail += "\n撤销接管失败：" + err.Error()
		}
	}
	// 顺序保护：装好后若检测到旧版官方面板，把安全顺序讲清楚 ——
	// 旧卸载器按文件名删驱动文件，顺序错了会耳机静音（见 oldPanelNote）。
	if ok && !sel.uninstall {
		detail += s.oldPanelNote()
	}
	return detail, ok
}

func demoRun() {
	hold := 3 * time.Second
	for _, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "--demo="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				hold = time.Duration(n) * time.Second
			}
		}
	}
	uiRun(uiRunCfg{
		needChoice:  true,
		presel:      optSel{install: true, restart: true, takeover: true},
		labels:      progressLabels,
		probe:       probe,
		autoConfirm: hold,
		work: func(optSel) (string, bool) {
			time.Sleep(2500 * time.Millisecond)
			return "（界面自检，未做任何改动）", true
		},
	})
}

// fallbackChoice 是建不出窗口时的兜底：退回 Yes/No 消息框，按现状问装还是卸。
// 返回 "" 表示用户放弃。
func fallbackChoice() string {
	s := probe()
	if e := s.packErr(); e != "" {
		msgBox(e, "iKF 驱动管理", mbOK|mbIconError)
		return ""
	}
	dev := "未检测到耳机"
	if s.deviceID != "" {
		dev = "已连接耳机"
	}
	if len(s.published) == 0 {
		r := msgBox("本机尚未安装耳机驱动组件。\n\n"+
			"当前状态："+dev+"\n"+
			"硬件 ID："+orDash(s.hwid)+"\n\n"+
			"是否现在安装？（需要管理员权限，约 10–30 秒）",
			"iKF 驱动管理", mbYesNo|mbIconQuestion)
		if r == idYes {
			return "install"
		}
		return ""
	}
	r := msgBox("耳机驱动组件已安装。\n\n"+
		"当前状态："+dev+"\n"+
		"硬件 ID："+orDash(s.hwid)+"\n"+
		"驱动库发布名："+strings.Join(s.published, "、")+"\n\n"+
		"是否卸载？卸载后耳机在本控制面板里将暂时不可用，直到重新安装。\n"+
		"（想重装请改用 --install 参数运行）",
		"iKF 驱动管理", mbYesNo|mbIconWarning|mbDefButton2)
	if r == idYes {
		return "uninstall"
	}
	return ""
}

func printStatus(s *drvState) {
	fmt.Println("程序目录 : " + exeDir())
	fmt.Println("驱动描述 : " + s.infPath + exist(s.infPath))
	fmt.Println("驱动签名 : " + s.catPath + exist(s.catPath))
	fmt.Println("硬件 ID  : " + orDash(s.hwid))
	fmt.Println("设备实例 : " + orDash(s.deviceID))
	if len(s.published) == 0 {
		fmt.Println("驱动库   : 未安装")
	} else {
		fmt.Println("驱动库   : 已安装 → " + strings.Join(s.published, "、"))
	}
	if s.oldPanel != "" {
		fmt.Println("旧版面板 : " + s.oldPanel + "（卸旧版前请先装好新驱动，卸完重跑一次 --install）")
	}
}

func exist(p string) string {
	if fileExists(p) {
		return "（存在）"
	}
	return "（缺失）"
}

// elevate 以管理员身份重启自己，参数原样带上，并**等它结束再退出**。
//
// 等是为了让退出码有意义：脚本与主程序都靠退出码判断到底装成没装成。
// 用 ShellExecuteEx 而不是 ShellExecute —— 只有前者给子进程句柄，能 Wait，
// 也才能把「用户点了 UAC 的否」(1223) 和「安装器自己失败」分开。
//
// 不用 requireAdministrator 清单而是运行时提权：go build 出来的 exe 没有内嵌
// 清单资源，加清单要额外的 .syso 步骤；运行时提权则让双击、命令行、
// 被主程序 ShellExecute 三种入口的行为完全一致。
func elevate() int {
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	params := ""
	if len(os.Args) > 1 {
		params = windows.ComposeCommandLine(os.Args[1:])
	}

	var sei shellExecInfoW
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	sei.fMask = 0x00000040
	sei.lpVerb = windows.StringToUTF16Ptr("runas")
	sei.lpFile = windows.StringToUTF16Ptr(exe)
	sei.lpDirectory = windows.StringToUTF16Ptr(filepath.Dir(exe))
	sei.nShow = windows.SW_SHOWNORMAL
	if params != "" {
		sei.lpParameters = windows.StringToUTF16Ptr(params)
	}

	if r, _, _ := pShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		return 1223
	}
	if sei.hProcess == 0 {
		return 0
	}
	defer windows.CloseHandle(sei.hProcess)
	windows.WaitForSingleObject(sei.hProcess, windows.INFINITE)
	var code uint32
	if windows.GetExitCodeProcess(sei.hProcess, &code) != nil {
		return 0
	}
	return int(code)
}

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

var pShellExecuteExW = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteExW")

func has(a string) bool {
	for _, v := range os.Args[1:] {
		if v == a {
			return true
		}
	}
	return false
}

func hasOpt(name string) bool {
	for _, v := range os.Args[1:] {
		if v == name || strings.HasPrefix(v, name+"=") {
			return true
		}
	}
	return false
}
