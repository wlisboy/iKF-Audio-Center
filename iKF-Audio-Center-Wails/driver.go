//go:build windows

// osConfLib.dll 的 Go 封装 —— center_src.py 中 Driver 类的逐项移植。
//
// 移植时保留了两条不能改的约定：
//  1. 属性名走 UTF-16（syscall.UTF16PtrFromString）
//  2. pValue 按属性所属表二选一（通用 = 普通指针；OEM = 二级指针 → int32 声道号）
//
// 调用必须串行化：DLL 非线程安全。
package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ---------------------------------------------------------------- 属性名表

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

// 临时诊断开关（定位 PropertyControl 内部崩溃用）
var pcTrace = os.Getenv("IKF_PC_TRACE") != ""

var genericSet = func() map[string]bool {
	m := make(map[string]bool, len(genericNames))
	for _, n := range genericNames {
		m[n] = true
	}
	return m
}()

var oemSet = func() map[string]bool {
	m := make(map[string]bool, len(oemNamesList))
	for _, n := range oemNamesList {
		m[n] = true
	}
	return m
}()

var allNames = append(append([]string{}, genericNames...), oemNamesList...)

// knownName 208 项的成员判定集合。WriteRaw 用它区分
// 「驱动确实有这个属性，只是我们没接进界面」与「根本不存在的名字」。
var knownName = func() map[string]bool {
	m := make(map[string]bool, len(allNames))
	for _, n := range allNames {
		m[n] = true
	}
	return m
}()

// ---------------------------------------------------------------- 可写白名单

const (
	TBool = iota
	TInt
	TFloat
)

// 两个设备端点。osConfLib 的 GetDeviceCount(type,…) 有两条互相独立的链表，
// 两套上下文的属性可读性完全不同：
//
//	type=0 → 渲染端点（扬声器）  GetSupportFeature = 16801926
//	type=1 → 采集端点（麦克风）  GetSupportFeature = 164352
//
// **麦克风侧参数必须打在采集端点上**，否则一律返回 ret=2（不支持）。
// 早期实现只建了渲染端点，导致整个麦克风页被误判为「驱动未开放」。
const (
	EpRender  = "render"
	EpCapture = "capture"
)

type wspec struct {
	kind     int
	lo, hi   float64
	hasRange bool
	// choices 非空时表示离散取值（写入前做白名单校验，避免给驱动递非法值）
	choices []int
}

// writable —— 按端点分表，S3 起真源 = model/device-model.toml [[writable]]
// （80 = render 65 + capture 15，装载时自校验，含 SS×16/EQ×10/声道×2 显式展开）。
//
// 注意 VolumeControl / VolumeScalarControl / MuteControl 在**两个端点上都有**，
// 语义分别是扬声器音量与麦克风音量，所以必须分开登记。
// 历史逐条真值注释（GFSControl 恢复登记、SURRSENS 绑定更正、声道切换证伪、
// SS 规范化区间、DevSampleRate 离散收口等）随数据走，见 TOML 与当日日志。
var writable = func() map[string]map[string]wspec {
	kindOf := map[string]int{"bool": TBool, "int": TInt, "float": TFloat}
	out := map[string]map[string]wspec{EpRender: {}, EpCapture: {}}
	for _, w := range modelData.Writable {
		sp := wspec{kind: kindOf[w.T], choices: w.Choices}
		if w.Lo != nil && w.Hi != nil {
			sp.lo, sp.hi, sp.hasRange = *w.Lo, *w.Hi, true
		}
		out[w.Ep][w.Name] = sp
	}
	return out
}()

var channelParams = map[string]int{
	"VolumeScalarControl#0": 0, // FrontLeft
	"VolumeScalarControl#1": 1, // FrontRight
}

// splitChannel `VolumeScalarControl#0` → ("VolumeScalarControl", 0)；
// 无 "#" → (name, -1)。与 Python 侧 _split_channel 必须一致。
func splitChannel(name string) (string, int) {
	if i := strings.LastIndex(name, "#"); i >= 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil {
			return name[:i], n
		}
	}
	return name, -1
}

// spec 取某端点上的可写规格
func spec(ep, name string) (wspec, bool) {
	m, ok := writable[ep]
	if !ok {
		return wspec{}, false
	}
	sp, ok := m[name]
	return sp, ok
}

// writableCount 可写参数总数（两条端点合计），供自检输出
func writableCount() int {
	n := 0
	for _, m := range writable {
		n += len(m)
	}
	return n
}

// environments —— RFX_ENVIRONMENT 的索引语义已由静态反编译确认：
// CMEffectAPO_108B.dll 的 FUN_18021efd0 对 *(this+0x27C) 做 switch，
// case 0="Default"、1="Generic"、2="Paddedcell" … 23="Underwater"、24="KTV"；
// 原面板 Skin/FaceLift/apoSpeakerEM.cml 的 RadioButton Tag 与之吻合
// （Bathroom=4 / ConcertHall=8 / UnderWater=23 / Psychotic=26），
// 即 Tag = 下拉位置（含被注释掉的 Default 项）。故 0 = 预设（原厂默认）。
// 本机出厂值 1 在此映射下即「一般环境」，合理。
// 24..27（迷幻/晕眩/疯狂/KTV）取自原面板下拉定义；APO 调试表在 24 处
// 标注为 KTV，与面板不一致，故 24 之后仅作可选扩展。
var environments = []string{
	"预设（原厂默认）", "一般环境", "吸音室", "卧房", "浴室",
	"客厅", "石室", "观众席", "音乐厅", "洞穴", "竞技场", "飞机棚",
	"地毯走廊", "走廊", "石头回廊", "胡同", "森林", "城市", "山中",
	"采石场", "平原", "停车场", "下水道", "水底",
	"迷幻", "晕眩", "疯狂", "KTV",
}

var roomSizes = []string{"小房间", "中房间", "大房间"}

// magicVoices —— 魔音类型，取自原面板 apoMicInXearMagicVoice.cml 的 RadioButton 文案
var magicVoices = []string{"怪兽", "卡通", "男人", "女人"}

// testChannels —— 声道测试：掩码取自原面板 apoSpeakerSetting.cml 的
// PlayTestSound Tag 全集。四元组：标识 / 掩码 / 中文 / 测试音 wav 文件名。
//
// ★ 第 28 轮：第四个字段是**必须**的。反编译 osConfLib.dll
// （Ghidra 12.1.4，PlayTestSound@0x1800060E0）后才看清第一个参数是
// LPWSTR 类型的 wav 路径，直接进 mmioOpenW + CreateFileW ——
// 也就是 MSDN 那个经典示例 WaveOpenFile；拿不到文件句柄就 return 0x10001。
// 以前把设备上下文指针塞在这里当路径用，必然打不开文件，
// 于是面板上一直显示「声道测试不可用（65537）」。
// 资源就在驱动安装目录下：<CPL>/sound/*.wav，原面板自带的那 8 个。
var testChannels = [][4]interface{}{
	{"LF", 1, "左前", "L-Front.wav"},
	{"RF", 2, "右前", "R-Front.wav"},
	{"C", 4, "中置", "Center.wav"},
	{"W", 8, "低音", "Subwoofer.wav"},
	{"LB", 16, "左后", "L-BackSurround.wav"},
	{"RB", 32, "右后", "R-BackSurround.wav"},
	{"LS", 512, "左环绕", "L-Surround.wav"},
	{"RS", 1024, "右环绕", "R-Surround.wav"},
}

// testWav 掩码 → 测试音 wav 的绝对路径；掩码不在表里返回空串。
//
// 必须是绝对路径：驱动内部走 mmioOpenW，相对路径是按进程当前目录解析的，
// 而本程序的当前目录不确定。
func (d *Driver) testWav(mask int) string {
	if d.cpl == "" {
		return ""
	}
	for _, tc := range testChannels {
		if m, ok := tc[1].(int); ok && m == mask {
			name, _ := tc[3].(string)
			if name == "" {
				return ""
			}
			return filepath.Join(d.cpl, "sound", name)
		}
	}
	return ""
}

// noopSkip：写入是「相对增量」而非绝对值的属性，不能做「值相同即跳过」
var noopSkip = map[string]bool{"VolumeControl": true}

// slowConst —— 「读一次要两秒、值却永远不变」的属性，读一次就记住。
//
// 实测（.workbuddy-ai/tmp/time_props.py，本机 iKF-V11，208 项逐项计时）：
//
//	GetDirectXVer      2120.8 ms   ← 唯一的慢项
//	GetFirmwareVer        8.5 ms
//	GetDriverVer          5.6 ms
//	GetAudioCodecName     4.1 ms
//	其余 204 项合计        约 23 ms
//
// 它慢是因为驱动内部要枚举 DXGI 适配器。而 /api/status 和 /api/params 都会
// 读它，于是**两个端点都被钉在 2.1 秒**上（实测 /api/status 2092ms、
// /api/params 2114ms，而 /api/meter 只要 2ms）。
//
// 后果不只是慢：前端页面加载时会并发打这两个端点，2 秒的 DLL 调用把驱动
// 线程堵满，连一个简单的 /api/status 都要排到 2.9 秒才回。缓存之后两个
// 端点都降到约 30ms。
//
// 只缓存**读成功**（ret == 0）的结果：
//
//	· 值本身是这台机器的 DirectX 版本，进程生命周期内不会变，所以缓存
//	  不会让界面显示过期数据；
//	· 失败不缓存，避免一次瞬时故障（0x10001 之类）被永久记住 ——
//	  前端会因此一直显示「—」。
//
// GetDirectXVer 是只读属性（不在写入白名单里），所以不需要考虑写失效。
// 换机器若发现别的慢项，把名字加进来即可。
var slowConst = map[string]bool{"GetDirectXVer": true}

type constEntry struct {
	ret int
	val string
}

// hazard —— 移植时最需要注意的一处语言差异。
//
// 这三项在驱动内部会解引用我们传进去的 pValue 结构，需要额外的参数形态；
// 用普通约定调用会触发访问违例（0xC0000005）。
//
//	Python 的 ctypes 会把该访问违例转换成可捕获的 OSError，
//	所以 center_src.py 里 `except OSError: return -2` 能优雅降级。
//
//	Go 的 syscall 不做这层转换：访问违例会直接终止整个进程，
//	defer/recover 拦不住（recover 只能捕获 Go 自己的 panic）。
//
// 因此在 Go 侧必须显式跳过，结果码沿用 Python 的 -2 语义，
// 保证两张能力表逐项一致。
var hazard = map[string]bool{
	"IsSupportFormat":             true,
	"GetSpeakerShiftVolumeLevel":  true,
	"GetSpeakerShiftDistanceByDB": true,
}

// ---------------------------------------------------------------- 驱动定位

const cplEnv = "IKF_CPL"

func registryCpl() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.READ)
	if err != nil {
		return ""
	}
	defer k.Close()
	subs, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return ""
	}
	for _, s := range subs {
		sk, err := registry.OpenKey(k, s, registry.READ)
		if err != nil {
			continue
		}
		name, _, _ := sk.GetStringValue("DisplayName")
		loc, _, _ := sk.GetStringValue("InstallLocation")
		sk.Close()
		if loc == "" {
			continue
		}
		low := strings.ToLower(name)
		if strings.Contains(low, "ikf") || strings.Contains(low, "headset") {
			return loc
		}
	}
	return ""
}

// appDir 返回 exe 所在目录 —— 绿色版（免安装）资源的根。
//
// 只能用 exe 自身路径推导：提权后当前目录会变成 System32，
// 双击与「以管理员身份运行」的进程 cwd 还未必相同，相对路径一律不可信。
func appDir() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(p)
}

// localCpls 绿色版候选：程序目录**排在安装版前面**，同目录自带的资源优先于 Program Files。
//
// 支持两种铺法（都在同一个目录里就能跑）：
//
//	<exeDir>/CPL/     整份 CPL 目录拷过来（与安装版同构，推荐）
//	<exeDir>/         osConfLib.dll 直接挨着 exe（扁平铺法）
func localCpls() []string {
	d := appDir()
	if d == "" {
		return nil
	}
	return []string{filepath.Join(d, "CPL"), d}
}

// sharedCpls 安装版候选：旧安装包铺在 Program Files 下的路径。
// 绿色版跑在没装过驱动的机器上时这里会落空，属于预期。
func sharedCpls() []string {
	var out []string
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out,
			filepath.Join(pf, "iKF headset", "CPL"),
			filepath.Join(pf, "iKF headset"))
	}
	out = append(out, `C:\Program Files\iKF headset\CPL`)
	return out
}

// walkMaxDepth 递归兜底的深度上限。见 findDLL 的说明。
//
// 取 4：官方铺法最深是 <CPL>/Driver/x64/vista（3 层），但 sharedCpls() 里还有
// 一个**父目录级**兜底 `C:\Program Files\iKF headset`，它的 DLL 在第 4 层
// （CPL\Driver\x64\vista\）。4 既保住那条兜底，又能排掉工程根里便携包那份
// （dist/iKF-Audio-Center-Portable/CPL/Driver/x64/vista 在第 6 层）。
const walkMaxDepth = 4

func findDLL(cpl string) string {
	rels := []string{
		filepath.Join("Driver", "x64", "vista", "osConfLib.dll"),
		filepath.Join("Driver", "x64", "osConfLib.dll"),
		filepath.Join("x64", "vista", "osConfLib.dll"),
		"osConfLib.dll",
	}
	for _, r := range rels {
		p := filepath.Join(cpl, r)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	// 兜底：目录内递归找一次。
	//
	// ★★ 第 50 轮的坑：兜底必须**限深**。候选里有 appDir() = 程序所在目录，
	// 源码/开发态下它就是工程根，而工程根的 dist/ 里躺着便携包，便携包自带
	// <工程根>/dist/iKF-Audio-Center-Portable/CPL/Driver/x64/vista/osConfLib.dll。
	// 不限深地 Walk 一遍会把它认下来，于是「工程根就是一个 CPL」，
	// sound/ profile/ lang/ FactoryDefault.xml 全部失联
	// （症状：源码运行时声道测试 0/8 资源就位、组态与词条读不到）。
	// 上面 rels 里官方文档过的铺法最深是 Driver/x64/vista（3 层），但 sharedCpls()
	// 还有个父目录级兜底（DLL 在第 4 层），故上限取 walkMaxDepth = 4；
	// 便携包那份在 6 层，天然排除。与 Python 侧 _WALK_MAX_DEPTH 保持一致。
	var found string
	filepath.Walk(cpl, func(p string, info os.FileInfo, err error) error {
		if err != nil || found != "" {
			return nil
		}
		rel, rerr := filepath.Rel(cpl, p)
		if rerr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(os.PathSeparator)))
		}
		if info.IsDir() {
			if depth >= walkMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Base(p), "osConfLib.dll") {
			found = p
		}
		return nil
	})
	return found
}

// ---------------------------------------------------------------- 解码

func u16(raw []byte) string {
	u := make([]uint16, 0, 64)
	for i := 0; i+1 < len(raw); i += 2 {
		v := uint16(raw[i]) | uint16(raw[i+1])<<8
		if v == 0 {
			break
		}
		u = append(u, v)
	}
	return string(utf16.Decode(u))
}

func guessType(v string) string {
	if v == "0" || v == "1" {
		return "bool"
	}
	if _, err := strconv.Atoi(v); err == nil {
		return "int"
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return "float"
	}
	return "str"
}

// ---------------------------------------------------------------- Driver

// 移植时的第二处语言差异，也是 Go 版必须额外做的一件事：**线程亲和**。
//
// osConfLib.dll 的设备上下文带有线程局部状态（TLS）：
// ConfLibInit / CreateDeviceList / GetDeviceById 在哪个 OS 线程上调用，
// 之后 PropertyControl 就必须在同一个 OS 线程上调用，
// 否则会返回 0x10001（设备上下文无效）。
//
// Python 版天然满足这一点——调用始终发生在同一个固定线程上。
// Go 的 goroutine 是 M:N 调度的，会在 OS 线程之间迁移：
// 实测同一份代码在主 goroutine 里 208 项全部正常，
// 放到 net/http 的处理 goroutine 里就有 1 项（GetDirectXVer）返回 0x10001。
//
// 解决办法：把所有 DLL 调用收敛到一个 runtime.LockOSThread() 钉住的
// 专用 goroutine 上，通过 channel 投递。这样同时也天然实现了串行化，
// 取代了 Python 版里的互斥锁。
type Driver struct {
	reqCh chan func()

	confInit  *syscall.LazyProc
	confUnini *syscall.LazyProc
	createLst *syscall.LazyProc
	getCount  *syscall.LazyProc
	getByID   *syscall.LazyProc
	propCtrl  *syscall.LazyProc
	regCb     *syscall.LazyProc

	// 三条附加导出：原面板用它们做声道测试（监听两条 2026-10-20 移除：
	// 官方 RunListen2Device 依赖的 COM 服务器驱动包里没有，见 listen.go）
	playTest  *syscall.LazyProc
	stopTest  *syscall.LazyProc
	closeTest *syscall.LazyProc
	showAsio  *syscall.LazyProc

	// 渲染端点（type=0）
	devKeep []byte
	devPtr  uintptr

	// 采集端点（type=1）——麦克风侧参数只能打在这里
	capKeep  []byte
	capPtr   uintptr
	capReady bool
	capCnt   int

	// PlayTestSound 的第三个参数指向一个 3×dword 结构，
	// 内容就是设备上下文前 12 字节：链表内索引 / 设备类型 / 链表选择。
	// 0 = 渲染链表，1 = 采集链表（反汇编确认，传别的值返回 0x10001）。
	devIdx  int32
	devList int32
	capIdx  int32
	capList int32

	cpl    string
	dll    string
	devCnt int
	errMsg string
	ready  bool

	// slowConst 的缓存，键是 "端点\x00属性名"。见 slowConst 的说明。
	constMu    sync.Mutex
	constCache map[string]constEntry
}

// NewDriver 启动钉住线程的工作 goroutine
func NewDriver() *Driver {
	d := &Driver{reqCh: make(chan func(), 64)}
	go func() {
		runtime.LockOSThread() // 关键：整个生命周期不换线程
		for f := range d.reqCh {
			f()
		}
	}()
	return d
}

// run 把一次调用投递到工作线程并等待完成
func (d *Driver) run(fn func()) {
	done := make(chan struct{})
	d.reqCh <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// Open 必须在工作线程上完成初始化，否则 TLS 与后续调用不在同一线程
func (d *Driver) Open() bool {
	var ok bool
	d.run(func() { ok = d.openLocked() })
	return ok
}

func (d *Driver) openLocked() bool {
	// 候选顺序（与 Python 版 center_src.py 的 _open_impl 逐项一致）：
	//   IKF_CPL 环境变量 → 绿色版程序目录 → 注册表安装位置 → Program Files 安装版
	var cands []string
	if v := os.Getenv(cplEnv); v != "" {
		cands = append(cands, v)
	}
	cands = append(cands, localCpls()...)
	if r := registryCpl(); r != "" {
		cands = append(cands, r)
	}
	cands = append(cands, sharedCpls()...)

	var tried []string
	// ★ 2026-10-26：循环里有三个不同的失败点（LoadLibrary / ConfLibInit /
	// 设备枚举为 0），以前统一报"加载失败"。VM 实证（2026-10-26 00:02）：
	// 插上耳机立刻启动程序时，重试 3 次失败发生在设备还在枚举的窗口里
	// （设备管理器 00:02:59 才"已启动设备"，应用 00:03:14 自愈）——真实
	// 原因多半是"设备数为 0"而非加载失败。failWhy 记录最后一次尝试
	// （通常是便携包自带 CPL）真实失败在哪一步，随 errMsg 进按天日志。
	failWhy := ""
	seen := map[string]bool{}
	for _, cpl := range cands {
		if cpl == "" || seen[cpl] {
			continue
		}
		seen[cpl] = true
		if st, err := os.Stat(cpl); err != nil || !st.IsDir() {
			continue
		}
		dll := findDLL(cpl)
		if dll == "" {
			continue
		}
		tried = append(tried, dll)

		// 让依赖 DLL 能从同目录解析（等价于 Python 的 os.add_dll_directory）
		dirPtr, _ := syscall.UTF16PtrFromString(filepath.Dir(dll))
		kernel32.NewProc("SetDllDirectoryW").Call(uintptr(unsafe.Pointer(dirPtr)))
		runtime.KeepAlive(dirPtr)

		mod := syscall.NewLazyDLL(dll)
		if err := mod.Load(); err != nil {
			// 常见：126 依赖缺失、32 ERROR_SHARING_VIOLATION（杀软扫描占住
			// 文件）、1115 DLL 初始化失败 —— 数字码比文字好 grep。
			failWhy = fmt.Sprintf("加载 DLL 失败（%v）", err)
			continue
		}
		d.confInit = mod.NewProc("ConfLibInit")
		d.confUnini = mod.NewProc("ConfLibUnInit")
		d.createLst = mod.NewProc("CreateDeviceList")
		d.getCount = mod.NewProc("GetDeviceCount")
		d.getByID = mod.NewProc("GetDeviceById")
		d.propCtrl = mod.NewProc("PropertyControl")
		d.regCb = mod.NewProc("RegisterCallbackFunction")
		d.playTest = mod.NewProc("PlayTestSound")
		d.stopTest = mod.NewProc("StopTestSound")
		d.closeTest = mod.NewProc("CloseTestSound")
		d.showAsio = mod.NewProc("ShowAsioPanel")

		if r, _, _ := d.confInit.Call(); int32(r) != 0 {
			failWhy = fmt.Sprintf("ConfLibInit 返回 %d", int32(r))
			continue
		}
		d.createLst.Call()

		var cnt int32
		d.getCount.Call(0, uintptr(unsafe.Pointer(&cnt)))
		runtime.KeepAlive(&cnt)
		if cnt <= 0 {
			failWhy = "设备枚举为 0（耳机未插入或系统还在枚举 USB 设备）"
			/* ★ 2026-10-22：Init 成功就必须配对撤销。以前这里直接 continue，
			   驱动没装时启动重试 4 次就留下 4 次未配对的 ConfLibInit，之后
			   Close 只撤 1 次 —— 净残留 3 次。实测本机（设备在线）重复 Init
			   无害，但这是明确的资源管理缺口，不能靠"没崩"继续放着。 */
			d.confUnini.Call()
			continue
		}
		dev := make([]byte, 32)
		r, _, _ := d.getByID.Call(0, 0, uintptr(unsafe.Pointer(&dev[0])))
		runtime.KeepAlive(dev)
		if int32(r) != 0 {
			d.confUnini.Call()
			continue
		}
		d.devKeep = dev
		d.devPtr = uintptr(unsafe.Pointer(&dev[0]))
		d.devIdx, d.devList = 0, 0 // GetDeviceById(0, 0, …)：渲染链表第 0 个

		// 采集端点（麦克风）。拿不到不算失败 —— 有些机型只有渲染端点。
		var ccnt int32
		d.getCount.Call(1, uintptr(unsafe.Pointer(&ccnt)))
		runtime.KeepAlive(&ccnt)
		d.capCnt = int(ccnt)
		if ccnt > 0 {
			cdev := make([]byte, 32)
			cr, _, _ := d.getByID.Call(1, 0, uintptr(unsafe.Pointer(&cdev[0])))
			runtime.KeepAlive(cdev)
			if int32(cr) == 0 {
				d.capKeep = cdev
				d.capPtr = uintptr(unsafe.Pointer(&cdev[0]))
				d.capReady = true
				d.capIdx, d.capList = 0, 1 // GetDeviceById(1, 0, …)：采集链表第 0 个
			}
		}

		d.cpl, d.dll, d.devCnt, d.ready, d.errMsg = cpl, dll, int(cnt), true, ""
		d.warmUpLocked()
		return true
	}

	if len(tried) > 0 {
		msg := tried[len(tried)-1]
		if failWhy != "" {
			msg += "，" + failWhy
		}
		d.errMsg = fmt.Sprintf("耳机驱动组件未就绪（%s）。请重新安装 iKF headset 驱动，并确认耳机已插入后重试。", msg)
	} else {
		d.errMsg = "未检测到耳机驱动。请先安装 iKF headset 驱动，并确认耳机已连接到电脑，然后重新打开本程序。"
	}
	return false
}

func (d *Driver) Close() {
	d.run(func() {
		if d.confUnini != nil {
			func() {
				defer func() { recover() }()
				d.confUnini.Call()
			}()
		}
		d.ready = false
		/* ★ 2026-10-21：指针必须一起清。以前只清 ready，Close→Open 之后
		   若新设备枚举不出采集端点，capReady/capPtr 还留着上一轮的旧值 ——
		   那是已经作废的设备上下文（Close 已 ConfLibUnInit），再拿去
		   PropertyControl 就是悬垂指针。reopen 让这条路径变成常规路径，
		   所以在这里一次性归零。 */
		d.devPtr, d.capPtr = 0, 0
		d.devKeep, d.capKeep = nil, nil
		d.capReady = false
		d.devCnt, d.capCnt = 0, 0
		/* ★ 2026-10-22：常量缓存一起清。Close 之后设备可能换了/驱动重装了，
		   沿用上一轮 slowConst 的读数（如 GetDirectXVer）没有意义。 */
		d.constMu.Lock()
		d.constCache = nil
		d.constMu.Unlock()
	})
}

// Reopen 松开旧上下文再重新枚举端点。
//
// ★ 2026-10-21（用户拍板）：装完驱动立刻打开主程序时，音频端点可能还在
// pnputil /restart-device 的重建窗口期里，openLocked 会一次失败。以前
// 界面就此永久停在「未连接驱动」，唯一的出路是彻底关掉重开。Reopen 给
// 前端一个自恢复入口（POST /api/driver/reopen），与 RunSetup 里装完驱动
// 那一步 close→open 完全同路。
//
// ★ 2026-10-22（虚拟机上暴露，用户实测）：装完驱动再点「重新检测」，出现的
// 是「麦克风 + 自定义场景」不可用、其余正常，而且**退出重开就好**。
//
// 原因是端点重建不是原子的：渲染端点先就绪、采集端点（麦克风）后到。
// openLocked 一次就返回的话渲染侧全通、采集侧全 -9（ptrFor 里 capReady
// 为 false），界面就是"一半能用"。场景墙同步空掉，因为 CPL 路径跟着一起没落地。
//
// ★ 重试判据不能用「上一轮有没有采集端点」：主程序启动时驱动根本没装，
// 上一轮必然没有，判据恒不成立 —— 第一次修就栽在这里（一次都没重试）。
// 现在只认「本轮连上了但采集端点缺失」，重试到拿到为止。
func (d *Driver) Reopen() bool {
	ok := false
	for i := 0; i < 5; i++ {
		if i > 0 {
			ms := 400 << uint(i-1)
			if ms > 1600 {
				ms = 1600
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		d.Close()
		ok = d.Open()
		if !ok {
			/* 完全没连上（驱动没装/端点整个没起来）：只再给一次机会
			   （400ms）就够了。等满 5 轮 = 4.4 秒，用户点一下「重新检测」
			   干等 4 秒才被告知没驱动，这个体验不能要。 */
			if i >= 1 {
				return false
			}
			continue
		}
		// 采集端点还没起来 → 端点重建没结束，这里才值得多等几轮
		// （400/800/1600/1600ms）
		if !d.capReady && i < 4 {
			continue
		}
		return true
	}
	return ok
}

// ptrFor 取端点对应的设备上下文指针
func (d *Driver) ptrFor(ep string) (uintptr, bool) {
	if ep == EpCapture {
		if !d.capReady {
			return 0, false
		}
		return d.capPtr, true
	}
	if !d.ready {
		return 0, false
	}
	return d.devPtr, true
}

// pc 是唯一入口：payload 非空即写入（值取 *ppBuffer 首个 dword）
// 整体投递到钉住的工作线程执行。
// warmUp 丢弃式预热读取。**必须在工作线程上调用**（openLocked 内部），
// 因此这里不再走 d.run —— 那会和 openLocked 自己形成自锁。
//
// 实测：ConfLibInit 之后**第一次** PropertyControl 读取有约 10% 的概率
// 返回 ret=0 但缓冲区一个字节都没写（值为空串）。这是驱动内部的初始化
// 竞态，不是调用方的问题 —— Python 版因为解释器启动慢，几乎碰不到；
// Go 版启动快，稳定复现（`--dump` 第 4 行 Enable_EQ_GFX 约 2/20 次为空）。
//
// 修法：初始化完成后先做一次注定被丢弃的读，把驱动内部那条懒初始化路径
// 走通，之后所有读取都稳定。两条交付线用同一策略，保证输出逐行一致。
func (d *Driver) warmUpLocked() {
	if d.propCtrl == nil || d.devPtr == 0 {
		return
	}
	nm, err := syscall.UTF16PtrFromString("Enable_EQ_GFX")
	if err != nil {
		return
	}
	buf := make([]byte, 1024)
	keep := make([]byte, 1024)
	pBuf := uintptr(unsafe.Pointer(&buf[0]))
	d.propCtrl.Call(d.devPtr, uintptr(unsafe.Pointer(nm)),
		uintptr(unsafe.Pointer(&pBuf)), uintptr(unsafe.Pointer(&keep[0])), 0)
	runtime.KeepAlive(buf)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(nm)
}

func (d *Driver) pc(ep, name string, isSet bool, payload []byte) (ret int32, out []byte) {
	d.run(func() {
		ret, out = d.pcLocked(ep, name, isSet, payload)
	})
	return ret, out
}

func (d *Driver) pcLocked(ep, name string, isSet bool, payload []byte) (int32, []byte) {
	// IKF_PC_TRACE=1 时把每次 PropertyControl 调用打到 stderr。
	// 为「--serve 偶发崩溃（AV 发生在 PropertyControl 内部，见 MEMORY.md）」留的
	// 诊断开关：崩溃时最后一行就是肇事属性。平时零开销（一个布尔判断），
	// 根因定位完成后再删。
	if pcTrace {
		fmt.Fprintf(os.Stderr, "PC %s %s set=%v\n", ep, name, isSet)
	}
	devPtr, ok := d.ptrFor(ep)
	if !ok {
		return -9, nil
	}
	buf := make([]byte, 1024)
	if len(payload) > 0 {
		copy(buf, payload)
	}
	pBuf := uintptr(unsafe.Pointer(&buf[0]))
	ppBuf := uintptr(unsafe.Pointer(&pBuf))

	// ---- pValue 约定：按属性所属的表二选一 ----
	// ★ 第 52 轮：`<属性名>#<声道索引>` 先拆成真名 + 索引（没带 "#" 仍走 -1）。
	baseName, chIdx := splitChannel(name)
	var pVal uintptr
	var keepGen []byte
	var ci *int32
	var outer *uintptr
	if genericSet[baseName] {
		keepGen = make([]byte, 1024)
		pVal = uintptr(unsafe.Pointer(&keepGen[0]))
	} else {
		ci = new(int32)
		*ci = int32(chIdx) // -1 = 主声道
		outer = new(uintptr)
		*outer = uintptr(unsafe.Pointer(ci))
		pVal = uintptr(unsafe.Pointer(outer))
	}

	namePtr, err := syscall.UTF16PtrFromString(baseName)
	if err != nil {
		return -1, nil
	}
	b := uintptr(0)
	if isSet {
		b = 1
	}
	r, _, _ := d.propCtrl.Call(devPtr, uintptr(unsafe.Pointer(namePtr)), ppBuf, pVal, b)

	runtime.KeepAlive(buf)
	runtime.KeepAlive(keepGen)
	runtime.KeepAlive(ci)
	runtime.KeepAlive(outer)
	runtime.KeepAlive(namePtr)

	out := make([]byte, len(buf))
	copy(out, buf)
	return int32(r), out
}

// Read 返回 (ret, 字符串值)。ret: 0 成功 / 2 不支持 / -1 未找到 / -2 需特殊参数
func (d *Driver) Read(name string) (ret int, val string) {
	return d.ReadEp(EpRender, name)
}

// ReadEp 在指定端点上读。ep = "render" / "capture"
// ★ 健壮性审计 #4（2026-10-31）：原来的调用侧 ptrFor 预检已删 —— 它在
// HTTP goroutine 上裸读 d.ready/capPtr，与工作线程上的 Open/Close 构成
// 数据竞争。pcLocked 在工作线程上会复检并同样返回 -9，行为不变。
func (d *Driver) ReadEp(ep, name string) (ret int, val string) {
	if hazard[name] {
		return -2, ""
	}
	if slowConst[name] {
		return d.readConst(ep, name)
	}
	return d.readEpUncached(ep, name)
}

// readConst 查 slowConst 的缓存；没有就真读一次，读成功才记。
// 锁只护 map 本身 —— 真读走 pc 的串行化，不会并发。
func (d *Driver) readConst(ep, name string) (int, string) {
	key := ep + "\x00" + name
	d.constMu.Lock()
	e, hit := d.constCache[key]
	d.constMu.Unlock()
	if hit {
		return e.ret, e.val
	}
	ret, val := d.readEpUncached(ep, name)
	if ret == 0 {
		d.constMu.Lock()
		if d.constCache == nil {
			d.constCache = make(map[string]constEntry, 4)
		}
		d.constCache[key] = constEntry{ret, val}
		d.constMu.Unlock()
	}
	return ret, val
}

func (d *Driver) readEpUncached(ep, name string) (ret int, val string) {
	defer func() {
		if rec := recover(); rec != nil {
			ret, val = -2, ""
		}
	}()
	r, raw := d.pc(ep, name, false, nil)
	return int(r), u16(raw)
}

// Write 按白名单裁剪后写入。返回结果字典。
func (d *Driver) Write(name string, value interface{}) map[string]interface{} {
	return d.WriteEp(EpRender, name, value)
}

// WriteEp 在指定端点上写。
// ★ 健壮性审计 #4：调用侧 ptrFor 预检已删（理由同 ReadEp）—— 端点缺失
// 由 pcLocked 复检返回 -9，下方统一转成原样的友好文案。
func (d *Driver) WriteEp(ep, name string, value interface{}) map[string]interface{} {
	sp, ok := spec(ep, name)
	if !ok {
		return map[string]interface{}{"ok": false, "error": "当前设备不支持该设置", "ret": -1}
	}
	if hazard[name] {
		return map[string]interface{}{"ok": false, "error": "该参数需要特殊参数结构", "ret": -2}
	}

	var payload []byte
	var target string
	switch sp.kind {
	case TBool:
		on := false
		switch v := value.(type) {
		case string:
			s := strings.ToLower(strings.TrimSpace(v))
			switch s {
			case "1", "true", "on", "yes":
				on = true
			case "0", "false", "off", "no", "":
				on = false
			default:
				// ★ 健壮性审计 #7：原来未识别的串静默当「关」写 0 ——
				// 传错值的人看到的是成功。改成与 TInt/TFloat 同口径报错。
				return map[string]interface{}{"ok": false, "ret": -1,
					"error": fmt.Sprintf("布尔值不合法：%v", value)}
			}
		case bool:
			on = v
		default:
			// JSON 解出来是 float64；内部调用可能是 int / int32 …
			f, err := toFloat(value)
			if err != nil {
				return map[string]interface{}{"ok": false, "ret": -1,
					"error": fmt.Sprintf("布尔值不合法：%v", value)}
			}
			on = f != 0
		}
		n := int32(0)
		if on {
			n = 1
		}
		payload = le32(uint32(n))
		target = strconv.Itoa(int(n))
	case TFloat:
		f, err := toFloat(value)
		if err != nil {
			return map[string]interface{}{"ok": false, "error": fmt.Sprintf("值不合法：%v", value)}
		}
		if sp.hasRange {
			f = math.Max(sp.lo, math.Min(sp.hi, f))
		}
		payload = le32(math.Float32bits(float32(f)))
		target = fmt.Sprintf("%f", f)
	default: // TInt
		f, err := toFloat(value)
		if err != nil {
			return map[string]interface{}{"ok": false, "error": fmt.Sprintf("值不合法：%v", value)}
		}
		v := int(math.Round(f))
		if sp.hasRange {
			if float64(v) < sp.lo {
				v = int(sp.lo)
			}
			if float64(v) > sp.hi {
				v = int(sp.hi)
			}
		}
		// 离散取值：驱动对非法值一律返回 0x10003，先在应用层收口
		if len(sp.choices) > 0 {
			hit := false
			for _, c := range sp.choices {
				if c == v {
					hit = true
					break
				}
			}
			if !hit {
				return map[string]interface{}{
					"ok": false, "ret": -1,
					"error": fmt.Sprintf("只接受 %v", sp.choices),
				}
			}
		}
		payload = le32(uint32(int32(v)))
		target = strconv.Itoa(v)
	}

	// 值未变化时视为成功，不打扰驱动。
	// 实测 MuteControl 重复写同值会返回 0x10003，但语义上这是一次成功的无操作。
	// ★ 健壮性审计 #8：TFloat 原来拿 "%f" 生成的 target（"0.500000"）与驱动
	// 回读原文（"0.5"）做字符串比较，永远不等 → 滑杆每次都白打一轮写+读回。
	// 浮点改按数值比较（valueUnchanged），其余仍按原文比较。
	if !noopSkip[name] {
		if r0, cur := d.ReadEp(ep, name); r0 == 0 && valueUnchanged(cur, target, sp.kind) {
			return map[string]interface{}{"ok": true, "ret": 0, "value": cur, "read_ret": 0, "noop": true}
		}
	}

	ret := int32(-3)
	func() {
		defer func() {
			if recover() != nil {
				ret = -3
			}
		}()
		ret, _ = d.pc(ep, name, true, payload)
	}()
	// ★ 健壮性审计 #6 说明：这个 recover 只拦得住 Go 层 panic。DLL 内部的
	// 访问违例会直接终止整个进程（见本文件 hazard 段注释），根本走不到
	// ret=-3 —— 文案里的「访问异常」只对理论上的 Go panic 成立，保留仅为
	// 契约稳定，别把它当成安全网。
	if ret == -9 {
		return map[string]interface{}{"ok": false, "error": "耳机未连接或驱动未就绪", "ret": -9}
	}
	if ret == -3 {
		return map[string]interface{}{"ok": false, "error": "写入触发访问异常", "ret": -3}
	}

	// 驱动写入是异步生效的：不等一下就读回会拿到旧值。
	// 与 center_src.py 的 time.sleep(0.02) 保持一致。
	time.Sleep(20 * time.Millisecond)

	r2, back := d.ReadEp(ep, name)
	return map[string]interface{}{
		"ok": ret == 0 && r2 == 0, "ret": int(ret),
		"value": back, "read_ret": r2,
	}
}

// valueUnchanged noop 跳过的「值未变」判定（健壮性审计 #8）：
// 字符串相等直接过；浮点再按数值比一次（驱动回读格式与 %f 不一致）。
func valueUnchanged(cur, target string, kind int) bool {
	if cur == target {
		return true
	}
	if kind != TFloat {
		return false
	}
	a, ea := strconv.ParseFloat(strings.TrimSpace(cur), 64)
	b, eb := strconv.ParseFloat(strings.TrimSpace(target), 64)
	return ea == nil && eb == nil && a == b
}

// WriteAuto 先走白名单；未命中但属于 208 项时改走 WriteRaw。
//
// 口径（用户要求「官方内容零遗漏，功能能不能用交给硬件匹配」）：
// 官方面板里有、但新界面没登记成可写控件的属性（例如音箱方位的
// SS_LF_ANGLE / SS_RF_ANGLE 一族），也必须真写一次并把驱动的真实返回码
// 报出来，而不是在前端直接短路成「本机不支持」。本机这些属性返回 2
// （存在但从未写过），那是硬件给的结论，不是我们替它下的。
//
// kind 为空时按 JSON 解出来的值类型推断（true/false → bool、整数 → int、
// 其余 → float），前端也可以显式传 kind 覆盖。
func (d *Driver) WriteAuto(ep, name string, value interface{}, kind string) map[string]interface{} {
	if _, ok := spec(ep, name); ok {
		return d.WriteEp(ep, name, value)
	}
	if kind == "" {
		kind = inferKind(value)
	}
	return d.WriteRaw(ep, name, value, kind)
}

// inferKind 按值的实际类型猜打包方式。只用于白名单之外的属性，
// 猜错也只会让驱动回一个失败码，不会写坏别的参数。
func inferKind(v interface{}) string {
	switch t := v.(type) {
	case bool:
		return "bool"
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "false", "on", "off", "yes", "no":
			return "bool"
		}
		return "int"
	default:
		if f, err := toFloat(v); err == nil {
			if f == math.Trunc(f) {
				return "int"
			}
			return "float"
		}
	}
	return "int"
}

// WriteRaw 白名单之外、但确实是 208 项里的属性，也真写一次并如实回报驱动返回码。
//
// 用途：回放官方面板导出的组态。官方档里会出现 DefaultDeviceControl
// 这类我们未接入界面的属性（FactoryDefault.xml 就只有它）。把它按
// 「未开放写入」跳掉，等于把官方组态做成了空操作 —— 那是遗漏。
//
// 这里不做范围钳制（钳制只针对界面控件），值一律按 kind 打包，
// 写不写得进由驱动自己决定，我们只如实回报。
//
// hazard 三项必须显式跳过：它们会解引用 pValue，触发访问违例，
// 而 Go 的 syscall 不做 ctypes 那层转换，进程会被直接杀掉。
// Python 侧同样跳过，保证两条交付线的写回结果逐项一致。
func (d *Driver) WriteRaw(ep, name string, value interface{}, kind string) map[string]interface{} {
	if hazard[name] {
		return map[string]interface{}{"ok": false, "ret": -2,
			"error": "该参数需要特殊参数结构"}
	}
	if !knownName[name] {
		return map[string]interface{}{"ok": false, "ret": -1, "error": "未知属性名"}
	}
	if _, ok := d.ptrFor(ep); !ok {
		return map[string]interface{}{"ok": false, "ret": -9, "error": "耳机未连接或驱动未就绪"}
	}

	var payload []byte
	switch kind {
	case "float":
		f, err := toFloat(value)
		if err != nil {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": fmt.Sprintf("值不合法：%v", value)}
		}
		payload = le32(math.Float32bits(float32(f)))
	case "bool":
		f, err := toFloat(value)
		if err != nil {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": fmt.Sprintf("值不合法：%v", value)}
		}
		n := int32(0)
		if f != 0 {
			n = 1
		}
		payload = le32(uint32(n))
	default:
		f, err := toFloat(value)
		if err != nil {
			return map[string]interface{}{"ok": false, "ret": -1,
				"error": fmt.Sprintf("值不合法：%v", value)}
		}
		payload = le32(uint32(int32(int(math.Round(f)))))
	}

	ret := int32(-3)
	func() {
		defer func() {
			if recover() != nil {
				ret = -3
			}
		}()
		ret, _ = d.pc(ep, name, true, payload)
	}()
	if ret == -3 {
		return map[string]interface{}{"ok": false, "ret": -3,
			"error": "写入触发访问异常"}
	}
	time.Sleep(20 * time.Millisecond)
	r2, back := d.ReadEp(ep, name)
	return map[string]interface{}{
		"ok": ret == 0 && r2 == 0, "ret": int(ret),
		"value": back, "read_ret": r2,
	}
}

// ---------------------------------------------------------------- 函数型导出
//
// osConfLib.dll 的 15 个导出里有 6 个不是「属性」而是直接调用，
// 原面板用它们做声道测试与麦克风监听，早期实现完全没接。
// 这些调用同样必须钉在工作线程上。

// PlayTest 声道测试。mask 取自原面板 apoSpeakerSetting.cml 的
// PlayTestSound Tag 全集：LF=1 RF=2 C=4 W=8 LB=16 RB=32 LS=512 RS=1024。
//
// ★ 第 28 轮修正。反汇编确认签名是 PlayTestSound(LPWSTR wavPath, int mode,
// void* pInfo)：
//
//	wavPath  测试音 WAV 的路径。**不是设备上下文** —— 它直接进
//	         mmioOpenW + CreateFileW（MSDN 的 WaveOpenFile 示例），
//	         拿不到文件句柄就 return 0x10001，一个字节都不往下走。
//	         这正是「声道测试永远不可用」的真正原因：以前这里传的是
//	         设备上下文指针，被当成宽字符串去开文件，必然失败。
//	mode     声道掩码。
//	pInfo    指向 3 个 dword：[+0] 链表内索引 [+8] 链表选择（0=渲染 1=采集）。
//	         早期只传 2 个参数，第三个寄存器 R8 是垃圾值，函数读 [R8]
//	         直接触发访问违例（Go 的 syscall 会让整个进程被硬杀，
//	         recover 拦不住）。
func (d *Driver) PlayTest(mask int) map[string]interface{} {
	if !d.ready {
		return map[string]interface{}{"ok": false, "error": "耳机未连接或驱动未就绪", "ret": -9}
	}
	if d.playTest == nil || d.playTest.Find() != nil {
		return map[string]interface{}{"ok": false, "error": "当前驱动不支持声道测试", "ret": -1}
	}
	wav := d.testWav(mask)
	if wav == "" {
		return map[string]interface{}{"ok": false, "ret": -2, "mask": mask,
			"error": "未知的声道掩码"}
	}
	if _, err := os.Stat(wav); err != nil {
		return map[string]interface{}{"ok": false, "ret": -4, "mask": mask,
			"error": "找不到测试音资源（" + wav + "）"}
	}
	path, err := syscall.UTF16PtrFromString(wav)
	if err != nil {
		return map[string]interface{}{"ok": false, "ret": -4, "mask": mask,
			"error": "测试音路径无法转换"}
	}
	// 结构体必须活到调用结束之后，用切片持有避免被 GC 提前回收
	info := [3]int32{d.devIdx, 0, d.devList}
	ret := int32(-9)
	func() {
		defer func() {
			if recover() != nil {
				ret = -3
			}
		}()
		d.run(func() {
			r, _, _ := d.playTest.Call(uintptr(unsafe.Pointer(path)),
				uintptr(int32(mask)), uintptr(unsafe.Pointer(&info[0])))
			runtime.KeepAlive(path)
			runtime.KeepAlive(info)
			ret = int32(r)
		})
	}()
	if ret == -3 {
		return map[string]interface{}{"ok": false, "error": "声道测试调用异常", "ret": -3}
	}
	if ret != 0 {
		return map[string]interface{}{"ok": false, "ret": int(ret), "mask": mask,
			"error": fmt.Sprintf("驱动拒绝了这次调用（0x%X）", uint32(ret))}
	}
	return map[string]interface{}{"ok": true, "ret": 0, "mask": mask}
}

// testProc 是 StopTestSound / CloseTestSound 的公共实现。
// 反汇编确认二者**忽略全部入参**（用模块内部单例），恒返回 0，
// 因此不能传设备上下文——多传一个参数会让被调用方按 cdecl 清栈时错位。
func (d *Driver) testProc(p *syscall.LazyProc, name, what string) map[string]interface{} {
	if !d.ready {
		return map[string]interface{}{"ok": false, "error": "耳机未连接或驱动未就绪", "ret": -9}
	}
	if p == nil || p.Find() != nil {
		return map[string]interface{}{"ok": false,
			"error": "当前驱动不支持该功能", "ret": -1}
	}
	ret := int32(-9)
	func() {
		defer func() {
			if recover() != nil {
				ret = -3
			}
		}()
		d.run(func() {
			r, _, _ := p.Call()
			ret = int32(r)
		})
	}()
	if ret == -3 {
		return map[string]interface{}{"ok": false, "error": what + "调用异常", "ret": -3}
	}
	return map[string]interface{}{"ok": ret == 0, "ret": int(ret)}
}

func (d *Driver) StopTest() map[string]interface{} {
	return d.testProc(d.stopTest, "StopTestSound", "停止测试")
}

func (d *Driver) CloseTest() map[string]interface{} {
	return d.testProc(d.closeTest, "CloseTestSound", "关闭测试")
}

// Listen 麦克风监听：把采集端点的信号送到默认输出装置。
// 驱动只对录音类设备放行（pDevice+4 ∈ {2,3,5,0xA}），故打在采集端点。
// ShowAsio 原面板 MainWindow 的 BtnASIO。CM108B 无 ASIO，预期返回 -1。
func (d *Driver) ShowAsio() map[string]interface{} {
	if !d.ready {
		return map[string]interface{}{"ok": false, "error": "耳机未连接或驱动未就绪", "ret": -9}
	}
	if d.showAsio == nil || d.showAsio.Find() != nil {
		return map[string]interface{}{"ok": false, "error": "当前驱动不支持 ASIO", "ret": -1}
	}
	ret := int32(-9)
	func() {
		defer func() {
			if recover() != nil {
				ret = -3
			}
		}()
		d.run(func() {
			r, _, _ := d.showAsio.Call(d.devPtr)
			ret = int32(r)
		})
	}()
	if ret == -3 {
		return map[string]interface{}{"ok": false, "error": "ASIO 面板调用异常", "ret": -3}
	}
	return map[string]interface{}{"ok": ret == 0, "ret": int(ret)}
}

// ParamMapEp 指定端点的全量参数表。w 只在该端点确实可写时为 true。
func (d *Driver) ParamMapEp(ep string) map[string]map[string]interface{} {
	out := make(map[string]map[string]interface{}, len(allNames))
	for _, n := range allNames {
		ret, val := d.ReadEp(ep, n)
		sp, w := spec(ep, n)
		t := guessType(val)
		if w {
			switch sp.kind {
			case TBool:
				t = "bool"
			case TFloat:
				t = "float"
			default:
				t = "int"
			}
		}
		out[n] = map[string]interface{}{"r": ret, "v": val, "w": w, "t": t}
	}
	// ★ 第 52 轮：渲染端点额外补上逐声道音量（hNames 里带 "#" 的虚拟参数）。
	// 它们不是 208 项真实属性，所以不进 allNames；但界面滑杆按统一路径取值，
	// 必须出现在这张表里。与 Python 侧 param_map 逐项一致。
	if ep == EpRender {
		for n := range channelParams {
			ret, val := d.ReadEp(ep, n)
			sp, w := spec(ep, n)
			t := guessType(val)
			if w {
				switch sp.kind {
				case TBool:
					t = "bool"
				case TFloat:
					t = "float"
				default:
					t = "int"
				}
			}
			out[n] = map[string]interface{}{"r": ret, "v": val, "w": w, "t": t}
		}
	}
	return out
}

func (d *Driver) Status() map[string]interface{} {
	if !d.ready {
		msg := d.errMsg
		if msg == "" {
			msg = "耳机未连接或驱动未就绪"
		}
		return map[string]interface{}{"ok": false, "error": msg}
	}
	g := func(n string) string { _, v := d.Read(n); return v }
	num := func(n string, def float64) float64 {
		r, v := d.Read(n)
		if r != 0 || v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return def
		}
		return f
	}

	eq := make([]int, 0, 10)
	for i := 1; i <= 10; i++ {
		r, v := d.Read(fmt.Sprintf("EQ_Slider%d", i))
		n := 0
		if r == 0 {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				n = int(f)
			}
		}
		eq = append(eq, n)
	}
	var eqOn interface{}
	if r, v := d.Read("Enable_EQ_GFX"); r == 0 {
		eqOn = v == "1"
	}
	mute := func() bool { _, v := d.Read("MuteControl"); return v == "1" }()

	ch := int(num("DevChannelNumControl", 0))
	rate := num("DevSampleRateControl", 0)
	bits := int(num("DevBitDepthControl", 0))

	// 与 Python 版逐字符对齐：`ch or "?"` 的语义是「0 → ?」，
	// 而 kHz 分支里 Python 直接用了原始 int（0 会显示成 "0"），这里也照搬。
	chTxt, rateTxt, bitsTxt := "?", "?", "?"
	if ch != 0 {
		chTxt = strconv.Itoa(ch)
	}
	if rate != 0 {
		rateTxt = fmt.Sprintf("%g", rate)
	}
	if bits != 0 {
		bitsTxt = strconv.Itoa(bits)
	}
	formats := fmt.Sprintf("%s 声道 / %s Hz / %s bit", chTxt, rateTxt, bitsTxt)
	rateText := rateTxt + " Hz"
	if rate >= 1000 {
		rateText = fmt.Sprintf("%g kHz", rate/1000.0)
		formats = fmt.Sprintf("%s 声道 / %s / %s bit", strconv.Itoa(ch), rateText, strconv.Itoa(bits))
	}

	orDefault := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	// numOrNil 对应 Python 的 `num(name, float, None)`：读不到就是 null，
	// 前端据此判断「本机未开放该参数」（vol_scalar === null → 旋钮置灰）。
	numOrNil := func(n string) interface{} {
		r, v := d.Read(n)
		if r != 0 || v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		return f
	}

	// micNum 对应 Python 版的 mic_num()：麦克风侧读数必须走采集端点，
	// 走渲染端点一律返回 2（不支持）。
	micNum := func(n string) interface{} {
		if !d.capReady {
			return nil
		}
		r, v := d.ReadEp(EpCapture, n)
		if r != 0 || v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		return f
	}

	return map[string]interface{}{
		"ok":            true,
		"model":         orDefault(g("GetDeviceFriendlyName"), "未知设备"),
		"codec":         orDefault(g("GetAudioCodecName"), "—"),
		"driver":        orDefault(g("GetDriverVer"), "—"),
		"firmware":      orDefault(g("GetFirmwareVer"), "—"),
		"hwid":          orDefault(g("GetDeviceID"), "—"),
		"dx":            orDefault(g("GetDirectXVer"), "—"),
		"formats":       formats,
		"channels":      ch,
		"rate_text":     rateText,
		"bit_depth":     bits,
		"cpl":           d.cpl,
		"dll":           d.dll,
		"device_count":  d.devCnt,
		"capture_count": d.capCnt,
		"eq":            eq,
		"eq_on":         eqOn,
		"mute":          mute,
		"room":          int(num("RFX_ROOMSIZE", 0)),
		"env":           int(num("RFX_ENVIRONMENT", 0)),
		"vol_scalar":    numOrNil("VolumeScalarControl"),
		"vol_db":        numOrNil("VolumeControl"),
		"vol_min":       num("GetMinVol", -37.0),
		"vol_max":       num("GetMaxVol", 0.0),
		"vol_step":      num("GetVolStep", 1.0),
		"environments":  environments,
		"room_sizes":    roomSizes,
		"magic_voices":  magicVoices,
		"test_channels": func() [][]interface{} {
			out := make([][]interface{}, 0, len(testChannels))
			for _, tc := range testChannels {
				// 四个字段都给前端：第四项是测试音 wav 文件名，界面把它
				// 挂在 chip 的 tooltip 上 —— 以后有人发现某档不响，
				// 至少能在界面上直接看到它在找哪个文件。
				out = append(out, []interface{}{tc[0], tc[1], tc[2], tc[3]})
			}
			return out
		}(),
		// ---- 麦克风侧（采集端点）----
		"mic_ready":      d.capReady,
		"mic_vol_scalar": micNum("VolumeScalarControl"),
		"mic_vol_db":     micNum("VolumeControl"),
		"mic_mute":       micNum("MuteControl"),
		"mic_nr_on":      micNum("Enable_NOISEREDUCTION_LFX"),
		"mic_nr_level":   micNum("NoiseReduction_Level"),
		"mic_magic_on":   micNum("Enable_MAGICVOICE"),
		"mic_magic_sel":  micNum("MagicVoice_Selection"),
		"mic_echo_on":    micNum("Enable_MICECHO"),
		"mic_echo_level": micNum("MICECHO_Level"),
		"mic_agc":        micNum("AGCControl"),
		"mic_aa_vol":     micNum("AAVolumeControl"),
		"mic_rate":       micNum("DevSampleRateControl"),
		"dev_rate":       rate,
	}
}

// Meter 实时电平。渲染端点 8 声道，采集端点 1 声道。
func (d *Driver) Meter(ep string) map[string]interface{} {
	if _, ok := d.ptrFor(ep); !ok {
		return map[string]interface{}{"ok": false, "ep": ep}
	}
	r, v := d.ReadEp(ep, "GetPeakMeter")
	peak := 0.0
	if r == 0 {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			peak = f
		}
	}
	_, mc := d.ReadEp(ep, "GetMeterChannelCount")
	if mc == "" {
		mc = "?"
	}
	return map[string]interface{}{"ok": r == 0, "peak": peak, "ep": ep, "channels": mc}
}

// ---------------------------------------------------------------- 小工具

func le32(u uint32) []byte {
	return []byte{byte(u), byte(u >> 8), byte(u >> 16), byte(u >> 24)}
}

func toFloat(v interface{}) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(t), 64)
	}
	return 0, fmt.Errorf("不支持的类型")
}
