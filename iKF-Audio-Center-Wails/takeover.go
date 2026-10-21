//go:build windows

package main

// 入口接管 —— CLI 落点（P5-3 起逻辑在 internal/takeover 包）
// ====================================================================
//
// 官方安装器写了两个入口（Run\Cm108BSound 开机自启 + CLSID {125C480D}
// 控制面板项），接管 = 改指向本程序，撤销 = 改回旧面板。注册表读写、
// 状态体检全部在 internal/takeover，主程序（本文件的 CLI 分支、托盘
// 菜单「修复开机入口」、启动自愈）与 iKF-驱动管理.exe（勾选清单）共用。
//
// 权限：两项都在 HKLM/HKCR，非管理员写不动。走 setup.go 的 runElevated
// 惯例 —— 主程序恒定 asInvoker 不弹 UAC，只有 --takeover 明确触发时才
// ShellExecuteEx(runas) 重拉自己一个 --takeover-admin 子进程。
//
// 验收：跑完后核对注册表三项 + 开机启动行为（见内部取证报告）。

import (
	"fmt"

	"ikfaudiocenter/internal/takeover"
)

// runTakeover 是 `--takeover` 的落点（非管理员态）：先试直写，失败则
// 提权重跑自己。返回进程退出码。
func runTakeover() int {
	fmt.Println("入口接管：Run\\Cm108BSound + CLSID {125C480D} → 本程序")
	if _, err := takeover.Write(); err == nil {
		fmt.Println("接管完成（当前已是管理员）。")
		return 0
	}
	self, err := takeover.ExePath()
	if err != nil {
		fmt.Println("拿不到自身路径：", err)
		return 2
	}
	fmt.Println("需要管理员权限，弹出 UAC……（取消则不做任何改动）")
	code, err := runElevated(self, "--takeover-admin")
	if err != nil {
		// 用户点了 UAC「否」也走这里（拒绝不是错误，如实退出即可）
		fmt.Println("未提权，接管未执行：", err)
		return 1
	}
	if code != 0 {
		fmt.Printf("提权子进程以退出码 %d 结束，接管未完成。\n", code)
		return code
	}
	fmt.Println("接管完成。开机自启与控制面板项已指向本程序。")
	return 0
}

// runTakeoverAdmin 是 `--takeover-admin`（提权子进程）的落点：
// 只做注册表改写，不建窗口不碰驱动，退出码即结果。
func runTakeoverAdmin() int {
	if _, err := takeover.Write(); err != nil {
		return 3
	}
	return 0
}

// runTakeoverUndo 是 `--takeover-undo` 的落点：先试直写，失败提权重跑。
func runTakeoverUndo() int {
	fmt.Println("回滚入口：Run\\Cm108BSound + CLSID {125C480D} → 旧面板")
	if _, err := takeover.UndoWrite(); err == nil {
		fmt.Println("回滚完成（当前已是管理员）。")
		return 0
	}
	self, err := takeover.ExePath()
	if err != nil {
		fmt.Println("拿不到自身路径：", err)
		return 2
	}
	fmt.Println("需要管理员权限，弹出 UAC……（取消则不做任何改动）")
	code, err := runElevated(self, "--takeover-undo-admin")
	if err != nil {
		fmt.Println("未提权，回滚未执行：", err)
		return 1
	}
	if code != 0 {
		fmt.Printf("提权子进程以退出码 %d 结束，回滚未完成。\n", code)
		return code
	}
	fmt.Println("回滚完成。开机自启与控制面板项已指回旧面板。")
	return 0
}

// runTakeoverUndoAdmin 是 `--takeover-undo-admin`（提权子进程）的落点。
func runTakeoverUndoAdmin() int {
	if _, err := takeover.UndoWrite(); err != nil {
		return 3
	}
	return 0
}

// runEntryDelete 是 `--entry-delete` 的落点（2026-10-20：托盘「删除开机
// 入口」）：先试直删，失败提权重跑。删除 = Run 值与 CLSID 命令值一并删掉，
// 随时可用「修复开机入口」恢复。
func runEntryDelete() int {
	fmt.Println("删除开机入口：Run\\Cm108BSound + CLSID 命令值")
	if _, err := takeover.DeleteEntry(); err == nil {
		fmt.Println("删除完成（当前已是管理员）。")
		return 0
	}
	self, err := takeover.ExePath()
	if err != nil {
		fmt.Println("拿不到自身路径：", err)
		return 2
	}
	fmt.Println("需要管理员权限，弹出 UAC……（取消则不做任何改动）")
	code, err := runElevated(self, "--entry-delete-admin")
	if err != nil {
		fmt.Println("未提权，删除未执行：", err)
		return 1
	}
	if code != 0 {
		fmt.Printf("提权子进程以退出码 %d 结束，删除未完成。\n", code)
		return code
	}
	fmt.Println("删除完成。开机不再自启；「修复开机入口」可随时恢复。")
	return 0
}

// runEntryDeleteAdmin 是 `--entry-delete-admin`（提权子进程）的落点。
func runEntryDeleteAdmin() int {
	if _, err := takeover.DeleteEntry(); err != nil {
		return 3
	}
	return 0
}
