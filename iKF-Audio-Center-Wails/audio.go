//go:build windows

package main

// audio.go —— ikf_audio.dll（audiocore/，音频热路径 Rust 实现）的 Go 绑定层。
//
// 只做「查找 → 加载 → ABI 版本协商 → 自检」。与 osConfLib.dll 同款
// LoadLibrary 模式（driver.go），同一个纪律：
// DLL 缺失/ABI 错配 = 功能明确降级，绝不崩主程序、绝不猜接口。
//
// 查找顺序（先开发态后部署态）：
//   1. <exeDir>/ikf_audio.dll            部署（便携包根 / 安装目录）
//   2. <exeDir>/../audiocore/target/release/ikf_audio.dll   源码态（wails dev / go test）
//
// 使用方式：热路径各模块（loopback/listen/recorder）在 Start 前
// 调 audioReady()；false = 音频热路不可用，调用方明确报错。

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
	// ★ 首探互斥。若「查 atomic 就做」，两个 goroutine
	// 并发首探会各自 LoadLibrary 一遍并竞写 audioDLL —— 后果良性（同一文件）
	// 但仍是数据竞争。双检锁收口。
	audioMu sync.Mutex
)

func audioDLLCandidates() []string {
	var out []string
	if d := appDir(); d != "" {
		out = append(out, filepath.Join(d, "ikf_audio.dll"))
		if wd, err := filepath.Abs(filepath.Join(d, "..", "audiocore", "target", "release")); err == nil {
			out = append(out, filepath.Join(wd, "ikf_audio.dll"))
		}
	}
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
	if st := audioState.Load(); st != 0 {
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
			continue
		}
		got, _, _ := abi.Call()
		if got != audioABI {
			continue
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
		s := string(buf[:n-1])
		audioDLL = dll
		audioVer.Store(&s)
		audioState.Store(1)
		return true
	}
	audioState.Store(-1)
	return false
}

func audioReady() bool { return audioProbe() }

func audioVersion() string {
	audioProbe()
	if v := audioVer.Load(); v != nil {
		return *v
	}
	return ""
}

func audioStatus() string {
	if audioProbe() {
		return fmt.Sprintf("rust: %s (abi %d)", audioVersion(), audioABI)
	}
	return "rust: 未启用（ikf_audio.dll 缺失或 ABI 错配）"
}
