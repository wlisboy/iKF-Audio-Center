package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// ensureChannelTopology 声道拓扑必须是**立体声（2 声道）**。
//
// 两种局面都要处理（2026-10-22 用户虚拟机实测前只处理了后一半）：
//
//	① ret != 0（没有记忆值）—— 装完驱动 / 端点重建后的常态。以前只有
//	   启动路径会播种，而装驱动那一刻主程序往往已经在跑，「重新检测」
//	   连上后没人再管它，设备页声道组就停在「四个 chip 全灭」。
//	② 值为 4/6/8 —— 声道拓扑是驱动服务级状态，重启/重新枚举后会被复位
//	   成驱动的默认 5.1；本机耳机硬件就是立体声，纯属驱动默认值作祟。
//
// ★ 写后必须**回读确认**，不能只看写入返回码。用户实测现象：装完驱动
// 首次启动声道组不是立体声，退出主程序再打开就对了 —— 说明那次写入
// 落在端点重建窗口里被丢弃了，而写入本身返回成功。所以这里最多重试
// 3 次、每次间隔 200ms，只有回读到 "2" 才算成功。
//
// 除 2/4/6/8 之外的怪值一律不猜（读不稳定时宁可不动），不替驱动下结论。
func ensureChannelTopology(d *Driver) (bool, string) {
	if d == nil || !d.ready {
		return false, ""
	}
	isStereo := func(s string) bool {
		s = strings.TrimSpace(s)
		return s == "2" || s == "2.0"
	}
	ret, v := d.Read("Speaker_Channel_Setting")
	if ret == 0 && v == "" {
		ret, v = d.Read("Speaker_Channel_Setting") // 已知读抖动，重读一次
	}
	if ret == 0 && isStereo(v) {
		return false, ""
	}
	var why string
	switch {
	case ret != 0:
		why = "无记忆值（驱动重装/端点重建）"
	case v == "4" || v == "6" || v == "8":
		why = "驱动复位成 " + v
	default:
		return false, ""
	}
	for i := 0; i < 3; i++ {
		d.WriteAuto(EpRender, "Speaker_Channel_Setting", 2, "")
		if r, w := d.Read("Speaker_Channel_Setting"); r == 0 && isStereo(w) {
			return true, fmt.Sprintf("%s → 立体声 2（第 %d 次写入生效）", why, i+1)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false, fmt.Sprintf("%s → 立体声 2 写入 3 次均未生效（端点可能仍在重建）", why)
}

// initstate.go —— 连上驱动后必须跑一次的初始化链。
//
// 为什么抽成一个函数：**启动路径**和**「重新检测」/装驱动重连**做的是同一件事
// （把设备恢复到可用起点 = 「默认」场景状态），以前只有启动路径跑它，
// 于是用户最常见的那条路径正好错过：
//
//	装驱动 → 主程序已在跑（或启动那刻驱动还没装好）→ Open 失败/半就绪
//	→ 启动时的 RestoreDefaultScene 因 !ready 直接返回
//	→ 用户点「重新检测」→ Reopen 成功 → 没人再跑恢复
//	→ 设备全 ret=2，界面一片「未设置」，用户必须自己去场景页点「默认」才写回来
//
// 2026-10-22 用户实测：「得我去自定义场景点默认然后所有参数才会按照默认去设定」。
//
// 幂等性由被调函数各自保证，可以放心反复跑：
//   - RestoreDefaultScene：指纹保护，出厂默认表缺省 < 21/42 项就一个字都不写
//   - HealMissingParams  ：只补 ret==2 的项，有值的不动
//   - EnsureSeedScenes   ：已有档且项数够就不动
//
// full=true 给启动路径用，多做两件**一次性**的事（清废弃标记文件、场景分组名
// 就地升级）；重连路径不需要，跑它们只是白花时间。与 Python 线
// ensure_initial_state(drv, full) 逐字段对应。
func EnsureInitialState(d *Driver, full bool) map[string]interface{} {
	out := map[string]interface{}{}
	if d == nil || !d.ready {
		/* 没连上就什么都别碰。注意这里不能"尽力而为"地写 —— 半就绪时
		   采集端点读回 -9（不是 2），指纹会算错，写进去的也是半套。 */
		return out
	}

	if full {
		cleanupLegacyLastApplied(d)
	}

	/* ★ 2026-10-22：声道拓扑排在最前，且放进这条链里 —— 它以前只挂在
	   启动路径上，装完驱动点「重新检测」连上后没人补，设备页声道组就
	   不是立体声。放最前还有一个原因：切拓扑可能触发端点属性重建，
	   必须在写参数**之前**把这件事做完，否则刚写好的参数会被打断。 */
	if changed, msg := ensureChannelTopology(d); changed {
		out["topology"] = msg
		log.Printf("[初始化] 声道拓扑：%s", msg)
		time.Sleep(150 * time.Millisecond) // 给驱动一点时间把属性落稳
	} else if msg != "" {
		log.Printf("[初始化] 声道拓扑：%s", msg) // 没写成功也要留痕
	}

	/* ★ 2026-10-22 第三版：先问「驱动换过一茬没有」（initfp.go）。
	   以前唯一的判据是 21/42 启发式（缺省过半才算被重置），它测的是
	   「值在不在」，而用户要的是「驱动被重装过」—— 两者不等价：驱动装
	   了一半（20/42 缺省）就不写，驱动升级后把旧值搬回来也不写。
	   现在指纹（端点 GUID + 驱动版本 + 设备实例）变化 ⟹ 直接全量写，
	   不再看缺省多少。

	   顺序铁律不变：恢复必须在自愈**之前**。自愈把 ret=2 项补成默认值后，
   	   「端点被重置」的启发式指纹同样测不出来了（2026-10-20）。 */
	fpChanged, fpWhy := initFpChanged(d)
	fullWriteOK := true
	if fpChanged {
		items := BuildInitManifest(d)
		applied, failed, _ := applyInitManifest(d, items, true)
		fullWriteOK = failed == 0
		msg := fmt.Sprintf("安装指纹变化（%s），已按初始化清单全量写回（成功 %d / 失败 %d / 共 %d 项）",
			fpWhy, applied, failed, len(items)-len(initDeviceItems))
		out["restored"] = true
		out["restore_reason"] = fpWhy
		out["restore_msg"] = msg
		log.Printf("[初始化] %s", msg)
	} else if triggered, msg := RestoreDefaultScene(d, false); triggered {
		out["restored"] = true
		out["restore_msg"] = msg
		log.Printf("[初始化] 场景自动恢复：%s", msg)
	}

	if n := HealMissingParams(d); n > 0 {
		out["healed"] = n
		log.Printf("[初始化] 设备侧自愈：补写了 %d 项出厂默认值", n)
	}

	/* 播种只在设备**真的被改过**时才做。EnsureSeedScenes 内部要走
	   fullSnapshot（实读全部可写项），白跑一次是几百毫秒 ——
	   重连是用户点按钮等着的交互，不能拿这个拖它。
	   启动路径（full）无论如何都要跑：首次运行没有内置档。 */
	if full || out["restored"] != nil || out["healed"] != nil {
		EnsureSeedScenes(d)
	}

	if full {
		if mf, mt := MigrateProfileGroups(d); mf > 0 {
			out["migrated"] = mf
			log.Printf("[初始化] 场景分组名升级：%d 个文件的 %d 个 <ApoEffect> 换成官方页名",
				mf, mt)
		}
	}

	/* 指纹落盘 = 「这台机器这个驱动版本，我已经初始化过了」。
	   放在最后：前面所有写设备的动作都结束了，记下的才是真实现状。

	   ★ 全量写有失败项时**不落指纹** —— 下次启动会再判一次「变了」，
	     重试那一轮；补空缺（HealMissingParams）紧跟在上一步跑过，
	     能补的都补了，这里落盘不会掩盖半套状态。
	   ★ 首次运行（无基线）也走这里：只记基线、不写设备（initfp.go
	     安全规则 1），所以老用户升级上来调音不会被清。 */
	if fpChanged && !fullWriteOK {
		log.Printf("[初始化] 全量写有失败项，指纹暂不更新（下次启动重试）")
	} else if err := saveInitFp(d); err != nil {
		log.Printf("[初始化] 安装指纹写入失败：%v", err)
	}
	return out
}
