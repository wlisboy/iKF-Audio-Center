//go:build windows

package main

// 入口自检与自愈（P5-1'，2026-10-17；P5-3 起注册表逻辑在 internal/takeover）
// ====================================================================
//
// 前提：交付物是**便携包**（dist\iKF-Audio-Center-Portable 整目录），
// 用户会把它拷到任意位置 —— 桌面、D 盘、U 盘、公司共享目录。因此
// 「把安装目录固定到 Program Files」这条路线不成立，接管写死路径也不成立。
//
// 正确的模型是：**入口跟着便携包走**。
//
//	注册表里记的是"上一次运行的那个 exe 的绝对路径"。
//	用户把整个目录拷到别处、从新位置双击一次 → 本文件负责把两处入口改到新位置。
//
// 三种状态：
//
//	entryOK       两处都指向本程序，且本程序确实在这个路径上
//	entryForeign  指向另一个仍然存在的 exe（多半是旧面板，或另一份拷贝）
//	entryStale    指向一个**已经不存在的** exe（目录被移动/删除/重建）
//
// 处理策略：
//
//	entryOK      不动。
//	entryForeign 只提示，不自动改 —— 两份拷贝共存时擅自改写会让用户
//	             不知道哪份在开机启动。由用户在提示框或托盘菜单里决定。
//	entryStale   提示后一键修复（入口本来已经坏了，修只会让它重新工作）。
//
// 提示每「自身路径」只弹一次（HKCU\Software\iKF Audio Center\EntryPromptPath），
// 用户点了「否」也不会反复骚扰；换个位置再运行会重新问一次。
//
// 权限：读 Run/CLSID 普通用户即可；改写走 internal/takeover.Write →
// runElevated，只在用户确认后才弹 UAC。
// 托盘隐藏启动（/h /d）**不弹提示** —— 开机时弹 UAC 是不可接受的。

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"ikfaudiocenter/internal/takeover"
)

// entryStatus 是 takeover.Status 的别名：tray.go / entry_test.go 沿用旧名。
type entryStatus = takeover.Status

// exeFromCmd / sameExe 是包内同名函数的本地别名（单测与老代码沿用旧名）。
func exeFromCmd(s string) string { return takeover.ExeFromCmd(s) }
func sameExe(a, b string) bool   { return takeover.SameExe(a, b) }

// readEntryStatus 读两处入口并逐项判定。读操作不需要管理员。
func readEntryStatus() entryStatus { return takeover.Read() }

// entryFix 把两处入口改到本程序。非管理员时自动提权（弹一次 UAC）。
// 返回 (改写项数, 错误)。与 --takeover 完全同一条路径，避免两套逻辑。
func entryFix() (int, error) {
	if n, err := takeover.Write(); err == nil {
		return n, nil
	}
	self, err := takeover.ExePath()
	if err != nil {
		return 0, err
	}
	code, err := runElevated(self, "--takeover-admin")
	if err != nil {
		return 0, err // 用户点了 UAC「否」也走这里
	}
	if code != 0 {
		return 0, fmt.Errorf("提权子进程退出码 %d", code)
	}
	return 2, nil
}

// entryDelete 删除两处入口（2026-10-20 托盘「删除开机入口」）。
// 与 entryFix 对称：非管理员时自动提权。删除可逆 —— 「修复开机入口」恢复。
func entryDelete() (int, error) {
	if n, err := takeover.DeleteEntry(); err == nil {
		return n, nil
	}
	self, err := takeover.ExePath()
	if err != nil {
		return 0, err
	}
	code, err := runElevated(self, "--entry-delete-admin")
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("提权子进程退出码 %d", code)
	}
	return 2, nil
}

// entryAskedBefore / markEntryAsked 记录「这个自身路径已经问过了」。
// 用 HKCU，无需提权；换位置运行（路径变了）会重新询问。
func entryAskedBefore(self string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, entryPromptKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(entryPromptName)
	return err == nil && takeover.SameExe(v, self)
}

func markEntryAsked(self string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, entryPromptKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue(entryPromptName, self)
}

// ---- 提示框 ----

const (
	entryPromptKey  = `Software\iKF Audio Center`
	entryPromptName = "EntryPromptPath"

	mbYesNo         = 0x00000004
	mbIconQuestion  = 0x00000020
	mbIconWarning   = 0x00000030
	mbSetForeground = 0x00010000
	idYes           = 6
)

// messageBox 返回用户点了哪个按钮（IDYES / IDNO / 0=失败）。
func messageBox(text, caption string, flags uintptr) int {
	p := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(caption)
	r, _, _ := p.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
	return int(r)
}

// maybePromptEntryFix 在窗口模式启动时调一次（托盘隐藏启动不调）。
// 设计成「最多问一次、问完记住」，避免每次开界面都打扰。
func maybePromptEntryFix() {
	st := readEntryStatus()
	if st.OK() || st.Self == "" {
		return
	}
	if entryAskedBefore(st.Self) {
		return
	}
	markEntryAsked(st.Self)

	var text string
	if st.Stale() {
		text = "开机自启与控制面板项的入口已经失效。\n\n" +
			"当前记录：\n" + firstNonEmpty(st.RunExe, st.ClsExe) + "\n\n" +
			"（该文件不存在，通常是把程序目录移动或删除过）\n\n" +
			"是否改指向当前位置？\n" + st.Self
	} else {
		text = "开机自启与控制面板项当前指向另一个程序：\n\n" +
			firstNonEmpty(st.RunExe, st.ClsExe) + "\n\n" +
			"本程序位置：\n" + st.Self + "\n\n" +
			"是否把入口改到本程序？"
	}

	flags := uintptr(mbYesNo | mbIconQuestion)
	if st.Stale() {
		flags = uintptr(mbYesNo | mbIconWarning)
	}
	if messageBox(text, "iKF Audio Center · 开机入口", flags) != idYes {
		return
	}
	if _, err := entryFix(); err != nil {
		messageBox("修复未完成：\n\n"+err.Error()+"\n\n可以稍后在托盘右键菜单里重试。",
			"iKF Audio Center · 开机入口", uintptr(mbIconWarning))
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// entryStatusLine 给 --entry-status 用的多行文本（回归/排障用）。
func entryStatusLine() string {
	st := readEntryStatus()
	var b strings.Builder
	fmt.Fprintf(&b, "自身路径 : %s\n", st.Self)
	fmt.Fprintf(&b, "Run      : ok=%v exists=%v\n           %s\n", st.RunOK, st.RunExists, st.RunValue)
	fmt.Fprintf(&b, "CLSID    : ok=%v exists=%v\n           %s\n", st.ClsOK, st.ClsExists, st.ClsValue)
	fmt.Fprintf(&b, "结论     : %s\n", st.Describe())
	return b.String()
}
