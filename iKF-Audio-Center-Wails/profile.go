//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 组态（特效组态）—— 完整复刻官方的 Profile 系统。
//
// 官方模型（实测 CPL 目录，非推测）：
//
//	CPL/profile/<名称>.cml       一个 RadioButton，GroupName="ProFileMenu"，
//	                             Tooltip 存描述，Image.Check.Normal 存图标
//	CPL/profile/<名称>.profile   数据文件：
//	                             <Profile Name="…">
//	                               <Endpoint Name="Speakers" DataFlow="Out">
//	                                 <ApoEffect Name="SpeakerEQ">
//	                                   <Property Name="EQ_Slider1" Value="6"
//	                                             ValueType="System.Int32"
//	                                             Args="-1" ArgsType="System.Int32"/>
//	CPL/profile/default/*        驱动自带的内置组态（本机只有 HiFi，体为空 =
//	                             全部走 APO 默认，即「HiFi 模式」= 不叠加音效）
//	CPL/profile/UserDefine.cml   当前选中的自定义档标记
//	CPL/profile/UserDefineFormwork.cml   新增组态用的模板（Tooltip="Description"）
//	CPL/FactoryDefault.xml       原厂默认，官方 UI 的「预设」档
//
// 关键点：<Property Name> 用的就是 osConfLib.dll 的 208 项属性名，
// 所以这套格式可以直接落在 PropertyControl 上，无需额外映射表。
//
// CPL 位于 Program Files 下**不可写**（实测 Permission denied），
// 因此用户自建组态写入 %APPDATA%\iKF Audio Center\profile\；
// 读取时两处都扫，驱动自带的内置档照常列出。
//
// 本文件与 Python 版（tools/src/center_src.py 的「组态」一节）逐字段一致。

const (
	profileFactory  = "预设" // 对应 FactoryDefault.xml
	profileTemplate = "UserDefineFormwork.cml"
	profileCurrent  = "UserDefine.cml"
)

// profileEndpoints 组态文件里的端点遍历顺序（与 Python 版一致）
var profileEndpoints = []string{EpRender, EpCapture}

// profileGroups 属性 → 官方 ApoEffect 分组名（写进 .profile 的 <ApoEffect Name>，
// 便于与官方面板的导出文件互相阅读）。
//
// ★★ 第 51 轮（G 档）：整表归一为**页面 Canvas 名**。依据 = 官方随驱动发的组态
// 模板 CPL/FactoryDefault.xml（全机唯一一份官方格式样本）：
//
//	<ApoEffect Name="SpeakerSetting">  ← apoSpeakerSetting.cml Canvas Name
//	<ApoEffect Name="MicInVolume">     ← apoMicInVol.cml       Canvas Name
//
// 两例全中；exe 里 0x186A08~0x1875E0 那张 101 串的页面名表也是同一套
// 「端点族前缀 + 页名」命名。旧表的 Volume / Environment / Karaoke / … 出自
// C-Media 的**人读显示名**（CMCplExt_108B.dll 0x08DB28 的 "Full Name" 列表），
// 官方面板读不出归属 —— 用旧名导出的场景在官方面板里点了没反应。
// 已存旧档由 groupAlias 兜底。逐字段对应 Python 版 center_src.py 的 PROFILE_GROUPS。
var profileGroups = map[string]map[string]string{
	EpRender: {
		// apoSpeakerVol / apoSpeakerEQ / apoSpeakerEM
		"MuteControl": "SpeakerVolume", "VolumeControl": "SpeakerVolume",
		"VolumeScalarControl": "SpeakerVolume",
		"Enable_EQ_GFX":       "SpeakerEQ", "EQ_Slider": "SpeakerEQ",
		"Enable_RFX_GFX":  "SpeakerEM",
		"RFX_ENVIRONMENT": "SpeakerEM",
		"RFX_ROOMSIZE":    "SpeakerEM",
		// apoSpeakerKaraoke（人声消除 + 升降调同页）
		"Enable_VOCALCANCEL_GFX": "SpeakerKaraoke",
		"VOCALCANCEL_LEVEL":      "SpeakerKaraoke",
		"Enable_KEYSHIFT_GFX":    "SpeakerKaraoke",
		"KEYSHIFT_LEVEL":         "SpeakerKaraoke",
		// apoSpeakerSamplingRate / apoSpeakerSetting（声道复制也在这张页）
		"DevSampleRateControl":    "SpeakerSamplingRate",
		"Enable_Channel_COPY_LFX": "SpeakerSetting",
		// 设备控制，无页面
		"GFSControl": "GFS",
		// A 档（第 46 轮）
		"Enable_AUDIOBRILLIANT_LFX":    "SpeakerXearAudioBrilliant",
		"AUDIOBRILLIANT_LEVEL":         "SpeakerXearAudioBrilliant",
		"Enable_BM_GFX":                "SpeakerXearDynamicBass",
		"BM_Slider_BassLevel":          "SpeakerXearDynamicBass",
		"BM_Slider_Frequency":          "SpeakerXearDynamicBass",
		"Enable_SMART_VOLUME_GFX":      "SpeakerXearSmartVolume",
		"Enable_VOICECLARITY_LFX":      "SpeakerXearVoiceClarity",
		"VOICECLARITY_LEVEL":           "SpeakerXearVoiceClarity",
		"VOICECLARITY_NOISESUPP_LEVEL": "SpeakerXearVoiceClarity",
		// B 档（第 47 轮）
		"Enable_CONLAB_HPSPK_LFX": "SpeakerXearSurroundHeadPhone",
		"CONLAB_HPSPK_MODE":       "SpeakerXearSurroundHeadPhone",
		"CONLAB_HPSPK_ROOMSIZE":   "SpeakerXearSurroundHeadPhone",
		"CONLAB_HPSPK_SFX":        "SpeakerXearSurroundSpeakers",
		"CONLAB_HPSPK_SPKANGLE":   "SpeakerXearSurroundSpeakers",
		"Enable_SURRSENS_LFX":     "SpeakerSurroundMax",
		// C 档（第 48 轮）
		"Enable_DHDVS_LFX": "SpeakerDolbyHPVS",
		"DHDVS_USEDH":      "SpeakerDolbyHPVS",
		"DVS_Mode":         "SpeakerDolbyHPVS",
		"DH_Mode":          "SpeakerDolbyHPVS",
		// D 档（第 49 轮）：SS_*_ANGLE / SS_*_DIST 走 groupOf 的前缀规则
		"Enable_SS_LFX": "SpeakerVirtual",
		// E 档（第 50 轮）
		"Speaker_Channel_Setting": "SpeakerSetting",
		"Enable_SwapLfeCn_LFX":    "SpeakerSetting",
		"Enable_SwapSrBk_LFX":     "SpeakerSetting",
	},
	EpCapture: {
		"VolumeControl":       "MicInVolume",
		"VolumeScalarControl": "MicInVolume",
		"MuteControl":         "MicInVolume",
		// AA（模拟功放）控件就在 apoMicInVol.cml 上
		"AAVolumeControl":           "MicInVolume",
		"Enable_NOISEREDUCTION_LFX": "MicInNoiseReduction",
		"NoiseReduction_Level":      "MicInNoiseReduction",
		"Enable_MAGICVOICE":         "MicInXearMagicVoice",
		"MagicVoice_Selection":      "MicInXearMagicVoice",
		"AGCControl":                "MicInAGC",
		"DevSampleRateControl":      "MicInSamplingRate",
		// 无官方页，保留旧名
		"Enable_MICECHO": "MicEcho", "MICECHO_Level": "MicEcho",
		// 第 62 轮 · 麦克风专项三开关（组名 = 官方 Canvas 名，场景存档分组用）
		"Enable_AEC_LFX":      "MicInAEC",
		"Enable_ENC_LFX":      "MicInENC",
		"Enable_MICARRAY_LFX": "MicInBeamForming",
	},
}

// groupAlias 旧版工具写出过的分组名 → 归一到现在的 Canvas 名。
// **只用于读**：用户已存下来的场景不该因这次改名而显示成「其他」。
var groupAlias = map[string]string{
	"Volume":               "SpeakerVolume",
	"Environment":          "SpeakerEM",
	"Karaoke":              "SpeakerKaraoke",
	"KeyShift":             "SpeakerKaraoke",
	"SamplingRate":         "SpeakerSamplingRate",
	"ChannelCopy":          "SpeakerSetting",
	"XearSurroundSpeakers": "SpeakerVirtual",
	"NoiseReduction":       "MicInNoiseReduction",
	"XearMagicVoice":       "MicInXearMagicVoice",
	"AAVolume":             "MicInVolume",
}

// profileEndpoint 端点 → 官方 Endpoint 名 / DataFlow
var profileEndpoint = map[string][2]string{
	EpRender:  {"Speakers", "Out"},
	EpCapture: {"Mic", "In"},
}

// profileValueType 数值类型 → ValueType 文本（官方用 .NET 类型名）
var profileValueType = map[string]string{
	"bool": "System.UInt32", "int": "System.Int32", "float": "System.Single",
}

// ProfileEntry 列表里的一项
type ProfileEntry struct {
	Name    string `json:"name"`
	Desc    string `json:"desc"`
	Builtin bool   `json:"builtin"`
	Dir     string `json:"dir"`
	Items   int    `json:"items"`
	HasData bool   `json:"has_data"`
	// Groups 这份场景用到的模块短标签（如 ["EQ","虚拟环绕","降噪"]）。
	// 场景卡片的参数摘要直接用它，避免为每张卡片再拉一次内容。
	Groups []string `json:"groups"`
}

// kindName 把 wspec.kind 转成 Python 侧同名的类型字符串。
// 组态文件里的 ValueType / 写回时的分支都按这三个名字走，两条线必须一致。
func kindName(kind int) string {
	switch kind {
	case TBool:
		return "bool"
	case TInt:
		return "int"
	default:
		return "float"
	}
}

func appdataProfileDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		if h, err := os.UserHomeDir(); err == nil {
			base = h
		}
	}
	return filepath.Join(base, "iKF Audio Center", "profile")
}

// profileDirs 返回 [(目录, 是否驱动自带)]，用户目录排前面（同名时优先）
func profileDirs(d *Driver) [][2]interface{} {
	out := [][2]interface{}{{appdataProfileDir(), false}}
	if d != nil && d.cpl != "" {
		out = append(out,
			[2]interface{}{filepath.Join(d.cpl, "profile", "default"), true},
			[2]interface{}{filepath.Join(d.cpl, "profile"), true})
	}
	return out
}

// builtinScenes 内置场景名单：源包自带（UserDefine*/HiFi）+
// EnsureSeedScenes 播种（默认/游戏/音乐/电影）。
//
// ★ builtin 判定按名字、不按目录：便携包下用户自建场景与内置档同住
// CPL\profile（profileWriteDir 就返回这里），按目录一刀切会把用户场景
// 也标成内置 —— 前端场景墙 filter(!builtin) 直接被滤空，表现为
// 「自定义场景不见了、官方场景也没了应用入口」（2026-10-19 真机确认）。
var builtinScenes = map[string]bool{
	profileFactoryScene:  true, // 默认（原「原厂默认」，2026-10-20 改名）
	"游戏":                 true,
	"音乐":                 true,
	"电影":                 true,
	"UserDefine":         true,
	"UserDefineFormwork": true,
	"HiFi":               true,
}

// builtinMark 目录自带标记 × 内置名单 —— 两者同时成立才算内置档。
func builtinMark(dirBuiltin bool, stem string) bool {
	return dirBuiltin && builtinScenes[stem]
}

// sceneExclude 不进场景的参数（2026-10-19，回应「应用场景后部分开关不同步」）：
//   - 音量 / 静音：个人听音状态，不是场景属性。写进场景后，应用时写 -10dB
//     会被系统音量通路立刻回滚，读回值与档不一致 → 界面滑杆/读数「不同步」。
//   - DevSampleRateControl：设备级采样率，写它触发 USB 重协商 → 端点重建 →
//     采集端会话值全清（实测 6 项退回 ret=2、2 项暂时消失 ret=-1）。
//     播种/自建场景混进这一项，是「应用场景后麦克风页整体不同步」的根因。
//   - Speaker_Channel_Setting（2026-10-19 深夜补充）：设备级声道拓扑，用户在
//     设备页自己选（2=立体声/4/6=5.1/8=7.1），不属于音效场景。播种时设备
//     正好是 5.1，实读值 6 被存进四张官方卡 → 应用任何场景都把设备页切回
//     5.1（用户实测报障）。与音量/采样率同类：设备级状态，场景不动它。
//
// 三个收口：ProfileSave（用户自建）、fullSnapshot（播种补齐）、
// ProfileApply（兜底旧档里已有的这类项，跳过不写）。
var sceneExclude = map[string]bool{
	"VolumeControl":           true,
	"VolumeScalarControl":     true,
	"MuteControl":             true,
	"AAVolumeControl":         true,
	"DevSampleRateControl":    true,
	"Speaker_Channel_Setting": true,
}

// excludedFromScene 场景排除判定（统一入口）。
// 除两张名单（sceneExclude 设备级 + sceneUnproven APO 未实现）外，还要挡
// 带通道后缀的多通道项（如 VolumeScalarControl#0/#1）—— 档里存的是带 #
// 的名字，只按裸名精确匹配会漏（2026-10-19 实测：电影档 63 项里混着
// VolumeScalarControl#0/#1，apply 照写、播种判据也查不出来）。
func excludedFromScene(name string) bool {
	return strings.Contains(name, "#") || sceneExclude[name] || sceneUnproven[name]
}

// sceneUnproven APO 未实现的效果家族，不进场景（2026-10-19 用户拍板：
// 「自定义场景按照那些生效去调整就好，不需要把 APO 未实现的加进去」）。
//
// 判据链（内部取证报告 + 回环 A/B 实测 + REA-OUT 字符串
// 取证。★ 2026-10-19 深夜按反编译产物复核修正：此前「bit2/13/14 是空壳」
// 是前端注释笔误 —— K 档 §2.2 原文：GFX 只有 bit3/4/20 是空桩，bit1/2/13/14
// 四个都是真模块）：
//
//	真实现 —— 留在场景：CReverbLib 环境音效（bit1）、CEQ15BandLib EQ（bit2）、
//	          CKeyShifterLib 升降调（bit13）、CVocalCancellationLib 人声消除
//	          （bit14）—— 四个 GFX 位都有真实 vftable 与构造体（非空桩）；
//	          CONLAB 耳机环绕（第 44 轮真·应用流 A/B 测到 19.9，噪声底两倍）；
//	          采集侧 CBeamFormingLib 指向性麦克风 / CMagicVoiceLib 魔音
//	          及降噪/AGC/AEC/ENC/回音全部；
//	空桩/字符串零命中 —— 排除：SS 虚拟环绕家族（RTTI 无类名、A/B 四次噪声底）、
//	          Dolby 家族（DHDVS/PLIIX/NeoPC/DDL 全零命中）、动态低音
//	          （BassManagement 零命中）、音频增艳（Brilliant 零命中）、
//	          虚拟低音 / SonicMapper / 语音清晰度（双证据零命中）。
//	环绕增强（Enable_SURRSENS_LFX）特殊：CXearSurrMaxLib 有实体（0x4e748
//	          对象构造，K 档 LFX 唯一真模块），但 A/B 未测到生效——它有前置
//	          条件（需虚拟环绕/扬声器移位/真实多声道之一开启）。按用户
//	          「只收确认生效」口径暂不进场景；界面悬浮文字已改为如实描述
//	          （不再标「未实现」）。
//
// 总开关与从属参数一起排除 —— 总开关无效时子参数必然无效。
// 界面上这些控件照常显示可交互（UNPROVEN 口径：不置灰、写驱动、如实标注），
// 只是场景不再管理它们；驱动收写入的（ret=0）手动拨动仍即时生效。
var sceneUnproven = map[string]bool{
	// SS 虚拟环绕家族（总开关 + 8 方位角/距）
	"Enable_SS_LFX": true,
	"SS_C_ANGLE":    true,
	"SS_LF_ANGLE":   true,
	"SS_RF_ANGLE":   true,
	"SS_LS_ANGLE":   true,
	"SS_RS_ANGLE":   true,
	"SS_LB_ANGLE":   true,
	"SS_RB_ANGLE":   true,
	"SS_LFE_ANGLE":  true,
	"SS_C_DIST":     true,
	"SS_LF_DIST":    true,
	"SS_RF_DIST":    true,
	"SS_LS_DIST":    true,
	"SS_RS_DIST":    true,
	"SS_LB_DIST":    true,
	"SS_RB_DIST":    true,
	"SS_LFE_DIST":   true,
	// Dolby 家族
	"Enable_DHDVS_LFX": true,
	"DHDVS_USEDH":      true,
	"DH_Mode":          true,
	"DVS_Mode":         true,
	// 动态低音 / 音频增艳 / 语音清晰度
	"Enable_BM_GFX":                 true,
	"BM_Slider_BassLevel":           true,
	"BM_Slider_Frequency":           true,
	"Enable_AUDIOBRILLIANT_LFX":     true,
	"AUDIOBRILLIANT_LEVEL":          true,
	"Enable_VOICECLARITY_LFX":       true,
	"VOICECLARITY_LEVEL":            true,
	"VOICECLARITY_NOISESUPPRESSION": true,
	// 环绕增强：CXearSurrMaxLib 有实体但 A/B 未测到生效（前置条件依赖）
	"Enable_SURRSENS_LFX": true,
	// 虚拟低音 / SonicMapper / DDL / PLIIX / NeoPC
	"Enable_VIRTUALBASS_LFX": true,
	"Enable_SonicMapper":     true,
	"Enable_DDL_GFX":         true,
	"Enable_PLIIX_LFX":       true,
	"Enable_NEOPC_LFX":       true,
}

// hasExcludedItem 档里是否混有排除项 —— 是就说明这份档是排除名单出现
// 之前生成的，播种升级时整档重建。
func hasExcludedItem(items []ProfileItem) bool {
	for _, it := range items {
		if excludedFromScene(it.Name) {
			return true
		}
	}
	return false
}

// profileWriteDir 用户自建组态写到哪。CPL 可写就用 CPL（与官方面板共享），否则回退。
func profileWriteDir(d *Driver) string {
	if d != nil && d.cpl != "" {
		cplProf := filepath.Join(d.cpl, "profile")
		if st, err := os.Stat(cplProf); err == nil && st.IsDir() {
			t := filepath.Join(cplProf, ".wtest")
			if f, err := os.Create(t); err == nil {
				f.Close()
				os.Remove(t)
				return cplProf
			}
		}
	}
	p := appdataProfileDir()
	_ = os.MkdirAll(p, 0o755)
	return p
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if strings.HasPrefix(s, "\ufeff") {
		s = strings.TrimPrefix(s, "\ufeff")
	}
	return s
}

var reXmlTag = regexp.MustCompile(`(?i)<(/?)(Profile|Endpoint|ApoEffect|Property)\b([^>]*?)(/?)>`)

func cmlAttr(text, attr string) string {
	re := regexp.MustCompile(fmt.Sprintf(`\b%s\s*=\s*"([^"]*)"`, regexp.QuoteMeta(attr)))
	if m := re.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ProfileItem 一条 Property
type ProfileItem struct {
	Endpoint  string
	Flow      string
	Effect    string
	Name      string
	Value     string
	ValueType string
}

// profileItems 解析 .profile / FactoryDefault.xml
func profileItems(text string) []ProfileItem {
	var items []ProfileItem
	epName, flow, fx := "", "", ""
	for _, m := range reXmlTag.FindAllStringSubmatch(text, -1) {
		closing, tag, attrs := m[1], strings.ToLower(m[2]), m[3]
		switch tag {
		case "endpoint":
			if closing != "" {
				epName, flow = "", ""
			} else {
				epName = cmlAttr(attrs, "Name")
				flow = cmlAttr(attrs, "DataFlow")
			}
		case "apoeffect":
			if closing != "" {
				fx = ""
			} else {
				fx = cmlAttr(attrs, "Name")
			}
		case "property":
			if closing == "" {
				items = append(items, ProfileItem{
					Endpoint: epName, Flow: flow, Effect: fx,
					Name:      cmlAttr(attrs, "Name"),
					Value:     cmlAttr(attrs, "Value"),
					ValueType: cmlAttr(attrs, "ValueType"),
				})
			}
		}
	}
	return items
}

// kindOfValueType 官方 ValueType（.NET 类型名）→ 我们的 kind 字符串
func kindOfValueType(vtype, name string) string {
	v := strings.TrimSpace(vtype)
	if strings.HasSuffix(v, "Single") || strings.HasSuffix(v, "Double") {
		return "float"
	}
	if genericSet[name] && strings.HasSuffix(v, "UInt32") {
		// 通用表里的 UInt32 基本就是 0/1 开关
		return "bool"
	}
	return "int"
}

// coerce 组态文件里的文本值 → 数值。不合法返回 (0, false)
func coerce(val, kind string) (interface{}, bool) {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(val), "%f", &f); err != nil {
		return nil, false
	}
	switch kind {
	case "float":
		return f, true
	case "bool":
		if f != 0 {
			return 1, true
		}
		return 0, true
	default:
		return int(f), true
	}
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func groupOf(ep, name string) string {
	// ★ 第 52 轮：先剥掉 "#<声道索引>" 后缀再查表 —— 逐声道音量与主音量
	// 归属同一张官方页（SpeakerVolume / apoSpeakerVol.cml）。
	if base, _ := splitChannel(name); base != name {
		name = base
	}
	if tbl, ok := profileGroups[ep]; ok {
		if g, ok := tbl[name]; ok {
			return g
		}
	}
	if strings.HasPrefix(name, "EQ_Slider") {
		return "SpeakerEQ"
	}
	// 7.1 虚拟音箱摆位的 8 组 SS_*_ANGLE / SS_*_DIST（第 49 轮，D 档）在
	// writable 里是循环登记的，逐个铺进 profileGroups 会重复 16 行且以后加
	// 音箱还会漏。用前缀兜住，等价于映射到 apoSpeakerVirtual.cml。
	if strings.HasPrefix(name, "SS_") &&
		(strings.HasSuffix(name, "_ANGLE") || strings.HasSuffix(name, "_DIST")) {
		return "SpeakerVirtual"
	}
	return "General"
}

// groupLabel 官方 ApoEffect 分组名（= 页面 Canvas 名）→ 界面上显示的短标签。
// 场景详情只列分组，不逐个铺属性名 —— 一份场景动辄十几项，
// 全铺出来右侧面板撑不下，也没有可读性。
// 一张官方页住着多个模块时（Karaoke 页=人声消除+升降调、Setting 页=声道
// 配置/互换/复制、MicInVol 页=麦克风音量+模拟功放）写成合并描述：官方本来就
// 按「页」归属，拆开反而与官方 .profile 对不上。
var groupLabel = map[string]string{
	"SpeakerVolume":                "音量",
	"SpeakerEQ":                    "EQ",
	"SpeakerEM":                    "环境声",
	"SpeakerKaraoke":               "人声消除与变调",
	"SpeakerSamplingRate":          "采样率",
	"SpeakerSetting":               "声道设定",
	"SpeakerVirtual":               "虚拟环绕",
	"SpeakerXearSurroundHeadPhone": "耳机环绕",
	"SpeakerXearSurroundSpeakers":  "环绕扬声器",
	"SpeakerSurroundMax":           "环绕增强",
	"SpeakerDolbyHPVS":             "Dolby 环绕",
	"SpeakerXearAudioBrilliant":    "音频增艳",
	"SpeakerXearDynamicBass":       "动态低音",
	"SpeakerXearSmartVolume":       "智能音量",
	"SpeakerXearVoiceClarity":      "语音清晰度",
	"GFS":                          "GFS",
	"MicInVolume":                  "麦克风音量",
	"MicInNoiseReduction":          "降噪",
	"MicInXearMagicVoice":          "魔音",
	"MicEcho":                      "回音",
	"MicInAGC":                     "自动增益",
	// 第 62 轮 · 麦克风专项三组（组名 = 官方 Canvas 名；中文名照官方 zh-CN 词条，
	// ENC 官方未译，定「通话降噪」与既有「环境降噪」区分）
	"MicInAEC":          "回音消除",
	"MicInENC":          "通话降噪",
	"MicInBeamForming":  "麦克风收音",
	"MicInSamplingRate": "麦克风采样率",
	// 这两组我们**不写**，只用来把官方 FactoryDefault.xml（「原厂默认」场景）
	// 读好看 —— 它用的是 SpeakerQuarter / DesktopMic 两个族。
	"SpeakerQuarterSetting":  "四声道设定",
	"DesktopMicSamplingRate": "桌面麦克风采样率",
	"General":                "其他",
}

// groupOrder 渲染端点的分组展示顺序（照官方面板菜单：音量→EQ→环境→环绕一族→
// 人声→Xear 音效族→设定/采样率；采集端点整体排在后面 —— 场景的主体是听感，
// 麦克风是附加）。
var groupOrder = []string{
	"SpeakerVolume", "SpeakerEQ", "SpeakerEM", "SpeakerVirtual",
	"SpeakerXearSurroundHeadPhone", "SpeakerXearSurroundSpeakers",
	"SpeakerSurroundMax", "SpeakerDolbyHPVS", "SpeakerKaraoke",
	"SpeakerXearAudioBrilliant", "SpeakerXearDynamicBass",
	"SpeakerXearSmartVolume", "SpeakerXearVoiceClarity",
	"SpeakerSetting", "SpeakerSamplingRate", "GFS",
	"MicInVolume", "MicInNoiseReduction", "MicInAGC",
	"MicInXearMagicVoice", "MicEcho",
	"MicInAEC", "MicInENC", "MicInBeamForming",
	"MicInSamplingRate",
	"SpeakerQuarterSetting", "DesktopMicSamplingRate",
}

func groupRank(ep, key string) int {
	base := 0
	if ep == EpCapture {
		base = 1000
	}
	for i, g := range groupOrder {
		if g == key {
			return base + i
		}
	}
	return base + 500 // 表外的自定义分组排在已知分组之后
}

// profileGroupLabels 一份场景用到了哪些模块：按展示顺序去重，最多 4 个。
// 卡片副行只放得下这么多，再多就没有可读性了。
func profileGroupLabels(items []ProfileItem) []string {
	type row struct {
		rank  int
		key   string
		label string
	}
	seen := map[string]bool{}
	var rows []row
	for _, it := range items {
		if it.Name == "" {
			continue
		}
		ep := EpRender
		if strings.EqualFold(it.Flow, "in") {
			ep = EpCapture
		}
		g := it.Effect
		if g == "" {
			g = groupOf(ep, it.Name)
		}
		if a, ok := groupAlias[g]; ok { // 旧版工具写过的名字 → Canvas 名
			g = a
		}
		if seen[g] {
			continue
		}
		seen[g] = true
		lab, ok := groupLabel[g]
		if !ok {
			lab = g
		}
		rows = append(rows, row{groupRank(ep, g), g, lab})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].rank != rows[j].rank {
			return rows[i].rank < rows[j].rank
		}
		return rows[i].key < rows[j].key
	})
	out := []string{}
	for _, r := range rows {
		if len(out) >= 4 {
			break
		}
		out = append(out, r.label)
	}
	return out
}

type snapRow struct {
	Ep   string
	Name string
	Kind string
	Val  string
}

// buildProfileXML snapshot → 官方 schema 文本
func buildProfileXML(name string, snap []snapRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<Profile Name=\"%s\">\n", xmlEscape(name))
	for _, ep := range profileEndpoints {
		epName, flow := profileEndpoint[ep][0], profileEndpoint[ep][1]
		fmt.Fprintf(&b, "\t<Endpoint Name=\"%s\" DataFlow=\"%s\">\n", epName, flow)
		var order []string
		groups := map[string][]snapRow{}
		for _, r := range snap {
			if r.Ep != ep {
				continue
			}
			g := groupOf(ep, r.Name)
			if _, ok := groups[g]; !ok {
				order = append(order, g)
			}
			groups[g] = append(groups[g], r)
		}
		for _, g := range order {
			fmt.Fprintf(&b, "\t\t<ApoEffect Name=\"%s\">\n", g)
			for _, r := range groups[g] {
				vtype := profileValueType[r.Kind]
				if vtype == "" {
					vtype = "System.Int32"
				}
				// Args 用真实声道索引：OEM 属性走 -1（主声道），通用属性 0
				args := "-1"
				if genericSet[r.Name] {
					args = "0"
				}
				fmt.Fprintf(&b, "\t\t\t<Property Name=\"%s\" Value=\"%s\" UIName=\"\" "+
					"ValueType=\"%s\" Args=\"%s\" ArgsType=\"System.Int32\"/>\n",
					r.Name, xmlEscape(r.Val), vtype, args)
			}
			b.WriteString("\t\t</ApoEffect>\n")
		}
		b.WriteString("\t</Endpoint>\n")
	}
	b.WriteString("</Profile>\n")
	return b.String()
}

// buildCML 新增组态时写出的 RadioButton（结构照抄官方的 UserDefineFormwork.cml）
func buildCML(name, desc string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
		<RadioButton Name="%s" GroupName="ProFileMenu" Left="0" Top="0" `+
		`Width="42" Height="41" Cursor="Hand" `+
		`Image.Uncheck.Normal="PanelProfile/UserDefine_no.png" `+
		`Image.Check.Normal="PanelProfile/UserDefine_se.png" `+
		`Image.Check.MouseOver="PanelProfile/UserDefine_se.png" `+
		`Image.Uncheck.MouseOver="PanelProfile/UserDefine_mo.png" `+
		`Tooltip="%s" Tooltip.Check="%s" />
`, xmlEscape(name), xmlEscape(desc), xmlEscape(desc))
}

// ---------------------------------------------------------------- 语言资源

var langCache map[string]string

// langMap CPL/lang/zh-CN/*.xaml → {ml_键: 中文}。
// .cml 里 Tooltip 常直接写资源键（例如内置组态 HiFi 的 Tooltip 是
// ml_PanelProfile_HiFi），官方 UI 靠 XAML 资源字典解析。
func langMap(d *Driver) map[string]string {
	if langCache != nil {
		return langCache
	}
	out := map[string]string{}
	if d != nil && d.cpl != "" {
		dir := filepath.Join(d.cpl, "lang", "zh-CN")
		if entries, err := os.ReadDir(dir); err == nil {
			var names []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".xaml") {
					names = append(names, e.Name())
				}
			}
			sort.Strings(names)
			re := regexp.MustCompile(`x:Key="([^"]+)"\s*>([^<]*)<`)
			for _, fn := range names {
				txt := readText(filepath.Join(dir, fn))
				for _, m := range re.FindAllStringSubmatch(txt, -1) {
					out[m[1]] = strings.TrimSpace(m[2])
				}
			}
		}
	}
	langCache = out
	return out
}

// langText 把 ml_* 资源键还原成官方中文；不是键就原样返回
func langText(d *Driver, s string) string {
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "ml_") {
		if v, ok := langMap(d)[s]; ok {
			return v
		}
	}
	return s
}

// ---------------------------------------------------------------- 四个动作

// Profiles 列出全部组态
func (d *Driver) Profiles() map[string]interface{} {
	seen := map[string]bool{}
	list := []ProfileEntry{}
	for _, pair := range profileDirs(d) {
		dir, _ := pair[0].(string)
		dirBuiltin, _ := pair[1].(bool)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, fn := range names {
			if !strings.HasSuffix(strings.ToLower(fn), ".cml") {
				continue
			}
			if fn == profileTemplate || fn == profileCurrent {
				continue
			}
			stem := fn[:len(fn)-4]
			if seen[stem] {
				continue // 用户目录优先
			}
			seen[stem] = true
			cml := readText(filepath.Join(dir, fn))
			pfile := filepath.Join(dir, stem+".profile")
			cnt := 0
			has := false
			var glabels []string
			if st, err := os.Stat(pfile); err == nil && !st.IsDir() {
				has = true
				its := profileItems(readText(pfile))
				cnt = len(its)
				glabels = profileGroupLabels(its)
			}
			// 官方空模板（如 default/HiFi 只有 .cml 骨架、.profile 无参数
			// 快照）不进列表 —— 应用它等于什么都没做，还占一张场景卡。
			// 用户自建档不受影响：ProfileSave 一定带全量快照。
			if dirBuiltin && cnt == 0 {
				continue
			}
			list = append(list, ProfileEntry{
				Name: stem, Desc: langText(d, cmlAttr(cml, "Tooltip")),
				Builtin: builtinMark(dirBuiltin, stem),
				Dir:     dir, Items: cnt, HasData: has,
				Groups: glabels,
			})
		}
	}
	// 排序（2026-10-20 用户拍板：「默认」放第一位）：
	//   0 = 「默认」（一键还原基线，永远第一张卡）
	//   1 = 其余内置档（游戏/音乐/电影，按名字）
	//   2 = 用户自建档（通话/直播…，按名字）
	// 之前的口径是「用户档优先」，改默认置顶后官方档整体提前。
	rank := func(e ProfileEntry) int {
		if e.Name == profileFactoryScene {
			return 0
		}
		if e.Builtin {
			return 1
		}
		return 2
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i]), rank(list[j])
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})

	factory := ""
	if d != nil && d.cpl != "" {
		fp := filepath.Join(d.cpl, "FactoryDefault.xml")
		if st, err := os.Stat(fp); err == nil && !st.IsDir() {
			factory = fp
		}
	}
	// ★ 「预设」（FactoryDefault.xml）不再作为场景卡列出（2026-10-19 用户拍板）：
	//   它整档只有一条 DefaultDeviceControl=1 一键出厂指令（本机驱动未实现，
	//   应用必然无效），语义与 EnsureSeedScenes 播种的「原厂默认」卡重复 ——
	//   两张恢复出厂卡同墙只会让用户疑惑哪张才是真的。factory 路径仍随
	//   API 返回，名字「预设」继续作为保留字拦截（防止建出同名用户档）。
	wdir := profileWriteDir(d)
	writable := false
	if f, err := os.Create(filepath.Join(wdir, ".wtest")); err == nil {
		f.Close()
		os.Remove(filepath.Join(wdir, ".wtest"))
		writable = true
	}
	// active：设备真值识别 —— 当前设备参数与哪个场景快照完全一致就显示
	// 哪个；都不一致为 ""（自定义状态）。取代前端 localStorage 残留记忆
	// （2026-10-20 用户拍板：「使用中 · X」必须跟设备真实状态走）。
	return map[string]interface{}{
		"ok": true, "dir": wdir, "writable": writable,
		"factory": factory, "list": list, "active": identifyActiveScene(d),
	}
}

// profilePath 按名字找到 .profile 文件
func (d *Driver) profilePath(name string) (string, string, bool) {
	for _, pair := range profileDirs(d) {
		dir, _ := pair[0].(string)
		dirBuiltin, _ := pair[1].(bool)
		p := filepath.Join(dir, name+".profile")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, dir, builtinMark(dirBuiltin, name)
		}
	}
	return "", "", false
}

var reBadName = regexp.MustCompile(`[\\/:*?"<>|]`)

// ProfileSave 把当前全部可写参数快照成组态，写出 .profile + .cml（官方格式）
func (d *Driver) ProfileSave(name, desc string) map[string]interface{} {
	name = strings.TrimSpace(name)
	if name == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "场景名称不能为空"}
	}
	if reBadName.MatchString(name) {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "文件名不合法 (不可包含 /\\:*?\"<>| 字符)"}
	}
	if name == profileFactory {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": fmt.Sprintf("「%s」是默认场景，不能覆盖", profileFactory)}
	}

	var snap []snapRow
	for _, ep := range profileEndpoints {
		if _, ok := d.ptrFor(ep); !ok {
			continue
		}
		tbl := writable[ep]
		var pnames []string
		for p := range tbl {
			pnames = append(pnames, p)
		}
		sort.Strings(pnames)
		for _, pname := range pnames {
			// ★ 第 52 轮：`VolumeScalarControl#0/#1` 这类带声道后缀的虚拟参数
			// 不是驱动属性名，写进 .profile 官方面板读不懂、我们自己 apply
			// 也会失败 ⟹ 场景里一律不收（官方 FactoryDefault.xml 也没有逐声道音量）。
			// 口径统一走 excludedFromScene（sceneExclude 裸名 + # 后缀）。
			if excludedFromScene(pname) {
				continue
			}
			ret, val := d.ReadEp(ep, pname)
			if ret != 0 {
				continue
			}
			snap = append(snap, snapRow{ep, pname, kindName(tbl[pname].kind), val})
		}
	}
	if len(snap) == 0 {
		return map[string]interface{}{"ok": false, "ret": -2,
			"error": "当前没有可保存的设置"}
	}

	dir := profileWriteDir(d)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return map[string]interface{}{"ok": false, "ret": -3,
			"error": fmt.Sprintf("无法创建场景保存目录：%s", err)}
	}
	if desc == "" {
		desc = name
	}
	if err := os.WriteFile(filepath.Join(dir, name+".profile"),
		[]byte(buildProfileXML(name, snap)), 0o644); err != nil {
		return map[string]interface{}{"ok": false, "ret": -3,
			"error": fmt.Sprintf("写入失败：%s", err)}
	}
	if err := os.WriteFile(filepath.Join(dir, name+".cml"),
		[]byte(buildCML(name, desc)), 0o644); err != nil {
		return map[string]interface{}{"ok": false, "ret": -3,
			"error": fmt.Sprintf("写入失败：%s", err)}
	}
	return map[string]interface{}{"ok": true, "ret": 0, "name": name,
		"dir": dir, "items": len(snap)}
}

// ProfileApply 把组态里的每一条 Property 写回驱动
func (d *Driver) ProfileApply(name string) map[string]interface{} {
	var path string
	if name == profileFactory {
		if d.cpl == "" {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": "找不到 FactoryDefault.xml"}
		}
		path = filepath.Join(d.cpl, "FactoryDefault.xml")
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": "找不到 FactoryDefault.xml"}
		}
	} else {
		p, _, _ := d.profilePath(name)
		if p == "" {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": fmt.Sprintf("找不到场景「%s」", name)}
		}
		path = p
	}

	items := profileItems(readText(path))
	if len(items) == 0 {
		return map[string]interface{}{"ok": true, "ret": 0, "name": name,
			"applied": 0, "failed": 0, "skipped": 0,
			"note": "该场景为空（等同恢复驱动默认设置）"}
	}

	// writeOp 一条待写参数；isSwitch=true 的（布尔效果开关）第一轮先写。
	type writeOp struct {
		ep, name, kind    string
		v                 interface{}
		unknown, isSwitch bool
	}
	var ops []writeOp
	applied, failed, skipped := 0, 0, 0
	detail := []map[string]interface{}{}
	done := map[string]bool{}
	// DefaultDeviceControl（恢复出厂命令）被驱动拒绝的标记。
	// 本机 OEM 驱动未实现该命令（--serve 实测 0x10003，2026-10-15）；
	// 预设档整档就是这条指令，不含参数快照，原版面板同样无法恢复出厂。
	factoryCmdFailed := false
	for _, it := range items {
		ep := EpRender
		if strings.EqualFold(it.Flow, "in") {
			ep = EpCapture
		}
		if it.Name == "" {
			skipped++
			continue
		}
		// FactoryDefault.xml 里同一个属性会在多个 Endpoint 下重复出现
		// （Speakers / SpeakerQuarter / Mic / DesktopMic），去重后再写。
		key := ep + "\x00" + it.Name
		if done[key] {
			continue
		}
		done[key] = true

		// 兜底：旧档（含排除名单出现前保存的）里混进的音量/采样率项不写 ——
		// 音量被系统通路回滚造成读数不一致；采样率触发端点重建清空会话值。
		// 带通道后缀的（VolumeScalarControl#0）同样要挡，见 excludedFromScene。
		if excludedFromScene(it.Name) {
			skipped++
			continue
		}

		sp, known := spec(ep, it.Name)
		kind := ""
		unknown := !known
		if known {
			kind = kindName(sp.kind)
		} else {
			// 白名单外但确实是 208 项里的属性 —— 照样真写，
			// 由驱动决定收不收（官方组态里就有这类属性）。
			kind = kindOfValueType(it.ValueType, it.Name)
		}
		v, okv := coerce(it.Value, kind)
		if !okv {
			skipped++
			detail = append(detail, map[string]interface{}{
				"name": it.Name, "ep": ep, "ret": -1,
				"error": fmt.Sprintf("值不合法：%s", it.Value)})
			continue
		}
		ops = append(ops, writeOp{ep: ep, name: it.Name, v: v,
			kind: kind, unknown: unknown, isSwitch: known && sp.kind == TBool})
	}

	// ★ 两轮写入（2026-10-19，回应「应用场景后级别参数写不进」）：
	// 驱动语义是**效果开关先开，级别/滑杆参数才收写入**（真机探针证实：
	// AUDIOBRILLIANT_LEVEL 等 5 组在 Enable_ 关闭时写一律失败，开关打开
	// 后立即成功）。所以第一轮先写全部布尔开关，第二轮写其余参数。
	for _, pass := range []bool{true, false} {
		for _, op := range ops {
			if op.isSwitch != pass {
				continue
			}
			var res map[string]interface{}
			if op.unknown {
				res = d.WriteRaw(op.ep, op.name, op.v, op.kind)
			} else {
				res = d.WriteEp(op.ep, op.name, op.v)
			}
			if okr, _ := res["ok"].(bool); okr {
				applied++
			} else {
				failed++
				errMsg, _ := res["error"].(string)
				if op.name == "DefaultDeviceControl" {
					factoryCmdFailed = true
					errMsg = "当前耳机不支持一键恢复出厂设置"
				}
				detail = append(detail, map[string]interface{}{
					"name": op.name, "ep": op.ep,
					"ret": res["ret"], "error": errMsg})
			}
		}
	}
	code := 0
	if failed > 0 {
		code = -3
	}
	if len(detail) > 12 {
		detail = detail[:12]
	}
	m := map[string]interface{}{"ok": failed == 0, "ret": code,
		"name": name, "applied": applied, "failed": failed,
		"skipped": skipped, "detail": detail}
	if factoryCmdFailed && applied == 0 {
		m["error"] = "当前耳机不支持一键恢复出厂设置，该场景无法应用"
	}
	return m
}

// ProfileDelete 删除用户自建组态。驱动自带的档拒绝删除。
func (d *Driver) ProfileDelete(name string) map[string]interface{} {
	name = strings.TrimSpace(name)
	if name == "" {
		return map[string]interface{}{"ok": false, "ret": -1, "error": "缺少场景名称"}
	}
	if name == profileFactory {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "原厂默认档不可删除"}
	}
	wdir := profileWriteDir(d)
	path, dir, builtin := d.profilePath(name)
	if path == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": fmt.Sprintf("找不到场景「%s」", name)}
	}
	// 只允许删用户目录里的档；驱动自带目录即使可写也不动
	if builtin && !strings.EqualFold(dir, wdir) {
		return map[string]interface{}{"ok": false, "ret": -2,
			"error": fmt.Sprintf("「%s」是系统内置场景，不能删除", name)}
	}
	removed := []string{}
	for _, ext := range []string{".profile", ".cml"} {
		p := filepath.Join(dir, name+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if err := os.Remove(p); err != nil {
				return map[string]interface{}{"ok": false, "ret": -3,
					"error": fmt.Sprintf("删除失败：%s", err)}
			}
			removed = append(removed, filepath.Base(p))
		}
	}
	return map[string]interface{}{"ok": true, "ret": 0, "name": name,
		"removed": removed}
}

// ProfileItems 读出一个场景的实际内容。
//
// 两处用途（场景页改版后新增）：
//   - 右侧「场景详情」：按 ApoEffect 分组列出这份场景动了哪些模块
//   - 「已修改、未保存」脏标记：pairs 与当前驱动读数逐项比对
//
// pairs 的键统一成 "render|EQ_Slider1" / "capture|AGCControl"，
// 值与 .profile 里存的文本一致；前端拿它和 /api/params 的 v 做归一化比较。
//
// 与 ProfileApply 一致，同一个 (端点, 属性) 只保留第一次出现 ——
// FactoryDefault.xml 里同一属性会在 Speakers / SpeakerQuarter / Mic 下重复。
func (d *Driver) ProfileItems(name string) map[string]interface{} {
	name = strings.TrimSpace(name)
	if name == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "缺少场景名称"}
	}
	path, builtin := "", false
	if name == profileFactory {
		if d == nil || d.cpl == "" {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": "找不到 FactoryDefault.xml"}
		}
		path = filepath.Join(d.cpl, "FactoryDefault.xml")
		builtin = true
	} else {
		p, _, b := d.profilePath(name)
		if p == "" {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": fmt.Sprintf("找不到场景「%s」", name)}
		}
		path, builtin = p, b
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": fmt.Sprintf("读不到场景「%s」的内容", name)}
	}

	type grpOut struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Ep    string `json:"ep"`
		N     int    `json:"n"`
		On    int    `json:"on"` // 2026-10-20：该组「开启」的开关数（值=1 的 Enable_*/AGCControl）
	}
	pairs := map[string]string{}
	seen := map[string]bool{}
	idx := map[string]*grpOut{}
	var order []string
	onTotal := 0

	// isSwitchOn 「开启」口径（2026-10-20 用户拍板：详情卡显示实际应用参数）：
	// 只有开关型参数（Enable_* / AGCControl）值为 1 才算「开启」；
	// 档位/滑杆（EQ_Slider*、*_LEVEL、RFX_ENVIRONMENT、CONLAB_HPSPK_*…）
	// 跟随所属开关，不重复计数——否则一份「全 36 项」的档会显得全开。
	isSwitchOn := func(name, val string) bool {
		if val != "1" {
			return false
		}
		return strings.HasPrefix(name, "Enable_") || name == "AGCControl"
	}

	for _, it := range profileItems(readText(path)) {
		if it.Name == "" {
			continue
		}
		ep := EpRender
		if strings.EqualFold(it.Flow, "in") {
			ep = EpCapture
		}
		key := ep + "|" + it.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		pairs[key] = it.Value
		inc := 0
		if isSwitchOn(it.Name, it.Value) {
			inc = 1
			onTotal++
		}

		// 文件里写的 <ApoEffect Name> 才是真正的分组归属；
		// 缺了（手工编辑过的档）再退回属性名反查。
		g := it.Effect
		if g == "" {
			g = groupOf(ep, it.Name)
		}
		if a, ok := groupAlias[g]; ok { // 旧版工具写过的名字 → Canvas 名
			g = a
		}
		gk := ep + "|" + g
		if g2, ok := idx[gk]; ok {
			g2.N++
			g2.On += inc
			continue
		}
		lab, ok := groupLabel[g]
		if !ok {
			lab = g
		}
		idx[gk] = &grpOut{Key: g, Label: lab, Ep: ep, N: 1, On: inc}
		order = append(order, gk)
	}

	groups := make([]grpOut, 0, len(order))
	for _, gk := range order {
		groups = append(groups, *idx[gk])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		ri, rj := groupRank(groups[i].Ep, groups[i].Key),
			groupRank(groups[j].Ep, groups[j].Key)
		if ri != rj {
			return ri < rj
		}
		return groups[i].Key < groups[j].Key
	})

	return map[string]interface{}{
		"ok": true, "ret": 0, "name": name, "builtin": builtin,
		"dir": filepath.Dir(path), "count": len(pairs), "on": onTotal,
		"groups": groups, "pairs": pairs,
	}
}

// reProfileNameAttr 匹配 .profile 根节点上的 Name 属性（连同它前面的引号）
var reProfileNameAttr = regexp.MustCompile(`(<Profile\b[^>]*?\bName=")[^"]*(")`)

// ProfileRename 重命名用户自建场景：内容原样搬到新名字下。
//
// 做法是「读旧档 → 改根节点 Name → 写新档 → 删旧档」，而不是
// 重新快照当前驱动状态 —— 改名的语义是「换个名字」，不是「用现在的
// 状态覆盖它」。官方 .profile / .cml 的其余结构原样保留。
//
// 只允许改用户写入目录里的档；驱动自带的档与「预设」拒绝改名。
func (d *Driver) ProfileRename(oldName, newName string) map[string]interface{} {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "缺少原场景名称"}
	}
	if newName == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": "场景名称不能为空"}
	}
	if reBadName.MatchString(newName) {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": `文件名不合法 (不可包含 /\:*?"<>| 字符)`}
	}
	if newName == profileFactory {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": fmt.Sprintf("「%s」是默认场景，不能占用这个名字", profileFactory)}
	}
	if newName == oldName {
		return map[string]interface{}{"ok": true, "ret": 0,
			"name": newName, "renamed": false}
	}

	wdir := profileWriteDir(d)
	path, dir, builtin := d.profilePath(oldName)
	if path == "" {
		return map[string]interface{}{"ok": false, "ret": -1,
			"error": fmt.Sprintf("找不到场景「%s」", oldName)}
	}
	if builtin && !strings.EqualFold(filepath.Clean(dir), filepath.Clean(wdir)) {
		return map[string]interface{}{"ok": false, "ret": -2,
			"error": fmt.Sprintf("「%s」是系统内置场景，不能重命名", oldName)}
	}
	if _, _, exists := d.profilePath(newName); exists {
		return map[string]interface{}{"ok": false, "ret": -3,
			"error": fmt.Sprintf("已存在同名场景「%s」", newName)}
	}
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		return map[string]interface{}{"ok": false, "ret": -4,
			"error": fmt.Sprintf("无法创建场景目录：%s", err)}
	}

	// 描述留在 .cml 的 Tooltip 里；列表项读的就是它，改名时一并搬走。
	desc := langText(d, cmlAttr(readText(filepath.Join(dir, oldName+".cml")), "Tooltip"))

	body := readText(path)
	if !reProfileNameAttr.MatchString(body) {
		return map[string]interface{}{"ok": false, "ret": -5,
			"error": "场景文件结构异常，未找到 <Profile Name> 节点"}
	}
	// ReplaceAllString 的替换串里 $ 有特殊含义，名字里带 $ 会串味
	safe := strings.ReplaceAll(xmlEscape(newName), "$", "$$")
	body = reProfileNameAttr.ReplaceAllString(body, "${1}"+safe+"${2}")

	if err := os.WriteFile(filepath.Join(wdir, newName+".profile"),
		[]byte(body), 0o644); err != nil {
		return map[string]interface{}{"ok": false, "ret": -6,
			"error": fmt.Sprintf("写入失败：%s", err)}
	}
	if err := os.WriteFile(filepath.Join(wdir, newName+".cml"),
		[]byte(buildCML(newName, desc)), 0o644); err != nil {
		os.Remove(filepath.Join(wdir, newName+".profile"))
		return map[string]interface{}{"ok": false, "ret": -6,
			"error": fmt.Sprintf("写入失败：%s", err)}
	}
	for _, ext := range []string{".profile", ".cml"} {
		p := filepath.Join(dir, oldName+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			os.Remove(p)
		}
	}
	return map[string]interface{}{"ok": true, "ret": 0,
		"name": newName, "from": oldName, "renamed": true}
}

// ---------------------------------------------------------------- 预置场景

// profileFactoryScene 自建「默认」场景名（2026-10-20 用户拍板：原名「原厂默认」
// 改短）：官方 FactoryDefault.xml 只是一条本机驱动拒收的恢复出厂指令
// （见 ProfileApply），对用户毫无用处；改为把当前驱动状态整档快照，
// 成为可一键还原的基线。
const profileFactoryScene = "默认"

// seedItem 一条预置参数
type seedItem struct {
	Ep   string
	Name string
	Val  string
}

// seedScenes 预置场景（游戏/音乐/电影）。参数只取本机驱动实测可写的
// 项（2026-10-15 --dump：render 66 项 / capture 38 项），VirtualBass、
// VoiceClarity、AudioBrilliant 等本机 APO 未实现的效果一概不放，
// 避免应用时被驱动拒收、整档标「不可用」。
//
// EQ 曲线取值范围 ±12（与官方 12 预设一致）：
//
//	游戏  —— 低频守门 + 高频强化（脚步/枪声定位），自定义曲线
//	音乐  —— 官方「古典乐」曲线（Classic，0x14018a090 索引 9 静态提取）
//	电影  —— 低频震撼 + 高频微亮，自定义曲线
var seedScenes = []struct {
	Name  string
	Desc  string
	Items []seedItem
}{
	{
		Name: "游戏",
		Desc: "游戏场景 · 听声辨位（虚拟环绕 + 语音清晰）",
		Items: []seedItem{
			{EpRender, "Enable_EQ_GFX", "1"},
			{EpRender, "EQ_Slider1", "4"}, {EpRender, "EQ_Slider2", "3"},
			{EpRender, "EQ_Slider3", "1"}, {EpRender, "EQ_Slider4", "-1"},
			{EpRender, "EQ_Slider5", "-3"}, {EpRender, "EQ_Slider6", "-2"},
			{EpRender, "EQ_Slider7", "1"}, {EpRender, "EQ_Slider8", "4"},
			{EpRender, "EQ_Slider9", "6"}, {EpRender, "EQ_Slider10", "7"},
			{EpRender, "Enable_SS_LFX", "1"},
			{EpRender, "Enable_RFX_GFX", "0"},
			{EpRender, "Enable_VOCALCANCEL_GFX", "0"},
			{EpRender, "Enable_KEYSHIFT_GFX", "0"},
			{EpRender, "Enable_Channel_COPY_LFX", "0"},
			{EpCapture, "Enable_NOISEREDUCTION_LFX", "1"},
			{EpCapture, "Enable_MAGICVOICE", "0"},
			{EpCapture, "Enable_MICECHO", "0"},
			{EpCapture, "AGCControl", "1"},
		},
	},
	{
		Name: "音乐",
		Desc: "音乐场景 · 原声还原（官方古典乐 EQ 曲线）",
		Items: []seedItem{
			{EpRender, "Enable_EQ_GFX", "1"},
			{EpRender, "EQ_Slider1", "0"}, {EpRender, "EQ_Slider2", "7"},
			{EpRender, "EQ_Slider3", "-1"}, {EpRender, "EQ_Slider4", "-7"},
			{EpRender, "EQ_Slider5", "-12"}, {EpRender, "EQ_Slider6", "-8"},
			{EpRender, "EQ_Slider7", "0"}, {EpRender, "EQ_Slider8", "10"},
			{EpRender, "EQ_Slider9", "4"}, {EpRender, "EQ_Slider10", "-5"},
			{EpRender, "Enable_SS_LFX", "0"},
			{EpRender, "Enable_RFX_GFX", "0"},
			{EpRender, "Enable_VOCALCANCEL_GFX", "0"},
			{EpRender, "Enable_KEYSHIFT_GFX", "0"},
			{EpRender, "Enable_Channel_COPY_LFX", "0"},
			{EpCapture, "Enable_MAGICVOICE", "0"},
			{EpCapture, "Enable_MICECHO", "0"},
		},
	},
	{
		Name: "电影",
		Desc: "电影场景 · 包围感（虚拟环绕 + 观众席环境声）",
		Items: []seedItem{
			{EpRender, "Enable_EQ_GFX", "1"},
			{EpRender, "EQ_Slider1", "5"}, {EpRender, "EQ_Slider2", "4"},
			{EpRender, "EQ_Slider3", "2"}, {EpRender, "EQ_Slider4", "0"},
			{EpRender, "EQ_Slider5", "-1"}, {EpRender, "EQ_Slider6", "0"},
			{EpRender, "EQ_Slider7", "1"}, {EpRender, "EQ_Slider8", "2"},
			{EpRender, "EQ_Slider9", "3"}, {EpRender, "EQ_Slider10", "4"},
			{EpRender, "Enable_SS_LFX", "1"},
			{EpRender, "Enable_RFX_GFX", "1"},
			{EpRender, "RFX_ENVIRONMENT", "7"},
			{EpRender, "Enable_VOCALCANCEL_GFX", "0"},
			{EpRender, "Enable_KEYSHIFT_GFX", "0"},
			{EpRender, "Enable_Channel_COPY_LFX", "0"},
			{EpCapture, "Enable_NOISEREDUCTION_LFX", "1"},
			{EpCapture, "Enable_MAGICVOICE", "0"},
			{EpCapture, "Enable_MICECHO", "0"},
		},
	},
}

// factoryDefaults 出厂默认值表 —— 设备侧自愈（HealMissingParams）的依据。
//
// 这 42 项（渲染 36 + 采集 6）属于「写后即可读」型：设备里没值时读回 ret=2
// （存在但无值），写过一次才变 ret=0 并跨进程持久。官方程序在位时，官方面板
// （安装时的 `/h /i` + 每次开机的 `/h /d`）会把它们维持在设备里；官方卸载器跑
// `XearAudioCenter /u /q` 会清掉设备侧的值，于是全部退回 ret=2，
// UI 按「本机驱动未开放」把开关灰掉 —— 看起来像「虚拟环绕 / 升降 Key 丢了」，
// 其实没丢，缺的只是值。
//
// 值取自官方安装状态的实测快照（2026-10-14 `--dump`，当时两端点合计
// ret=0 有 104 项）：设备 USB\VID_0D8C&PID_0012&MI_00 · 驱动 108B（WHQL）·
// 能力掩码 16801926 / 采集 164352。
// 与 seedScenes 一样硬编码：它必须不可篡改 —— 一旦允许被「存为新场景」
// 覆盖，自愈就会拿一份被改过的值去补设备。
var factoryDefaults = []seedItem{
	// SpeakerEQ —— 均衡器（11 项）
	{EpRender, "Enable_EQ_GFX", "1"},
	{EpRender, "EQ_Slider1", "0"},
	{EpRender, "EQ_Slider2", "7"},
	{EpRender, "EQ_Slider3", "-1"},
	{EpRender, "EQ_Slider4", "-7"},
	{EpRender, "EQ_Slider5", "-12"},
	{EpRender, "EQ_Slider6", "-8"},
	{EpRender, "EQ_Slider7", "0"},
	{EpRender, "EQ_Slider8", "10"},
	{EpRender, "EQ_Slider9", "4"},
	{EpRender, "EQ_Slider10", "-5"},
	// General —— Xear Surround 音箱方位（16 项）
	{EpRender, "SS_C_ANGLE", "0"},
	{EpRender, "SS_C_DIST", "0"},
	{EpRender, "SS_LF_ANGLE", "-32"},
	{EpRender, "SS_LF_DIST", "0"},
	{EpRender, "SS_RF_ANGLE", "32"},
	{EpRender, "SS_RF_DIST", "0"},
	{EpRender, "SS_LS_ANGLE", "-90"},
	{EpRender, "SS_LS_DIST", "0"},
	{EpRender, "SS_RS_ANGLE", "90"},
	{EpRender, "SS_RS_DIST", "0"},
	{EpRender, "SS_LB_ANGLE", "-148"},
	{EpRender, "SS_LB_DIST", "0"},
	{EpRender, "SS_RB_ANGLE", "148"},
	{EpRender, "SS_RB_DIST", "0"},
	{EpRender, "SS_LFE_ANGLE", "180"},
	{EpRender, "SS_LFE_DIST", "0"},
	// XearSurroundSpeakers —— 虚拟环绕总开关（1 项）
	{EpRender, "Enable_SS_LFX", "0"},
	// KeyShift —— 升降 Key（2 项）
	{EpRender, "Enable_KEYSHIFT_GFX", "0"},
	{EpRender, "KEYSHIFT_LEVEL", "0"},
	// Karaoke —— 人声消除（2 项）
	{EpRender, "Enable_VOCALCANCEL_GFX", "0"},
	{EpRender, "VOCALCANCEL_LEVEL", "50"},
	// Environment —— 环境音效（3 项）
	{EpRender, "Enable_RFX_GFX", "0"},
	{EpRender, "RFX_ENVIRONMENT", "7"},
	{EpRender, "RFX_ROOMSIZE", "0"},
	// ChannelCopy —— 声道复制（1 项）
	{EpRender, "Enable_Channel_COPY_LFX", "0"},
	// 采集端点（麦克风）—— 6 项
	// 官方卸载后同样退回 ret=2，界面把麦克风页的降噪 / 魔音 / 回音
	// 三个开关及两个滑杆一并灰掉。值取自官方在位时的采集端快照。
	{EpCapture, "Enable_NOISEREDUCTION_LFX", "0"},
	{EpCapture, "NoiseReduction_Level", "14"},
	{EpCapture, "Enable_MAGICVOICE", "0"},
	{EpCapture, "MagicVoice_Selection", "1"},
	{EpCapture, "Enable_MICECHO", "0"},
	{EpCapture, "MICECHO_Level", "0"},
}

// fullSnapshot 把场景快照补成全量（2026-10-19，回应「恢复后场景缺参数」）：
// 特征项之外，先按 factoryDefaults 补默认值，再用设备实读补齐剩余可写项
// （调用前必须已跑过 HealMissingParams，否则 ret=2 的项读不到值会缺参数）。
// 优先级：特征项 > 默认表 > 实读；同名键去重保序。
func fullSnapshot(d *Driver, base []snapRow) []snapRow {
	seen := map[string]bool{}
	out := make([]snapRow, 0, len(base)+64)
	for _, r := range base {
		k := r.Ep + "\x00" + r.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		if excludedFromScene(r.Name) {
			continue // base 直通项也要过排除名单（seed 表里可能带 # 通道项）
		}
		out = append(out, r)
	}
	add := func(ep, name, val string) {
		k := ep + "\x00" + name
		if seen[k] || excludedFromScene(name) {
			return
		}
		sp, known := spec(ep, name)
		if !known {
			return
		}
		seen[k] = true
		out = append(out, snapRow{ep, name, kindName(sp.kind), val})
	}
	for _, it := range factoryDefaults {
		add(it.Ep, it.Name, it.Val)
	}
	if d != nil && d.ready {
		for _, ep := range []string{EpRender, EpCapture} {
			names := make([]string, 0, len(writable[ep]))
			for n := range writable[ep] {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if r, v := d.ReadEp(ep, n); r == 0 {
					add(ep, n, v)
				}
			}
		}
	}
	return out
}

// HealMissingParams 设备侧自愈：把「存在但当前无值」（ret=2）的项补上出厂值。
//
// 这是官方行为的等价复刻 —— 官方靠安装时的 `/h /i` 与每次开机的 `/h /d`
// 维持这些值；新框架只读不写，值一旦被清（卸载官方程序、换 USB 口、
// 重装驱动都可能触发）就再也回不来，界面于是把开关灰成「本机驱动未开放」。
//
// 判据用 ret==2 而不是「值为空」：2 的语义正是「项存在、但设备侧还没有值」，
// 也就是用户从没配过的项。ret=0（已有值）一律跳过 —— 绝不覆盖用户当前设置；
// ret=-1（项不存在）也不动，那是硬件本身的结论。
//
// 返回实际补写的项数（供启动日志与自检）。
func HealMissingParams(d *Driver) int {
	if d == nil || !d.ready {
		return 0
	}
	healed := 0
	for _, it := range factoryDefaults {
		if ret, _ := d.ReadEp(it.Ep, it.Name); ret != 2 {
			continue
		}
		sp, known := spec(it.Ep, it.Name)
		kind := ""
		if known {
			kind = kindName(sp.kind)
		}
		if res := d.WriteAuto(it.Ep, it.Name, it.Val, kind); res["ok"] == true {
			healed++
		}
	}
	return healed
}

// reApoName 匹配 <ApoEffect Name="…"> 的“前缀 + 值 + 收尾引号”三段。
var reApoName = regexp.MustCompile(`(<ApoEffect\s+Name=")([^"]*)(")`)

// MigrateProfileGroups 把存量场景文件里的**旧 ApoEffect 名**就地换成官方
// Canvas 名（第 51 轮）。
//
// 为什么不能只靠 groupAlias：别名只在**我们读**的时候兜底 —— 文件本身还是
// 旧名，官方面板照旧读不出，用这些档的场景在官方面板里点了没反应。
//
// 改什么：只改 <ApoEffect Name="…"> 的**属性值**。Property 的名字/值/端点/
// 缩进/换行全部原样保留 —— 纯字符串替换，不重新序列化，所以不会动到设置本身。
//
// 安全性：命中才写；写前把原文件另存一份 <name>.profile.bak（每次覆盖，
// 只保留最近一次）。只读目录（装到 Program Files）写不进去就跳过。
//
// 返回 (改写的文件数, 改写的标签数)。与 Python 版 migrate_profile_groups 逐字段对应。
func MigrateProfileGroups(d *Driver) (int, int) {
	if d == nil {
		return 0, 0
	}
	files, tags := 0, 0
	for _, pair := range profileDirs(d) {
		dir, _ := pair[0].(string)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() ||
				!strings.HasSuffix(strings.ToLower(e.Name()), ".profile") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			txt := string(raw)
			n := 0
			out := reApoName.ReplaceAllStringFunc(txt, func(m string) string {
				sub := reApoName.FindStringSubmatch(m)
				ng, ok := groupAlias[sub[2]]
				if !ok {
					return m
				}
				n++
				return sub[1] + ng + sub[3]
			})
			if n == 0 || out == txt {
				continue
			}
			if err := os.WriteFile(p+".bak", raw, 0o644); err != nil {
				continue
			}
			if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
				continue
			}
			files++
			tags += n
		}
	}
	return files, tags
}

// EnsureSeedScenes 启动时补齐预置场景（幂等）：
//   - 游戏/音乐/电影：写目录缺失时按 seedScenes 生成；存量档项数不足时
//     升级重建（播种档 builtin，UI 禁止用户改，升级无害）
//   - 原厂默认：按 factoryDefaults 生成（存量档项数不足时同样升级重建）
//
// ★ 全量播种：特征项之外按「factoryDefaults → 设备实读」补齐其余可写参数
//
//	（fullSnapshot），场景档回到旧版 ProfileSave 的全量形态 —— 旧版小档
//	只有 18-42 项，用户对比「参数变少」。调用前必须已跑过 HealMissingParams。
//
// 驱动未就绪时跳过，等下次启动再补。
func EnsureSeedScenes(d *Driver) {
	if d == nil || !d.ready {
		return
	}
	wdir := profileWriteDir(d)
	if err := os.MkdirAll(wdir, 0o755); err != nil {
		return
	}
	for _, s := range seedScenes {
		p := filepath.Join(wdir, s.Name+".profile")
		var snap []snapRow
		okAll := true
		for _, it := range s.Items {
			if excludedFromScene(it.Name) {
				continue // seed 表也可能混进音量/采样率项，别带进场景档
			}
			sp, known := spec(it.Ep, it.Name)
			if !known {
				// 能力面与预期不符：宁可缺档也不写一份应用会失败的
				okAll = false
				break
			}
			snap = append(snap, snapRow{it.Ep, it.Name, kindName(sp.kind), it.Val})
		}
		if !okAll || len(snap) == 0 {
			continue
		}
		snap = fullSnapshot(d, snap)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			// 幂等：已有档就不动 —— 但项数不足（旧版小档）或混有排除项
			//（音量/采样率，见 sceneExclude）时升级重建。
			// 播种档 builtin，UI 禁止用户改，升级重建无害。
			old := profileItems(readText(p))
			if len(old) >= len(snap) && !hasExcludedItem(old) {
				os.WriteFile(filepath.Join(wdir, s.Name+".cml"),
					[]byte(buildCML(s.Name, s.Desc)), 0o644)
				continue
			}
		}
		if err := os.WriteFile(p, []byte(buildProfileXML(s.Name, snap)), 0o644); err != nil {
			continue
		}
		os.WriteFile(filepath.Join(wdir, s.Name+".cml"),
			[]byte(buildCML(s.Name, s.Desc)), 0o644)
	}
	// 原厂默认：直接落出厂值表，不再快照当前驱动状态。
	//
	// 这里原来是 `d.ProfileSave(profileFactoryScene, …)` —— 那是缺陷：它把
	// 「此刻设备上有什么」当成出厂默认存下来，设备侧没值时存下的就是空档，
	// 以后拿它恢复也恢复不回来（残留的几份同名档内容互不相同，就是这么来的）。
	// 改成与 HealMissingParams 同源的固定表，此后不再漂移。
	//
	// 档项数不足（旧版小档）时升级重建：播种档 builtin，UI 禁止用户改，安全。
	p := filepath.Join(wdir, profileFactoryScene+".profile")
	var fsnap []snapRow
	for _, it := range factoryDefaults {
		sp, known := spec(it.Ep, it.Name)
		if !known {
			continue // 本机能力面没这一项 —— 宁可缺也不写真会失败的
		}
		fsnap = append(fsnap, snapRow{it.Ep, it.Name, kindName(sp.kind), it.Val})
	}
	fsnap = fullSnapshot(d, fsnap)
	if len(fsnap) > 0 {
		needWrite := true
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			old := profileItems(readText(p))
			needWrite = len(old) < len(fsnap) || hasExcludedItem(old)
		}
		if needWrite {
			if err := os.WriteFile(p, []byte(buildProfileXML(profileFactoryScene, fsnap)), 0o644); err == nil {
				os.WriteFile(filepath.Join(wdir, profileFactoryScene+".cml"),
					[]byte(buildCML(profileFactoryScene, "出厂默认值 · 一键还原")), 0o644)
			}
		}
	}
}
