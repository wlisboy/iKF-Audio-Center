package main

import (
	"os"
	"path/filepath"
	"testing"
)

// excludedFromScene 回归（2026-10-19）：档内存的是带通道后缀的名字
// （VolumeScalarControl#0），只按 sceneExclude 裸名精确匹配会漏 ——
// 电影档 63 项里混着 #0/#1、apply 照写、hasExcludedItem 也查不出来。
// 口径：裸名命中名单 或 名字含 "#"（多通道虚拟参数）都算排除。
func TestExcludedFromScene(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"VolumeControl", true},
		{"VolumeScalarControl", true},
		{"MuteControl", true},
		{"AAVolumeControl", true},
		{"DevSampleRateControl", true},
		{"Speaker_Channel_Setting", true}, // 设备级声道拓扑，播种实读 5.1 曾把设备页切回 5.1
		// APO 未实现家族（2026-10-19 用户拍板：场景只收确认生效的）
		{"Enable_SS_LFX", true},          // 虚拟环绕（四次 A/B 均噪声底）
		{"SS_LB_ANGLE", true},            // SS 家族子参数随总开关一起排除
		{"SS_LFE_DIST", true},
		{"Enable_DHDVS_LFX", true},       // Dolby 家族
		{"DHDVS_USEDH", true},
		{"Enable_BM_GFX", true},          // 动态低音（BassManagement 零命中）
		{"Enable_SURRSENS_LFX", true},    // 环绕增强：模块有实体但 A/B 未确认生效
		{"Enable_VOICECLARITY_LFX", true},// 语音清晰度（双证据未实现）
		// ★ 2026-10-19 深夜修正：升降调/人声消除是真模块（CKeyShifterLib/
		// CVocalCancellationLib 有 vftable+构造体，K 档 GFX bit13/14），
		// 之前依据「空壳」笔误被排除——恢复场景收录
		{"Enable_KEYSHIFT_GFX", false},
		{"KEYSHIFT_LEVEL", false},
		{"Enable_VOCALCANCEL_GFX", false},
		{"VOCALCANCEL_LEVEL", false},
		// 真实现 —— 必须留在场景
		{"Enable_RFX_GFX", false},        // CReverbLib 真建模块
		{"RFX_ENVIRONMENT", false},
		{"Enable_CONLAB_HPSPK_LFX", false}, // A/B 测到 19.9（噪声底两倍）
		{"CONLAB_HPSPK_MODE", false},
		{"Enable_EQ_GFX", false},         // EQ 官方核心
		{"Enable_MAGICVOICE", false},     // CMagicVoiceLib 真建
		{"Enable_MICARRAY_LFX", false},   // CBeamFormingLib 真建
		{"Enable_SMART_VOLUME_GFX", false}, // 未测过，按规矩不判死
		{"Enable_SwapLfeCn_LFX", false},
		{"VolumeScalarControl#0", true}, // 带通道后缀 —— 本次修的就是漏掉它
		{"VolumeScalarControl#1", true},
		{"VolumeControl#3", true},
		{"MuteControl#1", true},
		{"EQ_Slider1", false},
		{"Enable_EQ_GFX", false},
		{"NoiseReduction_Level", false},
	}
	for _, c := range cases {
		if got := excludedFromScene(c.name); got != c.want {
			t.Errorf("excludedFromScene(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// builtin 判定回归（2026-10-19）：CPL\profile 目录整棵被标 builtinDir 时，
// 只有内置名单内的名字才是内置档 —— 用户自建场景同名目录里必须是非内置。
func TestBuiltinMark(t *testing.T) {
	cases := []struct {
		dirBuiltin bool
		stem       string
		want       bool
	}{
		{true, "默认", true}, // 2026-10-20 改名（原「原厂默认」）
		{true, "游戏", true},
		{true, "音乐", true},
		{true, "电影", true},
		{true, "UserDefine", true},
		{true, "UserDefineFormwork", true},
		{true, "HiFi", true},
		{true, "我的场景", false},   // 便携包 CPL\profile 下的用户自建
		{true, "UserDefine2", false}, // 名字接近但不在名单
		{false, "默认", false},  // 用户目录（AppData）里同名 → 用户优先
		{false, "我的场景", false},
	}
	for _, c := range cases {
		if got := builtinMark(c.dirBuiltin, c.stem); got != c.want {
			t.Errorf("builtinMark(%v, %q) = %v, want %v",
				c.dirBuiltin, c.stem, got, c.want)
		}
	}
}

// 集成：真机驱动就绪时，在 dist 的 CPL/profile 放一个用户自建档，
// Profiles() 必须把它标成非内置（出现在场景墙），且官方档仍是内置。
// 驱动不可用（CI/无设备）时跳过。
func TestProfilesBuiltinOnDevice(t *testing.T) {
	d := NewDriver()
	if !d.Open() {
		t.Skip("驱动不可用，跳过真机集成断言")
	}
	defer d.Close()

	wdir := profileWriteDir(d)
	stem := "ZZZ_TestUserScene"
	os.WriteFile(filepath.Join(wdir, stem+".cml"), []byte("<Scene/>"), 0o644)
	os.WriteFile(filepath.Join(wdir, stem+".profile"), []byte("<Profile/>"), 0o644)
	defer func() {
		os.Remove(filepath.Join(wdir, stem+".cml"))
		os.Remove(filepath.Join(wdir, stem+".profile"))
	}()

	p := d.Profiles()
	list, _ := p["list"].([]ProfileEntry)
	if len(list) == 0 {
		t.Fatal("场景列表为空")
	}
	seenUser := false
	for _, e := range list {
		if e.Name == stem {
			seenUser = true
			if e.Builtin {
				t.Errorf("用户自建场景 %q 被标成内置（会被场景墙滤掉）", stem)
			}
		}
	}
	if !seenUser {
		t.Errorf(" Profiles() 没列出用户自建场景 %q", stem)
	}
	// 播种档若在本机可见（测试进程的 CPL 解析目标不一定是便携包），
	// 必须标内置；测试环境可能根本没播种档，所以是条件断言。
	// 例外：CPL 目录不可写时 profileWriteDir 回退 AppData（dirBuiltin=false），
	// 播种档落在那里 builtin 本来就是 false（环境固有行为，非名单失效）——
	// 只对「CPL 自带标记目录」下的档断言。
	for _, e := range list {
		if e.Dir == appdataProfileDir() {
			continue
		}
		if builtinScenes[e.Name] && e.Items > 0 && !e.Builtin {
			t.Errorf("播种档 %q 未标内置（会被场景墙当用户档）", e.Name)
		}
	}
}
