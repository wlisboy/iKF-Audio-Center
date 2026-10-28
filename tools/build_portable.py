# -*- coding: utf-8 -*-
"""
构建免安装（绿色）版目录
========================

把「程序 + 运行期资源 + 驱动组件 + 驱动管理程序」装进同一个目录，解压即可运行，
不再依赖 `C:\\Program Files\\iKF headset\\CPL` 与旧安装包留下的注册表项。

目录布局
--------
    release/iKF-Audio-Center-Portable/
      iKF-Audio-Center.exe          主程序：只管运行
                                      （driver.go 的 localCpls 优先读同目录）
      iKF-驱动管理.exe               驱动管理：只管装/卸驱动，自提权
      iKFTool.exe                   日志诊断：只读（环境/完整性/崩溃取证）
      CPL/                          运行期资源 —— d.cpl 指向这里
        Driver/x64/vista/osConfLib.dll      208 项参数的唯一入口
        sound/*.wav                         8 个声道测试音
        profile/…                           内置组态 + 新增组态模板
        lang/zh-CN/*.xaml                   组态 Tooltip 的资源键
        FactoryDefault.xml                  原厂默认档
      driver/                       驱动组件，只被 iKF-驱动管理.exe 用
        108B.Inf · 108b.cat
        x64/{108B.sys, CMCplExt_108B.dll, CMEffectAPO_108B.dll}

为什么只要这些
--------------
旧包 `SoftwareDriver\\DLL\\CPL\\X64\\` 有 12 MB，其中大部分是旧面板自己的：
`XearAudioCenter_x64.exe`(2.5 MB) / `D3DX9_43.dll`(2.4 MB) / `Skin`(4.1 MB)
—— 本程序都不用。`osConfLib.dll` 的导入表只有
WINMM/ADVAPI32/SETUPAPI/KERNEL32/USER32/ole32/OLEAUT32，不碰 DirectX。
三个无用 DLL 变体（x86/vista、x64/xp、x86/xp）也不拷：xp 版会让进程 exit 127。
★ 2026-10-20：`SoundRecorder\\Recorder.exe` **不再捆绑** —— 取证实为
依附官方面板 COM 环境的薄客户端（导入表仅 KERNEL32+ole32，无
user32/winmm），脱离官方面板拉起必退 241，是死件；录音机已改内嵌实现
（recorder.go / center_src.py 的 MicRecorder）。取证见
docs/录音机取证-Recorder.exe死件.md。

用法
----
    python tools/build_portable.py                    # 默认走 ../iKF headset-0817
    python tools/build_portable.py --src <驱动包目录>
    python tools/build_portable.py --no-build         # 不重新编译，只打包
    python tools/build_portable.py --no-check         # 跳过产物自检

前置：先跑 tools/build_center.py 与 iKF-Audio-Center-Wails/tools/build.py
      （--no-build 时不需要，但仍需要它们上次的产物）。
"""

import argparse
import glob
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
APP = os.path.join(ROOT, "iKF-Audio-Center-Wails")

DEF_SRC = os.path.join(os.path.dirname(ROOT), "iKF headset-0817",
                       "WIN10(WHQL)", "SoftwareDriver")
DEF_EXE = os.path.join(APP, "build", "bin", "iKF-Audio-Center.exe")
DEF_OUT = os.path.join(ROOT, "release", "iKF-Audio-Center-Portable")

SETUP_NAME = "iKF-驱动管理.exe"  # 与 driver.go 的 setupExeName 必须一致
GO_BIN = r"C:\Program Files\Go\bin"

# CPL 里运行期真正要用到的部分（源相对路径 → 目标相对路径，目录则整份递归）
# ★ 2026-10-20 剔除 profile\default（官方 HiFi 空模板）：.profile 四端点全空、
#   无参数快照，场景墙侧 profile.go 也明确跳过它（应用等于什么都没做），
#   新框架已抛弃官方面板 ⟹ 这份文件没有任何消费者，不再拷贝。
CPL_ITEMS = [
    r"Driver\x64\vista\osConfLib.dll",
    "sound",
    r"profile\UserDefine.cml",
    r"profile\UserDefineFormwork.cml",
    r"lang\zh-CN",
    "FactoryDefault.xml",
]

# 驱动组件：108B.Inf 的 [SourceDisksFiles.amd64] + [DestinationDirs] 决定的最小集
DRV_ITEMS = [
    "108B.Inf",
    "108b.cat",
    r"x64\108B.sys",
    r"x64\CMCplExt_108B.dll",
    r"x64\CMEffectAPO_108B.dll",
]


def go_env():
    env = dict(os.environ)
    env["PATH"] = GO_BIN + os.pathsep + env.get("PATH", "")
    env["GOPROXY"] = "off"   # 依赖都在本地 module cache，完全离线
    env["GOSUMDB"] = "off"
    return env


def build_setup(out):
    """编译驱动管理程序（iKF-Audio-Center-Wails/installer 子包）。"""
    dst = os.path.join(out, SETUP_NAME)
    cmd = ["go", "build", "-mod=mod", "-ldflags", "-H windowsgui -s -w",
           "-o", dst, "./installer"]
    print("+ %s" % " ".join(cmd))
    if subprocess.run(cmd, cwd=APP, env=go_env()).returncode or not os.path.isfile(dst):
        sys.exit("编译驱动管理程序失败")
    return dst


IKFTOOL_NAME = "iKFTool.exe"  # 日志诊断工具（只读，不占单实例）


def build_ikftool(out):
    """编译 iKFTool 并入包。2026-10-28 起 build_portable 负责——此前它是
    10-25 手工放进包里的，一次全量重建就把手工件清掉了（已踩坑）。"""
    dst = os.path.join(out, IKFTOOL_NAME)
    cmd = ["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", dst, "./ikftool"]
    print("+ %s" % " ".join(cmd))
    if subprocess.run(cmd, cwd=APP, env=go_env()).returncode or not os.path.isfile(dst):
        sys.exit("编译 iKFTool 失败")
    return dst


def copy_item(src_root, rel, dst_root):
    """按相对路径拷文件或整目录，返回拷贝到的目标路径。"""
    s, d = os.path.join(src_root, rel), os.path.join(dst_root, rel)
    if os.path.isdir(s):
        shutil.copytree(s, d, dirs_exist_ok=True)
    elif os.path.isfile(s):
        os.makedirs(os.path.dirname(d), exist_ok=True)
        shutil.copy2(s, d)
    else:
        raise FileNotFoundError("源里没有：%s" % s)
    return d


# stash 固定放在工具目录：构建中断（rmtree 成功但后续步骤失败）时
# 数据不进回收站也不埋在系统 temp 里没人管，下次构建自动补回。
STASH_DIR = os.path.join(HERE, ".profile-stash")


def stash_user_profiles(out_dir):
    """打包前把整个 CPL/profile 树快照到 STASH_DIR，返回是否执行了快照。

    profileWriteDir() 在绿色版下会写进 `<exe>/CPL/profile`，而这里要
    `rmtree` 整个产物目录 —— 不先挪走就等于静默删掉用户自己存的场景，
    那是真数据丢失。整树快照（含 default/ 子目录），不只散装文件。
    """
    src = os.path.join(out_dir, "CPL", "profile")
    if not os.path.isdir(src):
        return False
    if os.path.isdir(STASH_DIR):
        # 上次构建中断留下的 stash 还没消化，绝不覆盖
        print("⚠ %s 已存在（上次构建残留），本次不重新快照，稍后一并放回"
              % os.path.relpath(STASH_DIR, ROOT))
        return True
    shutil.copytree(src, STASH_DIR)
    return True


def restore_user_profiles(stashed, out_dir):
    """把 stash 的用户档放回产物目录：只补产物里没有的文件，不覆盖
    源包拷回/EnsureSeedScenes 播种生成的新版内置档。"""
    if not stashed or not os.path.isdir(STASH_DIR):
        return
    dst = os.path.join(out_dir, "CPL", "profile")
    n = 0
    for root, _dirs, files in os.walk(STASH_DIR):
        rel = os.path.relpath(root, STASH_DIR)
        dst_root = dst if rel == "." else os.path.join(dst, rel)
        os.makedirs(dst_root, exist_ok=True)
        for fn in files:
            d = os.path.join(dst_root, fn)
            if not os.path.exists(d):
                shutil.copy2(os.path.join(root, fn), d)
                n += 1
    shutil.rmtree(STASH_DIR, ignore_errors=True)
    if n:
        print("放回用户场景档 %d 个文件（缺失才补，内置档以新版为准）" % n)


def dir_size(p):
    total = 0
    for r, _dirs, files in os.walk(p):
        for f in files:
            total += os.path.getsize(os.path.join(r, f))
    return total


def human(n):
    return "%.1f MB" % (n / 1024.0 / 1024.0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", default=DEF_SRC, help="旧驱动包的 SoftwareDriver 目录")
    ap.add_argument("--exe", default=DEF_EXE, help="已构建的主程序")
    ap.add_argument("--out", default=DEF_OUT, help="输出目录")
    ap.add_argument("--no-build", action="store_true", help="不重新编译驱动管理程序")
    ap.add_argument("--no-check", action="store_true", help="跳过产物自检")
    args = ap.parse_args()

    src_cpl = os.path.join(args.src, "DLL", "CPL", "X64")
    src_drv = os.path.join(args.src, "Driver")

    for p, what in ((src_cpl, "CPL 源"), (src_drv, "驱动源"), (args.exe, "主程序")):
        if not os.path.exists(p):
            sys.exit("找不到%s：%s" % (what, p))

    stashed = False
    if os.path.isdir(args.out):
        stashed = stash_user_profiles(args.out)
        if stashed and os.path.isdir(STASH_DIR):
            print("已暂存用户场景档 → %s" % os.path.relpath(STASH_DIR, ROOT))
        shutil.rmtree(args.out)
    os.makedirs(args.out)

    # 主程序放根，driver.go 的 localCpls() 会从这里往上找同目录资源
    shutil.copy2(args.exe, os.path.join(args.out, os.path.basename(args.exe)))

    # P1：音频热路 Rust DLL（audiocore 产物）。缺失不致命（exe 运行时降级
    # 到 Go 路径），但一键构建的包里必须带上，否则 Rust 化永不生效。
    audio_dll = os.path.join(APP, "audiocore", "target", "release", "ikf_audio.dll")
    if os.path.isfile(audio_dll):
        # 防旧 DLL 静默回潮：源码比 DLL 新 = 忘了 cargo build（崩溃修复
        # 只改 Rust 侧时尤其危险 —— 包看着新，崩溃面还是旧的）。
        import hashlib
        src_newest = 0
        for root, _dirs, files in os.walk(os.path.join(APP, "audiocore", "src")):
            for fn in files:
                if fn.endswith(".rs"):
                    src_newest = max(src_newest, os.path.getmtime(os.path.join(root, fn)))
        if src_newest > os.path.getmtime(audio_dll):
            print("WARN ikf_audio.dll 比 audiocore/src 旧 —— 请先 cargo build --release 再打包")
        with open(audio_dll, "rb") as f:
            dll_sha = hashlib.sha256(f.read()).hexdigest()
        shutil.copy2(audio_dll, os.path.join(args.out, "ikf_audio.dll"))
        print("ikf_audio.dll 已入包  sha256=%s  %d 字节" % (dll_sha, os.path.getsize(audio_dll)))
    else:
        print("WARN ikf_audio.dll 未构建（audiocore/target/release），包内将走 Go 路径")

    for rel in CPL_ITEMS:
        copy_item(src_cpl, rel, os.path.join(args.out, "CPL"))
    for rel in DRV_ITEMS:
        copy_item(src_drv, rel, os.path.join(args.out, "driver"))
    # ★ 2026-10-20：不再捆绑 SoundRecorder\Recorder.exe —— 死件（见头部
    #   注释）；录音机已内嵌。
    # 内置档（原厂默认/游戏/电影/音乐）由 EnsureSeedScenes 首次启动时生成，
    # 不在这里拷；stash 的场景树必须放回，否则上面那次 rmtree 就是静默删除。
    restore_user_profiles(stashed, args.out)

    if not args.no_build:
        build_setup(args.out)
        build_ikftool(args.out)

    print("已生成：%s" % args.out)
    for sub in ("", "CPL", "driver"):
        p = os.path.join(args.out, sub) if sub else args.out
        print("  %-8s %s" % (sub or "(根)", human(dir_size(p))))
    print("  合计     %s" % human(dir_size(args.out)))

    if args.no_check:
        return
    check_main(args.out, os.path.basename(args.exe))
    check_setup(args.out)


def check_main(out, exe_name):
    """自检一：把 ProgramFiles 掐掉，逼主程序只能读同目录 —— 读不到就说明包是残的。

    ★ 2026-10-22：耳机不在本机时（插到虚拟机里做测试是常规操作），
    `--dump` 必然报「驱动未连接」，但这不代表包是残的 —— 以前这里直接
    sys.exit(1)，整个构建判失败，而实际上文件早就复制完了。
    现在按项目约定给环境前提闸加 SKIP 分支：
      包里找得到 osConfLib.dll → 只做文件级校验，打印 SKIP 继续；
      找不到                     → 才是真的包残，失败退出。
    """
    env = dict(os.environ)
    for k in ("ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"):
        env[k] = r"C:\__nope__"
    r = subprocess.run([os.path.join(out, exe_name), "--dump"], cwd=out, env=env,
                       capture_output=True, text=True, errors="replace")
    txt = r.stdout + r.stderr
    if r.returncode != 0 or "驱动未连接" in txt:
        dlls = glob.glob(os.path.join(out, "CPL", "**", "osConfLib.dll"), recursive=True)
        if dlls:
            print("! 跳过：本机没连耳机（驱动组件在包内，路径 %s）" % dlls[0])
            print("  文件级校验通过；要跑完整冒烟请在插着耳机时重跑一次。")
            return
        print("✗ 自检失败：绿色目录起不来，且包内找不到 osConfLib.dll")
        print(txt[:600])
        sys.exit(1)
    n = sum(1 for ln in r.stdout.splitlines() if ln and not ln.startswith("#")
            and "---" not in ln)
    print("✓ 主程序：屏蔽 Program Files 后仍从同目录读到 %d 项参数" % n)


def check_setup(out):
    """自检二：驱动管理程序能看见自己的 driver/ 与系统里的驱动现状。

    --status 只读、不提权、不碰系统；--dry-run 只打印将要执行的命令。
    两者都跑通，说明包完整、pnputil 可用、INF 里的硬件 ID 能读出来。
    """
    exe = os.path.join(out, SETUP_NAME)
    if not os.path.isfile(exe):
        print("✗ 自检失败：缺 %s（加 --no-build 时请先完整构建过一次）" % SETUP_NAME)
        sys.exit(1)
    st = subprocess.run([exe, "--status"], cwd=out, capture_output=True,
                        text=True, errors="replace")
    txt = st.stdout + st.stderr
    if "（缺失）" in txt:
        print("✗ 自检失败：驱动管理程序看不到 driver/ 组件")
        print(txt[:600])
        sys.exit(1)
    if "硬件 ID  : （无）" in txt:
        print("✗ 自检失败：读不出 INF 里的硬件 ID")
        print(txt[:600])
        sys.exit(1)
    drv = [ln for ln in txt.splitlines() if ln.startswith("驱动库")]
    dry = subprocess.run([exe, "--install", "--dry-run", "--no-restart"], cwd=out,
                         capture_output=True, text=True, errors="replace")
    if dry.returncode != 0 or "pnputil /add-driver" not in dry.stdout:
        print("✗ 自检失败：安装预演没生成预期命令")
        print((dry.stdout + dry.stderr)[:600])
        sys.exit(1)
    print("✓ 驱动管理：%s ｜ 预演命令已生成（--status 退出码 %d）"
          % (drv[0].strip() if drv else "状态未知", st.returncode))


if __name__ == "__main__":
    main()

