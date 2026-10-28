//go:build windows

package main

// audio_lb.go —— Loopback 的 Rust 路径（ikf_audio.dll 会话 ABI）绑定。
//
// S2 切流策略：双路径共存，Rust 路径由环境变量 IKF_AUDIO_RUST=1 显式启用，
// 默认仍走 Go 路径。真机对比验收后再翻转默认（切流 = 删开关，删 Go 路径
// 在 S5 清退阶段做）。
//
// 会话句柄：open 返回 DLL 侧 Box<LoopbackSession> 的裸指针，存进
// Loopback.rustH；close 由 DLL 回收（含采集线程 join 与字段清零）。

import (
	"os"
	"unsafe"
)

// lbStatusC 与 Rust loopback::LbStatusOut 逐字段对齐（repr(C)）。
// 对齐推演：4×i32=16 + 2×u16=4 + u32=4 + i32=4(+4 pad) + u64 + f32 + i32
// + [16]+[256]+[256] —— Go 同序字段自然对齐结果一致，无需 cgo。
type lbStatusC struct {
	running    int32
	rate       int32
	ch         int32
	bits       int32
	rawTag     uint16
	subTag     uint16
	chanMask   uint32
	blockAlign int32
	frames     uint64
	level      float32
	errCode    int32
	kind       [16]byte
	dev        [256]byte
	err        [256]byte
}

// cstr 把定长 UTF-8 字段读到字符串（截到第一个 NUL）。
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// audioRustWanted P1 翻默认后：Rust 路径**默认启用**（S2/S3/S4 已真机逐项
// 验收），IKF_AUDIO_RUST=0 可显式关回 Go 路径（排障用）。DLL 缺失时
// audioReady() 为 false，自然落回 Go 路径。
func audioRustWanted() bool { return os.Getenv("IKF_AUDIO_RUST") != "0" }

// lbDevicesViaRust 端点枚举。行格式 "id\tname\tdef\n"（def ∈ {0,1}）。
func lbDevicesViaRust() (map[string]interface{}, bool) {
	if !audioProbe() {
		return nil, false
	}
	pr := audioDLL.NewProc("ikf_loopback_devices")
	// 两遍拉：先要长度
	n, _, _ := pr.Call(0, 0)
	if n == 0 {
		return map[string]interface{}{"ok": false, "error": "无法枚举播放设备"}, false
	}
	buf := make([]byte, n)
	r, _, _ := pr.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(n))
	if r == 0 {
		return map[string]interface{}{"ok": false, "error": "无法枚举播放设备"}, false
	}
	defID, list := "", make([]map[string]interface{}, 0, 4)
	for _, line := range splitLines(buf) {
		parts := splitTabs(line)
		if len(parts) < 3 {
			continue
		}
		if parts[2] == "1" {
			defID = parts[0]
		}
		list = append(list, map[string]interface{}{
			"id": parts[0], "name": parts[1], "def": parts[2] == "1",
		})
	}
	return map[string]interface{}{"ok": true, "default": defID, "list": list}, true
}

// lbSpectrumViaRust 拉最新 512 字节。
func lbSpectrumViaRust(h uintptr) ([]byte, bool) {
	if !audioProbe() || h == 0 {
		return nil, false
	}
	pr := audioDLL.NewProc("ikf_loopback_spectrum")
	out := make([]byte, lbBins)
	r, _, _ := pr.Call(h, uintptr(unsafe.Pointer(&out[0])), uintptr(len(out)))
	return out, r == 0
}

// lbStatusViaRust 拉状态快照。
func lbStatusViaRust(h uintptr) (lbStatusC, bool) {
	if !audioProbe() || h == 0 {
		return lbStatusC{}, false
	}
	var st lbStatusC
	pr := audioDLL.NewProc("ikf_loopback_status")
	r, _, _ := pr.Call(h, uintptr(unsafe.Pointer(&st)))
	return st, r == 0
}

// splitLines / splitTabs 定长辅助（避免 strings import 翻倍）。
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func splitTabs(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\t' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	out = append(out, string(b[start:]))
	return out
}

// lisStatusViaRust 拉监听状态快照。
func lisStatusViaRust(h uintptr) (lsStatusC, bool) {
	if !audioProbe() || h == 0 {
		return lsStatusC{}, false
	}
	var st lsStatusC
	pr := audioDLL.NewProc("ikf_listen_status")
	r, _, _ := pr.Call(h, uintptr(unsafe.Pointer(&st)))
	return st, r == 0
}

// lsStatusC 与 Rust listen::LsStatusOut 逐字段对齐（repr(C)）。
type lsStatusC struct {
	running     int32
	rate        int32
	ch          int32
	sent        uint64
	pkts        uint64
	silentPkts  uint64
	capPeak     float32
	dev         [256]byte
	out         [256]byte
	err         [256]byte
}

// recStatusC 与 Rust recorder::RecStatusOut 逐字段对齐（repr(C)）。
type recStatusC struct {
	running  int32
	rate     int32
	ch       int32
	frames   uint64
	size     int64
	errCode  int32
	kind     [16]byte
	dev      [256]byte
	file     [512]byte
	err      [256]byte
}

// recStatusViaRust 拉录音状态快照。
func recStatusViaRust(h uintptr) (recStatusC, bool) {
	if !audioProbe() || h == 0 {
		return recStatusC{}, false
	}
	var st recStatusC
	pr := audioDLL.NewProc("ikf_recorder_status")
	r, _, _ := pr.Call(h, uintptr(unsafe.Pointer(&st)))
	return st, r == 0
}
