//go:build windows

// WASAPI 回环采集（系统输出 → 频谱）
// ====================================================================
//
// 目的：让「可视化」页不必再弹浏览器的屏幕共享选择框。
// 浏览器没有「按设备/系统输出」取音频的 Web API（getUserMedia 只能选麦克风，
// getDisplayMedia 只能选屏幕/窗口/标签页），所以这一层必须落到原生。
//
// 链路
// ----
//
//	IMMDeviceEnumerator.GetDefaultAudioEndpoint(eRender, eConsole)
//	  或 GetDevice(id) —— 按端点 ID 锁定某个输出设备（仅耳机模式）
//	  → IMMDevice.Activate(IID_IAudioClient)
//	  → IAudioClient.GetMixFormat()            ← 共享模式必须用混音格式
//	  → IAudioClient.Initialize(SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK, 200ms, 0, mix)
//	  → IAudioClient.GetService(IID_IAudioCaptureClient)
//	  → IAudioClient.Start()
//	  → 轮询 IAudioCaptureClient.GetNextPacketSize / GetBuffer / ReleaseBuffer
//	  → 混成单声道 float32 → 环形缓冲 → 每 20ms 一次 1024 点 FFT → 512 个字节
//
// 为什么手写 COM 而不是引库
// ------------------------
// 构建是离线的（GOPROXY=off），go.mod 里只有 golang.org/x/sys。
// 加 github.com/gen2brain/malgo 之类的绑定拿不到包，所以直接用 syscall 走
// vtable。COM 的虚表布局是稳定 ABI，照着头文件抄下标即可。
//
// 与 Web Audio 的语义对齐
// ----------------------
// 前端原来读的是 AnalyserNode.getByteFrequencyData。为了让两套音源
// （后端回环 / 屏幕捕获）的画面一致，这里的 FFT 逐条按 Chromium 的**实测行为**来：
//
//	· Blackman 窗（w[n] = 0.42 − 0.5cos + 0.08cos(2·)，相干增益 0.42）
//	· 幅度归一化 mag = |X[k]| / N —— 满幅正弦读 −13.56 dB，
//	  不是规范正文那句「0 dB」。常数是实测反解出来的，见 waMagScale
//	· 平滑 X̂[k] = τ·X̂[k−1] + (1−τ)·|X[k]|，τ = smoothingTimeConstant = 0.8。
//	  实测：**每调用一次施加一次**，与墙钟无关（同一渲染块内连调则完全不推进，
//	  因为输入缓冲没变、FFT 结果被复用）。所以按固定 20ms 节拍算即可
//	· 字节映射 b = 255·(dB − minDecibels)/(maxDecibels − minDecibels)，截断取整
//	  默认 minDecibels = −100、maxDecibels = −30
//
// 验收方式：
//
//	--loopback-fft        设备无关的解析解自检（CI 可跑）
//	--loopback-fft dump   导出 512 字节，与 Chromium 侧对拍
//	                       （.workbuddy-ai/tmp/probe_wa_spectrum.js 生成基准）
//	--loopback-test       真实 WASAPI 端到端（可配 --wav 当信号源）
//	--loopback-devices    列出可锁定的播放端点（与 Python 线逐行一致）
package main

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ---------------------------------------------------------------- COM 基础

type guidT struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidMMDeviceEnumerator = guidT{0xBCDE0395, 0xE52F, 0x467C,
		[8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator = guidT{0xA95664D2, 0x9614, 0x4F35,
		[8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient = guidT{0x1CB9AD4C, 0xDBFA, 0x4C32,
		[8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient = guidT{0xC8ADBD64, 0xE71E, 0x48A0,
		[8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
)

// 虚表下标（0/1/2 是 IUnknown 的 QueryInterface/AddRef/Release）
const (
	vtblIMMDeviceGetDefaultEndpoint = 4 // IMMDeviceEnumerator
	vtblIMMDeviceEnumEndpoints      = 3 // IMMDeviceEnumerator：列出全部活动端点
	vtblIMMDeviceGetDevice          = 5 // IMMDeviceEnumerator：按端点 ID 取设备
	vtblIMMDeviceActivate           = 3 // IMMDevice
	vtblIMMDeviceOpenPropStore      = 4 // IMMDevice：开属性仓库（拿友好名）
	vtblIMMDeviceGetId              = 5 // IMMDevice：拿端点 ID
	vtblCollGetCount                = 3 // IMMDeviceCollection
	vtblCollItem                    = 4 // IMMDeviceCollection
	vtblPropStoreGetValue           = 5 // IPropertyStore
	vtblAudioClientInitialize       = 3
	vtblAudioClientGetMixFormat     = 8
	vtblAudioClientStart            = 10
	vtblAudioClientStop             = 11
	vtblAudioClientGetService       = 14
	vtblCaptureGetBuffer            = 3
	vtblCaptureReleaseBuffer        = 4
	vtblCaptureGetNextPacketSize    = 5
)

const (
	coinitMultithreaded = 0x0
	clsctxAll           = 0x17
	audclntShareShared  = 0
	audclntLoopback     = 0x00020000
	audclntSilent       = 0x00000002
	eRender             = 0
	eConsole            = 0
	devStateActive      = 1  // DEVICE_STATE_ACTIVE：只列「在」的端点
	stgmRead            = 0  // STGM_READ：属性仓库只读
	vtLPWSTR            = 31 // PROPVARIANT.vt == VT_LPWSTR
)

var (
	ole32        = syscall.NewLazyDLL("ole32.dll")
	procCoInit   = ole32.NewProc("CoInitializeEx")
	procCoUnin   = ole32.NewProc("CoUninitialize")
	procCoCreate = ole32.NewProc("CoCreateInstance")
	procCoAlloc  = ole32.NewProc("CoTaskMemAlloc")
	procCoFree   = ole32.NewProc("CoTaskMemFree")
	procCoVarClr = ole32.NewProc("PropVariantClear")
)

// comCall 按虚表下标调一次方法。
//
// p 用 unsafe.Pointer 而不是 uintptr 保存：一是 uintptr 不受 GC 追踪，
// 二是 `unsafe.Pointer(uintptr)` 这种反向转换会被 go vet 的 unsafeptr
// 判为可疑（driver.go 之所以干净，正是因为它只做正向的
// `uintptr(unsafe.Pointer(x))`）。全程用 unsafe.Pointer 两端都干净。
//
// ★ COM 接口指针 p 指向的是「虚表指针」，不是虚表本身：
//
//	  fn = (*(void***)p)[idx]
//	少解一层引用就会跳到 p + idx*8 那片内存里的垃圾值上，
//	表现为 `fatal error: unexpected signal` + addr=0xffffffffffffffff，
//	而且 Go 的 recover 拦不住（是 SEH 级别的访问违例）。
//
// 不做 AddRef/Release 记账：每次会话按「枚举器 → 设备 → AudioClient →
// CaptureClient」的顺序各拿一次，结束时逆序 Release，引用计数是平的。
func comCall(p unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(p)
	fn := *(*uintptr)(unsafe.Add(vtbl, idx*int(unsafe.Sizeof(uintptr(0)))))
	a := make([]uintptr, 0, len(args)+1)
	a = append(a, uintptr(p))
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(fn, a...)
	return r
}

func comRelease(p unsafe.Pointer) {
	if p != nil {
		comCall(p, 2) // IUnknown::Release
	}
}

func hresBad(hr uintptr) bool { return int32(hr) < 0 }

// -------------------------------------------------------- 端点枚举辅助

// PROPERTYKEY / PROPVARIANT（只抄用到的字段）。x64 上 PROPVARIANT 的值
// 联合体固定落在偏移 8（vt 2 字节 + 保留 6 字节），拿 pwszVal 就够。
type propKey struct {
	Fmt guidT
	Pid uint32
}

type propVariant struct {
	Vt  uint16
	_   [6]byte
	Val unsafe.Pointer
}

// PKEY_Device_FriendlyName —— 端点友好名（"Speakers (C-Media USB Audio…)"）
var pkeyFriendlyName = propKey{
	Fmt: guidT{0xA45C254E, 0xDF1C, 0x4EFD,
		[8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
	Pid: 14,
}

// utf16Heap 把字符串拷到 CoTaskMem 的堆上（UTF-16 + NUL），传给
// GetDevice 这类收 LPCWSTR 的方法。用 CoTaskMemAlloc 而不是 GlobalAlloc：
// 反正本文件里释放都用 CoTaskMemFree，一对儿。
func utf16Heap(s string) unsafe.Pointer {
	u, err := syscall.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		return nil
	}
	p, _, _ := procCoAlloc.Call(uintptr(len(u)) * 2)
	if p == 0 {
		return nil
	}
	for i, w := range u {
		*(*uint16)(unsafe.Add(unsafe.Pointer(p), i*2)) = w
	}
	return unsafe.Pointer(p)
}

// utf16ToStr 读一个 LPWSTR。指针全程是 unsafe.Pointer，用 unsafe.Add
// 走偏移 —— 反向的 unsafe.Pointer(uintptr) 会吃 go vet 警告。
func utf16ToStr(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*uint16)(unsafe.Add(p, n*2)) != 0 {
		n++
	}
	if n == 0 {
		return ""
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(p), n)))
}

// immDeviceID 拿端点 ID（形如 {0.0.1.00000000}.{guid}），调用方负责 Release
// 传进来的设备指针，但 ID 字符串本身的 CoTaskMemFree 在这里面做。
func immDeviceID(dev unsafe.Pointer) (string, bool) {
	var s unsafe.Pointer
	if hresBad(comCall(dev, vtblIMMDeviceGetId, uintptr(unsafe.Pointer(&s)))) || s == nil {
		return "", false
	}
	defer procCoFree.Call(uintptr(s))
	return utf16ToStr(s), true
}

// immDeviceName 拿端点友好名。失败返回空串 —— 名字只是展示，拿不到不该
// 拖垮采集（比如某些虚拟设备没有属性仓库）。
func immDeviceName(dev unsafe.Pointer) string {
	var store unsafe.Pointer
	if hresBad(comCall(dev, vtblIMMDeviceOpenPropStore, stgmRead,
		uintptr(unsafe.Pointer(&store)))) || store == nil {
		return ""
	}
	defer comRelease(store)
	var pv propVariant
	if hresBad(comCall(store, vtblPropStoreGetValue,
		uintptr(unsafe.Pointer(&pkeyFriendlyName)),
		uintptr(unsafe.Pointer(&pv)))) {
		return ""
	}
	defer procCoVarClr.Call(uintptr(unsafe.Pointer(&pv)))
	if pv.Vt != vtLPWSTR {
		return ""
	}
	return utf16ToStr(pv.Val)
}

// ---------------------------------------------------------------- 混音格式

// WAVEFORMATEX，18 字节。
//
// ★ 注意 Go 会把它按 4 字节对齐补到 20 字节 —— 所以下面
// waveFormatExtensible **不能**用嵌入的方式复用这个类型，
// 否则 Samples / dwChannelMask / SubFormat 会整体后移 2 字节，
// SubFormat.Data1 读出来是垃圾，格式判定落到 default 分支，
// 表现是「链路正常、帧数照涨，但频谱恒为 0」。
type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

// WAVEFORMATEXTENSIBLE：18 字节的 WAVEFORMATEX 后面紧跟
// wValidBitsPerSample(2) + dwChannelMask(4) + SubFormat(16) = 40 字节。
// 这里平铺写全，不嵌入 waveFormatEx（见上）。
type waveFormatExtensible struct {
	FormatTag      uint16 // 0
	Channels       uint16 // 2
	SamplesPerSec  uint32 // 4
	AvgBytesPerSec uint32 // 8
	BlockAlign     uint16 // 12
	BitsPerSample  uint16 // 14
	CbSize         uint16 // 16
	ValidBits      uint16 // 18
	ChannelMask    uint32 // 20
	SubFormat      guidT  // 24
}

// wfSubFormatOffset 是 SubFormat 在 WAVEFORMATEXTENSIBLE 里的固定偏移，
// 用于对拍结构体布局没算错（见 loopbackTest 的输出）。
const wfSubFormatOffset = 24

// ---------------------------------------------------------------- 环形缓冲

// 只保留最近的 8192 个单声道样本（48 kHz 下约 170ms）。
// 做 1024 点 FFT 只需要最后 1024 个，留余量是为了容忍轮询抖动。
const (
	lbRingLen = 8192
	lbFFTSize = 1024
	lbBins    = lbFFTSize / 2 // 512，与 Web Audio frequencyBinCount 一致
	lbPeriod  = 20 * time.Millisecond
)

// Loopback 是一路系统回环采集会话。
//
// 线程模型：采集跑在一个 runtime.LockOSThread() 钉住的 goroutine 上。
// COM 的单元（apartment）状态是线程局部的 —— CoInitializeEx 在哪个线程调的，
// 接口指针就必须在哪个线程用。这与 driver.go 里 osConfLib 必须钉线程是
// 同一类约束，但两者互不相干，各自钉各自的线程。
type Loopback struct {
	mu      sync.Mutex
	running bool
	errMsg  string
	rate    int
	ch      int
	srcTag  string

	// devSel 是用户指定的端点 ID（空 = 跟随默认播放设备）；
	// devName 是实际打开的端点友好名，给前端展示「正在采哪个设备」。
	devSel  string
	devName string

	// 混音格式的原始字段，供自检诊断「为什么落到 unsupported」。
	// 只读快照，不参与采集逻辑。
	bits       int
	rawTag     uint16 // WAVEFORMATEX.wFormatTag（0xFFFE 表示 EXTENSIBLE）
	subTag     uint16 // EXTENSIBLE 的 SubFormat.Data1（真正的编码）
	chanMask   uint32
	blockAlign int

	ring  []float32
	ringW int
	ringN int

	prev    []float32 // 平滑用的上一帧幅度
	bins    []byte    // 512 字节，/api/spectrum 直接返回
	level   float32   // 最近一帧峰值幅度（0..1），给面板电平条用
	frames  uint64    // 累计采到的帧数
	startAt time.Time

	stopCh chan struct{}
	doneCh chan struct{}
}

func NewLoopback() *Loopback {
	return &Loopback{
		ring: make([]float32, lbRingLen),
		prev: make([]float32, lbBins),
		bins: make([]byte, lbBins),
	}
}

// Start 打开一路回环。device 是端点 ID（Devices() 列出来的那种）：
//
//	""          → 跟随默认播放设备（旧行为，向后兼容）
//	非空        → 锁定该端点，别的设备上放什么都进不来
//
// 已经开着且端点没换就返回 ok=true（幂等）；开着但端点换了，先收旧会话
// 再按新端点重开 —— 这是「采集途中切换耳机」的路径。
func (l *Loopback) Start(device string) map[string]interface{} {
	l.mu.Lock()
	if l.running && l.devSel == device {
		rate, ch, tag := l.rate, l.ch, l.srcTag
		bits, rawTag, subTag := l.bits, l.rawTag, l.subTag
		dev := l.devName
		l.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"rate": rate, "ch": ch, "format": tag, "dev": dev,
			"bits": bits, "tag": rawTag, "sub": subTag}
	}
	if l.running {
		// 端点换了：停掉旧会话（worker 会退出），再当作全新启动走一遍
		stop, done := l.stopCh, l.doneCh
		l.mu.Unlock()
		select {
		case <-stop:
		default:
			close(stop)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		l.mu.Lock()
		l.running = false
	}
	l.devSel = device
	l.errMsg = ""
	// ★ 换端点重开（乃至出错后的重开）必须清掉上一场会话的可见字段：
	// 下面的等待循环以 rate>0 判「开好了」，不清的话会立刻读到旧会话的
	// rate 返回假 ok —— 症状是「start 一个不存在的端点也返回成功」。
	l.rate, l.ch, l.srcTag = 0, 0, ""
	l.devName = ""
	l.bits, l.rawTag, l.subTag, l.chanMask, l.blockAlign = 0, 0, 0, 0, 0
	l.stopCh = make(chan struct{})
	l.doneCh = make(chan struct{})
	stop, done := l.stopCh, l.doneCh
	l.running = true
	l.mu.Unlock()

	go l.worker(stop, done)

	// 等它把设备打开（或失败）。COM 打开通常几毫秒，给 3 秒余量。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		rate, ch, tag, errMsg := l.rate, l.ch, l.srcTag, l.errMsg
		bits, rawTag, subTag := l.bits, l.rawTag, l.subTag
		dev := l.devName
		l.mu.Unlock()
		if rate > 0 {
			return map[string]interface{}{"ok": true, "rate": rate, "ch": ch,
				"format": tag, "dev": dev,
				"bits": bits, "tag": rawTag, "sub": subTag}
		}
		if errMsg != "" {
			l.mu.Lock()
			l.running = false
			l.mu.Unlock()
			return map[string]interface{}{"ok": false, "error": errMsg}
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.mu.Lock()
	l.running = false
	l.mu.Unlock()
	close(stop)
	return map[string]interface{}{"ok": false, "error": "打开系统回环超时"}
}

// Stop 关掉会话并等采集线程退出。
func (l *Loopback) Stop() map[string]interface{} {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true}
	}
	stop, done := l.stopCh, l.doneCh
	l.mu.Unlock()

	select {
	case <-stop:
	default:
		close(stop)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

	l.mu.Lock()
	l.running = false
	l.rate, l.ch, l.srcTag = 0, 0, ""
	l.devName = ""
	l.bits, l.rawTag, l.subTag, l.chanMask, l.blockAlign = 0, 0, 0, 0, 0
	l.ringW, l.ringN, l.frames = 0, 0, 0
	for i := range l.ring {
		l.ring[i] = 0
	}
	for i := range l.prev {
		l.prev[i] = 0
	}
	for i := range l.bins {
		l.bins[i] = 0
	}
	l.level = 0
	l.mu.Unlock()
	return map[string]interface{}{"ok": true}
}

// Status 供 /api/loopback/status 与自检使用。
func (l *Loopback) Status() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]interface{}{
		"ok": true, "running": l.running, "rate": l.rate, "ch": l.ch,
		"format": l.srcTag, "bins": lbBins, "frames": l.frames,
		"level": math.Round(float64(l.level)*1000) / 1000,
		"dev":   l.devName,
		// 混音格式原始字段：自检用，前端不依赖
		"bits": l.bits, "tag": l.rawTag, "sub": l.subTag,
		"mask": l.chanMask, "align": l.blockAlign,
	}
	if l.errMsg != "" {
		out["error"] = l.errMsg
	}
	if l.running && !l.startAt.IsZero() {
		out["uptime"] = int(time.Since(l.startAt).Seconds())
	}
	return out
}

// Devices 枚举当前活动的播放端点 —— WASAPI loopback 可以锁定的全部目标。
//
// 这是「仅耳机可视化」的入口：每个输出端点（耳机 / 音箱 / 显示器）都有
// 自己独立的回环流，锁定耳机端点后，音箱上放什么都进不来，反之亦然。
// 跟随默认端点做不到这一点 —— 默认切到音箱，它就跟着抓音箱。
//
// 在调用线程（HTTP handler goroutine）上临时 CoInitializeEx：MTA 是按
// 线程计数的，进来一次配一次 CoUninitialize 就平了，不影响采集线程
// 自己的单元状态。蓝牙耳机这里会列出两个端点（Stereo/A2DP 与
// Hands-Free/HFP），选哪个由前端引导，后端不掺和。
func (l *Loopback) Devices() map[string]interface{} {
	hr, _, _ := procCoInit.Call(0, coinitMultithreaded)
	// S_OK(0) 才配 CoUninitialize；S_FALSE / RPC_E_CHANGED_MODE 不配。
	if int32(hr) == 0 {
		defer procCoUnin.Call()
	}

	var enumerator unsafe.Pointer
	hr, _, _ = procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumerator)))
	if hresBad(hr) || enumerator == nil {
		return map[string]interface{}{"ok": false, "error": "无法创建音频设备枚举器"}
	}
	defer comRelease(enumerator)

	var coll unsafe.Pointer
	hr = comCall(enumerator, vtblIMMDeviceEnumEndpoints,
		eRender, devStateActive, uintptr(unsafe.Pointer(&coll)))
	if hresBad(hr) || coll == nil {
		return map[string]interface{}{"ok": false, "error": "无法枚举播放设备"}
	}
	defer comRelease(coll)

	var count uint32
	comCall(coll, vtblCollGetCount, uintptr(unsafe.Pointer(&count)))

	// 先拿默认端点的 ID，给列表打 [默认] 标。拿不到也不致命，只是没标记。
	defID := ""
	var ddev unsafe.Pointer
	hr = comCall(enumerator, vtblIMMDeviceGetDefaultEndpoint,
		eRender, eConsole, uintptr(unsafe.Pointer(&ddev)))
	if !hresBad(hr) && ddev != nil {
		defID, _ = immDeviceID(ddev)
		comRelease(ddev)
	}

	list := make([]map[string]interface{}, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev unsafe.Pointer
		if hresBad(comCall(coll, vtblCollItem, uintptr(i),
			uintptr(unsafe.Pointer(&dev)))) || dev == nil {
			continue
		}
		id, ok := immDeviceID(dev)
		if ok {
			list = append(list, map[string]interface{}{
				"id": id, "name": immDeviceName(dev), "def": id == defID,
			})
		}
		comRelease(dev)
	}
	return map[string]interface{}{"ok": true, "default": defID, "list": list}
}

// Spectrum 返回当前 512 个频谱字节。
//
// 用 base64 而不是二进制直出：契约要和 Python 版逐字段一致，而 Python 侧
// 同一份数据走 json 也是 base64。512 字节 → 684 个 base64 字符，
// 30Hz 下约 20KB/s，本地环回完全够用。
func (l *Loopback) Spectrum() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return map[string]interface{}{"ok": false, "running": false, "error": "回环未启动"}
	}
	// 原始样本峰值（线性 float32）：bins/level 是 dB 量化+平滑后的可视化
	// 值，底噪级信号（~1e-4）会显示成 0——诊断「端点里到底有没有数据」
	// 必须看原始峰值（2026-10-20 监听排障教训；与 Python 线对齐）。
	peak := 0.0
	if l.ringN > 0 {
		cnt := l.ringN
		if cnt > lbRingLen {
			cnt = lbRingLen
		}
		start := (l.ringW - cnt + lbRingLen) % lbRingLen
		for i := 0; i < cnt; i++ {
			v := math.Abs(float64(l.ring[(start+i)%lbRingLen]))
			if v > peak {
				peak = v
			}
		}
	}
	cp := make([]byte, len(l.bins))
	copy(cp, l.bins)
	return map[string]interface{}{
		"ok": true, "running": true, "rate": l.rate, "ch": l.ch,
		"bins":   base64.StdEncoding.EncodeToString(cp),
		"level":  math.Round(float64(l.level)*1000) / 1000,
		"frames": l.frames,
		"peak":   math.Round(peak*1e7) / 1e7,
	}
}

// ---------------------------------------------------------------- 采集线程

// worker 是会话的外层循环：跑一次会话，失败就退避重试。
// 换耳机 / 拔插 USB 声卡会让流失效，自动重开比让用户重启程序好。
func (l *Loopback) worker(stop chan struct{}, done chan struct{}) {
	runtime.LockOSThread()
	defer close(done)

	hr, _, _ := procCoInit.Call(0, coinitMultithreaded)
	// S_OK(0) / S_FALSE(1) 都算成功；RPC_E_CHANGED_MODE 说明这个线程已经是
	// 别的单元模型，照样能用，此时不该调 CoUninitialize。
	if int32(hr) == 0 {
		defer procCoUnin.Call()
	}

	for attempt := 0; ; attempt++ {
		why, stopped, fatal := l.session(stop)
		if stopped {
			return
		}
		// fatal = 确定性失败（端点 ID 不存在之类），重试没有意义，
		// 立刻把原因报给 Start 的等待循环和 /api/loopback/status。
		// 「找不到指定播放设备」归 fatal（拔掉了就是拔掉了）；
		// 「找不到默认播放设备」保持重试 —— 驱动/设备启动慢时等它出现。
		if fatal || attempt >= 5 {
			l.mu.Lock()
			l.errMsg = why
			if !fatal {
				l.errMsg += "（已重试 5 次）"
			}
			l.running = false
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		l.errMsg = ""
		l.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(attempt+1) * 400 * time.Millisecond):
		}
	}
}

// session 跑一次完整的「打开 → 采集」，返回中断原因、是否收到停止信号、
// 是否为确定性失败（fatal=true 时 worker 不再退避重试）。
func (l *Loopback) session(stop chan struct{}) (why string, stopped, fatal bool) {
	var dev, client, capture, mixPtr unsafe.Pointer

	defer func() {
		if client != nil {
			comCall(client, vtblAudioClientStop)
		}
		comRelease(capture)
		comRelease(client)
		comRelease(dev)
		if mixPtr != nil {
			procCoFree.Call(uintptr(mixPtr))
		}
	}()

	// ---- 打开设备 ----
	var enumerator unsafe.Pointer
	hr, _, _ := procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumerator)))
	if hresBad(hr) || enumerator == nil {
		return "无法创建音频设备枚举器", false, true
	}
	defer comRelease(enumerator)

	// 指定了端点 ID 就按 ID 锁定；空 = 跟随默认播放设备。
	l.mu.Lock()
	sel := l.devSel
	l.mu.Unlock()
	if sel != "" {
		wsel := utf16Heap(sel)
		if wsel == nil {
			return "端点 ID 无法编码", false, true
		}
		hr = comCall(enumerator, vtblIMMDeviceGetDevice,
			uintptr(wsel), uintptr(unsafe.Pointer(&dev)))
		if hresBad(hr) || dev == nil {
			return "找不到指定播放设备（已拔出或端点 ID 失效？）", false, true
		}
	} else {
		hr = comCall(enumerator, vtblIMMDeviceGetDefaultEndpoint,
			eRender, eConsole, uintptr(unsafe.Pointer(&dev)))
		if hresBad(hr) || dev == nil {
			return "找不到默认播放设备（耳机没插好？）", false, false
		}
	}

	hr = comCall(dev, vtblIMMDeviceActivate,
		uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&client)))
	if hresBad(hr) || client == nil {
		return "无法激活 IAudioClient", false, false
	}

	// 共享模式**必须**用 GetMixFormat 给的格式初始化，自己指定采样率会拿到
	// AUDCLNT_E_UNSUPPORTED_FORMAT；回环模式下 IsFormatSupported 也帮不上忙。
	hr = comCall(client, vtblAudioClientGetMixFormat, uintptr(unsafe.Pointer(&mixPtr)))
	if hresBad(hr) || mixPtr == nil {
		return "无法读取混音格式", false, false
	}
	wf := (*waveFormatEx)(mixPtr)
	rate, ch, bits := int(wf.SamplesPerSec), int(wf.Channels), int(wf.BitsPerSample)
	tag, blockAlign := wf.FormatTag, int(wf.BlockAlign)
	rawTag, subTag, mask := tag, uint16(0), uint32(0)
	// WAVE_FORMAT_EXTENSIBLE：真正的编码在 SubFormat 里，取它的 Data1
	// （标准格式的 KSDATAFORMAT_SUBTYPE 前 4 字节就是格式标签）。
	if tag == 0xFFFE {
		ext := (*waveFormatExtensible)(mixPtr)
		subTag = uint16(ext.SubFormat.Data1)
		mask = ext.ChannelMask
		tag = subTag
	}
	if rate <= 0 || ch <= 0 || blockAlign <= 0 {
		return "混音格式异常", false, true
	}
	kind := "unsupported"
	switch {
	case tag == 3 && bits == 32:
		kind = "float32"
	case tag == 1 && bits == 16:
		kind = "pcm16"
	case tag == 1 && bits == 32:
		kind = "pcm32"
	case tag == 1 && bits == 24:
		kind = "pcm24"
	}

	l.mu.Lock()
	l.rate, l.ch, l.srcTag = rate, ch, kind
	l.bits, l.rawTag, l.subTag, l.chanMask = bits, rawTag, subTag, mask
	l.blockAlign = blockAlign
	l.devName = immDeviceName(dev)
	l.startAt = time.Now()
	l.frames = 0
	l.mu.Unlock()

	// Initialize：200ms 缓冲（100ns 单位）；共享模式的周期参数必须给 0。
	const bufDuration = 200 * 10000
	hr = comCall(client, vtblAudioClientInitialize,
		audclntShareShared, audclntLoopback, bufDuration, 0, uintptr(mixPtr), 0)
	if hresBad(hr) {
		return "打开耳机音频通道失败，请重新插拔耳机后重试", false, false
	}

	hr = comCall(client, vtblAudioClientGetService,
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capture)))
	if hresBad(hr) || capture == nil {
		return "无法获取 IAudioCaptureClient", false, false
	}

	hr = comCall(client, vtblAudioClientStart)
	if hresBad(hr) {
		return "启动耳机音频通道失败，请重新插拔耳机后重试", false, false
	}

	// ---- 采集循环 ----
	//
	// 刻意用轮询而不是 AUDCLNT_STREAMFLAGS_EVENTCALLBACK：事件驱动的回环采集
	// 有额外前提（历史版本直接返回 E_INVALIDARG），而 5ms 轮询的延迟对可视化
	// 完全够用，可靠性优先。
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	lastFFT := time.Now()

	for {
		select {
		case <-stop:
			return "", true, false
		case <-ticker.C:
		}

		broken := false
		for n := 0; n < 64; n++ {
			var packet uint32
			if hresBad(comCall(capture, vtblCaptureGetNextPacketSize,
				uintptr(unsafe.Pointer(&packet)))) {
				broken = true // 通常意味着流已失效（设备被拔了）
				break
			}
			if packet == 0 {
				break
			}
			var pData unsafe.Pointer
			var frames, flags uint32
			var devPos, qpc uint64
			if hresBad(comCall(capture, vtblCaptureGetBuffer,
				uintptr(unsafe.Pointer(&pData)),
				uintptr(unsafe.Pointer(&frames)),
				uintptr(unsafe.Pointer(&flags)),
				uintptr(unsafe.Pointer(&devPos)),
				uintptr(unsafe.Pointer(&qpc)))) {
				break
			}
			if frames > 0 {
				if flags&audclntSilent != 0 || pData == nil {
					l.pushSilence(int(frames))
				} else {
					l.pushFrames(pData, int(frames), ch, blockAlign, int(tag), bits)
				}
			}
			comCall(capture, vtblCaptureReleaseBuffer, uintptr(frames))
		}

		if time.Since(lastFFT) >= lbPeriod {
			lastFFT = time.Now()
			l.computeSpectrum()
		}
		if broken {
			return "回环流已中断", false, false
		}
	}
}

func hex32(v uintptr) string {
	const hexd = "0123456789ABCDEF"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = hexd[v&0xF]
		v >>= 4
	}
	return string(b[:])
}

// ---------------------------------------------------------------- 采样入环

func (l *Loopback) pushSilence(frames int) {
	l.mu.Lock()
	for i := 0; i < frames; i++ {
		l.ring[l.ringW] = 0
		l.ringW = (l.ringW + 1) % lbRingLen
	}
	l.ringN += frames
	l.frames += uint64(frames)
	l.mu.Unlock()
}

// lbDownmix 决定「多声道怎么变成一条波形」。
//
// ★ 只取前左/前右两声道求平均，**不是**对全部声道求平均。
//
// 原因：前端另一条音源（getDisplayMedia 屏幕捕获）拿到的是**立体声**流 ——
// 操作系统已经把 7.1 设备混成了两声道。若这边按 8 声道平均，
// 同一段音乐在两条路径上会差 20·log10(8/2) ≈ 12 dB ≈ 44 个字节，
// 表现为「切到后端回环地形突然矮一截」。取前对平均，与立体声下混等价。
//
// 代价：只存在于后置声道的 5.1/7.1 内容会漏掉。对可视化无所谓 ——
// iKF 这只有 8 声道的虚拟 7.1，实际内容都从前对进来。
func lbDownmix(ch int) (take int, inv float32) {
	if ch >= 2 {
		return 2, 0.5
	}
	return 1, 1.0
}

// pushFrames 把交错的多声道数据混成单声道 float32 写入环形缓冲。
//
// Windows 默认混音格式绝大多数是 WAVE_FORMAT_EXTENSIBLE + IEEE_FLOAT 32bit，
// 但板载声卡也可能给 PCM16，所以四种都实现 —— 少一种就会变成
// 「能打开却全是噪声」这种很难查的问题。
func (l *Loopback) pushFrames(pData unsafe.Pointer, frames, ch, blockAlign, tag, bits int) {
	raw := unsafe.Slice((*byte)(pData), frames*blockAlign)
	take, inv := lbDownmix(ch)

	l.mu.Lock()
	switch {
	case tag == 3 && bits == 32:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			var acc float32
			for c := 0; c < take; c++ {
				acc += math.Float32frombits(binary.LittleEndian.Uint32(raw[base+c*4:]))
			}
			l.ring[l.ringW] = acc * inv
			l.ringW = (l.ringW + 1) % lbRingLen
		}
	case tag == 1 && bits == 16:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			var acc float32
			for c := 0; c < take; c++ {
				acc += float32(int16(binary.LittleEndian.Uint16(raw[base+c*2:]))) / 32768
			}
			l.ring[l.ringW] = acc * inv
			l.ringW = (l.ringW + 1) % lbRingLen
		}
	case tag == 1 && bits == 32:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			var acc float32
			for c := 0; c < take; c++ {
				acc += float32(int32(binary.LittleEndian.Uint32(raw[base+c*4:]))) / 2147483648
			}
			l.ring[l.ringW] = acc * inv
			l.ringW = (l.ringW + 1) % lbRingLen
		}
	case tag == 1 && bits == 24:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			var acc float32
			for c := 0; c < take; c++ {
				o := base + c*3
				v := int32(raw[o]) | int32(raw[o+1])<<8 | int32(raw[o+2])<<16
				if v&0x800000 != 0 {
					v |= ^0xFFFFFF // 24 位符号扩展
				}
				acc += float32(v) / 8388608
			}
			l.ring[l.ringW] = acc * inv
			l.ringW = (l.ringW + 1) % lbRingLen
		}
	default:
		// 未知格式：写静音而不是乱码，至少不会把地形抖成噪点
		for f := 0; f < frames; f++ {
			l.ring[l.ringW] = 0
			l.ringW = (l.ringW + 1) % lbRingLen
		}
	}
	l.ringN += frames
	l.frames += uint64(frames)
	l.mu.Unlock()
}

// ---------------------------------------------------------------- 频谱

// Web Audio AnalyserNode 的默认参数，必须与前端 AnalyserNode 一致。
const (
	waMinDB      = -100.0
	waMaxDB      = -30.0
	waSmoothing  = 0.8
	blackmanGain = 0.42 // Blackman 窗的相干增益（Σw/N）
)

// ★ 归一化常数：Chromium 的 AnalyserNode 用的是 mag = |X[k]| / N，
// **不是**规范正文里那句「满幅正弦 = 0 dB」推出来的 2/(N·CG)。
//
// 这是实测出来的，不是推的（.workbuddy-ai/tmp/probe_wa_spectrum.js）：
// 把振荡器放在 bin 中心（937.5 Hz = 48000·20/1024），扫振幅，
// 等平滑收敛后读稳态字节，反解 mag = k·A：
//
//	amp 0.1     → 字节 242   k = 0.2097
//	amp 0.01    → 字节 169   k = 0.2087
//	amp 0.001   → 字节 96    k = 0.2078
//	amp 0.0001  → 字节 23    k = 0.2069
//
// 四个十年跨度上 k 稳定在 0.2085 附近（字节是截断取整，天然偏低 ≤1 个字节
// = 0.27 dB = 3.2%，所以真值就是 0.21 = CG/2）。也就是：
//
//	mag = |X_blackman[k]| / N，满幅正弦读 −13.56 dB 而不是 0 dB。
//
// 用 2/(N·0.42) 会整体高 20·log10(4.762) ≈ 13.56 dB ≈ 49 个字节 ——
// 表现是「后端回环那条路的地形明显比屏幕捕获那条高一头」。
// 两条音源必须同字节，所以这里跟 Chromium 走，不跟规范正文走。
//
// 交叉验证：0 dB 截顶档的并列区间，实测 amp=1 是 #19..#21（3 格），
// 与本归一化下的 Blackman 主瓣形状一致（旧的 4.76 倍会给出 5 格）。
const waMagScale = 1.0 / float64(lbFFTSize)

var (
	fftCosTab []float64
	fftSinTab []float64
	fftWinTab []float64
)

func init() {
	fftCosTab = make([]float64, lbFFTSize)
	fftSinTab = make([]float64, lbFFTSize)
	for i := 0; i < lbFFTSize; i++ {
		a := 2 * math.Pi * float64(i) / lbFFTSize
		fftCosTab[i] = math.Cos(a)
		fftSinTab[i] = math.Sin(a)
	}
	fftWinTab = make([]float64, lbFFTSize)
	for n := 0; n < lbFFTSize; n++ {
		x := 2 * math.Pi * float64(n) / lbFFTSize
		fftWinTab[n] = 0.42 - 0.5*math.Cos(x) + 0.08*math.Cos(2*x)
	}
}

// computeSpectrum 取最近 1024 个样本做一次 FFT，更新 l.bins 与 l.level。
//
// 只在采集线程上调用，所以取样本、FFT、写 bins 三段是串行的；
// 只有读/写共享字段时才持锁，FFT 本体不持锁。
func (l *Loopback) computeSpectrum() {
	re := make([]float64, lbFFTSize)
	im := make([]float64, lbFFTSize)

	l.mu.Lock()
	if l.ringN < lbFFTSize {
		l.mu.Unlock()
		return
	}
	start := (l.ringW - lbFFTSize + lbRingLen) % lbRingLen
	for i := 0; i < lbFFTSize; i++ {
		re[i] = float64(l.ring[(start+i)%lbRingLen]) * fftWinTab[i]
	}
	l.mu.Unlock()

	fftRadix2(re, im)

	// 归一化见 waMagScale 的说明：|X[k]|/N，与 Chromium 的 AnalyserNode 逐字节对齐。
	norm := waMagScale
	var level float32

	l.mu.Lock()
	for k := 0; k < lbBins; k++ {
		mag := math.Hypot(re[k], im[k]) * norm
		// 平滑按规范做在幅度上。prev 存 float32 —— 浏览器的
		// AnalyserNode 内部也是 float 累加，这里保持一致，避免长跑后
		// 与屏幕捕获那条路径的字节值出现系统性偏差。
		s := waSmoothing*float64(l.prev[k]) + (1-waSmoothing)*mag
		l.prev[k] = float32(s)
		if s > float64(level) {
			level = float32(s)
		}
		db := -1000.0
		if s > 0 {
			db = 20 * math.Log10(s)
		}
		v := (db - waMinDB) / (waMaxDB - waMinDB) * 255
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		l.bins[k] = byte(v)
	}
	if level > 1 {
		level = 1
	}
	l.level = level
	l.mu.Unlock()
}

// fftRadix2 就地做基 2 复数 FFT。长度必须是 2 的幂。
func fftRadix2(re, im []float64) {
	n := len(re)
	if n < 2 || n&(n-1) != 0 {
		return
	}
	// 位反转置换
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	// 蝶形。twiddle 用长度 n 的整表索引：e^{-2πi·k·n/length/n}，
	// 索引 k*(n/length) 一定落在 [0, n/2) 内，直接查表。
	for length := 2; length <= n; length <<= 1 {
		half := length >> 1
		step := n / length
		for i := 0; i < n; i += length {
			for k := 0; k < half; k++ {
				idx := k * step
				cr, ci := fftCosTab[idx], -fftSinTab[idx]
				ar, ai := re[i+k], im[i+k]
				br, bi := re[i+k+half], im[i+k+half]
				tr, ti := br*cr-bi*ci, br*ci+bi*cr
				re[i+k], im[i+k] = ar+tr, ai+ti
				re[i+k+half], im[i+k+half] = ar-tr, ai-ti
			}
		}
	}
}
