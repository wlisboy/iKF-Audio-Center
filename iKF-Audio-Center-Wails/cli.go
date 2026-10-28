//go:build windows

package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// winmm 只用来在自检里播一段 wav 当信号源（见 playWav）。
var (
	procPlaySound = syscall.NewLazyDLL("winmm.dll").NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndLoop      = 0x0008
	sndFilename  = 0x00020000
)

// ====================================================================
// 无界面命令行：回归测试与诊断
// --------------------------------------------------------------------
// 这些子命令不建窗口，纯粹为了在 CI / 沙箱里验证驱动桥接层的正确性。
// 输出格式刻意与 Python 版（iKF-Audio-Center.py）对齐，便于逐行 diff。
// ====================================================================

// selfTest 等价于 Python 版的 `--self-test`，输出逐行对齐。
func selfTest() {
	line := strings.Repeat("=", 68)
	dash := strings.Repeat("-", 68)

	fmt.Println(line)
	fmt.Printf(" %s %s  ·  接口自检\n", appName, appVer)
	fmt.Println(line)
	fmt.Printf("驱动目录 : %s\n", drv.cpl)
	fmt.Printf("驱动模块 : %s\n", drv.dll)
	fmt.Printf("音频热路 : %s\n", audioStatus())

	if !drv.ready {
		fmt.Printf("状态     : 未连接 —— %s\n", drv.errMsg)
		os.Exit(1)
	}

	st := drv.Status()
	fmt.Printf("状态     : 已连接（%v 个设备端点）\n", st["device_count"])
	fmt.Println(dash)
	for _, kv := range []struct{ key, label string }{
		{"model", "设备型号"}, {"codec", "编解码器"},
		{"driver", "驱动版本"}, {"firmware", "固件版本"},
		{"formats", "音频格式"}, {"hwid", "设备 ID"},
	} {
		fmt.Printf("%-9s: %v\n", kv.label, st[kv.key])
	}
	fmt.Printf("%-9s: %s\n", "主音量", fmt.Sprintf("%.2f dB（范围 %.1f .. %.1f，步进 %.1f）",
		f64(st["vol_db"]), f64(st["vol_min"]), f64(st["vol_max"]), f64(st["vol_step"])))
	fmt.Printf("%-9s: %.4f\n", "标量音量", f64(st["vol_scalar"]))
	fmt.Printf("%-9s: %s\n", "静音", onOff(asBool(st["mute"])))
	fmt.Printf("%-9s: %s（%s）\n", "均衡器", onOff(asBool(st["eq_on"])), joinEq(st["eq"]))
	fmt.Println(dash)

	var ok, unsup, notfound, broken int
	for _, n := range allNames {
		switch r, _ := drv.Read(n); r {
		case 0:
			ok++
		case 2:
			unsup++
		case -1:
			notfound++
		default:
			broken++
		}
	}
	fmt.Printf("属性总数 : %d（通用 %d + OEM %d）\n", len(allNames), len(genericNames), len(oemNamesList))
	fmt.Printf("可读     : %d\n", ok)
	/* ★ 2026-10-22：这一行原写「本机不支持」。ret=2 的实测含义是
	   「存在但从未写过」（卸载/重装驱动清空 FxProperties 后全变 2，
	   写一次就回 0），不是本机不支持。同一件事两套说法，和界面上
	   麦克风页徽章那处一起改成「未写入」。 */
	fmt.Printf("未写入   : %d\n", unsup)
	fmt.Printf("未找到   : %d\n", notfound)
	fmt.Printf("需特殊参数: %d\n", broken)
	fmt.Println(dash)
	fmt.Printf("可写参数 : %d 项（渲染端点 %d + 采集端点 %d）\n",
		writableCount(), len(writable[EpRender]), len(writable[EpCapture]))
	if drv.capReady {
		cok := 0
		for _, n := range allNames {
			if r, _ := drv.ReadEp(EpCapture, n); r == 0 {
				cok++
			}
		}
		fmt.Printf("麦克风端点: 已建立（可读 %d 项）\n", cok)
	} else {
		fmt.Println("麦克风端点: 未建立（该机型可能只有渲染端点）")
	}
	pf := drv.Profiles()
	names := []string{}
	if lst, ok := pf["list"].([]ProfileEntry); ok {
		for _, e := range lst {
			names = append(names, e.Name)
		}
	}
	joined := "无"
	if len(names) > 0 {
		joined = strings.Join(names, "、")
	}
	fmt.Printf("特效组态 : %d 个（%s）\n", len(names), joined)
	suffix := ""
	if w, _ := pf["writable"].(bool); !w {
		suffix = "（不可写）"
	}
	fmt.Printf("组态目录 : %s%s\n", pf["dir"], suffix)
	fmt.Println(line)
}

// dumpParams 等价于 Python 版的 `--dump [render|capture]`。
func dumpParams(ep string) {
	if !drv.ready {
		fmt.Printf("驱动未连接：%s\n", drv.errMsg)
		os.Exit(1)
	}
	if _, ok := drv.ptrFor(ep); !ok {
		fmt.Printf("端点不可用：%s\n", ep)
		os.Exit(1)
	}
	fmt.Printf("# 端点：%s\n", ep)
	fmt.Printf("%-34s %-10s %-6s %s\n", "属性名", "返回码", "表", "值")
	fmt.Println(strings.Repeat("-", 78))
	for _, n := range allNames {
		table := "OEM"
		if genericSet[n] {
			table = "通用"
		}
		r, v := drv.ReadEp(ep, n)
		fmt.Printf("%-34s %-10d %-6s %s\n", n, r, table, v)
	}
}

// roundTrip 写入往返验证：每项写后立即还原。
// 与 Python 版实测的 5 项对照（Enable_SS_LFX / EQ_Slider3 / RFX_ROOMSIZE /
// MuteControl / Enable_EQ_GFX）逐项打印 ok / ret / 读回值 / 还原值。
func roundTrip() {
	if !drv.ready {
		fmt.Printf("驱动未连接：%s\n", drv.errMsg)
		os.Exit(1)
	}
	checks := []struct {
		name string
		val  interface{}
	}{
		{"Enable_SS_LFX", 1}, {"EQ_Slider3", 7}, {"RFX_ROOMSIZE", 2},
		{"MuteControl", 1}, {"Enable_EQ_GFX", 0},
	}
	fmt.Println("写入往返验证（每项写后立即还原）：")
	for _, c := range checks {
		_, before := drv.Read(c.name)
		res := drv.Write(c.name, c.val)
		_, after := drv.Read(c.name)
		drv.Write(c.name, before) // 还原
		_, restored := drv.Read(c.name)
		fmt.Printf("  %-18s %s → 写%v  ok=%v ret=%v 读回=%q 还原=%q\n",
			c.name, before, c.val, res["ok"], res["ret"], after, restored)
	}
}

// serveHTTP 用普通 net/http 暴露同一套 /api/*，便于用 curl 做契约回归。
// 正常运行时不会走到这里 —— 那时服务跑在 Wails 的内部 scheme 上，不占端口。
func serveHTTP(addr string) {
	idx, err := assets.ReadFile("frontend/dist/index.html")
	if err != nil {
		log.Fatalf("内嵌前端资源缺失：%v", err)
	}
	api := &API{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(idx)
			return
		case "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
			return
		}
		api.ServeHTTP(w, r)
	})
	log.Printf("HTTP 服务已启动：http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ---------------------------------------------------------------- 回环自检

// playWav 用 winmm 的 PlaySound 播一个 wav，异步、走默认渲染端点。
//
// 为什么要在自检里自己发声：本沙箱没有别的办法往默认端点推声音
// （headless Chrome 无音频设备；PowerShell 从 Bash 调被安全策略拦）。
// 而回环采集在「系统静音」时返回全 0 是**正确行为** —— 不给信号源就
// 分不清「链路坏了」和「本来就没声音」。自己播一段就都清楚了。
//
// ★ 第 28 轮注：原先这条注释写着「驱动自带的 PlayTestSound 在本机返回
//   0x10001，所以走不通」—— 那个结论是错的。0x10001 是因为调用方把
//   设备上下文当成了 wav 路径（见 driver.go 的 PlayTest），修好之后
//   PlayTestSound 是可用的，--tone 也重新走驱动自带的测试音。
//
// 不释放 winmm 句柄：SND_ASYNC 下由系统托管，进程退出自动停。
func playWav(path string) error { return playWavEx(path, false) }

func playWavEx(path string, loop bool) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	flags := uintptr(sndFilename | sndAsync | sndNoDefault)
	if loop {
		flags |= sndLoop // SND_LOOP 必须与 SND_ASYNC 同用
	}
	r, _, _ := procPlaySound.Call(uintptr(unsafe.Pointer(p)), 0, flags)
	if r == 0 {
		return fmt.Errorf("PlaySound 返回 0（文件不存在或没有可用输出设备）")
	}
	return nil
}

// playOnly 只往默认播放设备播一段 wav，**不开回环**。
//
// 用途：手工验证「后端回环能不能采到声音」时的信号源。
// 之所以不直接用 --loopback-test --wav：那个会自己再开一路回环，
// 于是场上有两个回环客户端，排查问题时变量变多。
// （两个客户端共存本身是合法的，共享模式下互不影响。）
//
// loop=true 时循环播放 —— 自动化测试里页面的加载时机不好卡，
// 循环播能把「声音窗口」拉满整个测试时长，避免时序竞态。
func playOnly(path string, secs float64, loop bool) {
	if err := playWavEx(path, loop); err != nil {
		fmt.Printf("播放失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("正在播放 %s（%.0f 秒%s，走默认播放设备）\n",
		path, secs, map[bool]string{true: "，循环", false: ""}[loop])
	time.Sleep(time.Duration(secs * float64(time.Second)))
	procPlaySound.Call(0, 0, 0) // NULL 停止播放
}

// loopbackFFTTest 是**设备无关**的频谱正确性自检。
//
// 绕过 WASAPI，直接往环形缓冲灌已知波形，逐项核对与 Chromium 的
// AnalyserNode 是否同字节。**期望值不是推导出来的，是实测的** ——
// 由 .workbuddy-ai/tmp/probe_wa_spectrum.js 在 headless Chrome 里
// 扫振幅、等平滑收敛后读出来的稳态字节：
//
//	amp 1.0     → 255（截顶）  并列 #19..#21
//	amp 0.1     → 242
//	amp 0.01    → 169
//	amp 0.001   → 96
//	amp 0.0001  → 23
//
// 频点取 937.5 Hz = 48000·20/1024，正好落在第 20 个 bin 中心，主瓣不劈叉。
// 平滑 τ = 0.8 要迭代 40 帧才收敛（1 − 0.8^40 ≈ 99.99%）。
//
//	dump 参数：额外导出 512 字节，供与 Chromium 全谱对拍
func loopbackFFTTest(dump bool) {
	const rate = 48000
	const wantBin = 20
	line := strings.Repeat("=", 68)
	dash := strings.Repeat("-", 68)

	fmt.Println(line)
	fmt.Printf(" %s %s  ·  频谱计算自检（与 Chromium 同字节）\n", appName, appVer)
	fmt.Println(line)
	fmt.Printf("采样率   : %d Hz · FFT %d 点 · %d 个 bin · 每 bin %.4f Hz\n",
		rate, lbFFTSize, lbBins, float64(rate)/float64(lbFFTSize))
	fmt.Printf("窗函数   : Blackman（相干增益 %.2f）· 平滑 τ = %.1f（每调用一次）\n",
		blackmanGain, waSmoothing)
	fmt.Printf("归一化   : mag = |X[k]| / N（实测反解，非规范正文的 2/(N·CG)）\n")
	fmt.Printf("dB 区间  : %.0f .. %.0f\n", waMinDB, waMaxDB)
	fmt.Println(dash)

	// 期望字节全部来自 headless Chrome 的实测（见函数头）。
	cases := []struct {
		name  string
		amp   float64
		want  int
		tieLo int // 0 表示不检查并列区间
		tieHi int
	}{
		{"满幅 0 dB", 1.0, 255, 19, 21},
		{"-20 dB", 0.1, 242, 0, 0},
		{"-40 dB", 0.01, 169, 0, 0},
		{"-60 dB", 0.001, 96, 0, 0},
		{"-80 dB", 0.0001, 23, 0, 0},
		{"静音", 0, 0, 0, 0},
	}

	bad := 0
	for _, c := range cases {
		b := synthSpectrum(rate, c.amp)
		lo, hi, pv := -1, -1, 0
		for k, v := range b {
			switch {
			case int(v) > pv:
				pv, lo, hi = int(v), k, k
			case int(v) == pv && pv > 0:
				hi = k
			}
		}
		where := fmt.Sprintf("#%d", lo)
		if hi != lo {
			where = fmt.Sprintf("#%d..#%d", lo, hi)
		}

		why := ""
		if c.amp == 0 {
			if pv != 0 {
				why = "静音应全 0"
			}
			where = "—"
		} else if absInt(pv-c.want) > 1 {
			why = fmt.Sprintf("字节应为 %d±1", c.want)
		} else if lo < 0 || wantBin < lo || wantBin > hi {
			why = fmt.Sprintf("峰值区间应含 #%d", wantBin)
		} else if c.tieLo > 0 && (lo != c.tieLo || hi != c.tieHi) {
			why = fmt.Sprintf("并列区间应为 #%d..#%d", c.tieLo, c.tieHi)
		}
		verdict := "OK"
		if why != "" {
			verdict = "✗ " + why
			bad++
		}
		fmt.Printf("  %-10s 峰值 %-11s 字节 %-4d 期望 %-4d %s\n",
			c.name, where, pv, c.want, verdict)
	}

	fmt.Println(dash)
	if bad == 0 {
		fmt.Println("结论     : 全部通过 —— 与 Chromium AnalyserNode 同字节")
	} else {
		fmt.Printf("结论     : %d 项不符\n", bad)
	}
	fmt.Println(line)

	if dump {
		b := synthSpectrum(rate, 0.01)
		fmt.Println("BINS " + strings.Join(byteStrings(b), " "))
	}
	if bad > 0 {
		os.Exit(1)
	}
}

// synthSpectrum 把一段 bin 中心正弦灌进环形缓冲并跑到收敛，返回 512 字节。
// 与 Chromium 侧 probe 的信号完全一致，是两条实现可比的前提。
func synthSpectrum(rate int, amp float64) []byte {
	l := NewLoopback()
	l.rate, l.running = rate, true
	// ringW 设成 1024：computeSpectrum 取 ring[(ringW−1024+8192)%8192 ...]，
	// 起点正好是 0，与下面填样本的位置对齐。
	l.ringW = lbFFTSize
	l.ringN = lbFFTSize
	for i := 0; i < lbFFTSize; i++ {
		l.ring[i] = float32(amp * math.Sin(2*math.Pi*937.5*float64(i)/float64(rate)))
	}
	for i := 0; i < 40; i++ { // 让平滑收敛
		l.computeSpectrum()
	}
	out := make([]byte, len(l.bins))
	copy(out, l.bins)
	return out
}

func byteStrings(b []byte) []string {
	s := make([]string, len(b))
	for i, v := range b {
		s[i] = strconv.Itoa(int(v))
	}
	return s
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// lbBands 与前端 BANDS 逐项一致（lo/hi 是 512 条 bin 的下标）。
//
// 两边必须同步改：这个表是「后端频谱到底有没有内容」的唯一判据，
// 也是两条音源（后端回环 / 屏幕捕获）画面对齐的验收点。
var lbBands = []struct {
	name   string
	lo, hi int
}{
	{"超低音", 0, 1}, {"低音", 2, 3}, {"中低音", 4, 7}, {"中音", 8, 18},
	{"中高音", 19, 46}, {"临场感", 47, 93}, {"明亮度", 94, 186}, {"空气感", 187, 372},
}

// loopbackDevices 列出可锁定的播放端点（--loopback-devices）。
//
// 输出与 Python 线的 --loopback-devices **逐行一致**，是两条线设备枚举
// 实现的回归判据；同时也是「仅耳机可视化」的人工验收入口 —— 拿这里的
// 端点 ID 填给 /api/loopback/start 的 device 参数即可锁定该设备。
func loopbackDevices() {
	line := strings.Repeat("=", 68)
	dash := strings.Repeat("-", 68)
	fmt.Println(line)
	fmt.Printf(" %s %s  ·  播放端点列表（WASAPI loopback 可选）\n", appName, appVer)
	fmt.Println(line)
	fmt.Println("[默认] 标记当前默认播放设备；回环不带 device 参数时采它。")
	fmt.Println("蓝牙耳机选 Stereo/A2DP，不要选 Hands-Free（16k 窄带，音乐不在那条上）。")
	fmt.Println(dash)
	res := lb.Devices()
	if ok, _ := res["ok"].(bool); !ok {
		fmt.Printf("枚举失败 —— %v\n", res["error"])
		fmt.Println(line)
		os.Exit(1)
	}
	list, _ := res["list"].([]map[string]interface{})
	defID, _ := res["default"].(string)
	for i, d := range list {
		label, _ := d["name"].(string)
		id, _ := d["id"].(string)
		if id == defID {
			label = "[默认] " + label
		}
		fmt.Printf(" %d. %s\n      %s\n", i+1, label, id)
	}
	fmt.Println(dash)
	fmt.Printf("共 %d 个端点\n", len(list))
	fmt.Println(line)
}

// resolveLoopbackDevice 把 --device 的参数解析成端点 ID。
//
// 接受两种写法：
//
//	"端点 ID"   原样采用（Devices() 列出来的那种 GUID 串）
//	"名字子串"  在端点友好名里做不区分大小写的包含匹配，取第一个命中
//
// 子串写法是为了省事：本机耳机端点叫「扬声器 (iKF-V11)」，敲
// `--device iKF` 即可，不必粘那一长串 GUID。arg 为空表示跟随系统默认。
func resolveLoopbackDevice(arg string) (string, string, error) {
	res := lb.Devices()
	if ok, _ := res["ok"].(bool); !ok {
		return "", "", fmt.Errorf("枚举端点失败 —— %v", res["error"])
	}
	list, _ := res["list"].([]map[string]interface{})
	defID, _ := res["default"].(string)

	nameOf := func(id string) string {
		for _, d := range list {
			if v, _ := d["id"].(string); v == id {
				n, _ := d["name"].(string)
				return n
			}
		}
		return id
	}

	if arg == "" {
		return "", nameOf(defID), nil
	}
	for _, d := range list { // 先当完整 ID
		if v, _ := d["id"].(string); v == arg {
			n, _ := d["name"].(string)
			return arg, n, nil
		}
	}
	low := strings.ToLower(arg)
	for _, d := range list { // 再当名字子串
		n, _ := d["name"].(string)
		if strings.Contains(strings.ToLower(n), low) {
			id, _ := d["id"].(string)
			return id, n, nil
		}
	}
	names := make([]string, 0, len(list))
	for _, d := range list {
		n, _ := d["name"].(string)
		names = append(names, n)
	}
	return "", "", fmt.Errorf("找不到端点 %q —— 可选：%s", arg, strings.Join(names, " / "))
}

// loopbackTest 是无界面的回环采集自检。
//
// 为什么需要它：本沙箱起不了 GUI（WebView2 必崩），前端那条路验不了；
// 而回环采集整条链路（COM 单元、虚表下标、混音格式分支、FFT 归一化）
// 全是「错了就静默返回 0」的类型 —— 没有这个入口就只能靠肉眼。
//
//	--loopback-test [秒]     默认 6 秒
//	--wav <路径>             采集期间用 winmm 播这段 wav 当信号源
//	--tone                   改用驱动自带的 PlayTestSound 当信号源
//	--device <ID|名字子串>    锁定某个播放端点（不给 = 跟随系统默认）
//
// 输出三个层面的证据：
//  1. 打开成功 + 实际混音格式（Hz / 声道 / 采样编码）
//  2. 帧计数是否在涨 —— 证明采集循环真的在收包（设备空闲时 WASAPI 回环
//     也会送静音包，所以帧数增长与「有没有声音」无关，是链路健康的判据）
//  3. 八个频段的均值与峰值 bin 频率 —— 证明 FFT 与归一化是对的
func loopbackTest(secs float64, tone bool, wav string, devArg string) {
	line := strings.Repeat("=", 68)
	dash := strings.Repeat("-", 68)

	fmt.Println(line)
	fmt.Printf(" %s %s  ·  系统回环采集自检\n", appName, appVer)
	fmt.Println(line)
	fmt.Printf("驱动     : %s\n", drv.dll)
	if !drv.ready {
		fmt.Printf("状态     : 驱动未连接 —— %s\n", drv.errMsg)
	}

	// ★ 「默认播放设备」不一定是耳机。本机装了 FxSound，默认端点是
	//   FxSound Speakers，而界面（律动空间）是**显式锁耳机端点**的
	//   （见设计稿的 pickHeadset：名字含 iKF → 含 C-Media → 回落默认）。
	//   所以这里的默认行为（跟随默认）与界面行为**不一致**，只适合
	//   验采集链路本身；要验「耳机那条路上有没有声音」必须 --device。
	dev, devName, err := resolveLoopbackDevice(devArg)
	if err != nil {
		fmt.Println(dash)
		fmt.Printf("端点     : %v\n", err)
		fmt.Println(line)
		os.Exit(1)
	}
	if devArg == "" {
		fmt.Printf("端点     : 跟随系统默认（%s）\n", devName)
		fmt.Println("           ↑ 默认未必是耳机；装了 FxSound 之类虚拟声卡时")
		fmt.Println("             默认端点常是虚拟设备，界面里则是显式锁耳机")
	} else {
		fmt.Printf("端点     : 已锁定 %s\n", devName)
	}
	fmt.Println(dash)

	res := lb.Start(dev)
	if ok, _ := res["ok"].(bool); !ok {
		fmt.Printf("回环     : 启动失败 —— %v\n", res["error"])
		os.Exit(1)
	}
	rate := int(f64(res["rate"]))
	ch := int(f64(res["ch"]))
	fmt.Printf("回环     : 已启动（%d Hz · %d 声道 · %v）\n", rate, ch, res["format"])
	// 把混音格式的原始字段打出来。格式判错时症状是「帧数照涨、频谱恒 0」，
	// 只看 kind 是查不出来的 —— 必须能看到 wFormatTag / SubFormat / 位深。
	bits := int(f64(res["bits"]))
	fmt.Printf("混音格式 : wFormatTag=0x%04X", int(f64(res["tag"])))
	if int(f64(res["tag"])) == 0xFFFE {
		fmt.Printf("（EXTENSIBLE）SubFormat=0x%04X", int(f64(res["sub"])))
	}
	fmt.Printf(" · %d bit\n", bits)
	if bits == 0 {
		fmt.Println("           ↑ 位深读成 0，多半是 WAVEFORMATEXTENSIBLE 结构体布局算错了")
	}
	if already, _ := res["already"].(bool); already {
		fmt.Println("           注意：本次是复用已有会话，帧计数不是从 0 起算")
	}

	// 信号源。PlayTestSound 是驱动自己往**耳机**那条路推的测试音，
	// 所以「回环采不采得到它」取决于回环锁在哪个端点：
	// 锁耳机 → 采得到；跟随默认（本机默认是 FxSound Speakers）→ 采不到，
	// 频谱恒 0。频谱全 0 而帧数照涨时，先怀疑端点对不上，别怀疑 FFT。
	if tone {
		// 掩码 1 = 左前。★ 第 28 轮以前这里写的是 0x6BF —— 一串既不是单个
		// 声道、也不在驱动「整组」白名单里的位，加上第一个参数传错，
		// 无论传什么都返 0x10001，看不出是参数问题。
		tr := drv.PlayTest(1)
		fmt.Printf("测试音   : 驱动 PlayTestSound %v\n", tr)
		defer func() {
			drv.StopTest()
			drv.CloseTest()
		}()
	}
	if wav != "" {
		if err := playWav(wav); err != nil {
			fmt.Printf("信号源   : 播放失败 —— %v\n", err)
		} else {
			fmt.Printf("信号源   : %s\n", wav)
		}
	}

	// ---- 采样 ----
	st0 := lb.Status()
	frames0 := uint64(0)
	if v, ok := st0["frames"].(uint64); ok {
		frames0 = v
	}

	peakLevel := 0.0
	bandSum := make([]float64, len(lbBands))
	bandMax := make([]int, len(lbBands))
	globalMax := 0
	// 峰值 bin 取「最响的那一帧」而不是最后一帧 —— 声音往往在采集窗口
	// 中途就停了，最后一帧是静音，打出来永远是 #0 ≈ 0 Hz，白搭。
	var best []byte
	bestTop := -1
	samples := 0

	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	for time.Now().Before(deadline) {
		<-tk.C
		sp := lb.Spectrum()
		if ok, _ := sp["ok"].(bool); !ok {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(str(sp["bins"]))
		if err != nil || len(raw) != lbBins {
			continue
		}
		samples++
		if lv := f64(sp["level"]); lv > peakLevel {
			peakLevel = lv
		}
		top := 0
		for bi, b := range lbBands {
			sum := 0
			for k := b.lo; k <= b.hi && k < len(raw); k++ {
				sum += int(raw[k])
				if int(raw[k]) > bandMax[bi] {
					bandMax[bi] = int(raw[k])
				}
				if int(raw[k]) > globalMax {
					globalMax = int(raw[k])
				}
				if int(raw[k]) > top {
					top = int(raw[k])
				}
			}
			bandSum[bi] += float64(sum) / float64(b.hi-b.lo+1)
		}
		if top > bestTop {
			bestTop = top
			best = raw
		}
	}

	st1 := lb.Status()
	frames1 := uint64(0)
	if v, ok := st1["frames"].(uint64); ok {
		frames1 = v
	}
	got := frames1 - frames0
	expect := float64(rate) * secs
	fmt.Println(dash)
	fmt.Printf("采样     : %.1f 秒内收到 %d 个频谱帧\n", secs, samples)
	fmt.Printf("采集帧数 : %d（期望约 %.0f，达成率 %.1f%%）\n",
		got, expect, ratio(float64(got), expect)*100)
	if samples == 0 || got == 0 {
		fmt.Println("结论     : 采集链路**没有**跑起来 —— 检查回环是否被设备占用")
		fmt.Println(line)
		lb.Stop()
		os.Exit(1)
	}
	fmt.Printf("峰值电平 : %.4f（0..1，满幅为 1）\n", peakLevel)
	fmt.Println(dash)
	fmt.Println("频段均值 / 峰值（字节 0..255，-100dB..-30dB）：")
	for bi, b := range lbBands {
		avg := bandSum[bi] / float64(samples)
		fmt.Printf("  %-8s %5.1f  %s  %d\n",
			b.name, avg, bar(avg), bandMax[bi])
	}
	fmt.Println(dash)

	if best != nil {
		pk, pv := 0, 0
		for k, v := range best {
			if int(v) > pv {
				pk, pv = k, int(v)
			}
		}
		hz := float64(pk) * float64(rate) / float64(lbFFTSize)
		fmt.Printf("峰值 bin : #%d ≈ %.0f Hz（字节 %d，取最响的一帧）\n", pk, hz, pv)
	}
	fmt.Printf("声道掩码 : 0x%04X\n", int(f64(st1["mask"])))
	fmt.Printf("字节极值 : 全局最大 %d\n", globalMax)
	verdict := "有内容"
	if globalMax == 0 {
		verdict = "全 0 —— 先查端点，再查 FFT"
	}
	fmt.Printf("结论     : 链路健康，频谱%s\n", verdict)
	if globalMax == 0 {
		// ★ 这里以前只写「FFT 或归一化有问题」，把人往错的方向引。
		//   实测（本机装 FxSound）：跟随默认端点时帧数照涨、频谱恒 0，
		//   锁到耳机端点后同一个测试音立刻有内容 —— 端点对不上是第一嫌疑。
		fmt.Println("可能原因 :")
		fmt.Println("           ① 回环端点与发声设备不是同一个（最常见）。本机装")
		fmt.Println("              了 FxSound 时默认端点就是虚拟设备，试 --device iKF；")
		fmt.Println("           ② 信号源根本没出声（系统静音 / 音量拉到 0…）；")
		fmt.Println("           ③ FFT 或归一化有问题 —— 可用 --loopback-fft 排除。")
	}
	fmt.Println(line)

	lb.Stop()
}

// bar 把 0..255 画成 24 格条，纯文本环境下也能一眼看出形状。
func bar(v float64) string {
	n := int(v / 255 * 24)
	if n < 0 {
		n = 0
	}
	if n > 24 {
		n = 24
	}
	return "[" + strings.Repeat("#", n) + strings.Repeat(".", 24-n) + "]"
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------- 小工具

func f64(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	// 下面这几个是 Status()/Start() 里以窄整型放进去的
	// （bits/tag/sub/mask 等）。漏掉会静默返回 0，
	// 表现是「字段明明有值，自检打出来是 0」。
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	}
	return 0
}

func asBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

func joinEq(v interface{}) string {
	eq, _ := v.([]int)
	parts := make([]string, 0, len(eq))
	for _, n := range eq {
		parts = append(parts, fmt.Sprintf("%+d", n))
	}
	return strings.Join(parts, " ")
}
