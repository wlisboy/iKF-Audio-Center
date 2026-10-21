package main

import (
	"sort"
	"testing"
)

// 全量可写性真机探测：白名单逐项「读 → 写当前值 → 读回」。
// 写当前值无扰动；wret!=0 的项 = 界面做成可交互但驱动实际不收。
func TestWritableProbe(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用")
	}
	defer d.Close()

	bad := 0
	for _, ep := range []string{EpRender, EpCapture} {
		names := make([]string, 0, len(writable[ep]))
		for n := range writable[ep] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if sceneExclude[n] {
				continue
			}
			r0, v0 := d.ReadEp(ep, n)
			if r0 != 0 || v0 == "" {
				// 无值项（ret=2 从未写过）不参与「写当前值」探测 ——
				// 空串会被值域校验拒绝，不代表参数不可写；
				// 其可写性由 apply 两轮写（开关前置）语义保证。
				continue
			}
			res := d.WriteEp(ep, n, v0)
			okr, _ := res["ok"].(bool)
			r1, v1 := d.ReadEp(ep, n)
			if !okr || r1 != 0 || v1 != v0 {
				bad++
				t.Logf("✗ %-8s %-30s read0=%d write_ok=%v read1=%d (v=%q->%q)",
					ep[:3], n, r0, okr, r1, v0, v1)
			}
		}
	}
	t.Logf("探测完成：异常 %d 项", bad)
}
