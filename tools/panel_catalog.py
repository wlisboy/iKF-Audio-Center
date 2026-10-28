# -*- coding: utf-8 -*-
"""
生成官方面板控件的紧凑目录，供离线验收基线使用。

来源：tools/原版面板控件目录.json（由 parse_panel_cml.py 从
      CPL/Skin/FaceLift/*.cml + CPL/lang/zh-CN/*.xaml 解析而来）

输出：tools/official_catalog.json
      结构 {"pages":[{"file":…, "title":…, "controls":[[type,name,label,min,max,tag],…]}]}

设计取舍
--------
.cml 里的 Tag 存的是**值**（单选按钮序号、声道掩码等），不是属性名；
控件与属性的绑定在 XearAudioCenter_x64.exe 里（由 Ghidra PanelMap.java 提取）。
所以这里只导出「官方长什么样」——页面、控件、官方中文标签、滑杆值域，
用途是给 tools/make_mapping_table.py 生成「原面板功能映射表.md」（零遗漏验收基线），
证明移植没有遗漏，而不是自动接线。

**2026-10-14：本文件已与界面解耦。** 新面板不再内联这份目录，
界面里也没有「官方面板页对照」页（用户要求复刻品独立、不露出比对功能）。
下方「构建期注入」一节保留仅为将来可能的复用，当前无调用方。

只保留有交互意义的控件类型；纯装饰（Image / Canvas / 无文字的 TextBlock）丢掉。
"""

import io
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(ROOT, "tools", "原版面板控件目录.json")
OUT = os.path.join(ROOT, "tools", "official_catalog.json")

# 有交互意义的控件类型
KEEP = {"Slider", "RadioButton", "CheckBox", "ComboBox", "ComboBoxItem",
        "Button", "TextBox", "RepeatButton", "ToggleButton", "SkinMenuItem"}

# 页面分类。外壳类也**照收不误** —— 目标是「官方有什么就列什么」，
# 由使用者在页面上自己判断哪些是功能、哪些是壳子，工具不替他筛掉。
SHELL_PAGES = {
    "DialogMessageBox.cml", "DialogInfo.cml", "DialogInfo_DetailWindow.cml",
    "DialogInfo_Legal.cml", "Dialog_WelcomeMsg.cml", "SystemTrayLClick.cml",
    "SystemTrayRClick.cml", "DesignDictResource.cml",
}
MENU_PREFIX = "Menu"
PROFILE_PAGES = {"PanelProfile.cml", "Dialog_AddProFile.cml", "Dialog_DelProFile.cml"}


def page_kind(pn):
    """功能页 / 组态页 / 设备菜单 / 对话框 / 外壳"""
    if pn in SHELL_PAGES:
        return "外壳"
    if pn in PROFILE_PAGES:
        return "组态"
    if pn.startswith(MENU_PREFIX):
        return "设备菜单"
    if pn.startswith("Dialog_"):
        return "对话框"
    return "功能页"


# 页面标题：优先用页面里第一个带文字标题控件
TITLE_HINTS = (
    "ml_SpeakerEQ_Title", "ml_MicInVol_Title", "ml_Title",
)


def main():
    d = json.load(open(SRC, encoding="utf-8"))
    pages = []
    total = 0

    for pn in sorted(d["panels"].keys()):
        cs = d["panels"][pn].get("controls")
        if not cs:
            continue
        rows = []
        title = ""
        title_alt = ""
        for c in cs:
            ty = c.get("type", "")
            tx = (c.get("text") or "").strip()
            tk = (c.get("text_key") or "").strip().lower()
            # 页面标题：优先取 text_key 以 _title 结尾的文本块（官方命名约定），
            # 否则退回第一个有文字的文本块，最后才用文件名。
            if ty == "TextBlock" and tx and tx != "NULL":
                if not title and tk.endswith("_title"):
                    title = tx
                if not title_alt:
                    title_alt = tx
            if ty not in KEEP:
                continue
            nm = (c.get("name") or "").strip()
            # 没有名字也没有文字的，纯装饰
            if not nm and not tx:
                continue
            mn = (c.get("min") or "").strip()
            mx = (c.get("max") or "").strip()
            tg = (c.get("tag") or "").strip()
            rows.append([ty, nm, tx, mn, mx, tg])
        if not rows:
            continue
        if not title:
            title = title_alt or pn.replace(".cml", "")
        total += len(rows)
        pages.append({"file": pn, "title": title, "kind": page_kind(pn),
                      "controls": rows})

    kinds = {}
    for p in pages:
        kinds[p["kind"]] = kinds.get(p["kind"], 0) + 1

    out = {
        "pages": pages,
        "pageCount": len(pages),
        "controlCount": total,
        "kinds": kinds,
        "note": "Tag 列存的是值（序号/掩码），属性绑定在面板 exe 内",
    }
    with io.open(OUT, "w", encoding="utf-8", newline="\n") as f:
        f.write(json.dumps(out, ensure_ascii=False, separators=(",", ":")))
    print("已生成：%s" % OUT)
    print("  页面 %d 个，交互控件 %d 个，体积 %.1f KB"
          % (len(pages), total, os.path.getsize(OUT) / 1024.0))
    print("  分类：%s" % "、".join("%s %d" % (k, v) for k, v in sorted(kinds.items())))
    return 0


# ====================================================================
# 构建期注入（2026-10-14 起已停用，当前无调用方）
# ====================================================================
# 曾用于把目录内联成 <script type="application/json" id="__official">，
# 供界面的「官方面板页对照」页读取。该页已按用户要求整体移除，
# build_center.py / build_frontend.py 也不再调用 inject()。
# 以下代码保留仅为将来可能的复用。

MARK = 'id="__official"'


def inline_script():
    """生成内联的官方目录 script 片段；目录缺失时返回空串。"""
    if not os.path.isfile(OUT):
        return ""
    txt = io.open(OUT, encoding="utf-8").read()
    # 防止内容里出现 </ 提前闭合 script
    txt = txt.replace("</", "<\\/")
    return '<script type="application/json" id="__official">' + txt + '</script>'


def inject(html):
    """把官方目录注入 HTML（放在 </body> 之前）。

    返回 (新 html, 是否注入)。已经注入过或目录缺失时原样返回。
    注意顺序：必须在桥接层 script 之前注入，桥接层是立即执行的 IIFE，
    运行时就要能读到这个 JSON。
    """
    if MARK in html:
        return html, False
    frag = inline_script()
    if not frag:
        return html, False
    if "</body>" in html:
        return html.replace("</body>", frag + "\n</body>", 1), True
    return html + frag, True


if __name__ == "__main__":
    raise SystemExit(main())
