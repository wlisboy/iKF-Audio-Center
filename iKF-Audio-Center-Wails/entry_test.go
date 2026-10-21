//go:build windows

package main

// entry.go 的纯函数单测（不碰注册表、不碰设备，可随时跑）
//
//	go test -run TestExeFromCmd -v .
//
// 只覆盖「命令行 → exe 路径」这条最容易出错的解析，以及入口状态的三态判定。
// 真实注册表读写由 `iKF-Audio-Center.exe --entry-status` 在真机上验。

import "testing"

func TestExeFromCmd(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"C:\dir\a.exe" /h /d`, `C:\dir\a.exe`},
		{`"C:\dir with space\a.exe"`, `C:\dir with space\a.exe`},
		{`"C:\dir\a.exe"`, `C:\dir\a.exe`},
		{``, ``},
		{`   `, ``},
		// 无引号且带参数：截到第一个空格
		{`C:\dir\a.exe /h`, `C:\dir\a.exe`},
	}
	for _, c := range cases {
		if got := exeFromCmd(c.in); got != c.want {
			t.Errorf("exeFromCmd(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSameExe(t *testing.T) {
	if !sameExe(`C:\Dir\App.EXE`, `c:\dir\app.exe`) {
		t.Error("大小写不同应判为同一个文件")
	}
	if !sameExe(`C:\Dir\App.exe`, `C:\Dir\\App.exe`) {
		t.Error("多余分隔符应判为同一个文件")
	}
	if sameExe(`C:\Dir\a.exe`, `C:\Dir\b.exe`) {
		t.Error("不同文件不应相等")
	}
	if sameExe("", `C:\Dir\a.exe`) || sameExe(`C:\Dir\a.exe`, "") {
		t.Error("空路径不应相等")
	}
}

func TestEntryStatusShort(t *testing.T) {
	self := `C:\app\iKF.exe`

	ok := entryStatus{Self: self, RunExe: self, RunExists: true, RunOK: true,
		ClsExe: self, ClsExists: true, ClsOK: true}
	if !ok.OK() || ok.Stale() || ok.Short() != "已指向本程序" {
		t.Errorf("OK 态判定错误：OK=%v Stale=%v Short=%q", ok.OK(), ok.Stale(), ok.Short())
	}

	stale := entryStatus{Self: self, RunExe: `C:\old\iKF.exe`, RunExists: false}
	if stale.OK() || !stale.Stale() || stale.Short() != "入口已失效" {
		t.Errorf("失效态判定错误：OK=%v Stale=%v Short=%q", stale.OK(), stale.Stale(), stale.Short())
	}

	foreign := entryStatus{Self: self, RunExe: `C:\old\iKF.exe`, RunExists: true}
	if foreign.OK() || foreign.Stale() || foreign.Short() != "指向其他程序" {
		t.Errorf("外部态判定错误：OK=%v Stale=%v Short=%q", foreign.OK(), foreign.Stale(), foreign.Short())
	}

	empty := entryStatus{Self: self}
	if empty.OK() || empty.Stale() || empty.Short() != "未设置" {
		t.Errorf("未设置态判定错误：Short=%q", empty.Short())
	}
}
