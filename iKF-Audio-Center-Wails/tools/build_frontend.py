# -*- coding: utf-8 -*-
"""
构建 Wails 版前端资源
=====================

把设计稿与驱动桥接层合成一个自包含的 index.html，
供 main.go 的 `//go:embed all:frontend/dist` 内联进 exe。

输入：
  design/iKF-Audio-Center-1610.html   已完成的设计稿（含内联产品图与 Logo 遮罩）
  tools/build_center.py                      其中的 BRIDGE_JS / FAVICON（与 Python 版共用同一份，不复制）

输出：
  iKF-Audio-Center-Wails/frontend/dist/index.html

设计稿本身**一行都不改** —— Wails 的 AssetServer 直接接受任意
http.Handler，/api/* 契约与 Python 版完全一致，桥接层无需任何适配。

用法：
    python iKF-Audio-Center-Wails/tools/build_frontend.py
"""

import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
APP = os.path.dirname(HERE)                       # iKF-Audio-Center-Wails/
ROOT = os.path.dirname(APP)                       # 工作区根

BUILD_CENTER = os.path.join(ROOT, "tools", "build_center.py")
DESIGN = os.path.join(ROOT, "design", "iKF-Audio-Center-1610.html")
OUT = os.path.join(APP, "frontend", "dist", "index.html")

MARK = "/* ============================================================\n   驱动桥接层"


def load_bridge():
    """从 build_center.py 载入 BRIDGE_JS / FAVICON。

    直接 import 而不是复制一份，是为了保证 Python 版与 Wails 版
    永远共用同一份桥接逻辑 —— 改一处两边同时生效。
    """
    if not os.path.isfile(BUILD_CENTER):
        print("找不到 %s" % BUILD_CENTER)
        sys.exit(1)
    spec = importlib.util.spec_from_file_location("build_center", BUILD_CENTER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.BRIDGE_JS, mod.FAVICON


def main():
    if not os.path.isfile(DESIGN):
        print("找不到设计稿：%s" % DESIGN)
        return 1

    bridge_js, favicon = load_bridge()
    html = open(DESIGN, encoding="utf-8").read()
    before = len(html)

    # ---- 1. 注入桥接层 ----
    if "</body>" not in html:
        print("设计稿里没有 </body>，无法注入。")
        return 1
    if MARK in html:
        print("设计稿里已经注入过桥接层，跳过注入。")
    else:
        html = html.replace("</body>", "<script>" + bridge_js + "</script>\n</body>", 1)

    # ---- 2. 注入内联 favicon（消除 /favicon.ico 请求） ----
    if 'rel="icon"' not in html and "</title>" in html:
        html = html.replace("</title>", "</title>\n" + favicon, 1)

    # ---- 3. 自检：桥接层只注入了一次，且关键入口都在 ----
    assert html.count("驱动桥接层") == 1, "桥接层注入次数异常"
    for probe in ("/api/status", "/api/params", "/api/meter", "/api/set",
                  "/api/test", "/api/listen"):
        assert probe in html, "桥接层缺少契约入口：%s" % probe

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8", newline="\n") as f:
        f.write(html)

    print("已生成：%s" % OUT)
    print("  设计稿  : %.1f KB" % (before / 1024.0))
    print("  桥接层  : %.1f KB" % (len(bridge_js) / 1024.0))
    print("  产物    : %.1f KB" % (len(html) / 1024.0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
