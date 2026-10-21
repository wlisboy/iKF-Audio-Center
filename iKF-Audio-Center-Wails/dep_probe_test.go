package main

import (
	"testing"
)

// 依赖关系探针：先开 Enable_ 开关，再看级别/滑杆参数是否变得可写。
func TestDepProbe(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用")
	}
	defer d.Close()

	cases := []struct {
		sw   string
		dep  string
		val  string
	}{
		{"AUDIOBRILLIANT_LEVEL", "Enable_AUDIOBRILLIANT_LFX", "3"},
		{"BM_Slider_BassLevel", "Enable_BM_GFX", "5"},
		{"VOICECLARITY_LEVEL", "Enable_VOICECLARITY_LFX", "3"},
		{"CONLAB_HPSPK_MODE", "Enable_CONLAB_HPSPK_LFX", "1"},
		{"DHDVS_USEDH", "Enable_DHDVS_LFX", "1"},
		{"Speaker_Channel_Setting", "Enable_Channel_COPY_LFX", "1"},
	}
	for _, c := range cases {
		r0, _ := d.ReadEp(EpRender, c.dep)
		res0 := d.WriteEp(EpRender, c.dep, "1")
		ok0, _ := res0["ok"].(bool)
		r1, _ := d.ReadEp(EpRender, c.sw)
		res := d.WriteEp(EpRender, c.sw, c.val)
		okr, _ := res["ok"].(bool)
		r2, v2 := d.ReadEp(EpRender, c.sw)
		t.Logf("%-28s [开关 %s pre=%d ok=%v] 级别 pre=%d write=%v post=%d,%q",
			c.sw, c.dep, r0, ok0, r1, okr, r2, v2)
		// 还原开关为关，避免污染用户状态
		d.WriteEp(EpRender, c.dep, "0")
	}
}
