//go:build windows

// WASAPI 回环采集（系统输出 → 频谱）—— P1 清退后的纯转发层
// ====================================================================
//
// 采集/FFT/环形缓冲的全部实现住在 audiocore（Rust cdylib，见
// audiocore/src/{loopback,spectrum}.rs），经 audio.go 的 LoadLibrary 绑定
// 使用。本文件只剩三类东西：
//
//  1. Loopback 的公开方法壳（Start/Stop/Status/Spectrum/Devices）——
//     app.go / cli.go 的调用点不动，前端与闸无感知；
//  2. **Go 侧频谱对拍基准**（computeSpectrum/fftRadix2/表 + wa* 常量）——
//     `--loopback-fft` 自检与 TestRustFFTParity 靠它逐字节对拍，护栏不能删；
//  3. Rust 路径转发（startRust/stopRust/statusRust/spectrumRust）。
//
// 与 Web Audio 的语义对齐（历史决策，Rust 侧 spectrum.rs 逐条继承）：
//
//	· Blackman 窗（相干增益 0.42）· 幅度归一化 mag = |X[k]| / N（实测反解，
//	  满幅正弦读 −13.56 dB，不是规范正文的 0 dB）· 平滑 τ=0.8 每调用一次
//	  施加一次 · 字节映射截断取整，minDecibels=−100、maxDecibels=−30
package main

import (
	"runtime"
	"encoding/base64"
	"math"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------- 频谱常量

// 只保留最近的 8192 个单声道样本（48 kHz 下约 170ms）。
// 做 1024 点 FFT 只需要最后 1024 个，留余量是为了容忍轮询抖动。
const (
	lbRingLen = 8192
	lbFFTSize = 1024
	lbBins    = lbFFTSize / 2 // 512，与 Web Audio frequencyBinCount 一致
)

// Web Audio AnalyserNode 的默认参数，必须与前端 AnalyserNode 一致。
const (
	waMinDB      = -100.0
	waMaxDB      = -30.0
	waSmoothing  = 0.8
	blackmanGain = 0.42 // Blackman 窗的相干增益（Σw/N）
)

// 归一化常数：Chromium 的 AnalyserNode 用 mag = |X[k]| / N（实测反解，
// 见 audiocore/src/spectrum.rs 的同款注记）。
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

// ---------------------------------------------------------------- 会话壳

// Loopback 是一路系统回环采集会话（实现在 Rust）。
// 保留的字段分两组：startRust 等转发的记账快照（前端展示），和
// synthSpectrum 用的频谱状态（对拍基准）。
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

	// 混音格式的原始字段，供自检诊断。
	bits       int
	rawTag     uint16
	subTag     uint16
	chanMask   uint32
	blockAlign int

	// 频谱对拍基准状态（synthSpectrum 灌样本 → computeSpectrum）。
	ring  []float32
	ringW int
	ringN int
	prev  []float32
	bins  []byte
	level float32

	startAt time.Time

	// rustH != 0 = Rust 会话句柄（DLL 侧 Box<LoopbackSession>）。
	rustH uintptr
}

func NewLoopback() *Loopback {
	return &Loopback{
		ring: make([]float32, lbRingLen),
		prev: make([]float32, lbBins),
		bins: make([]byte, lbBins),
	}
}

// Start 打开一路回环。device 是端点 ID（Devices() 列出来的那种），
// 空 = 跟随系统默认。
func (l *Loopback) Start(device string) map[string]interface{} {
	return l.startRust(device)
}

// Stop 关掉会话。
func (l *Loopback) Stop() map[string]interface{} {
	return l.stopRust()
}

// Status 供 /api/loopback/status 与自检使用。
func (l *Loopback) Status() map[string]interface{} {
	return l.statusRust()
}

// Devices 列出可锁定的播放端点。
func (l *Loopback) Devices() map[string]interface{} {
	if m, ok := lbDevicesViaRust(); ok {
		return m
	}
	return map[string]interface{}{"ok": false, "error": "无法枚举播放设备（ikf_audio.dll 不可用？）"}
}

// Spectrum 返回当前 512 个频谱字节（base64，契约与 Python 版一致）。
func (l *Loopback) Spectrum() map[string]interface{} {
	return l.spectrumRust()
}

// ---------------------------------------------------------------- 对拍基准

// computeSpectrum 取最近 1024 个样本做一次 FFT，更新 l.bins 与 l.level。
// 这份 Go 实现**只**被 synthSpectrum（--loopback-fft 对拍基准）使用；
// 运行时频谱由 Rust 侧提供，两者必须逐字节一致（TestRustFFTParity 锁定）。
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

	norm := waMagScale
	var level float32

	l.mu.Lock()
	for k := 0; k < lbBins; k++ {
		mag := math.Hypot(re[k], im[k]) * norm
		// 平滑按规范做在幅度上。prev 存 float32 —— 浏览器的
		// AnalyserNode 内部也是 float 累加，这里保持一致。
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
	// 蝶形。twiddle 用长度 n 的整表索引，直接查表。
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

// ---------------------------------------------------------------- Rust 转发

// startRust 打开设备的 COM 调用必须在**无 COM 历史的 MTA 线程**上做：
// 面板主线程是 STA（webview2 要求），STA 上创建的对象移交给 Rust 采集
// 线程（MTA）= 编组违规，会话静默失败（清退首版真机踩坑）。所以把
// startRustInner 包进锁定 OS 线程的干净 goroutine——与原 Go worker 的
// 线程模型（专用线程 + 自行 CoInitializeEx）语义一致。
func (l *Loopback) startRust(device string) map[string]interface{} {
	ch := make(chan map[string]interface{}, 1)
	go func() {
		runtime.LockOSThread()
		ch <- l.startRustInner(device)
	}()
	return <-ch
}

func (l *Loopback) startRustInner(device string) map[string]interface{} {
	if !audioReady() {
		return map[string]interface{}{"ok": false,
			"error": "ikf_audio.dll 不可用（音频热路未就绪），无法打开回环"}
	}
	l.mu.Lock()
	if l.rustH != 0 && l.devSel == device {
		st, _ := lbStatusViaRust(l.rustH)
		l.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"rate": int(st.rate), "ch": int(st.ch), "format": cstr(st.kind[:]),
			"dev": cstr(st.dev[:]), "bits": int(st.bits),
			"tag": st.rawTag, "sub": st.subTag}
	}
	if l.rustH != 0 {
		// 端点换了：先收旧句柄（DLL 内部 join 采集线程）
		h := l.rustH
		l.rustH = 0
		l.running = false
		l.mu.Unlock()
		audioDLL.NewProc("ikf_loopback_close").Call(h)
		l.mu.Lock()
	}
	var epPtr uintptr
	if device != "" {
		w, err := syscall.UTF16FromString(device)
		if err != nil {
			l.mu.Unlock()
			return map[string]interface{}{"ok": false, "error": "端点 ID 无法编码"}
		}
		epPtr = uintptr(unsafe.Pointer(&w[0]))
	}
	var errBuf [256]byte
	l.mu.Unlock()
	h, _, _ := audioDLL.NewProc("ikf_loopback_open").Call(
		epPtr, uintptr(unsafe.Pointer(&errBuf[0])), uintptr(len(errBuf)))
	if h == 0 {
		return map[string]interface{}{"ok": false, "error": cstr(errBuf[:])}
	}
	// 等采集线程填好端点友好名（与 listen 同款；start 响应的 dev 直接
	// 显示在可视化页右上角，空名会退化成"默认播放设备"）
	deadline := time.Now().Add(3 * time.Second)
	var st lbStatusC
	for {
		st, _ = lbStatusViaRust(h)
		if cstr(st.dev[:]) != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.mu.Lock()
	l.rustH = h
	l.devSel = device
	l.running = true
	l.errMsg = ""
	l.rate, l.ch, l.srcTag = int(st.rate), int(st.ch), cstr(st.kind[:])
	l.devName = cstr(st.dev[:])
	l.bits, l.rawTag, l.subTag = int(st.bits), st.rawTag, st.subTag
	l.chanMask, l.blockAlign = st.chanMask, int(st.blockAlign)
	l.startAt = time.Now()
	l.mu.Unlock()
	return map[string]interface{}{"ok": true, "rate": int(st.rate), "ch": int(st.ch),
		"format": cstr(st.kind[:]), "dev": cstr(st.dev[:]),
		"bits": int(st.bits), "tag": st.rawTag, "sub": st.subTag}
}

func (l *Loopback) stopRust() map[string]interface{} {
	l.mu.Lock()
	h := l.rustH
	l.rustH = 0
	l.running = false
	l.rate, l.ch, l.srcTag = 0, 0, ""
	l.devName = ""
	l.bits, l.rawTag, l.subTag, l.chanMask, l.blockAlign = 0, 0, 0, 0, 0
	l.ringW, l.ringN = 0, 0
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
	if h != 0 {
		audioDLL.NewProc("ikf_loopback_close").Call(h)
	}
	return map[string]interface{}{"ok": true}
}

// frames 记账字段（Status 展示用；真实值在 DLL 侧，statusRust 拉取覆盖）。
var _ = struct{}{}

func (l *Loopback) statusRust() map[string]interface{} {
	// ★破坏性检测 P0-3：DLL 调用与字段读取全程持锁 —— 原实现拿完 h 就
	// 解锁，stopRust 可在窗口内 close(h)，status 读到已释放的会话
	// （假成功比崩溃更难发现）。DLL 调用微秒级，锁内不构成热点。
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.rustH
	out := map[string]interface{}{
		"ok": true, "running": l.running, "rate": l.rate, "ch": l.ch,
		"format": l.srcTag, "bins": lbBins, "frames": uint64(0),
		"level": 0.0, "dev": l.devName,
		"bits": l.bits, "tag": l.rawTag, "sub": l.subTag,
		"mask": l.chanMask, "align": l.blockAlign,
	}
	if h == 0 {
		return out
	}
	st, ok := lbStatusViaRust(h)
	if !ok {
		return out
	}
	out["running"] = st.running != 0
	out["rate"] = int(st.rate)
	out["ch"] = int(st.ch)
	out["frames"] = st.frames
	out["level"] = float64(int(st.level*1000)) / 1000
	out["dev"] = cstr(st.dev[:])
	out["bits"] = int(st.bits)
	out["tag"] = st.rawTag
	out["sub"] = st.subTag
	out["mask"] = st.chanMask
	out["align"] = int(st.blockAlign)
	if e := cstr(st.err[:]); e != "" {
		out["error"] = e
	}
	if l.running && !l.startAt.IsZero() {
		out["uptime"] = int(time.Since(l.startAt).Seconds())
	}
	return out
}

func (l *Loopback) spectrumRust() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, running, rate := l.rustH, l.running, l.rate
	if !running || h == 0 {
		return map[string]interface{}{"ok": false, "running": false, "error": "回环未启动"}
	}
	bins, ok := lbSpectrumViaRust(h)
	if !ok {
		return map[string]interface{}{"ok": false, "running": false, "error": "频谱读取失败"}
	}
	st, _ := lbStatusViaRust(h)
	return map[string]interface{}{
		"ok": true, "running": true, "rate": rate, "ch": int(st.ch),
		"bins":   base64.StdEncoding.EncodeToString(bins),
		"level":  float64(int(st.level*1000)) / 1000,
		"frames": st.frames,
		"peak":   0.0, // 原始峰值不维护（level 足够），诊断看 level
	}
}
