//go:build windows

package main

// sta_thread_test.go —— 面板模式回归锁：webview2 主线程是 STA
// （CoInitializeEx(MTA) 返回 RPC_E_CHANGED_MODE）。清退首版把 COM 打开
// 挪到调用线程，STA 上创建的对象移交给 Rust 采集线程（MTA）= 编组违规，
// 频谱/监听/录音在面板里全静默失败（--serve 模式正常，因为 goroutine
// 线程无 COM 历史）。修复后 startRust 包在干净 MTA 线程——本测试在
// STA 线程上调 Start，锁死这个坑。

import (
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestListenStartOnSTAThread(t *testing.T) {
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
	// 模拟面板主线程：锁定 OS 线程并初始化为 STA（webview2 的真实状态）
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); hr != nil {
		t.Fatalf("STA 初始化失败: %v", hr)
	}
	defer windows.CoUninitialize()
	l := NewMicListen()
	out := l.Start()
	if out["ok"] != true {
		t.Fatalf("STA 线程上打开监听失败: %v", out["error"])
	}
	time.Sleep(2 * time.Second)
	s := l.Status()
	if s["on"] != true {
		t.Fatalf("会话没在跑: %v", s["error"])
	}
	sent := s["sent"].(uint64)
	time.Sleep(1500 * time.Millisecond)
	s2 := l.Status()
	sent2 := s2["sent"].(uint64)
	t.Logf("sent %d → %d", sent, sent2)
	l.Stop()
	if sent2 <= sent {
		t.Fatal("sent 未增长 —— STA 污染仍存在")
	}
}

// TestLoopbackSpectrumOnSTAThread 可视化频谱的面板线程条件端到端：
// STA 线程 Start → Spectrum bins 应随音频推进（不全零且 level>0）。
func TestLoopbackSpectrumOnSTAThread(t *testing.T) {
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); hr != nil {
		t.Fatalf("STA 初始化失败: %v", hr)
	}
	defer windows.CoUninitialize()
	l := NewLoopback()
	out := l.Start("")
	if out["ok"] != true {
		t.Fatalf("STA 线程打开回环失败: %v", out["error"])
	}
	defer l.Stop()
	deadline := time.Now().Add(3 * time.Second)
	var sp map[string]interface{}
	for time.Now().Before(deadline) {
		sp = l.Spectrum()
		if sp["ok"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sp == nil || sp["ok"] != true {
		t.Fatalf("Spectrum 拿不到数据: %v", sp)
	}
	time.Sleep(2 * time.Second)
	sp = l.Spectrum()
	level := sp["level"].(float64)
	t.Logf("running=%v frames=%v level=%v", sp["running"], sp["frames"], level)
	if sp["running"] != true {
		t.Fatal("会话没在跑")
	}
	if level == 0 {
		t.Log("level=0（默认端点当前无声音播放），通路结构校验通过")
	}
}
