//go:build windows

package main

// 入口状态与手动修复（P5-1'，2026-10-17；P5-3 起注册表逻辑在 internal/takeover）
// ====================================================================
//
// 前提：交付物是**便携包**（dist\iKF-Audio-Center-Portable 整目录），
// 用户会把它拷到任意位置 —— 桌面、D 盘、U 盘、公司共享目录。因此
// 「把安装目录固定到 Program Files」这条路线不成立，接管写死路径也不成立。
//
// 正确的模型是：**入口跟着便携包走**。注册表里记的是"上一次安装/修复时
// 那个 exe 的绝对路径"；目录拷到别处后，重新装一次驱动即改到新位置。
//
// 2026-10-23（用户定稿）：启动时**不再询问、不再弹 UAC** —— 原来窗口模式
// 启动会问「入口指向另一个程序，是否改到本程序？」并在确认后提权，体感就是
// 「装完驱动还弹一个替代官方面板的 UAC」。现在入口改写只剩两个入口：
//  1. 装驱动：安装器此刻已提权，--takeover 顺带写好（setup.go）
//  2. 托盘菜单「修复开机入口」/「删除开机入口」：用户主动点才提权（tray.go）
//
// 权限：读 Run/CLSID 普通用户即可（--entry-status / 状态体检）；
// 改写走 internal/takeover.Write / DeleteEntry → runElevated。

import (
	"fmt"
	"strings"

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
