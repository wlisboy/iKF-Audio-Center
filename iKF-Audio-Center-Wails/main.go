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

import ("path/filepath"
"io"

	"context"
	"embed"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

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

// ensureChannelTopology 已迁到 initstate.go —— 声道拓扑纠正不再只是
// 「启动期一次性」的事：装完驱动点「重新检测」连上后同样要补（2026-10-22
// 用户虚拟机实测：首次启动声道组不是立体声、重启主程序才对，就是因为它
// 当时只挂在启动路径上，而那次写入落在端点重建窗口里被丢弃了）。
// 现在由 EnsureInitialState 统一调用，写后回读确认 + 最多重试 3 次。

// seedSceneDefaults 驱动重装/参数存储清空后，把「默认」场景整体应用一次
// （2026-10-20 用户拍板：重装后所有参数按自定义场景默认来设置）。
// 判据：3 个哨兵参数（EQ 开关 / 房间大小 / AGC——「默认」场景必写的三处）
// 全部读不到（ret=2/空）才动手 —— 只要有记忆值就视为已有状态，绝不动用户
// 当前的调音。场景不含声道参数，拓扑播种由随后的 EnsureInitialState
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

// openAttempts 返回启动时 drv.Open() 的总尝试次数。
//
// ★ 2026-10-21（用户拍板）：Open 以前只试一次。iKF-驱动管理装完驱动后
// 会 `pnputil /restart-device` 重建音频端点，用户在「已安装完成」上点
// 确定就立刻打开主程序，很容易正好撞在重建窗口期里 —— 一次失败界面就
// 永久停在「未连接驱动」，以前只能彻底关掉重开。
//
// 无界面子命令只给 1 次：失败本身就是它们要观测的结果，多等 7 秒只会
// 拖慢闸脚本与排障。GUI 启动（含 --serve）才容忍这个窗口期：1+3 次。
func openAttempts() int {
	for _, a := range []string{
		"--self-test", "--selftest", "--roundtrip", "--dump", "--heal", "--init-check", "--init-fp",
		"--channel-test", "--play", "--loopback-devices", "--loopback-fft",
		"--loopback-test", "--restore-scene",
		// ★ 2026-10-22：--serve 也是无界面模式，必须单次。以前漏了它，
		//   服务启动要白等 1+2+4=7 秒，闸脚本按 sleep 5 起就撞「页面没就绪」。
		"--serve",
	} {
		if hasArg(a) {
			return 1
		}
	}
	return 4
}

// openWithRetry 按 1s / 2s / 4s 退避重试 openAttempts() 次。
func openWithRetry() bool {
	n := openAttempts()
	for i := 0; i < n; i++ {
		if i > 0 {
			wait := time.Duration(1<<uint(i-1)) * time.Second
			log.Printf("[启动] 驱动未就绪（%s），%v 后重试 %d/%d",
				drv.errMsg, wait, i, n-1)
			time.Sleep(wait)
		}
		if drv.Open() {
			if i > 0 {
				log.Printf("[启动] 第 %d 次重连成功 —— 端点重建窗口期已过", i)
			}
			return true
		}
	}
	return false
}

// setupFileLog 把标准日志落到 exe 旁的 log/yyyy-mm-dd.txt（追加）。
// ★ 2026-10-24（用户要求）：GUI 模式没有 stdout，此前 log.Printf 的输出
// 全部丢失（破坏性检测报告也点名"panic 无日志"）；现在同时写文件与
// stderr（serve 模式两者都能看）。按天分文件，多实例追加写安全。
func setupFileLog() {
	dir := filepath.Join(appDir(), "log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".txt"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("==== iKF Audio Center 启动（PID %d，%s）====", os.Getpid(),
		strings.Join(os.Args[1:], " "))
}

func main() {
	setupFileLog() // 日志最先挂上：之后任何阶段的输出都有据可查

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

	// ★破坏性检测 P1-2：入口族分发结束后仍带未识别参数 → 打印用法退出。
	// 原实现直接落进 GUI 常驻：自动化脚本写错参数时看到的是「进程不退出」
	// 而不是参数错误，还会抢走单实例锁与托盘。已知带参入口全部在上面
	// 分发（含 cli.go 的 serve/dump/play 等与 --takeover 管理族），
	// 走到这里还拿着参数 = 全都不认识。
	if extra := unknownFlags(); len(extra) > 0 {
		fmt.Fprintf(os.Stderr, "未知参数：%s\n\n用法：iKF-Audio-Center.exe [无参数=打开面板]\n"+
			"  --serve <addr>            无界面 HTTP 服务（如 127.0.0.1:18500）\n"+
			"  --self-test               自检\n"+
			"  --dump [capture]          参数转储\n"+
			"  其余诊断入口见 --help 文档\n", strings.Join(extra, " "))
		os.Exit(2)
	}

	// 单实例守卫必须在最前：第二实例在打开驱动/建 WebView2 之前就退出，
	// 避免两套驱动上下文与 WebView2 用户数据目录互杀。
	if guardSingleInstance() {
		log.Println("[单实例] 已有实例在运行，转交通知后退出")
		return
	}

	if ok := openWithRetry(); ok {
		log.Printf("驱动已就绪：%s（%d 个设备端点）", drv.dll, drv.devCnt)
		seedSceneDefaults() // 参数存储为空（驱动重装）→ 应用「默认」场景
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
	// --init-fp：只读查看「安装指纹」—— 端点 GUID / 采集端点 / 驱动版本 /
	// 设备实例，以及它相对上次记录有没有变。
	//
	// 这是「初始化什么时候该全量写」的可观察面（initfp.go）：变了 ⟹ 下次
	// 初始化按清单全量写；没变 ⟹ 只补空缺。装上驱动后跑一条命令就能确认
	// 「这次重装会被认出来」，不用等界面表现。**只读，不写设备、不落盘。**
	if hasArg("--init-fp") {
		if !drv.ready {
			fmt.Printf("驱动未就绪，无法读取指纹：%s\n", drv.errMsg)
			os.Exit(3)
		}
		fmt.Print(initFpReport(drv))
		return
	}
	// --init-check：把「初始化清单」逐项对一遍设备，只读不写。
	//
	// 这是「初始化」从口头约定变成可验收的关键一步：以前判断「装完驱动是不是
	// 真回到默认」只能拿界面肉眼看（一片「未设置」还是不是），现在一条命令
	// 给出「应有 55 · 达标 a · 未设置 b · 偏离 c」。退出码 1 = 不达标，
	// 可直接当安装后的验收闸（2026-10-22 用户要求"确保每一项按既定执行"）。
	if hasArg("--init-check") {
		if !drv.ready {
			fmt.Printf("驱动未就绪，无法核对：%s\n", drv.errMsg)
			os.Exit(3)
		}
		r, lines := CheckInitManifest(drv)
		fmt.Printf("初始化清单核对（只读，不写设备）\n")
		fmt.Printf("----------------------------------------------------------------\n")
		for _, ln := range lines {
			fmt.Println(ln)
		}
		fmt.Printf("----------------------------------------------------------------\n")
		fmt.Printf("应有 %d 项（参数 %d + 设备级 %d） · 达标 %d · 未设置 %d · 偏离 %d · 不支持 %d\n",
			r.Total, r.Total-len(initDeviceItems), len(initDeviceItems),
			r.OK, r.Missing, r.Drift, r.Unsupported)
		if r.Missing+r.Drift > 0 {
			fmt.Printf("结论：不达标（未设置/偏离 = %d 项）\n", r.Missing+r.Drift)
			os.Exit(1)
		}
		fmt.Printf("结论：达标 —— 设备已是初始化定义的默认状态\n")
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
	// ★ 2026-10-22：整条链收进 EnsureInitialState —— 「重新检测」和装驱动
	//   重连也要跑同一套（以前只在这里跑，用户装完驱动点重新检测就错过了）。
	EnsureInitialState(drv, true)

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
			// 2026-10-23（用户定稿）：启动不再做入口询问 —— 那条链要提权，
			// 体感是「装完驱动还弹一个替代官方面板的 UAC」。入口改写收口两处：
			// 装驱动时安装器已提权顺带写好（setup.go --takeover）、
			// 托盘菜单「修复/删除开机入口」用户主动点才提权（entry.go）。
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

// knownFlags 全部已支持的命令行参数（入口族 + 诊断族 + 面板开关）。
// ★破坏性检测 P1-2：入口族分发结束后逐一比对，未识别的打回 2。
var knownFlags = map[string]bool{
	"--serve": true, "--self-test": true, "--selftest": true, "--roundtrip": true,
	"--dump": true, "--play": true, "--secs": true, "--tone": true, "--wav": true,
	"--loop": true, "--loopback-test": true, "--loopback-devices": true,
	"--loopback-fft": true, "--heal": true, "--restore-scene": true, "--force": true,
	"--init-check": true, "--init-fp": true, "--device": true,
	"--takeover": true, "--takeover-admin": true, "--takeover-undo": true,
	"--takeover-undo-admin": true, "--entry-delete": true,
	"--entry-delete-admin": true, "--entry-status": true,
	"--hidden": true, "--tray": true, "--no-gpu": true, "--webview-compat": true,
}

// unknownFlags 返回 os.Args 里未识别的 -- 开头参数（值参数的值不算 flag，
// 由调用方白名单处理；裸非 -- 参数是历史兼容的位置参数，不拦）。
func unknownFlags() []string {
	var out []string
	for i, a := range os.Args {
		if i == 0 || !strings.HasPrefix(a, "--") || knownFlags[a] {
			continue
		}
		out = append(out, a)
	}
	return out
}

func argIndex(a string) int {
	for i, v := range os.Args {
		if v == a {
			return i
		}
	}
	return -1
}
