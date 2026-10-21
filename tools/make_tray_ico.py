# -*- coding: utf-8 -*-
"""生成托盘专用图标 tray.ico（第 65 轮）。

背景：build/windows/icon.ico 是黑底白字的 iKF 字标 —— 16px 时是一块
看不清的黑疙瘩，托盘上「显示不正常」。

本脚本从 design/brand-logo-mask.png（RGBA 字标蒙版）裁出最左侧的
Ⓚ 徽记，做成「白圆盘 + 黑 K」徽章：16px 下可辨识，深浅任务栏都显眼。

输出：iKF-Audio-Center-Wails/build/windows/tray.ico（16/24/32/48 四帧）。
托盘代码 tray.go 嵌入的是 tray.ico（与 exe 资源图标 icon.ico 分开，
窗口标题栏/任务栏大图标仍用官方字标）。

用法：python tools/make_tray_ico.py   （需 Pillow：托管 venv 已装）
"""

import os
import sys

from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(ROOT, "design", "brand-logo-mask.png")
OUT = os.path.join(ROOT, "iKF-Audio-Center-Wails", "build", "windows", "tray.ico")

SIZES = [16, 24, 32, 48]


def main():
    im = Image.open(SRC).convert("RGBA")
    a = im.split()[3]
    bbox = a.getbbox()
    print("蒙版尺寸 %s，字标 bbox=%s" % (im.size, bbox))

    # 找字标的「列簇」：alpha 列和 > 0 的连续段。第一段 = Ⓚ 徽记。
    w, h = im.size
    px = a.load()
    col_has = [any(px[x, y] > 8 for y in range(h)) for x in range(w)]
    segs, start = [], None
    for x in range(w):
        if col_has[x] and start is None:
            start = x
        elif not col_has[x] and start is not None:
            segs.append((start, x - 1))
            start = None
    if start is not None:
        segs.append((start, w - 1))
    print("列簇:", segs)
    if not segs:
        sys.exit("蒙版里没有内容")
    x0, x1 = segs[0]

    # Ⓚ 徽记的正方形 bbox（取列簇 × 全高内容行）
    y0, y1 = a.getbbox()[:2][1], a.getbbox()[3]
    rows = [y for y in range(h) if any(px[x, y] > 8 for x in range(x0, x1 + 1))]
    y0, y1 = min(rows), max(rows)
    glyph = im.crop((x0, y0, x1 + 1, y1 + 1))
    print("Ⓚ 徽记 bbox: (%d,%d)-(%d,%d) 尺寸 %s" % (x0, y0, x1, y1, glyph.size))

    # 徽章：白圆盘 + 黑 K。盘径 = 徽记对角线 × 余量，K 居中占盘径 ~62%。
    S = 256  # 母版尺寸，再缩到各帧
    disc_r = int(S * 0.5 * 0.98)
    canvas = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(canvas)
    cx = cy = S // 2
    d.ellipse((cx - disc_r, cy - disc_r, cx + disc_r, cy + disc_r),
              fill=(255, 255, 255, 255))
    # 字标是白色（亮度≈255），用它的 alpha 做形状 → 涂黑
    glyph_a = glyph.split()[3]
    k_size = int(disc_r * 2 * 0.60)
    k = glyph_a.resize((k_size, k_size), Image.LANCZOS)
    black = Image.new("RGBA", k.size, (17, 17, 20, 255))
    canvas.paste(black, (cx - k_size // 2, cy - k_size // 2), k)

    canvas.save(OUT, format="ICO", sizes=[(s, s) for s in SIZES])
    print("已生成 %s（帧 %s，%d 字节）" % (OUT, SIZES, os.path.getsize(OUT)))


if __name__ == "__main__":
    main()
