# -*- coding: utf-8 -*-
"""生成《原面板功能映射表》—— 零遗漏验收基线。

输入：
  tools/原版面板控件目录.json   官方面板 59 个 .cml 的控件树（含 Tag 值域）
  docs/属性ID表.md             osConfLib.dll 208 项属性名与 ID（参考）

输出：
  docs/原面板功能映射表.md

设计原则
--------
.cml 里的 Tag 存的是**值**（滑杆序号、单选按钮序号、声道掩码），
控件与驱动属性的绑定写在 XearAudioCenter_x64.exe 里，静态提取不全。
所以本表的「驱动属性」列是**已确认**的绑定（来自反汇编 + 实机往返验证 +
官方 lang 资源交叉印证），未确认的显式标为「未恢复」，不臆造。
"""
import io
import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(ROOT, "tools", "原版面板控件目录.json")
CAT = os.path.join(ROOT, "tools", "official_catalog.json")
OUT = os.path.join(ROOT, "docs", "原面板功能映射表.md")

# --------------------------------------------------------------------
# 官方页面 → 新面板落点 / 驱动属性
# 键是 .cml 文件名。每条 = (新面板位置, [驱动属性], 本机实测状态)
# --------------------------------------------------------------------
PAGE_MAP = {
    "MainWindow.cml": ("左侧导航 + 品牌区", [], "已复刻（窗口框架）"),
    "PanelProfile.cml": ("特效组态页", [], "已复刻（新增/删除组态）"),
    "PanelDeviceList.cml": ("设备页 · 设备信息", ["GetDeviceModel", "GetCodecVersion",
                                            "GetDriverVersion", "GetFirmwareVersion",
                                            "GetDeviceFormatString", "GetDeviceID",
                                            "GetSupportFeature", "GetDirectXVer"],
                            "已接通（只读）"),
    "PanelStatus.cml": ("右上读数区", ["GetPeakMeter"], "已接通（实时电平轮询）"),

    "apoSpeakerVol.cml": ("声音页 · 音量", ["VolumeControl", "VolumeScalarControl",
                                       "MuteControl"], "已接通（可写，已验因果）"),
    "apoSpeakerEQ.cml": ("均衡器页", ["Enable_EQ_GFX"] + ["EQ_Slider%d" % i for i in range(1, 11)],
                         "已接通（开关 + 10 段 + 12 档预设 + 自定义组态）"),
    "apoSpeakerHWEQ.cml": ("（官方页对照）", ["HWEQTrebleControl", "HWEQBassControl"],
                           "属性名未注册（ret=-1）· 真不可用"),
    "apoSpeakerEM.cml": ("声音页 · 环境", ["Enable_RFX_GFX", "RFX_ENVIRONMENT", "RFX_ROOMSIZE"],
                         "已接通（28 档环境，已验写入）"),
    "apoSpeakerSetting.cml": ("设备页 · 声道测试 / 声音页 · 音箱方位",
                              ["SpeakerShifterControl"] + ["SS_%s_ANGLE" % s for s in
                               ("C", "LF", "RF", "LS", "RS", "LB", "RB", "LFE")],
                              "音箱方位 8 项已接通（写后即可读，已验往返）；"
                              "PlayTestSound 已接通（★ 第 28 轮：第一个参数是测试音 WAV 路径）"),
    "apoSpeakerSamplingRate.cml": ("设备页 · 采样率", ["DevSampleRateControl",
                                                "DevSampleRateExControl", "DevFormatControl",
                                                "DevBitDepthControl", "DevChannelNumControl"],
                                   "已接通 44.1/48 kHz；位深/声道/掩码本机只读"),
    "apoSpeakerAGC.cml": ("麦克风页 · 增强", ["AGCControl"],
                          "渲染端点 ret=-1，采集端点可写（已改打采集端点）"),
    "apoSpeakerKaraoke.cml": ("声音页 · 人声消除", ["Enable_VOCALCANCEL_GFX",
                                              "VOCALCANCEL_LEVEL"], "已接通（可写）"),
    "apoSpeakerXearSmartVolume.cml": ("（官方页对照）", ["Enable_SMART_VOLUME_GFX"],
                                      "可写 · 效果未验证"),
    "apoSpeakerXearAudioBrilliant.cml": ("声音页 · 音频增艳",
                                         ["Enable_AUDIOBRILLIANT_LFX",
                                          "AUDIOBRILLIANT_LEVEL"],
                                         "已接通开关 · 效果未验证"),
    "apoSpeakerXearVoiceClarity.cml": ("（官方页对照）",
                                       ["Enable_VOICECLARITY_LFX",
                                        "VOICECLARITY_LEVEL"], "可写 · 效果未验证"),
    "apoSpeakerXearDynamicBass.cml": ("（官方页对照）",
                                      ["Enable_BM_GFX", "BM_Slider_Frequency",
                                       "BM_Slider_BassLevel", "BM_LargeSpeaker"],
                                      "可写 · 效果未验证"),
    "apoSpeakerFlexBass.cml": ("（官方页对照）", ["BM_Slider_BassLevel", "Enable_BM_GFX"],
                               "可写 · 效果未验证"),
    "apoSpeakerVirtual.cml": ("（官方页对照）", ["Enable_DHDVS_LFX", "DHDVS_USEDH",
                                            "DH_Mode", "DVS_Mode"], "可写 · 效果未验证"),
    "apoSpeakerVirtualV2.cml": ("（官方页对照）", ["Enable_DHDVS_LFX", "DVS_Mode"],
                                "可写 · 效果未验证"),
    "apoSpeakerXearSurroundSpeakers.cml": ("声音页 · 虚拟环绕 + 音箱方位",
                                           ["Enable_SS_LFX", "SpeakerShifterControl"]
                                           + ["SS_%s_ANGLE" % s for s in
                                              ("C", "LF", "RF", "LS", "RS", "LB", "RB", "LFE")]
                                           + ["SS_%s_DIST" % s for s in
                                              ("C", "LF", "RF", "LS", "RS", "LB", "RB", "LFE")],
                                           "已接通（开关 + 拖动音箱写回方位，已验往返）"),
    "apoSpeakerXearSurroundHeadPhone.cml": ("（官方页对照）",
                                            ["Enable_CONLAB_HPSPK_LFX",
                                             "CONLAB_HPSPK_USEHP",
                                             "CONLAB_HPSPK_ROOMSIZE",
                                             "CONLAB_HPSPK_SPKANGLE"], "可写 · 效果未验证"),
    "apoSpeakerVirtualXearSurroundHeadPhone.cml": ("（官方页对照）",
                                                   ["Enable_CONLAB_HPSPK_LFX",
                                                    "CONLAB_HPSPK_SFX",
                                                    "CONLAB_HPSPK_MODE"], "可写 · 效果未验证"),
    "apoSpeakerSurroundMax.cml": ("（官方页对照）", ["Enable_SURRMAX_FOR_HPSPK",
                                               "SURRSENS_CONTENTTYPE",
                                               "SURRSENS_EXPFACTOR",
                                               "SURRSENS_SPEAKERANGLE"], "可写 · 效果未验证"),
    "apoSpeakerDolbyPLIIx.cml": ("（官方页对照）", ["Enable_PLIIX_LFX", "PLIIX_MODE",
                                               "PLIIX_SLIDER_DIMENSION",
                                               "PLIIX_SLIDER_CENTERWIDTH",
                                               "PLIIX_PANORAMA"], "可写 · 效果未验证"),
    "apoSpeakerDolbyHPVS.cml": ("（官方页对照）", ["Enable_DolbyHPVS_LFX",
                                             "DolbyHPVSControl"], "属性名未注册（ret=-1）· 真不可用"),
    "apoSpeakerDolbyHPV2.cml": ("（官方页对照）", ["Enable_DolbyHPV2_LFX",
                                             "DolbyHPV2EffectControl"],
                                "属性名未注册（ret=-1）· 真不可用"),
    "apoSpeakerDTSSS.cml": ("（官方页对照）", ["Enable_DTSSS_LFX", "DTSSSControl"],
                            "属性名未注册（ret=-1）· 真不可用"),
    "apoSpeakerDTSNeoPC.cml": ("（官方页对照）", ["Enable_NEOPC_LFX", "NEOPC_MODE",
                                             "NEOPC_SLIDER_DIMENSION",
                                             "NEOPC_SLIDER_CENTERGAIN"],
                               "可写 · 效果未验证"),
    "apoSpeakerXearAudioBrilliant2.cml": ("（官方页对照）", ["Enable_AUDIOBRILLIANT_LFX"],
                                          "已接通开关 · 效果未验证"),
    "apoSpeakerAGC2.cml": ("麦克风页 · 增强", ["AGCControl"], "采集端点可写"),

    "apoMicInVol.cml": ("麦克风页 · 音量", ["VolumeControl", "VolumeScalarControl",
                                       "MuteControl"], "已接通（采集端点，已验往返）"),
    "apoMicInNoiseReduction.cml": ("麦克风页 · 降噪", ["Enable_NOISEREDUCTION_LFX",
                                                 "NoiseReduction_Level"],
                                   "已接通（强度 8..20，已验往返）"),
    "apoMicInXearMagicVoice.cml": ("麦克风页 · 魔音", ["Enable_MAGICVOICE",
                                                 "MagicVoice_Selection"],
                                   "已接通（怪兽/卡通/男人/女人）"),
    "apoMicInKaraoke.cml": ("麦克风页 · 回音", ["Enable_MICECHO", "MICECHO_Level"],
                            "已接通（−12..+12 dB）"),
    "apoMicInAGC.cml": ("麦克风页 · 增强", ["AGCControl"], "已接通（Microphone Boost）"),
    "apoMicInBoost.cml": ("麦克风页 · 模拟功放", ["AAVolumeControl", "AAMuteControl",
                                            "GetAAMaxVol", "GetAAMinVol", "GetAAVolStep"],
                          "AAVolumeControl 已接通（−23..+8 dB）；其余只读"),
    "apoMicInSamplingRate.cml": ("麦克风页 · 采样率", ["DevSampleRateControl"],
                                 "已接通（44.1/48 kHz）"),
    "apoMicInAEC.cml": ("（官方页对照）", ["Enable_AEC_LFX"], "可写（采集端点）· 效果未验证"),
    "apoMicInENC.cml": ("（官方页对照）", ["Enable_ENC_LFX"], "可写（采集端点）· 效果未验证"),
    "apoMicInBeamForming.cml": ("（官方页对照）", ["Enable_MICARRAY_LFX",
                                              "MICARRAY_BEAMWIDTH"],
                                "可写（采集端点）· 效果未验证"),

    "MenuSpeakers.cml": ("（设备类型菜单）", [], "已复刻（本机为 Speakers 端点）"),
    "MenuHeadset.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuHeadPhone.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuSpeakersQuarter.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuMicIn.cml": ("（设备类型菜单）", [], "已复刻（本机有采集端点）"),
    "MenuDesktopMic.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuLineIn.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuSPDIFIn.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuSPDIFOut.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuStereoMixer.cml": ("（设备类型菜单）", [], "本机未插入该形态设备"),
    "MenuTextBox.cml": ("（设备类型菜单）", [], "辅助控件"),

    "Dialog_AddProFile.cml": ("特效组态页 · 新增表单", [], "已复刻"),
    "Dialog_DelProFile.cml": ("特效组态页 · 删除", [], "已复刻"),
    "Dialog_AppSetting.cml": ("（应用设置）", [], "未复刻（官方为皮肤/语言设置）"),
    "Dialog_WelcomeMsg.cml": ("（首次运行提示）", [], "不复刻（安装引导）"),
    "DialogInfo.cml": ("（信息对话框）", [], "不复刻（外壳）"),
    "DialogInfo_DetailWindow.cml": ("（信息对话框）", [], "不复刻（外壳）"),
    "DialogInfo_Legal.cml": ("（法律声明）", [], "不复刻（外壳）"),
    "DialogMessageBox.cml": ("（消息框）", [], "不复刻（外壳）"),
    "SystemTrayLClick.cml": ("（托盘菜单）", [], "不复刻（外壳）"),
    "SystemTrayRClick.cml": ("（托盘菜单）", [], "不复刻（外壳）"),
    "DesignDictResource.cml": ("（资源字典索引）", [], "不复刻（非功能页）"),
}

# 控件分类（哪些节点算「交互控件」、哪些页是外壳）只在 tools/panel_catalog.py
# 里定义一次，本表通过 official_catalog.json 复用，避免出现第二套口径。


def main():
    d = json.load(io.open(SRC, encoding="utf-8"))
    panels = d["panels"]
    cat = json.load(io.open(CAT, encoding="utf-8"))
    cpages = {p["file"]: p for p in cat["pages"]}

    rows, npages, nctrl = [], 0, 0
    unmapped_pages = []
    for pn in sorted(panels.keys()):
        cp = cpages.get(pn) or {}
        # 控件数一律取自 official_catalog.json（与界面「官方面板」页同源），
        # 保证验收基线与界面展示永远一致，不会出现两套口径。
        n = len(cp.get("controls") or [])
        kind = cp.get("kind") or ""
        if n == 0 and kind in ("外壳", "对话框"):
            continue
        info = PAGE_MAP.get(pn)
        if info is None:
            unmapped_pages.append(pn)
            info = ("（未登记）", [], "待核对")
        where, props, status = info
        npages += 1
        rows.append((pn, n, where, props, status))

    total_ctrl = sum(r[1] for r in rows)
    mapped = sum(r[1] for r in rows if r[3])
    # ★ 2026-10-16 更正：判据换成**写探测**结果，不再拿只读 ret=2 当「不支持」。
    #   ret=2 = 属性存在但从没被写过，写一次即 ret=0 且跨进程持久（实测 64/73 可写）。
    #   真正不可用的只有 ret=-1（属性名在驱动里根本没注册）。
    unavail = sum(r[1] for r in rows if "ret=-1" in r[4] or "未插入" in r[4])
    unverif = sum(r[1] for r in rows if "效果未验证" in r[4])

    L = []
    A = L.append
    A("# 原面板功能映射表 · 零遗漏验收基线")
    A("")
    A("本表回答一个问题：**官方面板有的东西，新面板有没有遗漏。**")
    A("")
    A("数据来源")
    A("")
    A("- `CPL/Skin/FaceLift/*.cml` —— 官方面板 59 个页面的控件树（位置 / 类型 / Tag 值域）")
    A("- `tools/official_catalog.json` —— 界面「官方面板」页同源目录（控件数以此为准）")
    A("- `CPL/lang/zh-CN/*.xaml` —— 892 条官方中文文案（`ml_*` 键）")
    A("- `CPL/profile/*.profile` `*.cml`、`CPL/FactoryDefault.xml` —— 官方特效组态格式")
    A("- `osConfLib.dll` 静态反汇编 + 本机 208 项逐项实测（见 `属性ID表.md`）")
    A("")
    A("一个必须说明的事实：`.cml` 里的 `Tag` 存的是**值**（滑杆序号、单选按钮序号、")
    A("声道掩码），控件与驱动属性的绑定写在 `XearAudioCenter_x64.exe` 内部，")
    A("静态提取不全。所以「驱动属性」列只写**已确认**的绑定")
    A("（反汇编 + 实机往返 + 官方 lang 资源三方交叉印证），")
    A("未确认的不臆造，显式标为「未恢复」。")
    A("")
    A("**页面名对照（以设计稿 `design/audio-center.html` 为准）**")
    A("")
    A("- 声音（第 31 轮前叫「声场」）· 均衡器（前「均衡」）· 麦克风")
    A("- 律动空间 · 自定义场景 · 设备")
    A("- 参数（前「全部参数」，再早叫「高级参数」）")
    A("")
    A("下表「新面板位置」列用的就是这七个名字。")
    A("")
    A("---")
    A("")
    A("## 一、覆盖总览")
    A("")
    A("| 指标 | 数值 |")
    A("|---|---|")
    A("| 官方页面（含外壳对话框） | %d |" % len(panels))
    A("| 逐页列出（含外壳对照） | %d |" % npages)
    A("| 交互控件（与界面同源） | %d |" % total_ctrl)
    A("| 已登记驱动属性绑定的控件 | %d |" % mapped)
    A("| 属性可写、效果未验证的控件 | %d |" % unverif)
    A("| 属性名未注册 / 本机未插入的控件 | %d |" % unavail)
    A("| 未登记页面 | %d |" % len(unmapped_pages))
    A("")
    A("新面板的复刻方式分四档：")
    A("")
    A("1. **已接通** —— 官方控件在新面板有对应控件，直接写驱动，实机往返验证过。")
    A("2. **可写 · 效果未验证** —— 属性存在且写一次即 `ret=0`（写探测实证），")
    A("   但主机侧回环 A/B 测不到音频变化，无法判断是设备侧处理还是真的无效果。")
    A("   界面上如实标注「未验证」，不置灰、不写「本机不支持」。")
    A("3. **真不可用** —— 只读 `ret=-1`，属性名在驱动里根本没注册（Dolby/DTS 授权项、")
    A("   硬件 EQ）。这一类才可以说「本机没有」。")
    A("4. **不复刻** —— 仅限窗口外壳、安装引导、法律声明、托盘菜单，无音频功能。")
    A("")
    A("> ★ 2026-10-16 更正：「本机 ret=2（不支持）」这一判据**整列作废**。")
    A("> 只读探测返回 `ret=2` 的含义是「属性存在、从没被赋过值」，不是「本机不支持」。")
    A("> 写探测（读 → 写 → 再读）实测：候选 73 项中 **64 项可写**，只有 9 项 `ret=-1`。")
    A("> 详见 `docs/功能移植性复审-写探测.md`。")
    A("")
    A("---")
    A("")
    A("## 二、逐页映射")
    A("")
    A("| 官方页面 | 交互控件 | 新面板落点 | 驱动属性 | 本机状态 |")
    A("|---|---:|---|---|---|")
    for pn, n, where, props, status in rows:
        pl = "、".join("`%s`" % p for p in props) if props else "—"
        if len(pl) > 190:
            pl = pl[:187] + "…"
        A("| `%s` | %d | %s | %s | %s |" % (pn, n, where, pl, status))
    A("")
    if unmapped_pages:
        A("未登记页面：" + "、".join("`%s`" % p for p in unmapped_pages))
        A("")

    A("---")
    A("")
    A("## 三、新面板控件 → 驱动属性（正向，全部实机验证）")
    A("")
    A("### 渲染端点（扬声器）· 41 项可写")
    A("")
    A("| 新面板控件 | 驱动属性 | 类型 | 值域 | 实测 |")
    A("|---|---|---|---|---|")
    rend = [
        ("声音页 · 主音量", "VolumeControl", "float", "−37.0 … 0.0 dB", "写后读回一致"),
        ("声音页 · 标量音量", "VolumeScalarControl", "float", "0 … 1", "写后读回一致"),
        ("声音页 · 静音", "MuteControl", "bool", "0/1", "写后读回一致"),
        ("声音页 · 环境开关", "Enable_RFX_GFX", "bool", "0/1", "写后读回一致"),
        ("声音页 · 环境类型", "RFX_ENVIRONMENT", "int", "0 … 27", "写后读回一致"),
        ("声音页 · 房间大小", "RFX_ROOMSIZE", "int", "0 … 2", "写后读回一致"),
        ("声音页 · 虚拟环绕", "Enable_SS_LFX", "bool", "0/1", "写后读回一致"),
        ("均衡器页 · 均衡开关", "Enable_EQ_GFX", "bool", "0/1", "写后读回一致"),
        ("均衡器页 · 10 段频点", "EQ_Slider1…10", "int", "−20 … +20 dB", "写后读回一致"),
        ("声音页 · 人声消除开关", "Enable_VOCALCANCEL_GFX", "bool", "0/1", "写后读回一致"),
        ("声音页 · 人声消除强度", "VOCALCANCEL_LEVEL", "int", "0 … 100", "写后读回一致"),
        ("声音页 · 变调开关", "Enable_KEYSHIFT_GFX", "bool", "0/1", "写后读回一致"),
        ("声音页 · Key 档位", "KEYSHIFT_LEVEL", "int", "−4 … +4", "写后读回一致"),
        ("声音页 · 智能音量(GFS)", "GFSControl", "int", "0/1", "注册表开关，本机无消费者"),
        ("设备页 · 输出采样率", "DevSampleRateControl", "int", "44100 / 48000", "写入持久，其余值被拒"),
        ("设备页 · 声道复制", "Enable_Channel_COPY_LFX", "bool", "0/1", "写后读回一致"),
        ("声音页 · 拖动音箱（8 个方位）",
         "SS_C/LF/RF/LS/RS/LB/RB/LFE_ANGLE", "int", "−180 … +180（驱动不校验）",
         "写后读回一致，跨进程持久；写 −32 读回 −32"),
        ("声音页 · 音箱距离（8 个）",
         "SS_C/LF/RF/LS/RS/LB/RB/LFE_DIST", "int", "单位未确认（不钳制）",
         "写后读回一致，跨进程持久"),
    ]
    for a, b, c, e, f in rend:
        A("| %s | `%s` | %s | %s | %s |" % (a, b, c, e, f))
    A("")
    A("### 采集端点（麦克风）· 12 项可写")
    A("")
    A("| 新面板控件 | 驱动属性 | 类型 | 值域 | 实测 |")
    A("|---|---|---|---|---|")
    cap = [
        ("麦克风页 · 输入音量", "VolumeControl", "float", "−12 … +23 dB", "写后读回一致"),
        ("麦克风页 · 标量音量", "VolumeScalarControl", "float", "0 … 1", "写后读回一致"),
        ("麦克风页 · 静音", "MuteControl", "bool", "0/1", "写后读回一致"),
        ("麦克风页 · 降噪开关", "Enable_NOISEREDUCTION_LFX", "bool", "0/1", "0↔1 往返通过"),
        ("麦克风页 · 降噪强度", "NoiseReduction_Level", "int", "8 … 20", "写后读回一致"),
        ("麦克风页 · 魔音开关", "Enable_MAGICVOICE", "bool", "0/1", "写后读回一致"),
        ("麦克风页 · 魔音类型", "MagicVoice_Selection", "int", "0 … 3", "怪兽/卡通/男人/女人"),
        ("麦克风页 · 回音开关", "Enable_MICECHO", "bool", "0/1", "写后读回一致"),
        ("麦克风页 · 回音电平", "MICECHO_Level", "int", "−12 … +12", "写后读回一致"),
        ("麦克风页 · 麦克风增强", "AGCControl", "bool", "0/1", "写后读回一致"),
        ("麦克风页 · 模拟功放音量", "AAVolumeControl", "float", "−23 … +8 dB", "写后读回一致"),
        ("麦克风页 · 采集采样率", "DevSampleRateControl", "int", "44100 / 48000", "写入持久"),
    ]
    for a, b, c, e, f in cap:
        A("| %s | `%s` | %s | %s | %s |" % (a, b, c, e, f))
    A("")
    A("---")
    A("")
    A("## 四、函数型导出（非属性）")
    A("")
    A("`osConfLib.dll` 的 15 个导出里有 6 个是直接调用而非属性，")
    A("原面板用它们做声道测试与麦克风监听。ABI 由反汇编还原：")
    A("")
    A("| 导出 | 真实签名 | 新面板接线 | 本机实测 |")
    A("|---|---|---|---|")
    A("| `PlayTestSound` | `(LPWSTR wavPath, uint mode, int *pInfo)` —— "
      "**第一个参数是测试音 WAV 的绝对路径**，不是设备上下文；"
      "`mode` 是声道掩码；`pInfo[0]`=链表索引、`pInfo[8]`=链表号 | "
      "设备页 · 8 个声道测试芯片 | `0`（成功，8 个声道全部） |")
    A("| `StopTestSound` | `()` 忽略入参，用模块内单例 | 设备页 · 停止 | `0`（成功） |")
    A("| `CloseTestSound` | `()` 同上 | 设备页 · 关闭 | `0`（成功） |")
    A("| `RunListen2Device` | `(pDevice)`，要求 `pDevice+4 ∈ {2,3,5,0xA}` | "
      "麦克风页 · 监听开关 | `0x80040014`（类未注册） |")
    A("| `StopListen2Device` | `(pDevice)` 同上 | 麦克风页 · 监听开关 | `0x8007000E`（内存不足） |")
    A("| `ShowAsioPanel` | `(pDevice)` | 设备页 · ASIO 按钮 | `−1000`（无 ASIO） |")
    A("")
    A("**早期实现的错误**：`PlayTestSound` 曾按 2 个参数调用，第三个寄存器 `R8` 是垃圾值，")
    A("函数读 `[R8]` 触发访问违例。Python 的 ctypes 把它转成可捕获的 `OSError`，")
    A("于是被判成「本机不支持」；Go 的 syscall 会直接杀进程。")
    A("")
    A("★ 第 28 轮更正：按 3 参数调用后不再崩溃，但**恒返 `0x10001`**，当时把它读成")
    A("「设备能力拒绝」，据此得出「本机不支持声道测试」—— **那个结论是错的**。")
    A("反编译 `PlayTestSound@0x1800060E0` 后看清：第一个参数是 `LPWSTR`，直接进")
    A("`FUN_180008fe0`，而那函数就是 MSDN 的 `WaveOpenFile`（`mmioOpenW` → 校验")
    A("`'WAVE'`/`'fmt '`/`'data'` → `CreateFileW`）；拿不到文件句柄就返回")
    A("`0xffffffff`，上层即 `return 0x10001`。")
    A("")
    A("⟹ **`0x10001` = 打开测试音 WAV 失败**。修法是第一个参数传")
    A("`<CPL>/sound/<声道>.wav` 的绝对路径（8 个声道各有对应文件），")
    A("`argtypes` 用 `c_char_p` 的宽字符版。改完 8 个声道全部返回 `0`。")
    A("")
    A("---")
    A("")
    A("## 五、官方面板有、新面板没有的（逐条说明）")
    A("")
    A("| 项目 | 原因 |")
    A("|---|---|")
    A("| 皮肤 / 语言 / 主题设置对话框 | 新面板自带深浅色主题与中文界面，无皮肤概念 |")
    A("| 安装引导、法律声明、消息框 | 窗口外壳，无音频功能 |")
    A("| 托盘右键菜单 | 外壳；新面板为普通窗口 |")
    A("| 硬件 EQ（`HWEQTrebleControl` / `HWEQBassControl`） | 本机驱动 `ret=-1`，"
      "官方面板在这台机器上同样是死的 |")
    A("| 音箱方位拖拽（`SS_*_ANGLE`） | **已更正：不再是缺口。** 详见下方「★ 写后即可读」 |")
    A("")
    A("> 原表中「官方 EQ 预设的精确曲线」一行**已删除** —— 该结论（「曲线提取不到、"
      "按语义拟合」）于 2026-10-14 被推翻。官方 12×10 精确曲线已从 "
      "`XearAudioCenter_x64.exe` 静态提取，见下方「★ 官方 EQ 曲线」。")
    A("")
    A("### ★ 「写后即可读」—— 一次结论更正（2026-10-14）")
    A("")
    A("`SS_*_ANGLE` / `SS_*_DIST` 这 16 项此前被判「本机不支持」，"
      "依据是只读遍历时返回 `ret=2`。**那个结论是错的。**")
    A("")
    A("实测：这些属性属于「写后即可读」—— 没写过时读返回 2（存在但无值），"
      "**写一次就变成可读（ret=0）且跨进程持久**；驱动不做值域校验，"
      "负数照收（写 `SS_LF_ANGLE=-32` 读回 −32，写 `SS_C_ANGLE=200` 也读回 200）。")
    A("")
    A("```")
    A("# 复现：写之前 ret=2，写一次之后 ret=0 且新进程复读一致")
    A("POST /api/set {name:'SS_LS_ANGLE', value:12, kind:'int'}   -> ok, 读回 12")
    A("```")
    A("")
    A("因此**只读探测不足以判定「本机不支持」**：凡是官方面板里出现、"
      "而驱动只读返回 2 的属性，都必须真写一次再看结论。"
      "新面板已据此改造：拖动音箱会真写 `SS_*_ANGLE`（写回值即画面上的角度，"
      "负数保留），被拒才显示驱动返回码。")
    A("")
    A("本机 208 项的计数因此变化：可读 50 → 66，本机不支持 105 → 89，"
      "可写 37 → 53（渲染端点 25 → 41）。")
    A("")
    A("### ★ 官方 EQ 曲线 —— 二次结论更正（2026-10-14）")
    A("")
    A("此前判定「exe 内无 12×10 表、曲线只能拟合」是**扫描策略的误判**："
      "早期只按元素宽度 1/2/4 字节在 `.rdata/.data/.text` 里找「连续且形状像 EQ 的数组」，"
      "而真实表混在大量常量池里、且行首若干值为 0，特征不明显，被噪声淹没。")
    A("")
    A("本轮改用**「先定位消费函数、再反查表地址」**的路线（Ghidra headless 批处理），一步锁定：")
    A("")
    A("```")
    A("FUN_1400295d0  = CSpeakerEQHandler::ApplyPreset")
    A("  if (param_2 < 0xc)")
    A("      value = (&DAT_14018a090)[ band + param_2 * 10 ]   // 行主序，行=预设，列=频段")
    A("```")
    A("")
    A("- **表地址 `0x14018a090`**，**12 行 × 10 列 float32**（`presetIndex*10 + bandIndex`）。")
    A("- **行号 ↔ 预设名**由 EQ 页命令分发器 `FUN_140025560` 闭环确认：它持有数组 "
      "`0x140211f80`（步长 `0x28`，成员是 `std::wstring` 控件名），逐项与命中的控件名比对，"
      "**命中位置即预设索引**；`FUN_140028540` 再用 `*(int *)(param_1 + 0xfc) == index` "
      "设置选中态，交叉验证。")
    A("- **列 ↔ 频点**：官方 10 滑杆 `EQ_Slider1..10`（属性 ID `0x0001..0x000A`）对应频点 "
      "`30 / 60 / 120 / 250 / 500 / 1k / 2k / 4k / 8k / 16k Hz`"
      "（**注意官方是 30/60/120，此前误记为 31/62/125**）。")
    A("- **形状自洽校验**：索引 7=高音（强提高频）、8=低音（强提低频）、11=预设（全 0）、"
      "5=摇滚（V 型）、10=声乐（中频 +14），与听感预期一致。")
    A("")
    A("提取结果（单位 dB，官方真值）已落入 `design/audio-center.html` 的 "
      "`HZ` 与 `PRE` 两个常量，并经 `tools/build_center.py` / "
      "`iKF-Audio-Center-Wails/tools/build.py` 同步进两条交付线。")
    A("")
    A("```")
    A("索引 0  Dance    [  5,  2,  0,-10, -5,  0,  6, 12, 11, 10]")
    A("索引 1  Rap      [-13, -9, -4,  2,  8, 12,  2,-11, -2,  8]")
    A("索引 2  Metal    [ 12,  7, -3,  4, 13,  8,  3, -3,  8, 12]")
    A("索引 3  Jazz     [ -8,  6,  0,  8, -8, 10, -2, 13,  8,  1]")
    A("索引 4  SoftRock [  5,  5,  2, -2, -8,-12, -4,  0,  7,  9]")
    A("索引 5  Rock     [  6,  3, -8,-12, -4,  3,  8, 10, 10,  7]")
    A("索引 6  Live     [ -6,  0,  4,  8,  9,  9,  5,  3,  2,  1]")
    A("索引 7  Treble   [-10,-11,-12, -6,  2,  8, 13, 13, 11, 14]")
    A("索引 8  Bass     [ 11,  7,  3,  0,  0, -6,-10,-10, -2, -2]")
    A("索引 9  Classic  [  0,  7, -1, -7,-12, -8,  0, 10,  4, -5]")
    A("索引 10 Vocal    [-13, -8,  0,  6, 14,  4, -4, -7, -8,-10]")
    A("索引 11 Default  [  0,  0,  0,  0,  0,  0,  0,  0,  0,  0]")
    A("```")
    A("")
    A("**工具**：`C:\\ikf-re\\scripts\\DumpRange.java`（"
      "`DumpRange.java <hexAddr> <byteLen> <outFile>`，按 f32/i32/u8 解释内存）、"
      "`Hunt.java`（字符串→引用→所属函数→反编译一次跑完）、`Callers.java`。"
      "脚本在 Ghidra headless 里以 `-noanalysis -postScript` 复用 "
      "`C:\\ikf-re\\proj\\ikf` 已分析工程，秒级返回。")
    A("")
    A("")
    A("---")
    A("")
    A("## 六、验收方法")
    A("")
    A("```bash")
    A("# 1. 两条交付线逐行一致（自检 + 双端点全量 dump）")
    A("diff <(python iKF-Audio-Center.py --self-test) <(iKF-Audio-Center.exe --selftest)")
    A("diff <(python iKF-Audio-Center.py --dump)          <(iKF-Audio-Center.exe --dump)")
    A("diff <(python iKF-Audio-Center.py --dump capture)  <(iKF-Audio-Center.exe --dump capture)")
    A("")
    A("# 2. 界面回归：真实 Chrome + CDP 派发受信任事件")
    A("iKF-Audio-Center.exe --serve :17878")
    A("node .workbuddy-ai/tmp/verify_r5.js <CDP端口> http://127.0.0.1:17878/ <截图目录>")
    A("```")
    A("")
    A("`verify_r5.js` 覆盖：麦克风页（采集端点）· 设备页扩展 · 特效组态页")
    A("（新增 → 应用 → 删除端到端）· 均衡器页 12 档预设 · 参数表端点切换 ·")
    A("**声音页真拖音箱 → `SS_*_ANGLE` 写回驱动（受信任事件，事后自动还原）** ·")
    A("7 分组 × 6 档窗口的布局避让。")
    A("")

    io.open(OUT, "w", encoding="utf-8", newline="\n").write("\n".join(L) + "\n")
    print("已生成：%s" % OUT)
    print("  功能页 %d 个，交互控件 %d 个，已登记绑定 %d 个"
          % (npages, total_ctrl, mapped))
    if unmapped_pages:
        print("  未登记页面:", unmapped_pages)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
