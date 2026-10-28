package main

/*
initfp.go —— 安装指纹：真正分清「驱动被重装过」与「普通启动」。

## 为什么要它

前两轮把初始化收成了一份清单（initmanifest.go）和一条链（initstate.go），
但「什么时候该全量写」这个判据一直是**启发式**：出厂表 42 项里缺省 ≥21 项
就当成「驱动被重置」（lastscene.go:lastAppliedResetThreshold）。

这个启发式有三个缺口，都是用户真实会遇到的：

	① 驱动装了一半 —— 比如 20/42 项被清、其余还在。没到阈值 ⟹ 一项都不写，
	   设备停在「一半默认一半残留」，界面看不出来，只有逐项核对才发现。
	② 值还在但语义变了 —— 驱动小版本升级、端点重建后又把旧值搬回来。
	   缺省 0/42 ⟹ 判「无需恢复」，可那套值是上个驱动的。
	③ 换了个 USB 口 —— hwid 变（`8&33013dbd` 那一段）、端点 GUID 全新，
	   FxProperties 是空的，但如果驱动顺手把邻居端点的值复制过来就测不出来。

启发式测的是「值在不在」，而用户说的是「**驱动被重装过**」。这是两件事，
得直接测后者。

## 判据（2026-10-22 用户拍板：端点 GUID + 驱动版本）

	端点 GUID   GetExtraInfo → `{0.0.0.00000000}.{b2446ada-…}`
	            这是 MMDevice 的端点 ID，重装驱动/换口必然变（实测本机
	            iKF-V11 有两个渲染 GUID，其中 {204114b8-…} 是上一次安装
	            留下的死端点 —— 光按设备名在注册表里数会数到两个）。
	驱动版本    GetDriverVer → `10.0.1.1`
	设备实例    GetDeviceID  → `usb\vid_0d8c&pid_0012&mi_00\8&33013dbd&0&0000`

三个都是 osConfLib 的**只读查询**，两条线现成能读（/api/device_info 就在用），
不需要碰注册表、不需要枚举 MMDevices。

## 三条安全规则（比判据本身更重要）

1. 指纹文件**不存在 ≠ 重装**。首次运行（新装、或从旧版本升级上来）只记基线，
   绝不因此全量写 —— 否则每个老用户升级一次就被清一次调音。
   「全新的机器」由 21/42 启发式兜底（值全空，两条判据都指向全量写）。
2. 只有**两边都非空且不同**才算「变了」。读抖动、采集端点临时没起来、
   注册表还没长出来 —— 这些都表现为空串，一律判「不知道」，宁可不动。
3. 字段是新旧**合并**写的：新读不到的字段保留旧值，绝不用空串覆盖。
   否则一次读失败就把基线擦掉，下次启动又变成「首次记录」。

指纹文件落 `%APPDATA%\iKF Audio Center\init-fingerprint.json`（跟随用户，
不跟随驱动）—— 驱动卸载不会删它，所以「卸载→重装」这条路能测出来。
*/

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	initFpVer  = 1
	initFpName = "init-fingerprint.json"
)

// initFp 一份安装指纹。
type initFp struct {
	Ver      int    `json:"ver"`
	Endpoint string `json:"endpoint"` // GetExtraInfo（渲染端点）
	Capture  string `json:"capture"`  // GetExtraInfo（采集端点，无麦时为空）
	Hwid     string `json:"hwid"`     // GetDeviceID
	Driver   string `json:"driver"`   // GetDriverVer
	Model    string `json:"model"`    // GetDeviceFriendlyName
	At       string `json:"at"`       // 记录时间，纯排障用
}

// appdataStateDir 用户级状态目录（%APPDATA%\iKF Audio Center）。
// 与 profile 子目录同级：场景文件在 profile/ 里，指纹在外面，
// 不会被 profileDirs / 场景枚举当成场景档。
func appdataStateDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		if h, err := os.UserHomeDir(); err == nil {
			base = h
		}
	}
	return filepath.Join(base, "iKF Audio Center")
}

func initFpPath() string { return filepath.Join(appdataStateDir(), initFpName) }

// readInitFp 读指纹文件。第二个返回值 = 文件是否存在且可解析。
func readInitFp() (initFp, bool) {
	var fp initFp
	b, err := os.ReadFile(initFpPath())
	if err != nil || len(b) == 0 {
		return fp, false
	}
	if err := json.Unmarshal(b, &fp); err != nil {
		return initFp{}, false // 半截文件/手工改坏：当没有，重记基线
	}
	if fp.Ver != initFpVer {
		return initFp{}, false // 结构升级：同样当没有，不猜旧字段
	}
	return fp, true
}

// fpField 读一个只读查询项，读不到返回空串（重读一次抗已知抖动）。
func fpField(d *Driver, ep, name string) string {
	ret, v := d.ReadEp(ep, name)
	if ret != 0 || strings.TrimSpace(v) == "" {
		ret, v = d.ReadEp(ep, name)
	}
	if ret != 0 {
		return ""
	}
	return strings.TrimSpace(v)
}

// initFpNow 读当前指纹（只读，不落盘）。
func initFpNow(d *Driver) initFp {
	fp := initFp{
		Ver:      initFpVer,
		Endpoint: fpField(d, EpRender, "GetExtraInfo"),
		Hwid:     fpField(d, EpRender, "GetDeviceID"),
		Driver:   fpField(d, EpRender, "GetDriverVer"),
		Model:    fpField(d, EpRender, "GetDeviceFriendlyName"),
	}
	if d.capReady {
		fp.Capture = fpField(d, EpCapture, "GetExtraInfo")
	}
	return fp
}

// fpKnown 指纹里是否至少有一个可比的字段（否则「不知道」，不做判断）。
func (fp initFp) known() bool {
	return fp.Endpoint != "" || fp.Hwid != "" || fp.Driver != ""
}

// fpDiffers 两边都非空且不同才算变了 —— 见文件头「安全规则 2」。
func fpDiffers(a, b string) bool {
	return a != "" && b != "" && !strings.EqualFold(a, b)
}

// shortFp 端点 ID 太长，日志里只留 GUID 那一段。
func shortFp(s string) string {
	if i := strings.LastIndex(s, "}."); i >= 0 && i+2 < len(s) {
		return s[i+2:]
	}
	if s == "" {
		return "（空）"
	}
	return s
}

// initFpChanged 判定「这台机器上的驱动换过一茬」。
//
// 返回 (是否该全量写, 原因)。原因串会进日志与 /api/driver/reopen 响应，
// 用户看到参数被重置时能知道是谁干的。
func initFpChanged(d *Driver) (bool, string) {
	if d == nil || !d.ready {
		return false, ""
	}
	cur := initFpNow(d)
	if !cur.known() {
		return false, "" // 三个字段全读不到：判不了，宁可不动用户参数
	}
	old, ok := readInitFp()
	if !ok {
		/* 安全规则 1：没有指纹 ≠ 重装。只记基线，不写设备。
		   真正全新的机器由 21/42 启发式兜底（值全空）。 */
		return false, ""
	}
	var why []string
	if fpDiffers(old.Endpoint, cur.Endpoint) {
		why = append(why, fmt.Sprintf("渲染端点 %s → %s",
			shortFp(old.Endpoint), shortFp(cur.Endpoint)))
	}
	if fpDiffers(old.Capture, cur.Capture) {
		why = append(why, fmt.Sprintf("采集端点 %s → %s",
			shortFp(old.Capture), shortFp(cur.Capture)))
	}
	if fpDiffers(old.Driver, cur.Driver) {
		why = append(why, fmt.Sprintf("驱动版本 %s → %s", old.Driver, cur.Driver))
	}
	if fpDiffers(old.Hwid, cur.Hwid) {
		why = append(why, fmt.Sprintf("设备实例 %s → %s", old.Hwid, cur.Hwid))
	}
	if len(why) == 0 {
		return false, ""
	}
	return true, strings.Join(why, "；")
}

// mergeInitFp 新读到的覆盖旧的，但**空串不覆盖**（安全规则 3）。
func mergeInitFp(old, cur initFp) initFp {
	pick := func(o, c string) string {
		if c != "" {
			return c
		}
		return o
	}
	return initFp{
		Ver:      initFpVer,
		Endpoint: pick(old.Endpoint, cur.Endpoint),
		Capture:  pick(old.Capture, cur.Capture),
		Hwid:     pick(old.Hwid, cur.Hwid),
		Driver:   pick(old.Driver, cur.Driver),
		Model:    pick(old.Model, cur.Model),
	}
}

// saveInitFp 记录/刷新基线。写完后这次的「重装」就不会再触发第二次。
//
// 写临时文件再 rename：半截 JSON 会让下次启动把基线当成损坏（回到「首次
// 记录」），虽然安全但会白丢一次重装判据。
func saveInitFp(d *Driver) error {
	if d == nil || !d.ready {
		return nil
	}
	cur := initFpNow(d)
	if !cur.known() {
		return nil // 读不到就别写，免得把好基线擦成空
	}
	old, _ := readInitFp()
	merged := mergeInitFp(old, cur)
	merged.At = time.Now().Format("2006-01-02 15:04:05")

	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	dir := appdataStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := initFpPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, initFpPath())
}

// initFpReport 给 --init-fp 命令用的人读报告（只读，不落盘）。
func initFpReport(d *Driver) string {
	var sb strings.Builder
	sb.WriteString("安装指纹（只读，不写设备）\n")
	sb.WriteString(strings.Repeat("-", 60) + "\n")
	p := initFpPath()
	sb.WriteString(fmt.Sprintf("指纹文件 : %s\n", p))
	old, ok := readInitFp()
	if !ok {
		if _, err := os.Stat(p); err == nil {
			sb.WriteString("已记录   : （文件存在但无法解析 —— 下次启动会重记基线）\n")
		} else {
			sb.WriteString("已记录   : （无 —— 下次启动记基线，不写设备）\n")
		}
	} else {
		sb.WriteString(fmt.Sprintf("已记录   : 端点 %s · 采集 %s\n", shortFp(old.Endpoint), shortFp(old.Capture)))
		sb.WriteString(fmt.Sprintf("           %s · %s · %s\n", old.Driver, old.Hwid, old.Model))
		sb.WriteString(fmt.Sprintf("           记录于 %s\n", old.At))
	}

	cur := initFpNow(d)
	sb.WriteString(fmt.Sprintf("当前     : 端点 %s · 采集 %s\n", shortFp(cur.Endpoint), shortFp(cur.Capture)))
	sb.WriteString(fmt.Sprintf("           %s · %s · %s\n", cur.Driver, cur.Hwid, cur.Model))
	sb.WriteString(strings.Repeat("-", 60) + "\n")

	changed, why := initFpChanged(d)
	switch {
	case !cur.known():
		sb.WriteString("结论     : 读不到指纹字段，无法判定（不动用户参数）\n")
	case changed:
		sb.WriteString(fmt.Sprintf("结论     : 变了 —— 下次初始化走全量写（%s）\n", why))
	default:
		if !ok {
			sb.WriteString("结论     : 无基线 —— 下次启动记录基线，设备不动\n")
		} else {
			sb.WriteString("结论     : 未变 —— 初始化只补空缺（ret=2），不覆盖调音\n")
		}
	}
	return sb.String()
}

// initFpSummary 一行摘要，塞进 /api/device_info 让人能直接看到现状。
func initFpSummary(d *Driver) map[string]interface{} {
	cur := initFpNow(d)
	old, ok := readInitFp()
	changed, why := initFpChanged(d)
	m := map[string]interface{}{
		"path":     initFpPath(),
		"recorded": ok,
		"changed":  changed,
		"current": map[string]string{
			"endpoint": cur.Endpoint,
			"capture":  cur.Capture,
			"driver":   cur.Driver,
			"hwid":     cur.Hwid,
			"model":    cur.Model,
		},
	}
	if ok {
		m["baseline"] = map[string]string{
			"endpoint": old.Endpoint,
			"capture":  old.Capture,
			"driver":   old.Driver,
			"hwid":     old.Hwid,
			"model":    old.Model,
			"at":       old.At,
		}
	}
	if why != "" {
		m["reason"] = why
	}
	return m
}
