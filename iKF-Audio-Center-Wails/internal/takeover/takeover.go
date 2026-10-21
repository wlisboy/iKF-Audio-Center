//go:build windows

// Package takeover —— 入口接管（P5-3 从主程序 takeover.go/entry.go 抽出）
// ====================================================================
//
// 官方安装器（CmiSetupConfig.dll [ADDREG_64]）写了两个需要接管的入口：
//
//	HKLM\...\CurrentVersion\Run\Cm108BSound      = "<旧面板>" /h /d   ← 开机自启
//	HKCR\CLSID\{125C480D-...}\Shell\Open\Command = "<旧面板>"         ← 控制面板项
//	（HKLM\...\Explorer\ControlPanel\NameSpace\{125C480D-...} 只是登记
//	  上面那个 CLSID，指向不用动）
//
// 接管 = 把两处的可执行文件改成新 exe；撤销 = 改回旧面板。
// 主程序（--takeover CLI / 托盘「修复开机入口」/ 启动自愈）与
// iKF-驱动管理.exe（勾选清单「接管/撤销接管」）共用本包，只此一份逻辑。
//
// 权限：两项都在 HKLM/HKCR，需要管理员。调用方自理 ——
//
//   - 主程序恒定 asInvoker，写失败时由调用方 runElevated 提权重跑自己；
//   - 驱动管理程序启动即提权，可直接调 Write/UndoWrite。
//
// P2 实测依据（为什么 /h /d 原样保留）：
//
//   - 旧 /h /d 是「隐藏托盘常驻」语义 → 新 exe 把 /h /d / --hidden / --tray
//     解释为 StartHidden 启动（窗口隐藏、托盘在），行为等价；
//   - 旧 /h /i 只写 Hook\EnableGFS=1 → 新面板 GFSControl 开关本就读写
//     此值，接管后由用户在界面里自己控制，不自作主张替用户开。
package takeover

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	// RunPath / RunName 开机自启项。值名保持 Cm108BSound（不新增条目，
	// 卸载旧版时好清理 —— 与官方安装器同一落点）。
	RunPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`
	RunName = "Cm108BSound"

	// ClsidCmd 控制面板项的打开命令。HKCR 是 HKLM\Software\Classes 与
	// HKCU 的合并视图，写 HKCR 即落在 HKLM 侧（与官方安装器一致）。
	// CLSID 与 CmiSetupConfig.dll [ADDREG_64] 写入的逐字一致。
	ClsidCmd = `CLSID\{125C480D-543D-49C8-8840-4D28400ED72A}\Shell\Open\Command`

	// RunArgs 旧面板的开机参数原样保留 —— 主程序把 /h /d 解释为
	// 「隐藏进托盘启动」。
	RunArgs = ` /h /d`

	// OldExe 旧面板 exe（官方安装器默认 INSTALLDIR）。撤销接管把两处
	// 注册表值改回它；若旧版已被卸载，这两处本来就是死路径（无害）。
	OldExe = `C:\Program Files\iKF headset\CPL\XearAudioCenter_x64.exe`
)

// ExePath 返回本程序的可执行文件绝对路径。
//
// 提权重跑后子进程的当前目录会变成 System32，所以路径必须取绝对值
// （os.Executable 恒为绝对）。os.Executable 极少失败；失败时退回
// argv[0] 推断，并把原始错误一并带回，让调用方决定要不要继续。
func ExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		if len(os.Args) > 0 {
			if a, aerr := filepath.Abs(os.Args[0]); aerr == nil {
				return a, err
			}
		}
		return "", err
	}
	return filepath.Abs(p)
}

// 官方卸载器会删掉的入口注册表（2026-10-20 本机实测，官方程序卸载后）：
//
//	Run\Cm108BSound 值            ← 没了
//	CLSID\{125C480D}\Shell 子树   ← 整个没了（Command 键不存在）
//	CLSID 根 / DefaultIcon / NameSpace ← 都还在
//
// 因此 Write 必须能在「键被删」的残局上从零重建：CreateKey（RegCreateKeyExW
// 会自动补齐中间路径）而非 OpenKey，并把官方安装器写过的显示元数据补回来，
// 否则控制面板里出现的是一个无名空项。元数据逐字取自卸载前实读（见
// 内部取证报告）。

// clsidRoot 官方控制面板 CLSID 根键与官方安装器写过的显示元数据。
const clsidRoot = `CLSID\{125C480D-543D-49C8-8840-4D28400ED72A}`

var clsidMeta = []struct{ name, value string }{
	{"", "iKF headset"},
	{"InfoTip", "Configure your sound card"},
	{"System.ApplicationName", "CMedia.XearAudioCenter"},
	{"System.ControlPanel.Category", "4"},
	{"System.Software.TasksFileUrl", "internal"},
}

// nsPath 控制面板「名称空间」登记键——它只登记 CLSID（默认值=显示名），
// 指向靠 CLSID\Shell\Open\Command。卸载器同样可能删掉它，Write 顺带重建。
const nsPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\ControlPanel\NameSpace\{125C480D-543D-49C8-8840-4D28400ED72A}`

// ensureControlPanelMeta 重建 CLSID 根的显示元数据与 NameSpace 登记。
// 全部「缺了才写」（官方卸载删掉的补回来，还在的绝不覆盖——比如 Command
// 指向新程序后这里不会碰它）。需要管理员，调用方自理。
func ensureControlPanelMeta() error {
	k, _, err := registry.CreateKey(registry.CLASSES_ROOT, clsidRoot, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("重建 CLSID 根键失败：%w", err)
	}
	for _, m := range clsidMeta {
		if v, _, err := k.GetStringValue(m.name); err != nil || v == "" {
			if err := k.SetStringValue(m.name, m.value); err != nil {
				k.Close()
				return err
			}
		}
	}
	k.Close()

	// DefaultIcon（官方卸载实测会保留，但缺了图标就没了——一并兜底）
	ki, _, err := registry.CreateKey(registry.CLASSES_ROOT, clsidRoot+`\DefaultIcon`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("重建 DefaultIcon 失败：%w", err)
	}
	if v, _, err := ki.GetStringValue(""); err != nil || v == "" {
		_ = ki.SetStringValue("", `C:\Windows\CM108B.ico`)
	}
	ki.Close()

	// NameSpace 登记
	kn, _, err := registry.CreateKey(registry.LOCAL_MACHINE, nsPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("重建 NameSpace 登记失败：%w", err)
	}
	if v, _, err := kn.GetStringValue(""); err != nil || v == "" {
		_ = kn.SetStringValue("", "iKF headset")
	}
	kn.Close()
	return nil
}

// Write 实际改写两处注册表指向本程序，返回成功改写的项数。
// 任何一项失败都立即返回错误 —— 半接管状态比不接管更糟。
func Write() (int, error) {
	exe, err := ExePath()
	if err != nil {
		return 0, err
	}

	// 1) 开机自启（HKLM Run 键系统恒有，OpenKey 即可）
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RunPath,
		registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return 0, fmt.Errorf("打不开 Run 键（多半不是管理员）：%w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(RunName, `"`+exe+`"`+RunArgs); err != nil {
		return 0, err
	}
	n := 1

	// 2) 控制面板项：官方卸载会把 Shell 子树整棵删掉（2026-10-20 实测），
	//    必须 CreateKey 从零重建（RegCreateKeyExW 自动补齐中间路径），
	//    并补回官方显示元数据 —— 否则控制面板里是无名空项。
	if err := ensureControlPanelMeta(); err != nil {
		return n, err
	}
	k2, _, err := registry.CreateKey(registry.CLASSES_ROOT, ClsidCmd,
		registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return n, fmt.Errorf("重建 CLSID Shell\\Open\\Command：%w", err)
	}
	defer k2.Close()
	if err := k2.SetStringValue("", `"`+exe+`"`); err != nil {
		return n, err
	}
	return n + 1, nil
}

// UndoWrite 把两处入口改回旧面板（撤销接管），返回成功改写的项数。
// 与 Write 对称：任何一项失败都立即返回。官方卸载可能已删掉 Shell 子树
// （2026-10-20 实测），与 Write 同样用 CreateKey 重建 + 补官方元数据 ——
// 回滚后控制面板项要能正常显示旧面板。
func UndoWrite() (int, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RunPath,
		registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return 0, fmt.Errorf("打不开 Run 键（多半不是管理员）：%w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(RunName, `"`+OldExe+`"`+RunArgs); err != nil {
		return 0, err
	}

	if err := ensureControlPanelMeta(); err != nil {
		return 1, err
	}
	k2, _, err := registry.CreateKey(registry.CLASSES_ROOT, ClsidCmd,
		registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return 1, fmt.Errorf("重建 CLSID Shell\\Open\\Command：%w", err)
	}
	defer k2.Close()
	if err := k2.SetStringValue("", OldExe); err != nil {
		return 1, err
	}
	return 2, nil
}

// DeleteEntry 删除两处入口（2026-10-20 用户拍板：托盘要「可写也要可删」）：
//
//	Run\Cm108BSound 值整个删掉      ← 开机不再自启（删除≠改回旧面板）
//	CLSID Shell\Open\Command 默认值删掉 ← 控制面板项不再是「打开旧面板」
//
// 两处都随时可以用 Write（修复开机入口）原样恢复，删除是可逆操作。
// 返回成功删除的项数（0~2）：值本来就不存在按「无需删除」计 0，
// 不算错误 —— 幂等，重复点菜单不出错。
func DeleteEntry() (int, error) {
	n := 0
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RunPath, registry.SET_VALUE)
	if err != nil {
		return n, fmt.Errorf("打不开 Run 键（多半不是管理员）：%w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(RunName); err == nil {
		n++
	}

	// Command 键可能整个不存在（官方卸载删了 Shell 子树，或从未接管）——
	// 那就没有可删的值，按幂等成功处理，不算错误。
	k2, err := registry.OpenKey(registry.CLASSES_ROOT, ClsidCmd, registry.SET_VALUE)
	if err == nil {
		defer k2.Close()
		if err := k2.DeleteValue(""); err == nil {
			n++
		}
	}
	return n, nil
}

// ---- 入口体检 ----

// Status 是一次入口体检的结果。
type Status struct {
	Self string // 本程序绝对路径

	RunValue  string
	RunExe    string
	RunExists bool
	RunOK     bool

	ClsValue  string
	ClsExe    string
	ClsExists bool
	ClsOK     bool
}

// OK 两处都指向本程序。
func (e Status) OK() bool { return e.RunOK && e.ClsOK }

// Stale 至少一处指向不存在的文件（入口已坏）。
func (e Status) Stale() bool {
	if e.RunExe != "" && !e.RunExists {
		return true
	}
	return e.ClsExe != "" && !e.ClsExists
}

// Describe 给提示框与托盘菜单用的一句话状态。
func (e Status) Describe() string {
	switch {
	case e.OK():
		return "已指向本程序"
	case e.Stale():
		return "入口已失效（目标文件不存在）"
	case e.RunExe == "" && e.ClsExe == "":
		return "未设置"
	default:
		other := e.RunExe
		if other == "" {
			other = e.ClsExe
		}
		return "指向 " + other
	}
}

// Short 给托盘菜单/勾选清单用的一句话（不能太长，放不下完整路径）。
func (e Status) Short() string {
	switch {
	case e.OK():
		return "已指向本程序"
	case e.Stale():
		return "入口已失效"
	case e.RunExe == "" && e.ClsExe == "":
		return "未设置"
	default:
		return "指向其他程序"
	}
}

// ExeFromCmd 从注册表命令行里取出 exe 路径。
//
// 两种形态都要认：
//
//	"C:\dir\a.exe" /h /d     ← 官方安装器与 Write 都带引号
//	C:\dir\a.exe             ← CLSID 的 Shell\Open\Command 不带参数
//
// 无引号且带空格时无法可靠切分（路径本身可能含空格），此时优先把整串
// 当路径试一次，不存在再按第一个空格截断。
func ExeFromCmd(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if i := strings.Index(s[1:], `"`); i >= 0 {
			return s[1 : 1+i]
		}
		return strings.Trim(s, `"`)
	}
	if _, err := os.Stat(s); err == nil {
		return s
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

// SameExe 比较两个路径是否指同一个可执行文件（忽略大小写与多余分隔符）。
func SameExe(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// Read 读两处入口并逐项判定。读操作不需要管理员。
func Read() Status {
	st := Status{}
	if p, err := ExePath(); err == nil {
		st.Self = p
	}

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RunPath,
		registry.QUERY_VALUE); err == nil {
		v, _, _ := k.GetStringValue(RunName)
		k.Close()
		st.RunValue = v
		st.RunExe = ExeFromCmd(v)
		if st.RunExe != "" {
			if _, err := os.Stat(st.RunExe); err == nil {
				st.RunExists = true
			}
		}
		st.RunOK = st.RunExists && SameExe(st.RunExe, st.Self)
	}

	if k, err := registry.OpenKey(registry.CLASSES_ROOT, ClsidCmd,
		registry.QUERY_VALUE); err == nil {
		v, _, _ := k.GetStringValue("")
		k.Close()
		st.ClsValue = v
		st.ClsExe = ExeFromCmd(v)
		if st.ClsExe != "" {
			if _, err := os.Stat(st.ClsExe); err == nil {
				st.ClsExists = true
			}
		}
		st.ClsOK = st.ClsExists && SameExe(st.ClsExe, st.Self)
	}
	return st
}
