//go:build windows && chaos

// chaos_test.go —— 破坏性检测（不稳定用例，**不参与**常规闸）
//
// ★ 构建标签 chaos：默认 `go test ./...` 完全看不到这个文件，
//   必须显式 `go test -tags chaos` 才会编入。理由：
//   - 会真崩进程（野指针/双重释放用例）；
//   - 会占音频会话十几秒；
//   放进常规闸等于给每次回归埋雷。
//
// 用法：go test -tags chaos -c -o ../tools/diag/chaos.test.exe .
//      然后由 tools/diag/run_chaos_dll.py 逐个用例**独立进程**运行。
//
// 判定标准（Windows NTSTATUS）：
//   0xC0000409 STATUS_STACK_BUFFER_OVERRUN —— Rust panic=abort 的 __fastfail，
//               也是本机历史报错的那个码
//   0xC0000005 STATUS_ACCESS_VIOLATION   —— 释放后使用 / 野指针
//   0xC0000374 STATUS_HEAP_CORRUPTION    —— double free
//
// 与常规闸的区别：常规闸只验「正常路径行为对」，这里验「非常规路径不死」。
package main

import (
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func chaosNeed(t *testing.T) {
	t.Helper()
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
}

func chaosProc(name string) *windows.LazyProc {
	return audioDLL.NewProc(name)
}

func chaosMark(format string, a ...interface{}) {
	fmt.Printf("CHAOS> "+format+"\n", a...)
	os.Stdout.Sync()
}

// ---------------------------------------------------------------- 00 基线

func TestChaos00Baseline(t *testing.T) {
	chaosNeed(t)
	chaosMark("baseline: abi=%d ver=%q status=%s", audioABI, audioVersion(), audioStatus())
	if audioABI != 1 {
		t.Fatalf("ABI 非预期：%d", audioABI)
	}
}

// ---------------------------------------------------------------- 01 版本串缓冲

func TestChaos01VersionCaps(t *testing.T) {
	chaosNeed(t)
	pr := chaosProc("ikf_version")
	n, _, _ := pr.Call(0, 0)
	chaosMark("version: 需要长度 n=%d", n)
	if n == 0 || n > 256 {
		t.Fatalf("版本长度荒谬：%d", n)
	}
	// 满容量
	buf := make([]byte, n+64)
	r, _, _ := pr.Call(uintptr(unsafe.Pointer(&buf[0])), n)
	chaosMark("version: cap=n → r=%d str=%q", r, string(buf[:n-1]))
	if r != n {
		t.Fatalf("cap 充足时返回 %d，应为 %d", r, n)
	}
	// cap 差一个字节：必须不写（哨兵保持）
	for _, cap := range []uintptr{0, 1, 2, n - 1} {
		guard := make([]byte, n+64)
		for i := range guard {
			guard[i] = 0xCC
		}
		pr.Call(uintptr(unsafe.Pointer(&guard[0])), cap)
		if guard[0] != 0xCC {
			t.Fatalf("cap=%d 不足却写了缓冲（越界写风险）", cap)
		}
	}
	// 谎报超大 cap（实际缓冲只有 n+64）
	guard := make([]byte, n+64)
	pr.Call(uintptr(unsafe.Pointer(&guard[0])), 0xFFFFFFFF)
	chaosMark("version: cap 谎报 4G 未越界（实际只写 n 字节）")
}

// ---------------------------------------------------------------- 02 FFT 探针

func TestChaos02FftProbe(t *testing.T) {
	chaosNeed(t)
	pr := chaosProc("ikf_fft_probe")
	rates := []uint32{0, 1, 2, 7999, 8000, 44100, 47999, 48000, 48001, 96000, 192000, 0x7FFFFFFF, 0xFFFFFFFF}
	amps := map[string]float64{
		"0": 0, "1": 1, "-1": -1,
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
		"1e308": 1e308, "-1e308": -1e308,
		"denormal": math.SmallestNonzeroFloat64,
	}
	buf := make([]byte, 512)
	for _, rate := range rates {
		for name, amp := range amps {
			r, _, _ := pr.Call(uintptr(rate), uintptr(math.Float64bits(amp)),
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if r != 0 {
				t.Fatalf("rate=%d amp=%s 返回 %d", rate, name, r)
			}
		}
	}
	// cap 边界：不足必须返回 512 且不写
	for _, cap := range []uintptr{0, 1, 511} {
		g := make([]byte, 512)
		for i := range g {
			g[i] = 0xAA
		}
		r, _, _ := pr.Call(48000, uintptr(math.Float64bits(1.0)), uintptr(unsafe.Pointer(&g[0])), cap)
		if r != 512 || g[0] != 0xAA {
			t.Fatalf("cap=%d 时 r=%d（应为 512）且不应写入", cap, r)
		}
	}
	chaosMark("fft_probe: %d 组 rate×amp 全部返回 0，cap 边界未越界", len(rates)*len(amps))
}

// ---------------------------------------------------------------- 03 非法句柄

// 这三个用例预期会杀进程 —— 目的在于确认 DLL 侧到底有没有句柄校验。
// Go 产品代码不会传这种值，但只要没有校验，Go 侧的并发窗口就必然是崩溃。

func TestChaos03aStatusBadHandle(t *testing.T) {
	chaosNeed(t)
	chaosMark("status(0xDEADBEEF) —— 野指针解引用")
	var st lbStatusC
	r, _, _ := chaosProc("ikf_loopback_status").Call(0xDEADBEEF, uintptr(unsafe.Pointer(&st)))
	chaosMark("status(bad) 返回 %d（没崩 = DLL 有校验）", r)
}

func TestChaos03bSpectrumBadHandle(t *testing.T) {
	chaosNeed(t)
	chaosMark("spectrum(1) —— 野指针解引用")
	buf := make([]byte, 512)
	r, _, _ := chaosProc("ikf_loopback_spectrum").Call(1, uintptr(unsafe.Pointer(&buf[0])), 512)
	chaosMark("spectrum(bad) 返回 %d", r)
}

func TestChaos03cCloseBadHandle(t *testing.T) {
	chaosNeed(t)
	chaosMark("close(0xDEADBEEF) —— Box::from_raw 野指针")
	chaosProc("ikf_loopback_close").Call(0xDEADBEEF)
	chaosMark("close(bad) 返回（没崩）")
}

// ---------------------------------------------------------------- 04 释放后使用

func TestChaos04StatusAfterClose(t *testing.T) {
	chaosNeed(t)
	open := chaosProc("ikf_loopback_open")
	closeP := chaosProc("ikf_loopback_close")
	h, _, _ := open.Call(0, 0, 0)
	if h == 0 {
		t.Skip("回环打开失败（无设备？）")
	}
	chaosMark("open → h=%#x，随后 close 再 status（UAF）", h)
	closeP.Call(h)
	time.Sleep(50 * time.Millisecond)
	var st lbStatusC
	r, _, _ := chaosProc("ikf_loopback_status").Call(h, uintptr(unsafe.Pointer(&st)))
	chaosMark("close 后 status 返回 %d（没崩 = 有保护）", r)
}

func TestChaos05DoubleClose(t *testing.T) {
	chaosNeed(t)
	open := chaosProc("ikf_loopback_open")
	closeP := chaosProc("ikf_loopback_close")
	h, _, _ := open.Call(0, 0, 0)
	if h == 0 {
		t.Skip("回环打开失败")
	}
	chaosMark("open → h=%#x，close 两次（double free）", h)
	closeP.Call(h)
	closeP.Call(h)
	chaosMark("double close 返回（没崩）")
}

func TestChaos06ListenAfterClose(t *testing.T) {
	chaosNeed(t)
	h, _, _ := chaosProc("ikf_listen_open").Call(0, 0)
	if h == 0 {
		t.Skip("监听打开失败（无麦克风？）")
	}
	chaosMark("listen open → h=%#x，close 后 status（UAF）", h)
	chaosProc("ikf_listen_close").Call(h)
	time.Sleep(50 * time.Millisecond)
	var st lsStatusC
	r, _, _ := chaosProc("ikf_listen_status").Call(h, uintptr(unsafe.Pointer(&st)))
	chaosMark("close 后 listen_status 返回 %d", r)
}

// ---------------------------------------------------------------- 07 设备枚举边界

func TestChaos07DevicesCaps(t *testing.T) {
	chaosNeed(t)
	pr := chaosProc("ikf_loopback_devices")
	n, _, _ := pr.Call(0, 0)
	chaosMark("devices: 需要长度 n=%d", n)
	if n == 0 {
		t.Skip("枚举失败")
	}
	small := make([]byte, 8)
	for i := range small {
		small[i] = 0xCC
	}
	r, _, _ := pr.Call(uintptr(unsafe.Pointer(&small[0])), 1)
	if r != n || small[0] != 0xCC {
		t.Fatalf("cap=1 时 r=%d（应 %d）且不应写入", r, n)
	}
	buf := make([]byte, n)
	r, _, _ = pr.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(n))
	if r == 0 {
		t.Fatalf("cap=n 时枚举失败")
	}
	chaosMark("devices: cap 边界正常，%d 字节", r)
}

// ---------------------------------------------------------------- 08 并发竞态（产品路径）

// TestChaos08LoopbackRace 是最贴近真实的一枪：
// Go 侧 statusRust / spectrumRust 都是「锁内取 h → 解锁 → 调 DLL」，
// 而 stopRust 同样「锁内取 h → 置 0 → 解锁 → close(h)」。
// 取到 h 之后、DLL 调用之前被抢占，就会对着已释放的 Box 解引用。
func TestChaos08LoopbackRace(t *testing.T) {
	chaosNeed(t)
	chaosMark("loopback 并发：4×start/stop + 6×status/spectrum，20s")

	deadline := time.Now().Add(20 * time.Second)
	var stop int32
	go func() {
		time.Sleep(20 * time.Second)
		atomic.StoreInt32(&stop, 1)
	}()

	for i := 0; i < 4; i++ {
		go func() {
			for atomic.LoadInt32(&stop) == 0 {
				lb.Start("")
				lb.Stop()
			}
		}()
	}
	for i := 0; i < 6; i++ {
		go func() {
			for atomic.LoadInt32(&stop) == 0 {
				lb.Status()
				lb.Spectrum()
			}
		}()
	}
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	atomic.StoreInt32(&stop, 1)
	time.Sleep(1500 * time.Millisecond)
	lb.Stop()
	chaosMark("loopback 并发 20s 存活")
}

// TestChaos09ListenRace 与 08 同款，监听会话。
func TestChaos09ListenRace(t *testing.T) {
	chaosNeed(t)
	chaosMark("listen 并发：3×start/stop + 5×status/gain，20s")

	deadline := time.Now().Add(20 * time.Second)
	var stop int32
	go func() {
		time.Sleep(20 * time.Second)
		atomic.StoreInt32(&stop, 1)
	}()
	for i := 0; i < 3; i++ {
		go func() {
			for atomic.LoadInt32(&stop) == 0 {
				lis.Start()
				lis.Stop()
			}
		}()
	}
	for i := 0; i < 5; i++ {
		go func(n int) {
			for atomic.LoadInt32(&stop) == 0 {
				lis.Status()
				lis.SetGain(1 + float64(n))
				lis.SetGain(math.NaN())
				lis.SetGain(math.Inf(1))
			}
		}(i)
	}
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	atomic.StoreInt32(&stop, 1)
	time.Sleep(1500 * time.Millisecond)
	lis.Stop()
	chaosMark("listen 并发 20s 存活")
}

// TestChaos10PureFuncs 纯函数边界：不碰设备，跑完即知。
func TestChaos10PureFuncs(t *testing.T) {
	vals := []interface{}{
		nil, map[string]interface{}{"a": 1}, []interface{}{1, 2}, []byte{1},
		math.NaN(), math.Inf(1), math.Inf(-1), "NaN", "Inf", "", "  12  ",
		"1e999", "0x10", "-0", int64(1) << 62, struct{}{}, complex(1, 2),
	}
	for _, v := range vals {
		f, err := toFloat(v)
		k := inferKind(v)
		chaosMark("toFloat(%T)=%v,%v inferKind=%s", v, f, err, k)
	}
	// cstr 空缓冲 / 全非零
	if got := cstr(nil); got != "" {
		t.Fatalf("cstr(nil)=%q", got)
	}
	all := make([]byte, 8)
	for i := range all {
		all[i] = 'x'
	}
	if got := cstr(all); got != "xxxxxxxx" {
		t.Fatalf("cstr(无 NUL)=%q", got)
	}
	// WriteAuto 未知名 / hazard 名：必须在碰驱动指针前返回
	for _, n := range []string{"", "NotARealParam_ZZZ", "DefaultDeviceControl"} {
		r := drv.WriteAuto(EpRender, n, math.NaN(), "")
		chaosMark("WriteAuto(%q) → %v", n, r)
	}
}
