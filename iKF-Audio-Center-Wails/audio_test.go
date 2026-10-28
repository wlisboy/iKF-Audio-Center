//go:build windows

package main

// audio_test.go —— P1-S1 链路锁定：DLL 在标准位置、加载、ABI 协商、自检。
// 跑之前先 `cargo build --release`（build.bat 步骤 2b 会自动做）。

import "testing"

func TestAudioProbe(t *testing.T) {
	// 开发机必跑 cargo（构建链第 2b 步），DLL 缺失 = 构建链断了
	if !audioProbe() {
		t.Fatalf("ikf_audio.dll 不可用 —— 检查 audiocore/target/release/ 是否有新构建")
	}
	if audioVersion() == "" {
		t.Fatal("加载成功但版本串为空")
	}
	t.Logf("audio: %s", audioVersion())
}

func TestAudioStatusString(t *testing.T) {
	s := audioStatus()
	if audioReady() && s[:5] != "rust:" {
		t.Fatalf("状态串口径漂移: %q", s)
	}
}
