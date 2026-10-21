package main

// listen.go —— 内嵌麦克风监听（2026-10-20）
//
// 为什么不走官方路径（drv.Listen → osConfLib.RunListen2Device）：
//   反编译实锤（内部取证报告）：RunListen2Device
//   内部 CoCreateInstance(CLSID {E642D042-5BCF-4E80-9451-83F4E398A88F})，
//   该 COM 服务器在整套 WHQL 驱动包、System32、注册表里都不存在
//   （CLSID 只出现在 osConfLib 调用方体内）——官方原版面板同样开不了，
//   实测恒返 0x80040154（REGDB_E_CLASSNOTREG）。
// 所以麦克风页「监听」开关改为本文件：WASAPI 采默认麦克风端点（麦克风
// 自己的混音格式），软件上混进渲染端自己的混音格式（样本写 FL/FR），
// 实时送默认输出设备播放（等价 Windows 声音控制台里自带的「侦听此设备」）。
// ★ 转换必须自己做：WASAPI 引擎的 AUTOCONVERTPCM 无论放采集侧还是渲染
//   侧都会产出静音（本机实测，见 session 内 2026-10-20 排障注释）。
//
// 与 Python 线 center_src.py 的 MicListen 逐字段对齐；COM/采集基建
// 复用 loopback.go / recorder.go 的手写虚表。

import (
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// IAudioRenderClient 与渲染侧虚表下标（loopback 只用采集侧，缺这些）
var iidIAudioRenderClient = guidT{0xF294ACFC, 0x3146, 0x4483,
	[8]byte{0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2}}

const (
	vtblAudioClientGetBufferSize     = 4
	vtblAudioClientGetCurrentPadding = 6
	vtblRenderGetBuffer              = 3
	vtblRenderReleaseBuffer          = 4

	// eCapture = 1：GetDefaultAudioEndpoint 的 dataFlow（监听仍用
	// 默认采集端点；录音机已迁到渲染端 loopback，eRender 在 loopback.go）。
	eCapture = 1

	// AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM（Win8.1+）：允许用与端点
	// 混音格式不同的格式初始化，引擎内部转换；SRC_DEFAULT_QUALITY
	// 用默认质量做重采样。采集侧因此可以按「渲染端点的混音格式」开，
	// 两边字节流同格式直接搬运。
	audclntAutoConvert    = 0x80000000
	audclntSrcDefaultQual = 0x08000000
)

// MicListen 一路麦克风监听会话。
//
// 线程模型与 MicRecorder 相同：COM 钉专用 OS 线程、单会话不重试
// （监听是用户在场操作，断流自动重拉只会造成爆音与状态错乱）。
type MicListen struct {
	mu      sync.Mutex
	running bool
	errMsg  string
	devName string // 采集端点（麦克风）友好名
	outName string // 渲染端点（输出）友好名
	rate    int
	ch      int

	sent uint64 // 已送入渲染端点的字节数（验收断言用：必须持续增长）

	// 排障诊断（2026-10-20 静音 bug）：采集包计数、SILENT 旗标包数、
	// 采集数据峰值（float32 绝对值最大，正常底噪 ~3e-4，全零=0）
	pkts       uint64
	silentPkts uint64
	capPeak    float32

	stopCh chan struct{}
	doneCh chan struct{}
}

func NewMicListen() *MicListen { return &MicListen{} }

var lis = NewMicListen()

// ---------------------------------------------------------------- 出入

// Start 开始监听。已在听则幂等返回 ok。
func (l *MicListen) Start() map[string]interface{} {
	l.mu.Lock()
	if l.running {
		dev, out, rate, ch := l.devName, l.outName, l.rate, l.ch
		l.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"dev": dev, "out": out, "rate": rate, "ch": ch}
	}
	l.errMsg = ""
	l.devName, l.outName = "", ""
	l.rate, l.ch = 0, 0
	l.sent = 0
	l.stopCh = make(chan struct{})
	l.doneCh = make(chan struct{})
	stop, done := l.stopCh, l.doneCh
	l.running = true
	l.mu.Unlock()

	go l.worker(stop, done)

	// 等 worker 把两端设备都开好（devName 非空即开好）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		dev, out, rate, ch, errMsg := l.devName, l.outName, l.rate, l.ch, l.errMsg
		l.mu.Unlock()
		if dev != "" {
			return map[string]interface{}{"ok": true,
				"dev": dev, "out": out, "rate": rate, "ch": ch}
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
	return map[string]interface{}{"ok": false, "error": "打开监听设备超时"}
}

// Stop 停止监听并等线程收尾。未在听时幂等返回 ok。
func (l *MicListen) Stop() map[string]interface{} {
	l.mu.Lock()
	if !l.running {
		errMsg := l.errMsg
		l.mu.Unlock()
		out := map[string]interface{}{"ok": true, "already": true}
		if errMsg != "" {
			out["error"] = errMsg
		}
		return out
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
	case <-time.After(3 * time.Second):
	}

	l.mu.Lock()
	l.running = false
	errMsg := l.errMsg
	l.mu.Unlock()
	out := map[string]interface{}{"ok": true}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

// Status 供诊断与验收断言（sent 必须在监听中持续增长）。
func (l *MicListen) Status() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]interface{}{
		"ok": true, "on": l.running, "dev": l.devName,
		"out": l.outName, "rate": l.rate, "ch": l.ch, "sent": l.sent,
		"pkts": l.pkts, "silentPkts": l.silentPkts, "capPeak": l.capPeak,
	}
	if l.errMsg != "" {
		out["error"] = l.errMsg
	}
	return out
}

// ---------------------------------------------------------------- 线程

func (l *MicListen) worker(stop chan struct{}, done chan struct{}) {
	// 与 loopback/recorder 同款：锁 OS 线程 + COM 单元初始化。
	hr, _, _ := procCoInit.Call(0, coinitMultithreaded)
	if int32(hr) == 0 {
		defer procCoUnin.Call()
	}
	l.session(stop)
	close(done)
}

func (l *MicListen) session(stop chan struct{}) {
	var devC, devR, cliC, cliR, capC, renC, mixPtr, renPtr unsafe.Pointer
	var bufFrames uint32

	// 共享模式 200ms
	const bufDuration = 200 * 10000

	defer func() {
		if cliC != nil {
			comCall(cliC, vtblAudioClientStop)
		}
		if cliR != nil {
			comCall(cliR, vtblAudioClientStop)
		}
		comRelease(capC)
		comRelease(renC)
		comRelease(cliC)
		comRelease(cliR)
		comRelease(devC)
		comRelease(devR)
		if mixPtr != nil {
			procCoFree.Call(uintptr(mixPtr))
		}
		if renPtr != nil {
			procCoFree.Call(uintptr(renPtr))
		}
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
	}()

	var enumerator unsafe.Pointer
	hr, _, _ := procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumerator)))
	if hresBad(hr) || enumerator == nil {
		l.fail("无法创建音频设备枚举器")
		return
	}
	defer comRelease(enumerator)

	// ★★ 2026-10-20 排障实录（三次实测才定位，两线同病）：
	//   v1 采集侧 AUTOCONVERTPCM（采集按渲染混音格式 8ch 开）→ 静音；
	//   v2 渲染侧 AUTOCONVERTPCM（渲染按采集格式请求）→ 依旧静音。
	//   判据：监听开着时用输出端点回环采 30 万帧**原始浮点**峰值精确 0，
	//   而采集流有真实底噪（capPeak≈0.0017、silentPkts=0、sent 持续增长，
	//   即引擎在消费我们的帧却产出静音）。
	//   v3 终案：两端都用各自的原生混音格式，**软件上混**——渲染端按
	//   渲染混音格式（8ch float32）标准初始化，把单声道样本写进 FL/FR。
	//   不再依赖 WASAPI 引擎的任何格式转换。

	// ---- 采集端点：麦克风自己的混音格式（共享模式混合格式必为 float32） ----
	hr = comCall(enumerator, vtblIMMDeviceGetDefaultEndpoint,
		eCapture, eConsole, uintptr(unsafe.Pointer(&devC)))
	if hresBad(hr) || devC == nil {
		l.fail("找不到默认麦克风（耳机没插好？）")
		return
	}
	hr = comCall(devC, vtblIMMDeviceActivate,
		uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&cliC)))
	if hresBad(hr) || cliC == nil {
		l.fail("无法激活采集端 IAudioClient")
		return
	}
	hr = comCall(cliC, vtblAudioClientGetMixFormat, uintptr(unsafe.Pointer(&mixPtr)))
	if hresBad(hr) || mixPtr == nil {
		l.fail("无法读取麦克风混音格式")
		return
	}
	wf := (*waveFormatEx)(mixPtr)
	rate, ch, bits := int(wf.SamplesPerSec), int(wf.Channels), int(wf.BitsPerSample)
	tag, blockAlign := int(wf.FormatTag), int(wf.BlockAlign)
	if tag == 0xFFFE {
		tag = int((*waveFormatExtensible)(mixPtr).SubFormat.Data1)
	}
	if rate <= 0 || ch <= 0 || blockAlign <= 0 {
		l.fail("麦克风混音格式异常")
		return
	}
	if tag != 3 { // 共享模式混合格式必为 float32；别的情况直接拒绝（不猜格式）
		l.fail(fmt.Sprintf("麦克风混音格式不受支持（tag=%d bits=%d）", tag, bits))
		return
	}
	hr = comCall(cliC, vtblAudioClientInitialize,
		audclntShareShared, 0, bufDuration, 0, uintptr(mixPtr), 0)
	if hresBad(hr) {
		l.fail("采集端 Initialize 失败，HRESULT=0x" + hex32(hr))
		return
	}
	hr = comCall(cliC, vtblAudioClientGetService,
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capC)))
	if hresBad(hr) || capC == nil {
		l.fail("无法获取 IAudioCaptureClient")
		return
	}

	// ---- 输出客户端：渲染端自己的混音格式（软件上混，不用引擎转换） ----
	hr = comCall(enumerator, vtblIMMDeviceGetDefaultEndpoint,
		eRender, eConsole, uintptr(unsafe.Pointer(&devR)))
	if hresBad(hr) || devR == nil {
		l.fail("找不到默认输出设备")
		return
	}
	hr = comCall(devR, vtblIMMDeviceActivate,
		uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&cliR)))
	if hresBad(hr) || cliR == nil {
		l.fail("无法激活输出端 IAudioClient")
		return
	}
	hr = comCall(cliR, vtblAudioClientGetMixFormat, uintptr(unsafe.Pointer(&renPtr)))
	if hresBad(hr) || renPtr == nil {
		l.fail("无法读取输出混音格式")
		return
	}
	wfR := (*waveFormatEx)(renPtr)
	renCh, renBits := int(wfR.Channels), int(wfR.BitsPerSample)
	renTag, renBlockAlign := int(wfR.FormatTag), int(wfR.BlockAlign)
	if renTag == 0xFFFE {
		renTag = int((*waveFormatExtensible)(renPtr).SubFormat.Data1)
	}
	if renCh <= 0 || renBlockAlign <= 0 {
		l.fail("输出混音格式异常")
		return
	}
	if renTag != 3 || renBits != 32 { // 混合格式必为 float32；别的情况拒绝
		l.fail(fmt.Sprintf("输出混音格式不受支持（tag=%d bits=%d）", renTag, renBits))
		return
	}
	hr = comCall(cliR, vtblAudioClientInitialize,
		audclntShareShared, 0, bufDuration, 0, uintptr(renPtr), 0)
	if hresBad(hr) {
		l.fail("输出端 Initialize 失败，HRESULT=0x" + hex32(hr))
		return
	}
	hr = comCall(cliR, vtblAudioClientGetBufferSize,
		uintptr(unsafe.Pointer(&bufFrames)))
	if hresBad(hr) || bufFrames == 0 {
		l.fail("无法读取输出缓冲大小")
		return
	}
	hr = comCall(cliR, vtblAudioClientGetService,
		uintptr(unsafe.Pointer(&iidIAudioRenderClient)),
		uintptr(unsafe.Pointer(&renC)))
	if hresBad(hr) || renC == nil {
		l.fail("无法获取 IAudioRenderClient")
		return
	}

	hr = comCall(cliC, vtblAudioClientStart)
	if hresBad(hr) {
		l.fail("采集端 Start 失败，HRESULT=0x" + hex32(hr))
		return
	}
	hr = comCall(cliR, vtblAudioClientStart)
	if hresBad(hr) {
		l.fail("输出端 Start 失败，HRESULT=0x" + hex32(hr))
		return
	}

	l.mu.Lock()
	l.devName = immDeviceName(devC)
	l.outName = immDeviceName(devR)
	l.rate, l.ch = rate, ch
	l.mu.Unlock()

	// ---- 主循环：5ms 轮询。采包 → pending；pending → 渲染缓冲 ----
	pending := make([]byte, 0, 1<<16)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		broken := false
		// 1) 排干采集包（静音包补零，保证渲染时钟一直有料）
		for n := 0; n < 64; n++ {
			var packet uint32
			if hresBad(comCall(capC, vtblCaptureGetNextPacketSize,
				uintptr(unsafe.Pointer(&packet)))) {
				broken = true
				break
			}
			if packet == 0 {
				break
			}
			var pData unsafe.Pointer
			var frames, flags uint32
			var devPos, qpc uint64
			if hresBad(comCall(capC, vtblCaptureGetBuffer,
				uintptr(unsafe.Pointer(&pData)),
				uintptr(unsafe.Pointer(&frames)),
				uintptr(unsafe.Pointer(&flags)),
				uintptr(unsafe.Pointer(&devPos)),
				uintptr(unsafe.Pointer(&qpc)))) {
				broken = true
				break
			}
			if frames > 0 {
				l.mu.Lock()
				l.pkts++
				if flags&audclntSilent != 0 || pData == nil {
					l.silentPkts++
					l.mu.Unlock()
					pending = append(pending,
						make([]byte, int(frames)*blockAlign)...)
				} else {
					raw := unsafe.Slice((*byte)(pData), int(frames)*blockAlign)
					// 采集数据峰值（float32）：诊断「采集侧就是静音」用
					for i := 0; i+3 < len(raw); i += 4 {
						v := *(*float32)(unsafe.Pointer(&raw[i]))
						if v < 0 {
							v = -v
						}
						if v > l.capPeak {
							l.capPeak = v
						}
					}
					l.mu.Unlock()
					pending = append(pending, raw...)
				}
			}
			comCall(capC, vtblCaptureReleaseBuffer, uintptr(frames))
		}
		if broken {
			l.fail("麦克风数据流已中断（设备被拔出？）")
			return
		}

		// 2) 软件上混后往渲染端推：单声道 float32 → 渲染混音格式，
		//    样本写入 FL/FR（前两声道），其余声道清零。
		//    积压超过一个缓冲说明消费不掉（设备暂停）：丢弃到最新，
		//    避免延迟无限增长。
		byteAvail := int(bufFrames) * blockAlign
		if len(pending) > byteAvail {
			pending = pending[len(pending)-byteAvail:]
		}
		for len(pending) >= blockAlign {
			var pad uint32
			if hresBad(comCall(cliR, vtblAudioClientGetCurrentPadding,
				uintptr(unsafe.Pointer(&pad)))) {
				l.fail("输出端数据流已中断")
				return
			}
			free := int(bufFrames) - int(pad)
			if free <= 0 {
				break
			}
			n := free
			if n > len(pending)/blockAlign {
				n = len(pending) / blockAlign
			}
			var pData unsafe.Pointer
			if hresBad(comCall(renC, vtblRenderGetBuffer, uintptr(n),
				uintptr(unsafe.Pointer(&pData)))) || pData == nil {
				l.fail("输出端 GetBuffer 失败")
				return
			}
			dst := unsafe.Slice((*byte)(pData), n*renBlockAlign)
			// 整块清零（其余声道必须归零），再写 FL/FR
			for i := range dst {
				dst[i] = 0
			}
			src := pending[:n*blockAlign]
			for f := 0; f < n; f++ {
				s := *(*float32)(unsafe.Pointer(&src[f*blockAlign]))
				o := f * renBlockAlign
				*(*float32)(unsafe.Pointer(&dst[o])) = s   // FL
				*(*float32)(unsafe.Pointer(&dst[o+4])) = s // FR
			}
			comCall(renC, vtblRenderReleaseBuffer, uintptr(n), 0)
			pending = pending[n*blockAlign:]
			l.mu.Lock()
			l.sent += uint64(n * renBlockAlign)
			l.mu.Unlock()
		}
	}
}

func (l *MicListen) fail(why string) {
	l.mu.Lock()
	l.errMsg = why
	l.mu.Unlock()
}
