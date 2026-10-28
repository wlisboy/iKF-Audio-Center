#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""容错版 .cml 解析器（替代 parse_panel_cml.py）。

为什么需要它
------------
原版 XearAudioCenter 的 .cml 是**手工维护的**，存在标签不配对的情况，例如
`Skin/FaceLift/apoMicInNoiseReduction.cml` 末尾多出一个 `</Canvas>`。
用 `xml.etree.ElementTree` 严格解析会整份抛 ParseError，
调用方（旧脚本）只记一条 error，于是**整页控件凭空消失** ——
这正是「官方有的东西新面板漏掉」的隐藏来源。

本脚本改成基于标签扫描的容错解析：
按 `<Tag .../>` / `<Tag ...>...</Tag>` 逐个抽取元素，不依赖嵌套平衡，
因此单个畸形标签最多影响它自己，不会拖垮整页。

输出与旧脚本**同一套 schema**（lang_zh / lang_en / panels），
下游 panel_catalog.py、make_mapping_table.py 无需改动。

用法：
    python parse_panel_cml2.py <CPL目录> <输出目录>
"""
import glob
import io
import json
import os
import re
import sys

# 控件类型白名单之外的标签仍然记录，但这里只对「有交互意义」的做统计
INTERACTIVE = {"Slider", "RadioButton", "CheckBox", "ComboBox", "ComboBoxItem",
               "Button", "TextBox", "RepeatButton", "ToggleButton", "SkinMenuItem"}

# 一个「属性="值"」对
RE_ATTR = re.compile(r'([A-Za-z_][\w.\-]*)\s*=\s*"([^"]*)"')
# 元素：<Tag attrs>  或  <Tag attrs/>  或  </Tag>
RE_TAG = re.compile(r'<\s*(/?)\s*([A-Za-z_][\w.\-]*)((?:[^<>"\']|"[^"]*"|\'[^\']*\')*?)(/?)\s*>')


def load_lang(folder):
    """ml_key -> 文案。xaml 是 UTF-8 BOM，个别文件是 UTF-16。"""
    d = {}
    if not os.path.isdir(folder):
        return d
    for p in sorted(glob.glob(os.path.join(folder, "*.xaml"))):
        txt = None
        for enc in ("utf-8-sig", "utf-16", "latin-1"):
            try:
                txt = io.open(p, encoding=enc).read()
                break
            except (OSError, UnicodeError):
                continue
        if not txt:
            continue
        for m in re.finditer(r'x:Key="([^"]+)"\s*>\s*(.*?)\s*</sys:String>',
                             txt, re.S):
            d[m.group(1)] = m.group(2)
    return d


def resolve(v, lang):
    if not v:
        return v
    if v.startswith("ml_"):
        return lang.get(v, v)
    return v


def parse_cml(raw, zh, en):
    """容错解析。返回 (控件列表, 畸形标签计数)。"""
    ctrls = []
    malformed = 0
    depth = 0
    for m in RE_TAG.finditer(raw):
        closing, tag, attrs_s, self_close = \
            m.group(1), m.group(2), m.group(3), m.group(4)
        if closing:
            depth = max(0, depth - 1)
            continue
        depth += 1
        if self_close:
            depth = max(0, depth - 1)

        attrs = {}
        for am in RE_ATTR.finditer(attrs_s):
            attrs[am.group(1)] = am.group(2)

        text = attrs.get("Text", "")
        content = attrs.get("Content", "")
        item = {
            "type": tag,
            "name": attrs.get("Name", ""),
            "tag": attrs.get("Tag", ""),
            "text": resolve(text, zh),
            "text_en": resolve(text, en),
            "text_key": text if text.startswith("ml_") else "",
            "tooltip": resolve(attrs.get("ToolTip", attrs.get("Tooltip", "")), zh),
            "min": attrs.get("Minimum", ""),
            "max": attrs.get("Maximum", ""),
            "small": attrs.get("SmallChange", ""),
            "content": resolve(content, zh),
            "content_key": content if content.startswith("ml_") else "",
            "left": attrs.get("Left", ""),
            "top": attrs.get("Top", ""),
            "vis": attrs.get("Visibility", ""),
            "depth": depth,
        }
        if any(item[k] for k in ("name", "text", "content", "tooltip")):
            ctrls.append(item)
    return ctrls, malformed


def main(argv):
    cpl = argv[1] if len(argv) > 1 else \
        r"C:\Program Files\iKF headset\CPL"
    out_dir = argv[2] if len(argv) > 2 else \
        os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                     "tools")
    skin = os.path.join(cpl, "Skin", "FaceLift")
    zh = load_lang(os.path.join(cpl, "lang", "zh-CN"))
    en = load_lang(os.path.join(cpl, "lang", "en-US"))
    print("zh-CN 文案 %d 条 / en-US %d 条" % (len(zh), len(en)))

    panels = {}
    for p in sorted(glob.glob(os.path.join(skin, "**", "*.cml"), recursive=True)):
        rel = os.path.relpath(p, skin).replace("\\", "/")
        if rel.startswith("Dictionary") or rel.startswith("Design"):
            continue
        raw = io.open(p, encoding="utf-8-sig", errors="replace").read()
        ctrls, _ = parse_cml(raw, zh, en)
        panels[rel] = {"controls": ctrls}

    os.makedirs(out_dir, exist_ok=True)
    dst = os.path.join(out_dir, "原版面板控件目录.json")
    with io.open(dst, "w", encoding="utf-8", newline="\n") as f:
        json.dump({"lang_zh": zh, "lang_en": en, "panels": panels}, f,
                  ensure_ascii=False, indent=1)

    tot = sum(len(v["controls"]) for v in panels.values())
    inter = sum(1 for v in panels.values() for c in v["controls"]
                if c["type"] in INTERACTIVE)
    empty = [k for k, v in panels.items() if not v["controls"]]
    print("已写出：%s" % dst)
    print("  面板 %d 个，元素 %d 个，其中交互控件 %d 个"
          % (len(panels), tot, inter))
    print("  零控件页 %d 个：%s" % (len(empty), "、".join(empty) or "无"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
