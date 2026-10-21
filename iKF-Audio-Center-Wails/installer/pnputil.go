//go:build windows

package main

// 系统侧驱动操作 —— 全部交给 Windows 自带的 pnputil
// =================================================
//
//	/enum-drivers                                  读 DriverStore：装没装
//	/add-driver <inf> /install                     装
//	/delete-driver <oemNN.inf> /uninstall /force   卸
//	/restart-device <实例ID>                        让端点重新绑定，APO 立刻生效
//
// 不自己写 SetupAPI：108B.Inf 已经把整套动作声明全了 —— 内核服务 108B、
// APO 的 CLSID（SfxFx.AddReg）、端点上的 FX 属性（SysFx.AddReg 走
// mssysfx.CopyFilesAndRegister），pnputil 就是这些声明的官方执行入口。
// 旧安装器那步 `XearAudioCenter /h /i` 注册的是**旧面板自己**的控制面板项，
// 与本程序无关，不需要复刻。
//
// 解析一律只认 ASCII 记号（oemNN.inf、硬件 ID），不认标签文字：pnputil 的
// 输出跟着系统语言走，中文机上是 GBK 编码，靠「发布名称」这类词匹配会翻车。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	infBase = "108B.Inf" // 必须与 driver/ 下的文件名逐字节一致 —— .cat 按文件名索引
	catBase = "108b.cat"
	drvSub  = "driver"

	createNoWindow = 0x08000000

	// pnputil 会拿这两个当「退出码」返回，但它们表示**已经成功**：
	// 3010 = ERROR_SUCCESS_REBOOT_REQUIRED（删/装都常见，设备在用时必现）
	// 1641 = ERROR_SUCCESS_REBOOT_INITIATED
	// 按非 0 一律当失败会误报 —— 真机上第一次卸载就是这么被错判的。
	errRebootRequired  = 3010
	errRebootInitiated = 1641
)

// diag 攒原始输出，只在失败时落盘（见 writeLog）：跑成功不留文件，出问题有据可查。
var diag []string

func note(format string, a ...interface{}) {
	diag = append(diag, fmt.Sprintf(format, a...))
}

// writeLog 把攒下的原始输出追加到 exe 同目录的日志（不可写则回退 %TEMP%）。
func writeLog() string {
	body := strings.Join(diag, "\n")
	if strings.TrimSpace(body) == "" {
		return ""
	}
	stamp := time.Now().Format("2006-01-02 15:04:05")
	for _, dir := range []string{exeDir(), os.TempDir()} {
		p := filepath.Join(dir, "iKF-驱动管理.log")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			continue
		}
		fmt.Fprintf(f, "\n===== %s =====\n%s\n", stamp, body)
		f.Close()
		return p
	}
	return ""
}

// rebootNote 把「成功但建议重启」翻成人话，并说明为什么。
func rebootNote(code int) string {
	if code == errRebootRequired || code == errRebootInitiated {
		return "\n（系统提示需要重启后才能完全生效。如果界面读不到参数，请重启一次。）"
	}
	return ""
}

var (
	reOemInf = regexp.MustCompile(`(?i)\boem\d+\.inf\b`)
	reAnyInf = regexp.MustCompile(`(?i)\b[a-z0-9_.\-]+\.inf\b`)
)

// drvState 是「系统里现在是什么情况」，全部来自只读查询。
type drvState struct {
	infPath   string   // 要装的 INF 绝对路径
	catPath   string   // 同目录的 .cat，pnputil 会按 INF 的 CatalogFile 去找
	published []string // DriverStore 里已发布的 oemNN.inf；空 = 没装
	hwid      string   // INF 声明的硬件 ID
	deviceID  string   // 设备实例 ID；空 = 设备当前不在
	oldPanel  string   // 旧版官方面板 exe 路径；空 = 未检测到（P3 顺序保护）
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}

// run 跑一个控制台程序并隐藏它的窗口 —— 本程序是 GUI 子系统，
// 不显式给 CREATE_NO_WINDOW 的话 pnputil 会自己弹一个黑框出来。
func run(name string, args ...string) (string, int) {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	raw, err := c.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return consoleText(raw), code
}

// consoleText 把控制台输出转成 UTF-8。pnputil 在中文系统上按 GBK 写，
// 已经是合法 UTF-8 的（英文系统）原样返回。
func consoleText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
		return string(s)
	}
	return string(b)
}

// probeStore 读 DriverStore，找出所有「原始名 = 108B.Inf」的发布项。
//
// 按空行切块，每块里 oemNN.inf 是发布名、另一个 .inf 是原始名。
// /enum-drivers 在没安装任何第三方驱动时退出码非 0（"找不到驱动程序包"），
// 所以非 0 不当错误，直接当作空结果。
func probeStore() []string {
	out, _ := run("pnputil", "/enum-drivers")
	var got []string
	var blk []string
	flush := func() {
		defer func() { blk = blk[:0] }()
		var pub, orig string
		for _, ln := range blk {
			for _, m := range reOemInf.FindAllString(ln, -1) {
				pub = m
			}
			for _, m := range reAnyInf.FindAllString(ln, -1) {
				if !reOemInf.MatchString(m) {
					orig = m
				}
			}
		}
		if pub != "" && strings.EqualFold(orig, infBase) {
			got = append(got, pub)
		}
	}
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		blk = append(blk, ln)
	}
	flush()
	sort.Strings(got)
	return got
}

// hwidFromInf 读 INF 的 [ManufacturerID.NTamd64] 段取硬件 ID。
// 硬编码会随 INF 换版失效，读出来才算数。
func hwidFromInf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	in := false
	for _, ln := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			in = strings.EqualFold(t, "[ManufacturerID.NTamd64]")
			continue
		}
		if !in || t == "" || strings.HasPrefix(t, ";") {
			continue
		}
		if i := strings.LastIndex(t, ","); i >= 0 {
			return strings.TrimSpace(t[i+1:])
		}
	}
	return ""
}

// probeDevice 找硬件 ID 对应的设备实例。设备没插着不算错误，只影响提示文案。
//
// 不用 Upper 后做子串定位：UTF-8 下大小写变换可能改字节长度，中文输出会错位。
func probeDevice(hwid string) string {
	if hwid == "" {
		return ""
	}
	out, _ := run("pnputil", "/enum-devices", "/class", "Media")
	pat := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(hwid) + `[a-zA-Z0-9_&.\\\-]*`)
	return pat.FindString(out)
}

func probe() *drvState {
	d := exeDir()
	s := &drvState{
		infPath: filepath.Join(d, drvSub, infBase),
		catPath: filepath.Join(d, drvSub, catBase),
	}
	s.hwid = hwidFromInf(s.infPath)
	s.published = probeStore()
	s.deviceID = probeDevice(s.hwid)
	s.oldPanel = probeOldPanel()
	return s
}

// probeOldPanel 检测旧版官方面板是否还在（P3 顺序保护）。
//
// 背景（内部取证报告 §1.6）：旧卸载器 [POSTDELFILE_REMOVE_64]
// 按**文件名**删文件、不认路径归属 —— System32\Drivers\108B.sys、
// CMEffectAPO_108B.dll、CMCplExt_108B.dll 都在删除清单里。谁先卸旧版谁就把
// 驱动文件删了（耳机静音，直到重新 --install）。所以必须引导用户按
// 「先装新驱动 → 再卸旧版 → 重跑一次 --install 恢复被删文件」的顺序走。
//
// 检测用文件存在性而不是注册表：Run/CLSID 可能已被接管（P1-A）改指向新 exe，
// 文件系统才是旧版卸载器会不会再跑的真实依据。INSTALLDIR 取官方安装器默认值
// %ProgramFiles%\iKF headset（CmiSetupConfig.dll 按 INI 写死的那套）。
func probeOldPanel() string {
	exe := filepath.Join(os.Getenv("ProgramFiles"), "iKF headset", "CPL",
		"XearAudioCenter_x64.exe")
	if fileExists(exe) {
		return exe
	}
	return ""
}

// oldPanelNote 旧版还在时给一句安全顺序提示；没检测到返回空串。
// 2026-10-19 精简：新框架直接抛弃官方旧面板，这里只留必须知道的一句 ——
// 旧卸载器会按文件名删驱动文件，顺序错了耳机静音（P3 取证结论不变）。
func (s *drvState) oldPanelNote() string {
	if s.oldPanel == "" {
		return ""
	}
	return "\n\n⚠ 检测到旧版官方面板：" + filepath.Dir(s.oldPanel) +
		"\n旧卸载器会按文件名删驱动文件：先卸旧版，卸完重跑一次本程序安装补回文件即可。"
}

// packErr 检查免安装包是否完整。缺 .cat 是最容易踩的坑 ——
// 没有它 pnputil 会以「签名无效」失败，而不是「文件不存在」。
func (s *drvState) packErr() string {
	if !fileExists(s.infPath) {
		return "找不到驱动描述文件：\n" + s.infPath + "\n\n本程序需要与 driver 目录放在同一个文件夹里。"
	}
	if !fileExists(s.catPath) {
		return "找不到驱动签名文件：\n" + s.catPath + "\n\n驱动包不完整，请重新解压。"
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// install 把 driver/ 加进 DriverStore 并立即装到已连接的设备上。
func (s *drvState) install() (string, bool) {
	if e := s.packErr(); e != "" {
		return e, false
	}
	out, code := run("pnputil", "/add-driver", s.infPath, "/install")
	note("pnputil /add-driver %s /install → 退出码 %d\n%s", s.infPath, code, out)
	if code != 0 && code != errRebootRequired && code != errRebootInitiated {
		return "安装失败（pnputil 退出码 " + itoa(code) + "）。\n\n" + tail(out, 900), false
	}
	got := probeStore()
	if len(got) == 0 {
		// 命令报成功但驱动库里没有 —— 不能算成功
		return "命令已执行，但系统驱动库里仍找不到该驱动包。\n\n" + tail(out, 700), false
	}
	// 2026-10-20（用户定稿）：完成弹窗只说结果，pnputil 原始输出留在日志
	// （note() 已落盘），失败弹窗才带技术细节。行尾不带句号。
	return "驱动组件已安装完成" + rebootNote(code), true
}

// uninstall 删掉 DriverStore 里所有该驱动的发布项。
// 同一份 INF 反复安装会攒出多个 oemNN.inf，只删一个等于没卸干净。
func (s *drvState) uninstall() (string, bool) {
	got := probeStore()
	if len(got) == 0 {
		return "系统里没有该驱动组件，无需卸载。", true
	}
	var lines []string
	ok := true
	reboot := 0
	for _, name := range got {
		out, code := run("pnputil", "/delete-driver", name, "/uninstall", "/force")
		note("pnputil /delete-driver %s /uninstall /force → 退出码 %d\n%s", name, code, out)
		if code != 0 && code != errRebootRequired && code != errRebootInitiated {
			ok = false
			lines = append(lines, name+" 删除失败（退出码 "+itoa(code)+"）："+tail(out, 400))
			continue
		}
		if code == errRebootRequired || code == errRebootInitiated {
			reboot = code
		}
		lines = append(lines, name+" 已删除")
	}
	left := probeStore()
	if len(left) > 0 {
		ok = false
		lines = append(lines, "驱动库里仍有残留："+strings.Join(left, "、"))
	}
	if !ok {
		// 失败才逐项带细节；成功弹窗只说结果
		return strings.Join(lines, "\n") + rebootNote(reboot), false
	}
	// 2026-10-20（用户定稿）：卸载完成固定建议重启电脑（不再依赖 pnputil
	// 是否回报了 reboot required）。
	return "驱动组件已卸载完成，建议重启电脑", true
}

// restartDevice 重启设备实例，逼音频引擎重新加载端点 ——
// 不重启的话刚注册的 APO 要等到下次重新插拔才挂上。
// 老系统没有 /restart-device（Win10 1903+ 才有），失败就算了，由提示兜底。
func (s *drvState) restartDevice() string {
	if s.deviceID == "" {
		return ""
	}
	if _, code := run("pnputil", "/restart-device", s.deviceID); code != 0 {
		return ""
	}
	return s.deviceID
}

// dryRunText 只把将要执行的东西念出来，一个字都不落地。
func dryRunText(s *drvState, action string) string {
	var b strings.Builder
	b.WriteString("— 预演，未执行任何系统改动 —\n\n")
	b.WriteString("Inf : " + s.infPath + "\n")
	b.WriteString("Cat : " + s.catPath + "\n")
	b.WriteString("HWID: " + s.hwid + "\n")
	b.WriteString("设备: " + orDash(s.deviceID) + "\n")
	b.WriteString("已装: " + orDash(strings.Join(s.published, "、")) + "\n\n")
	if action == "install" {
		b.WriteString("pnputil /add-driver \"" + s.infPath + "\" /install\n")
		b.WriteString("pnputil /restart-device \"" + orDash(s.deviceID) + "\"\n")
	} else {
		list := s.published
		if len(list) == 0 {
			b.WriteString("（驱动库里没有该驱动，无需执行删除）")
			return b.String()
		}
		for _, n := range list {
			b.WriteString("pnputil /delete-driver " + n + " /uninstall /force\n")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// tail 取输出的末尾若干字节，失败时给用户看的原话就在最后几行。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func orDash(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}
