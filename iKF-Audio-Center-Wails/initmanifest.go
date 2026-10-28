package main

/*
initmanifest.go —— 「初始化清单」：初始化目标的**唯一真相**。

## 为什么要这个文件

2026-10-22 用户把这几轮修复归纳成一句话：「从卸载驱动到重新安装，我们在
设计一个场景 → 初始化，自定义场景里的默认就是我们的初始化」。

说得对，但当时的实现配不上这个说法 ——「初始化该写哪些项」散在**五处**：

	FACTORY_DEFAULTS        42 项（硬编码，含 17 项 SS_* 摆位）
	默认.profile            37 项（官方档，含 12 项采集/新参数）
	applySceneExtras        运行时补那 12 项（只补 ret==2）
	ensureChannelTopology   设备级 1 项（拓扑，不在任何表里）
	seedSceneDefaults       3 个哨兵（影子判据）

于是「初始化到底会写哪些项」这个问题只能靠读五个函数回答，而且每次修一个
漏洞就冒一个新的（10-20 到 10-22 连着冒了三次）。这里把并集算出来，做成
一份**可枚举的数据**，链、核对命令、闸三方共用同一份。

## 清单构成（2026-10-22 实测口径）

	出厂表 42 ∪ 默认档 37 = 54
	  交集 25 项（值逐项一致，无冲突 —— 冲突时以出厂表为准）
	  只在出厂表 17 项 SS_*（7.1 摆位角度/距离）
	  只在默认档 12 项 AGCControl / Enable_AEC_LFX / Enable_ENC_LFX /
	            Enable_MICARRAY_LFX / CONLAB_HPSPK_MODE·_ROOMSIZE·_SFX /
	            Enable_CONLAB_HPSPK_LFX / GFSControl / Enable_SMART_VOLUME_GFX /
	            Enable_SwapLfeCn_LFX / Enable_SwapSrBk_LFX
	+ 设备级 1 项 Speaker_Channel_Setting = 2（立体声）
	= 55 项

## 两项元数据

	source  factory / scene / device —— 这项从哪来（排障时要能倒查）
	scope   param / device           —— 执行策略不同：
	        param  安装级触发全量写，其它触发只补 ret==2（不覆盖用户调音）
	        device 任何触发都校正（拓扑是硬件事实，不是用户偏好）

与 Python 侧 build_init_manifest / apply_init_manifest 逐字段对应。
*/

import (
	"fmt"
	"strings"
)

// initItem 初始化清单的一项。
type initItem struct {
	Ep     string // EpRender / EpCapture
	Name   string
	Val    string
	Kind   string // "" / "bool" / "int"，由 spec() 反查
	Source string // factory / scene / device
	Scope  string // param / device
}

// initDeviceItems 设备级项（不在任何参数表里，硬件事实）。
// 目前只有声道拓扑：本机耳机就是立体声，4/6/8 是驱动默认值作祟。
// ★ S2 起真源 = model/device-model.toml [[init_device]]（1 项，装载时自校验）。
var initDeviceItems = func() []initItem {
	out := make([]initItem, 0, len(modelData.InitDevice))
	for _, kv := range modelData.InitDevice {
		out = append(out, initItem{Ep: kv.Ep, Name: kv.Name, Val: kv.Val,
			Source: "device", Scope: "device"})
	}
	return out
}()

// BuildInitManifest 汇总「初始化目标」= 出厂表 ∪ 「默认」场景档 ∪ 设备级。
//
// 顺序固定（出厂 → 场景补差 → 设备级），去重靠 seen。相同名字以**出厂表
// 为准**：两者值是逐项一致的（10-22 实测 25 项交集零冲突），但出厂表是
// 硬编码、不会被场景文件被改而漂移。
//
// 场景档读不到（首次运行还没播种）就只给 42 项 + 设备级 —— 不报错、不编值。
func BuildInitManifest(d *Driver) []initItem {
	out := make([]initItem, 0, len(factoryDefaults)+16)
	seen := map[string]bool{}
	add := func(it initItem) {
		key := it.Ep + "\x00" + it.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, it)
	}

	for _, it := range factoryDefaults {
		add(initItem{Ep: it.Ep, Name: it.Name, Val: it.Val,
			Kind: itemKind(it.Ep, it.Name), Source: "factory", Scope: "param"})
	}

	if d != nil {
		if p, _, ok := d.profilePath(profileFactoryScene); ok && p != "" {
			for _, it := range profileItems(readText(p)) {
				if it.Name == "" || excludedFromScene(it.Name) {
					continue // 逐声道项 / 场景排除名单，与 profileSave 口径一致
				}
				ep := EpRender
				if strings.EqualFold(it.Flow, "in") {
					ep = EpCapture
				}
				// 本机能力面没有的项写了也是白写 —— 不进清单（与旧
				// applySceneExtras 的 known 判据一致）。
				if _, known := spec(ep, it.Name); !known {
					continue
				}
				add(initItem{Ep: ep, Name: it.Name, Val: it.Value,
					Kind: itemKind(ep, it.Name), Source: "scene", Scope: "param"})
			}
		}
	}

	for _, it := range initDeviceItems {
		add(it)
	}
	return out
}

// itemKind 把 spec 的类型翻成 WriteAuto 认识的字符串（未知则空串，交给它自己判）。
func itemKind(ep, name string) string {
	if sp, known := spec(ep, name); known {
		return kindName(sp.kind)
	}
	return ""
}

// applyInitManifest 按清单执行。返回 (写入成功, 写入失败, 跳过)。
//
// strong=true  —— 全量写（安装级触发 / 用户点「默认」）：不看当前值，一律写。
//                 这是「装完驱动就该是默认状态」的唯一可靠实现。
// strong=false —— 只补 ret==2（普通启动）：有值一个字都不动，保护用户调音。
//
// 设备级项（scope=device）**不在这里写** —— 它要走 ensureChannelTopology 的
// 「写后回读 + 重试」，那条路径才有抗端点重建窗口的能力。
func applyInitManifest(d *Driver, items []initItem, strong bool) (int, int, int) {
	applied, failed, skipped := 0, 0, 0
	for _, it := range items {
		if it.Scope == "device" {
			continue
		}
		if !strong {
			if ret, _ := d.ReadEp(it.Ep, it.Name); ret != 2 {
				skipped++
				continue // 有值（0）或不存在（-1/-2）：都不是本次该改的
			}
		}
		if res := d.WriteAuto(it.Ep, it.Name, it.Val, it.Kind); res["ok"] == true {
			applied++
		} else {
			failed++
		}
	}
	return applied, failed, skipped
}

// InitCheckResult 一条核对结果。
type InitCheckResult struct {
	Total, OK, Missing, Drift, Unsupported int
}

// CheckInitManifest 逐项核对设备与清单（只读，绝不写）。
//
// 每一项归四类：
//
//	OK           ret=0 且值 == 目标
//	Missing      ret=2（存在但从没写过 —— 界面「未设置」）
//	Drift        ret=0 但值 != 目标（用户改过，或初始化没跑成）
//	Unsupported  ret<0（本机没有这项能力，不算失败）
func CheckInitManifest(d *Driver) (InitCheckResult, []string) {
	var r InitCheckResult
	var lines []string
	items := BuildInitManifest(d)
	r.Total = len(items)
	for _, it := range items {
		ret, val := d.ReadEp(it.Ep, it.Name)
		if ret == 0 && val == "" {
			ret, val = d.ReadEp(it.Ep, it.Name) // 已知读抖动，重读一次
		}
		switch {
		case ret == 0 && val == it.Val:
			r.OK++
			lines = append(lines, fmt.Sprintf("  达标  %-8s %-32s %-8s %s",
				it.Ep, it.Name, it.Val, it.Source))
		case ret == 2:
			r.Missing++
			lines = append(lines, fmt.Sprintf("  未设置 %-7s %-32s %-8s %s（存在但从未写过）",
				it.Ep, it.Name, it.Val, it.Source))
		case ret == 0:
			r.Drift++
			lines = append(lines, fmt.Sprintf("  偏离  %-8s %-32s 目标 %-6s 当前 %s",
				it.Ep, it.Name, it.Val, val))
		default:
			r.Unsupported++
			lines = append(lines, fmt.Sprintf("  不支持 %-8s %-32s ret=%d",
				it.Ep, it.Name, ret))
		}
	}
	return r, lines
}
