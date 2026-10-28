// iKFTool —— iKF Audio Center 日志诊断工具（独立 CLI，不碰驱动/不占单实例）。
//
// 职责划分（2026-10-25 用户定）：驱动管理=装/卸驱动，iKFTool=日志诊断，
// 主程序=运行使用。本工具全程**只读**（注册表读 / wevtutil / 文件哈希），
// 主程序开着也能跑。
//
//	用法：iKFTool.exe [-zip] [-arm | -arm-off] [额外场景目录…]
//	  （缺省扫描 = 自动发现：exe 旁 CPL/profile、CPL/profile/default、
//	    %APPDATA% 档；位置参数是额外场景目录）
//
// 报告六段：运行环境（系统/WebView2）→ 包完整性（SHA256）→ 进程与日志现状
// → 崩溃取证（事件日志/WER/panic 行/转储）→ 场景诊断 → 交叉检查。
// 另有两个动作模式（只做动作不跑诊断）：
//	  -zip      把 log\ 按天日志 + 驱动管理日志 + 崩溃转储打成回传 zip
//	  -arm      给主程序/驱动管理布防 WER 本地崩溃转储（HKCU，免提权）
//	  -arm-off  撤销布防
//
// 输出：控制台全文 + 追加进 exe 旁 log\yyyy-mm-dd.txt（★2026-10-25 用户
// 要求 log 目录只保留主程序的按天日志，不再单独写诊断报告文件）。
//
// ★ 注意：builtinScenes 名单与 model/device-model.toml 的 [scene] builtin
//   人工同步 —— 主程序那边是 go:embed 真源，这里只做分类展示。
package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var reXmlTag = regexp.MustCompile(`(?i)<(/?)(Profile|Endpoint|ApoEffect|Property)\b([^>]*?)(/?)>`)

func cmlAttr(text, attr string) string {
	re := regexp.MustCompile(fmt.Sprintf(`\b%s\s*=\s*"([^"]*)"`, regexp.QuoteMeta(attr)))
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// profileItem 场景档里的一条参数（Property 节点）。
type profileItem struct {
	Endpoint, Flow, Effect, Name, Value, ValueType string
}

// profileItems 解析 .profile/.cml 文本为参数清单（与主程序 profile.go 同源逻辑）。
func profileItems(text string) []profileItem {
	var items []profileItem
	epName, flow, fx := "", "", ""
	for _, m := range reXmlTag.FindAllStringSubmatch(text, -1) {
		closing, tag, attrs := m[1], strings.ToLower(m[2]), m[3]
		switch tag {
		case "endpoint":
			if closing != "" {
				epName, flow = "", ""
			} else {
				epName = cmlAttr(attrs, "Name")
				flow = cmlAttr(attrs, "DataFlow")
			}
		case "apoeffect":
			if closing != "" {
				fx = ""
			} else {
				fx = cmlAttr(attrs, "Name")
			}
		case "property":
			if closing == "" {
				items = append(items, profileItem{
					Endpoint: epName, Flow: flow, Effect: fx,
					Name:      cmlAttr(attrs, "Name"),
					Value:     cmlAttr(attrs, "Value"),
					ValueType: cmlAttr(attrs, "ValueType"),
				})
			}
		}
	}
	return items
}

// builtinScenes 内置场景名单。★与 model/device-model.toml [scene].builtin
// 人工同步（2026-10-24 版：游戏已移出，出厂默认 = 默认/音乐/电影）。
var builtinScenes = map[string]bool{
	"默认": true, "音乐": true, "电影": true,
	"UserDefine": true, "UserDefineFormwork": true, "HiFi": true,
}

// expectFactory 出厂默认应有的播种场景（缺失会在报告里警告）。
var expectFactory = []string{"默认", "音乐", "电影"}

// expectNormal 应存在的普通场景（缺失会在报告里警告）。
var expectNormal = []string{"游戏"}

// sceneExclude / sceneUnproven 的名字级子集：报告里对混入参数打 ⚠。
// （完整判据在主程序 profile.go；这里列最常见的。）
func isSuspiciousParam(name string) bool {
	if strings.Contains(name, "#") {
		return true
	}
	switch name {
	case "MuteControl", "VolumeScalarControl", "DevSampleRateControl",
		"Speaker_Channel_Setting", "EnableTopology":
		return true
	}
	return false
}

type sceneFile struct {
	name string    // 场景名（去扩展名）
	path string    // 完整路径
	dir  string    // 来源目录
	bdir bool      // 目录是否驱动自带（CPL）
	item []profileItem
	err  string
	fi   os.FileInfo
}

// ---------------- 诊断新段（2026-10-25，定位=日志诊断） ----------------

// runCmd 跑一条外部命令（超时兜底），stdout+stderr 合并返回。
func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func regStr(root registry.Key, path, name string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

func regDword(root registry.Key, path, name string) (uint32, bool) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	return uint32(v), err == nil
}

const wv2GUID = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// secRuntime 运行环境：直接回答「什么系统、能不能起来窗口」。
// B 线首问：家庭版？24H2？WebView2 缺失是 Wails 程序白屏/起不来的经典原因。
func secRuntime(w func(string, ...interface{})) {
	w("")
	w("──────── 运行环境 ────────")
	cv := `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	build := regStr(registry.LOCAL_MACHINE, cv, "CurrentBuildNumber")
	ubr, _ := regDword(registry.LOCAL_MACHINE, cv, "UBR")
	prod := regStr(registry.LOCAL_MACHINE, cv, "ProductName")
	// 注册表 ProductName 在 Win11 上仍写 "Windows 10"（微软未更新），
	// build ≥ 22000 即 Win11 —— 不纠正会误导「是不是 Win11」的判断。
	if n, err := strconv.Atoi(build); err == nil && n >= 22000 {
		prod = strings.Replace(prod, "Windows 10", "Windows 11", 1)
	}
	w("系统 : %s %s（EditionID=%s，build %s.%d）",
		prod,
		regStr(registry.LOCAL_MACHINE, cv, "DisplayVersion"),
		regStr(registry.LOCAL_MACHINE, cv, "EditionID"), build, ubr)
	locale := regStr(registry.CURRENT_USER, `Control Panel\International`, "LocaleName")
	if locale == "" {
		locale = regStr(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Nls\Language`, "InstallLanguage")
	}
	w("区域 : %s   进程架构 : %s   管理员 : %v",
		locale, runtime.GOARCH, windows.GetCurrentProcessToken().IsElevated())
	wv2 := regStr(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\`+wv2GUID, "pv")
	if wv2 == "" {
		wv2 = regStr(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\EdgeUpdate\Clients\`+wv2GUID, "pv")
	}
	if wv2 == "" {
		wv2 = regStr(registry.CURRENT_USER, `Software\Microsoft\EdgeUpdate\Clients\`+wv2GUID, "pv")
	}
	if wv2 == "" {
		w("WebView2 : ⚠ 注册表未找到运行时 —— 主程序窗口会白屏/起不来")
	} else {
		w("WebView2 : %s", wv2)
	}
	// ★已安装杀软（root\SecurityCenter2）：静默终止/注入型 fail-fast 的
	// 头号嫌疑就是杀软（2026-10-25 远端模式：启动正常→2-3 分钟→无声死亡），
	// 这个字段必须自动带出来。productState 原样记录，由人工判读开关状态。
	if out, err := runCmd(30*time.Second, "powershell", "-NoProfile", "-Command",
		"Get-CimInstance -Namespace root/SecurityCenter2 -Class AntiVirusProduct | ForEach-Object { $_.displayName + '  productState=0x' + $_.productState.ToString('X6') }"); err == nil && strings.TrimSpace(out) != "" {
		w("杀软 :")
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(l) != "" {
				w("  %s", strings.TrimSpace(l))
			}
		}
	} else {
		w("杀软 : 查询不到（SecurityCenter2 无记录、被精简或 PowerShell 受限）")
	}
}

// integrityFiles 包完整性要查的文件（相对 exe 目录）。
var integrityFiles = []string{
	"iKF-Audio-Center.exe",
	"ikf_audio.dll",
	"iKF-驱动管理.exe",
	"iKFTool.exe",
	`CPL\Driver\x64\vista\osConfLib.dll`,
}

// secIntegrity 包完整性：大小 + mtime + SHA256。直接回答「传输有没有截断、
// exe 与 dll 是不是一对」（10-23 远端排障时靠对方手动 certutil 对哈希）。
func secIntegrity(w func(string, ...interface{}), exe string) {
	w("")
	w("──────── 包完整性（SHA256）────────")
	for _, rel := range integrityFiles {
		p := filepath.Join(exe, filepath.FromSlash(rel))
		fi, err := os.Stat(p)
		if err != nil {
			w("⚠ %-24s 缺失（绿色包拆散/没拷全）", rel)
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			w("⚠ %-24s 不可读：%v", rel, err)
			continue
		}
		sum := sha256.Sum256(b)
		w("%-24s %9d 字节  %s  %s", rel, fi.Size(),
			hex.EncodeToString(sum[:16])+"…", fi.ModTime().Format("2006-01-02 15:04"))
	}
}

// avProcs 国产杀软/管家的特征进程表。★SecurityCenter2 不可靠：本机实测
// 火绒在跑（HipsDaemon/HipsTray）但 SecurityCenter2 只报 Defender —— 所以
// 进程级探测才是真源（2026-10-25 教训）。增量补常见厂商即可。
var avProcs = []struct{ proc, name string }{
	{"hipsdaemon.exe", "火绒"}, {"hipstray.exe", "火绒"}, {"usysdiag.exe", "火绒"},
	{"360safe.exe", "360"}, {"360tray.exe", "360"}, {"360sd.exe", "360"},
	{"360rp.exe", "360"}, {"zhudongfanghu.exe", "360主动防御"},
	{"qqpcrtp.exe", "腾讯管家"}, {"qqpctray.exe", "腾讯管家"},
	{"kxetray.exe", "金山毒霸"}, {"kwsprotect64.exe", "金山"},
	{"baidusd.exe", "百度杀毒"}, {"2345safe.exe", "2345管家"},
	{"wsctrl.exe", "火绒EDR"}, {"edrsvc.exe", "火绒EDR"},
	{"msmpeng.exe", "Windows Defender"},
}

// secProcsLogs 进程与日志现状。
func secProcsLogs(w func(string, ...interface{}), exe string) {
	w("")
	w("──────── 进程与日志现状 ────────")
	running := map[string]bool{}
	if snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0); err == nil {
		var pe windows.ProcessEntry32
		pe.Size = uint32(unsafe.Sizeof(pe))
		if windows.Process32First(snap, &pe) == nil {
			for {
				running[strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))] = true
				if windows.Process32Next(snap, &pe) != nil {
					break
				}
			}
		}
		windows.CloseHandle(snap)
	}
	var avs []string
	for _, av := range avProcs {
		if running[av.proc] {
			avs = append(avs, av.name+"("+av.proc+")")
		}
	}
	if len(avs) > 0 {
		w("杀软进程 : %s", strings.Join(avs, "、"))
	} else {
		w("杀软进程 : 未检测到已知杀软在运行")
	}
	for _, name := range []string{"iKF-Audio-Center.exe", "iKF-驱动管理.exe"} {
		if running[strings.ToLower(name)] {
			w("在运行 : %s", name)
		} else {
			w("未运行 : %s", name)
		}
	}
	logDir := filepath.Join(exe, "log")
	if entries, err := os.ReadDir(logDir); err != nil {
		w("⚠ log 目录不可读：%v", err)
	} else {
		w("log 目录：%d 个文件", len(entries))
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				w("  %-24s %8d 字节  %s", e.Name(), fi.Size(), fi.ModTime().Format("2006-01-02 15:04"))
			}
		}
	}
	// 驱动管理日志只在安装/卸载失败时生成：exe 旁优先，%TEMP% 兜底
	drvLog, drvLogAt := "", ""
	for _, p := range []string{filepath.Join(exe, "iKF-驱动管理.log"), filepath.Join(os.TempDir(), "iKF-驱动管理.log")} {
		if b, err := os.ReadFile(p); err == nil {
			drvLog, drvLogAt = string(b), p
			break
		}
	}
	if drvLog == "" {
		w("驱动管理日志 : 无（iKF-驱动管理.log 只在装/卸失败时生成，属正常）")
	} else {
		w("驱动管理日志 : %s（最后 30 行）", drvLogAt)
		lines := strings.Split(strings.TrimRight(drvLog, "\n"), "\n")
		if n := len(lines); n > 30 {
			lines = lines[n-30:]
		}
		for _, l := range lines {
			w("  %s", l)
		}
	}
}

// secCrash 崩溃取证：事件日志（异常代码+出错模块）→ WER 归档 → 转储目录
// → 按天日志里的 panic 行。判据（2026-10-23 定）：事件里出错模块=exe 自身
// → Go fatal；=osConfLib.dll → 原生侧；=杀软 DLL → 注入；无记录 → 加载
// 阶段就死了（代码一行没跑）。
func secCrash(w func(string, ...interface{}), exe string) {
	w("")
	w("──────── 崩溃取证 ────────")
	for _, prov := range []string{"Application Error", "Windows Error Reporting"} {
		out, err := runCmd(15*time.Second, "wevtutil", "qe", "Application",
			"/q:*[System[Provider[@Name='"+prov+"']]]", "/c:50", "/rd:true", "/f:text")
		if err != nil && strings.TrimSpace(out) == "" {
			w("  %s：无匹配事件（或查询受限）", prov)
			continue
		}
		hit, shown := 0, 0
		for _, blk := range strings.Split(out, "\nEvent[") {
			if !strings.Contains(strings.ToLower(blk), "ikf") {
				continue
			}
			hit++
			if shown < 5 {
				shown++
				w("  ── %s 事件（第 %d 条）──", prov, hit)
				for _, l := range strings.Split(strings.TrimSpace(blk), "\n") {
					if strings.TrimSpace(l) != "" {
						w("  %s", l)
					}
				}
			}
		}
		switch {
		case hit == 0:
			w("  %s：近 50 条里无 iKF 记录（负发现同样重要）", prov)
		case hit > 5:
			w("  …共 %d 条含 iKF（只展示前 5 条）", hit)
		}
	}
	werDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "Windows", "WER", "ReportArchive")
	var werHits []string
	if entries, err := os.ReadDir(werDir); err == nil {
		for _, e := range entries {
			if !strings.Contains(strings.ToLower(e.Name()), "ikf") {
				continue
			}
			if fi, err := e.Info(); err == nil {
				werHits = append(werHits, fi.ModTime().Format("2006-01-02 15:04")+"  "+e.Name())
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(werHits)))
	if len(werHits) == 0 {
		w("WER 归档 : 无 iKF 记录")
	} else {
		w("WER 归档 : %d 条，最新 5 条：", len(werHits))
		for i, h := range werHits {
			if i >= 5 {
				break
			}
			w("  %s", h)
		}
	}
	dumps := filepath.Join(os.Getenv("LOCALAPPDATA"), "iKF", "dumps")
	if entries, err := os.ReadDir(dumps); err == nil && len(entries) > 0 {
		w("崩溃转储（%s）：%d 个", dumps, len(entries))
		for _, e := range entries {
			if fi, err := e.Info(); err == nil {
				w("  %-44s %10d 字节  %s", e.Name(), fi.Size(), fi.ModTime().Format("2006-01-02 15:04"))
			}
		}
	} else {
		w("崩溃转储 : 无（用 -arm 布防后，下次崩溃会在这里留 dump）")
	}
	panicCount := 0
	var panicLast []string
	if entries, err := os.ReadDir(filepath.Join(exe, "log")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(exe, "log", e.Name()))
			if err != nil {
				continue
			}
			for _, l := range strings.Split(string(b), "\n") {
				if strings.Contains(l, "panic：") {
					panicCount++
					panicLast = append(panicLast, e.Name()+"  "+strings.TrimSpace(l))
				}
			}
		}
	}
	if panicCount == 0 {
		w("Rust panic 行 : 无")
	} else {
		w("Rust panic 行 : %d 条（来自 ikf_audio.dll 的落盘），最后几条：", panicCount)
		for i := len(panicLast) - 1; i >= 0 && i >= len(panicLast)-3; i-- {
			w("  %s", panicLast[i])
		}
	}
}

// armDumps -arm / -arm-off：布防/撤销 WER 本地转储（HKCU 免提权）。
// DumpType=1 minidump：含崩溃线程栈（fail-fast 现场够用），体积小到发得动微信；
// full dump 在 Go 进程上动辄几百 MB，没人会真发。
// ★ 2026-10-25 追加 GOTRACEBACK=crash（用户级环境变量）：Go 运行时 fatal
//   error（concurrent map / deadlocked / unexpected fault…）默认是
//   print→exit(2)，**不留任何 dump**（远端 25264 静默消失就是这么死的）；
//   设为 crash 后改走 fail-fast → WER → dump。注意 procdump -e 抓不到
//   fail-fast（int 29h 直达内核，不经过用户态异常分发），WER 是唯一通路。
func armDumps(off bool) int {
	const dumpDir = `%LOCALAPPDATA%\iKF\dumps`
	for _, name := range []string{"iKF-Audio-Center.exe", "iKF-驱动管理.exe", "iKFTool.exe"} {
		keyPath := `Software\Microsoft\Windows\Windows Error Reporting\LocalDumps\` + name
		if off {
			err := registry.DeleteKey(registry.CURRENT_USER, keyPath)
			switch {
			case err == nil:
				fmt.Printf("[arm-off] %s：已撤销\n", name)
			case errors.Is(err, registry.ErrNotExist):
				fmt.Printf("[arm-off] %s：本来就没布防\n", name)
			default:
				fmt.Printf("[arm-off] %s：撤销失败 %v\n", name, err)
			}
			continue
		}
		k, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
		if err != nil {
			fmt.Printf("[arm] %s：失败 %v\n", name, err)
			continue
		}
		_ = k.SetExpandStringValue("DumpFolder", dumpDir)
		_ = k.SetDWordValue("DumpCount", 5)
		_ = k.SetDWordValue("DumpType", 1)
		k.Close()
		fmt.Printf("[arm] %s：已布防（dump → %s，最多 5 份）\n", name, dumpDir)
	}
	// GOTRACEBACK=crash：只动我们自己设过的值，不碰用户已有的其他配置
	envKey := `Environment`
	if off {
		if k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
			if v, _, _ := k.GetStringValue("GOTRACEBACK"); v == "crash" {
				_ = k.DeleteValue("GOTRACEBACK")
				fmt.Println("[arm-off] GOTRACEBACK：已撤销")
			}
			k.Close()
		}
	} else {
		if k, _, err := registry.CreateKey(registry.CURRENT_USER, envKey, registry.SET_VALUE); err == nil {
			_ = k.SetStringValue("GOTRACEBACK", "crash")
			k.Close()
			fmt.Println("[arm] GOTRACEBACK=crash（用户级）：Go 运行时 fatal error 也会留 dump")
			broadcastEnvChange()
		}
	}
	if off {
		fmt.Println("布防已全部撤销。")
	} else {
		fmt.Println("下次程序崩溃后，把 %LOCALAPPDATA%\\iKF\\dumps 里的 .dmp 发回来即可；撤销用 iKFTool.exe -arm-off")
		fmt.Println("注意：procdump -e 抓不到 0xC0000409（fail-fast 不经过用户态异常分发），请勿再用它等崩溃。")
	}
	return 0
}

// broadcastEnvChange 通知 Explorer 刷新环境变量，双击启动的新进程即可拿到
// GOTRACEBACK（否则要重新登录）。
func broadcastEnvChange() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	p := user32.NewProc("SendMessageTimeoutW")
	ptr, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	const (
		HWND_BROADCAST   = 0xFFFF
		WM_SETTINGCHANGE = 0x001A
		SMTO_ABORTIFHUNG = 0x0002
	)
	_, _, _ = p.Call(HWND_BROADCAST, WM_SETTINGCHANGE, 0,
		uintptr(unsafe.Pointer(ptr)), SMTO_ABORTIFHUNG, 2000, 0)
}

// makeZip -zip：按天日志 + 驱动管理日志 + 崩溃转储 → 一个回传 zip（exe 旁）。
func makeZip(exe string) (string, int, error) {
	host, _ := os.Hostname()
	out := filepath.Join(exe, fmt.Sprintf("iKF-诊断-%s-%s.zip", host, time.Now().Format("20060102-150405")))
	zf, err := os.Create(out)
	if err != nil {
		return "", 0, err
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)
	n := 0
	add := func(p, zname string) {
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			return
		}
		h, err := zip.FileInfoHeader(fi)
		if err != nil {
			return
		}
		h.Name, h.Method = zname, zip.Deflate
		if w2, err := zw.CreateHeader(h); err == nil {
			if _, err := io.Copy(w2, f); err == nil {
				n++
			}
		}
	}
	logDir := filepath.Join(exe, "log")
	if entries, err := os.ReadDir(logDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				add(filepath.Join(logDir, e.Name()), "log/"+e.Name())
			}
		}
	}
	add(filepath.Join(exe, "iKF-驱动管理.log"), "iKF-驱动管理.log")
	dumps := filepath.Join(os.Getenv("LOCALAPPDATA"), "iKF", "dumps")
	if entries, err := os.ReadDir(dumps); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				add(filepath.Join(dumps, e.Name()), "dumps/"+e.Name())
			}
		}
	}
	if err := zw.Close(); err != nil {
		return "", n, err
	}
	return out, n, nil
}

func main() {
	start := time.Now()
	zipFlag := flag.Bool("zip", false, "把 log\\ 按天日志 + 驱动管理日志 + 崩溃转储打成回传 zip")
	arm := flag.Bool("arm", false, "给主程序/驱动管理布防 WER 本地崩溃转储（HKCU，免提权），只做动作不跑诊断")
	armOff := flag.Bool("arm-off", false, "撤销 -arm 布防，只做动作不跑诊断")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"用法：iKFTool.exe [-zip] [-arm | -arm-off] [额外场景目录…]\n"+
				"  无参数 = 跑全量诊断（运行环境/包完整性/进程与日志/崩溃取证/场景），\n"+
				"           报告打印到控制台并追加进 exe 旁 log\\yyyy-mm-dd.txt\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *arm {
		os.Exit(armDumps(false))
	}
	if *armOff {
		os.Exit(armDumps(true))
	}

	var b strings.Builder
	w := func(f string, a ...interface{}) { fmt.Fprintf(&b, f+"\n", a...) }

	w("════════ iKFTool 日志诊断报告 ════════")
	w("时间 : %s", start.Format("2006-01-02 15:04:05"))
	w("目录 : %s", mustExeDir())

	// ---- 新四段：环境 → 完整性 → 进程/日志 → 崩溃取证（全部只读） ----
	secRuntime(w)
	secIntegrity(w, mustExeDir())
	secProcsLogs(w, mustExeDir())
	secCrash(w, mustExeDir())

	w("")
	w("──────── 场景诊断 ────────")
	// ---- 收集候选目录（与主程序 profileDirs 同口径） ----
	exe := mustExeDir()
	type cand struct{ dir string; builtin bool }
	var cands []cand
	seen := map[string]bool{}
	add := func(dir string, builtin bool) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		cands = append(cands, cand{dir, builtin})
	}
	add(filepath.Join(exe, "CPL", "profile", "default"), true)
	add(filepath.Join(exe, "CPL", "profile"), true)
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		add(filepath.Join(appdata, "iKF Audio Center", "profile"), false)
	}
	// 命令行显式追加目录（flag.Parse 之后的全部位置参数）
	for _, a := range flag.Args() {
		if st, err := os.Stat(a); err == nil && st.IsDir() {
			add(a, false)
		}
	}

	// ---- 扫描全部场景（.profile=参数快照，.cml=列表元数据；两两成对。
	//      主列表以 .cml 为准，参数从同名 .profile 读 —— 与主程序一致） ----
	var scenes []sceneFile
	seenScene := map[string]bool{} // name+dir 去重（用户目录优先）
	for _, c := range cands {
		entries, err := os.ReadDir(c.dir)
		if err != nil {
			w("⚠ 目录不可读：%s（%v）", c.dir, err)
			continue
		}
		w("扫描 : %s（builtin=%v，%d 项）", c.dir, c.builtin, len(entries))
		for _, e := range entries {
			fn := e.Name()
			lfn := strings.ToLower(fn)
			if !strings.HasSuffix(lfn, ".cml") && !strings.HasSuffix(lfn, ".profile") {
				continue
			}
			var stem string
			if strings.HasSuffix(lfn, ".profile") {
				stem = fn[:len(fn)-len(".profile")]
			} else {
				stem = fn[:len(fn)-len(".cml")]
			}
			key := stem + "|" + c.dir
			if seenScene[key] {
				continue
			}
			seenScene[key] = true
			sf := sceneFile{name: stem, dir: c.dir, bdir: c.builtin,
				path: filepath.Join(c.dir, stem+".profile")}
			if fi, err := os.Stat(sf.path); err == nil {
				sf.fi = fi
			} else if fi, err := os.Stat(filepath.Join(c.dir, stem+".cml")); err == nil {
				sf.fi = fi
			}
			// 参数快照在 .profile；没有就退读 .cml（老档形态）
			raw, err := os.ReadFile(sf.path)
			if err != nil {
				if raw2, err2 := os.ReadFile(filepath.Join(c.dir, stem+".cml")); err2 == nil {
					sf.err = ""
					sf.item = profileItems(string(raw2))
					raw = raw2
				} else {
					sf.err = err.Error()
				}
			} else {
				sf.item = profileItems(string(raw))
			}
			scenes = append(scenes, sf)
		}
	}
	// 与主程序同口径排序：默认 → builtin → 普通，组内字母序
	rankOf := func(s sceneFile) int {
		if s.name == "默认" {
			return 0
		}
		if builtinScenes[s.name] && s.bdir {
			return 1
		}
		return 2
	}
	sort.SliceStable(scenes, func(i, j int) bool {
		ri, rj := rankOf(scenes[i]), rankOf(scenes[j])
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(scenes[i].name) < strings.ToLower(scenes[j].name)
	})

	// ---- 摘要 ----
	w("")
	w("──────── 场景清单（%d 个）────────", len(scenes))
	for _, s := range scenes {
		kind := "普通"
		if builtinScenes[s.name] && s.bdir {
			kind = "出厂"
		}
		n := "—"
		if s.fi != nil {
			n = fmt.Sprintf("%dB", s.fi.Size())
		}
		st := fmt.Sprintf("%d 参数", len(s.item))
		if s.err != "" {
			st = "读取失败：" + s.err
		}
		w("  [%s] %-8s %-10s %s", kind, s.name, n, st)
	}

	// ---- 逐场景参数明细 ----
	w("")
	w("──────── 参数明细 ────────")
	for _, s := range scenes {
		w("")
		w("◆ %s（%s）", s.name, s.path)
		if s.fi != nil {
			w("  修改时间: %s", s.fi.ModTime().Format("2006-01-02 15:04:05"))
		}
		curEp, curFx := "", ""
		for _, it := range s.item {
			if it.Endpoint != curEp || it.Effect != curFx {
				curEp, curFx = it.Endpoint, it.Effect
				w("  ── %s / %s", curEp, curFx)
			}
			flag := ""
			if isSuspiciousParam(it.Name) {
				flag = "   ⚠ 不应进场景（音量/静音/采样率/拓扑类）"
			}
			w("    %s = %s%s", it.Name, it.Value, flag)
		}
		if len(s.item) == 0 && s.err == "" {
			w("  （空档：无参数快照）")
		}
	}

	// ---- 交叉检查 ----
	w("")
	w("──────── 交叉检查 ────────")
	have := map[string]bool{}
	for _, s := range scenes {
		have[s.name] = true
	}
	for _, n := range expectFactory {
		if !have[n] {
			w("⚠ 出厂默认缺「%s」—— 新机器首次启动会由主程序播种", n)
		}
	}
	for _, n := range expectNormal {
		if !have[n] {
			w("⚠ 「%s」缺失 —— 主程序启动（full 初始化）会自动播种重建", n)
		}
	}
	warn := 0
	for _, s := range scenes {
		for _, it := range s.item {
			if isSuspiciousParam(it.Name) {
				warn++
				w("⚠ 「%s」混入排除项 %s（应用时会被主程序过滤，但档本身不干净）",
					s.name, it.Name)
			}
		}
	}
	if warn == 0 {
		w("✓ 无排除项混入、出厂场景齐全")
	}

	// ---- 落盘 ----
	// ★ 2026-10-25：不再单独写 诊断报告-<时间戳>.txt —— log 目录只保留
	//   主程序的按天日志。报告追加进同一份 log\yyyy-mm-dd.txt（O_APPEND
	//   多写者安全，与 setupFileLog 的单行写互不踩踏），控制台照常全文。
	rep := b.String()
	fmt.Print(rep)
	dir := filepath.Join(mustExeDir(), "log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "\n报告未落盘（log 目录建不了）：%v\n", err)
		os.Exit(1)
	}
	out := filepath.Join(dir, start.Format("2006-01-02")+".txt")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n报告写盘失败：%v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	entry := fmt.Sprintf("\n==== iKFTool 日志诊断（%s，PID %d）====\n%s\n",
		start.Format("2006-01-02 15:04:05"), os.Getpid(), rep)
	if _, err := f.WriteString(entry); err != nil {
		fmt.Fprintf(os.Stderr, "\n报告写盘失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n报告已追加：%s\n", out)

	if *zipFlag {
		p, n, err := makeZip(mustExeDir())
		if err != nil {
			fmt.Fprintf(os.Stderr, "打包失败：%v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已打包 %d 个文件：%s\n（把这一个 zip 发回来即可）\n", n, p)
	}
}

func mustExeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

var _ = io.Discard
