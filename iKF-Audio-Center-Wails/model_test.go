//go:build windows

package main

// model_test.go —— S1/S2 数据真源锁定：装载、基线、关键真值。
// 这些断言 = 闸门的静态前哨；数据改动必须从这里开始同步。

import "testing"

func TestModelBaselines(t *testing.T) {
	if len(modelData.Scene.Exclude) != 6 || len(modelData.Scene.Unproven) != 35 {
		t.Fatalf("排除名单漂移: %d/%d", len(modelData.Scene.Exclude), len(modelData.Scene.Unproven))
	}
	if len(modelData.Factory) != 42 {
		t.Fatalf("出厂表漂移: %d", len(modelData.Factory))
	}
	if len(modelData.GroupAlias) != 10 || len(modelData.Scene.Builtin) != 6 {
		t.Fatalf("别名/内置漂移: %d/%d", len(modelData.GroupAlias), len(modelData.Scene.Builtin))
	}
	// S3 基线：名字表 208 + 白名单 80（65/15 自检口径）
	if len(modelData.Names.Generic) != 112 || len(modelData.Names.Oem) != 96 {
		t.Fatalf("名字表漂移: %d/%d", len(modelData.Names.Generic), len(modelData.Names.Oem))
	}
	if len(genericNames) != 112 || len(oemNamesList) != 96 {
		t.Fatalf("names.go 派生断链: %d/%d", len(genericNames), len(oemNamesList))
	}
	if n := writableCount(); n != 80 {
		t.Fatalf("白名单漂移: %d (应 80)", n)
	}
}

func TestWritableFromModel(t *testing.T) {
	// 自检口径：render 65 / capture 15
	rc := map[string]int{"render": 0, "capture": 0}
	for ep, m := range writable {
		rc[ep] += len(m)
	}
	if rc["render"] != 65 || rc["capture"] != 15 {
		t.Fatalf("白名单端点分布漂移: %v", rc)
	}
	// 关键规格抽查：kind / 值域 / 离散 / 不钳制
	sp, ok := spec("render", "GFSControl")
	if !ok || sp.kind != TBool {
		t.Fatalf("GFSControl 登记漂移: %+v ok=%v", sp, ok)
	}
	sp, _ = spec("render", "RFX_ENVIRONMENT")
	if sp.lo != 0 || sp.hi != 27 || !sp.hasRange {
		t.Fatalf("RFX_ENVIRONMENT 值域漂移: %+v", sp)
	}
	sp, _ = spec("render", "Speaker_Channel_Setting")
	if len(sp.choices) != 4 {
		t.Fatalf("声道配置离散漂移: %+v", sp.choices)
	}
	sp, _ = spec("capture", "DevSampleRateControl")
	if len(sp.choices) != 2 {
		t.Fatalf("采样率离散漂移: %+v", sp.choices)
	}
	sp, _ = spec("render", "SS_C_ANGLE")
	if sp.lo != -180 || sp.hi != 180 {
		t.Fatalf("SS 角度规范化区间漂移: %+v", sp)
	}
	sp, _ = spec("render", "SS_C_DIST")
	if sp.hasRange {
		t.Fatalf("SS 距离不应钳制: %+v", sp)
	}
	sp, _ = spec("render", "VolumeScalarControl#0")
	if !ok || sp.kind != TFloat || !sp.hasRange {
		t.Fatalf("声道虚拟参数漂移: %+v ok=%v", sp, ok)
	}
	if _, ok := spec("render", "SpeakerShifterControl"); ok {
		t.Fatal("SpeakerShifterControl 不应登记（官方页 4 模式不是驱动属性）")
	}
}

func TestFactoryDefaultsFromModel(t *testing.T) {
	render, capture := 0, 0
	eq := map[string]string{}
	for _, it := range factoryDefaults {
		switch it.Ep {
		case EpRender:
			render++
		case EpCapture:
			capture++
		default:
			t.Fatalf("非法端点: %q", it.Ep)
		}
		if len(it.Name) > 9 && it.Name[:9] == "EQ_Slider" {
			eq[it.Name] = it.Val
		}
	}
	if render != 36 || capture != 6 {
		t.Fatalf("出厂表端点分布漂移: render=%d capture=%d", render, capture)
	}
	// EQ 十条 = 官方预设全 0 平直曲线（索引 11 反编译真值）
	for name, val := range eq {
		if val != "0" {
			t.Fatalf("EQ 预设口径漂移: %s=%s (应全 0)", name, val)
		}
	}
	if len(eq) != 10 {
		t.Fatalf("EQ 条数漂移: %d", len(eq))
	}
}

func TestProfileGroupsFromModel(t *testing.T) {
	// 关键归属抽查：音量族 / EQ 前缀键 / 合并页 / 麦克风专项
	if g := groupOf("render", "VolumeScalarControl#0"); g != "SpeakerVolume" {
		t.Fatalf("声道后缀剥除失效: %q", g)
	}
	if g := groupOf("render", "EQ_Slider3"); g != "SpeakerEQ" {
		t.Fatalf("EQ 归属漂移: %q", g)
	}
	if g := groupOf("render", "SS_LB_ANGLE"); g != "SpeakerVirtual" {
		t.Fatalf("SS 前缀规则漂移: %q", g)
	}
	if g := groupOf("render", "Enable_VOCALCANCEL_GFX"); g != "SpeakerKaraoke" {
		t.Fatalf("合并页归属漂移: %q", g)
	}
	if g := groupOf("capture", "Enable_ENC_LFX"); g != "MicInENC" {
		t.Fatalf("麦克风专项漂移: %q", g)
	}
	if groupLabel["SpeakerQuarterSetting"] != "四声道设定" || groupLabel["General"] != "其他" {
		t.Fatalf("标签漂移（含只读两组）")
	}
	if groupOrder[0] != "SpeakerVolume" || len(groupOrder) != 27 {
		t.Fatalf("展示顺序漂移: %d", len(groupOrder))
	}
	if profileEndpoint["render"][0] != "Speakers" || profileEndpoint["capture"][1] != "In" {
		t.Fatalf("端点元数据漂移")
	}
	if profileValueType["float"] != "System.Single" {
		t.Fatalf("ValueType 漂移")
	}
	if r := groupRank("capture", "MicInAEC"); r < 1000 {
		t.Fatalf("采集 rank 基数漂移: %d", r)
	}
}

func TestInitDeviceItemsFromModel(t *testing.T) {
	if len(initDeviceItems) != 1 {
		t.Fatalf("设备级漂移: %d", len(initDeviceItems))
	}
	it := initDeviceItems[0]
	if it.Ep != EpRender || it.Name != "Speaker_Channel_Setting" ||
		it.Val != "2" || it.Source != "device" || it.Scope != "device" {
		t.Fatalf("拓扑项内容漂移: %+v", it)
	}
}

func TestBuildInitManifestNoDevice(t *testing.T) {
	// d=nil：无场景档可读 ⟹ 只给出厂 42 + 设备级 1 = 43
	items := BuildInitManifest(nil)
	if len(items) != 43 {
		t.Fatalf("清单漂移: %d (应 43)", len(items))
	}
	src := map[string]int{}
	for _, it := range items {
		src[it.Source]++
	}
	if src["factory"] != 42 || src["device"] != 1 {
		t.Fatalf("来源分布漂移: %v", src)
	}
}
