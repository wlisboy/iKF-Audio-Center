package main

// listen.go —— 内嵌麦克风监听 —— P1 清退后的纯转发层
//
// 链路（实现在 audiocore/src/listen.rs）：默认麦克风（capture，自身混音
// 格式）→ 软件上混（样本写 FL/FR）→ 默认渲染端点（render，自身混音格式）。
//
// 为什么不走官方路径（drv.Listen → osConfLib.RunListen2Device）：
//   反编译实锤（工具/监听取证-监听COM组件缺失.md）：RunListen2Device
//   内部 CoCreateInstance(CLSID {E642D042-…})，该 COM 服务器在整套 WHQL
//   驱动包、System32、注册表里都不存在——官方原版面板同样开不了。
// ★ 转换必须自己做：WASAPI 引擎的 AUTOCONVERTPCM 无论放采集侧还是渲染
//   侧都会产出静音（2026-10-20 排障实测，教训已继承进 Rust 实现）。
//
// 与 Python 线 center_src.py 的 MicListen 逐字段对齐。

import (
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// MicListen 一路麦克风监听会话（实现在 Rust）。
type MicListen struct {
	mu      sync.Mutex
	running bool
	errMsg  string
	devName string // 采集端点（麦克风）友好名
	outName string // 渲染端点（输出）友好名
	rate    int
	ch      int

	// sent/pkts/silentPkts/capPeak：statusRust 从 DLL 实时拉取覆盖；
	// 这里保留字段只为 Status() 的键集合在句柄为 0 时也完整。
	sent       uint64
	pkts       uint64
	silentPkts uint64
	capPeak    float32

	// rustH != 0 = Rust 会话句柄。
	rustH uintptr
}

func NewMicListen() *MicListen { return &MicListen{} }

var lis = NewMicListen()

// Start 开始监听。已在听则幂等返回 ok。
func (l *MicListen) Start() map[string]interface{} {
	return l.startRust()
}

// Stop 停止监听。未在听时幂等返回 ok。
func (l *MicListen) Stop() map[string]interface{} {
	return l.stopRust()
}

// Status 供诊断与验收断言（sent 必须在监听中持续增长）。
func (l *MicListen) Status() map[string]interface{} {
	return l.statusRust()
}

// SetGain 设置监听增益（1.0=原始电平，上限 8）。实时生效，未监听时缓存。
func (l *MicListen) SetGain(g float64) map[string]interface{} {
	if g < 1 {
		g = 1
	} else if g > 8 {
		g = 8
	}
	l.mu.Lock()
	h := l.rustH
	l.mu.Unlock()
	if h == 0 || !audioProbe() {
		return map[string]interface{}{"ok": false, "error": "监听未开启"}
	}
	// ★f32 必须按位传：uintptr(float32) 是值截断（3.0→3），Rust 端
	// from_bits(3)=次正规数≈0 → 增益归零、监听无声（2026-10-24 真机踩坑）。
	bits := math.Float32bits(float32(g))
	r, _, _ := audioDLL.NewProc("ikf_listen_set_gain").Call(h, uintptr(bits))
	return map[string]interface{}{"ok": r == 0, "gain": g}
}

// ---------------------------------------------------------------- Rust 转发

// startRust 打开设备的 COM 调用必须在**无 COM 历史的 MTA 线程**上做：
// 面板主线程是 STA（webview2 要求），STA 上创建的对象移交给 Rust 采集
// 线程（MTA）= 编组违规，会话静默失败（清退首版真机踩坑）。所以把
// startRustInner 包进锁定 OS 线程的干净 goroutine——与原 Go worker 的
// 线程模型（专用线程 + 自行 CoInitializeEx）语义一致。
func (l *MicListen) startRust() map[string]interface{} {
	ch := make(chan map[string]interface{}, 1)
	go func() {
		runtime.LockOSThread()
		ch <- l.startRustInner()
	}()
	return <-ch
}

func (l *MicListen) startRustInner() map[string]interface{} {
	if !audioReady() {
		return map[string]interface{}{"ok": false,
			"error": "ikf_audio.dll 不可用（音频热路未就绪），无法打开监听"}
	}
	l.mu.Lock()
	if l.rustH != 0 {
		st, _ := lisStatusViaRust(l.rustH)
		l.mu.Unlock()
		return map[string]interface{}{"ok": true, "already": true,
			"dev": cstr(st.dev[:]), "out": cstr(st.out[:]),
			"rate": int(st.rate), "ch": int(st.ch)}
	}
	l.errMsg = ""
	l.mu.Unlock()

	var errBuf [256]byte
	h, _, _ := audioDLL.NewProc("ikf_listen_open").Call(
		uintptr(unsafe.Pointer(&errBuf[0])), uintptr(len(errBuf)))
	if h == 0 {
		return map[string]interface{}{"ok": false, "error": cstr(errBuf[:])}
	}
	// 与原 Go 路径语义对齐：等线程填好 devName 再返回（最多 3 秒）
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ := lisStatusViaRust(h)
		if cstr(st.dev[:]) != "" {
			l.mu.Lock()
			l.rustH = h
			l.running = true
			l.devName = cstr(st.dev[:])
			l.outName = cstr(st.out[:])
			l.rate, l.ch = int(st.rate), int(st.ch)
			l.mu.Unlock()
			return map[string]interface{}{"ok": true,
				"dev": l.devName, "out": l.outName, "rate": l.rate, "ch": l.ch}
		}
		if time.Now().After(deadline) {
			// 超时也返回成功（会话已开），名字后到
			st, _ := lisStatusViaRust(h)
			l.mu.Lock()
			l.rustH = h
			l.running = true
			l.rate, l.ch = int(st.rate), int(st.ch)
			l.mu.Unlock()
			return map[string]interface{}{"ok": true,
				"dev": "", "out": cstr(st.out[:]), "rate": l.rate, "ch": l.ch}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *MicListen) stopRust() map[string]interface{} {
	l.mu.Lock()
	h := l.rustH
	l.rustH = 0
	l.running = false
	errMsg := l.errMsg
	l.mu.Unlock()
	if h != 0 {
		audioDLL.NewProc("ikf_listen_close").Call(h)
	}
	out := map[string]interface{}{"ok": true}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

func (l *MicListen) statusRust() map[string]interface{} {
	// ★破坏性检测 P0-3：DLL 调用与字段读取全程持锁（同 loopback）。
	l.mu.Lock()
	defer l.mu.Unlock()
	h, running := l.rustH, l.running
	out := map[string]interface{}{
		"ok": true, "on": running, "dev": l.devName, "out": l.outName,
		"rate": l.rate, "ch": l.ch, "sent": l.sent,
		"pkts": l.pkts, "silentPkts": l.silentPkts, "capPeak": l.capPeak,
	}
	if h == 0 {
		return out
	}
	st, ok := lisStatusViaRust(h)
	if !ok {
		return out
	}
	out["on"] = st.running != 0
	out["dev"] = cstr(st.dev[:])
	out["out"] = cstr(st.out[:])
	out["rate"] = int(st.rate)
	out["ch"] = int(st.ch)
	out["sent"] = st.sent
	out["pkts"] = st.pkts
	out["silentPkts"] = st.silentPkts
	out["capPeak"] = st.capPeak
	if e := cstr(st.err[:]); e != "" {
		out["error"] = e
	}
	return out
}
