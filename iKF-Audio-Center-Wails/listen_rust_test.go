//go:build windows

package main

// listen_rust_test.go —— S3 真机验收：Rust 监听会话（麦克风→软件上混→渲染端点）。
// 判据与 Go 版 listen 闸同口径：sent 必须在监听中持续增长（渲染时钟在消费）。

import (
	"testing"
	"time"
)

func TestListenRustRealDevice(t *testing.T) {
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
	l := NewMicListen()
	out := l.startRust()
	if out["ok"] != true {
		t.Fatalf("Rust 监听打开失败: %v", out["error"])
	}
	t.Logf("dev=%v out=%v rate=%v ch=%v", out["dev"], out["out"], out["rate"], out["ch"])

	// 跑 3 秒，采样两次 status 看 sent 是否增长
	time.Sleep(3 * time.Second)
	s1 := l.statusRust()
	if s1["on"] != true {
		t.Fatalf("会话没在跑: %v", s1["error"])
	}
	time.Sleep(2 * time.Second)
	s2 := l.statusRust()

	sent1, sent2 := s1["sent"].(uint64), s2["sent"].(uint64)
	t.Logf("sent: %d → %d（增长 %d）· pkts=%v silent=%v capPeak=%v",
		sent1, sent2, sent2-sent1, s2["pkts"], s2["silentPkts"], s2["capPeak"])
	if sent2 <= sent1 {
		t.Fatalf("sent 未增长（sent=%d）—— 渲染端没在消费帧", sent1)
	}
	if pkts := s2["pkts"].(uint64); pkts == 0 {
		t.Fatal("采集包数 0 —— 麦克风采不到数据")
	}

	stop := l.stopRust()
	if stop["ok"] != true {
		t.Fatalf("stop 失败: %v", stop)
	}
	time.Sleep(200 * time.Millisecond)
	if l.statusRust()["on"] == true {
		t.Fatal("stop 后会话仍在跑")
	}
}
