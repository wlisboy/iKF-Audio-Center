# -*- coding: utf-8 -*-
"""
一键构建 iKF Audio Center（Wails 2 版）
=======================================

两步：
  1. 生成前端资源  frontend/dist/index.html
     （设计稿 + 驱动桥接层，由 build_frontend.py 完成）
  2. 用 Wails CLI 编译单文件 exe
     （wails build 会自动带上 `-tags production`，并把图标 / 版本资源 /
      manifest 编进产物 —— 直接 go build 是拿不到这些的）

用法：
    python iKF-Audio-Center-Wails/tools/build.py

前置：
    Go ≥ 1.25、Wails CLI v2、WebView2 Runtime。
    离线可构建（Wails 模块已在本地 module cache，构建时设 GOPROXY=off）。

产物：
    iKF-Audio-Center-Wails/build/bin/iKF-Audio-Center.exe
"""

import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
APP = os.path.dirname(HERE)
ROOT = os.path.dirname(APP)
OUT = os.path.join(APP, "build", "bin", "iKF-Audio-Center.exe")

GO_BIN = r"C:\Program Files\Go\bin"
WAILS_BIN = os.path.join(os.path.expanduser("~"), "go", "bin")


def run(cmd, cwd, env):
    print("+ %s" % " ".join(cmd))
    p = subprocess.run(cmd, cwd=cwd, env=env)
    return p.returncode


def main():
    env = dict(os.environ)
    env["PATH"] = WAILS_BIN + os.pathsep + GO_BIN + os.pathsep + env.get("PATH", "")
    # 完全离线构建：依赖都在本地 module cache 里
    env["GOPROXY"] = "off"
    env["GOSUMDB"] = "off"

    print("=" * 68)
    print(" 1/2  生成前端资源")
    print("=" * 68)
    if run([sys.executable, os.path.join(HERE, "build_frontend.py")], ROOT, env):
        print("前端资源生成失败")
        return 1

    print()
    print("=" * 68)
    print(" 2/2  编译（wails build）")
    print("=" * 68)
    # -webview2 显式声明部署策略，不吃 Wails 默认值（默认恰好也是 download，
    # 但写出来才能防止将来 Wails 漂移；离线整包分发时改 embed）。
    # -ldflags 不在这里传 -s -w：wails build 自己会加一遍（旧写法产物里
    # 是 "-s -w -w -s"，无害但重复）。
    if run(["wails", "build", "-s", "-skipbindings", "-m", "-nosyncgomod",
            "-nocolour", "-webview2", "download"], APP, env):
        print("编译失败")
        return 1

    if os.path.isfile(OUT):
        print()
        print("产物：%s" % OUT)
        print("体积：%.2f MB" % (os.path.getsize(OUT) / 1024.0 / 1024.0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
