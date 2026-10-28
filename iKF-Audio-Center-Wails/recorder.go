package main

// recorder.go —— 耳机录音 —— P1 清退后的纯转发层
//
// 演进：官方捆绑的 SoundRecorder\Recorder.exe 是依附官方面板 COM 环境的
// 薄客户端，脱离官方面板单独拉起必然静默退出（恒退 241，取证见
// 工具/录音机取证-Recorder.exe死件.md）。内嵌实现录渲染端点回环
// （WASAPI loopback，耳机里正在播放的声音）。
//
// 采集/下混/写盘/finalize 的实现住在 audiocore/src/recorder.rs；路径策略
// （recDir：注册表 My Music → iKF Audio Center 子目录，吃 OneDrive 重定向）
// 留在 Go——DLL 不碰注册表与用户目录，WAV 全路径由 startRust 传入。
// 写 16bit 立体声 WAV。API 形状不变：POST /api/recorder/start|stop +
// GET /api/recorder/status；与 Python 线 center_src.py 逐字段对齐。

import (
	"runtime"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// MicRecorder 一路耳机录音会话（实现在 Rust）。
//
// 录音是用户在场操作：断流后**不自动重试**（自动换文件续录会让一段录音
// 裂成多份），Rust 侧中断即 finalize 保存已录部分并置错误。
type MicRecorder struct {
	mu      sync.Mutex
	running bool
	errMsg  string
	rate    int
	ch      int    // WAV 声道数（恒 2：立体声输出）
	srcTag  string // 源编码（float32/…），诊断用
	devName string // 实际打开的端点友好名（默认输出设备）

	file    string // 最近一次（或正在进行的）WAV 全路径
	seconds float64
	size    int64

	startAt time.Time

	// rustH != 0 = Rust 会话句柄。
	rustH uintptr
}

func NewMicRecorder() *MicRecorder { return &MicRecorder{} }

var rec = NewMicRecorder()

// Start 开始录音。已在录则幂等返回 ok。
func (m *MicRecorder) Start() map[string]interface{} {
	return m.startRust()
}

// Stop 停止录音并等采集线程把 WAV 收尾。未在录时返回最近一次的结果。
func (m *MicRecorder) Stop() map[string]interface{} {
	return m.stopRust()
}

// Status 供 /api/recorder/status。
func (m *MicRecorder) Status() map[string]interface{} {
	return m.statusRust()
}

// ---------------------------------------------------------------- 路径策略

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

// ---------------------------------------------------------------- Rust 转发

// startRust 打开设备的 COM 调用必须在**无 COM 历史的 MTA 线程**上做：
// 面板主线程是 STA（webview2 要求），STA 上创建的对象移交给 Rust 采集
// 线程（MTA）= 编组违规，会话静默失败（清退首版真机踩坑）。所以把
// startRustInner 包进锁定 OS 线程的干净 goroutine——与原 Go worker 的
// 线程模型（专用线程 + 自行 CoInitializeEx）语义一致。
func (m *MicRecorder) startRust() map[string]interface{} {
	ch := make(chan map[string]interface{}, 1)
	go func() {
		runtime.LockOSThread()
		ch <- m.startRustInner()
	}()
	return <-ch
}

func (m *MicRecorder) startRustInner() map[string]interface{} {
	if !audioReady() {
		return map[string]interface{}{"ok": false,
			"error": "ikf_audio.dll 不可用（音频热路未就绪），无法开始录音"}
	}
	m.mu.Lock()
	if m.rustH != 0 {
		st, _ := recStatusViaRust(m.rustH)
		file := cstr(st.file[:])
		m.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"file": file, "rate": int(st.rate), "ch": int(st.ch), "dev": cstr(st.dev[:])}
	}
	m.errMsg = ""
	m.mu.Unlock()

	dir, err := recDir()
	if err != nil {
		return map[string]interface{}{"ok": false, "error": "无法创建录音目录：" + err.Error()}
	}
	path := filepath.Join(dir, "iKF录音_"+time.Now().Format("20060102_150405")+".wav")
	w16, err := syscall.UTF16FromString(path)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": "录音路径无法编码"}
	}

	var errBuf [256]byte
	h, _, _ := audioDLL.NewProc("ikf_recorder_open").Call(
		uintptr(unsafe.Pointer(&w16[0])),
		uintptr(unsafe.Pointer(&errBuf[0])), uintptr(len(errBuf)))
	if h == 0 {
		return map[string]interface{}{"ok": false, "error": cstr(errBuf[:])}
	}
	// 与 Go 路径语义对齐：等 rate>0（设备开好）
	deadline := time.Now().Add(3 * time.Second)
	var st recStatusC
	for {
		st, _ = recStatusViaRust(h)
		if st.rate > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.rate == 0 {
		audioDLL.NewProc("ikf_recorder_close").Call(h)
		return map[string]interface{}{"ok": false, "error": cstr(st.err[:])}
	}
	m.mu.Lock()
	m.rustH = h
	m.running = true
	m.file = cstr(st.file[:])
	m.rate, m.ch = int(st.rate), int(st.ch)
	m.devName = cstr(st.dev[:])
	m.srcTag = cstr(st.kind[:])
	m.startAt = time.Now()
	m.mu.Unlock()
	return map[string]interface{}{"ok": true, "file": m.file,
		"rate": m.rate, "ch": m.ch, "dev": m.devName}
}

func (m *MicRecorder) stopRust() map[string]interface{} {
	m.mu.Lock()
	h := m.rustH
	m.rustH = 0
	m.running = false
	m.mu.Unlock()
	if h != 0 {
		// close 内部 join 线程（含 finalize 补头）
		audioDLL.NewProc("ikf_recorder_close").Call(h)
	}
	m.mu.Lock()
	file, errMsg := m.file, m.errMsg
	seconds := 0.0
	if !m.startAt.IsZero() {
		seconds = time.Since(m.startAt).Seconds()
	}
	m.mu.Unlock()
	// size 以文件实况为准（DLL 侧 finalize 实算，记账可能略偏）
	size := int64(0)
	if f, err := os.Stat(file); err == nil {
		size = f.Size() - 44
	}
	out := map[string]interface{}{"ok": true, "file": file,
		"seconds": seconds, "size": size}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

func (m *MicRecorder) statusRust() map[string]interface{} {
	// ★破坏性检测 P0-3：DLL 调用与字段读取全程持锁（同 loopback）。
	m.mu.Lock()
	defer m.mu.Unlock()
	h, running := m.rustH, m.running
	seconds := 0.0
	if running && !m.startAt.IsZero() {
		seconds = time.Since(m.startAt).Seconds()
	}
	out := map[string]interface{}{
		"ok": true, "on": running, "recording": running,
		"file": m.file, "rate": m.rate, "ch": m.ch,
		"dev": m.devName, "src": m.srcTag,
		"size": m.size, "seconds": seconds,
	}
	if h == 0 {
		return out
	}
	st, ok := recStatusViaRust(h)
	if !ok {
		return out
	}
	out["on"] = st.running != 0
	out["recording"] = st.running != 0 // ★与 Python 线字段名对齐：前端 tick()
	// 按 st.recording 判断（字段缺失=灵动岛计时恒 00:00，2026-10-24 真机）
	out["rate"] = int(st.rate)
	out["ch"] = int(st.ch)
	out["frames"] = st.frames
	out["size"] = st.size
	out["dev"] = cstr(st.dev[:])
	out["file"] = cstr(st.file[:])
	out["src"] = cstr(st.kind[:])
	if e := cstr(st.err[:]); e != "" {
		out["error"] = e
	}
	return out
}
