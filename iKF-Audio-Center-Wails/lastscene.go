package main

// lastscene.go —— 驱动重置后的「自动回到默认场景」+ 设备真值场景识别。
//
// 背景（2026-10-20 用户澄清需求）：驱动卸载重装会清空设备侧参数
// （FxProperties），开机自愈只补 42 项出厂默认；用户期望的恢复行为是
// **回到「默认」场景**（干净起点），之后自己决定用哪个场景（点场景卡
// 随时改，写回路径已实测可靠）。界面「使用中 · X」此前来自前端
// localStorage（与设备无关、跨重装残留），形成「显示在用、实际被清」
// 的假象 —— 现改为后端按设备真值识别（identifyActiveScene）。
//
// 与 Python 版（tools/src/center_src.py 的 restore_default_scene /
// identify_active_scene）逐字段对应。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lastAppliedResetThreshold —— 「端点被重置」指纹阈值。
// factoryDefaults 共 42 项（渲染 36 + 采集 6），正常使用中的机器绝大多数
// 有值；卸载器清空 / PnP 端点重建后全部退回 ret=2。过半即判定被重置。
const lastAppliedResetThreshold = 21

// missingDefaultCount —— 重置指纹：factoryDefaults 表里当前 ret==2 的项数。
func missingDefaultCount(d *Driver) int {
	n := 0
	for _, it := range factoryDefaults {
		if ret, _ := d.ReadEp(it.Ep, it.Name); ret == 2 {
			n++
		}
	}
	return n
}

// RestoreDefaultScene 驱动重置后的自动恢复：指纹命中 → 把出厂默认参数
// 全量写回（=「默认」场景的状态；apply 场景文件依赖播种顺序，直接写
// factoryDefaults 表无文件依赖、幂等）。守护/主程序/--restore-scene 共用。
// force=true 跳过指纹（排障兜底：指纹窗口被自愈提前跑掉时手动恢复）。
// 指纹不成立绝不写 —— 保护用户手动调过但没存场景的参数。
// 返回 (是否触发了写设备, 人读描述)。
func RestoreDefaultScene(d *Driver, force bool) (bool, string) {
	if d == nil || !d.ready {
		return false, "驱动未就绪，无法检测"
	}
	missing := missingDefaultCount(d)
	if !force && missing < lastAppliedResetThreshold {
		return false, fmt.Sprintf("设备参数在位（出厂默认表缺省仅 %d/%d 项），无需恢复",
			missing, len(factoryDefaults))
	}
	// 全量重写（不只补 ret==2 —— force 路径可能有残留的错误值），
	// 与 HealMissingParams 相同的值表，语义 = 自动应用「默认」场景。
	applied, failed := 0, 0
	for _, it := range factoryDefaults {
		sp, known := spec(it.Ep, it.Name)
		kind := ""
		if known {
			kind = kindName(sp.kind)
		}
		res := d.WriteAuto(it.Ep, it.Name, it.Val, kind)
		if ok, _ := res["ok"].(bool); ok {
			applied++
		} else {
			failed++
		}
	}
	if failed == 0 {
		return true, fmt.Sprintf("检测到参数被驱动重置（缺省 %d/%d 项），已自动恢复出厂默认参数（写入 %d 项）",
			missing, len(factoryDefaults), applied)
	}
	return true, fmt.Sprintf("尝试恢复出厂默认参数未完全成功（成功 %d / 失败 %d）",
		applied, failed)
}

// identifyActiveScene 设备真值场景识别：当前设备参数与哪个场景快照
// 完全一致就返回哪个名字；都不一致返回 ""（自定义状态）。
//
// 判据（与 profileSave 的快照口径一致）：
//   - 只比 .profile 里实际存的参数项（带 # 的逐声道项不会出现在快照里）
//   - 设备侧 ret!=0（无值/不支持）视为不匹配 —— 宁可诚实显示「自定义状态」
//   - 文本精确比对（快照值 = 当时的设备读数原文，驱动格式稳定）
func identifyActiveScene(d *Driver) string {
	if d == nil || !d.ready {
		return ""
	}
	for _, name := range profileNamesForIdentify(d) {
		p, _, _ := d.profilePath(name)
		if p == "" {
			continue
		}
		items := profileItems(readText(p))
		if len(items) == 0 {
			continue
		}
		ok := true
		for _, it := range items {
			if it.Name == "" || strings.Contains(it.Name, "#") {
				continue
			}
			ep := EpRender
			if strings.EqualFold(it.Flow, "in") {
				ep = EpCapture
			}
			ret, val := d.ReadEp(ep, it.Name)
			// ★ ret=0 但值列读空 = 已知读抖动（--dump 同款）：重读一次再判，
			//   仍空才算不匹配（宁可显示「自定义状态」也不误报场景）。
			if ret == 0 && val == "" {
				ret, val = d.ReadEp(ep, it.Name)
			}
			if ret != 0 || val != it.Value {
				ok = false
				break
			}
		}
		if ok {
			return name
		}
	}
	return ""
}

// profileNamesForIdentify 识别候选（按列表顺序，内置在前）。
// ★ 不能走 d.Profiles() —— 那里面会调 identifyActiveScene，互相递归
// （2026-10-20 实测把 /api/profiles 挂死）；这里直接枚举场景文件名。
func profileNamesForIdentify(d *Driver) []string {
	seen := map[string]bool{}
	var names []string
	for _, pair := range profileDirs(d) {
		dir, _ := pair[0].(string)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var here []string
		for _, e := range entries {
			n := e.Name()
			if !strings.HasSuffix(strings.ToLower(n), ".profile") {
				continue
			}
			stem := n[:len(n)-len(".profile")]
			// 模板/当前标记（UserDefine*.cml 的同名档）与重复档跳过
			if stem == "" || stem+".cml" == profileTemplate ||
				stem+".cml" == profileCurrent || seen[stem] {
				continue
			}
			seen[stem] = true
			here = append(here, stem)
		}
		sort.Strings(here)
		names = append(names, here...)
	}
	return names
}

// cleanupLegacyLastApplied 上一轮「恢复上次场景」方案的标记文件已按
// 2026-10-20 用户拍板废弃 —— 启动时顺手删掉遗留文件。
func cleanupLegacyLastApplied(d *Driver) {
	dir := profileWriteDir(d)
	if dir == "" {
		return
	}
	os.Remove(filepath.Join(dir, "LastApplied.txt"))
}
