package main

import "testing"

func TestSpkChannelProbe(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用")
	}
	defer d.Close()
	for _, v := range []string{"2", "8", "6"} {
		res := d.WriteEp(EpRender, "Speaker_Channel_Setting", v)
		okr, _ := res["ok"].(bool)
		r, vv := d.ReadEp(EpRender, "Speaker_Channel_Setting")
		t.Logf("Speaker_Channel_Setting=%s write=%v post=%d,%q", v, okr, r, vv)
	}
}
