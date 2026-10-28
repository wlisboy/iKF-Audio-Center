# -*- coding: utf-8 -*-
"""
读取 iKF / C-Media CM108B 系列耳机的设备型号
============================================

背景：驱动包里没有型号。108B.Inf 的 [Strings] 只有 C-Media 的通用串
      108B.DeviceDesc = "USB Audio Class 1.0 and 2.0 Device"
      CMEDIA          = "C-MEDIA Inc."
      皮肤和语言包里也只有 "iKF Headset Audio"（窗口标题）。
      所以型号只能来自【设备固件上报的 USB 字符串描述符】。

本脚本走三条通道，任一成功即可，互为交叉验证：
  通道 1  SetupAPI      —— 最通用，等价于设备管理器「属性 → 详细信息」
  通道 2  Enum\\USB      —— 注册表缓存的 USB iProduct 字符串
  通道 3  MMDevices     —— 音频端点属性，等价于「声音」面板里的显示名

用法：
    python 读取设备型号.py            # 自动探测
    python 读取设备型号.py --json     # 输出 JSON，便于被界面调用
"""

import ctypes
import ctypes.wintypes as wt
import json
import sys

try:
    import winreg
except ImportError:  # 非 Windows
    winreg = None

# C-Media 的 USB 厂商 ID；iKF 全系用的是同一颗 CM108B
CMEDIA_VID = "0D8C"
FALLBACK_PID = "0012"          # CM108B 的默认 PID，仅作兜底匹配

SPDRP_DEVICEDESC = 0x00
SPDRP_HARDWAREID = 0x01
SPDRP_FRIENDLYNAME = 0x0C

DIGCF_PRESENT = 0x02
DIGCF_ALLCLASSES = 0x04

# 这些不是型号，是 Windows 或 INF 的通用占位串，命中就丢掉
JUNK = {
    "usb audio device", "usb audio class 1.0 and 2.0 device",
    "usb composite device", "usb input device", "speakers", "扬声器",
    "headphones", "耳机", "usb audio class 1.0 and 2.0 device audio",
    "通用 usb 音频设备",
}


class SP_DEVINFO_DATA(ctypes.Structure):
    _fields_ = [
        ("cbSize", wt.DWORD),
        ("ClassGuid", ctypes.c_byte * 16),
        ("DevInst", wt.DWORD),
        ("Reserved", ctypes.POINTER(ctypes.c_ulong)),
    ]


def _is_model(s):
    """判断一个字符串像不像真实型号。"""
    if not s or not s.strip():
        return False
    s = s.strip()
    if s.lower() in JUNK:
        return False
    if s.startswith("@") or "%" in s:      # 未解析的 INF 引用
        return False
    if s.lower().startswith("usb "):       # 通用描述
        return False
    return True


# ---------------------------------------------------------------- 通道 1
def via_setupapi():
    """用 SetupAPI 枚举 USB 设备，读 FriendlyName。"""
    try:
        setupapi = ctypes.WinDLL("setupapi", use_last_error=True)
    except OSError:
        return None

    setupapi.SetupDiGetClassDevsW.restype = ctypes.c_void_p
    setupapi.SetupDiGetClassDevsW.argtypes = [
        ctypes.c_void_p, ctypes.c_wchar_p, wt.HWND, wt.DWORD]
    setupapi.SetupDiEnumDeviceInfo.argtypes = [
        ctypes.c_void_p, wt.DWORD, ctypes.POINTER(SP_DEVINFO_DATA)]
    setupapi.SetupDiGetDeviceRegistryPropertyW.argtypes = [
        ctypes.c_void_p, ctypes.POINTER(SP_DEVINFO_DATA), wt.DWORD,
        ctypes.POINTER(wt.DWORD), ctypes.c_void_p, wt.DWORD, ctypes.POINTER(wt.DWORD)]

    h = setupapi.SetupDiGetClassDevsW(
        None, "USB", None, DIGCF_PRESENT | DIGCF_ALLCLASSES)
    if not h or h == -1:
        return None

    def prop(dev, code):
        buf = ctypes.create_unicode_buffer(1024)
        need = wt.DWORD()
        ok = setupapi.SetupDiGetDeviceRegistryPropertyW(
            ctypes.c_void_p(h), ctypes.byref(dev), code, None,
            buf, ctypes.sizeof(buf), ctypes.byref(need))
        if not ok:
            return None
        # 多字符串：REG_MULTI_SZ 用 \0 分隔，取第一段
        return buf.value.split("\x00")[0].strip() or None

    found = []
    i = 0
    while True:
        dev = SP_DEVINFO_DATA()
        dev.cbSize = ctypes.sizeof(SP_DEVINFO_DATA)
        if not setupapi.SetupDiEnumDeviceInfo(ctypes.c_void_p(h), i, ctypes.byref(dev)):
            break
        i += 1

        hwid = prop(dev, SPDRP_HARDWAREID) or ""
        if CMEDIA_VID.lower() not in hwid.lower():
            continue
        fri = prop(dev, SPDRP_FRIENDLYNAME)
        desc = prop(dev, SPDRP_DEVICEDESC)
        found.append({"hardwareId": hwid, "friendlyName": fri, "deviceDesc": desc})

    setupapi.SetupDiDestroyDeviceInfoList(ctypes.c_void_p(h))
    for d in found:
        if _is_model(d["friendlyName"]):
            return d
    return found[0] if found else None


# ---------------------------------------------------------------- 通道 2
def via_enum_usb():
    """读 HKLM\\SYSTEM\\CurrentControlSet\\Enum\\USB 下缓存的 iProduct。"""
    if not winreg:
        return None
    base = r"SYSTEM\CurrentControlSet\Enum\USB"
    try:
        k = winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE, base)
    except OSError:
        return None

    out = None
    n = 0
    while True:
        try:
            vid = winreg.EnumKey(k, n)
        except OSError:
            break
        n += 1
        if CMEDIA_VID.lower() not in vid.lower():
            continue
        try:
            vk = winreg.OpenKey(k, vid)
        except OSError:
            continue
        m = 0
        while True:
            try:
                inst = winreg.EnumKey(vk, m)
            except OSError:
                break
            m += 1
            path = vid + "\\" + inst
            try:
                ik = winreg.OpenKey(k, path)
                fri, _ = winreg.QueryValueEx(ik, "FriendlyName")
                winreg.CloseKey(ik)
            except OSError:
                fri = None
            if _is_model(fri):
                out = {"hardwareId": "USB\\" + path, "friendlyName": fri}
                break
        winreg.CloseKey(vk)
        if out:
            break
    winreg.CloseKey(k)
    return out


# ---------------------------------------------------------------- 通道 3
def via_mmdevices():
    """读音频端点属性，拿「声音」面板里显示的那个名字。"""
    if not winreg:
        return None
    PKEY_DEV_DESC = "{a45c254e-df1c-4efd-8020-67d146a850e0},2"
    PKEY_IF_NAME = "{b3f8fa53-0004-438e-9003-51a46e139bfc},6"
    base = (r"SOFTWARE\Microsoft\Windows\CurrentVersion"
            r"\MMDevices\Audio\Render")
    try:
        k = winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE, base)
    except OSError:
        return None

    out = None
    i = 0
    while True:
        try:
            guid = winreg.EnumKey(k, i)
        except OSError:
            break
        i += 1
        try:
            pk = winreg.OpenKey(k, guid + r"\Properties")
        except OSError:
            continue
        props = {}
        j = 0
        while True:
            try:
                name, val, _ = winreg.EnumValue(pk, j)
            except OSError:
                break
            j += 1
            props[name.lower()] = val
        winreg.CloseKey(pk)

        iface = props.get(PKEY_IF_NAME.lower())
        if not _is_model(iface):
            continue
        out = {
            "guid": guid,
            "interfaceName": iface,
            "deviceDesc": props.get(PKEY_DEV_DESC.lower()),
            # 「声音」面板里的完整显示名
            "displayName": "%s (%s)" % (props.get(PKEY_DEV_DESC.lower()), iface),
        }
        break
    winreg.CloseKey(k)
    return out


# ---------------------------------------------------------------- 主流程
def detect():
    r = {"connected": False, "model": None, "source": None, "channels": {}}

    a = via_setupapi()
    r["channels"]["setupapi"] = a
    b = via_enum_usb()
    r["channels"]["enumUsb"] = b
    c = via_mmdevices()
    r["channels"]["mmdevices"] = c

    if a and _is_model(a.get("friendlyName")):
        r["model"], r["source"] = a["friendlyName"], "SetupAPI"
    elif b:
        r["model"], r["source"] = b["friendlyName"], "Enum\\USB"
    elif c:
        r["model"], r["source"] = c["interfaceName"], "MMDevices"

    r["connected"] = r["model"] is not None
    if c:
        r["displayName"] = c["displayName"]
    if a:
        r["hardwareId"] = a["hardwareId"]
    return r


def main():
    info = detect()
    if "--json" in sys.argv:
        print(json.dumps(info, ensure_ascii=False, indent=2))
        return

    print("=" * 62)
    if info["connected"]:
        print("  设备已连接")
        print("  型号      : %s" % info["model"])
        print("  来源通道  : %s" % info["source"])
        if info.get("displayName"):
            print("  系统显示名: %s" % info["displayName"])
        if info.get("hardwareId"):
            print("  硬件 ID   : %s" % info["hardwareId"])
    else:
        print("  未检测到 iKF / C-Media 音频设备（耳机没插？）")
    print("=" * 62)

    print("\n[通道 1] SetupAPI")
    print("  %s" % (info["channels"]["setupapi"] or "无结果"))
    print("\n[通道 2] Enum\\USB 注册表")
    print("  %s" % (info["channels"]["enumUsb"] or "无结果"))
    print("\n[通道 3] MMDevices 音频端点")
    print("  %s" % (info["channels"]["mmdevices"] or "无结果"))


if __name__ == "__main__":
    main()
