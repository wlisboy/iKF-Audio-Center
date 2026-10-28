//go:build windows

package main

// audio_fft_parity_test.go —— S2 双跑对比护栏：Go synthSpectrum 与
// Rust ikf_fft_probe 对同一组输入必须输出**逐字节相同**的 512 字节频谱。
// 不一致 = 频谱链路（窗/FFT/归一化/平滑/字节映射）在某一侧漂移。
// 这是设备无关的确定性对比，跑一次几毫秒。

import (
	"bytes"
	"math"
	"testing"
	"unsafe"
)

func rustFFTBins(t *testing.T, rate int, amp float64) []byte {
	t.Helper()
	if !audioProbe() {
		t.Skip("ikf_audio.dll 不可用")
	}
	pr := audioDLL.NewProc("ikf_fft_probe")
	buf := make([]byte, lbBins)
	r, _, _ := pr.Call(uintptr(rate), uintptr(math.Float64bits(amp)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r != 0 {
		t.Fatalf("ikf_fft_probe 返回 %d", r)
	}
	return buf
}

func TestRustFFTParity(t *testing.T) {
	cases := []struct {
		name string
		amp  float64
	}{
		{"满幅0dB", 1.0}, {"-20dB", 0.1}, {"-40dB", 0.01},
		{"-60dB", 0.001}, {"-80dB", 0.0001}, {"静音", 0},
	}
	for _, c := range cases {
		want := synthSpectrum(48000, c.amp)
		got := rustFFTBins(t, 48000, c.amp)
		if !bytes.Equal(want, got) {
			// 找出差异点给可读诊断
			diff := 0
			for i := range want {
				if want[i] != got[i] {
					if diff == 0 {
						t.Logf("  首个差异 bin #%d: go=%d rust=%d", i, want[i], got[i])
					}
					diff++
				}
			}
			t.Errorf("%s: 频谱逐字节不一致（%d/%d 个 bin 差异）", c.name, diff, len(want))
		} else {
			t.Logf("%s: 512 字节一致", c.name)
		}
	}
}
