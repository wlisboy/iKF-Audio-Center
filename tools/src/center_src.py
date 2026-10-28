#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
iKF Audio Center —— 单文件驱动控制面板
====================================================================

一个文件、零第三方依赖。用 ctypes 直接调用驱动自带的 osConfLib.dll，
把参数读写的真实状态渲染到界面上。

驱动通道
--------
osConfLib.dll 导出 15 个 extern "C" 函数，其中 PropertyControl 是统一参数入口：

    int PropertyControl(void* pDevice, const wchar_t* pPropName,
                        void** ppBuffer, void* pValue, BOOL bSet);

实机还原出的三条关键约定（静态反汇编 + 本机往返验证）：

  1. pPropName 是 **UTF-16 宽字符串**（不是 ANSI）。函数内部按 16 位逐字比较，
     传 ANSI 会一律返回 -1。
  2. 读取时结果以 **UTF-16 字符串** 写进 *ppBuffer（固定 1024 字节，函数先清零）。
  3. 写入时函数从 ***ppBuffer 的首个 dword** 取值，pValue 不参与通用属性；
     只有 OEM 属性（第二张表）会把 pValue 当二级指针解引用，
     *pValue 指向一个 int32 声道索引（-1 = 主音量）。

属性名分两张表：通用 112 项 + OEM 96 项 = 208 项。

返回码
------
    0        成功
    1        列表为空
    2        属性存在，但本设备未开放
   -1        名称查表失败 / 该操作不支持
  0x10001   设备上下文无效 / 打开测试音文件失败
  0x10003    写入被拒绝
  0x10004    读取失败

用法
----
    python iKF-Audio-Center.py              启动界面（自动打开浏览器）
    python iKF-Audio-Center.py --no-browser  只起服务，不打开浏览器
    python iKF-Audio-Center.py --port 17900  指定端口
    python iKF-Audio-Center.py --self-test   只做接口自检，打印诊断报告
    python iKF-Audio-Center.py --channel-test <声道>
                                             在指定声道播测试音（不发出声音的
                                             资源检查走 --self-test）；<声道>
                                             可为 LF/RF/C/W/LB/RB/LS/RS、
                                             中文名，或 all（逐个放一遍）
    python iKF-Audio-Center.py --dump        打印渲染端点全部 208 项实测值
    python iKF-Audio-Center.py --dump capture  打印采集端点（麦克风）208 项
    python iKF-Audio-Center.py --loopback-devices
                                             列出可锁定的播放端点
    python iKF-Audio-Center.py --loopback-fft  频谱计算自检（设备无关）
    python iKF-Audio-Center.py --loopback-test [秒] [--wav 文件] [--tone]
                             [--device ID|子串]  系统回环采集自检
    python iKF-Audio-Center.py --play <wav> [--secs N] [--loop]
                                             只播一段 wav 当信号源

可视化页的音源
--------------
「律动空间」页默认用**后端系统回环**（WASAPI loopback），不弹任何选择框；
启动时**自动锁定耳机端点** —— 匹配顺序：端点名含 iKF → 含 C-Media → 回落
默认播放设备（见设计稿的 pickHeadset）。之所以不跟随默认：本机装了 FxSound，
默认端点是 FxSound Speakers，跟着默认就会采到别路的声音。每个输出端点都有
独立的回环流，锁定耳机后音箱上放什么都进不来。
拿不到设备时可切到浏览器的屏幕捕获兜底。
两条路的频谱都与 Chromium 的 AnalyserNode **逐字节对齐**，
实现在 Loopback 类里，与 Wails 线（iKF-Audio-Center-Wails/loopback.go）一一对应。

设备端点
--------
GetDeviceCount 有两条互相独立的链表，两套上下文的属性可读性完全不同：

    type=0 → 渲染端点（扬声器）  GetSupportFeature = 16801926
    type=1 → 采集端点（麦克风）  GetSupportFeature =   164352

麦克风侧参数（降噪 / 魔音 / 回音 / AGC / 模拟功放音量）**只在采集端点上存在**，
打在渲染端点上一律返回 2。HTTP 层用 ?ep=render|capture 区分，
写入用 {"name":…, "value":…, "ep":"capture"}。
"""

import base64
import ctypes
import io
import json
import math
import os
import queue
import re
import socket
import struct
import subprocess
import sys
import threading
import time
import wave
import webbrowser
from ctypes import wintypes
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

APP_NAME = "iKF Audio Center"
APP_VER = "1.0.0"

def _load_device_model():
    """装载 device-model.toml（数据即真源）—— S1 场景 / S2 出厂 / S3 名字表+白名单。

    查找顺序：IKF_MODEL_TOML 环境变量 → 构建产物位置（仓库根）→ 源码位置
    （tools/src 向上两级）。失败 = 拒绝启动，绝不内置兜底 —— 兜底就是真源
    漂移复发（与 Go 侧 model.go 同一纪律）。tomllib 需 3.11+。
    """
    import tomllib
    here = os.path.dirname(os.path.abspath(__file__))
    cands = [os.environ.get("IKF_MODEL_TOML"),
             os.path.join(here, "iKF-Audio-Center-Wails", "model", "device-model.toml"),
             os.path.join(here, os.pardir, os.pardir,
                          "iKF-Audio-Center-Wails", "model", "device-model.toml")]
    path = next((c for c in cands if c and os.path.isfile(c)), None)
    if path is None:
        raise SystemExit("[model] 找不到 device-model.toml（可用 IKF_MODEL_TOML 指定）")
    try:
        with open(path, "rb") as f:
            m = tomllib.load(f)
        scene = m["scene"]
        out = {
            "exclude": frozenset(scene["exclude"]),
            "unproven": frozenset(scene["unproven"]),
            "builtin": set(scene["builtin"]),
            "alias": dict(m.get("group_alias", {})),
            "factory_profile": scene["factory_profile"],
            "factory_scene": scene["factory_scene"],
            # S2：出厂表 42 + 初始化设备级 1（保序）
            "factory": [(kv["ep"], kv["name"], kv["val"]) for kv in m.get("factory", [])],
            "init_device": [(kv["ep"], kv["name"], kv["val"]) for kv in m.get("init_device", [])],
        }
        assert m["meta"]["schema_version"] == 1, "schema_version != 1"
        assert (len(out["exclude"]), len(out["unproven"])) == (6, 35), \
            "排除名单规模漂移: %d/%d != 6/35" % (len(out["exclude"]), len(out["unproven"]))
        assert len(out["builtin"]) == 6, "内置场景漂移: %d != 6" % len(out["builtin"])
        assert len(out["alias"]) == 10, "分组别名漂移: %d != 10" % len(out["alias"])
        assert len(out["factory"]) == 42, "出厂表漂移: %d != 42" % len(out["factory"])
        assert len(out["init_device"]) == 1, "设备级漂移: %d != 1" % len(out["init_device"])
        # S3：名字表 208（112+96，顺序即下标）+ 白名单 80（65/15 自检口径）
        out["names_generic"] = list(m["names"]["generic"])
        out["names_oem"] = list(m["names"]["oem"])
        out["writable"] = [dict(kv) for kv in m["writable"]]
        assert (len(out["names_generic"]), len(out["names_oem"])) == (112, 96), \
            "名字表漂移: %d/%d != 112/96" % (len(out["names_generic"]), len(out["names_oem"]))
        assert len(out["writable"]) == 80, "白名单漂移: %d != 80" % len(out["writable"])
        _rc = {"render": 0, "capture": 0}
        for _w in out["writable"]:
            assert _w["t"] in ("bool", "int", "float"), "非法 kind: %r" % _w
            assert ("lo" in _w) == ("hi" in _w), "lo/hi 必须成对: " + _w["name"]
            _rc[_w["ep"]] += 1
        assert (_rc["render"], _rc["capture"]) == (65, 15), \
            "白名单端点分布漂移: %r != 65/15" % _rc
        # S4：分组映射 53 / 标签 28 / 顺序 27 / 端点元数据 / ValueType
        out["profile_groups"] = {ep: dict(tbl) for ep, tbl in m.get("profile_groups", {}).items()}
        out["group_label"] = dict(m.get("group_label", {}))
        out["display"] = dict(m.get("display", {}))
        out["profile_endpoint"] = dict(m.get("profile_endpoint", {}))
        out["profile_value_type"] = dict(m.get("profile_value_type", {}))
        _gn = sum(len(t) for t in out["profile_groups"].values())
        assert _gn == 53, "分组映射漂移: %d != 53" % _gn
        assert len(out["group_label"]) == 28, "标签漂移: %d != 28" % len(out["group_label"])
        assert len(out["display"].get("group_order", ())) == 27, "展示顺序漂移"
        for _g in out["display"]["group_order"]:
            assert _g in out["group_label"], "顺序里有组没有标签: " + _g
        assert len(out["profile_endpoint"]) == 2 and len(out["profile_value_type"]) == 3, "端点元数据漂移"
    except SystemExit:
        raise
    except Exception as e:
        raise SystemExit("[model] device-model.toml 校验失败 (%s): %s" % (path, e))
    return out


_MODEL = _load_device_model()


# ====================================================================
# 一、属性名表（从 osConfLib.dll 的字符串表实测导出）
# ====================================================================

# ★ S3 起真源 = model/device-model.toml [names]（208 = 112+96，装载时自校验）。
# 构建注入 __GENERIC__/__OEM__ 与 tools/propnames.json 同步退役。
GENERIC_NAMES = list(_MODEL["names_generic"])
OEM_NAMES = list(_MODEL["names_oem"])

GENERIC_SET = frozenset(GENERIC_NAMES)
ALL_NAMES = list(GENERIC_NAMES) + list(OEM_NAMES)

# --------------------------------------------------------------------
# 设备端点：osConfLib.dll 有两条互相独立的设备链表
#
#   GetDeviceCount(0, …) → 渲染端点（扬声器）  GetSupportFeature = 16801926
#   GetDeviceCount(1, …) → 采集端点（麦克风）  GetSupportFeature =  164352
#
# 两条链表是两套独立上下文，属性可读性完全不同。
# **麦克风侧参数必须打在采集端点上**，打在渲染端点上一律返回 2（不支持）。
# 早期实现只建了渲染端点，于是整个麦克风页被误判成「驱动未开放」。
# --------------------------------------------------------------------
EP_RENDER = "render"
EP_CAPTURE = "capture"
ENDPOINTS = (EP_RENDER, EP_CAPTURE)

# --------------------------------------------------------------------
# 可写属性白名单：全部经实机「写入 → 读回」往返验证
#
# 按端点分表。VolumeControl / VolumeScalarControl / MuteControl
# 在**两个端点上都有**，语义分别是扬声器音量与麦克风音量，必须分开登记。
# --------------------------------------------------------------------
# ★ S3 起真源 = model/device-model.toml [[writable]]（80 条显式展开，
# 含 SS×16 / EQ×10 / 声道×2；kind 值域离散全在数据里，装载时自校验）。
# 历史逐条证据注释（A-E 档移植、第 48/50/62 轮更正等）随数据走。
WRITABLE = {ep: {} for ep in ENDPOINTS}
for _w in _MODEL["writable"]:
    WRITABLE[_w["ep"]][_w["name"]] = _w["t"]

# --------------------------------------------------------------------
# 场景排除名单（2026-10-22，与 Go 侧 profile.go 的 sceneExclude / sceneUnproven
# 逐项同值；闸 scene_parity_gate.py 会反查两线的项数是否一致）
#
# 口径：「存为新场景」与「应用场景」共用同一份判定 —— 排除项既不写进 .profile，
#   应用旧档时也跳过不写。Go 侧 2026-10-19 就有这套名单，Python 一直只有
#   `"#" in pname` 一层，实测同一个「存为新场景」：Go 37 项 / Python 64 项。
# ★ DevSampleRateControl 最要命：写它会触发 USB 重协商 → 端点重建 →
#   采集端会话值全清，正是「应用场景后麦克风页整体不同步」的根因。
#   Go 修了，Python 一直没修。
# --------------------------------------------------------------------
# ★ S1 起真源 = iKF-Audio-Center-Wails/model/device-model.toml [scene].exclude
SCENE_EXCLUDE = _MODEL["exclude"]
# ★ S1 起真源 = 同上 [scene].unproven
SCENE_UNPROVEN = _MODEL["unproven"]


def excluded_from_scene(name):
    """场景排除判定（唯一入口）。带 # 声道后缀的（VolumeScalarControl#0）同样挡。"""
    return "#" in name or name in SCENE_EXCLUDE or name in SCENE_UNPROVEN


def scene_excluded_count():
    """排除名单规模（自检用）：设备级 6 + APO 未实现 35。"""
    return len(SCENE_EXCLUDE), len(SCENE_UNPROVEN)


def writable_count():
    """可写参数总数（两个端点合计），供自检输出。"""
    return sum(len(v) for v in WRITABLE.values())


def _infer_kind(value):
    """按值的实际类型推断打包方式，供 write_auto 处理白名单外的属性。

    猜错也只会让驱动回一个失败码，不会写坏别的参数。与 Go 版 inferKind 对齐。
    """
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, str):
        if value.strip().lower() in ("true", "false", "on", "off", "yes", "no"):
            return "bool"
        return "int"
    try:
        f = float(value)
    except (TypeError, ValueError):
        return "int"
    return "int" if f == int(f) else "float"


# --------------------------------------------------------------------
# HAZARD —— 必须显式跳过的三项
#
# 这三项在驱动内部会解引用我们传进去的 pValue 结构，需要额外的参数形态；
# 用普通约定调用会触发访问违例（0xC0000005）。
#
#   Python 的 ctypes 会把该访问违例转换成可捕获的 OSError，
#   所以 read() 里 `except OSError: return -2` 能优雅降级。
#
#   Go 的 syscall 不做这层转换：访问违例会直接终止整个进程，
#   defer/recover 拦不住（recover 只能捕获 Go 自己的 panic）。
#
# 因此在 Go 侧必须显式跳过；这里也一并跳过，保证两条交付线的
# 能力表与写回结果逐项一致（否则 Python 得 -3、Go 得 -2）。
# --------------------------------------------------------------------
HAZARD = frozenset((
    "IsSupportFormat",
    "GetSpeakerShiftVolumeLevel",
    "GetSpeakerShiftDistanceByDB",
))

# --------------------------------------------------------------------
# slowConst —— 「读一次要两秒、值却永远不变」的属性，读一次就记住。
#
# 实测（tmp/time_props.py，本机 iKF-V11，208 项逐项计时）：
#   GetDirectXVer      2120.8 ms   ← 唯一的慢项
#   GetFirmwareVer        8.5 ms
#   其余 206 项合计       约 23 ms
#
# 它慢是因为驱动内部要枚举 DXGI 适配器。而 /api/status 与 /api/params 都会
# 读它，于是两个端点都被钉在 2.1 秒上（实测 /api/status 2092ms、
# /api/params 2114ms，而 /api/meter 只要 2ms）。页面加载时的并发请求会
# 把驱动线程堵满，连一个简单的 /api/status 都要排 2.9 秒。
# 缓存后两个端点都降到约 30ms。
#
# 只缓存读成功（ret == 0）：值是这台机器的 DirectX 版本，进程生命周期内
# 不会变；失败不缓存，避免一次瞬时故障被永久记住。GetDirectXVer 是只读
# 属性（不在写入白名单里），无需考虑写失效。换机器若发现别的慢项，加进来。
# --------------------------------------------------------------------
_SLOW_CONST = frozenset(("GetDirectXVer",))

# --------------------------------------------------------------------
# 取值范围：取自驱动自带面板定义 Skin/FaceLift/*.cml 里的 Slider Min/Max
#   apoSpeakerEQ.cml        EQ 推子 ±20 dB
#   apoSpeakerKaraoke.cml   升降 Key −4..+4 / 人声消除 0..100
#   apoSpeakerEM.cml        房间大小 3 档（小/中/大）
#   apoMicInNoiseReduction.cml   降噪强度 8..20
#   apoMicInXearMagicVoice.cml   魔音类型 0..3
#   apoMicInKaraoke.cml          麦克风回音 −12..+12
#   apoSpeakerXearAudioBrilliant.cml  增艳强度 0..6
#   apoSpeakerXearDynamicBass.cml     低音强度 0..10 / 截止 100..400 Hz
#   apoSpeakerXearVoiceClarity.cml    清晰度 0..6 / 通信降噪 0..20 dB
#   apoSpeakerXearSurroundHeadPhone.cml
#                                     环绕模式 Tag 0/1（音乐·游戏 / 电影）
#                                     房间大小 Tag 0=大 1=中 2=小（★ 与直觉反序，
#                                     界面上的「小/中/大」必须按这个映射换算）
#   GFSControl              不是参数而是 HKCU\SOFTWARE\C-Media\Hook\EnableGFS
#                           的读写接口（osConfLib 反编译确认），故按 0/1 布尔处理
# 取值范围与端点无关，故仍按属性名统一登记。
# --------------------------------------------------------------------
# ★ S3 起真源 = TOML [[writable]] 的 lo/hi（缺省 = 不钳制）。
RANGES = {}
for _w in _MODEL["writable"]:
    if "lo" in _w:
        rng = (_w["lo"], _w["hi"])
        if _w["name"] in RANGES:
            assert RANGES[_w["name"]] == rng, "值域跨端点不一致: " + _w["name"]
        RANGES[_w["name"]] = rng


# --------------------------------------------------------------------
# ★★ 第 52 轮（H 档）：**逐声道音量** —— 名字里带 "#<声道索引>" 的虚拟参数
#
# 背景：PropertyControl 的 pValue 是二级指针，*pValue → int32 声道索引；
# 早期实现只按需要写死 -1。第 52 轮用「改索引再读回」验证它是**真参数**：
#   VolumeScalarControl idx=0 → 0.500000   idx=1 → 0.500000
#   idx=2..7                 → ret=0x10004（该声道不存在）
#   写 idx=0=0.30 / idx=1=0.70 → 互不影响地读回 0.30 / 0.70
# 索引语义与官方 apoSpeakerVol.cml 的 Slider Tag 完全一致：
#   0=FL 1=FR 2=FC 3=LFE 4=BL 5=BR 6=SL 7=SR
# 本机 iKF 耳机端点是**立体声端点**：只有 0/1 存在；把
# Speaker_Channel_Setting 切到 4/6/8 也不会展开（h_probe3 实测），
# 所以界面只做左/右两根，不摆 8 根点了没反应的滑杆。
#
# ★ 命名约定：`<真实属性名>#<声道索引>`。_pc() 在调用前拆开，因此
#   读 / 写 / 置灰 / 滑杆同步全部复用既有路径，没有第二套机制。
# ★ 但它们**不是驱动属性**：`#0`/`#1` 不能进 .profile（官方面板读不懂），
#   所以 profile_save 显式跳过含 "#" 的名字；_group_of 先剥后缀再查分组表。
# ★ 也**不进 `ALL_NAMES`**（那是 208 项真实属性的名单），只在 param_map()
#   里额外补出来，于是「参数」页不会多出两个假参数行。
# ★ S3 起真源 = TOML [[writable]] 里带 # 的虚拟参数（名字内 #<声道索引>）。
CHANNEL_PARAMS = {}
for _w in _MODEL["writable"]:
    if "#" in _w["name"]:
        CHANNEL_PARAMS[_w["name"]] = int(_w["name"].rpartition("#")[2])


def _split_channel(name):
    """`VolumeScalarControl#0` → ("VolumeScalarControl", 0)；无 "#" → (name, -1)。

    Go 线同名函数 centerSplitChannel（tools/… profile.go），两处必须一致。
    """
    if "#" in name:
        base, _, tail = name.rpartition("#")
        try:
            return base, int(tail)
        except ValueError:
            return name, -1
    return name, -1

# 离散取值白名单：驱动对非法值一律返回 0x10003，先在应用层收口。
# 实测 DevSampleRateControl 只接受 44100 / 48000，
# 88200 / 96000 / 176400 / 192000 全部被拒（原面板那六档在本机是死的）。
# ★ 第 50 轮（E 档）Speaker_Channel_Setting：官方 .cml 只给了 4 个 RadioButton，
#   Tag 分别是 2 / 4 / 6 / 8（= 声道数本身，不是档位下标）。实测这 4 个值
#   写→读回全部原样（wret=0 / ret=0），其它值没有官方依据，一律挡在应用层。
# ★ S3 起真源 = TOML [[writable]] 的 choices（驱动对非法值返回 0x10003，
# 先在应用层收口；同名字跨端点必须同值，装载时断言）。
CHOICES = {}
for _w in _MODEL["writable"]:
    if _w.get("choices"):
        ch = tuple(_w["choices"])
        if _w["name"] in CHOICES:
            assert CHOICES[_w["name"]] == ch, "离散取值跨端点不一致: " + _w["name"]
        CHOICES[_w["name"]] = ch

# 魔音类型：取自原面板 apoMicInXearMagicVoice.cml 的 RadioButton 文案
MAGIC_VOICES = ["怪兽", "卡通", "男人", "女人"]

# 声道测试：掩码取自原面板 apoSpeakerSetting.cml 的 PlayTestSound Tag 全集，
# 第四个字段是**测试音 WAV 的文件名** —— PlayTestSound 的第一个参数就是它。
#
# ★ 第 28 轮：以前这里只有三个字段，声音是驱动「自己找」的，于是调用方
#   只能猜第一个参数传什么，最后传了设备上下文指针 —— 结果是必然失败。
#   反编译 osConfLib.dll（Ghidra 12.1.4，PlayTestSound@0x1800060E0）看清了：
#     第一个参数是 **LPWSTR**，直接进 mmioOpenW + CreateFileW，
#     也就是 MSDN 那个经典示例 WaveOpenFile；拿不到文件句柄就 return 0x10001。
#   驱动安装目录下正好躺着这 8 个文件：<CPL>/sound/*.wav，
#   文件名与声道一一对应（见下表），是原面板自己带的那份。
TEST_CHANNELS = [
    ("LF", 1, "左前", "L-Front.wav"),
    ("RF", 2, "右前", "R-Front.wav"),
    ("C", 4, "中置", "Center.wav"),
    ("W", 8, "低音", "Subwoofer.wav"),
    ("LB", 16, "左后", "L-BackSurround.wav"),
    ("RB", 32, "右后", "R-BackSurround.wav"),
    ("LS", 512, "左环绕", "L-Surround.wav"),
    ("RS", 1024, "右环绕", "R-Surround.wav"),
]
# 掩码 -> 文件名，play_test 用
TEST_WAV = dict((c[1], c[3]) for c in TEST_CHANNELS)

# 环境类型。索引语义由 CMEffectAPO_108B.dll 反编译 + 原面板 apoSpeakerEM.cml
# 的 RadioButton Tag 双重确认（详见 工具/build_center.py 的 ENV_FALLBACK 注释）：
#   0 = 预设（原厂默认，面板下拉里被注释掉的那一项）
#   1..23 = 一般环境 … 水底（EAX 标准序，APO 调试 switch 逐项吻合）
#   24..27 = 迷幻 / 晕眩 / 疯狂 / KTV（取自原面板下拉定义，顺序存疑）
ENVIRONMENTS = [
    "预设（原厂默认）", "一般环境", "吸音室", "卧房", "浴室",
    "客厅", "石室", "观众席", "音乐厅", "洞穴", "竞技场", "飞机棚",
    "地毯走廊", "走廊", "石头回廊", "胡同", "森林", "城市", "山中",
    "采石场", "平原", "停车场", "下水道", "水底",
    "迷幻", "晕眩", "疯狂", "KTV",
]
ROOM_SIZES = ["小房间", "中房间", "大房间"]

# 相对量属性：写入是「增量」而非「绝对值」，不能做「值相同即跳过」的判断
NOOP_SKIP = frozenset(["VolumeControl"])

# 只读展示项（顺序即界面「设备」页的展示顺序）
INFO_KEYS = [
    "GetDeviceFriendlyName", "GetAudioCodecName", "GetDriverVer",
    "GetFirmwareVer", "GetDeviceID", "GetDirectXVer",
]


# ====================================================================
# 二、驱动定位
# ====================================================================

def _registry_cpl():
    """从卸载信息里找安装位置。"""
    try:
        import winreg
    except ImportError:
        return None
    roots = [
        (winreg.HKEY_LOCAL_MACHINE,
         r"SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall"),
        (winreg.HKEY_LOCAL_MACHINE,
         r"SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall"),
    ]
    for root, path in roots:
        try:
            key = winreg.OpenKey(root, path)
        except OSError:
            continue
        i = 0
        while True:
            try:
                sub = winreg.EnumKey(key, i)
                i += 1
            except OSError:
                break
            try:
                sk = winreg.OpenKey(key, sub)
                try:
                    name, _ = winreg.QueryValueEx(sk, "DisplayName")
                except OSError:
                    name = ""
                try:
                    loc, _ = winreg.QueryValueEx(sk, "InstallLocation")
                except OSError:
                    loc = ""
                winreg.CloseKey(sk)
            except OSError:
                continue
            low = str(name).lower()
            if loc and ("ikf" in low or "c-media" in low or "cmedia" in low):
                cand = os.path.join(loc, "CPL")
                if os.path.isdir(cand):
                    return cand
                if os.path.isdir(loc):
                    return loc
    return None


def _app_dir():
    """程序所在目录 —— 绿色版（免安装）资源的根。

    frozen（PyInstaller 打包）取 exe 目录；源码运行取本文件目录。
    """
    if getattr(sys, "frozen", False):
        return os.path.dirname(os.path.abspath(sys.executable))
    return os.path.dirname(os.path.abspath(__file__))


def _local_cpls():
    """绿色版候选：程序目录**排在安装版前面**，同目录自带的资源优先于 Program Files。

    支持两种铺法（都在同一个目录里就能跑）：
        <app>/CPL/     整份 CPL 目录拷过来（与安装版同构，推荐）
        <app>/         osConfLib.dll 直接挨着程序（扁平铺法）
    """
    d = _app_dir()
    return [os.path.join(d, "CPL"), d]


def _shared_cpls():
    """安装版候选：旧安装包铺在 Program Files 下的路径 + 仓库内的驱动包位置。"""
    out = []
    for var in ("ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"):
        base = os.environ.get(var)
        if not base or not os.path.isdir(base):
            continue
        try:
            for name in sorted(os.listdir(base)):
                low = name.lower()
                if low.startswith("ikf") or "c-media" in low or "cmedia" in low:
                    out.append(os.path.join(base, name, "CPL"))
                    out.append(os.path.join(base, name))
        except OSError:
            pass
    # 环境变量缺失时的硬编码兜底（与 Go 版 sharedCpls() 一致）
    out.append(r"C:\Program Files\iKF headset\CPL")
    here = _app_dir()
    for rel in (
        os.path.join("WIN10(WHQL)", "SoftwareDriver", "DLL", "CPL", "X64"),
        os.path.join("SoftwareDriver", "DLL", "CPL", "X64"),
        os.path.join("..", "CPL"),
    ):
        out.append(os.path.join(here, rel))
    return out


# ★★ 第 50 轮的坑：递归兜底必须**限深**。
#
# 候选目录里有一个是 `_app_dir()`（= 程序所在目录）。源码运行时它就是**工程根**，
# 而工程根的 dist/ 里躺着上一轮打好的便携包，便携包自带
# `<工程根>/dist/iKF-Audio-Center-Portable/CPL/Driver/x64/vista/osConfLib.dll`。
# 原来那两遍 os.walk 没有深度上限，一路钻进去把那份 dll 认了下来 ——
# 于是 `_find_dll(<工程根>)` 成功，`self.cpl` 被判成**工程根**，
# `sound/` `profile/` `lang/` `FactoryDefault.xml` 全部指向不存在的路径
# （症状：源码运行时「声道测试 0/8 资源就位」、组态与词条读不到）。
#
# 官方文档过的铺法最深就是 `<CPL>/Driver/x64/vista`（相对 CPL 3 层），
# 但候选目录里还有一个**父目录级**的兜底 `C:\Program Files\iKF headset`
# （见 _shared_cpls，DLL 在它的第 4 层 `CPL\Driver\x64\vista\`）——
# 所以上限取 **4** 层：既保住了那条兜底，又能把便携包那份排掉
# （工程根 → dist/iKF-Audio-Center-Portable/CPL/Driver/x64/vista 是第 6 层）。
# 真正的安装版 / 便携版不走这里（它们走上面的显式相对路径），不受影响。
_WALK_MAX_DEPTH = 4


def _prune(dirs, root, base):
    """深度到顶就不再往下走（就地改 os.walk 的 dirs 列表即可剪枝）。"""
    rel = os.path.relpath(root, base)
    depth = 0 if rel in (".", "") else rel.count(os.sep) + 1
    if depth >= _WALK_MAX_DEPTH:
        del dirs[:]


def _find_dll(cpl):
    """在 CPL 目录下定位 x64 版 osConfLib.dll。"""
    for sub in ("", r"Driver\x64\vista", r"Driver\x64\xp",
                r"Driver\x86\vista", r"Driver\x64", r"Driver"):
        p = os.path.join(cpl, sub, "osConfLib.dll")
        if os.path.isfile(p):
            return p
    for root, _dirs, files in os.walk(cpl):
        _prune(_dirs, root, cpl)
        if "osConfLib.dll" in files and os.path.basename(root).lower() == "x64":
            return os.path.join(root, "osConfLib.dll")
    for root, _dirs, files in os.walk(cpl):
        _prune(_dirs, root, cpl)
        if "osConfLib.dll" in files:
            return os.path.join(root, "osConfLib.dll")
    return None


# ====================================================================
# 三、驱动桥接
# ====================================================================

def _u16(raw):
    """1024 字节缓冲区 → UTF-16 字符串（截到第一个 NUL）。"""
    s = raw.decode("utf-16-le", "replace")
    i = s.find("\x00")
    if i >= 0:
        s = s[:i]
    return s.strip()


def _guess_type(text):
    if text in ("0", "1"):
        return "bool"
    if text and text.lstrip("+-").isdigit():
        return "int"
    try:
        float(text)
        return "float"
    except (TypeError, ValueError):
        return "str"


def _query_ep(query):
    """从查询串里取端点，非法值一律回落渲染端点。"""
    try:
        v = parse_qs(query).get("ep", [EP_RENDER])[0]
    except Exception:
        return EP_RENDER
    return v if v in ENDPOINTS else EP_RENDER


class Driver(object):
    """osConfLib.dll 的薄封装。所有调用串行化（DLL 非线程安全）。

    线程亲和
    --------
    设备上下文带有线程局部状态（TLS）：ConfLibInit / CreateDeviceList /
    GetDeviceById 在哪个 OS 线程上调用，之后 PropertyControl 就必须在同一个
    OS 线程上调用，否则返回 0x10001（设备上下文无效）。

    实测：`--dump` / `--self-test` 在主线程上跑 208 项全部正常；
    同样的调用经 ThreadingHTTPServer 的处理器线程发起时，
    GetDirectXVer 会返回 0x10001，可读项从 49 掉到 48。
    （Go 版是 M:N 调度，问题更明显，所以那边用 runtime.LockOSThread 钉线程。）

    解决办法与 Go 版一致：所有 DLL 调用收敛到一个专用的工作线程上，
    调用方通过队列投递并等待结果。这样同时也天然实现了串行化，
    取代了原先的互斥锁。
    """

    def __init__(self):
        self.lib = None
        self.ctx = None
        self.cpl = None
        self.dll = None
        # close() 缓存下来的已加载句柄 —— 重连复用它，绝不 FreeLibrary
        self._lib_cached = None
        self.error = None
        self._keep = None
        # 采集端点（麦克风）。拿不到不算失败 —— 有些机型只有渲染端点。
        self.cap_ctx = None
        self._cap_keep = None
        self.cap_count = 0
        self.device_count = 0
        # _SLOW_CONST 的缓存，键是 (端点, 属性名)。见 _SLOW_CONST 的说明。
        self._const_cache = {}
        # 设备上下文前 12 字节：[0]=链表内索引 [4]=设备类型 [8]=链表号
        # PlayTestSound 的第三个参数就是这三元组，见 _bind 的注释。
        self.dev_idx = 0
        self.dev_list = 0
        self.cap_idx = 0
        self.cap_list = 1
        # 专用工作线程
        self._jobs = queue.Queue()
        self._worker = threading.Thread(target=self._loop, daemon=True)
        self._worker.start()

    # ---------------- 工作线程 ----------------
    def _loop(self):
        while True:
            item = self._jobs.get()
            if item is None:
                break
            fn, box = item
            try:
                box["v"] = fn()
            except BaseException as exc:      # 含 OSError（访问违例）
                box["e"] = exc
            finally:
                box["done"].set()

    def _call(self, fn):
        """把 fn 投递到工作线程执行并等结果。"""
        box = {"done": threading.Event()}
        self._jobs.put((fn, box))
        box["done"].wait()
        if "e" in box:
            raise box["e"]
        return box["v"]

    def close(self):
        if self.lib is not None:
            try:
                # 线程已经退出时再投递就是永久阻塞 —— 二次 close 必须先看它活着
                if self._worker is not None and self._worker.is_alive():
                    self._call(lambda: self.lib.ConfLibUnInit())
            except Exception:
                pass
            # ★ 2026-10-21：**不释放 DLL**。ctypes 的 WinDLL 对象一被回收就
            # FreeLibrary 真卸载 osConfLib.dll，而驱动内部状态/线程还指着它 ——
            # 实测第一次 reopen 正常、第二次必崩（进程无声退出，HTTP 无响应）。
            # Go 版 LazyDLL 本来就只 ConfLibUnInit、从不卸载，这里缓存句柄
            # 与之逐项对齐，重连时复用同一份映射。
            self._lib_cached = self.lib
            self.lib = None
        # ★ 2026-10-21：上下文必须一起清。以前只清 lib，close→open 之后
        # 若新设备枚举不出采集端点，cap_ctx 还留着上一轮的旧指针 —— 那是
        # 已作废的设备上下文，再拿去 PropertyControl 就是悬垂指针。
        # reopen 让这条路径变成常规路径（与 Go 版 driver.go Close 对齐）。
        self.ctx = None
        self.cap_ctx = None
        self._keep = None
        self._cap_keep = None
        self.cap_count = 0
        self.device_count = 0
        # ★ 2026-10-22：常量缓存一起清（与 Go 版 Close 对齐）—— Close 之后
        #   设备可能换了/驱动重装了，沿用上一轮 slowConst 读数没有意义。
        self._const_cache = {}
        self._jobs.put(None)
        # ★ 2026-10-21：必须等线程真的退出再返回。_ensure_worker 用
        # is_alive() 判断，线程还在处理结束哨兵时它仍是 True —— 于是
        # open() 不会重建线程，任务投进一个即将没人消费的队列，close→open
        # 死死卡在 _call 上（实测 POST /api/driver/reopen 永不返回）。
        w = self._worker
        if w is not None and w.is_alive() and w is not threading.current_thread():
            w.join(timeout=3)

    # ---------------- 端点 ----------------
    def ctx_of(self, ep):
        """取端点对应的设备上下文。返回 None 表示该端点不可用。"""
        if ep == EP_CAPTURE:
            return self.cap_ctx
        return self.ctx

    def endpoint_ready(self, ep):
        return self.lib is not None and self.ctx_of(ep) is not None

    # ---------------- 装载 ----------------
    def open(self):
        self._ensure_worker()
        return self._call(self._open_impl)

    def reopen(self):
        """松开旧上下文再重新枚举，带退避重试（与 Go 版 Driver.Reopen 对齐）。

        ★ 2026-10-22（用户虚拟机实测）：装完驱动再点「重新检测」，出现的是
        「麦克风 + 自定义场景」不可用、其余正常，而且**退出重开就好**。
        端点重建不是原子的 —— 渲染端点先就绪、采集端点（麦克风）后到，
        _open_impl 一次返回的话渲染侧全通、采集侧全 -9，界面就是"一半能用"。

        ★ 判据不能用「上一轮有没有采集端点」：启动时驱动根本没装，上一轮
        必然没有，判据恒不成立 —— 第一次修就栽在这里（一次都没重试）。
        现在只认「本轮连上了但采集端点缺失」，重试到拿到为止
        （400 / 800 / 1600 / 1600ms，合计约 4.4 秒）。
        """
        ok = False
        for i in range(5):
            if i:
                time.sleep(min(0.4 * (1 << (i - 1)), 1.6))
            self.close()
            ok = self.open()
            if not ok:
                # 完全没连上（驱动没装/端点整个没起来）：只再给一次机会
                # （400ms）。等满 5 轮 = 4.4 秒，用户点一下「重新检测」
                # 干等 4 秒才被告知没驱动，这个体验不能要。
                if i >= 1:
                    return False
                continue
            # 采集端点还没起来 → 端点重建没结束，这里才值得多等几轮
            if self.cap_ctx is None and i < 4:
                continue
            return True
        return ok

    def _ensure_worker(self):
        """close() 之后重连前，先把工作线程拉回来。

        close() 会往队列送 None 收掉线程（Go 版那边只做 ConfLibUnInit、
        不停 goroutine），所以这里必须成对补上，否则 close→open 会死锁在
        没人消费的队列上。
        """
        if self._worker.is_alive():
            return
        self._jobs = queue.Queue()
        self._worker = threading.Thread(target=self._loop, daemon=True)
        self._worker.start()

    def _open_impl(self):
        # 候选顺序（与 Go 版 driver.go 的 openLocked 逐项一致）：
        #   IKF_CPL 环境变量 → 绿色版程序目录 → 注册表安装位置 → Program Files 安装版
        cands = []
        env = os.environ.get("IKF_CPL")
        if env:
            cands.append(env)
        cands += _local_cpls()
        reg = _registry_cpl()
        if reg:
            cands.append(reg)
        cands += _shared_cpls()

        tried = []
        seen = set()
        for cpl in cands:
            if not cpl or cpl in seen:
                continue
            seen.add(cpl)
            if not os.path.isdir(cpl):
                continue
            dll = _find_dll(cpl)
            if not dll:
                continue
            tried.append(dll)
            try:
                os.add_dll_directory(os.path.dirname(dll))
            except (OSError, AttributeError):
                pass
            try:
                # ★ 2026-10-21：同一份 DLL 走缓存句柄，别重新 WinDLL ——
                # 重复加载再加一次引用计数没意义，而释放又会真卸载（见 close）。
                if (self._lib_cached is not None and self.dll and
                        os.path.normcase(dll) == os.path.normcase(self.dll)):
                    lib = self._lib_cached
                else:
                    lib = ctypes.WinDLL(dll)
            except OSError:
                continue
            try:
                self._bind(lib)
                if lib.ConfLibInit() != 0:
                    continue
                lib.CreateDeviceList()
                count = ctypes.c_int(0)
                lib.GetDeviceCount(0, ctypes.byref(count))
                if count.value <= 0:
                    # ★ 2026-10-22：Init 成功就配对撤销（与 Go 版 openLocked 对齐）。
                    #   启动重试 4 次若都不配对，会留下 4 次净 Init。
                    lib.ConfLibUnInit()
                    continue
                dev = ctypes.create_string_buffer(32)
                if lib.GetDeviceById(0, 0,
                                     ctypes.cast(dev, ctypes.c_void_p)) != 0:
                    lib.ConfLibUnInit()
                    continue
                # 采集端点（type=1）。失败不影响整体连接。
                cdev = ctypes.create_string_buffer(32)
                ccount = ctypes.c_int(0)
                lib.GetDeviceCount(1, ctypes.byref(ccount))
                cap_ok = False
                if ccount.value > 0:
                    cap_ok = lib.GetDeviceById(
                        1, 0, ctypes.cast(cdev, ctypes.c_void_p)) == 0
            except OSError:
                continue
            self.lib = lib
            self.ctx = ctypes.cast(dev, ctypes.c_void_p)
            self._keep = dev
            if cap_ok:
                self.cap_ctx = ctypes.cast(cdev, ctypes.c_void_p)
                self._cap_keep = cdev
            self.cap_count = ccount.value
            self.cpl = cpl
            self.dll = dll
            self.device_count = count.value
            self.error = None  # 重连成功要把上一轮的失败原因擦掉
            self._warm_up()
            return True

        if tried:
            self.error = ("找到 osConfLib.dll（%s），但初始化失败："
                          "驱动可能未安装完成，或设备未插入。" % tried[0])
        else:
            self.error = ("未找到 osConfLib.dll。请先安装 iKF headset 驱动，"
                          "或用环境变量 IKF_CPL 指向驱动的 CPL 目录。")
        return False

    def _warm_up(self):
        """丢弃式预热读取。**必须从 _open_impl 内部调用**（已经在工作线程上），
        不能再走 self._call —— 那会和 _open_impl 自己形成自锁。

        实测：ConfLibInit 之后**第一次** PropertyControl 读取有约 10% 的概率
        返回 ret=0 但缓冲区一个字节都没写（值为空串）。这是驱动内部的初始化
        竞态 —— Go 版启动快，稳定复现（--dump 第 4 行 Enable_EQ_GFX 约 2/20
        次为空）；Python 版解释器启动慢，几乎碰不到，但同样是隐患。

        修法：初始化完成后先做一次注定被丢弃的读，把驱动内部那条懒初始化
        路径走通。两条交付线用同一策略，保证输出逐行一致。
        """
        try:
            buf = ctypes.create_string_buffer(1024)
            keep = ctypes.create_string_buffer(1024)
            pbuf = ctypes.cast(buf, ctypes.c_void_p)
            pp = ctypes.pointer(pbuf)
            pval = ctypes.cast(keep, ctypes.c_void_p)
            self.lib.PropertyControl(self.ctx, "Enable_EQ_GFX", pp, pval, 0)
            del keep, pval, pp, pbuf
        except (OSError, AttributeError, ValueError):
            pass

    @staticmethod
    def _bind(lib):
        lib.ConfLibInit.restype = ctypes.c_int
        lib.ConfLibInit.argtypes = []
        lib.ConfLibUnInit.restype = ctypes.c_int
        lib.ConfLibUnInit.argtypes = []
        lib.CreateDeviceList.restype = ctypes.c_int
        lib.CreateDeviceList.argtypes = []
        lib.GetDeviceCount.restype = ctypes.c_int
        lib.GetDeviceCount.argtypes = [ctypes.c_int,
                                       ctypes.POINTER(ctypes.c_int)]
        lib.GetDeviceById.restype = ctypes.c_int
        lib.GetDeviceById.argtypes = [ctypes.c_int, ctypes.c_int,
                                      ctypes.c_void_p]
        lib.PropertyControl.restype = ctypes.c_int
        lib.PropertyControl.argtypes = [
            ctypes.c_void_p, ctypes.c_wchar_p,
            ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p, ctypes.c_int]
        # ---- 函数型导出：原面板用它们做声道测试与麦克风监听，早期实现漏掉了
        # 签名由 osConfLib.dll 反编译还原（见 工具/原面板功能移植对照表.md §四）：
        #   PlayTestSound(wavPath, mode, pInfo)  —— **三个参数**：
        #     [1] wavPath : LPWSTR，测试音 WAV 的路径（第 28 轮改正，
        #                   以前按「设备指针」传，导致恒返 0x10001）
        #     [2] mode    : 声道掩码（1/2/4/8/16/32/512/1024，或 3/0x33/0x3F/
        #                   0xFF/0x107/0x60F/0x63F 表示「整组一起响」）
        #     [3] pInfo   : 3 个 dword —— [+0] 链表内索引 [+8] 链表选择
        #                   （0=渲染 1=采集）；早期只传两个参数，第三个寄存器
        #                   R8 是垃圾值，函数读 [R8] 直接触发访问违例
        #   StopTestSound() / CloseTestSound()   —— 忽略入参，用内部单例
        #   RunListen2Device(pDevice) / StopListen2Device(pDevice)
        #     —— 读 pDevice 的 +4（设备类型，须 ∈ {2,3,5,0xA}）与 +8（链表号）
        if hasattr(lib, "PlayTestSound"):
            lib.PlayTestSound.restype = ctypes.c_int
            lib.PlayTestSound.argtypes = [ctypes.c_wchar_p, ctypes.c_int,
                                          ctypes.c_void_p]
        if hasattr(lib, "StopTestSound"):
            lib.StopTestSound.restype = ctypes.c_int
            lib.StopTestSound.argtypes = []
        if hasattr(lib, "CloseTestSound"):
            lib.CloseTestSound.restype = ctypes.c_int
            lib.CloseTestSound.argtypes = []
        if hasattr(lib, "RunListen2Device"):
            lib.RunListen2Device.restype = ctypes.c_int
            lib.RunListen2Device.argtypes = [ctypes.c_void_p]
        if hasattr(lib, "StopListen2Device"):
            lib.StopListen2Device.restype = ctypes.c_int
            lib.StopListen2Device.argtypes = [ctypes.c_void_p]
        if hasattr(lib, "ShowAsioPanel"):
            lib.ShowAsioPanel.restype = ctypes.c_int
            lib.ShowAsioPanel.argtypes = [ctypes.c_void_p]

    # ---------------- 参数读写 ----------------
    def _pc(self, name, is_set, payload=b"", ep=EP_RENDER):
        """原始调用。返回 (ret, 1024 字节缓冲区原始内容)。
        整体投递到工作线程执行（线程亲和 + 串行化）。"""

        def job():
            ctx = self.ctx_of(ep)
            if ctx is None:
                return -9, b""
            buf = ctypes.create_string_buffer(1024)
            if payload:
                ctypes.memmove(buf, payload, len(payload))
            pbuf = ctypes.cast(buf, ctypes.c_void_p)
            pp = ctypes.pointer(pbuf)

            # ★ 第 52 轮：`<属性名>#<声道索引>` → 拆成真名 + 索引。
            # 没带 "#" 的一律走 -1（历史行为，未改动）。
            base, ch = _split_channel(name)
            if base in GENERIC_SET:
                # 通用属性：pValue 只需是非空有效指针（函数不读它）
                keep = ctypes.create_string_buffer(1024)
                pval = ctypes.cast(keep, ctypes.c_void_p)
            else:
                # OEM 属性：pValue 是二级指针，*pValue → int32 声道索引
                ci = ctypes.c_int(ch)
                inner = ctypes.cast(ctypes.pointer(ci), ctypes.c_void_p)
                outer = ctypes.pointer(inner)
                keep = (ci, inner, outer)
                pval = ctypes.cast(outer, ctypes.c_void_p)

            ret = self.lib.PropertyControl(ctx, base, pp, pval,
                                           1 if is_set else 0)
            raw = buf.raw
            del keep
            return ret, raw

        return self._call(job)

    def read(self, name, ep=EP_RENDER):
        """返回 (ret, 字符串值)。ret=0 成功，2 存在但从未写过（不是不支持），
        -1 未找到，-2 调用异常（该属性需要特殊 pValue 结构）。"""
        if self.lib is None or self.ctx_of(ep) is None:
            return -9, ""
        if name in _SLOW_CONST:
            return self._read_const(name, ep)
        return self._read_uncached(name, ep)

    def _read_const(self, name, ep):
        """读 slowConst 属性：命中缓存直接回，否则真读，读成功才记。

        见 _SLOW_CONST 的说明。锁不必加 —— dict 的单键读写受 GIL 保护，
        而真读本身走工作线程串行化，不会并发。"""
        key = (ep, name)
        hit = self._const_cache.get(key)
        if hit is not None:
            return hit
        ret, val = self._read_uncached(name, ep)
        if ret == 0:
            self._const_cache[key] = (ret, val)
        return ret, val

    def _read_uncached(self, name, ep):
        try:
            ret, raw = self._pc(name, False, ep=ep)
        except OSError:
            return -2, ""
        except Exception:
            return -2, ""
        return ret, _u16(raw)

    def write(self, name, value, ep=EP_RENDER):
        """写入并按白名单裁剪。返回结果字典。"""
        if self.lib is None or self.ctx_of(ep) is None:
            return {"ok": False, "error": "驱动未就绪", "ret": -9}
        kind = WRITABLE.get(ep, {}).get(name)
        if kind is None:
            return {"ok": False, "error": "该参数未开放写入", "ret": -1}

        lo, hi = RANGES.get(name, (None, None))
        try:
            if kind == "bool":
                if isinstance(value, str):
                    on = value.strip().lower() in ("1", "true", "on", "yes")
                else:
                    on = bool(value)
                payload = struct.pack("<i", 1 if on else 0)
                target = "1" if on else "0"
            elif kind == "float":
                f = float(value)
                if lo is not None:
                    f = max(lo, min(hi, f))
                payload = struct.pack("<f", f)
                target = "%f" % f
            else:
                v = int(round(float(value)))
                if lo is not None:
                    v = max(lo, min(hi, v))
                allowed = CHOICES.get(name)
                if allowed and v not in allowed:
                    return {"ok": False, "ret": -1,
                            "error": "只接受 %s" % (list(allowed),)}
                payload = struct.pack("<i", v)
                target = str(v)
        except (TypeError, ValueError):
            return {"ok": False, "error": "值不合法：%r" % (value,)}

        # 值未变化时直接视为成功，不再打扰驱动。
        # 实测 MuteControl 对「重复写同一个值」会返回 0x10003（写入被拒绝），
        # 但语义上这本来就是一次成功的无操作。
        # VolumeControl 是相对 dB 增量，不能用「值相同」判断，故排除。
        if name not in NOOP_SKIP:
            r0, cur = self.read(name, ep)
            if r0 == 0 and cur == target:
                return {"ok": True, "ret": 0, "value": cur,
                        "read_ret": 0, "noop": True}

        try:
            ret, _ = self._pc(name, True, payload, ep)
        except OSError:
            return {"ok": False, "error": "写入触发访问异常", "ret": -3}

        time.sleep(0.02)
        r2, back = self.read(name, ep)
        return {"ok": ret == 0 and r2 == 0, "ret": ret,
                "value": back, "read_ret": r2}

    def write_auto(self, name, value, ep=EP_RENDER, kind=None):
        """先走白名单；未命中但属于 208 项时改走 write_raw。

        口径（用户要求「官方内容零遗漏，功能能不能用交给硬件匹配」）：
        官方面板里有、但新界面没登记成可写控件的属性（例如音箱方位的
        SS_LF_ANGLE / SS_RF_ANGLE 一族），也必须真写一次并把驱动的真实
        返回码报出来，而不是在前端直接短路成「本机不支持」。本机这些属性
        返回 2（存在但不支持），那是硬件给的结论，不是我们替它下的。

        kind 为空时按值的实际类型推断（bool → bool、整数 → int、其余 → float）。
        """
        if name in WRITABLE.get(ep, {}):
            return self.write(name, value, ep)
        if kind is None:
            kind = _infer_kind(value)
        return self.write_raw(name, value, ep, kind)

    def write_raw(self, name, value, ep=EP_RENDER, kind="int"):
        """白名单之外、但确实是 208 项里的属性，也真写一次并如实回报驱动返回码。

        用途：回放官方面板导出的组态。官方档里会出现 DefaultDeviceControl
        这类我们未接入界面的属性（FactoryDefault.xml 就只有它）。把它按
        「未开放写入」跳掉，等于把官方组态做成了空操作 —— 那是遗漏。

        这里不做范围钳制（钳制只针对界面控件），值一律按 kind 打包，
        写不写得进由驱动自己决定，我们只如实回报。

        危险属性（HAZARD）必须显式跳过：它们会解引用 pValue，触发访问违例。
        Python 的 ctypes 会转成可捕获的 OSError，但 Go 的 syscall 会直接
        杀进程，为了让两条交付线的结果逐项一致，两边都跳过。
        """
        if name in HAZARD:
            return {"ok": False, "ret": -2, "error": "该参数需要特殊参数结构"}
        if name not in ALL_NAMES:
            return {"ok": False, "ret": -1, "error": "未知属性名"}
        if self.lib is None or self.ctx_of(ep) is None:
            return {"ok": False, "ret": -9, "error": "驱动未就绪"}
        try:
            if kind == "float":
                payload = struct.pack("<f", float(value))
                target = "%.4f" % float(value)
            elif kind == "bool":
                payload = struct.pack("<i", 1 if value else 0)
                target = "1" if value else "0"
            else:
                payload = struct.pack("<i", int(value))
                target = str(int(value))
        except (TypeError, ValueError):
            return {"ok": False, "ret": -1, "error": "值不合法：%r" % (value,)}
        try:
            ret, _ = self._pc(name, True, payload, ep)
        except OSError:
            return {"ok": False, "ret": -3, "error": "写入触发访问异常"}
        time.sleep(0.02)
        r2, back = self.read(name, ep)
        return {"ok": ret == 0 and r2 == 0, "ret": ret,
                "value": back, "read_ret": r2, "target": target}

    # ---------------- 函数型导出 ----------------
    def _invoke(self, proc_name, ep, *args):
        """调用一个非属性导出。同样必须落在工作线程上（线程亲和）。

        设备上下文只在导出确实声明了入参时才传 —— StopTestSound /
        CloseTestSound 的 argtypes 是空表（反汇编确认它们用模块内部单例），
        多传一个参数会被 ctypes 直接拒绝。
        """
        if self.lib is None or self.ctx_of(ep) is None:
            return None, {"ok": False, "error": "驱动未就绪", "ret": -9}
        if not hasattr(self.lib, proc_name):
            return None, {"ok": False, "ret": -1,
                          "error": "该驱动版本未导出 %s" % proc_name}
        fn = getattr(self.lib, proc_name)
        call = ([self.ctx_of(ep)] if fn.argtypes else []) + list(args)
        try:
            ret = self._call(lambda: fn(*call))
        except OSError:
            return None, {"ok": False, "ret": -3,
                          "error": "%s 调用异常" % proc_name}
        return ret, None

    def test_wav(self, mask):
        """声道掩码 → 测试音 WAV 的完整路径；掩码不在表里返回 None。

        资源在驱动安装目录下：<CPL>/sound/<声道>.wav。
        路径必须是**绝对路径** —— 驱动内部走 mmioOpenW，相对路径是按
        调用进程的当前目录解析的，而本程序的工作目录不确定。
        """
        name = TEST_WAV.get(int(mask))
        if not name or not self.cpl:
            return None
        return os.path.join(self.cpl, "sound", name)

    def play_test(self, mask):
        """声道测试：在指定声道上播一段测试音。

        ★ 第 28 轮修正。签名是 PlayTestSound(wavPath, mode, pInfo)：
          · 第一个参数是**测试音 WAV 的路径**（LPWSTR），不是设备上下文。
            反编译确认它直接进 mmioOpenW + CreateFileW（MSDN 的
            WaveOpenFile 示例），拿不到句柄就 return 0x10001。
            以前的实现把设备上下文指针塞在这里，被当成宽字符串去开文件，
            必然失败 —— 面板上就永远显示「声道测试不可用（65537）」。
          · pInfo 仍是 3 个 dword：+0 链表内索引、+8 链表选择（0=渲染）。
            这正是设备上下文前 12 字节的形状。
        """
        if self.lib is None or self.ctx is None:
            return {"ok": False, "error": "驱动未就绪", "ret": -9}
        if not hasattr(self.lib, "PlayTestSound"):
            return {"ok": False, "ret": -1,
                    "error": "该驱动版本未导出 PlayTestSound"}
        mask = int(mask)
        wav = self.test_wav(mask)
        if wav is None:
            return {"ok": False, "ret": -2, "mask": mask,
                    "error": "未知的声道掩码 0x%X" % mask}
        if not os.path.isfile(wav):
            return {"ok": False, "ret": -4, "mask": mask,
                    "error": "找不到测试音资源（%s）" % wav}
        info = (ctypes.c_int * 3)(self.dev_idx, 0, self.dev_list)
        try:
            ret = self._call(lambda: self.lib.PlayTestSound(
                wav, mask, ctypes.cast(info, ctypes.c_void_p)))
        except OSError:
            return {"ok": False, "ret": -3, "mask": mask,
                    "error": "PlayTestSound 调用异常"}
        if ret != 0:
            # 0x10001 是「测试音文件打不开」而不是「本机不支持」的可能性最大，
            # 但走到这里说明文件是在的，那就把原始码给出来备查。
            return {"ok": False, "ret": int(ret), "mask": mask,
                    "error": "驱动拒绝了这次调用（0x%X）" % (ret & 0xFFFFFFFF)}
        return {"ok": True, "ret": 0, "mask": mask}

    def stop_test(self):
        """停止声道测试。反汇编确认 StopTestSound() 忽略入参、
        用模块内部单例，且恒返回 0。"""
        ret, err = self._invoke("StopTestSound", EP_RENDER)
        if err:
            return err
        return {"ok": ret == 0, "ret": ret}

    def close_test(self):
        """关闭声道测试。同 StopTestSound，恒返回 0。"""
        ret, err = self._invoke("CloseTestSound", EP_RENDER)
        if err:
            return err
        return {"ok": ret == 0, "ret": ret}

    def show_asio(self):
        """原面板 MainWindow 的 BtnASIO。CM108B 无 ASIO，预期返回 -1。"""
        ret, err = self._invoke("ShowAsioPanel", EP_RENDER)
        if err:
            return err
        return {"ok": ret == 0, "ret": ret}

    # ---------------- 组态（特效组态） ----------------
    def profiles(self):
        """列出全部组态。返回 {ok, dir, writable, factory, list:[...]}。

        与 Go 侧 profile.go:Profiles 逐字段对齐（两线的场景墙必须同形）：
          - builtin 判定 = 目录自带 × BUILTIN_SCENES 名单（不按目录一刀切）；
          - 官方空模板（如 default/HiFi，.profile 无参数快照）不进列表；
          - 排序：「默认」永远第一张卡（2026-10-20 用户拍板），其次内置档，
            再次用户自建档，同层按名字；
          - 「预设」（FactoryDefault.xml）不再作为场景卡列出 —— 它整档只有
            一条 DefaultDeviceControl 一键出厂指令（本机驱动未实现，应用
            必然无效），与「默认」卡语义重复（Go 侧 2026-10-19 拍板同此）。
            factory 路径仍随 API 返回，名字「预设」在 save/rename 侧继续
            作为保留字拦截。
        """
        seen = {}
        for d, builtin in _profile_dirs(self):
            if not os.path.isdir(d):
                continue
            try:
                names = sorted(os.listdir(d))
            except OSError:
                continue
            for fn in names:
                if not fn.lower().endswith(".cml"):
                    continue
                if fn in (PROFILE_TEMPLATE, PROFILE_CURRENT):
                    continue
                stem = fn[:-4]
                if stem in seen:
                    continue          # 用户目录优先
                cml = _read_text(os.path.join(d, fn))
                pfile = os.path.join(d, stem + ".profile")
                desc = _lang_text(self, _cml_attr(cml, "Tooltip"))
                cnt, glabels = 0, []
                if os.path.isfile(pfile):
                    its = _profile_items(_read_text(pfile))
                    cnt = len(its)
                    glabels = _profile_group_labels(its)
                # 官方空模板不进列表（Go 侧 Profiles 同款过滤）
                if builtin and cnt == 0:
                    continue
                seen[stem] = {
                    "name": stem,
                    "desc": desc,
                    "builtin": builtin and stem in BUILTIN_SCENES,
                    "dir": d,
                    "items": cnt,
                    "has_data": os.path.isfile(pfile),
                    "groups": glabels,
                }
        # 排序：默认 → 内置 → 用户，同层按名字（Go 侧 rank 同款）
        def _rank(it):
            if it["name"] == PROFILE_FACTORY_SCENE:
                return 0
            return 1 if it["builtin"] else 2
        items = sorted(seen.values(),
                       key=lambda x: (_rank(x), x["name"].lower()))
        factory = ""
        if self.cpl:
            fp = os.path.join(self.cpl, "FactoryDefault.xml")
            if os.path.isfile(fp):
                factory = fp
        wdir = _profile_write_dir(self)
        writable = False
        if wdir:
            try:
                probe = os.path.join(wdir, ".wtest")
                with io.open(probe, "w") as f:
                    f.write("")
                os.remove(probe)
                writable = True
            except OSError:
                writable = False
        # active：设备真值识别 —— 与哪个场景快照完全一致就显示哪个，
        # 都不一致为 ""（自定义状态）。取代前端 localStorage 残留记忆。
        return {"ok": True, "dir": wdir,
                "writable": writable,
                "factory": factory, "list": items,
                "active": identify_active_scene(self)}

    def _profile_path(self, name):
        """按名字找到 .profile 文件；返回 (路径, 目录, 是否驱动自带)"""
        for d, builtin in _profile_dirs(self):
            p = os.path.join(d, name + ".profile")
            if os.path.isfile(p):
                return p, d, builtin
        return "", "", False

    def profile_save(self, name, desc=""):
        """把当前全部可写参数快照成组态，写出 .profile + .cml（官方格式）。"""
        name = (name or "").strip()
        if not name:
            return {"ok": False, "error": "组态名称不能为空", "ret": -1}
        if re.search(r'[\\/:*?"<>|]', name):
            return {"ok": False, "ret": -1,
                    "error": "文件名不合法 (不可包含 /\\:*?\"<>| 字符)"}
        if name == PROFILE_FACTORY:
            return {"ok": False, "ret": -1,
                    "error": "「%s」是原厂默认档，不能覆盖" % PROFILE_FACTORY}

        snapshot = []
        for ep in ENDPOINTS:
            if self.ctx_of(ep) is None:
                continue
            for pname in sorted(WRITABLE.get(ep, {})):
                # ★ 2026-10-22：口径收敛到 excluded_from_scene —— 不再只是挡
                # 「带声道后缀的虚拟参数」，而是设备级（音量/静音/采样率/声道拓扑）
                # 与 APO 未实现族一并排除。Go 侧 2026-10-19 起就走这条判定，
                # Python 此前只剩 `"#" in pname`，实测同一动作 37 vs 64 项。
                if excluded_from_scene(pname):
                    continue
                kind = WRITABLE[ep][pname]
                ret, val = self.read(pname, ep)
                if ret != 0:
                    continue
                snapshot.append((ep, pname, kind, val))
        if not snapshot:
            return {"ok": False, "ret": -2, "error": "没有可读参数，无法保存组态"}

        d = _profile_write_dir(self)
        try:
            os.makedirs(d, exist_ok=True)
        except OSError as e:
            return {"ok": False, "ret": -3, "error": "无法创建组态目录：%s" % e}
        try:
            with io.open(os.path.join(d, name + ".profile"), "w",
                         encoding="utf-8", newline="\n") as f:
                f.write(_build_profile_xml(name, snapshot))
            with io.open(os.path.join(d, name + ".cml"), "w",
                         encoding="utf-8", newline="\n") as f:
                f.write(_build_cml(name, desc or name))
        except OSError as e:
            return {"ok": False, "ret": -3, "error": "写入失败：%s" % e}
        return {"ok": True, "ret": 0, "name": name, "dir": d,
                "items": len(snapshot)}

    def profile_apply(self, name):
        """把组态里的每一条 Property 写回驱动。"""
        if name == PROFILE_FACTORY:
            path = os.path.join(self.cpl, "FactoryDefault.xml") if self.cpl else ""
            if not path or not os.path.isfile(path):
                return {"ok": False, "ret": -1, "error": "找不到 FactoryDefault.xml"}
        else:
            path, _, _ = self._profile_path(name)
            if not path:
                return {"ok": False, "ret": -1,
                        "error": "找不到组态「%s」" % name}

        items = _profile_items(_read_text(path))
        if not items:
            return {"ok": True, "ret": 0, "name": name,
                    "applied": 0, "failed": 0, "skipped": 0,
                    "note": "该组态为空（等同于全部走驱动默认）"}

        applied = failed = skipped = 0
        detail = []
        done = set()
        ops = []          # (ep, pname, v, kind, unknown, is_switch)
        factory_cmd_failed = False
        for ep_name, flow, _fx, pname, val, vtype in items:
            ep = EP_CAPTURE if flow.lower() == "in" else EP_RENDER
            if not pname:
                skipped += 1
                continue
            # FactoryDefault.xml 里同一个属性会在多个 Endpoint 下重复出现
            # （Speakers / SpeakerQuarter / Mic / DesktopMic），去重后再写。
            if (ep, pname) in done:
                continue
            done.add((ep, pname))
            # 兜底：旧档（排除名单出现前保存的）里混进的音量/采样率/拓扑项不写 ——
            # 音量被系统通路回滚造成读数不一致，采样率触发端点重建清空会话值。
            if excluded_from_scene(pname):
                skipped += 1
                continue
            kind = WRITABLE.get(ep, {}).get(pname)
            unknown = kind is None
            if unknown:
                # 白名单外但确实是 208 项里的属性 —— 照样真写，
                # 由驱动决定收不收（官方组态里就有这类属性）。
                kind = _kind_of_value_type(vtype, pname)
            v = _coerce(val, kind)
            if v is None:
                skipped += 1
                detail.append({"name": pname, "ep": ep, "ret": -1,
                               "error": "值不合法：%s" % val})
                continue
            # 两轮写入只对**登记过的布尔开关**讲：白名单外的项不知道语义，不猜。
            ops.append((ep, pname, v, kind, unknown,
                        (not unknown) and kind == "bool"))

        # ★ 两轮写入（与 Go 侧 ProfileApply 同序）：驱动的语义是**效果开关先开，
        #   级别/滑杆参数才收写入**（真机探针：Enable_ 关闭时写 LEVEL 一律失败）。
        #   所以第一轮写全部布尔开关，第二轮写其余参数。
        for want_switch in (True, False):
            for ep, pname, v, kind, unknown, is_switch in ops:
                if is_switch != want_switch:
                    continue
                if unknown:
                    res = self.write_raw(pname, v, ep, kind)
                else:
                    res = self.write(pname, v, ep)
                if res.get("ok"):
                    applied += 1
                else:
                    failed += 1
                    err = res.get("error")
                    if pname == "DefaultDeviceControl":
                        factory_cmd_failed = True
                        err = "当前耳机不支持一键恢复出厂设置"
                    detail.append({"name": pname, "ep": ep,
                                   "ret": res.get("ret"), "error": err})
        out = {"ok": failed == 0, "ret": 0 if failed == 0 else -3,
               "name": name, "applied": applied, "failed": failed,
               "skipped": skipped, "detail": detail[:12]}
        if factory_cmd_failed and applied == 0:
            out["error"] = "当前耳机不支持一键恢复出厂设置，该场景无法应用"
        return out

    def profile_delete(self, name):
        """删除用户自建组态。驱动自带的档拒绝删除。"""
        name = (name or "").strip()
        if not name:
            return {"ok": False, "ret": -1, "error": "缺少组态名称"}
        if name == PROFILE_FACTORY:
            return {"ok": False, "ret": -1, "error": "原厂默认档不可删除"}
        wdir = _profile_write_dir(self)
        path, d, builtin = self._profile_path(name)
        if not path:
            return {"ok": False, "ret": -1, "error": "找不到组态「%s」" % name}
        # 只允许删用户目录里的档；驱动自带目录即使可写也不动
        if builtin and os.path.normcase(d) != os.path.normcase(wdir):
            return {"ok": False, "ret": -2,
                    "error": "「%s」是驱动自带组态，不能删除" % name}
        removed = []
        for ext in (".profile", ".cml"):
            p = os.path.join(d, name + ext)
            if os.path.isfile(p):
                try:
                    os.remove(p)
                    removed.append(os.path.basename(p))
                except OSError as e:
                    return {"ok": False, "ret": -3, "error": "删除失败：%s" % e}
        return {"ok": True, "ret": 0, "name": name, "removed": removed}

    def profile_items(self, name):
        """读出一个场景的实际内容：分组摘要 + 键值对映射。

        与 Wails 版 /api/profile/items 逐字段一致。两处用途：
          - 右侧「场景详情」按 ApoEffect 分组列出这份场景动过哪些模块
          - 「已修改、未保存」脏标记 —— pairs 与当前驱动读数逐项比对

        pairs 的键统一成 "render|EQ_Slider1" / "capture|AGCControl"，
        值与 .profile 里存的文本一致。
        """
        name = (name or "").strip()
        if not name:
            return {"ok": False, "ret": -1, "error": "缺少场景名称"}
        builtin = False
        if name == PROFILE_FACTORY:
            if not self.cpl:
                return {"ok": False, "ret": -1,
                        "error": "找不到 FactoryDefault.xml"}
            path = os.path.join(self.cpl, "FactoryDefault.xml")
            builtin = True
        else:
            path, _d, builtin = self._profile_path(name)
            if not path:
                return {"ok": False, "ret": -1,
                        "error": "找不到场景「%s」" % name}
        if not os.path.isfile(path):
            return {"ok": False, "ret": -1,
                    "error": "读不到场景「%s」的内容" % name}

        items = _profile_items(_read_text(path))
        pairs, seen = {}, set()
        for _ep_name, flow, _fx, pname, val, _vt in items:
            if not pname:
                continue
            key = _profile_ep_of(flow) + "|" + pname
            if key in seen:
                continue
            seen.add(key)
            pairs[key] = val
        return {"ok": True, "ret": 0, "name": name, "builtin": builtin,
                "dir": os.path.dirname(path), "count": len(pairs),
                "groups": _profile_groups(items), "pairs": pairs}

    def profile_rename(self, old_name, new_name):
        """重命名用户自建场景：内容原样搬到新名字下。

        读旧档 → 改根节点 Name → 写新档 → 删旧档。改名的语义是「换个
        名字」，不是「用现在的状态覆盖它」，所以不重新快照驱动状态。
        只允许改用户写入目录里的档；驱动自带的档与「预设」拒绝改名。
        """
        old_name = (old_name or "").strip()
        new_name = (new_name or "").strip()
        if not old_name:
            return {"ok": False, "ret": -1, "error": "缺少原场景名称"}
        if not new_name:
            return {"ok": False, "ret": -1, "error": "场景名称不能为空"}
        if re.search(r'[\\/:*?"<>|]', new_name):
            return {"ok": False, "ret": -1,
                    "error": "文件名不合法 (不可包含 /\\:*?\"<>| 字符)"}
        if new_name == PROFILE_FACTORY:
            return {"ok": False, "ret": -1,
                    "error": "「%s」是默认场景，不能占用这个名字" % PROFILE_FACTORY}
        if new_name == old_name:
            return {"ok": True, "ret": 0, "name": new_name, "renamed": False}

        wdir = _profile_write_dir(self)
        path, d, builtin = self._profile_path(old_name)
        if not path:
            return {"ok": False, "ret": -1,
                    "error": "找不到场景「%s」" % old_name}
        if builtin and os.path.normcase(d) != os.path.normcase(wdir):
            return {"ok": False, "ret": -2,
                    "error": "「%s」是系统内置场景，不能重命名" % old_name}
        if self._profile_path(new_name)[0]:
            return {"ok": False, "ret": -3,
                    "error": "已存在同名场景「%s」" % new_name}
        try:
            os.makedirs(wdir, exist_ok=True)
        except OSError as e:
            return {"ok": False, "ret": -4, "error": "无法创建场景目录：%s" % e}

        # 描述留在 .cml 的 Tooltip 里，列表项读的就是它，改名时一并搬走
        desc = _lang_text(self, _cml_attr(
            _read_text(os.path.join(d, old_name + ".cml")), "Tooltip"))
        body = _read_text(path)
        # 用 lambda 而不是替换模板：名字里带 \ 或 $ 时模板会被解释
        new_body, n = re.subn(
            r'(<Profile\b[^>]*?\bName=")[^"]*(")',
            lambda m: m.group(1) + _xml_escape(new_name) + m.group(2),
            body, count=1, flags=re.I)
        if not n:
            return {"ok": False, "ret": -5,
                    "error": "场景文件结构异常，未找到 <Profile Name> 节点"}
        try:
            with io.open(os.path.join(wdir, new_name + ".profile"), "w",
                         encoding="utf-8", newline="\n") as f:
                f.write(new_body)
            with io.open(os.path.join(wdir, new_name + ".cml"), "w",
                         encoding="utf-8", newline="\n") as f:
                f.write(_build_cml(new_name, desc or new_name))
        except OSError as e:
            return {"ok": False, "ret": -6, "error": "写入失败：%s" % e}
        for ext in (".profile", ".cml"):
            p = os.path.join(d, old_name + ext)
            if os.path.isfile(p):
                try:
                    os.remove(p)
                except OSError:
                    pass
        # 改的恰好是「上次应用」的场景 → 同步恢复标记
        return {"ok": True, "ret": 0, "name": new_name,
                "from": old_name, "renamed": True}

    # ---------------- 汇总 ----------------
    def param_map(self, ep=EP_RENDER):
        """该端点全部 208 项 → {name: {r, v, w, t}}

        ★ 第 52 轮：渲染端点额外补上 CHANNEL_PARAMS（逐声道音量）。它们不是
        208 项真实属性（名字里带 "#"），所以不进 ALL_NAMES；但界面滑杆要按
        统一路径取值，必须出现在这张表里。
        """
        out = {}
        table = WRITABLE.get(ep, {})
        for name in ALL_NAMES:
            ret, val = self.read(name, ep)
            kind = table.get(name)
            out[name] = {
                "r": ret,
                "v": val,
                "w": kind is not None,
                "t": kind or _guess_type(val),
            }
        if ep == EP_RENDER:
            for name in sorted(CHANNEL_PARAMS):
                ret, val = self.read(name, ep)
                out[name] = {
                    "r": ret,
                    "v": val,
                    "w": table.get(name) is not None,
                    "t": table.get(name) or _guess_type(val),
                }
        return out

    def status(self):
        if self.lib is None:
            return {"ok": False, "error": self.error or "驱动未就绪"}

        def g(name):
            return self.read(name)[1]

        def num(name, cast=int, default=None):
            r, v = self.read(name)
            if r != 0 or not v:
                return default
            try:
                return cast(float(v))
            except (TypeError, ValueError):
                return default

        def mic_num(name, cast=int, default=None):
            """麦克风侧读数：必须走采集端点，走渲染端点一律返回 2。"""
            if self.cap_ctx is None:
                return default
            r, v = self.read(name, EP_CAPTURE)
            if r != 0 or not v:
                return default
            try:
                return cast(float(v))
            except (TypeError, ValueError):
                return default

        eq, eq_on = [], None
        for i in range(1, 11):
            r, v = self.read("EQ_Slider%d" % i)
            try:
                eq.append(int(float(v)) if r == 0 else 0)
            except (TypeError, ValueError):
                eq.append(0)
        r, v = self.read("Enable_EQ_GFX")
        if r == 0:
            eq_on = (v == "1")

        mute = self.read("MuteControl")[1] == "1"

        ch = num("DevChannelNumControl", int, 0)
        rate = num("DevSampleRateControl", int, 0)
        bits = num("DevBitDepthControl", int, 0)
        fmt = "%s 声道 / %s Hz / %s bit" % (
            ch or "?", rate or "?", bits or "?")
        rate_text = "%s Hz" % (rate or "?")
        try:
            rate_hz = float(rate)
            if rate_hz >= 1000:
                rate_text = "%g kHz" % (rate_hz / 1000.0)
                fmt = "%s 声道 / %s / %s bit" % (ch, rate_text, bits)
        except (TypeError, ValueError):
            pass

        return {
            "ok": True,
            "model": g("GetDeviceFriendlyName") or "未知设备",
            "codec": g("GetAudioCodecName") or "—",
            "driver": g("GetDriverVer") or "—",
            "firmware": g("GetFirmwareVer") or "—",
            "hwid": g("GetDeviceID") or "—",
            "dx": g("GetDirectXVer") or "—",
            "formats": fmt,
            "channels": ch,
            "rate_text": rate_text,
            "bit_depth": bits,
            "cpl": self.cpl,
            "dll": self.dll,
            "device_count": getattr(self, "device_count", 1),
            "capture_count": getattr(self, "cap_count", 0),
            "eq": eq,
            "eq_on": eq_on,
            "mute": mute,
            "room": num("RFX_ROOMSIZE", int, 0),
            "env": num("RFX_ENVIRONMENT", int, 0),
            "vol_scalar": num("VolumeScalarControl", float, None),
            "vol_db": num("VolumeControl", float, None),
            "vol_min": num("GetMinVol", float, -37.0),
            "vol_max": num("GetMaxVol", float, 0.0),
            "vol_step": num("GetVolStep", float, 1.0),
            "environments": ENVIRONMENTS,
            "room_sizes": ROOM_SIZES,
            "magic_voices": MAGIC_VOICES,
            "test_channels": [list(x) for x in TEST_CHANNELS],
            # ---- 麦克风侧（采集端点）----
            "mic_ready": self.cap_ctx is not None,
            "mic_vol_scalar": mic_num("VolumeScalarControl", float, None),
            "mic_vol_db": mic_num("VolumeControl", float, None),
            "mic_mute": mic_num("MuteControl", int, None),
            "mic_nr_on": mic_num("Enable_NOISEREDUCTION_LFX", int, None),
            "mic_nr_level": mic_num("NoiseReduction_Level", int, None),
            "mic_magic_on": mic_num("Enable_MAGICVOICE", int, None),
            "mic_magic_sel": mic_num("MagicVoice_Selection", int, None),
            "mic_echo_on": mic_num("Enable_MICECHO", int, None),
            "mic_echo_level": mic_num("MICECHO_Level", int, None),
            "mic_agc": mic_num("AGCControl", int, None),
            "mic_aa_vol": mic_num("AAVolumeControl", float, None),
            "mic_rate": mic_num("DevSampleRateControl", int, None),
            "dev_rate": rate,
        }

    def meter(self, ep=EP_RENDER):
        """实时电平（GetPeakMeter 是唯一本机可用的实时量）。
        渲染端点 8 声道，采集端点 1 声道。"""
        if self.lib is None or self.ctx_of(ep) is None:
            return {"ok": False}
        r, v = self.read("GetPeakMeter", ep)
        try:
            peak = float(v) if r == 0 else 0.0
        except (TypeError, ValueError):
            peak = 0.0
        return {"ok": r == 0, "peak": peak, "ep": ep,
                "channels": self.read("GetMeterChannelCount", ep)[1] or "?"}


# ====================================================================
# 四、HTTP 服务
# ====================================================================
# 组态（Profile）—— 完整复刻官方的「特效组态」系统
# ====================================================================
# 官方模型（实测 CPL 目录，非推测）：
#
#   CPL/profile/<名称>.cml       一个 RadioButton，GroupName="ProFileMenu"，
#                                Tooltip 存描述，Image.Check.Normal 存图标
#   CPL/profile/<名称>.profile   数据文件：
#                                <Profile Name="…">
#                                  <Endpoint Name="Speakers" DataFlow="Out">
#                                    <ApoEffect Name="SpeakerEQ">
#                                      <Property Name="EQ_Slider1" Value="6"
#                                                ValueType="System.Int32"
#                                                Args="-1" ArgsType="System.Int32"/>
#   CPL/profile/default/*        驱动自带的内置组态（本机只有 HiFi，体为空 =
#                                全部走 APO 默认，即「HiFi 模式」= 不叠加音效）
#   CPL/profile/UserDefine.cml   当前选中的自定义档标记
#   CPL/profile/UserDefineFormwork.cml   新增组态用的模板（Tooltip="Description"）
#   CPL/FactoryDefault.xml       原厂默认，官方 UI 的「预设」档
#
# 关键点：<Property Name> 用的就是 osConfLib.dll 的 208 项属性名，
# 所以这套格式可以直接落在我们的 PropertyControl 上，无需额外映射表。
#
# CPL 位于 Program Files 下**不可写**（实测 Permission denied），
# 因此用户自建组态写入 %APPDATA%\iKF Audio Center\profile\；
# 读取时两处都扫，驱动自带的内置档照常列出。
# --------------------------------------------------------------------

PROFILE_FACTORY = _MODEL["factory_profile"]   # 对应 FactoryDefault.xml（S1 起真源=TOML）
# 自建出厂基线场景名（2026-10-20 用户拍板由「原厂默认」改名「默认」）。
# ★ S1 起真源 = TOML [scene].factory_scene；与 Go 侧 profileFactoryScene 逐字对应
PROFILE_FACTORY_SCENE = _MODEL["factory_scene"]
# 内置场景名单：源包自带 + Go 侧 EnsureSeedScenes 播种。
# ★ builtin 判定按名字、不按目录（与 Go 侧 builtinScenes/builtinMark 逐项
#   对应）：便携包下用户自建场景与内置档同住 CPL\profile，按目录一刀切
#   会把用户场景也标成内置 —— 场景墙的可改可删判定、排序分层全部失真
#   （Go 侧 2026-10-19 真机确认过同样的坑）。
# ★ S1 起真源 = TOML [scene].builtin（7 项，含 HiFi）。
BUILTIN_SCENES = _MODEL["builtin"]
PROFILE_TEMPLATE = "UserDefineFormwork.cml"
PROFILE_CURRENT = "UserDefine.cml"

# --------------------------------------------------------------------
# 出厂默认值表 —— 设备侧自愈（heal_missing）的依据。
#
# 这 42 项（渲染 36 + 采集 6）属于「写后即可读」型：设备里没值时读回 ret=2
# （存在但无值），写过一次才变 ret=0 并跨进程持久。官方程序在位时，官方面板
# （安装时的 `/h /i` + 每次开机的 `/h /d`）会把它们维持在设备里；官方卸载器跑
# `XearAudioCenter /u /q` 会清掉设备侧的值，于是全部退回 ret=2，界面把开关
# 灰成「本机不支持」—— 看起来像「虚拟环绕 / 升降 Key 丢了」。
#
# 值取自官方安装状态的实测快照（2026-10-14 --dump，当时两端点合计 ret=0
# 有 104 项）。它们存在端点的 FxProperties
# （HKLM\SOFTWARE\...\MMDevices\Audio\Render\{GUID}\）里 —— 每组合一个
# C-Media 私有的 PROPERTYKEY，共 36 个值。所以「移植官方文件」治不了这个
# 病：值不在任何文件里，官方也是运行时写进去的。
#
# 与 Go 侧 iKF-Audio-Center-Wails/profile.go 的 factoryDefaults 逐项对应，
# 改一边必须同步另一边。
# --------------------------------------------------------------------
# ★ S2 起真源 = model/device-model.toml [[factory]]（42 项，装载时自校验）。
# 历史真值注释随数据走：EQ 全 0 = 官方预设索引 11（反编译）、
# RFX_ENVIRONMENT=0 = 官方预设口径（驱动原始出厂是 7，用户拍板按预设）。
FACTORY_DEFAULTS = [(EP_RENDER if kv[0] == EP_RENDER else EP_CAPTURE, kv[1], kv[2])
                    for kv in _MODEL["factory"]]

# 属性 → 官方 ApoEffect 分组名（写进 .profile 的 <ApoEffect Name>，
# 便于与官方面板的导出文件互相阅读）
#
# ★★ 第 51 轮（G 档）：分组名的依据终于拿到，整表归一到**页面 Canvas 名**。
#
#   证据 = 官方随驱动发的那份组态模板 `CPL/FactoryDefault.xml`（这是全机唯一一份
#   官方格式的 .profile 样本）。它写的是：
#       <ApoEffect Name="SpeakerSetting">   ←  apoSpeakerSetting.cml  Canvas Name="SpeakerSetting"
#       <ApoEffect Name="MicInVolume">      ←  apoMicInVol.cml       Canvas Name="MicInVolume"
#   两例全部吻合。exe 里 0x186A08~0x1875E0 那张 101 串的表
#   （`Headset* / HeadPhone* / Speaker* / SpeakerQuarter* / MicIn* / DesktopMic* /
#     LineIn* / SPDIFOut* / SPDIFIn* / HDMI* / StereoMix*` 全族）也是同一套
#   「端点族前缀 + 页名」命名，与第 50 轮查实的 `apo%s.cml` 拼名规则一致。
#   ⟹ **分组名 = 取值页面的 Canvas 名**，每一项都能独立验证
#      （文件就在 CPL/Skin/FaceLift/，Canvas 名在本模块注释里逐条标出）。
#
#   旧表里 11 个名字（Volume / Environment / Karaoke / KeyShift / SamplingRate /
#   ChannelCopy / XearSurroundSpeakers / NoiseReduction / XearMagicVoice / AAVolume
#   等）出自 C-Media 的**人读显示名**（`CMCplExt_108B.dll` 0x08DB28 起那张挂给
#   "Full Name" 列表框的表：Echo / Magic Voice / Noise Reduction / Karaoke /
#   Equalizer / Environment / Speaker Shifter …），官方面板按它读不出归属 ——
#   用这些名字导出的场景在官方面板里会「点了没反应」。整表已归一。
#   已存下来的旧场景文件由下面的 _GROUP_ALIAS 兜底，读回不受影响。
#
#   两个例外的保留名（都**没有**对应页面，改名无从谈起）：
#     · `GFSControl` —— 设备控制（HKCU\SOFTWARE\C-Media\Hook\EnableGFS，
#        第 46 轮取证），任何 .cml 里都没有它；
#     · `MicEcho` —— 旧面板 `CDlgMicEcho` 对话框，官方新面板没有这张页。
# ★ S4 起真源 = TOML [profile_groups]（53 条含 EQ_Slider 前缀键）。
# 组名出处/前缀规则/合并页口径见 TOML 注释；SS_* 前缀规则在 _group_of。
PROFILE_GROUPS = {ep: dict(tbl) for ep, tbl in _MODEL["profile_groups"].items()}

# 旧版工具写出过的分组名 → 归一到现在的 Canvas 名。
# **只用于读**：用户已经存下来的场景不该因为这次改名而在详情里显示成「其他」。
# ★ S1 起真源 = TOML [group_alias]（10 条，装载时自校验）。
_GROUP_ALIAS = _MODEL["alias"]

# 端点 → 官方 Endpoint 名 / DataFlow
# ★ S4 起真源 = TOML [profile_endpoint] / [profile_value_type]。
PROFILE_ENDPOINT = {ep: (v[0], v[1]) for ep, v in _MODEL["profile_endpoint"].items()}
PROFILE_VALUE_TYPE = dict(_MODEL["profile_value_type"])



def _appdata_profile_dir():
    base = os.environ.get("APPDATA") or os.path.expanduser("~")
    return os.path.join(base, "iKF Audio Center", "profile")


def _profile_dirs(drv):
    """返回 [(目录, 是否驱动自带)]，用户目录排前面（同名时优先）。"""
    out = [(_appdata_profile_dir(), False)]
    if drv and drv.cpl:
        out.append((os.path.join(drv.cpl, "profile", "default"), True))
        out.append((os.path.join(drv.cpl, "profile"), True))
    return out


def _profile_write_dir(drv):
    """用户自建组态写到哪。CPL 可写就用 CPL（与官方面板共享），否则回退。"""
    if drv and drv.cpl:
        cpl_prof = os.path.join(drv.cpl, "profile")
        if os.path.isdir(cpl_prof):
            try:
                t = os.path.join(cpl_prof, ".wtest")
                with open(t, "w"):
                    pass
                os.remove(t)
                return cpl_prof
            except OSError:
                pass
    d = _appdata_profile_dir()
    try:
        os.makedirs(d, exist_ok=True)
    except OSError:
        pass
    return d


def heal_missing(drv):
    """设备侧自愈：把「存在但当前无值」（ret=2）的项补上出厂值。

    这是官方行为的等价复刻 —— 官方靠安装时的 `/h /i` 与每次开机的
    `/h /d` 维持这些值；新框架只读不写，值一旦被清（卸载官方程序、
    换 USB 口、重装驱动都可能触发）就再也回不来，界面于是把开关灰成
    「本机不支持」。

    判据用 ret==2 而不是「值为空」：2 的语义正是「项存在、但设备侧还没
    有值」，也就是用户从没配过的项。ret=0（已有值）一律跳过 —— 绝不覆盖
    用户当前设置；ret=-1（项不存在）也不动，那是硬件本身的结论。

    返回实际补写的项数（供启动日志与自检）。与 Go 侧的
    profile.go:HealMissingParams 逐字段对应。
    """
    if drv is None or drv.lib is None:
        return 0
    # ★ 2026-10-22 第二版：补的范围从「出厂表 42 项」扩到**初始化清单**
    #   （55 项 = 出厂表 ∪ 「默认」档 ∪ 设备级）。以前那 12 项采集/新参数
    #   只能靠 _apply_scene_extras 补第二次，两套遍历各写各的；现在同一个
    #   执行器，strong=False 就是「只补 ret==2」，有值一个字不动。
    #   设备级（拓扑）由 ensure_channel_topology 负责，这里跳过。
    applied, _failed, _skipped = apply_init_manifest(
        drv, build_init_manifest(drv), strong=False)
    return applied


# ---------------- 驱动重置后自动回到「默认」场景（2026-10-20 用户拍板） ----------------
# 与 Go 侧 lastscene.go 逐字段对应。重置指纹命中 → 把出厂默认参数全量写回
# （=「默认」场景的状态；不依赖场景文件、幂等）。指纹不成立绝不写 —— 保护
# 用户手动调过但没存场景的参数。恢复跑在自愈之前（自愈补完默认值指纹就没了）。

LAST_APPLIED_RESET_THRESHOLD = 21   # FACTORY_DEFAULTS 过半即判定端点被重置


def _missing_default_count(drv):
    """重置指纹：FACTORY_DEFAULTS 表里当前 ret==2 的项数。"""
    n = 0
    for ep, name, _val in FACTORY_DEFAULTS:
        ret, _ = drv.read(name, ep)
        if ret == 2:
            n += 1
    return n


def restore_default_scene(drv, force=False):
    """驱动重置后的自动恢复：指纹命中 → 全量写回出厂默认参数。

    force=True 跳过指纹（排障兜底：指纹窗口被自愈提前跑掉时手动恢复）。
    返回 (是否触发写设备, 人读描述)。
    """
    if drv is None or drv.lib is None:
        return False, "驱动未就绪，无法检测"
    missing = _missing_default_count(drv)
    if not force and missing < LAST_APPLIED_RESET_THRESHOLD:
        return False, ("设备参数在位（出厂默认表缺省仅 %d/%d 项），无需恢复"
                       % (missing, len(FACTORY_DEFAULTS)))
    # ★ 2026-10-22 第二版：执行体不再「遍历出厂表 + 补一次场景档」，
    #   而是**按初始化清单**一次写完（见 build_init_manifest）。
    #   清单 = 出厂表 42 ∪ 「默认」档 37 ∪ 设备级 1 = 55 项。
    #   这里只做 param 项，拓扑交给 ensure_channel_topology —— 它才有
    #   「写后回读 + 重试」的抗端点重建窗口能力。
    #   strong=True：全量写，不看当前值（语义 = 自动应用「默认」场景）。
    items = build_init_manifest(drv)
    applied, failed, _skipped = apply_init_manifest(drv, items, strong=True)
    n_param = len([x for x in items if x["scope"] == "param"])
    if failed == 0:
        return True, ("检测到参数被驱动重置（缺省 %d/%d 项），已按初始化清单恢复"
                      "（写入 %d/%d 项）"
                      % (missing, len(FACTORY_DEFAULTS), applied, n_param))
    return True, ("按初始化清单恢复未完全成功（成功 %d / 失败 %d）"
                  % (applied, failed))


# ---------------- 初始化清单：初始化目标的唯一真相（2026-10-22） ----------------
# 与 Go 侧 initmanifest.go 逐字段对应。为什么要有它：在这之前「初始化该写
# 哪些项」散在五处（FACTORY_DEFAULTS 42 / 默认.profile 37 / _apply_scene_extras
# 补 12 / ensure_channel_topology 1 / seed 三哨兵），每次修一个漏洞就冒一个
# 新的。这里把并集算成一份可枚举的数据，链 / 核对命令 / 闸三方共用。
#
#   source  factory / scene / device —— 从哪来（排障时倒查）
#   scope   param / device           —— 执行策略不同：
#           param  安装级触发全量写，其它触发只补 ret==2（不覆盖用户调音）
#           device 任何触发都校正（拓扑是硬件事实，不是用户偏好）
# ★ S2 起真源 = TOML [[init_device]]（1 项，拓扑；scope=device 规则在解释器）。
INIT_DEVICE_ITEMS = [(EP_RENDER if kv[0] == EP_RENDER else EP_CAPTURE, kv[1], kv[2])
                     for kv in _MODEL["init_device"]]


def build_init_manifest(drv):
    """汇总初始化目标 = 出厂表 ∪ 「默认」场景档 ∪ 设备级。

    顺序固定（出厂 → 场景补差 → 设备级），去重靠 seen；同名以**出厂表为准**
    （两者值逐项一致，10-22 实测 25 项交集零冲突，但出厂表是硬编码、不会
    被场景文件被改而漂移）。场景档读不到就只给 42 + 设备级，不报错不编值。
    """
    out = []
    seen = set()

    def add(ep, name, val, source, scope):
        if (ep, name) in seen:
            return
        seen.add((ep, name))
        out.append({"ep": ep, "name": name, "val": str(val),
                    "source": source, "scope": scope})

    for ep, name, val in FACTORY_DEFAULTS:
        add(ep, name, val, "factory", "param")

    if drv is not None:
        try:
            path, _d, _b = drv._profile_path(PROFILE_FACTORY_SCENE)
        except Exception:
            path = ""
        items = []
        if path and os.path.isfile(path):
            try:
                items = _profile_items(_read_text(path))
            except Exception:
                items = []          # 只读目录 / 半就绪：清单退化成出厂表
        for _e, flow, _fx, name, val, _vt in items:
            if not name or "#" in name:
                continue            # 逐声道项不进场景档（与 profile_save 口径一致）
            ep = EP_CAPTURE if str(flow).lower() == "in" else EP_RENDER
            if WRITABLE.get(ep, {}).get(name) is None:
                continue            # 本机能力面没这项，写了也是白写
            add(ep, name, val, "scene", "param")

    for ep, name, val in INIT_DEVICE_ITEMS:
        add(ep, name, val, "device", "device")
    return out


def apply_init_manifest(drv, items=None, strong=False):
    """按清单执行。返回 (成功, 失败, 跳过)。

    strong=True  全量写（安装级触发 / 用户点默认）：不看当前值，一律写 ——
                 这是「装完驱动就该是默认状态」的唯一可靠实现。
    strong=False 只补 ret==2（普通启动）：有值一个字都不动，保护用户调音。

    设备级项（scope=device）不在这里写，走 ensure_channel_topology。
    """
    if drv is None or drv.lib is None:
        return 0, 0, 0
    if items is None:
        items = build_init_manifest(drv)
    applied = failed = skipped = 0
    for it in items:
        if it["scope"] == "device":
            continue
        ep, name, val = it["ep"], it["name"], it["val"]
        if not strong:
            ret, _cur = drv.read(name, ep)
            if ret != 2:
                skipped += 1
                continue        # 有值（0）或不存在（-1/-2）：都不是本次该改的
        kind = WRITABLE.get(ep, {}).get(name)
        try:
            res = drv.write_auto(name, val, ep, kind)
            if res.get("ok"):
                applied += 1
            else:
                failed += 1
        except Exception:
            failed += 1
    return applied, failed, skipped


def check_init_manifest(drv):
    """逐项核对设备与清单（只读，绝不写）。返回 (统计 dict, 明细行)。

    每项归四类：ok / missing（ret=2 从未写过）/ drift（有值但偏离目标）/
    unsupported（ret<0 本机没这项能力，不算失败）。
    """
    r = {"total": 0, "ok": 0, "missing": 0, "drift": 0, "unsupported": 0}
    lines = []
    items = build_init_manifest(drv)
    r["total"] = len(items)
    for it in items:
        ep, name, want = it["ep"], it["name"], it["val"]
        ret, cur = drv.read(name, ep)
        if ret == 0 and cur == "":
            ret, cur = drv.read(name, ep)      # 已知读抖动，重读一次
        if ret == 0 and str(cur) == want:
            r["ok"] += 1
            lines.append("  达标   %-7s %-32s %-8s %s" % (ep, name, want, it["source"]))
        elif ret == 2:
            r["missing"] += 1
            lines.append("  未设置 %-7s %-32s %-8s %s（存在但从未写过）"
                         % (ep, name, want, it["source"]))
        elif ret == 0:
            r["drift"] += 1
            lines.append("  偏离   %-7s %-32s 目标 %-6s 当前 %s"
                         % (ep, name, want, cur))
        else:
            r["unsupported"] += 1
            lines.append("  不支持 %-7s %-32s ret=%d" % (ep, name, ret))
    return r, lines


def identify_active_scene(drv):
    """设备真值场景识别：当前设备参数与哪个场景快照完全一致就返回哪个
    名字；都不一致返回 ""（自定义状态）。判据与 Go 侧 identifyActiveScene
    一致：只比快照实际存项；设备侧 ret!=0 视为不匹配；文本精确比对。
    （取代前端 localStorage 残留记忆 —— 2026-10-20 用户拍板：
    「使用中·X」必须跟设备真实状态走。）
    """
    if drv is None or drv.lib is None:
        return ""
    # ★ 不能走 drv.profiles() —— 那里面会调本函数，互相递归把接口挂死
    #（2026-10-20 实测）；直接枚举场景文件名。
    seen = set()
    names = []
    from_dir = _profile_dirs(drv)
    for d, _builtin in from_dir:
        if not os.path.isdir(d):
            continue
        try:
            fns = sorted(os.listdir(d))
        except OSError:
            continue
        here = []
        for fn in fns:
            if not fn.lower().endswith(".profile"):
                continue
            stem = fn[:-8]
            if not stem or stem + ".cml" in (PROFILE_TEMPLATE, PROFILE_CURRENT) \
                    or stem in seen:
                continue
            seen.add(stem)
            here.append(stem)
        names.extend(here)
    # ★ 2026-10-22：多个场景内容相同时，识别必须先判「默认」。
    # 当时 `音乐.profile` 与 `默认.profile` 逐项完全相同（默认就是古典乐曲线），
    # sorted() 按 UTF-8 排「音」在「默」之前，识别一命中就返回「音乐」。
    # ★ 同日改判：默认档 EQ 换成**官方预设 Default 的平直曲线**，音乐仍是
    #   古典乐 ⟹ 两档不再同分，这里只是兜底（用户自己把某档调得跟默认一样时
    #   仍然归到「默认」这个出厂态名字下）。与 Go 侧 profileNamesForIdentify 同口径。
    if PROFILE_FACTORY_SCENE in names:
        names.remove(PROFILE_FACTORY_SCENE)
        names.insert(0, PROFILE_FACTORY_SCENE)
    for name in names:
        path, _, _ = drv._profile_path(name)
        if not path or not os.path.isfile(path):
            continue
        items = _profile_items(_read_text(path))
        if not items:
            continue
        ok = True
        for _e, flow, _fx, pname, val, _vt in items:
            if not pname or "#" in pname:
                continue
            ep = EP_CAPTURE if flow.lower() == "in" else EP_RENDER
            ret, cur = drv.read(pname, ep)
            # ★ ret=0 但值列读空 = 已知读抖动：重读一次再判
            if ret == 0 and cur == "":
                ret, cur = drv.read(pname, ep)
            if ret != 0 or cur != val:
                ok = False
                break
        if ok:
            return name
    return ""


def cleanup_legacy_last_applied(drv):
    """上一轮「恢复上次场景」方案已废弃 —— 顺手删除遗留标记文件。"""
    wdir = _profile_write_dir(drv)
    if wdir:
        try:
            os.remove(os.path.join(wdir, "LastApplied.txt"))
        except OSError:
            pass


def migrate_profile_groups(drv):
    """把存量场景文件里的**旧 ApoEffect 名**就地换成官方 Canvas 名。

    为什么不能只靠 `_GROUP_ALIAS`
    ----------------------------
    别名只在**我们读**的时候兜底 —— 文件本身还是旧名，官方面板照旧读不出，
    用这些档的场景在官方面板里点了没反应。这里把文件改对才是真修好。

    改什么：只改 `<ApoEffect Name="…">` 的**属性值**。Property 的名字/值/
    端点/缩进/换行全部原样保留，所以这是一个纯字符串替换，不重新序列化。

    安全性：命中才写；写之前把原文件另存一份 `<name>.profile.bak`
    （每次覆盖，只保留最近一次），出问题可直接改名回退。

    返回 (改写的文件数, 改写的标签数)。与 Go 侧 MigrateProfileGroups 逐字段对应。
    """
    if drv is None:
        return 0, 0
    files = tags = 0
    for d, _builtin in _profile_dirs(drv):
        if not os.path.isdir(d):
            continue
        try:
            names = sorted(os.listdir(d))
        except OSError:
            continue
        for fn in names:
            if not fn.lower().endswith(".profile"):
                continue
            p = os.path.join(d, fn)
            try:
                with io.open(p, "r", encoding="utf-8-sig") as f:
                    txt = f.read()
            except (OSError, UnicodeError):
                continue
            hit = [0]

            def _sub(mo):
                ng = _GROUP_ALIAS.get(mo.group(2))
                if ng is None:
                    return mo.group(0)
                hit[0] += 1
                return mo.group(1) + ng + mo.group(3)

            new = re.sub(r'(<ApoEffect\s+Name=")([^"]*)(")', _sub, txt)
            if not hit[0] or new == txt:
                continue
            try:
                with io.open(p + ".bak", "w", encoding="utf-8") as f:
                    f.write(txt)
                with io.open(p, "w", encoding="utf-8") as f:
                    f.write(new)
            except OSError:
                continue          # 只读目录（如装到 Program Files）直接跳过
            files += 1
            tags += hit[0]
    return files, tags


# ---------------- 安装指纹：分清「驱动被重装过」与「普通启动」（10-22） --------
# 与 Go 侧 initfp.go 逐字段对应。
#
# 为什么要有它：前两轮把初始化收成了一份清单（INIT_DEVICE_ITEMS +
# build_init_manifest）和一条链（ensure_initial_state），但「什么时候该全量写」
# 一直是**启发式**：出厂表 42 项里缺省 >= 21 项才当成「驱动被重置」
# （LAST_APPLIED_RESET_THRESHOLD）。启发式测的是「值在不在」，用户说的是
# 「**驱动被重装过**」，两者不等价：驱动只装了一半（20/42 缺省）就不写；
# 驱动升级后把旧值搬回来也不写。
#
# 判据（2026-10-22 用户拍板：端点 GUID + 驱动版本）：
#   GetExtraInfo      -> {0.0.0.00000000}.{b2446ada-...}  渲染端点 ID
#   GetExtraInfo(cap) -> {0.0.1.00000000}.{3b3b5ea3-...}  采集端点 ID
#   GetDriverVer      -> 10.0.1.1
#   GetDeviceID       -> usb\vid_0d8c&pid_0012&mi_00\8&33013dbd&0&0000
# 都是 osConfLib 的只读查询，两条线现成能读，不碰注册表、不枚举 MMDevices
#（本机 iKF-V11 在 MMDevices 里有两个渲染 GUID，其中一个 {204114b8-...}
# 是上次安装留下的死端点 —— 光按设备名数会数到两个，GetExtraInfo 不会）。
#
# 三条安全规则（比判据本身更重要）：
#   1. 指纹文件不存在 != 重装 —— 首次运行只记基线，绝不因此全量写，
#      否则老用户升级一次就被清一次调音。全新机器由 21/42 启发式兜底。
#   2. 只有两边都非空且不同才算「变了」 —— 读抖动/采集端点没起来都表现为
#      空串，一律判「不知道」，宁可不动。
#   3. 字段新旧**合并**写，空串不覆盖旧值 —— 否则一次读失败就把基线擦掉。
INIT_FP_VER = 1
INIT_FP_NAME = "init-fingerprint.json"


def _appdata_state_dir():
    """用户级状态目录（%APPDATA%\\iKF Audio Center），与 profile 子目录同级。"""
    base = os.environ.get("APPDATA") or os.path.expanduser("~")
    return os.path.join(base, "iKF Audio Center")


def _init_fp_path():
    return os.path.join(_appdata_state_dir(), INIT_FP_NAME)


def _read_init_fp():
    """读指纹文件。第二个返回值 = 存在且可解析（结构版本不符也算没）。"""
    try:
        with io.open(_init_fp_path(), "r", encoding="utf-8") as f:
            fp = json.load(f)
    except (OSError, ValueError):
        return {}, False
    if not isinstance(fp, dict) or fp.get("ver") != INIT_FP_VER:
        return {}, False
    return fp, True


def _fp_field(drv, name, ep):
    """读一个只读查询项，读不到返回空串（重读一次抗已知抖动）。"""
    try:
        ret, v = drv.read(name, ep)
        if ret != 0 or not str(v or "").strip():
            ret, v = drv.read(name, ep)
    except Exception:
        return ""
    if ret != 0:
        return ""
    return str(v or "").strip()


def _init_fp_now(drv):
    """读当前指纹（只读，不落盘）。"""
    fp = {
        "ver": INIT_FP_VER,
        "endpoint": _fp_field(drv, "GetExtraInfo", EP_RENDER),
        "hwid": _fp_field(drv, "GetDeviceID", EP_RENDER),
        "driver": _fp_field(drv, "GetDriverVer", EP_RENDER),
        "model": _fp_field(drv, "GetDeviceFriendlyName", EP_RENDER),
        "capture": "",
    }
    if getattr(drv, "cap_ctx", None):
        fp["capture"] = _fp_field(drv, "GetExtraInfo", EP_CAPTURE)
    return fp


def _fp_known(fp):
    return bool(fp.get("endpoint") or fp.get("hwid") or fp.get("driver"))


def _fp_differs(a, b):
    """两边都非空且不同才算变了（安全规则 2）。"""
    a, b = str(a or ""), str(b or "")
    return bool(a) and bool(b) and a.lower() != b.lower()


def _short_fp(s):
    """端点 ID 太长，日志里只留 GUID 那一段。"""
    s = str(s or "")
    if "}." in s:
        return s.rsplit("}.", 1)[1]
    return s or "（空）"


def init_fp_changed(drv):
    """判定「这台机器上的驱动换过一茬」。返回 (是否该全量写, 原因)。"""
    if drv is None or drv.lib is None:
        return False, ""
    cur = _init_fp_now(drv)
    if not _fp_known(cur):
        return False, ""          # 三个字段全读不到：判不了，宁可不动用户参数
    old, ok = _read_init_fp()
    if not ok:
        return False, ""          # 安全规则 1：没有指纹 != 重装
    why = []
    if _fp_differs(old.get("endpoint"), cur["endpoint"]):
        why.append("渲染端点 %s → %s" % (_short_fp(old.get("endpoint")),
                                        _short_fp(cur["endpoint"])))
    if _fp_differs(old.get("capture"), cur["capture"]):
        why.append("采集端点 %s → %s" % (_short_fp(old.get("capture")),
                                        _short_fp(cur["capture"])))
    if _fp_differs(old.get("driver"), cur["driver"]):
        why.append("驱动版本 %s → %s" % (old.get("driver"), cur["driver"]))
    if _fp_differs(old.get("hwid"), cur["hwid"]):
        why.append("设备实例 %s → %s" % (old.get("hwid"), cur["hwid"]))
    if not why:
        return False, ""
    return True, "；".join(why)


def _save_init_fp(drv):
    """记录/刷新基线。新读到的覆盖旧的，但空串不覆盖（安全规则 3）。"""
    if drv is None or drv.lib is None:
        return False
    cur = _init_fp_now(drv)
    if not _fp_known(cur):
        return False              # 读不到就别写，免得把好基线擦成空
    old, _ok = _read_init_fp()
    merged = {"ver": INIT_FP_VER}
    for k in ("endpoint", "capture", "hwid", "driver", "model"):
        merged[k] = cur.get(k) or old.get(k) or ""
    merged["at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    try:
        d = _appdata_state_dir()
        if not os.path.isdir(d):
            os.makedirs(d)
        path = _init_fp_path()
        # 先写临时文件再 rename：半截 JSON 会让下次启动把基线当损坏
        with io.open(path + ".tmp", "w", encoding="utf-8") as f:
            f.write(json.dumps(merged, ensure_ascii=False, indent=2))
        os.replace(path + ".tmp", path)
        return True
    except OSError:
        return False


def init_fp_report(drv):
    """--init-fp 的人读报告（只读，不落盘）。"""
    lines = ["安装指纹（只读，不写设备）",
             "-" * 60,
             "指纹文件 : %s" % _init_fp_path()]
    old, ok = _read_init_fp()
    if ok:
        lines.append("已记录   : 端点 %s · 采集 %s"
                     % (_short_fp(old.get("endpoint")), _short_fp(old.get("capture"))))
        lines.append("           %s · %s · %s" % (old.get("driver"), old.get("hwid"),
                                               old.get("model")))
        lines.append("           记录于 %s" % old.get("at"))
    elif os.path.isfile(_init_fp_path()):
        lines.append("已记录   : （文件存在但无法解析 —— 下次启动会重记基线）")
    else:
        lines.append("已记录   : （无 —— 下次启动记基线，不写设备）")

    cur = _init_fp_now(drv)
    lines.append("当前     : 端点 %s · 采集 %s"
                 % (_short_fp(cur["endpoint"]), _short_fp(cur["capture"])))
    lines.append("           %s · %s · %s" % (cur["driver"], cur["hwid"], cur["model"]))
    lines.append("-" * 60)

    changed, why = init_fp_changed(drv)
    if not _fp_known(cur):
        lines.append("结论     : 读不到指纹字段，无法判定（不动用户参数）")
    elif changed:
        lines.append("结论     : 变了 —— 下次初始化走全量写（%s）" % why)
    elif not ok:
        lines.append("结论     : 无基线 —— 下次启动记录基线，设备不动")
    else:
        lines.append("结论     : 未变 —— 初始化只补空缺（ret=2），不覆盖调音")
    return "\n".join(lines)


def init_fp_summary(drv):
    """给 /api/driver 用的现状摘要（与 Go 侧 initFpSummary 同名同构）。"""
    cur = _init_fp_now(drv)
    old, ok = _read_init_fp()
    changed, why = init_fp_changed(drv)
    m = {
        "path": _init_fp_path(),
        "recorded": ok,
        "changed": changed,
        "current": {"endpoint": cur["endpoint"], "capture": cur["capture"],
                    "driver": cur["driver"], "hwid": cur["hwid"],
                    "model": cur["model"]},
    }
    if ok:
        m["baseline"] = {k: old.get(k) or "" for k in
                         ("endpoint", "capture", "driver", "hwid", "model", "at")}
    if why:
        m["reason"] = why
    return m


def ensure_channel_topology(drv):
    """声道拓扑必须是**立体声（2 声道）**。与 Go 侧 initstate.go 逐字段对应。

    两种局面都要处理：
      ① ret != 0（没有记忆值）—— 装完驱动 / 端点重建后的常态；
      ② 值为 4/6/8 —— 声道拓扑是驱动服务级状态，重启/重新枚举后会被复位
         成驱动的默认 5.1；本机耳机硬件就是立体声，纯属驱动默认值作祟。

    ★ 写后必须**回读确认**，不能只看写入返回码。用户虚拟机实测：装完驱动
    首次启动声道组不是立体声，退出主程序再打开就对了 —— 那次写入落在端点
    重建窗口里被丢弃，而写入本身返回成功。所以最多重试 3 次、间隔 200ms，
    只有回读到 "2" 才算成功。除 2/4/6/8 之外的怪值一律不猜。

    返回 (是否写入成功, 人读描述)。
    """
    if drv is None or drv.lib is None:
        return False, ""

    def _stereo(s):
        return str(s or "").strip() in ("2", "2.0")

    ret, v = drv.read("Speaker_Channel_Setting", EP_RENDER)
    if ret == 0 and v == "":
        ret, v = drv.read("Speaker_Channel_Setting", EP_RENDER)   # 已知读抖动，重读一次
    if ret == 0 and _stereo(v):
        return False, ""
    if ret != 0:
        why = "无记忆值（驱动重装/端点重建）"
    elif str(v).strip() in ("4", "6", "8"):
        why = "驱动复位成 %s" % v
    else:
        return False, ""
    for i in range(3):
        try:
            drv.write_auto("Speaker_Channel_Setting", 2, EP_RENDER, "int")
        except Exception:
            pass
        r, w = drv.read("Speaker_Channel_Setting", EP_RENDER)
        if r == 0 and _stereo(w):
            return True, "%s → 立体声 2（第 %d 次写入生效）" % (why, i + 1)
        time.sleep(0.2)
    return False, "%s → 立体声 2 写入 3 次均未生效（端点可能仍在重建）" % why


def ensure_initial_state(drv, full=False):
    """连上驱动后必须跑一次的初始化链。与 Go 侧 initstate.go:EnsureInitialState 对应。

    为什么抽成一个函数：**启动路径**和「重新检测」/装驱动重连做的是同一件事
    （把设备恢复到可用起点 = 「默认」场景状态），以前只有启动路径跑它，于是
    用户最常见的那条路径正好错过：

        装驱动 → 主程序已在跑（或启动那刻驱动还没装好）→ open 失败/半就绪
        → 启动时的 restore_default_scene 因未就绪直接返回
        → 用户点「重新检测」→ reopen 成功 → 没人再跑恢复
        → 设备全 ret=2，界面一片「未设置」，必须自己去场景页点「默认」

    2026-10-22 用户实测：「得我去自定义场景点默认然后所有参数才会按照默认去设定」。

    幂等性由被调函数各自保证，可反复跑：restore_default_scene 有指纹保护
    （缺省 < 21/42 项一个字都不写）、heal_missing 只补 ret==2。

    full=True 给启动路径用，多做一次性的事（清废弃标记文件、场景分组名就地
    升级）；重连路径不需要。

    返回非空 dict 供 HTTP 响应体透传（{"restored"/"healed"/"migrated"}）。
    """
    out = {}
    if drv is None or drv.lib is None:
        # 没连上就什么都别碰。半就绪时采集端点读回 -9（不是 2），指纹会算错。
        return out

    if full:
        cleanup_legacy_last_applied(drv)

    # ★ 2026-10-22：声道拓扑排在最前，且放进这条链里 —— 以前只挂在启动
    #   路径上（Python 线甚至一直没有），装完驱动点「重新检测」连上后
    #   没人补，设备页声道组就不是立体声。放最前还有一个原因：切拓扑可能
    #   触发端点属性重建，必须在写参数**之前**把这件事做完。
    topo_changed, topo_msg = ensure_channel_topology(drv)
    if topo_changed:
        out["topology"] = topo_msg
        print("[初始化] 声道拓扑：%s" % topo_msg)
        time.sleep(0.15)      # 给驱动一点时间把属性落稳
    elif topo_msg:
        print("[初始化] 声道拓扑：%s" % topo_msg)   # 没写成功也要留痕

    # ★ 2026-10-22 第三版：先问「驱动换过一茬没有」（init_fp_changed）。
    #   以前唯一的判据是 21/42 启发式（缺省过半才算被重置），它测的是
    #   「值在不在」，而用户要的是「驱动被重装过」—— 两者不等价：驱动装
    #   了一半（20/42 缺省）就不写，驱动升级后把旧值搬回来也不写。现在
    #   指纹（端点 GUID + 驱动版本 + 设备实例）变化 ⟹ 直接全量写。
    #
    #   顺序铁律不变：恢复必须在自愈**之前**（自愈把 ret=2 补成默认值后，
    #   「端点被重置」的启发式就再也测不出来了，2026-10-20）。
    fp_changed, fp_why = init_fp_changed(drv)
    full_write_ok = True
    if fp_changed:
        items = build_init_manifest(drv)
        applied, failed, _skipped = apply_init_manifest(drv, items, strong=True)
        full_write_ok = (failed == 0)
        n_param = len([x for x in items if x["scope"] == "param"])
        msg = ("安装指纹变化（%s），已按初始化清单全量写回（成功 %d / 失败 %d / 共 %d 项）"
               % (fp_why, applied, failed, n_param))
        out["restored"] = True
        out["restore_reason"] = fp_why
        out["restore_msg"] = msg
        print("[初始化] %s" % msg)
    else:
        triggered, msg = restore_default_scene(drv)
        if triggered:
            out["restored"] = True
            out["restore_msg"] = msg
            print("[初始化] 场景自动恢复：%s" % msg)

    healed = heal_missing(drv)
    if healed > 0:
        out["healed"] = healed
        print("[初始化] 设备侧自愈：补写了 %d 项出厂默认值" % healed)

    if full:
        mf, mt = migrate_profile_groups(drv)
        if mf > 0:
            out["migrated"] = mf
            print("[初始化] 场景分组名升级：%d 个文件的 %d 个 <ApoEffect> 换成官方页名"
                  % (mf, mt))

    # 指纹落盘 = 「这台机器这个驱动版本，我已经初始化过了」。放最后：前面
    # 所有写设备的动作都结束了，记下的才是真实现状。
    # ★ 全量写有失败项时**不落指纹** —— 下次启动会再判一次「变了」，重试
    #   那一轮（补空缺紧跟在上一步跑过，能补的都补了）。
    # ★ 首次运行（无基线）也走这里：只记基线、不写设备（安全规则 1），
    #   所以老用户升级上来调音不会被清。
    if fp_changed and not full_write_ok:
        print("[初始化] 全量写有失败项，指纹暂不更新（下次启动重试）")
    else:
        _save_init_fp(drv)
    return out


def _read_text(path):
    for enc in ("utf-8-sig", "utf-16", "latin-1"):
        try:
            with io.open(path, "r", encoding=enc) as f:
                return f.read()
        except (OSError, UnicodeError):
            continue
    return ""


_LANG_CACHE = {}


def _lang_map(drv):
    """CPL/lang/zh-CN/*.xaml → {ml_键: 中文}。

    .cml 里 Tooltip 常直接写资源键（例如内置组态 HiFi 的 Tooltip 是
    ml_PanelProfile_HiFi），官方 UI 靠 XAML 资源字典解析。
    这里把整份中文资源读进来，用于把键还原成官方文案。
    """
    if "m" in _LANG_CACHE:
        return _LANG_CACHE["m"]
    out = {}
    if drv and drv.cpl:
        d = os.path.join(drv.cpl, "lang", "zh-CN")
        if os.path.isdir(d):
            try:
                names = sorted(os.listdir(d))
            except OSError:
                names = []
            for fn in names:
                if not fn.lower().endswith(".xaml"):
                    continue
                txt = _read_text(os.path.join(d, fn))
                for m in re.finditer(r'x:Key="([^"]+)"\s*>([^<]*)<', txt):
                    out[m.group(1)] = m.group(2).strip()
    _LANG_CACHE["m"] = out
    return out


def _lang_text(drv, s):
    """把 ml_* 资源键还原成官方中文；不是键就原样返回。"""
    if not s:
        return s
    if s.startswith("ml_"):
        return _lang_map(drv).get(s, s)
    return s


def _cml_attr(text, attr):
    m = re.search(r'\b%s\s*=\s*"([^"]*)"' % re.escape(attr), text)
    return m.group(1).strip() if m else ""


def _profile_items(text):
    """解析 .profile / FactoryDefault.xml
    → [(端点名, DataFlow, 效果名, 属性, 值, ValueType)]"""
    items = []
    ep_name, flow, fx = "", "", ""
    for m in re.finditer(r"<(/?)(Profile|Endpoint|ApoEffect|Property)\b([^>]*?)(/?)>",
                         text, re.I):
        closing, tag, attrs, self_close = m.group(1), m.group(2).lower(), \
            m.group(3), m.group(4)
        if tag == "endpoint":
            if closing:
                ep_name, flow = "", ""
            else:
                ep_name = _cml_attr(attrs, "Name")
                flow = _cml_attr(attrs, "DataFlow")
        elif tag == "apoeffect":
            if closing:
                fx = ""
            else:
                fx = _cml_attr(attrs, "Name")
        elif tag == "property" and not closing:
            items.append((ep_name, flow, fx,
                          _cml_attr(attrs, "Name"),
                          _cml_attr(attrs, "Value"),
                          _cml_attr(attrs, "ValueType")))
    return items


def _kind_of_value_type(vtype, name):
    """官方 ValueType（.NET 类型名）→ 我们的 kind。"""
    v = (vtype or "").strip()
    if v.endswith("Single") or v.endswith("Double"):
        return "float"
    if name in GENERIC_SET and v.endswith("UInt32"):
        # 通用表里的 UInt32 基本就是 0/1 开关
        return "bool"
    return "int"


def _coerce(val, kind):
    """组态文件里的文本值 → Python 数值。不合法返回 None。"""
    try:
        f = float(val)
    except (TypeError, ValueError):
        return None
    if kind == "float":
        return f
    if kind == "bool":
        return 1 if f != 0 else 0
    return int(f)


def _xml_escape(s):
    return (str(s).replace("&", "&amp;").replace("<", "&lt;")
            .replace(">", "&gt;").replace('"', "&quot;"))


def _group_of(ep, name):
    # ★ 第 52 轮：先剥掉 "#<声道索引>" 后缀再查表 —— 逐声道音量归属与
    # 主音量同一张官方页（SpeakerVolume / apoSpeakerVol.cml）。
    base, _ch = _split_channel(name)
    if base != name:
        name = base
    tbl = PROFILE_GROUPS.get(ep, {})
    if name in tbl:
        return tbl[name]
    if name.startswith("EQ_Slider"):
        return "SpeakerEQ"
    # 7.1 虚拟音箱摆位的 8 组 SS_*_ANGLE / SS_*_DIST（第 49 轮，D 档）：
    # 它们在 WRITABLE 里是循环登记的，逐个写进 PROFILE_GROUPS 会重复 16 行，
    # 且以后加音箱还会漏。这里用前缀兜住，等价于映射到 apoSpeakerVirtual.cml。
    if name.startswith("SS_") and (name.endswith("_ANGLE")
                                   or name.endswith("_DIST")):
        return "SpeakerVirtual"
    return "General"


# 官方 ApoEffect 分组名（= 页面 Canvas 名）→ 界面短标签。
# 与 Wails 版 profile.go 的 groupLabel 逐项一致，两条线的场景详情必须同文案。
# 一张官方页住着多个模块时（Karaoke 页有人声消除+升降调、Setting 页有
# 声道配置/互换/复制、MicInVol 页有麦克风音量+模拟功放），标签写成合并描述 ——
# 官方本来就按「页」归属，拆开反而与官方 .profile 对不上。
# ★ S4 起真源 = TOML [group_label]（28 项，含只读两组）。
_GROUP_LABEL = dict(_MODEL["group_label"])

# 渲染端点的分组展示顺序（照官方面板菜单：音量→EQ→环境→环绕一族→人声→
# Xear 音效族→设定/采样率；采集端点整体排在后面）。
# ★ S4 起真源 = TOML [display].group_order（27 项；采集端 rank+1000 在 _group_rank）。
_GROUP_ORDER = tuple(_MODEL["display"]["group_order"])


def _profile_ep_of(flow):
    """组态文件里的 DataFlow → 我们的端点名。"""
    return EP_CAPTURE if (flow or "").strip().lower() == "in" else EP_RENDER


def _group_rank(ep, key):
    """排序键：渲染端点在前，表外分组排在已知分组之后。"""
    base = 1000 if ep == EP_CAPTURE else 0
    try:
        return base + _GROUP_ORDER.index(key)
    except ValueError:
        return base + 500


def _profile_groups(items):
    """一份场景的内容 → [{"key","label","ep","n"}]，已按展示顺序排好。

    同一个 (端点, 属性) 只算一次 —— FactoryDefault.xml 里同一属性会在
    Speakers / SpeakerQuarter / Mic 下重复出现。
    """
    idx, order, seen = {}, [], set()
    for _ep_name, flow, fx, pname, _val, _vt in items:
        if not pname:
            continue
        ep = _profile_ep_of(flow)
        nk = (ep, pname)
        if nk in seen:
            continue
        seen.add(nk)
        g = fx or _group_of(ep, pname)
        g = _GROUP_ALIAS.get(g, g)      # 旧版工具写出的分组名 → Canvas 名
        key = (ep, g)
        if key in idx:
            idx[key]["n"] += 1
            continue
        idx[key] = {"key": g, "label": _GROUP_LABEL.get(g, g), "ep": ep, "n": 1}
        order.append(key)
    out = [idx[k] for k in order]
    out.sort(key=lambda x: (_group_rank(x["ep"], x["key"]), x["key"]))
    return out


def _profile_group_labels(items, limit=4):
    """场景卡片的参数摘要：分组短标签，最多 limit 个。"""
    return [g["label"] for g in _profile_groups(items)][:limit]


def _build_profile_xml(name, snapshot):
    """snapshot = [(ep, 属性名, 类型, 值字符串)] → 官方 schema 文本"""
    lines = ['<Profile Name="%s">' % _xml_escape(name)]
    for ep in ENDPOINTS:
        rows = [r for r in snapshot if r[0] == ep]
        ep_name, flow = PROFILE_ENDPOINT[ep]
        lines.append('\t<Endpoint Name="%s" DataFlow="%s">' % (ep_name, flow))
        order, groups = [], {}
        for _, pname, kind, val in rows:
            g = _group_of(ep, pname)
            if g not in groups:
                groups[g] = []
                order.append(g)
            groups[g].append((pname, kind, val))
        for g in order:
            lines.append('\t\t<ApoEffect Name="%s">' % g)
            for pname, kind, val in groups[g]:
                vtype = PROFILE_VALUE_TYPE.get(kind, "System.Int32")
                # Args 用真实声道索引：OEM 属性走 -1（主声道），通用属性 0
                args = "0" if pname in GENERIC_SET else "-1"
                lines.append(
                    '\t\t\t<Property Name="%s" Value="%s" UIName="" '
                    'ValueType="%s" Args="%s" ArgsType="System.Int32"/>'
                    % (pname, _xml_escape(val), vtype, args))
            lines.append('\t\t</ApoEffect>')
        lines.append('\t</Endpoint>')
    lines.append('</Profile>')
    return "\n".join(lines) + "\n"


def _build_cml(name, desc):
    """新增组态时写出的 RadioButton（结构照抄官方的 UserDefineFormwork.cml）"""
    return (
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '\t\t<RadioButton Name="%s" GroupName="ProFileMenu" Left="0" Top="0" '
        'Width="42" Height="41" Cursor="Hand" '
        'Image.Uncheck.Normal="PanelProfile/UserDefine_no.png" '
        'Image.Check.Normal="PanelProfile/UserDefine_se.png" '
        'Image.Check.MouseOver="PanelProfile/UserDefine_se.png" '
        'Image.Uncheck.MouseOver="PanelProfile/UserDefine_mo.png" '
        'Tooltip="%s" Tooltip.Check="%s" />\n'
        % (_xml_escape(name), _xml_escape(desc), _xml_escape(desc)))


# ====================================================================
# 系统回环采集（WASAPI loopback → 频谱）
# --------------------------------------------------------------------
# 与 Wails 线（iKF-Audio-Center-Wails/loopback.go）**逐字段、逐字节对齐**。
# 改这里必须同步改那边，验收方式是两边的 `--loopback-fft` 输出一致。
#
# 链路
# ----
#   IMMDeviceEnumerator.GetDefaultAudioEndpoint(eRender, eConsole)
#     或 GetDevice(id) —— 按端点 ID 锁定某个输出设备（仅耳机模式）
#     → IMMDevice.Activate(IID_IAudioClient)
#     → IAudioClient.GetMixFormat()          ← 共享模式必须用混音格式
#     → IAudioClient.Initialize(SHARED, LOOPBACK, 200ms, 0, mix)
#     → IAudioClient.GetService(IID_IAudioCaptureClient)
#     → IAudioClient.Start()
#     → 轮询 GetNextPacketSize / GetBuffer / ReleaseBuffer
#     → 混成单声道 float32 → 环形缓冲 → 每 20ms 一次 1024 点 FFT → 512 字节
#
# 为什么手写 vtable
# ----------------
# 单文件线不能有第三方依赖（无 numpy / comtypes），所以直接用 ctypes 走
# 虚表。COM 的虚表布局是稳定 ABI，照头文件抄下标即可。
#
# ★ 两个必须记住的坑（Go 那边踩过一遍）
# ------------------------------------
# ① COM 接口指针指向的是「虚表指针」，不是虚表本身：
#      fn = (*(void***)p)[idx]
#    少解一层引用就会跳到 p + idx*8 那片内存里的垃圾值上，
#    进程直接访问违例（Python 会整个崩掉，try/except 拦不住）。
# ② WAVEFORMATEX 是 18 字节，但 ctypes 默认按 4 字节对齐会把它补到 20，
#    嵌进 WAVEFORMATEXTENSIBLE 后 SubFormat 整体后移 2 字节，格式判定落到
#    「未知」，症状是「帧数照涨、频谱恒 0」。所以下面两个结构体都平铺写全。
# ====================================================================


class _GUID(ctypes.Structure):
    _fields_ = [("Data1", ctypes.c_uint32),
                ("Data2", ctypes.c_uint16),
                ("Data3", ctypes.c_uint16),
                ("Data4", ctypes.c_ubyte * 8)]


# CLSID_MMDeviceEnumerator / 三个 IID
CLSID_MMDEVICE_ENUMERATOR = _GUID(
    0xBCDE0395, 0xE52F, 0x467C,
    (ctypes.c_ubyte * 8)(0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E))
IID_IMMDEVICE_ENUMERATOR = _GUID(
    0xA95664D2, 0x9614, 0x4F35,
    (ctypes.c_ubyte * 8)(0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6))
IID_IAUDIO_CLIENT = _GUID(
    0x1CB9AD4C, 0xDBFA, 0x4C32,
    (ctypes.c_ubyte * 8)(0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2))
IID_IAUDIO_CAPTURE_CLIENT = _GUID(
    0xC8ADBD64, 0xE71E, 0x48A0,
    (ctypes.c_ubyte * 8)(0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17))
IID_IAUDIO_RENDER_CLIENT = _GUID(       # 内嵌监听（listen.py 对应 listen.go）
    0xF294ACFC, 0x3146, 0x4483,
    (ctypes.c_ubyte * 8)(0xA7, 0xBF, 0xAD, 0xDC, 0xA7, 0xC2, 0x60, 0xE2))

# 虚表下标（0/1/2 是 IUnknown 的 QueryInterface/AddRef/Release）
VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT = 4
VTBL_IMMDEVICE_ENUM_ENDPOINTS = 3       # IMMDeviceEnumerator：列出全部活动端点
VTBL_IMMDEVICE_GET_DEVICE = 5           # IMMDeviceEnumerator：按端点 ID 取设备
VTBL_IMMDEVICE_ACTIVATE = 3
VTBL_IMMDEVICE_OPEN_PROP_STORE = 4      # IMMDevice：开属性仓库（拿友好名）
VTBL_IMMDEVICE_GET_ID = 5               # IMMDevice：拿端点 ID
VTBL_COLL_GET_COUNT = 3                 # IMMDeviceCollection
VTBL_COLL_ITEM = 4                      # IMMDeviceCollection
VTBL_PROP_STORE_GET_VALUE = 5           # IPropertyStore
VTBL_AUDIOCLIENT_INITIALIZE = 3
VTBL_AUDIOCLIENT_GET_MIX_FORMAT = 8
VTBL_AUDIOCLIENT_START = 10
VTBL_AUDIOCLIENT_STOP = 11
VTBL_AUDIOCLIENT_GET_SERVICE = 14
VTBL_CAPTURE_GET_BUFFER = 3
VTBL_CAPTURE_RELEASE_BUFFER = 4
VTBL_CAPTURE_GET_NEXT_PACKET_SIZE = 5
VTBL_AUDIOCLIENT_GET_BUFFER_SIZE = 4    # IAudioClient：缓冲总帧数
VTBL_AUDIOCLIENT_GET_CURRENT_PADDING = 6  # IAudioClient：未播放帧数
VTBL_RENDER_GET_BUFFER = 3              # IAudioRenderClient
VTBL_RENDER_RELEASE_BUFFER = 4
VTBL_RELEASE = 2

COINIT_MULTITHREADED = 0x0
CLSCTX_ALL = 0x17
AUDCLNT_SHARE_SHARED = 0
AUDCLNT_LOOPBACK = 0x00020000
AUDCLNT_SILENT = 0x00000002
# 2026-10-20 内嵌监听：允许按与端点混音格式不同的格式初始化采集端
# （引擎内部转换），两边得以用「渲染端点混音格式」直通搬运。
AUDCLNT_AUTO_CONVERT = 0x80000000       # Win8.1+
AUDCLNT_SRC_DEFAULT_QUALITY = 0x08000000
E_RENDER = 0
E_CONSOLE = 0
DEVICE_STATE_ACTIVE = 1                 # 只列「在」的端点
STGM_READ = 0                           # 属性仓库只读
VT_LPWSTR = 31                          # PROPVARIANT.vt == VT_LPWSTR

# Web Audio AnalyserNode 的默认参数与归一化，与 loopback.go 完全一致。
# ★ waMagScale = 1/N 是**实测反解**出来的（Chromium 的 AnalyserNode 用
#   |X[k]|/N，满幅正弦读 −13.56 dB，不是规范正文那句「0 dB」）。
#   详见 iKF-Audio-Center-Wails/loopback.go 里 waMagScale 的注释。
WA_MIN_DB = -100.0
WA_MAX_DB = -30.0
WA_SMOOTHING = 0.8
BLACKMAN_GAIN = 0.42
LB_RING_LEN = 8192
LB_FFT_SIZE = 1024
LB_BINS = LB_FFT_SIZE // 2
LB_PERIOD = 0.020

_ole32 = ctypes.WinDLL("ole32.dll")
_ole32.CoInitializeEx.restype = ctypes.c_long
_ole32.CoInitializeEx.argtypes = [ctypes.c_void_p, ctypes.c_uint32]
_ole32.CoUninitialize.restype = None
_ole32.CoCreateInstance.restype = ctypes.c_long
_ole32.CoCreateInstance.argtypes = [
    ctypes.POINTER(_GUID), ctypes.c_void_p, ctypes.c_uint32,
    ctypes.POINTER(_GUID), ctypes.POINTER(ctypes.c_void_p)]
_ole32.CoTaskMemFree.restype = None
_ole32.CoTaskMemFree.argtypes = [ctypes.c_void_p]
_ole32.PropVariantClear.restype = ctypes.c_long
_ole32.PropVariantClear.argtypes = [ctypes.c_void_p]

# 按参数个数缓存 WINFUNCTYPE 原型：每次调用都构造一遍太慢，
# FFT 20ms 一拍、每拍还要调好几次 GetBuffer/ReleaseBuffer。
_PROTO_CACHE = {}


def _com_proto(nargs):
    p = _PROTO_CACHE.get(nargs)
    if p is None:
        p = ctypes.WINFUNCTYPE(ctypes.c_long, ctypes.c_void_p,
                               *([ctypes.c_void_p] * nargs))
        _PROTO_CACHE[nargs] = p
    return p


def _com_call(ptr, idx, *args):
    """按虚表下标调一次 COM 方法，返回 HRESULT（已转成有符号）。

    ★ COM 接口指针指向的是虚表指针，必须解两层（见本节头部的坑 ①）。
    """
    vtbl = ctypes.cast(ptr, ctypes.POINTER(ctypes.c_void_p))[0]
    fn = ctypes.cast(vtbl, ctypes.POINTER(ctypes.c_void_p))[idx]
    return _com_proto(len(args))(fn)(ptr, *args)


def _com_release(ptr):
    if ptr:
        try:
            _com_call(ptr, VTBL_RELEASE)
        except Exception:
            pass


def _hres_bad(hr):
    return hr < 0


# ---- 端点枚举辅助（与 loopback.go 的 propKey/propVariant 对应） ----

class _PROPERTYKEY(ctypes.Structure):
    _fields_ = [("fmtid", _GUID), ("pid", ctypes.c_uint32)]


class _PROPVARIANT(ctypes.Structure):
    # x64 上值联合体固定在偏移 8（vt 2 字节 + 保留 6 字节），拿 pwszVal 就够
    _fields_ = [("vt", ctypes.c_uint16),
                ("_pad", ctypes.c_ubyte * 6),
                ("val", ctypes.c_void_p)]


# PKEY_Device_FriendlyName —— 端点友好名（"Speakers (C-Media USB Audio…)"）
PKEY_DEVICE_FRIENDLYNAME = _PROPERTYKEY(
    _GUID(0xA45C254E, 0xDF1C, 0x4EFD,
          (ctypes.c_ubyte * 8)(0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0)),
    14)


def _imm_device_id(dev):
    """拿端点 ID（形如 {0.0.1.00000000}.{guid}）。

    ID 字符串由 COM 分配，读完必须 CoTaskMemFree。失败返回空串。
    """
    pwsz = ctypes.c_void_p()
    hr = _com_call(dev, VTBL_IMMDEVICE_GET_ID, ctypes.byref(pwsz))
    if _hres_bad(hr) or not pwsz.value:
        return ""
    try:
        return ctypes.wstring_at(pwsz.value)
    finally:
        _ole32.CoTaskMemFree(pwsz)


def _imm_device_name(dev):
    """拿端点友好名。失败返回空串 —— 名字只是展示，拿不到不该拖垮采集。"""
    store = ctypes.c_void_p()
    hr = _com_call(dev, VTBL_IMMDEVICE_OPEN_PROP_STORE, STGM_READ,
                   ctypes.byref(store))
    if _hres_bad(hr) or not store.value:
        return ""
    try:
        pv = _PROPVARIANT()
        hr = _com_call(store, VTBL_PROP_STORE_GET_VALUE,
                       ctypes.byref(PKEY_DEVICE_FRIENDLYNAME), ctypes.byref(pv))
        if _hres_bad(hr):
            return ""
        try:
            if pv.vt == VT_LPWSTR and pv.val:
                return ctypes.wstring_at(pv.val)
            return ""
        finally:
            _ole32.PropVariantClear(ctypes.byref(pv))
    finally:
        _com_release(store)


class _WAVEFORMATEX(ctypes.Structure):
    _fields_ = [("wFormatTag", ctypes.c_uint16),
                ("nChannels", ctypes.c_uint16),
                ("nSamplesPerSec", ctypes.c_uint32),
                ("nAvgBytesPerSec", ctypes.c_uint32),
                ("nBlockAlign", ctypes.c_uint16),
                ("wBitsPerSample", ctypes.c_uint16),
                ("cbSize", ctypes.c_uint16)]


class _WAVEFORMATEXTENSIBLE(ctypes.Structure):
    # 平铺写全，不嵌入 _WAVEFORMATEX —— 见本节头部的坑 ②
    _fields_ = [("wFormatTag", ctypes.c_uint16),        # 0
                ("nChannels", ctypes.c_uint16),         # 2
                ("nSamplesPerSec", ctypes.c_uint32),    # 4
                ("nAvgBytesPerSec", ctypes.c_uint32),   # 8
                ("nBlockAlign", ctypes.c_uint16),       # 12
                ("wBitsPerSample", ctypes.c_uint16),    # 14
                ("cbSize", ctypes.c_uint16),            # 16
                ("wValidBitsPerSample", ctypes.c_uint16),  # 18
                ("dwChannelMask", ctypes.c_uint32),     # 20
                ("SubFormat", _GUID)]                   # 24


def _lb_downmix(ch):
    """多声道怎么变成一条波形。

    ★ 只取前左/前右两声道求平均，**不是**对全部声道求平均。
    前端另一条音源（getDisplayMedia 屏幕捕获）拿到的是立体声流，
    操作系统已经把 7.1 设备混成了两声道；这边若按 8 声道平均，
    同一段音乐两条路径会差 20·log10(8/2) ≈ 12 dB ≈ 44 个字节，
    表现为「切到后端回环地形突然矮一截」。与 loopback.go 的 lbDownmix 一致。
    """
    if ch >= 2:
        return 2, 0.5
    return 1, 1.0


class Loopback(object):
    """一路系统回环采集会话。

    线程模型：采集跑在一个专用线程上。COM 的单元（apartment）状态是
    线程局部的 —— CoInitializeEx 在哪个线程调的，接口指针就必须在哪个线程用。
    这与 Driver 必须把 DLL 调用钉在一个工作线程上是同一类约束，
    但两者互不相干，各自用各自的线程。
    """

    def __init__(self):
        self._lock = threading.Lock()
        self.running = False
        self.err = ""
        self.rate = 0
        self.ch = 0
        self.fmt = ""
        # dev_sel 是用户指定的端点 ID（空 = 跟随默认播放设备）；
        # dev_name 是实际打开的端点友好名，给前端展示「正在采哪个设备」。
        self.dev_sel = ""
        self.dev_name = ""
        # 混音格式原始字段，供自检诊断「为什么落到 unsupported」
        self.bits = 0
        self.raw_tag = 0
        self.sub_tag = 0
        self.chan_mask = 0
        self.block_align = 0
        self.level = 0.0
        self.frames = 0
        self.start_at = 0.0
        self._stop = threading.Event()
        self._done = threading.Event()
        self._thread = None
        self._ring = [0.0] * LB_RING_LEN
        self._ring_w = 0
        self._ring_n = 0
        self._prev = [0.0] * LB_BINS
        self._bins = bytearray(LB_BINS)
        # FFT 查表
        self._cos = [0.0] * LB_FFT_SIZE
        self._sin = [0.0] * LB_FFT_SIZE
        self._win = [0.0] * LB_FFT_SIZE
        for i in range(LB_FFT_SIZE):
            a = 2.0 * math.pi * i / LB_FFT_SIZE
            self._cos[i] = math.cos(a)
            self._sin[i] = math.sin(a)
            self._win[i] = 0.42 - 0.5 * math.cos(a) + 0.08 * math.cos(2 * a)

    # ---------------- 对外 ----------------

    def start(self, device=""):
        """打开一路回环。device 是端点 ID（devices() 列出来的那种）：

        ""      → 跟随默认播放设备（旧行为，向后兼容）
        非空    → 锁定该端点，别的设备上放什么都进不来

        已经开着且端点没换就返回 ok=True（幂等）；开着但端点换了，
        先收旧会话再按新端点重开 —— 这是「采集途中切换耳机」的路径。
        """
        device = device or ""
        with self._lock:
            if self.running and self.dev_sel == device:
                return {"ok": True, "already": True, "rate": self.rate,
                        "ch": self.ch, "format": self.fmt, "bits": self.bits,
                        "tag": self.raw_tag, "sub": self.sub_tag,
                        "dev": self.dev_name}
            th = None
            if self.running:
                # 端点换了：停掉旧会话（worker 会退出），再当作全新启动
                th = self._thread
                self._stop.set()
        if th is not None:
            th.join(timeout=2.0)
        with self._lock:
            self.running = False
            self.dev_sel = device
            self.err = ""
            # ★ 换端点重开（乃至出错后的重开）必须清掉上一场会话的可见字段：
            # 下面的等待循环以 rate>0 判「开好了」，不清的话会立刻读到
            # 旧会话的 rate 返回假 ok —— start 一个不存在的端点也返回成功。
            self.rate = self.ch = 0
            self.fmt = ""
            self.dev_name = ""
            self.bits = self.raw_tag = self.sub_tag = 0
            self._stop = threading.Event()
            self._done = threading.Event()
            self.running = True
        self._thread = threading.Thread(target=self._worker, daemon=True)
        self._thread.start()

        deadline = time.time() + 3.0
        while time.time() < deadline:
            with self._lock:
                rate, ch, fmt = self.rate, self.ch, self.fmt
                err = self.err
                bits, raw_tag, sub_tag = self.bits, self.raw_tag, self.sub_tag
                dev = self.dev_name
            if rate > 0:
                return {"ok": True, "rate": rate, "ch": ch, "format": fmt,
                        "bits": bits, "tag": raw_tag, "sub": sub_tag,
                        "dev": dev}
            if err:
                with self._lock:
                    self.running = False
                return {"ok": False, "error": err}
            time.sleep(0.02)
        with self._lock:
            self.running = False
        self._stop.set()
        return {"ok": False, "error": "打开系统回环超时"}

    def stop(self):
        with self._lock:
            if not self.running:
                return {"ok": True, "already": True}
            th = self._thread
        self._stop.set()
        if th is not None:
            th.join(timeout=2.0)
        with self._lock:
            self.running = False
            self.rate = self.ch = 0
            self.fmt = ""
            self.dev_name = ""
            self.bits = self.raw_tag = self.sub_tag = self.chan_mask = 0
            self.block_align = 0
            self._ring_w = self._ring_n = self.frames = 0
            self.level = 0.0
            for i in range(LB_RING_LEN):
                self._ring[i] = 0.0
            for i in range(LB_BINS):
                self._prev[i] = 0.0
                self._bins[i] = 0
        return {"ok": True}

    def status(self):
        with self._lock:
            out = {"ok": True, "running": self.running, "rate": self.rate,
                   "ch": self.ch, "format": self.fmt, "bins": LB_BINS,
                   "frames": self.frames,
                   "level": round(self.level, 3),
                   "dev": self.dev_name,
                   "bits": self.bits, "tag": self.raw_tag,
                   "sub": self.sub_tag, "mask": self.chan_mask,
                   "align": self.block_align}
            if self.err:
                out["error"] = self.err
            if self.running and self.start_at:
                out["uptime"] = int(time.time() - self.start_at)
            return out

    def spectrum(self):
        with self._lock:
            if not self.running:
                return {"ok": False, "running": False, "error": "回环未启动"}
            # 原始样本峰值（线性 float32）：bins/level 是 dB 量化+平滑后的
            # 可视化值，底噪级信号（~1e-4）会显示成 0——诊断「端点里到底
            # 有没有数据」必须看原始峰值（2026-10-20 监听排障教训）。
            peak = 0.0
            if self._ring_n > 0:
                cnt = self._ring_n
                if cnt > LB_RING_LEN:
                    cnt = LB_RING_LEN
                start = (self._ring_w - cnt + LB_RING_LEN) % LB_RING_LEN
                ring = self._ring
                for i in range(cnt):
                    v = ring[(start + i) % LB_RING_LEN]
                    if v < 0.0:
                        v = -v
                    if v > peak:
                        peak = v
            return {"ok": True, "running": True, "rate": self.rate,
                    "ch": self.ch,
                    "bins": base64.b64encode(bytes(self._bins)).decode("ascii"),
                    "level": round(self.level, 3), "frames": self.frames,
                    "peak": round(peak, 7)}

    @staticmethod
    def devices():
        """枚举当前活动的播放端点 —— WASAPI loopback 可以锁定的全部目标。

        这是「仅耳机可视化」的入口：每个输出端点（耳机 / 音箱 / 显示器）
        都有自己独立的回环流，锁定耳机端点后，音箱上放什么都进不来，
        反之亦然。跟随默认端点做不到这一点 —— 默认切到音箱，它就跟着抓音箱。

        在调用线程（HTTP handler）上临时 CoInitializeEx：MTA 是按线程
        计数的，进来一次配一次 CoUninitialize 就平了，不影响采集线程
        自己的单元状态。蓝牙耳机这里会列出两个端点（Stereo/A2DP 与
        Hands-Free/HFP），选哪个由前端引导，后端不掺和。
        """
        hr = _ole32.CoInitializeEx(None, COINIT_MULTITHREADED)
        uninit = (hr == 0)  # S_OK 才该 CoUninitialize
        try:
            enumerator = ctypes.c_void_p()
            hr = _ole32.CoCreateInstance(
                ctypes.byref(CLSID_MMDEVICE_ENUMERATOR), None, CLSCTX_ALL,
                ctypes.byref(IID_IMMDEVICE_ENUMERATOR),
                ctypes.byref(enumerator))
            if _hres_bad(hr) or not enumerator.value:
                return {"ok": False, "error": "无法创建音频设备枚举器"}
            try:
                coll = ctypes.c_void_p()
                hr = _com_call(enumerator, VTBL_IMMDEVICE_ENUM_ENDPOINTS,
                               E_RENDER, DEVICE_STATE_ACTIVE, ctypes.byref(coll))
                if _hres_bad(hr) or not coll.value:
                    return {"ok": False, "error": "无法枚举播放设备"}
                try:
                    count = ctypes.c_uint32()
                    _com_call(coll, VTBL_COLL_GET_COUNT, ctypes.byref(count))
                    # 先拿默认端点的 ID，给列表打 def 标。拿不到也不致命。
                    def_id = ""
                    ddev = ctypes.c_void_p()
                    hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT,
                                   E_RENDER, E_CONSOLE, ctypes.byref(ddev))
                    if not _hres_bad(hr) and ddev.value:
                        def_id = _imm_device_id(ddev.value)
                        _com_release(ddev)
                    out = []
                    for i in range(int(count.value)):
                        dev = ctypes.c_void_p()
                        hr = _com_call(coll, VTBL_COLL_ITEM, i, ctypes.byref(dev))
                        if _hres_bad(hr) or not dev.value:
                            continue
                        did = _imm_device_id(dev.value)
                        if did:
                            out.append({"id": did,
                                        "name": _imm_device_name(dev.value),
                                        "def": did == def_id})
                        _com_release(dev)
                finally:
                    _com_release(coll)
                return {"ok": True, "default": def_id, "list": out}
            finally:
                _com_release(enumerator)
        finally:
            if uninit:
                _ole32.CoUninitialize()

    # ---------------- 采集线程 ----------------

    def _worker(self):
        """外层循环：跑一次会话，失败就退避重试。

        换耳机 / 拔插 USB 声卡会让流失效，自动重开比让用户重启程序好。

        ★ stop 事件在线程入口捕获成局部变量，之后一律用局部引用 ——
        不能中途再读 self._stop：start() 换端点时会先停旧会话再换新的
        self._stop，若旧线程（join 超时没死透）回头读属性就会绑到新事件上，
        变成两个线程同时采集。与 loopback.go 的 worker(stop, done) 同构。
        """
        stop = self._stop
        hr = _ole32.CoInitializeEx(None, COINIT_MULTITHREADED)
        uninit = (hr == 0)  # S_OK 才该 CoUninitialize
        try:
            attempt = 0
            while True:
                try:
                    why, stopped, fatal = self._session(stop)
                except Exception as exc:
                    # 采集线程里漏出的异常会直接把线程打死，对象却停在
                    # running=True —— 表现为「状态显示在采，频谱永远不动」。
                    # 这里兜住，转成一次普通的会话失败，交给退避重试。
                    why, stopped, fatal = "采集线程异常：%r" % (exc,), False, False
                if stopped:
                    return
                # fatal = 确定性失败（端点 ID 不存在之类），重试没有意义，
                # 立刻把原因报给 start 的等待循环和 /api/loopback/status。
                # 「找不到指定播放设备」归 fatal（拔掉了就是拔掉了）；
                # 「找不到默认播放设备」保持重试 —— 驱动/设备启动慢时等它出现。
                if fatal or attempt >= 5:
                    with self._lock:
                        self.err = why if fatal else why + "（已重试 5 次）"
                        self.running = False
                    return
                with self._lock:
                    self.err = ""
                if stop.wait((attempt + 1) * 0.4):
                    return
                attempt += 1
        finally:
            if uninit:
                _ole32.CoUninitialize()

    def _session(self, stop):
        """跑一次完整的「打开 → 采集」，返回 (中断原因, 是否收到停止信号)。"""
        # 必须用 c_void_p() 而不是 None：None 传给 ctypes.byref 会 TypeError。
        # 这几个变量是「由被调方填充」的出参，用 c_void_p 才能拿到地址。
        dev = ctypes.c_void_p()
        client = ctypes.c_void_p()
        capture = ctypes.c_void_p()
        mix_ptr = ctypes.c_void_p()
        try:
            enumerator = ctypes.c_void_p()
            hr = _ole32.CoCreateInstance(
                ctypes.byref(CLSID_MMDEVICE_ENUMERATOR), None, CLSCTX_ALL,
                ctypes.byref(IID_IMMDEVICE_ENUMERATOR),
                ctypes.byref(enumerator))
            if _hres_bad(hr) or not enumerator.value:
                return "无法创建音频设备枚举器", False, True

            # 指定了端点 ID 就按 ID 锁定；空 = 跟随默认播放设备。
            # ctypes 的 c_void_p 形参可以直接收 str（转成 wchar_t*，
            # 缓冲在调用期间有效），不需要自己上堆。
            if self.dev_sel:
                hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEVICE,
                               self.dev_sel, ctypes.byref(dev))
                if _hres_bad(hr) or not (dev and dev.value):
                    return "找不到指定播放设备（已拔出或端点 ID 失效？）", False, True
            else:
                hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT,
                               E_RENDER, E_CONSOLE, ctypes.byref(dev))
                if _hres_bad(hr) or not (dev and dev.value):
                    return "找不到默认播放设备（耳机没插好？）", False, False

            hr = _com_call(dev, VTBL_IMMDEVICE_ACTIVATE,
                           ctypes.byref(IID_IAUDIO_CLIENT), CLSCTX_ALL, None,
                           ctypes.byref(client))
            if _hres_bad(hr) or not (client and client.value):
                return "无法激活 IAudioClient", False, False

            # 共享模式**必须**用 GetMixFormat 给的格式初始化，自己指定采样率
            # 会拿到 AUDCLNT_E_UNSUPPORTED_FORMAT。
            hr = _com_call(client, VTBL_AUDIOCLIENT_GET_MIX_FORMAT,
                           ctypes.byref(mix_ptr))
            if _hres_bad(hr) or not mix_ptr.value:
                return "无法读取混音格式", False, False

            wf = ctypes.cast(mix_ptr,
                             ctypes.POINTER(_WAVEFORMATEX)).contents
            rate = int(wf.nSamplesPerSec)
            ch = int(wf.nChannels)
            bits = int(wf.wBitsPerSample)
            tag = int(wf.wFormatTag)
            block_align = int(wf.nBlockAlign)
            raw_tag = tag
            sub_tag = 0
            chan_mask = 0
            # WAVE_FORMAT_EXTENSIBLE：真正的编码在 SubFormat 里，取它的 Data1
            # （标准格式的 KSDATAFORMAT_SUBTYPE 前 4 字节就是格式标签）。
            if tag == 0xFFFE:
                ext = ctypes.cast(mix_ptr,
                                  ctypes.POINTER(_WAVEFORMATEXTENSIBLE)).contents
                sub_tag = int(ext.SubFormat.Data1)
                chan_mask = int(ext.dwChannelMask)
                tag = sub_tag
            if rate <= 0 or ch <= 0 or block_align <= 0:
                return "混音格式异常", False, True

            kind = "unsupported"
            if tag == 3 and bits == 32:
                kind = "float32"
            elif tag == 1 and bits == 16:
                kind = "pcm16"
            elif tag == 1 and bits == 32:
                kind = "pcm32"
            elif tag == 1 and bits == 24:
                kind = "pcm24"

            with self._lock:
                self.rate, self.ch, self.fmt = rate, ch, kind
                self.bits, self.raw_tag = bits, raw_tag
                self.sub_tag, self.chan_mask = sub_tag, chan_mask
                self.block_align = block_align
                self.dev_name = _imm_device_name(dev.value)
                self.start_at = time.time()
                self.frames = 0

            # Initialize：200ms 缓冲（100ns 单位）；共享模式周期参数必须给 0。
            hr = _com_call(client, VTBL_AUDIOCLIENT_INITIALIZE,
                           AUDCLNT_SHARE_SHARED, AUDCLNT_LOOPBACK,
                           200 * 10000, 0, mix_ptr, None)
            if _hres_bad(hr):
                return ("IAudioClient::Initialize(LOOPBACK) 失败，HRESULT=0x%08X"
                        % (hr & 0xFFFFFFFF)), False, False

            hr = _com_call(client, VTBL_AUDIOCLIENT_GET_SERVICE,
                           ctypes.byref(IID_IAUDIO_CAPTURE_CLIENT),
                           ctypes.byref(capture))
            if _hres_bad(hr) or not (capture and capture.value):
                return "无法获取 IAudioCaptureClient", False, False

            hr = _com_call(client, VTBL_AUDIOCLIENT_START)
            if _hres_bad(hr):
                return ("IAudioClient::Start 失败，HRESULT=0x%08X"
                        % (hr & 0xFFFFFFFF)), False, False

            return self._capture_loop(stop, client, capture, ch, block_align,
                                      tag, bits)
        finally:
            if client is not None and client.value:
                try:
                    _com_call(client, VTBL_AUDIOCLIENT_STOP)
                except Exception:
                    pass
            _com_release(capture)
            _com_release(client)
            _com_release(dev)
            if mix_ptr.value:
                _ole32.CoTaskMemFree(mix_ptr)

    def _capture_loop(self, stop, client, capture, ch, block_align, tag, bits):
        """轮询收包。

        刻意用轮询而不是 AUDCLNT_STREAMFLAGS_EVENTCALLBACK：事件驱动的回环
        采集有额外前提（历史版本直接返回 E_INVALIDARG），而 5ms 轮询的延迟
        对可视化完全够用，可靠性优先。
        """
        packet = ctypes.c_uint32()
        p_data = ctypes.c_void_p()
        frames = ctypes.c_uint32()
        flags = ctypes.c_uint32()
        dev_pos = ctypes.c_uint64()
        qpc = ctypes.c_uint64()
        last_fft = time.time()

        while not stop.is_set():
            if stop.wait(0.005):
                return "", True, False

            broken = False
            for _ in range(64):
                hr = _com_call(capture, VTBL_CAPTURE_GET_NEXT_PACKET_SIZE,
                               ctypes.byref(packet))
                if _hres_bad(hr):
                    broken = True  # 通常意味着流已失效（设备被拔了）
                    break
                if packet.value == 0:
                    break
                hr = _com_call(capture, VTBL_CAPTURE_GET_BUFFER,
                               ctypes.byref(p_data), ctypes.byref(frames),
                               ctypes.byref(flags), ctypes.byref(dev_pos),
                               ctypes.byref(qpc))
                if _hres_bad(hr):
                    break
                n = int(frames.value)
                if n > 0:
                    if (flags.value & AUDCLNT_SILENT) or not p_data.value:
                        self._push_silence(n)
                    else:
                        self._push_frames(p_data.value, n, ch, block_align,
                                          tag, bits)
                _com_call(capture, VTBL_CAPTURE_RELEASE_BUFFER, n)

            now = time.time()
            if now - last_fft >= LB_PERIOD:
                last_fft = now
                self._compute_spectrum()
            if broken:
                return "回环流已中断", False, False
        return "", True, False

    # ---------------- 采样入环 ----------------

    def _write_ring(self, mixed):
        """把一串单声道样本写进环形缓冲。

        用切片赋值而不是逐元素写：48 kHz 下每秒要写 4.8 万个样本，
        逐元素写会把 GIL 占满。切片赋值是 C 级操作，还顺带把
        「持锁时间」压到最短。

        ★ 回绕那一段必须写 `ring[:X]`（从 0 开始），不能写 `ring[X:]`。
          切片赋值的语义是「用右边替换左边那个切片」——左右长度不等时
          Python 会**改变列表长度**。写成 `ring[X:]` 时左边长度 8192−X、
          右边长度 X，只有 X 恰好等于 4096 时才相等；其余情况下环形缓冲
          会被悄悄改短，`_compute_spectrum` 随即 IndexError。
          症状是「有时候跑得好好的，有时候几秒后崩」——很难查。
        """
        with self._lock:
            ring = self._ring
            w = self._ring_w
            cnt = len(mixed)
            if cnt >= LB_RING_LEN:
                # 一次推入的样本比整个环还多：只保留最后 LB_RING_LEN 个
                ring[:] = mixed[-LB_RING_LEN:]
                self._ring_w = 0
                self._ring_n += cnt
                self.frames += cnt
                return
            end = w + cnt
            if end <= LB_RING_LEN:
                ring[w:end] = mixed
            else:
                k = LB_RING_LEN - w
                ring[w:] = mixed[:k]
                ring[:end - LB_RING_LEN] = mixed[k:]
            self._ring_w = end % LB_RING_LEN
            self._ring_n += cnt
            self.frames += cnt

    def _push_silence(self, frames):
        self._write_ring([0.0] * frames)

    def _push_frames(self, addr, frames, ch, block_align, tag, bits):
        """把交错的多声道数据混成单声道 float32 写入环形缓冲。

        Windows 默认混音格式绝大多数是 WAVE_FORMAT_EXTENSIBLE + IEEE_FLOAT
        32bit，但板载声卡也可能给 PCM16，所以四种都实现 —— 少一种就会变成
        「能打开却全是噪声」这种很难查的问题。
        """
        take, inv = _lb_downmix(ch)
        n = frames * ch
        buf = ctypes.string_at(addr, frames * block_align)

        # 一次把整块解包成扁平样本表（C 级），再在下面按声道切片。
        # 比逐帧 struct.unpack_from 快数倍 —— 后者每秒要调 4.8 万次。
        if tag == 3 and bits == 32:
            vals = struct.unpack_from("<%df" % n, buf)
            scale = 1.0
        elif tag == 1 and bits == 16:
            vals = struct.unpack_from("<%dh" % n, buf)
            scale = 1.0 / 32768.0
        elif tag == 1 and bits == 32:
            vals = struct.unpack_from("<%di" % n, buf)
            scale = 1.0 / 2147483648.0
        elif tag == 1 and bits == 24:
            # 24 位没有 struct 格式码，只能手解（罕见格式，慢点无所谓）
            vals = []
            ap = vals.append
            for i in range(frames):
                o = i * block_align
                for _c in range(ch):
                    v = buf[o] | (buf[o + 1] << 8) | (buf[o + 2] << 16)
                    if v & 0x800000:
                        v -= 0x1000000  # 24 位符号扩展
                    ap(v)
                    o += 3
            scale = 1.0 / 8388608.0
        else:
            # 未知格式：写静音而不是乱码，至少不会把地形抖成噪点
            self._push_silence(frames)
            return

        # 按声道切片是 C 级操作；下混（前对平均或单声道）用列表推导。
        if take == 2:
            left = vals[0::ch]
            right = vals[1::ch]
            mixed = [(a + b) * scale * inv for a, b in zip(left, right)]
        else:
            mixed = [v * scale * inv for v in vals[0::ch]]
        self._write_ring(mixed)

    # ---------------- 频谱 ----------------

    def _compute_spectrum(self):
        """取最近 1024 个样本做一次 FFT，更新 _bins 与 level。

        只在采集线程上调用；只有读写共享字段时才持锁，FFT 本体不持锁。
        """
        with self._lock:
            if self._ring_n < LB_FFT_SIZE:
                return
            start = (self._ring_w - LB_FFT_SIZE + LB_RING_LEN) % LB_RING_LEN
            ring = self._ring
            win = self._win
            re = [0.0] * LB_FFT_SIZE
            im = [0.0] * LB_FFT_SIZE
            for i in range(LB_FFT_SIZE):
                re[i] = ring[(start + i) % LB_RING_LEN] * win[i]

        _fft_radix2(re, im, self._cos, self._sin)

        # 归一化见 waMagScale 的说明：|X[k]|/N，与 Chromium 的
        # AnalyserNode 逐字节对齐。
        norm = 1.0 / LB_FFT_SIZE
        span = WA_MAX_DB - WA_MIN_DB
        level = 0.0
        bins = self._bins
        prev = self._prev
        with self._lock:
            for k in range(LB_BINS):
                r = re[k]
                i2 = im[k]
                mag = math.sqrt(r * r + i2 * i2) * norm
                # 平滑按规范做在幅度上
                s = WA_SMOOTHING * prev[k] + (1.0 - WA_SMOOTHING) * mag
                prev[k] = s
                if s > level:
                    level = s
                db = 20.0 * math.log10(s) if s > 0 else -1000.0
                v = int((db - WA_MIN_DB) / span * 255.0)
                if v < 0:
                    v = 0
                elif v > 255:
                    v = 255
                bins[k] = v
            self.level = 1.0 if level > 1.0 else level


def _fft_radix2(re, im, cos_tab, sin_tab):
    """就地做基 2 复数 FFT。长度必须是 2 的幂。"""
    n = len(re)
    if n < 2 or (n & (n - 1)):
        return
    # 位反转置换
    j = 0
    for i in range(1, n):
        bit = n >> 1
        while j & bit:
            j ^= bit
            bit >>= 1
        j ^= bit
        if i < j:
            re[i], re[j] = re[j], re[i]
            im[i], im[j] = im[j], im[i]
    # 蝶形。twiddle 用长度 n 的整表索引：索引 k*(n/length) 一定落在 [0, n/2)。
    length = 2
    while length <= n:
        half = length >> 1
        step = n // length
        for base in range(0, n, length):
            k = 0
            for p in range(base, base + half):
                idx = k * step
                cr = cos_tab[idx]
                ci = -sin_tab[idx]
                q = p + half
                ar = re[p]
                ai = im[p]
                br = re[q]
                bi = im[q]
                tr = br * cr - bi * ci
                ti = br * ci + bi * cr
                re[p] = ar + tr
                im[p] = ai + ti
                re[q] = ar - tr
                im[q] = ai - ti
                k += 1
        length <<= 1


# ====================================================================

# 全局唯一的系统回环实例。与 Driver 不同，它**不**在启动时就开始采集 ——
# 采集要等前端点「开始」，而且会让系统的音频指示亮起，静默开着不合适。
LOOPBACK = Loopback()


# --------------------------------------------------------------------
# 内嵌录音机（麦克风 → WAV）
# --------------------------------------------------------------------
# 为什么不用官方捆绑的 SoundRecorder\Recorder.exe：
#   那是个依附官方面板 COM 环境的薄客户端，导入表仅 KERNEL32+ole32
#   （无 user32 画不了窗、无 winmm 记不了音），脱离官方面板单独拉起
#   必然静默退出（本机实测恒退 241，事件日志无崩溃记录）。
#   取证过程见 工具/录音机取证-Recorder.exe死件.md。
# 与 Wails 线 recorder.go 的 MicRecorder 逐字段对齐：start/stop/status
# 返回键、文件命名（文档\iKF录音\iKF录音_时间戳.wav）、以及「不做断流
# 自动重试」（自动续录会把一段录音裂成多份，宁可明确报错）。
# 采集/COM 基建直接复用上面 Loopback 那套手写虚表。

E_CAPTURE = 1  # GetDefaultAudioEndpoint 的 dataFlow：采集（录音）方向

_FDDID_DOCUMENTS = _GUID(
    0xFDD39AD0, 0x238F, 0x46AF,
    (ctypes.c_ubyte * 8)(0xAD, 0xB4, 0x6C, 0x85, 0x48, 0x03, 0x69, 0xC7))
# FOLDERID_Music {4BD8D571-6D19-48D3-BE97-422220080E43}（耳机录音落盘根）
_FDDID_MUSIC = _GUID(
    0x4BD8D571, 0x6D19, 0x48D3,
    (ctypes.c_ubyte * 8)(0xBE, 0x97, 0x42, 0x22, 0x20, 0x08, 0x0E, 0x43))
_shell32 = ctypes.WinDLL("shell32.dll")
_shell32.SHGetKnownFolderPath.restype = ctypes.c_long
_shell32.SHGetKnownFolderPath.argtypes = [
    ctypes.POINTER(_GUID), ctypes.c_uint32, ctypes.c_void_p,
    ctypes.POINTER(ctypes.c_void_p)]


def _documents_dir():
    """拿「文档」真实目录（SHGetKnownFolderPath，吃 OneDrive 重定向）。

    失败回退 %USERPROFILE%/Documents。返回 (路径, None) 或 (回退路径, hr)。
    """
    p = ctypes.c_void_p()
    hr = _shell32.SHGetKnownFolderPath(ctypes.byref(_FDDID_DOCUMENTS),
                                       0, None, ctypes.byref(p))
    if not _hres_bad(hr) and p.value:
        try:
            return ctypes.wstring_at(p.value), None
        finally:
            _ole32.CoTaskMemFree(p)
    home = os.path.expanduser("~")
    return os.path.join(home, "Documents"), hr or None


def _music_dir():
    """拿「音乐」真实目录（SHGetKnownFolderPath，吃 OneDrive 重定向）。

    失败回退 %USERPROFILE%/Music。返回 (路径, None) 或 (回退路径, hr)。
    """
    p = ctypes.c_void_p()
    hr = _shell32.SHGetKnownFolderPath(ctypes.byref(_FDDID_MUSIC),
                                       0, None, ctypes.byref(p))
    if not _hres_bad(hr) and p.value:
        try:
            return ctypes.wstring_at(p.value), None
        finally:
            _ole32.CoTaskMemFree(p)
    home = os.path.expanduser("~")
    return os.path.join(home, "Music"), hr or None


class MicRecorder(object):
    """一路耳机录音会话（渲染端点 loopback → 16bit 立体声 WAV）。

    2026-10-20 晚（用户拍板）：从「麦克风页录音机（录麦克风）」迁移为
    声音页「耳机录音」——录渲染端点回环，即耳机里正在播放的声音；
    WAV 落「音乐/iKF Audio Center/」，统一下混立体声。

    线程模型与 Loopback 相同：COM 单元状态是线程局部的，采集钉在
    专用线程上。差别是**不自动重试**——录音是用户在场操作，流中断
    自动换文件续录会把一段录音裂成多份，宁可明确报错。
    """

    def __init__(self):
        self._lock = threading.Lock()
        self.running = False
        self.err = ""
        self.rate = 0
        self.ch = 0
        self.fmt = ""
        self.dev_name = ""
        self.file = ""
        self.seconds = 0.0
        self.size = 0
        self._start_at = 0.0
        self._stop = threading.Event()
        self._done = threading.Event()
        self._thread = None

    # ---------------- 对外（与 recorder.go 逐字段对齐） ----------------

    def start(self):
        """开始录音。已在录则幂等返回 ok。"""
        with self._lock:
            if self.running:
                return {"ok": True, "already": True, "file": self.file,
                        "rate": self.rate, "ch": self.ch,
                        "dev": self.dev_name}
            self.err = ""
            self.rate, self.ch, self.fmt = 0, 0, ""
            self.dev_name = ""
            self.file = ""
            self.seconds, self.size = 0.0, 0
            self._stop = threading.Event()
            self._done = threading.Event()
            self.running = True
        stop, done = self._stop, self._done
        self._thread = threading.Thread(
            target=self._worker, args=(stop, done), daemon=True)
        self._thread.start()

        deadline = time.time() + 3.0
        while time.time() < deadline:
            with self._lock:
                rate, err = self.rate, self.err
                file, ch, dev = self.file, self.ch, self.dev_name
            if rate > 0:
                return {"ok": True, "file": file, "rate": rate,
                        "ch": ch, "dev": dev}
            if err:
                with self._lock:
                    self.running = False
                return {"ok": False, "error": err}
            time.sleep(0.02)
        with self._lock:
            self.running = False
        self._stop.set()
        return {"ok": False, "error": "打开输出设备超时"}

    def stop(self):
        """停止录音并等采集线程把 WAV 收尾。未在录时返回最近一次结果。"""
        with self._lock:
            if not self.running:
                out = {"ok": True, "already": True, "file": self.file,
                       "seconds": self.seconds, "size": self.size}
                if self.err:
                    out["error"] = self.err
                return out
            stop, done = self._stop, self._done
        if not stop.is_set():
            stop.set()
        done.wait(timeout=3.0)
        with self._lock:
            self.running = False
            out = {"ok": True, "file": self.file,
                   "seconds": self.seconds, "size": self.size}
            if self.err:
                out["error"] = self.err
            return out

    def status(self):
        """供 /api/recorder/status 与前端 500ms 轮询。"""
        with self._lock:
            out = {"ok": True, "recording": self.running, "file": self.file,
                   "seconds": self.seconds, "size": self.size,
                   "rate": self.rate, "ch": self.ch,
                   "dev": self.dev_name, "format": self.fmt}
            if self.err:
                out["error"] = self.err
            return out

    # ---------------- 采集线程 ----------------

    def _worker(self, stop, done):
        hr = _ole32.CoInitializeEx(None, COINIT_MULTITHREADED)
        uninit = (hr == 0)
        try:
            # ★ 不重试：见类型注释。一次会话到头。
            self._session(stop)
        except Exception as exc:
            # 采集线程漏出的异常直接把线程打死会停在 running=True。
            with self._lock:
                self.err = "录音线程异常：%r" % (exc,)
                self.running = False
        finally:
            if uninit:
                _ole32.CoUninitialize()
            done.set()

    def _session(self, stop):
        """跑一次完整录音会话（打开 → 采包 → 写 WAV → 收尾）。"""
        dev = ctypes.c_void_p()
        client = ctypes.c_void_p()
        capture = ctypes.c_void_p()
        mix_ptr = ctypes.c_void_p()
        wout = None
        try:
            enumerator = ctypes.c_void_p()
            hr = _ole32.CoCreateInstance(
                ctypes.byref(CLSID_MMDEVICE_ENUMERATOR), None, CLSCTX_ALL,
                ctypes.byref(IID_IMMDEVICE_ENUMERATOR),
                ctypes.byref(enumerator))
            if _hres_bad(hr) or not enumerator.value:
                self._fail("无法创建音频设备枚举器")
                return
            try:
                # 耳机录音=录**渲染端点**回环（耳机里正在播放的声音）。
                hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT,
                               E_RENDER, E_CONSOLE, ctypes.byref(dev))
                if _hres_bad(hr) or not (dev and dev.value):
                    self._fail("找不到默认输出设备（耳机没插好？）")
                    return

                hr = _com_call(dev, VTBL_IMMDEVICE_ACTIVATE,
                               ctypes.byref(IID_IAUDIO_CLIENT), CLSCTX_ALL,
                               None, ctypes.byref(client))
                if _hres_bad(hr) or not (client and client.value):
                    self._fail("无法激活 IAudioClient")
                    return

                # loopback 采集必须用渲染端混音格式（与 loopback 同因）。
                hr = _com_call(client, VTBL_AUDIOCLIENT_GET_MIX_FORMAT,
                               ctypes.byref(mix_ptr))
                if _hres_bad(hr) or not mix_ptr.value:
                    self._fail("无法读取输出混音格式")
                    return

                wf = ctypes.cast(mix_ptr,
                                 ctypes.POINTER(_WAVEFORMATEX)).contents
                rate, ch = int(wf.nSamplesPerSec), int(wf.nChannels)
                bits, block_align = int(wf.wBitsPerSample), int(wf.nBlockAlign)
                tag = int(wf.wFormatTag)
                if tag == 0xFFFE:
                    tag = int(ctypes.cast(
                        mix_ptr,
                        ctypes.POINTER(_WAVEFORMATEXTENSIBLE)
                    ).contents.SubFormat.Data1)
                if rate <= 0 or ch <= 0 or block_align <= 0:
                    self._fail("输出混音格式异常")
                    return
                wav_ch = 2  # 输出恒立体声：取 FL/FR（单声道源则双写）

                music, _hr2 = _music_dir()
                rec_root = os.path.join(music, "iKF Audio Center")
                try:
                    os.makedirs(rec_root, exist_ok=True)
                except OSError as exc:
                    self._fail("无法创建录音目录：%s" % exc)
                    return
                path = os.path.join(
                    rec_root, "iKF录音_%s.wav" % time.strftime("%Y%m%d_%H%M%S"))
                wout = wave.open(path, "wb")
                wout.setnchannels(wav_ch)
                wout.setsampwidth(2)
                wout.setframerate(rate)

                with self._lock:
                    self.rate, self.ch = rate, wav_ch
                    self.fmt = _src_kind(tag, bits)
                    self.dev_name = _imm_device_name(dev.value)
                    self.file = path
                    self._start_at = time.time()
            finally:
                _com_release(enumerator)

            # Initialize：200ms 缓冲 + LOOPBACK 旗标（渲染端点回环采集）。
            hr = _com_call(client, VTBL_AUDIOCLIENT_INITIALIZE,
                           AUDCLNT_SHARE_SHARED, AUDCLNT_LOOPBACK,
                           200 * 10000, 0, mix_ptr, None)
            if _hres_bad(hr):
                self._fail("IAudioClient::Initialize 失败，HRESULT=0x%08X"
                           % (hr & 0xFFFFFFFF))
                return
            hr = _com_call(client, VTBL_AUDIOCLIENT_GET_SERVICE,
                           ctypes.byref(IID_IAUDIO_CAPTURE_CLIENT),
                           ctypes.byref(capture))
            if _hres_bad(hr) or not (capture and capture.value):
                self._fail("无法获取 IAudioCaptureClient")
                return
            hr = _com_call(client, VTBL_AUDIOCLIENT_START)
            if _hres_bad(hr):
                self._fail("IAudioClient::Start 失败，HRESULT=0x%08X"
                           % (hr & 0xFFFFFFFF))
                return

            # ---- 收包循环：与 loopback 相同的 5ms 轮询 ----
            packet = ctypes.c_uint32()
            p_data = ctypes.c_void_p()
            frames = ctypes.c_uint32()
            flags = ctypes.c_uint32()
            dev_pos = ctypes.c_uint64()
            qpc = ctypes.c_uint64()
            broken = False

            while not stop.is_set():
                if stop.wait(0.005):
                    break
                broken = False
                for _ in range(64):
                    hr = _com_call(capture, VTBL_CAPTURE_GET_NEXT_PACKET_SIZE,
                                   ctypes.byref(packet))
                    if _hres_bad(hr):
                        broken = True
                        break
                    if packet.value == 0:
                        break
                    hr = _com_call(capture, VTBL_CAPTURE_GET_BUFFER,
                                   ctypes.byref(p_data), ctypes.byref(frames),
                                   ctypes.byref(flags), ctypes.byref(dev_pos),
                                   ctypes.byref(qpc))
                    if _hres_bad(hr):
                        broken = True
                        break
                    n = int(frames.value)
                    if n > 0:
                        if (flags.value & AUDCLNT_SILENT) or not p_data.value:
                            self._write(wout, b"\x00\x00" * (n * wav_ch), n)
                        else:
                            self._write_frames(wout, p_data.value, n, ch,
                                               block_align, tag, bits)
                    _com_call(capture, VTBL_CAPTURE_RELEASE_BUFFER, n)
                with self._lock:
                    if self._start_at:
                        self.seconds = time.time() - self._start_at
                if broken:
                    self._close_wav(wout)
                    wout = None
                    self._fail("输出端数据流已中断（设备被拔出？），已保存已录部分")
                    return

            # stop 信号正常收尾
            self._close_wav(wout)
            wout = None
        finally:
            if client is not None and client.value:
                try:
                    _com_call(client, VTBL_AUDIOCLIENT_STOP)
                except Exception:
                    pass
            _com_release(capture)
            _com_release(client)
            _com_release(dev)
            if mix_ptr.value:
                _ole32.CoTaskMemFree(mix_ptr)
            if wout is not None:
                # 异常路径：也要把头补上，文件至少能播
                self._close_wav(wout)
            with self._lock:
                self.running = False

    def _fail(self, why):
        with self._lock:
            self.err = why

    @staticmethod
    def _close_wav(wout):
        """wave 模块 close() 时补写 RIFF/data 长度，文件才能播。"""
        try:
            wout.close()
        except Exception:
            pass

    def _write(self, wout, pcm, n_frames):
        wout.writeframes(pcm)
        with self._lock:
            self.size += len(pcm)

    def _write_frames(self, wout, addr, n_frames, src_ch, block_align, tag, bits):
        """把一包交错**多声道源**数据下混为 16bit 立体声追加。

        取 FL/FR（声道 0/1），单声道源双写；渲染混音格式通常 float32
        8ch，直接输出 8ch WAV 兼容性差，统一下混到 2ch。"""
        buf = ctypes.string_at(addr, n_frames * block_align)
        ba = block_align
        out = bytearray(n_frames * 4)

        def put(i, v):
            if v > 32767:
                v = 32767
            elif v < -32768:
                v = -32768
            out[i] = v & 0xFF
            out[i + 1] = (v >> 8) & 0xFF

        if tag == 3 and bits == 32:            # IEEE float32
            vals = struct.unpack_from("<%df" % (n_frames * src_ch), buf)
            for f in range(n_frames):
                base = f * src_ch
                l = vals[base]
                r = vals[base + 1] if src_ch >= 2 else l
                put(f * 4, int(l * 32767))
                put(f * 4 + 2, int(r * 32767))
        elif tag == 1 and bits == 16:
            vals = struct.unpack_from("<%dh" % (n_frames * src_ch), buf)
            for f in range(n_frames):
                base = f * src_ch
                put(f * 4, vals[base])
                put(f * 4 + 2, vals[base + 1] if src_ch >= 2 else vals[base])
        elif tag == 1 and bits == 32:
            vals = struct.unpack_from("<%di" % (n_frames * src_ch), buf)
            for f in range(n_frames):
                base = f * src_ch
                put(f * 4, vals[base] >> 16)
                put(f * 4 + 2, (vals[base + 1] if src_ch >= 2
                                else vals[base]) >> 16)
        elif tag == 1 and bits == 24:
            for f in range(n_frames):
                base = f * ba
                v = buf[base] | (buf[base + 1] << 8) | (buf[base + 2] << 16)
                if v & 0x800000:
                    v -= 0x1000000
                put(f * 4, v >> 8)
                if src_ch >= 2:
                    v = (buf[base + 3] | (buf[base + 4] << 8)
                         | (buf[base + 5] << 16))
                    if v & 0x800000:
                        v -= 0x1000000
                put(f * 4 + 2, v >> 8)
        else:
            # 未知格式：写静音而不是乱码
            self._write(wout, b"\x00\x00" * (n_frames * 2), n_frames)
            return
        wout.writeframes(bytes(out))
        with self._lock:
            self.size += len(out)


def _src_kind(tag, bits):
    """与 loopback 的 kind 判定同款（诊断字段用）。"""
    if tag == 3 and bits == 32:
        return "float32"
    if tag == 1 and bits == 16:
        return "pcm16"
    if tag == 1 and bits == 32:
        return "pcm32"
    if tag == 1 and bits == 24:
        return "pcm24"
    return "unsupported"


REC = MicRecorder()


class MicListen(object):
    """一路麦克风监听会话（内嵌自研，2026-10-20）。

    为什么不走官方路径（Driver.listen → osConfLib.RunListen2Device）：
    反编译实锤（工具/监听取证-监听COM组件缺失.md），RunListen2Device
    内部 CoCreateInstance 的监听引擎 CLSID {E642D042-5BCF-4E80-9451-
    83F4E398A88F} 在整套 WHQL 驱动包、System32、注册表里都不存在——
    官方原版面板同样开不了，实测恒返 0x80040154（类未注册）。

    本类等价 Windows 声音控制台的「侦听此设备」：采默认麦克风端点
    （麦克风自己的混音格式），渲染端按采集格式请求 + AUTOCONVERTPCM
    由引擎上混，实时送默认输出设备。★转换必须放渲染侧：放采集侧
    （按渲染混音格式开采集流）出来全是静音（见 _session 排障注释）。
    线程模型与 MicRecorder 相同：COM 钉专用线程、单会话不重试。

    与 Go 线 listen.go 的 MicListen 逐字段对齐（start/stop/status
    响应形状、sent 计数语义）。
    """

    def __init__(self):
        self._lock = threading.Lock()
        self.running = False
        self.err = ""
        self.dev_name = ""
        self.out_name = ""
        self.rate = 0
        self.ch = 0
        self.sent = 0
        # 排障诊断（2026-10-20 静音 bug）：采集包计数、SILENT 旗标包数、
        # 采集数据峰值（float32 绝对值最大，正常底噪 ~3e-4，全零=0）
        self.pkts = 0
        self.silent_pkts = 0
        self.cap_peak = 0.0
        self._stop = threading.Event()
        self._done = threading.Event()
        self._thread = None

    # ---------------- 对外（与 listen.go 逐字段对齐） ----------------

    def start(self):
        """开始监听。已在听则幂等返回 ok。"""
        with self._lock:
            if self.running:
                return {"ok": True, "already": True, "dev": self.dev_name,
                        "out": self.out_name, "rate": self.rate, "ch": self.ch}
            self.err = ""
            self.dev_name, self.out_name = "", ""
            self.rate, self.ch = 0, 0
            self.sent = 0
            self.pkts = 0
            self.silent_pkts = 0
            self.cap_peak = 0.0
            self._stop = threading.Event()
            self._done = threading.Event()
            self.running = True
        stop, done = self._stop, self._done
        self._thread = threading.Thread(
            target=self._worker, args=(stop, done), daemon=True)
        self._thread.start()

        deadline = time.time() + 3.0
        while time.time() < deadline:
            with self._lock:
                dev, out, rate, ch, err = (self.dev_name, self.out_name,
                                           self.rate, self.ch, self.err)
            if dev:
                return {"ok": True, "dev": dev, "out": out,
                        "rate": rate, "ch": ch}
            if err:
                with self._lock:
                    self.running = False
                return {"ok": False, "error": err}
            time.sleep(0.02)
        with self._lock:
            self.running = False
        self._stop.set()
        return {"ok": False, "error": "打开监听设备超时"}

    def stop(self):
        """停止监听并等线程收尾。未在听时幂等返回 ok。"""
        with self._lock:
            if not self.running:
                out = {"ok": True, "already": True}
                if self.err:
                    out["error"] = self.err
                return out
            stop, done = self._stop, self._done
        if not stop.is_set():
            stop.set()
        done.wait(timeout=3.0)
        with self._lock:
            self.running = False
            out = {"ok": True}
            if self.err:
                out["error"] = self.err
            return out

    def status(self):
        """诊断与验收断言用（sent 必须在监听中持续增长）。"""
        with self._lock:
            out = {"ok": True, "on": self.running, "dev": self.dev_name,
                   "out": self.out_name, "rate": self.rate, "ch": self.ch,
                   "sent": self.sent, "pkts": self.pkts,
                   "silentPkts": self.silent_pkts,
                   "capPeak": self.cap_peak}
            if self.err:
                out["error"] = self.err
            return out

    # ---------------- 线程 ----------------

    def _worker(self, stop, done):
        hr = _ole32.CoInitializeEx(None, COINIT_MULTITHREADED)
        uninit = (hr == 0)
        try:
            self._session(stop)
        except Exception as exc:
            with self._lock:
                self.err = "监听线程异常：%r" % (exc,)
                self.running = False
        finally:
            if uninit:
                _ole32.CoUninitialize()
            done.set()

    def _session(self, stop):
        """跑一次完整监听会话（开两端 → 采包搬运 → 收尾）。"""
        dev_c = ctypes.c_void_p()
        dev_r = ctypes.c_void_p()
        cli_c = ctypes.c_void_p()
        cli_r = ctypes.c_void_p()
        cap_c = ctypes.c_void_p()
        ren_c = ctypes.c_void_p()
        mix_ptr = ctypes.c_void_p()
        ren_ptr = ctypes.c_void_p()
        try:
            enumerator = ctypes.c_void_p()
            hr = _ole32.CoCreateInstance(
                ctypes.byref(CLSID_MMDEVICE_ENUMERATOR), None, CLSCTX_ALL,
                ctypes.byref(IID_IMMDEVICE_ENUMERATOR),
                ctypes.byref(enumerator))
            if _hres_bad(hr) or not enumerator.value:
                self._fail("无法创建音频设备枚举器")
                return
            try:
                # ★★ 2026-10-20 排障实录（三次实测才定位，两线同病）：
                #   v1 采集侧 AUTOCONVERTPCM（采集按渲染混音格式 8ch 开）
                #   → 静音；v2 渲染侧 AUTOCONVERTPCM（渲染按采集格式请求）
                #   → 依旧静音。判据：监听开着时用输出端点回环采 30 万帧
                #   **原始浮点**峰值精确 0，而采集流有真实底噪（capPeak
                #   ≈0.0017、silentPkts=0、sent 持续增长——引擎在消费我们
                #   的帧却产出静音）。
                #   v3 终案：两端都用各自的原生混音格式，**软件上混**——
                #   渲染端按渲染混音格式（8ch float32）标准初始化，把
                #   单声道样本写进 FL/FR。不依赖引擎的任何格式转换。
                # ---- 采集端点：麦克风自己的混音格式（混合格式必为 float32） ----
                hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT,
                               E_CAPTURE, E_CONSOLE, ctypes.byref(dev_c))
                if _hres_bad(hr) or not (dev_c and dev_c.value):
                    self._fail("找不到默认麦克风（耳机没插好？）")
                    return
                hr = _com_call(dev_c, VTBL_IMMDEVICE_ACTIVATE,
                               ctypes.byref(IID_IAUDIO_CLIENT), CLSCTX_ALL,
                               None, ctypes.byref(cli_c))
                if _hres_bad(hr) or not (cli_c and cli_c.value):
                    self._fail("无法激活采集端 IAudioClient")
                    return
                hr = _com_call(cli_c, VTBL_AUDIOCLIENT_GET_MIX_FORMAT,
                               ctypes.byref(mix_ptr))
                if _hres_bad(hr) or not mix_ptr.value:
                    self._fail("无法读取麦克风混音格式")
                    return
                wf = ctypes.cast(mix_ptr,
                                 ctypes.POINTER(_WAVEFORMATEX)).contents
                rate, ch = int(wf.nSamplesPerSec), int(wf.nChannels)
                bits, block_align = int(wf.wBitsPerSample), int(wf.nBlockAlign)
                tag = int(wf.wFormatTag)
                if tag == 0xFFFE:
                    tag = int(ctypes.cast(
                        mix_ptr,
                        ctypes.POINTER(_WAVEFORMATEXTENSIBLE)
                    ).contents.SubFormat.Data1)
                if rate <= 0 or ch <= 0 or block_align <= 0:
                    self._fail("麦克风混音格式异常")
                    return
                if tag != 3:  # 混合格式必为 float32；别的情况拒绝（不猜格式）
                    self._fail("麦克风混音格式不受支持（tag=%d bits=%d）"
                               % (tag, bits))
                    return
                hr = _com_call(cli_c, VTBL_AUDIOCLIENT_INITIALIZE,
                               AUDCLNT_SHARE_SHARED, 0, 200 * 10000, 0,
                               mix_ptr, None)
                if _hres_bad(hr):
                    self._fail("采集端 Initialize 失败，HRESULT=0x%08X"
                               % (hr & 0xFFFFFFFF))
                    return
                hr = _com_call(cli_c, VTBL_AUDIOCLIENT_GET_SERVICE,
                               ctypes.byref(IID_IAUDIO_CAPTURE_CLIENT),
                               ctypes.byref(cap_c))
                if _hres_bad(hr) or not (cap_c and cap_c.value):
                    self._fail("无法获取 IAudioCaptureClient")
                    return

                # ★★ 2026-10-20 排障实录（三次实测才定位，两线同病）：
                #   v1 采集侧 AUTOCONVERTPCM → 静音；v2 渲染侧
                #   AUTOCONVERTPCM（渲染按采集格式请求）→ 依旧静音。
                #   判据：监听开着时用输出端点回环采 30 万帧**原始浮点**
                #   峰值精确 0，而采集流有真实底噪（capPeak≈0.0017、
                #   silentPkts=0、sent 持续增长——引擎在消费我们的帧却
                #   产出静音）。
                #   v3 终案：两端都用各自的原生混音格式，**软件上混**——
                #   渲染端按渲染混音格式（8ch float32）标准初始化，把
                #   单声道样本写进 FL/FR。不依赖引擎的任何格式转换。
                # ---- 输出客户端：渲染端自己的混音格式（软件上混） ----
                hr = _com_call(enumerator, VTBL_IMMDEVICE_GET_DEFAULT_ENDPOINT,
                               E_RENDER, E_CONSOLE, ctypes.byref(dev_r))
                if _hres_bad(hr) or not (dev_r and dev_r.value):
                    self._fail("找不到默认输出设备")
                    return
                hr = _com_call(dev_r, VTBL_IMMDEVICE_ACTIVATE,
                               ctypes.byref(IID_IAUDIO_CLIENT), CLSCTX_ALL,
                               None, ctypes.byref(cli_r))
                if _hres_bad(hr) or not (cli_r and cli_r.value):
                    self._fail("无法激活输出端 IAudioClient")
                    return
                hr = _com_call(cli_r, VTBL_AUDIOCLIENT_GET_MIX_FORMAT,
                               ctypes.byref(ren_ptr))
                if _hres_bad(hr) or not ren_ptr.value:
                    self._fail("无法读取输出混音格式")
                    return
                wfr = ctypes.cast(ren_ptr,
                                  ctypes.POINTER(_WAVEFORMATEX)).contents
                ren_ch = int(wfr.nChannels)
                ren_bits, ren_block = int(wfr.wBitsPerSample), int(wfr.nBlockAlign)
                ren_tag = int(wfr.wFormatTag)
                if ren_tag == 0xFFFE:
                    ren_tag = int(ctypes.cast(
                        ren_ptr,
                        ctypes.POINTER(_WAVEFORMATEXTENSIBLE)
                    ).contents.SubFormat.Data1)
                if ren_ch <= 0 or ren_block <= 0:
                    self._fail("输出混音格式异常")
                    return
                if ren_tag != 3 or ren_bits != 32:
                    self._fail("输出混音格式不受支持（tag=%d bits=%d）"
                               % (ren_tag, ren_bits))
                    return
                hr = _com_call(cli_r, VTBL_AUDIOCLIENT_INITIALIZE,
                               AUDCLNT_SHARE_SHARED, 0, 200 * 10000, 0,
                               ren_ptr, None)
                if _hres_bad(hr):
                    self._fail("输出端 Initialize 失败，HRESULT=0x%08X"
                               % (hr & 0xFFFFFFFF))
                    return
                buf_frames = ctypes.c_uint32()
                hr = _com_call(cli_r, VTBL_AUDIOCLIENT_GET_BUFFER_SIZE,
                               ctypes.byref(buf_frames))
                if _hres_bad(hr) or buf_frames.value == 0:
                    self._fail("无法读取输出缓冲大小")
                    return
                hr = _com_call(cli_r, VTBL_AUDIOCLIENT_GET_SERVICE,
                               ctypes.byref(IID_IAUDIO_RENDER_CLIENT),
                               ctypes.byref(ren_c))
                if _hres_bad(hr) or not (ren_c and ren_c.value):
                    self._fail("无法获取 IAudioRenderClient")
                    return
                hr = _com_call(cli_c, VTBL_AUDIOCLIENT_START)
                if _hres_bad(hr):
                    self._fail("采集端 Start 失败，HRESULT=0x%08X"
                               % (hr & 0xFFFFFFFF))
                    return
                hr = _com_call(cli_r, VTBL_AUDIOCLIENT_START)
                if _hres_bad(hr):
                    self._fail("输出端 Start 失败，HRESULT=0x%08X"
                               % (hr & 0xFFFFFFFF))
                    return

                with self._lock:
                    self.dev_name = _imm_device_name(dev_c.value)
                    self.out_name = _imm_device_name(dev_r.value)
                    self.rate, self.ch = rate, ch
            finally:
                _com_release(enumerator)

            # ---- 主循环：采包 → pending；pending → 渲染缓冲 ----
            packet = ctypes.c_uint32()
            p_data = ctypes.c_void_p()
            frames = ctypes.c_uint32()
            flags = ctypes.c_uint32()
            dev_pos = ctypes.c_uint64()
            qpc = ctypes.c_uint64()
            pad = ctypes.c_uint32()
            pending = bytearray()
            byte_avail = int(buf_frames.value) * block_align

            while not stop.is_set():
                if stop.wait(0.005):
                    break
                broken = False
                # 1) 排干采集包（静音包补零，保证渲染时钟一直有料）
                for _ in range(64):
                    hr = _com_call(cap_c, VTBL_CAPTURE_GET_NEXT_PACKET_SIZE,
                                   ctypes.byref(packet))
                    if _hres_bad(hr):
                        broken = True
                        break
                    if packet.value == 0:
                        break
                    hr = _com_call(cap_c, VTBL_CAPTURE_GET_BUFFER,
                                   ctypes.byref(p_data), ctypes.byref(frames),
                                   ctypes.byref(flags), ctypes.byref(dev_pos),
                                   ctypes.byref(qpc))
                    if _hres_bad(hr):
                        broken = True
                        break
                    n = int(frames.value)
                    if n > 0:
                        with self._lock:
                            self.pkts += 1
                        if (flags.value & AUDCLNT_SILENT) or not p_data.value:
                            with self._lock:
                                self.silent_pkts += 1
                            pending += b"\x00" * (n * block_align)
                        else:
                            raw = ctypes.string_at(
                                p_data.value, n * block_align)
                            with self._lock:
                                for v in struct.unpack(
                                        "<%df" % n, raw):
                                    a = abs(v)
                                    if a > self.cap_peak:
                                        self.cap_peak = a
                            pending += raw
                    _com_call(cap_c, VTBL_CAPTURE_RELEASE_BUFFER, n)
                if broken:
                    self._fail("麦克风数据流已中断（设备被拔出？）")
                    return

                # 2) 软件上混后往渲染端推：单声道 float32 → 渲染混音格式
                #    （样本写 FL/FR，其余声道清零）。积压超过一个缓冲就丢
                #    到最新，防延迟增长。
                if len(pending) > byte_avail:
                    del pending[:len(pending) - byte_avail]
                while len(pending) >= block_align:
                    hr = _com_call(cli_r, VTBL_AUDIOCLIENT_GET_CURRENT_PADDING,
                                   ctypes.byref(pad))
                    if _hres_bad(hr):
                        self._fail("输出端数据流已中断")
                        return
                    free = int(buf_frames.value) - int(pad.value)
                    if free <= 0:
                        break
                    n = min(free, len(pending) // block_align)
                    p_buf = ctypes.c_void_p()
                    hr = _com_call(ren_c, VTBL_RENDER_GET_BUFFER, n,
                                   ctypes.byref(p_buf))
                    if _hres_bad(hr) or not p_buf.value:
                        self._fail("输出端 GetBuffer 失败")
                        return
                    # 单声道 → ren_ch 声道：样本进 FL/FR，其余清零
                    samples = struct.unpack(
                        "<%df" % n, bytes(pending[:n * block_align]))
                    out = [0.0] * (n * ren_ch)
                    for i, s in enumerate(samples):
                        out[i * ren_ch] = s       # FL
                        out[i * ren_ch + 1] = s   # FR
                    ctypes.memmove(p_buf.value,
                                   struct.pack("<%df" % (n * ren_ch), *out),
                                   n * ren_block)
                    _com_call(ren_c, VTBL_RENDER_RELEASE_BUFFER, n, 0)
                    del pending[:n * block_align]
                    with self._lock:
                        self.sent += n * ren_block
        finally:
            for cli in (cli_c, cli_r):
                if cli is not None and cli.value:
                    try:
                        _com_call(cli, VTBL_AUDIOCLIENT_STOP)
                    except Exception:
                        pass
            _com_release(cap_c)
            _com_release(ren_c)
            _com_release(cli_c)
            _com_release(cli_r)
            _com_release(dev_c)
            _com_release(dev_r)
            if mix_ptr.value:
                _ole32.CoTaskMemFree(mix_ptr)
            if ren_ptr.value:
                _ole32.CoTaskMemFree(ren_ptr)
            with self._lock:
                self.running = False

    def _fail(self, why):
        with self._lock:
            self.err = why


LIS = MicListen()

# winmm 只用来在自检里播一段 wav 当信号源
SND_ASYNC = 0x0001
SND_NODEFAULT = 0x0002
SND_LOOP = 0x0008
SND_FILENAME = 0x00020000
try:
    _winmm = ctypes.WinDLL("winmm.dll")
    _winmm.PlaySoundW.restype = ctypes.c_int
    _winmm.PlaySoundW.argtypes = [ctypes.c_wchar_p, ctypes.c_void_p,
                                  ctypes.c_uint32]
except OSError:
    _winmm = None


def _play_wav(path, loop=False):
    """往默认播放设备播一段 wav，异步。

    为什么自检要自己发声：回环在「系统静音」时返回全 0 是**正确行为** ——
    不给信号源就分不清「链路坏了」和「本来就没声音」。
    """
    if _winmm is None:
        return False
    flags = SND_FILENAME | SND_ASYNC | SND_NODEFAULT
    if loop:
        flags |= SND_LOOP  # SND_LOOP 必须与 SND_ASYNC 同用
    return _winmm.PlaySoundW(path, None, flags) != 0


# 与前端 BANDS 逐项一致（lo/hi 是 512 条 bin 的下标）。
# 也是两条线（Go / Python）自检输出的可比项，改一处必须同步另一处。
LB_BANDS = [
    ("超低音", 0, 1), ("低音", 2, 3), ("中低音", 4, 7), ("中音", 8, 18),
    ("中高音", 19, 46), ("临场感", 47, 93), ("明亮度", 94, 186),
    ("空气感", 187, 372),
]


def _lb_synth(rate, amp):
    """把一段 bin 中心正弦灌进环形缓冲并跑到收敛，返回 512 字节。

    与 Chromium 侧 probe_wa_spectrum.js 的信号完全一致，
    是两条实现可比的前提。频点取 937.5 Hz = 48000·20/1024。
    """
    lb = Loopback()
    lb.rate, lb.running = rate, True
    # _ring_w 设成 1024：_compute_spectrum 取 ring[(w-1024+8192)%8192 ...]，
    # 起点正好是 0，与下面填样本的位置对齐。
    lb._ring_w = LB_FFT_SIZE
    lb._ring_n = LB_FFT_SIZE
    for i in range(LB_FFT_SIZE):
        lb._ring[i] = amp * math.sin(2 * math.pi * 937.5 * i / rate)
    for _ in range(40):  # 让平滑收敛
        lb._compute_spectrum()
    return bytes(lb._bins)


def _loopback_fft(dump=False):
    """设备无关的频谱正确性自检。

    期望值不是推导出来的，是**实测**的 —— 由 .workbuddy-ai/tmp/probe_wa_spectrum.js
    在 headless Chrome 里扫振幅、等平滑收敛后读出的稳态字节。
    与 Wails 线的 --loopback-fft 输出必须逐行一致。
    """
    rate = 48000
    line = "=" * 68
    dash = "-" * 68
    print(line)
    print(" %s %s  ·  频谱计算自检（与 Chromium 同字节）" % (APP_NAME, APP_VER))
    print(line)
    print("采样率   : %d Hz · FFT %d 点 · %d 个 bin · 每 bin %.4f Hz"
          % (rate, LB_FFT_SIZE, LB_BINS, float(rate) / LB_FFT_SIZE))
    print("窗函数   : Blackman（相干增益 %.2f）· 平滑 τ = %.1f（每调用一次）"
          % (BLACKMAN_GAIN, WA_SMOOTHING))
    print("归一化   : mag = |X[k]| / N（实测反解，非规范正文的 2/(N·CG)）")
    print("dB 区间  : %.0f .. %.0f" % (WA_MIN_DB, WA_MAX_DB))
    print(dash)

    cases = [
        ("满幅 0 dB", 1.0, 255, 19, 21),
        ("-20 dB", 0.1, 242, 0, 0),
        ("-40 dB", 0.01, 169, 0, 0),
        ("-60 dB", 0.001, 96, 0, 0),
        ("-80 dB", 0.0001, 23, 0, 0),
        ("静音", 0.0, 0, 0, 0),
    ]
    bad = 0
    for name, amp, want, tie_lo, tie_hi in cases:
        b = _lb_synth(rate, amp)
        lo = hi = -1
        pv = 0
        for k, v in enumerate(b):
            if v > pv:
                pv, lo, hi = v, k, k
            elif v == pv and pv > 0:
                hi = k
        where = "#%d" % lo
        if hi != lo:
            where = "#%d..#%d" % (lo, hi)

        why = ""
        if amp == 0:
            if pv != 0:
                why = "静音应全 0"
            where = "—"
        elif abs(pv - want) > 1:
            why = "字节应为 %d±1" % want
        elif lo < 0 or not (lo <= 20 <= hi):
            why = "峰值区间应含 #20"
        elif tie_lo and (lo != tie_lo or hi != tie_hi):
            why = "并列区间应为 #%d..#%d" % (tie_lo, tie_hi)
        verdict = "OK" if not why else ("✗ " + why)
        if why:
            bad += 1
        print("  %-10s 峰值 %-11s 字节 %-4d 期望 %-4d %s"
              % (name, where, pv, want, verdict))

    print(dash)
    if bad == 0:
        print("结论     : 全部通过 —— 与 Chromium AnalyserNode 同字节")
    else:
        print("结论     : %d 项不符" % bad)
    print(line)

    if dump:
        b = _lb_synth(rate, 0.01)
        print("BINS " + " ".join(str(v) for v in b))
    return 0 if bad == 0 else 1


def _resolve_loopback_device(arg):
    """把 --device 的参数解析成端点 ID（与 Wails 线同名函数同语义）。

    接受端点 ID 原样采用，或在端点友好名里做不区分大小写的包含匹配 ——
    本机耳机端点叫「扬声器 (iKF-V11)」，敲 ``--device iKF`` 即可，不必
    粘那一长串 GUID。arg 为空表示跟随系统默认。

    返回 ``(dev_id, dev_name)``；解析不到时返回 ``(None, 错误说明)``。
    """
    res = LOOPBACK.devices()
    if not res.get("ok"):
        return None, "枚举端点失败 —— %s" % res.get("error", "未知原因")
    lst = res["list"]

    def _name_of(dev_id):
        for d in lst:
            if d["id"] == dev_id:
                return d["name"]
        return dev_id

    if not arg:
        return "", _name_of(res.get("default") or "")
    for d in lst:
        if d["id"] == arg:
            return arg, d["name"]
    low = arg.lower()
    for d in lst:
        if low in d["name"].lower():
            return d["id"], d["name"]
    return None, "找不到端点 %r —— 可选：%s" % (
        arg, " / ".join(d["name"] for d in lst))


def _loopback_test(secs, drv, wav=None, tone=False, dev_arg=""):
    """无界面的回环采集自检，与 Wails 线的 --loopback-test 输出同构。"""
    line = "=" * 68
    dash = "-" * 68
    print(line)
    print(" %s %s  ·  系统回环采集自检" % (APP_NAME, APP_VER))
    print(line)
    print("驱动     : %s" % (drv.dll or "(未连接)"))

    # ★ 「默认播放设备」不一定是耳机。本机装了 FxSound，默认端点是
    #   FxSound Speakers，而界面（律动空间）是**显式锁耳机端点**的
    #   （设计稿 pickHeadset：名字含 iKF → 含 C-Media → 回落默认）。
    #   所以这里的默认行为与界面**不一致**，只适合验采集链路本身；
    #   要验「耳机那条路上有没有声音」必须 --device。
    dev, dev_name = _resolve_loopback_device(dev_arg)
    if dev is None:
        print(dash)
        print("端点     : %s" % dev_name)
        print(line)
        return 1
    if not dev_arg:
        print("端点     : 跟随系统默认（%s）" % dev_name)
        print("           ↑ 默认未必是耳机；装了 FxSound 之类虚拟声卡时")
        print("             默认端点常是虚拟设备，界面里则是显式锁耳机")
    else:
        print("端点     : 已锁定 %s" % dev_name)
    print(dash)

    res = LOOPBACK.start(dev)
    if not res.get("ok"):
        print("回环     : 启动失败 —— %s" % res.get("error"))
        return 1
    rate = int(res.get("rate") or 0)
    ch = int(res.get("ch") or 0)
    print("回环     : 已启动（%d Hz · %d 声道 · %s）"
          % (rate, ch, res.get("format")))
    raw_tag = int(res.get("tag") or 0)
    bits = int(res.get("bits") or 0)
    print("混音格式 : wFormatTag=0x%04X%s · %d bit"
          % (raw_tag,
             ("（EXTENSIBLE）SubFormat=0x%04X" % int(res.get("sub") or 0))
             if raw_tag == 0xFFFE else "",
             bits))
    if bits == 0:
        print("           ↑ 位深读成 0，多半是 WAVEFORMATEXTENSIBLE 布局算错了")

    if tone:
        # 掩码 1 = 左前（TEST_CHANNELS 里的一档）。★ 第 28 轮之前这里写的是
        # 0x6BF —— 那串位既不是单个声道也不是驱动的「整组」白名单，
        # 加上第一个参数传错，无论传什么都是 0x10001，看不出来是参数问题。
        #
        # PlayTestSound 是驱动往**耳机**那条路推的测试音，所以「采不采得到」
        # 取决于回环锁在哪个端点：锁耳机 → 采得到；跟随默认（本机默认是
        # FxSound Speakers）→ 采不到，频谱恒 0。频谱全 0 而帧数照涨时，
        # 先怀疑端点对不上，别怀疑 FFT。
        print("测试音   : 驱动 PlayTestSound %s" % (drv.play_test(1),))
    if wav:
        if _play_wav(wav):
            print("信号源   : %s" % wav)
        else:
            print("信号源   : 播放失败 —— %s" % wav)

    st0 = LOOPBACK.status()
    frames0 = int(st0.get("frames") or 0)
    peak_level = 0.0
    band_sum = [0.0] * len(LB_BANDS)
    band_max = [0] * len(LB_BANDS)
    global_max = 0
    best = None
    best_top = -1
    samples = 0

    deadline = time.time() + secs
    while time.time() < deadline:
        time.sleep(0.1)
        sp = LOOPBACK.spectrum()
        if not sp.get("ok"):
            continue
        raw = base64.b64decode(sp["bins"])
        if len(raw) != LB_BINS:
            continue
        samples += 1
        peak_level = max(peak_level, float(sp.get("level") or 0))
        top = 0
        for bi, (_nm, lo, hi) in enumerate(LB_BANDS):
            s = 0
            for k in range(lo, min(hi + 1, len(raw))):
                v = raw[k]
                s += v
                if v > band_max[bi]:
                    band_max[bi] = v
                if v > global_max:
                    global_max = v
                if v > top:
                    top = v
            band_sum[bi] += float(s) / (hi - lo + 1)
        if top > best_top:
            best_top = top
            best = raw

    st1 = LOOPBACK.status()
    got = int(st1.get("frames") or 0) - frames0
    expect = float(rate) * secs
    print(dash)
    print("采样     : %.1f 秒内收到 %d 个频谱帧" % (secs, samples))
    print("采集帧数 : %d（期望约 %.0f，达成率 %.1f%%）"
          % (got, expect, (got / expect * 100.0) if expect else 0.0))
    if samples == 0 or got == 0:
        print("结论     : 采集链路**没有**跑起来 —— 检查回环是否被设备占用")
        print(line)
        LOOPBACK.stop()
        return 1
    print("峰值电平 : %.4f（0..1，满幅为 1）" % peak_level)
    print(dash)
    print("频段均值 / 峰值（字节 0..255，-100dB..-30dB）：")
    for bi, (nm, _lo, _hi) in enumerate(LB_BANDS):
        avg = band_sum[bi] / samples
        n = int(avg / 255 * 24)
        n = 0 if n < 0 else (24 if n > 24 else n)
        print("  %-8s %5.1f  [%s%s]  %d"
              % (nm, avg, "#" * n, "." * (24 - n), band_max[bi]))
    print(dash)

    if best is not None:
        pk = 0
        pv = 0
        for k, v in enumerate(best):
            if v > pv:
                pk, pv = k, v
        print("峰值 bin : #%d ≈ %.0f Hz（字节 %d，取最响的一帧）"
              % (pk, float(pk) * rate / LB_FFT_SIZE, pv))
    print("声道掩码 : 0x%04X" % int(st1.get("mask") or 0))
    print("字节极值 : 全局最大 %d" % global_max)
    if global_max:
        print("结论     : 链路健康，频谱有内容")
    else:
        # ★ 这里以前只写「FFT 或归一化有问题」，把人往错的方向引。
        #   实测（本机装 FxSound）：跟随默认端点时帧数照涨、频谱恒 0，
        #   锁到耳机端点后同一个测试音立刻有内容 —— 端点对不上是第一嫌疑。
        print("结论     : 链路健康，频谱全 0 —— 先查端点，再查 FFT")
        print("可能原因 :")
        print("           ① 回环端点与发声设备不是同一个（最常见）。本机装")
        print("              了 FxSound 时默认端点就是虚拟设备，试 --device iKF；")
        print("           ② 信号源根本没出声（系统静音 / 音量拉到 0…）；")
        print("           ③ FFT 或归一化有问题 —— 可用 --loopback-fft 排除。")
    print(line)
    LOOPBACK.stop()
    return 0


def _loopback_devices():
    """列出可锁定的播放端点（--loopback-devices）。

    输出与 Wails 线的 --loopback-devices **逐行一致**，是两条线设备枚举
    实现的回归判据；同时也是「仅耳机可视化」的人工验收入口 —— 拿这里的
    端点 ID 填给 /api/loopback/start 的 device 参数即可锁定该设备。
    """
    line = "=" * 68
    dash = "-" * 68
    print(line)
    print(" %s %s  ·  播放端点列表（WASAPI loopback 可选）" % (APP_NAME, APP_VER))
    print(line)
    print("[默认] 标记当前默认播放设备；回环不带 device 参数时采它。")
    print("蓝牙耳机选 Stereo/A2DP，不要选 Hands-Free（16k 窄带，音乐不在那条上）。")
    print(dash)
    res = LOOPBACK.devices()
    if not res.get("ok"):
        print("枚举失败 —— %s" % res.get("error", "未知原因"))
        print(line)
        return 1
    for i, d in enumerate(res["list"]):
        label = d["name"]
        if d["id"] == res["default"]:
            label = "[默认] " + label
        print(" %d. %s" % (i + 1, label))
        print("      %s" % d["id"])
    print(dash)
    print("共 %d 个端点" % len(res["list"]))
    print(line)
    return 0


def _play_only(path, secs, loop=False):
    """只播一段 wav，不开回环（手工验证信号源用）。"""
    if not _play_wav(path, loop):
        print("播放失败：%s" % path)
        return 1
    print("正在播放 %s（%.0f 秒%s，走默认播放设备）"
          % (path, secs, "，循环" if loop else ""))
    try:
        time.sleep(secs)
    except KeyboardInterrupt:
        pass
    if _winmm is not None:
        _winmm.PlaySoundW(None, None, 0)  # 停止
    return 0


# ====================================================================

# ---------------------------------------------------------------- 驱动管理
#
# 与 Go 版（setup.go）逐字段一致：主程序只管运行，装/卸驱动交给同目录的
# iKF-驱动管理.exe —— 它自提权，主进程因此恒定无需 UAC。

SETUP_NAME = "iKF-驱动管理.exe"


def _setup_path():
    """同目录下的驱动管理程序；没带就返回空串（绿色包被拆散了）。"""
    d = _app_dir()
    if not d:
        return ""
    p = os.path.join(d, SETUP_NAME)
    return p if os.path.isfile(p) else ""


def driver_info(drv):
    """主程序眼里的驱动现状。ready 与 setup_ok 是两件事：
    前者是「能不能跑」（端点枚举得出来吗），后者是「有没有带安装器」。"""
    setup = _setup_path()
    info = {
        "ok": True,
        "ready": drv.lib is not None and drv.ctx is not None,
        "devices": drv.device_count,
        "capture": drv.cap_count,
        "cpl": drv.cpl,
        "dll": drv.dll,
        "error": drv.error or "",
        "setup": setup,
        "setup_ok": setup != "",
    }
    # ★ 2026-10-22：把「安装指纹」挂出来（见 init_fp_summary）—— 这是
    #   「初始化该全量写还是只补空缺」的判据本体现状，与 Go 侧 DriverInfo
    #   逐字段一致。只在 /api/driver 这类按需接口上取，不进 /api/status 轮询。
    if info["ready"]:
        try:
            info["init_fp"] = init_fp_summary(drv)
        except Exception:
            pass
    return info


def _run_elevated(exe, params):
    """用 runas 拉起 exe 并等它退出，返回 (退出码, 错误)。

    用 SHELLEXECUTEINFOW 而不是 ShellExecuteW：只有前者给进程句柄能等 ——
    装/卸驱动会重建端点，不等就重连会读到作废的设备上下文。
    """
    class SHELLEXECUTEINFOW(ctypes.Structure):
        _fields_ = [
            ("cbSize", wintypes.DWORD),
            ("fMask", wintypes.DWORD),
            ("hwnd", wintypes.HWND),
            ("lpVerb", wintypes.LPCWSTR),
            ("lpFile", wintypes.LPCWSTR),
            ("lpParameters", wintypes.LPCWSTR),
            ("lpDirectory", wintypes.LPCWSTR),
            ("nShow", ctypes.c_int),
            ("hInstApp", wintypes.HINSTANCE),
            ("lpIDList", ctypes.c_void_p),
            ("lpClass", wintypes.LPCWSTR),
            ("hkeyClass", wintypes.HKEY),
            ("dwHotKey", wintypes.DWORD),
            ("hIcon", wintypes.HANDLE),
            ("hProcess", wintypes.HANDLE),
        ]

    sei = SHELLEXECUTEINFOW()
    sei.cbSize = ctypes.sizeof(sei)
    sei.fMask = 0x00000040            # SEE_MASK_NOCLOSEPROCESS
    sei.lpVerb = "runas"
    sei.lpFile = exe
    sei.lpParameters = params or None
    # 提权后子进程的当前目录会变成 System32，工作目录必须显式给
    sei.lpDirectory = os.path.dirname(exe)
    sei.nShow = 1                     # SW_SHOWNORMAL

    shell32 = ctypes.windll.shell32
    shell32.ShellExecuteExW.argtypes = [ctypes.POINTER(SHELLEXECUTEINFOW)]
    shell32.ShellExecuteExW.restype = wintypes.BOOL
    if not shell32.ShellExecuteExW(ctypes.byref(sei)):
        return 0, ctypes.WinError()
    if not sei.hProcess:
        return 0, None
    k32 = ctypes.windll.kernel32
    try:
        k32.WaitForSingleObject(wintypes.HANDLE(sei.hProcess), 0xFFFFFFFF)
        code = wintypes.DWORD()
        if not k32.GetExitCodeProcess(wintypes.HANDLE(sei.hProcess),
                                      ctypes.byref(code)):
            return 0, None
        return code.value, None
    finally:
        k32.CloseHandle(wintypes.HANDLE(sei.hProcess))


def run_setup(drv, action):
    """拉起驱动管理程序并等它结束，然后重新连一次驱动。阻塞几秒到几十秒。"""
    if action not in ("", "install", "uninstall"):
        return {"ok": False, "error": "action 只能是 install 或 uninstall"}
    setup = _setup_path()
    if not setup:
        return {"ok": False,
                "error": "同目录下找不到 " + SETUP_NAME + "，无法安装或卸载驱动组件。"}
    params = {"install": "--install", "uninstall": "--uninstall"}.get(action, "")

    # 先松手：装驱动会重启设备实例，握着旧上下文等它回来只会读出错值
    drv.close()
    code, err = _run_elevated(setup, params)
    # ★ 2026-10-22：装完立刻重连走 reopen()（带退避重试），与 Go 版
    #   RunSetup 对齐 —— 端点重建期间采集端点常比渲染端点晚到。
    ok = drv.reopen()
    # ★ 2026-10-22：装完是**最该**跑初始化链的时刻 —— 安装器清空了
    #   FxProperties，设备参数全回 ret=2。不补跑就是「全未设置」，用户得
    #   自己去场景页点「默认」。与 Go 侧 RunSetup 同一套。
    init = ensure_initial_state(drv) if ok else {}

    info = driver_info(drv)
    if err:
        info["ok"] = False
        info["error"] = "无法启动驱动管理程序：" + str(err)
        return info
    info["action"] = action
    info["exit_code"] = code
    if init:
        info["init"] = init
    if code != 0:
        info["ok"] = False
        info["error"] = ("已取消：没有授予管理员权限，驱动组件未改动。" if code == 1223
                         else "驱动管理程序以退出码 %d 结束，请查看它的界面提示。" % code)
    return info


class Handler(BaseHTTPRequestHandler):
    driver = None
    html = b""
    server_version = "iKFAC/" + APP_VER

    def log_message(self, *args):
        pass

    def _send(self, code, body, ctype):
        if isinstance(body, str):
            body = body.encode("utf-8")
        try:
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionAbortedError, OSError):
            pass

    def _json(self, obj, code=200):
        self._send(code, json.dumps(obj, ensure_ascii=False),
                   "application/json; charset=utf-8")

    def do_GET(self):
        path, _, query = self.path.partition("?")
        ep = _query_ep(query)
        if path in ("/", "/index.html"):
            return self._send(200, self.html, "text/html; charset=utf-8")
        if path == "/favicon.ico":
            return self._send(204, b"", "image/x-icon")
        if path == "/api/status":
            return self._json(self.driver.status())
        if path == "/api/params":
            return self._json(self.driver.param_map(ep))
        if path == "/api/meter":
            return self._json(self.driver.meter(ep))
        if path == "/api/ping":
            return self._json({"ok": True, "app": APP_NAME, "ver": APP_VER})
        if path == "/api/profiles":
            return self._json(self.driver.profiles())
        if path == "/api/driver":
            return self._json(driver_info(self.driver))
        if path == "/api/profile/items":
            # 场景详情与「已修改未保存」脏标记的数据源
            try:
                nm = parse_qs(query).get("name", [""])[0]
            except Exception:
                nm = ""
            return self._json(self.driver.profile_items(nm))
        # 可视化页的音源（系统回环采集，见 Loopback）。契约与 Wails 线逐字段一致。
        if path == "/api/loopback/devices":
            return self._json(LOOPBACK.devices())
        if path == "/api/loopback/status":
            return self._json(LOOPBACK.status())
        if path == "/api/spectrum":
            return self._json(LOOPBACK.spectrum())
        # ★ 2026-10-20 内嵌录音机（MicRecorder）。契约与 Wails 线逐字段一致。
        #   旧 POST /api/recorder「拉起官方 Recorder.exe」已废弃 —— 那个 exe
        #   是依附官方面板 COM 环境的死件（取证见工具档），Python 线过去
        #   连路由都没有（前端调了必 404），本就是两线不同步的旧账。
        if path == "/api/recorder/status":
            return self._json(REC.status())
        if path == "/api/listen/status":
            return self._json(LIS.status())
        return self._json({"ok": False, "error": "not found"}, 404)

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        if path == "/api/loopback/start":
            # 请求体是可选的 {"device": "<端点 ID>"}：空体 / 空串 = 跟随
            # 默认播放设备（旧行为），带了就锁定那个端点。
            device = ""
            try:
                n = int(self.headers.get("Content-Length") or 0)
                if n:
                    body = json.loads(self.rfile.read(n).decode("utf-8") or "{}")
                    device = body.get("device") or ""
                    if not isinstance(device, str):
                        device = ""
            except Exception:
                return self._json({"ok": False, "error": "请求体解析失败"}, 400)
            return self._json(LOOPBACK.start(device))
        if path == "/api/loopback/stop":
            return self._json(LOOPBACK.stop())
        if path == "/api/recorder/start":
            return self._json(REC.start())
        if path == "/api/recorder/stop":
            return self._json(REC.stop())
        # ★ 2026-10-20 晚：录音灵动岛「打开文件夹」—— 资源管理器打开
        #   录音所在目录并**选中**该 WAV（explorer /select,）。路径由
        #   前端透传（来自 stop 响应的 file），只作为 shell 参数、
        #   不落任何解释器，无注入面。
        if path == "/api/recorder/open":
            try:
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n).decode("utf-8") or "{}") if n else {}
            except Exception:
                return self._json({"ok": False, "error": "请求体解析失败"}, 400)
            p = body.get("path") or ""
            if not isinstance(p, str) or not p:
                return self._json({"ok": False, "error": "缺少 path"}, 400)
            try:
                subprocess.Popen(["explorer", "/select,", p])
            except Exception as e:
                return self._json({"ok": False, "error": str(e)})
            return self._json({"ok": True})
        if path not in ("/api/set", "/api/test", "/api/listen", "/api/asio",
                        "/api/profile/save", "/api/profile/apply",
                        "/api/profile/delete", "/api/profile/rename",
                        "/api/driver/setup", "/api/driver/reopen"):
            return self._json({"ok": False, "error": "not found"}, 404)
        try:
            n = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(n).decode("utf-8") or "{}")
        except Exception:
            return self._json({"ok": False, "error": "请求体解析失败"}, 400)

        if path == "/api/driver/reopen":
            # ★ 2026-10-21：界面上「重新检测」的后端入口，与 Go 版
            #   Driver.ReopenInfo 同契约（driver_info + reopened）。
            #   装完驱动 / 端点重建窗口期里启动失败时用来自恢复。
            #   ★ 2026-10-22：改走 reopen()（带退避重试），不再手写
            #   close + open —— 装完驱动那一刻采集端点常比渲染端点晚到。
            ok = self.driver.reopen()
            info = driver_info(self.driver)
            info["ok"] = bool(ok)
            info["reopened"] = bool(ok)
            # ★ 2026-10-22：连上之后必须补跑初始化链（恢复默认 / 自愈）。
            #   以前这里只重连 —— 而装完驱动点「重新检测」是最常见的路径，
            #   启动那次恢复因驱动没就绪被跳过，设备停在「全 ret=2」，
            #   用户必须自己去场景页点「默认」。见 ensure_initial_state。
            if ok:
                init = ensure_initial_state(self.driver)
                if init:
                    info["init"] = init
            return self._json(info)
        if path == "/api/driver/setup":
            # 提权拉起同目录的驱动管理程序并等它结束，会阻塞数十秒
            act = body.get("action") or ""
            if act not in ("", "install", "uninstall"):
                return self._json({"ok": False,
                                   "error": "action 只能是 install 或 uninstall"}, 400)
            return self._json(run_setup(self.driver, act))
        if path == "/api/profile/save":
            return self._json(self.driver.profile_save(
                body.get("name"), body.get("desc") or ""))
        if path == "/api/profile/apply":
            return self._json(self.driver.profile_apply(body.get("name")))
        if path == "/api/profile/delete":
            return self._json(self.driver.profile_delete(body.get("name")))
        if path == "/api/profile/rename":
            return self._json(self.driver.profile_rename(
                body.get("name"), body.get("new_name")))

        if path == "/api/set":
            name = body.get("name")
            if not name:
                return self._json({"ok": False, "error": "缺少参数名"}, 400)
            ep = body.get("ep") or EP_RENDER
            if ep not in ENDPOINTS:
                ep = EP_RENDER
            # write_auto：白名单内按控件钳制；白名单外但属于 208 项的
            # （如音箱方位 SS_*_ANGLE）也真写一次，由驱动决定收不收。
            return self._json(self.driver.write_auto(
                name, body.get("value"), ep, body.get("kind")))

        if path == "/api/test":
            act = body.get("action") or "play"
            if act == "stop":
                return self._json(self.driver.stop_test())
            if act == "close":
                return self._json(self.driver.close_test())
            try:
                mask = int(body.get("mask") or 0)
            except (TypeError, ValueError):
                return self._json({"ok": False, "error": "掩码不合法"}, 400)
            return self._json(self.driver.play_test(mask))

        if path == "/api/listen":
            # 2026-10-20 内嵌监听：官方 RunListen2Device 依赖的 COM
            # 服务器驱动包里没有（见 工具/监听取证-监听COM组件缺失.md），
            # 改走 MicListen（listen.go 对应实现）。响应保留 {ok, on}
            # 形状，前端零改动。
            on = bool(body.get("on"))
            out = LIS.start() if on else LIS.stop()
            out["on"] = on
            return self._json(out)

        return self._json(self.driver.show_asio())


# ====================================================================
# 五、界面（新版设计稿 + 驱动桥接层）
# ====================================================================

HTML = r"""__HTML__"""


# ====================================================================
# 六、入口
# ====================================================================

def _free_port(start=17800, tries=40):
    for p in range(start, start + tries):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                s.bind(("127.0.0.1", p))
                return p
            except OSError:
                continue
    return 0


def _audio_rust_status():
    """音频热路 Rust DLL（ikf_audio.dll）状态 —— 与 Go 线 selfTest 输出逐行对齐。

    P1 只把 Go exe 的热路径切到 Rust；Python 线不消费该 DLL，这里只做
    存在性/ABI 探测供体检对齐（true 单源在 audiocore/target/release）。
    """
    import ctypes
    here = os.path.dirname(os.path.abspath(__file__))
    # here 可能是仓库根（构建产物）或 tools/src（源码态），两级候选都给
    cand = [os.path.join(here, "iKF-Audio-Center-Wails",
                         "audiocore", "target", "release", "ikf_audio.dll"),
            os.path.join(here, os.pardir, os.pardir, "iKF-Audio-Center-Wails",
                         "audiocore", "target", "release", "ikf_audio.dll"),
            os.path.join(here, os.pardir, "iKF-Audio-Center-Wails",
                         "audiocore", "target", "release", "ikf_audio.dll")]
    path = next((c for c in cand if os.path.isfile(c)), None)
    if path is None:
        return "rust: 未启用（ikf_audio.dll 未构建，cargo build --release audiocore）"
    try:
        lib = ctypes.CDLL(path)
        abi = lib.ikf_abi_version()
        lib.ikf_version.restype = ctypes.c_int
        n = lib.ikf_version(None, 0)
        buf = ctypes.create_string_buffer(n)
        lib.ikf_version(buf, n)
        ver = buf.value.decode("utf-8", "replace").rstrip("\x00")
        ok = lib.ikf_selftest() == 0xF00D
        return "rust: %s (abi %d, selftest %s, Py 线未接入)" % (ver, abi, "ok" if ok else "FAIL")
    except Exception as e:
        return "rust: 未启用（加载失败: %s）" % e


def _self_test(drv):
    print("=" * 68)
    print(" %s %s  ·  接口自检" % (APP_NAME, APP_VER))
    print("=" * 68)
    print("驱动目录 : %s" % drv.cpl)
    print("驱动模块 : %s" % drv.dll)
    print("音频热路 : %s" % _audio_rust_status())
    if drv.lib is None:
        print("状态     : 未连接 —— %s" % drv.error)
        return 1
    st = drv.status()
    print("状态     : 已连接（%d 个设备端点）" % st["device_count"])
    print("-" * 68)
    for k, label in (("model", "设备型号"), ("codec", "编解码器"),
                     ("driver", "驱动版本"), ("firmware", "固件版本"),
                     ("formats", "音频格式"), ("hwid", "设备 ID")):
        print("%-9s: %s" % (label, st[k]))
    print("%-9s: %s" % ("主音量", "%.2f dB（范围 %.1f .. %.1f，步进 %.1f）"
                        % (st["vol_db"] or 0, st["vol_min"] or 0,
                           st["vol_max"] or 0, st["vol_step"] or 0)))
    print("%-9s: %s" % ("标量音量", "%.4f" % (st["vol_scalar"] or 0)))
    print("%-9s: %s" % ("静音", "开" if st["mute"] else "关"))
    print("%-9s: %s（%s）" % ("均衡器", "开" if st["eq_on"] else "关",
                              " ".join("%+d" % v for v in st["eq"])))
    print("-" * 68)
    ok = unsup = notfound = broken = 0
    for name in ALL_NAMES:
        r, _v = drv.read(name)
        if r == 0:
            ok += 1
        elif r == 2:
            unsup += 1
        elif r == -1:
            notfound += 1
        else:
            broken += 1
    print("属性总数 : %d（通用 %d + OEM %d）"
          % (len(ALL_NAMES), len(GENERIC_NAMES), len(OEM_NAMES)))
    print("可读     : %d" % ok)
    # 2026-10-22：原写「本机不支持」。ret=2 的实测含义是「存在但从未写过」
    # （卸载/重装驱动清空 FxProperties 后全变 2，写一次就回 0），不是本机不支持。
    print("未写入   : %d" % unsup)
    print("未找到   : %d" % notfound)
    print("需特殊参数: %d" % broken)
    print("-" * 68)
    print("可写参数 : %d 项（渲染端点 %d + 采集端点 %d）"
          % (writable_count(), len(WRITABLE[EP_RENDER]),
             len(WRITABLE[EP_CAPTURE])))
    if drv.cap_ctx is not None:
        cok = 0
        for name in ALL_NAMES:
            if drv.read(name, EP_CAPTURE)[0] == 0:
                cok += 1
        print("麦克风端点: 已建立（可读 %d 项）" % cok)
    else:
        print("麦克风端点: 未建立（该机型可能只有渲染端点）")
    pf = drv.profiles()
    print("特效组态 : %d 个（%s）"
          % (len(pf["list"]),
             "、".join(p["name"] for p in pf["list"]) or "无"))
    print("组态目录 : %s%s" % (pf["dir"], "" if pf["writable"] else "（不可写）"))
    # 声道测试：只查资源在不在，不发声（自检不该突然响）。
    # 真正要听声音走 `--channel-test <声道>`。
    have = [c for c in TEST_CHANNELS
            if drv.test_wav(c[1]) and os.path.isfile(drv.test_wav(c[1]))]
    print("声道测试 : %d/%d 个测试音资源就位（%s）"
          % (len(have), len(TEST_CHANNELS),
             os.path.join(drv.cpl or "", "sound")))
    if len(have) != len(TEST_CHANNELS):
        miss = [c[2] for c in TEST_CHANNELS if c not in have]
        print("           缺：%s" % "、".join(miss))
    print("=" * 68)
    return 0


def _channel_test(drv, target):
    """`--channel-test` 的落点：在某个（或全部）声道上播测试音。

    target 可以是掩码（十进制 / 0x 十六进制），也可以是声道名
    （LF / RF / C / W / LB / RB / LS / RS）或中文名（左前…）。
    """
    if drv.lib is None:
        print("驱动未连接：%s" % drv.error)
        return 1
    if target.strip().lower() in ("all", "全部", "所有"):
        picks = [c[1] for c in TEST_CHANNELS]
    else:
        picks = []
        t = target.strip()
        for tag, mask, cn, _wav in TEST_CHANNELS:
            if t.upper() == tag or t == cn or t == str(mask):
                picks.append(mask)
        if not picks:
            try:
                picks = [int(t, 0)]
            except ValueError:
                print("认不出的声道：%s" % target)
                print("可用：%s"
                      % " / ".join("%s=%d(%s)" % (c[0], c[1], c[2])
                                   for c in TEST_CHANNELS))
                return 1
    for mask in picks:
        wav = drv.test_wav(mask)
        cn = dict((c[1], c[2]) for c in TEST_CHANNELS).get(mask, "?")
        res = drv.play_test(mask)
        print("  mask=%-5d %-6s %-24s -> %s"
              % (mask, cn, wav or "(无资源)", res))
        time.sleep(0.4)
    drv.stop_test()
    print("已停止。")
    return 0


# 启动重试时要跳过的无界面子命令：它们失败本身就是观测结果，
# 多等 7 秒只会拖慢闸脚本与排障（与 Go 版 openAttempts 逐项一致）。
_OPEN_ONCE_ARGS = ("--self-test", "--selftest", "--roundtrip", "--dump",
                   "--heal", "--channel-test", "--play",
                   "--loopback-devices", "--loopback-fft",
                   "--loopback-test", "--restore-scene",
                   # ★ 2026-10-22：--init-check / --init-fp 都是核对类命令，
                   #   失败本身就是它们要报的结果；Go 线早就在表里，这边漏了
                   #   ⟹ 驱动没装时白等 1+2+4=7 秒才报「未就绪」。
                   "--init-check", "--init-fp",
                   # ★ 2026-10-22：--no-browser 是无界面模式，必须单次。
                   #   以前漏了它，服务启动要白等 1+2+4=7 秒，闸脚本按
                   #   sleep 5 起就撞「页面没就绪」。
                   "--no-browser")


def open_with_retry(drv, argv):
    """启动时按 1s / 2s / 4s 退避重连（与 Go 版 openWithRetry 行为一致）。

    ★ 2026-10-21（用户拍板）：Open 以前只试一次。iKF-驱动管理装完驱动后
    会 `pnputil /restart-device` 重建音频端点，用户点完「已安装完成」就
    立刻打开主程序，很容易撞在重建窗口期里 —— 一次失败界面就永久停在
    「未连接驱动」，以前只能彻底关掉重开。GUI 重试，CLI 单次。
    """
    attempts = 1 if any(a in argv for a in _OPEN_ONCE_ARGS) else 4
    for i in range(attempts):
        if i:
            wait = 1 << (i - 1)
            print("[启动] 驱动未就绪（%s），%ds 后重试 %d/%d"
                  % (drv.error, wait, i, attempts - 1), flush=True)
            time.sleep(wait)
        if drv.open():
            if i:
                print("[启动] 第 %d 次重连成功 —— 端点重建窗口期已过" % i,
                      flush=True)
            return True
    return False


def _dump(drv, ep=EP_RENDER):
    if drv.lib is None:
        print("驱动未连接：%s" % drv.error)
        return 1
    if drv.ctx_of(ep) is None:
        print("端点不可用：%s" % ep)
        return 1
    print("# 端点：%s" % ep)
    print("%-34s %-10s %-6s %s" % ("属性名", "返回码", "表", "值"))
    print("-" * 78)
    for name in ALL_NAMES:
        r, v = drv.read(name, ep)
        print("%-34s %-10s %-6s %s"
              % (name, r, "通用" if name in GENERIC_SET else "OEM", v))
    return 0


def main(argv):
    if "--help" in argv or "-h" in argv:
        print(__doc__)
        return 0

    drv = Driver()
    open_with_retry(drv, argv)

    if "--self-test" in argv:
        return _self_test(drv)
    if "--channel-test" in argv:
        i = argv.index("--channel-test")
        if i + 1 >= len(argv):
            print("--channel-test 需要一个声道：LF/RF/C/W/LB/RB/LS/RS，"
                  "或 all")
            return 1
        return _channel_test(drv, argv[i + 1])
    if "--dump" in argv:
        i = argv.index("--dump")
        ep = EP_RENDER
        if i + 1 < len(argv) and argv[i + 1] in ENDPOINTS:
            ep = argv[i + 1]
        return _dump(drv, ep)

    # ---- 系统回环（可视化页音源）相关的无界面入口 ----
    if "--loopback-devices" in argv:
        return _loopback_devices()
    if "--loopback-fft" in argv:
        i = argv.index("--loopback-fft")
        return _loopback_fft(i + 1 < len(argv) and argv[i + 1] == "dump")
    if "--loopback-test" in argv:
        i = argv.index("--loopback-test")
        secs = 6.0
        if i + 1 < len(argv):
            try:
                v = float(argv[i + 1])
                if v > 0:
                    secs = v
            except ValueError:
                pass
        wav = None
        if "--wav" in argv:
            j = argv.index("--wav")
            if j + 1 < len(argv):
                wav = argv[j + 1]
        # --device：锁定某个播放端点（端点 ID 或名字子串）。
        # 不给就跟随系统默认播放设备 —— 但「默认」未必是耳机，
        # 本机默认是 FxSound Speakers，所以要验耳机那条路必须显式指定。
        dev_arg = ""
        if "--device" in argv:
            j = argv.index("--device")
            if j + 1 < len(argv):
                dev_arg = argv[j + 1]
        return _loopback_test(secs, drv, wav, "--tone" in argv, dev_arg)
    if "--play" in argv:
        i = argv.index("--play")
        if i + 1 >= len(argv):
            print("--play 需要给出 wav 路径")
            return 1
        secs = 10.0
        if "--secs" in argv:
            j = argv.index("--secs")
            if j + 1 < len(argv):
                try:
                    v = float(argv[j + 1])
                    if v > 0:
                        secs = v
                except ValueError:
                    pass
        return _play_only(argv[i + 1], secs, "--loop" in argv)

    # --restore-scene：手动触发一次「检测驱动重置指纹 → 自动回到默认场景」。
    # 加 --force 跳过指纹强制恢复（排障兜底）。启动路径也会自动跑（不带 force）。
    if "--restore-scene" in argv:
        if drv.lib is None:
            print("驱动未就绪，无法检测：%s" % drv.error)
            return 3
        _t, msg = restore_default_scene(drv, force="--force" in argv)
        print(msg)
        return 0

    # --heal：只跑一遍设备侧自愈，不进界面 —— 给「开关被灰掉、但不想
    # 连带改其他设置」的用户一个手动入口。核对效果看 --dump。
    if "--heal" in argv:
        if drv.lib is None:
            print("驱动未就绪，无法自愈：%s" % drv.error)
            return 3
        print("设备侧自愈：补写 %d 项（只补「存在但当前无值」的项，已有值不动）"
              % heal_missing(drv))
        return 0

    # --init-fp：只读查看「安装指纹」—— 端点 GUID / 采集端点 / 驱动版本 /
    # 设备实例，以及它相对上次记录有没有变。这是「初始化什么时候该全量写」
    # 的可观察面（见 init_fp_changed）：变了 ⟹ 下次初始化按清单全量写；
    # 没变 ⟹ 只补空缺。**只读，不写设备、不落盘。**
    if "--init-fp" in argv:
        if drv.lib is None:
            print("驱动未就绪，无法读取指纹：%s" % drv.error)
            return 3
        print(init_fp_report(drv))
        return 0

    # --init-check：把「初始化清单」逐项对一遍设备，只读不写。
    # 这是「初始化」从口头约定变成可验收的关键一步：以前判断「装完驱动是不是
    # 真回到默认」只能拿界面肉眼看，现在一条命令给出「应有 55 · 达标 a ·
    # 未设置 b · 偏离 c」。退出码 1 = 不达标，可直接当安装后的验收闸。
    if "--init-check" in argv:
        if drv.lib is None:
            print("驱动未就绪，无法核对：%s" % drv.error)
            return 3
        _r, _lines = check_init_manifest(drv)
        print("初始化清单核对（只读，不写设备）")
        print("-" * 64)
        for _ln in _lines:
            print(_ln)
        print("-" * 64)
        print("应有 %d 项（参数 %d + 设备级 %d） · 达标 %d · 未设置 %d · 偏离 %d · 不支持 %d"
              % (_r["total"], _r["total"] - len(INIT_DEVICE_ITEMS), len(INIT_DEVICE_ITEMS),
                 _r["ok"], _r["missing"], _r["drift"], _r["unsupported"]))
        if _r["missing"] + _r["drift"] > 0:
            print("结论：不达标（未设置/偏离 = %d 项）" % (_r["missing"] + _r["drift"]))
            return 1
        print("结论：达标 —— 设备已是初始化定义的默认状态")
        return 0

    # ★ 2026-10-22：整条链收进 ensure_initial_state —— 「重新检测」和装驱动
    #   重连也要跑同一套（以前只在这里跑，用户装完驱动点重新检测就错过了）。
    #   内部顺序：恢复默认（带指纹保护）→ 设备侧自愈 → 场景分组名升级。
    #   恢复默认必须在自愈**之前**：自愈补完默认值后指纹就测不出来了。
    ensure_initial_state(drv, full=True)

    port = _free_port()
    if "--port" in argv:
        try:
            port = int(argv[argv.index("--port") + 1])
        except (IndexError, ValueError):
            pass
    if not port:
        print("找不到可用端口。")
        return 1

    Handler.driver = drv
    Handler.html = HTML.encode("utf-8")

    httpd = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    url = "http://127.0.0.1:%d/" % port

    print("=" * 68)
    print(" %s %s" % (APP_NAME, APP_VER))
    print("=" * 68)
    if drv.lib is not None:
        st = drv.status()
        print(" 驱动     : %s" % drv.dll)
        print(" 设备     : %s  ·  %s" % (st["model"], st["codec"]))
        print(" 驱动版本 : %s   固件 : %s" % (st["driver"], st["firmware"]))
        print(" 音频格式 : %s" % st["formats"])
    else:
        print(" 驱动     : 未连接 —— %s" % drv.error)
        print(" 界面仍会打开，但以「未连接」状态运行。")
    print("-" * 68)
    print(" 地址     : %s" % url)
    print(" 停止     : 在本窗口按 Ctrl+C")
    print("=" * 68)

    if "--no-browser" not in argv:
        threading.Timer(0.6, lambda: webbrowser.open(url)).start()

    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\n正在关闭 …")
    finally:
        httpd.shutdown()
        LOOPBACK.stop()
        drv.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
