package main

import (
	"testing"
)

// 写→读 微探针：定位「应用场景后部分开关不同步」的层次。
func TestApplyProbe(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用")
	}
	defer d.Close()

	probe := []struct {
		ep   string
		name string
		val  string
	}{
		{EpCapture, "Enable_NOISEREDUCTION_LFX", "1"},
		{EpCapture, "NoiseReduction_Level", "14"},
		{EpCapture, "AGCControl", "1"},
		{EpRender, "Enable_EQ_GFX", "1"},
		{EpRender, "VolumeControl", "8"},
		{EpRender, "VolumeScalarControl", "0.7"},
		{EpRender, "Enable_AUDIOBRILLIANT_LFX", "1"},
	}
	for _, p := range probe {
		r0, v0 := d.ReadEp(p.ep, p.name)
		res := d.WriteEp(p.ep, p.name, p.val)
		okr, _ := res["ok"].(bool)
		r1, v1 := d.ReadEp(p.ep, p.name)
		t.Logf("%-8s %-28s write=%-5v pre(r=%d,v=%q) post(r=%d,v=%q) want=%q",
			p.ep[:3], p.name, okr, r0, v0, r1, v1, p.val)
	}
}
