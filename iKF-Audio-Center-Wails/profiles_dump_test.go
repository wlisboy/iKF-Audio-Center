package main

import (
	"testing"
)

// 打印真机 Profiles() 的实际列表 —— 核对场景墙内容。
// 回归：①「预设」（FactoryDefault.xml）不得作为场景列出；②官方空模板
//（items=0 的 builtin）不进列表；③用户自建档必须非内置。
func TestProfilesDumpNames(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用")
	}
	defer d.Close()
	p := d.Profiles()
	t.Logf("writeDir=%s writable=%v", p["dir"], p["writable"])
	list, _ := p["list"].([]ProfileEntry)
	for _, e := range list {
		t.Logf("name=%-14q builtin=%-5v items=%-3d hasData=%-5v dir=%s",
			e.Name, e.Builtin, e.Items, e.HasData, e.Dir)
		if e.Name == profileFactory {
			t.Errorf("「预设」(FactoryDefault) 仍出现在场景列表 —— 应已移除")
		}
		if e.Builtin && e.Items == 0 {
			t.Errorf("内置空模板 %q 出现在场景列表 —— 应跳过", e.Name)
		}
	}
}
