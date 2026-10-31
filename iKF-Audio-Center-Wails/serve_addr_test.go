//go:build windows

package main

// serveAddrLoopback 的表驱动测试（健壮性审计 #1 的回归护栏）：
// 默认/通配写法必须收口到回环；显式回环原样；显式非回环放行（警告由日志承担）。
import "testing"

func TestServeAddrLoopback(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{":18500", "127.0.0.1:18500"},
		{"0.0.0.0:18500", "127.0.0.1:18500"},
		{"18500", "127.0.0.1:18500"},
		{"127.0.0.1:18500", "127.0.0.1:18500"},
		{"localhost:8899", "localhost:8899"},
		{"[::1]:8899", "[::1]:8899"},
		{"192.168.1.5:18500", "192.168.1.5:18500"}, // 显式指定 = 明知故犯，放行
	}
	for _, c := range cases {
		if got := serveAddrLoopback(c.in); got != c.want {
			t.Errorf("serveAddrLoopback(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
