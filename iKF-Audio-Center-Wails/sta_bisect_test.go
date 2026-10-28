//go:build windows

package main

// 二分验证：同样的 loopback 流程，不初始化 STA（普通 MTA/无 COM 线程）。
import (
	"testing"
	"time"
)

func TestLoopbackSpectrumNoSTA(t *testing.T) {
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
	l := NewLoopback()
	out := l.Start("")
	if out["ok"] != true {
		t.Fatalf("打开回环失败: %v", out["error"])
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
	t.Logf("running=%v frames=%v level=%v", sp["running"], sp["frames"], sp["level"])
}
