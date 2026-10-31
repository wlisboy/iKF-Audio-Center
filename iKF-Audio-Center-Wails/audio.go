//go:build windows

package main

// audio.go —— ikf_audio.dll（audiocore/，P1 音频热路径 Rust 化）的 Go 绑定层。
//
// S1 骨架：只做「查找 → 加载 → ABI 版本协商 → 自检」，不接任何功能。
// 与 osConfLib.dll 同款 LoadLibrary 模式（driver.go），同一个纪律：
// DLL 缺失/ABI 错配 = 功能明确降级，绝不崩主程序、绝不猜接口。
//
// 查找顺序（先开发态后部署态）：
//   1. <exeDir>/ikf_audio.dll            部署（便携包根 / 安装目录）
//   2. <exeDir>/../audiocore/target/release/ikf_audio.dll   源码态（wails dev / go test）
//
// 使用方式：热路径各模块（S2 起的 loopback/listen/recorder）在 Start 前
// 调 audioReady()；false = 走既有 Go 路径（S1 期间唯一路径）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// abiVersion 必须与 audiocore/src/lib.rs 的 ABI_VERSION 逐字对应。
const audioABI = 1

var (
	audioDLL   *windows.LazyDLL
	audioState atomic.Int32 // 0=未探测 1=就绪 -1=不可用（缺失/ABI 错配/自检失败）
	audioVer   atomic.Pointer[string]
	// ★ 健壮性审计 #10：首探互斥。原来是「查 atomic 就做」，两个 goroutine
	// 并发首探会各自 LoadLibrary 一遍并竞写 audioDLL —— 后果良性（同一文件）
	// 但仍是数据竞争。双检锁收口。
	audioMu sync.Mutex
)

// audioDLLCandidates 按优先级返回候选路径。
func audioDLLCandidates() []string {
	var out []string
	if d := appDir(); d != "" {
		out = append(out, filepath.Join(d, "ikf_audio.dll"))
		if wd, err := filepath.Abs(filepath.Join(d, "..", "audiocore", "target", "release")); err == nil {
			out = append(out, filepath.Join(wd, "ikf_audio.dll"))
		}
	}
	// go test 的工作目录是包目录（Wails/），appDir 可能拿不到 exe —— 补一个相对路径
	if wd, err := filepath.Abs(filepath.Join("audiocore", "target", "release")); err == nil {
		out = append(out, filepath.Join(wd, "ikf_audio.dll"))
	}
	return out
}

// audioProbe 找到并加载 DLL、协商 ABI、跑自检。只成功一次，失败缓存。
func audioProbe() bool {
	if st := audioState.Load(); st != 0 {
		return st == 1
	}
	audioMu.Lock()
	defer audioMu.Unlock()
	if st := audioState.Load(); st != 0 { // 双检：并发首探只做一次
		return st == 1
	}
	for _, p := range audioDLLCandidates() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		dll := windows.NewLazySystemDLL(p)
		abi := dll.NewProc("ikf_abi_version")
		ver := dll.NewProc("ikf_version")
		st := dll.NewProc("ikf_selftest")
		if err := abi.Find(); err != nil || ver.Find() != nil || st.Find() != nil {
			continue // 导出不齐（半成品 DLL）= 视为不可用
		}
		got, _, _ := abi.Call()
		if got != audioABI {
			continue // 接口纪元错配 = 拒绝使用
		}
		sentinel, _, _ := st.Call()
		if sentinel != 0xF00D {
			continue
		}
		// 版本串（UTF-8 字节，拉两遍：先取长度）
		n, _, _ := ver.Call(0, 0)
		if n == 0 || n > 256 {
			continue
		}
		buf := make([]byte, n)
		ver.Call(uintptr(unsafe.Pointer(&buf[0])), n)
		s := string(buf[:n-1]) // 去 '\0'
		audioDLL = dll
		audioVer.Store(&s)
		audioState.Store(1)
		return true
	}
	audioState.Store(-1)
	return false
}

// audioReady 热路径模块开工前的统一闸门。
func audioReady() bool { return audioProbe() }

// audioVersion 就绪时返回版本串（如 "ikf-audio 0.1.0"），否则空串。
func audioVersion() string {
	audioProbe()
	if v := audioVer.Load(); v != nil {
		return *v
	}
	return ""
}

// audioStatus 诊断用：一行说清状态（self-test / status 接口里展示）。
func audioStatus() string {
	if audioProbe() {
		return fmt.Sprintf("rust: %s (abi %d)", audioVersion(), audioABI)
	}
	return "rust: 未启用（ikf_audio.dll 缺失或 ABI 错配，走 Go 路径）"
}
