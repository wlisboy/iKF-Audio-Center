package main

// recorder.go —— 耳机录音（2026-10-20 晚，用户拍板改名迁移）
//
// 演进：官方捆绑的 SoundRecorder\Recorder.exe 是依附官方面板 COM 环境的
// 薄客户端（导入表仅 KERNEL32+ole32，无 user32/winmm），脱离官方面板
// 单独拉起必然静默退出（实测恒退 241）—— 取证见 内部取证报告（未随仓库分发）。
// 第一版内嵌实现录的是**默认麦克风**（麦克风页「录音机」）；
// 本版按用户拍板迁移到声音页改名「耳机录音」：录渲染端点回环
// （WASAPI loopback 采集，耳机里正在播放的声音），麦克风录音入口彻底移除。
//
// 写 16bit 立体声 WAV 到「音乐\iKF Audio Center\」（Known Folder，吃
// OneDrive 重定向）。API 形状不变：POST /api/recorder/start|stop +
// GET /api/recorder/status。
//
// 与 Python 线 center_src.py 的 MicRecorder 逐字段对齐（API 形状、
// 文件命名、状态字段）；COM/采集基建复用 loopback.go 的手写虚表。

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// MicRecorder 一路耳机录音会话（渲染端点 loopback 采集）。
//
// 线程模型与 Loopback 相同：COM 单元状态是线程局部的，采集钉在专用
// OS 线程上。与 Loopback 的差别是**不自动重试**——录音是用户在场操作，
// 流中断后自动换文件续录会让一段录音裂成多份，宁可明确报错。
type MicRecorder struct {
	mu      sync.Mutex
	running bool
	errMsg  string
	rate    int
	ch      int    // WAV 声道数（恒 2：立体声输出）
	srcTag  string // 源编码（float32/pcm16/…），诊断用
	devName string // 实际打开的端点友好名（默认输出设备）

	file    string  // 最近一次（或正在进行的）WAV 全路径
	seconds float64 // 最近一次录音时长（进行中实时增长）
	size    int64   // 最近一次 WAV 数据字节数（不含头）
	frames  uint64

	startAt time.Time

	stopCh chan struct{}
	doneCh chan struct{}
}

func NewMicRecorder() *MicRecorder { return &MicRecorder{} }

var rec = NewMicRecorder()

// ---------------------------------------------------------------- 出入

// Start 开始录音。已在录则幂等返回 ok。
func (m *MicRecorder) Start() map[string]interface{} {
	m.mu.Lock()
	if m.running {
		file, rate, ch, dev := m.file, m.rate, m.ch, m.devName
		m.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"file": file, "rate": rate, "ch": ch, "dev": dev}
	}
	m.errMsg = ""
	m.rate, m.ch, m.srcTag = 0, 0, ""
	m.devName = ""
	m.frames, m.size, m.seconds = 0, 0, 0
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	stop, done := m.stopCh, m.doneCh
	m.running = true
	m.mu.Unlock()

	go m.worker(stop, done)

	// 等 worker 把设备打开（rate>0 即开好）。COM 打开通常几毫秒。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		rate, ch, errMsg, file, dev := m.rate, m.ch, m.errMsg, m.file, m.devName
		m.mu.Unlock()
		if rate > 0 {
			return map[string]interface{}{"ok": true, "file": file,
				"rate": rate, "ch": ch, "dev": dev}
		}
		if errMsg != "" {
			m.mu.Lock()
			m.running = false
			m.mu.Unlock()
			return map[string]interface{}{"ok": false, "error": errMsg}
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
	close(stop)
	return map[string]interface{}{"ok": false, "error": "打开输出设备超时"}
}

// Stop 停止录音并等采集线程把 WAV 收尾。未在录时返回最近一次的结果。
func (m *MicRecorder) Stop() map[string]interface{} {
	m.mu.Lock()
	if !m.running {
		file, seconds, size, errMsg := m.file, m.seconds, m.size, m.errMsg
		m.mu.Unlock()
		out := map[string]interface{}{"ok": true, "already": true,
			"file": file, "seconds": seconds, "size": size}
		if errMsg != "" {
			out["error"] = errMsg
		}
		return out
	}
	stop, done := m.stopCh, m.doneCh
	m.mu.Unlock()

	select {
	case <-stop:
	default:
		close(stop)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	m.mu.Lock()
	m.running = false
	file, seconds, size, errMsg := m.file, m.seconds, m.size, m.errMsg
	m.mu.Unlock()
	out := map[string]interface{}{"ok": true, "file": file,
		"seconds": seconds, "size": size}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

// Status 供 /api/recorder/status。
func (m *MicRecorder) Status() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]interface{}{
		"ok": true, "recording": m.running, "file": m.file,
		"seconds": m.seconds, "size": m.size,
		"rate": m.rate, "ch": m.ch, "dev": m.devName,
		"format": m.srcTag,
	}
	if m.errMsg != "" {
		out["error"] = m.errMsg
	}
	return out
}

// ---------------------------------------------------------------- 线程

func (m *MicRecorder) worker(stop chan struct{}, done chan struct{}) {
	runtime.LockOSThread()
	defer close(done)

	hr, _, _ := procCoInit.Call(0, coinitMultithreaded)
	if int32(hr) == 0 {
		defer procCoUnin.Call()
	}

	// ★ 不重试：见类型注释。一次会话到头，stop/断流/出错都收尾。
	m.session(stop)
}

func (m *MicRecorder) session(stop chan struct{}) {
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
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	// ---- 打开默认**渲染**端点（耳机录音=录输出端点回环） ----
	var enumerator unsafe.Pointer
	hr, _, _ := procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enumerator)))
	if hresBad(hr) || enumerator == nil {
		m.fail("无法创建音频设备枚举器")
		return
	}
	defer comRelease(enumerator)

	hr = comCall(enumerator, vtblIMMDeviceGetDefaultEndpoint,
		eRender, eConsole, uintptr(unsafe.Pointer(&dev)))
	if hresBad(hr) || dev == nil {
		m.fail("找不到默认输出设备（耳机没插好？）")
		return
	}

	hr = comCall(dev, vtblIMMDeviceActivate,
		uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&client)))
	if hresBad(hr) || client == nil {
		m.fail("无法激活 IAudioClient")
		return
	}

	// loopback 采集与渲染端共享混音格式（float32 多声道），自选格式
	// 会拿 AUDCLNT_E_UNSUPPORTED_FORMAT（与 loopback 同因）。
	hr = comCall(client, vtblAudioClientGetMixFormat, uintptr(unsafe.Pointer(&mixPtr)))
	if hresBad(hr) || mixPtr == nil {
		m.fail("无法读取输出混音格式")
		return
	}
	wf := (*waveFormatEx)(mixPtr)
	rate, srcCh, bits := int(wf.SamplesPerSec), int(wf.Channels), int(wf.BitsPerSample)
	tag, blockAlign := int(wf.FormatTag), int(wf.BlockAlign)
	if tag == 0xFFFE {
		tag = int((*waveFormatExtensible)(mixPtr).SubFormat.Data1)
	}
	if rate <= 0 || srcCh <= 0 || blockAlign <= 0 {
		m.fail("输出混音格式异常")
		return
	}
	// 混合格式的 SampleType 只有 float32（见 loopback 的口径）；pcm16
	// 等分支保留以防驱动端点声明非常规格式。
	wavCh := 2 // 输出恒立体声：取 FL/FR（单声道源则双写）

	// ---- 选目标 WAV 文件：音乐\iKF Audio Center\ ----
	dir, err := recDir()
	if err != nil {
		m.fail("无法创建录音目录：" + err.Error())
		return
	}
	path := filepath.Join(dir, "iKF录音_"+time.Now().Format("20060102_150405")+".wav")
	out, err := os.Create(path)
	if err != nil {
		m.fail("无法创建录音文件：" + err.Error())
		return
	}
	defer out.Close()
	out.Write(wavHeaderBytes(rate, wavCh, 0)) // 数据长度先占位，收尾补写

	m.mu.Lock()
	m.rate, m.ch = rate, wavCh
	m.srcTag = srcKind(tag, bits)
	m.devName = immDeviceName(dev)
	m.file = path
	m.startAt = time.Now()
	m.mu.Unlock()

	const bufDuration = 200 * 10000
	hr = comCall(client, vtblAudioClientInitialize,
		audclntShareShared, audclntLoopback, bufDuration, 0, uintptr(mixPtr), 0)
	if hresBad(hr) {
		m.fail("IAudioClient::Initialize(loopback) 失败，HRESULT=0x" + hex32(hr))
		return
	}
	hr = comCall(client, vtblAudioClientGetService,
		uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capture)))
	if hresBad(hr) || capture == nil {
		m.fail("无法获取 IAudioCaptureClient")
		return
	}
	hr = comCall(client, vtblAudioClientStart)
	if hresBad(hr) {
		m.fail("IAudioClient::Start 失败，HRESULT=0x" + hex32(hr))
		return
	}

	// ---- 采集循环：与 loopback 相同的 5ms 轮询（可靠性优先） ----
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			m.finalize(out, rate, wavCh)
			return
		case <-ticker.C:
		}

		broken := false
		for n := 0; n < 64; n++ {
			var packet uint32
			if hresBad(comCall(capture, vtblCaptureGetNextPacketSize,
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
			if hresBad(comCall(capture, vtblCaptureGetBuffer,
				uintptr(unsafe.Pointer(&pData)),
				uintptr(unsafe.Pointer(&frames)),
				uintptr(unsafe.Pointer(&flags)),
				uintptr(unsafe.Pointer(&devPos)),
				uintptr(unsafe.Pointer(&qpc)))) {
				broken = true
				break
			}
			if frames > 0 {
				if flags&audclntSilent != 0 || pData == nil {
					m.writeSilence(out, int(frames))
				} else {
					m.writeFrames(out, pData, int(frames), srcCh, blockAlign, int(tag), bits)
				}
			}
			comCall(capture, vtblCaptureReleaseBuffer, uintptr(frames))
		}

		m.mu.Lock()
		m.seconds = time.Since(m.startAt).Seconds()
		m.mu.Unlock()

		if broken {
			m.finalize(out, rate, wavCh)
			m.fail("输出端数据流已中断（设备被拔出？），已保存已录部分")
			return
		}
	}
}

func (m *MicRecorder) fail(why string) {
	m.mu.Lock()
	m.errMsg = why
	m.mu.Unlock()
}

// finalize 补写 WAV 头的 RIFF/data 长度字段。size 与 dataLength 按
// 「当前文件偏移 − 44」实算，不信任记账值。
func (m *MicRecorder) finalize(out *os.File, rate, ch int) {
	off, err := out.Seek(0, 1 /*FILE_CURRENT*/)
	if err != nil {
		return
	}
	dataLen := uint32(off - 44)
	out.WriteAt(wavHeaderBytes(rate, ch, dataLen), 0)
	m.mu.Lock()
	m.size = int64(dataLen)
	if m.rate > 0 && m.ch > 0 {
		m.seconds = float64(dataLen) / float64(rate*ch*2)
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------- WAV

// srcKind 与 loopback 的 kind 判定同款（诊断字段用）。
func srcKind(tag, bits int) string {
	switch {
	case tag == 3 && bits == 32:
		return "float32"
	case tag == 1 && bits == 16:
		return "pcm16"
	case tag == 1 && bits == 32:
		return "pcm32"
	case tag == 1 && bits == 24:
		return "pcm24"
	}
	return "unsupported"
}

// wavHeaderBytes 组 44 字节标准 WAV 头（PCM16）。
func wavHeaderBytes(rate, ch int, dataLen uint32) []byte {
	byteRate := uint32(rate * ch * 2)
	blockAlign := uint16(ch * 2)
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+dataLen)
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // fmt 块长
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], byteRate)
	binary.LittleEndian.PutUint16(h[32:], blockAlign)
	binary.LittleEndian.PutUint16(h[34:], 16) // bits
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:], dataLen)
	return h
}

// writeFrames 把一包交错的**多声道源**数据下混为 16bit 立体声追加到
// WAV：取 FL/FR（声道 0/1）；单声道源双写。渲染混音格式通常
// float32 8ch（本机实测），输出 8ch WAV 兼容性差，统一下混到 2ch。
func (m *MicRecorder) writeFrames(out *os.File, pData unsafe.Pointer, frames, srcCh, blockAlign, tag, bits int) {
	raw := unsafe.Slice((*byte)(pData), frames*blockAlign)
	pcm := make([]byte, 0, frames*2*2)
	appendI16 := func(v int) {
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		pcm = append(pcm, byte(v), byte(v>>8))
	}
	// sample(f,c) 把第 f 帧、第 c 声道的源采样归一为 [-1,1] float64
	switch {
	case tag == 3 && bits == 32: // IEEE float32
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			l := math.Float32frombits(binary.LittleEndian.Uint32(raw[base:]))
			var r float32
			if srcCh >= 2 {
				r = math.Float32frombits(binary.LittleEndian.Uint32(raw[base+4:]))
			} else {
				r = l
			}
			appendI16(int(l * 32767))
			appendI16(int(r * 32767))
		}
	case tag == 1 && bits == 16:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			l := int16(binary.LittleEndian.Uint16(raw[base:]))
			var r int16
			if srcCh >= 2 {
				r = int16(binary.LittleEndian.Uint16(raw[base+2:]))
			} else {
				r = l
			}
			appendI16(int(l))
			appendI16(int(r))
		}
	case tag == 1 && bits == 32:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			l := int(int32(binary.LittleEndian.Uint32(raw[base:])))
			var r int
			if srcCh >= 2 {
				r = int(int32(binary.LittleEndian.Uint32(raw[base+4:])))
			} else {
				r = l
			}
			appendI16(l >> 16)
			appendI16(r >> 16)
		}
	case tag == 1 && bits == 24:
		for f := 0; f < frames; f++ {
			base := f * blockAlign
			l24 := int32(raw[base]) | int32(raw[base+1])<<8 | int32(raw[base+2])<<16
			if l24&0x800000 != 0 {
				l24 |= ^0xFFFFFF
			}
			r24 := l24
			if srcCh >= 2 {
				r24 = int32(raw[base+3]) | int32(raw[base+4])<<8 | int32(raw[base+5])<<16
				if r24&0x800000 != 0 {
					r24 |= ^0xFFFFFF
				}
			}
			appendI16(int(l24 >> 8))
			appendI16(int(r24 >> 8))
		}
	default: // 未知格式：写静音而不是乱码
		m.writeSilence(out, frames)
		return
	}
	if len(pcm) == 0 {
		return
	}
	out.Write(pcm)
	m.mu.Lock()
	m.size += int64(len(pcm))
	m.frames += uint64(frames)
	m.mu.Unlock()
}

func (m *MicRecorder) writeSilence(out *os.File, frames int) {
	z := make([]byte, frames*2*2)
	out.Write(z)
	m.mu.Lock()
	m.size += int64(len(z))
	m.mu.Unlock()
}

// ---------------------------------------------------------------- 路径

// recDir 解析「音乐」目录（可被 OneDrive 重定向，所以先查 User Shell
// Folders 的 My Music），在其下建「iKF Audio Center」子目录。
// 失败回退 %USERPROFILE%\Music。
func recDir() (string, error) {
	music := ""
	if k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`,
		registry.QUERY_VALUE); err == nil {
		defer k.Close()
		if s, _, err := k.GetStringValue("My Music"); err == nil && s != "" {
			if e, err := registry.ExpandString(s); err == nil {
				music = e
			}
		}
	}
	if music == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		music = filepath.Join(home, "Music")
	}
	dir := filepath.Join(music, "iKF Audio Center")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
