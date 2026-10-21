//go:build windows

// iKF Audio Center —— Wails 2 桌面版
// ====================================================================
//
// 单文件 exe，无运行时依赖：不需要 Python、不需要 Node、不需要 .NET。
// 原生能力由 Go 提供，界面由 WebView2 渲染。
//
// 架构
// ----
//
//	osConfLib.dll ──syscall──> driver.go（驱动桥接，钉线程串行）
//	                              │
//	                              └──> app.go（/api/* 契约）
//	                                        │
//	                    Wails AssetServer ───┴──> WebView2
//	                       (内部 scheme)           index.html + 桥接层
//
// 关键点：整套 HTTP 服务跑在 Wails 的内部 scheme `http://wails.localhost` 上，
// **不监听任何真实 TCP 端口** —— 没有端口占用、没有防火墙弹窗。
// 前端资源用 go:embed 内联，产物是一个自包含的 exe。
//
// 构建
// ----
//
//	go build -tags production -ldflags "-H windowsgui -s -w" -o iKF-Audio-Center.exe .
//
// 必须带 `-tags production`，否则会链接到 Wails 的空壳实现
// （internal/app/app_default_windows.go），编译能过、启动几秒后静默退出，一个窗口都不建。
//
// 命令行
// ------
//
//	iKF-Audio-Center.exe              启动界面
//	iKF-Audio-Center.exe --selftest   驱动接口自检（无界面，回归用）
//	iKF-Audio-Center.exe --roundtrip  写入往返验证（无界面，回归用）
//	iKF-Audio-Center.exe --dump       打印渲染端点全部 208 项参数实测值
//	iKF-Audio-Center.exe --dump capture  打印采集端点（麦克风）208 项
//	iKF-Audio-Center.exe --heal       设备侧自愈：只补「存在但当前无值」的项
//	iKF-Audio-Center.exe --loopback-fft    频谱计算自检（设备无关，可进 CI）
//	iKF-Audio-Center.exe --loopback-devices  列出可锁定的播放端点
//	iKF-Audio-Center.exe --loopback-test [秒] [--wav 文件] [--tone]
//	                                       系统回环采集自检（无界面）
//	iKF-Audio-Center.exe --play <wav> [--secs N] [--loop]
//	                                       只播一段 wav 当信号源，不开回环
//	iKF-Audio-Center.exe --serve :N   用普通 net/http 暴露同一套 /api/*（回归用）
//	iKF-Audio-Center.exe --takeover   入口接管：Run 自启 + 控制面板项 → 本程序
//	                                  （非管理员自动提权重跑，见 takeover.go）
//	iKF-Audio-Center.exe --entry-status  打印开机入口现状（只读，回归用）
//	iKF-Audio-Center.exe /h /d        等价旧面板 daemon：隐藏进托盘启动
//	                                  （--hidden / --tray 同义；接管后的 Run 项用）
package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	appName = "iKF Audio Center"
	appVer  = "1.0.0"

	// 设计稿画布固定 1280×800（16:10），HTML 内部做整体等比缩放，
	// 所以窗口只需给一个合适的初始值；允许缩放，最小尺寸保证画布可读。
	winW, winH       = 1280, 800
	winMinW, winMinH = 960, 600
)

//go:embed all:frontend/dist
var assets embed.FS

// drv 是全局唯一的驱动实例。
// 注意：它必须在包初始化时创建，因为内部要启动一个 runtime.LockOSThread()
// 钉住的 goroutine —— DLL 的设备上下文带线程局部状态，初始化与后续调用必须同线程。
var drv = NewDriver()

// correctChannelTopology 启动期声道拓扑纠正（2026-10-20 用户拍板）。
//
// 背景：声道拓扑（Speaker_Channel_Setting）是驱动服务级状态——电脑重启 /
// 驱动重装 / 耳机重新枚举时，驱动会把它写成自己的默认 5.1（实测 2026-10-20：
// 场景应用与端点重建都不会动它，场景文件也早不含这项，但用户早上仍发现
// 设备被复位成 5.1）。而本机耳机硬件就是立体声，4/6/8 拓扑纯属驱动默认值
// 作祟、没有任何使用价值，用户明确同意启动时自动纠正回 2。
//
// 口径：只在启动时做一次；读不到（ret≠0，设备不在/不支持）就静默跳过——
// 不替驱动下结论的老规矩。用户在设备页的手动选择仍然生效（本函数只在
// 拓扑为非 2 时纠正，运行期不再管）。
// seedSceneDefaults 驱动重装/参数存储清空后，把「默认」场景整体应用一次
// （2026-10-20 用户拍板：重装后所有参数按自定义场景默认来设置）。
// 判据：3 个哨兵参数（EQ 开关 / 房间大小 / AGC——「默认」场景必写的三处）
// 全部读不到（ret=2/空）才动手 —— 只要有记忆值就视为已有状态，绝不动用户
// 当前的调音。场景不含声道参数，拓扑播种由随后的 correctChannelTopology
// 照常兜底。「默认」场景被删/改名时 ProfileApply 报失败，静默跳过。
func seedSceneDefaults() {
	sentinels := []string{"Enable_EQ_GFX", "RFX_ROOMSIZE", "AGCControl"}
	empty := 0
	for _, n := range sentinels {
		if ret, v := drv.Read(n); ret != 0 || strings.TrimSpace(v) == "" {
			empty++
		}
	}
	if empty < len(sentinels) {
		return
	}
	r := drv.ProfileApply("默认")
	applied, _ := r["applied"].(float64)
	failed, _ := r["failed"].(float64)
	log.Printf("[启动] 参数存储为空（驱动重装/存储清空），已应用「默认」场景：%d 项 / 失败 %d",
		int(applied), int(failed))
}

func correctChannelTopology() {
	ret, v := drv.Read("Speaker_Channel_Setting")
	if ret != 0 {
		// 2026-10-20（用户拍板）：驱动重装/参数存储清空后读不到值（ret=2），
		// 播种默认立体声 2 —— 设备页声道组不再「四个 chip 全灭」，UI 默认
		// 选中态与实际拓扑一致。设备不在/参数不支持时 WriteAuto 会失败，
		// 静默跳过，不替驱动下结论的老规矩不破。
		if r := drv.WriteAuto(EpRender, "Speaker_Channel_Setting", 2, ""); r["ret"] == 0 {
			log.Println("[启动] 声道拓扑无记忆值（驱动重装/存储清空），播种默认立体声 2")
		}
		return
	}
	switch v {
	case "4", "6", "8":
		if r := drv.WriteAuto(EpRender, "Speaker_Channel_Setting", 2, ""); r["ret"] == 0 {
			log.Printf("[启动] 声道拓扑纠正：%s → 2（立体声）。驱动服务级状态"+
				"在重启/重枚举后会复位成 5.1，属驱动默认值，见 2026-10-20 排查记录。", v)
		}
	}
}

func main() {
	// ---- 入口管理族 CLI：必须先于单实例守卫与 drv.Open ----
	// ★ 2026-10-20 排障发现：--takeover 的提权子进程（--takeover-admin）
	//   起来时，父进程还活着并握着 singleguard 的 Local\ 互斥锁 —— 子进程
	//   被当成「第二实例」静默退出 0，父进程误报「接管完成」，注册表其实
	//   一项都没写（驱动卸载重装后恢复自启时实测，50ms 轮询零命中）。
	//   这一族只改注册表：不碰驱动、不建窗口、不抢设备上下文，与单实例
	//   守卫无关，放最前分发。--takeover 父进程自身也放行 —— 带着守护
	//   进程跑接管（daemon 在跑时用户修入口）是合法场景。
	if hasArg("--takeover-admin") {
		os.Exit(runTakeoverAdmin())
	}
	if hasArg("--takeover-undo-admin") {
		os.Exit(runTakeoverUndoAdmin())
	}
	if hasArg("--entry-delete-admin") {
		os.Exit(runEntryDeleteAdmin())
	}
	if hasArg("--takeover") {
		os.Exit(runTakeover())
	}
	if hasArg("--takeover-undo") {
		os.Exit(runTakeoverUndo())
	}
	if hasArg("--entry-delete") {
		os.Exit(runEntryDelete())
	}
	if hasArg("--entry-status") {
		fmt.Print(entryStatusLine())
		return
	}

	// 单实例守卫必须在最前：第二实例在打开驱动/建 WebView2 之前就退出，
	// 避免两套驱动上下文与 WebView2 用户数据目录互杀。
	if guardSingleInstance() {
		log.Println("[单实例] 已有实例在运行，转交通知后退出")
		return
	}

	if ok := drv.Open(); ok {
		log.Printf("驱动已就绪：%s（%d 个设备端点）", drv.dll, drv.devCnt)
		seedSceneDefaults() // 参数存储为空（驱动重装）→ 应用「默认」场景
		correctChannelTopology()
		go startParamWatch() // 外部音量/静音变化（系统音量键等）→ 通知主界面
	} else {
		log.Printf("驱动未就绪：%s", drv.errMsg)
	}

	// ---- 无界面模式：回归测试与诊断用，不建窗口 ----
	if hasArg("--selftest") || hasArg("--self-test") {
		selfTest()
		return
	}
	if hasArg("--roundtrip") {
		roundTrip()
		return
	}
	if i := argIndex("--dump"); i >= 0 {
		ep := EpRender
		if i+1 < len(os.Args) && os.Args[i+1] == EpCapture {
			ep = EpCapture
		}
		dumpParams(ep)
		return
	}
	if i := argIndex("--play"); i >= 0 && i+1 < len(os.Args) {
		secs := 10.0
		if j := argIndex("--secs"); j >= 0 && j+1 < len(os.Args) {
			if v, err := strconv.ParseFloat(os.Args[j+1], 64); err == nil && v > 0 {
				secs = v
			}
		}
		playOnly(os.Args[i+1], secs, hasArg("--loop"))
		return
	}
	if i := argIndex("--loopback-devices"); i >= 0 {
		loopbackDevices()
		return
	}
	if i := argIndex("--loopback-fft"); i >= 0 {
		loopbackFFTTest(i+1 < len(os.Args) && os.Args[i+1] == "dump")
		return
	}
	if i := argIndex("--loopback-test"); i >= 0 {
		secs := 6.0
		if i+1 < len(os.Args) {
			if v, err := strconv.ParseFloat(os.Args[i+1], 64); err == nil && v > 0 {
				secs = v
			}
		}
		wav := ""
		if j := argIndex("--wav"); j >= 0 && j+1 < len(os.Args) {
			wav = os.Args[j+1]
		}
		// --device：锁定某个播放端点（端点 ID 或名字子串）。
		// 不给就跟随系统默认播放设备 —— 但「默认」未必是耳机
		// （本机装了 FxSound，默认端点是 FxSound Speakers），
		// 所以要验「耳机那条路上有没有声音」必须能显式指定。
		dev := ""
		if j := argIndex("--device"); j >= 0 && j+1 < len(os.Args) {
			dev = os.Args[j+1]
		}
		loopbackTest(secs, hasArg("--tone"), wav, dev)
		return
	}
	// --restore-scene：手动触发一次「检测驱动重置指纹 → 自动回到默认场景」
	//（lastscene.go）。加 --force 跳过指纹强制恢复（排障兜底：指纹窗口
	// 被自愈提前跑掉时用）。开机守护与主程序启动也会自动跑（不带 force）。
	if hasArg("--restore-scene") {
		if !drv.ready {
			log.Printf("驱动未就绪，无法检测：%s", drv.errMsg)
			os.Exit(3)
		}
		_, msg := RestoreDefaultScene(drv, hasArg("--force"))
		log.Printf("%s", msg)
		return
	}
	// --heal：只跑一遍设备侧自愈，不进窗口/服务 —— 给「开关被灰掉、
	// 但不想连带改其他设置」的用户一个手动入口。核对效果看 --dump。
	if hasArg("--heal") {
		if !drv.ready {
			log.Printf("驱动未就绪，无法自愈：%s", drv.errMsg)
			os.Exit(3)
		}
		log.Printf("设备侧自愈：补写 %d 项（只补「存在但当前无值」的项，已有值不动）",
			HealMissingParams(drv))
		return
	}
	// --takeover / --takeover-undo / --entry-delete / --entry-status 这族
	// 已移到 main() 最前（先于单实例守卫），见文件开头注释 —— 否则提权
	// 子进程会被守卫挡死、父进程误报成功（2026-10-20 实测回归）。

	// 设备侧自愈：官方靠安装时的 `/h /i` + 每次开机的 `/h /d` 维持默认值，
	// 这里做等价的事 —— 只补「存在但当前无值」（ret=2）的项。
	// ★ 必须在场景播种之前：全量播种要实读设备补齐非特征项，
	//   没自愈的项读回 ret=2，场景档就会缺参数（2026-10-19 用户实测）。
	// ★ 场景自动恢复必须在自愈**之前**：自愈把 ret=2 项补成默认值后，
	//   「端点被重置」的指纹就测不出来了（2026-10-20，lastscene.go）。
	//   恢复目标 = 「默认」场景状态（2026-10-20 用户拍板：重装/重置后
	//   自动回到默认，之后自己点场景卡换）。
	cleanupLegacyLastApplied(drv)
	if triggered, msg := RestoreDefaultScene(drv, false); triggered {
		log.Printf("[启动] 场景自动恢复：%s", msg)
	}
	if n := HealMissingParams(drv); n > 0 {
		log.Printf("[启动] 设备侧自愈：补写了 %d 项出厂默认值", n)
	}

	// ---- 预置场景（游戏/音乐/电影 + 原厂默认） ----
	// 放在所有诊断分支之后：--selftest/--dump 等纯诊断不落盘，
	// 窗口模式与 --serve 模式都会经过这里。
	EnsureSeedScenes(drv)

	// 存量场景文件的旧分组名就地升级（第 51 轮，见 MigrateProfileGroups）。
	// 只改 <ApoEffect Name> 的值，命中才写，且先留 .bak。
	if mf, mt := MigrateProfileGroups(drv); mf > 0 {
		log.Printf("[启动] 场景分组名升级：%d 个文件的 %d 个 <ApoEffect> 换成官方页名",
			mf, mt)
	}

	if i := argIndex("--serve"); i >= 0 && i+1 < len(os.Args) {
		serveHTTP(os.Args[i+1])
		return
	}

	// ---- 正常模式：建窗口 ----
	//
	// 两个兼容性逃生口，默认都不开。用途：在远程桌面 / 无显卡虚拟机 /
	// 装了会向 WebView2 注入未签名 DLL 的安全软件 / 某些受限沙箱里，
	// WebView2 的子进程会反复退出（日志里是
	// `WebVie2wProcess failed with kind 6 / kind 4`），
	// 于是 NavigationCompleted 永远不来 —— 而 Wails 的 ShowWindow 恰好
	// 挂在那个回调里（internal/frontend/desktop/windows/frontend.go 的
	// navigationCompleted，注释写着 "Hack to make it visible"），
	// 表现就是「窗口已创建、标题与尺寸都正确、但始终不可见」。
	//
	//	--no-gpu           只关 WebView2 的 GPU 加速
	//	--webview-compat   关 GPU + 关渲染进程代码完整性校验（更激进）
	//
	// 环境变量 IKF_WEBVIEW_NOGPU=1 等价于 --no-gpu，方便在快捷方式里挂。
	winOpts := &windows.Options{}
	if hasArg("--no-gpu") || hasArg("--webview-compat") || os.Getenv("IKF_WEBVIEW_NOGPU") == "1" {
		winOpts.WebviewGpuIsDisabled = true
		log.Println("[启动] 已禁用 WebView2 GPU 加速")
	}
	if hasArg("--webview-compat") {
		winOpts.WebviewDisableRendererCodeIntegrity = true
		log.Println("[启动] 已禁用 WebView2 渲染进程代码完整性校验")
	}

	// ★ P1 方案 A（接管）：Run 项以 /h /d 拉起时等价旧面板的「隐藏托盘
	//   常驻」—— StartHidden 启动，窗口不弹，托盘左键唤出。winShown 的
	//   初值必须同步，否则托盘第一次左键会走错分支（先隐藏了一遍）。
	startHidden := hasArg("/h") || hasArg("/d") || hasArg("--hidden") || hasArg("--tray")
	winShown = !startHidden

	err := wails.Run(&options.App{
		Title:       appName,
		Width:       winW,
		Height:      winH,
		StartHidden: startHidden,
		// 单实例：Wails v2.16 的 SingleInstanceLock 有 bug（FindWindowW 搜不到
		// HWND_MESSAGE 消息窗口，实测双开），改用自研守卫 singleguard.go，
		// 在 main() 最开头执行（先于 drv.Open / wails.Run）。
		// 设计稿深浅两套主题的底色分别是 #0B0B0D / #F2F2F4，
		// body 底为 #111114。窗口底色取 #111114，让等比缩放产生的
		// 上下留边与画面融为一体，不出现白边闪屏。
		MinWidth:         winMinW,
		MinHeight:        winMinH,
		BackgroundColour: &options.RGBA{R: 0x11, G: 0x11, B: 0x14, A: 255},
		Windows:          winOpts,
		AssetServer: &assetserver.Options{
			Assets: assets,
			// Handler 只在「内嵌资源里找不到同名文件」时被调用
			// （GET 走 404 回落，非 GET 直接进来），所以 /api/* 天然命中这里，
			// 而 `/` 仍由内嵌的 index.html 提供。前端与设计稿零改动。
			Handler: &API{},
		},
		// 这三条日志刻意保留：出问题时（例如某些受限环境下 WebView2 的
		// 沙箱助手进程起不来）靠它们能判断断点在哪一段 ——
		// 窗口对象有没有建、WebView2 环境有没有建、HTML 有没有加载解析完。
		//
		// ★ 第 64 轮（B4）：OnStartup 顺带起系统托盘；OnBeforeClose 把
		//   「点 ✕」拦截成隐藏进托盘，只有托盘菜单「退出」才真退出 ——
		//   与原面板 SystemTrayLClick/RClick 的常驻行为一致。
		OnStartup: func(ctx context.Context) {
			log.Println("[启动] 窗口对象已创建")
			startTray(ctx)
			// 入口自检（P5-1'）：便携包被拷到别处后，把开机自启与控制面板项
			// 跟着改到新位置。只在「用户主动打开界面」时问；/h /d 隐藏启动
			// 不弹（开机弹 UAC 不可接受），改由托盘菜单兜底。
			if !startHidden {
				go maybePromptEntryFix()
			}
		},
		OnDomReady: func(ctx context.Context) { log.Println("[启动] 界面已加载解析完成（OnDomReady）") },
		// 返回 true = 拦截本次关闭。托盘「退出」已置 quitRequested 时放行。
		OnBeforeClose: func(ctx context.Context) bool {
			if quitWanted() {
				return false
			}
			log.Println("[托盘] 窗口关闭 → 隐藏进托盘（退出请在托盘右键菜单）")
			winShown = false
			wruntime.WindowHide(ctx)
			return true
		},
		OnShutdown: func(ctx context.Context) {
			log.Println("[退出] 停止回环并释放驱动")
			lb.Stop()
			drv.Close()
		},
	})
	if err != nil {
		log.Fatalf("启动失败：%v", err)
	}
}

func hasArg(a string) bool { return argIndex(a) >= 0 }

func argIndex(a string) int {
	for i, v := range os.Args {
		if v == a {
			return i
		}
	}
	return -1
}
