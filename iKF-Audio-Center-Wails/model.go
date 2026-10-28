//go:build windows

package main

// model.go —— device-model.toml（数据即真源）的 Go 侧装载器。
//
// S1 范围：场景排除名单 / 内置场景 / 分组别名。后续 S2-S4 再接入
// 出厂表、名字表、白名单、分组名（见 docs/device-model-抽取清单.md）。
//
// 失败策略：文件缺失、解析失败、自校验不过 = 拒绝启动。
// 绝不内置兜底表 —— 兜底就是真源漂移复发（数据真源的唯一纪律）。

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

//go:embed model/device-model.toml
var modelToml []byte

type modelFile struct {
	Meta struct {
		SchemaVersion int    `toml:"schema_version"`
		Device        string `toml:"device"`
	}
	Scene struct {
		FactoryProfile string   `toml:"factory_profile"`
		FactoryScene   string   `toml:"factory_scene"`
		Builtin        []string `toml:"builtin"`
		Exclude        []string `toml:"exclude"`
		Unproven       []string `toml:"unproven"`
	}
	GroupAlias map[string]string `toml:"group_alias"`

	// S2：出厂表 42 + 初始化设备级 1
	Factory    []modelKV `toml:"factory"`
	InitDevice []modelKV `toml:"init_device"`

	// S3：属性名表 208 + 可写白名单 80
	Names    modelNames `toml:"names"`
	Writable []modelW   `toml:"writable"`

	// S4：分组映射 53 / 标签 28 / 展示顺序 27 / 端点元数据 / ValueType
	ProfileGroups     map[string]map[string]string `toml:"profile_groups"`
	GroupLabel        map[string]string            `toml:"group_label"`
	Display           struct {
		GroupOrder []string `toml:"group_order"`
	} `toml:"display"`
	ProfileEndpoint   map[string][]string `toml:"profile_endpoint"`
	ProfileValueType  map[string]string   `toml:"profile_value_type"`
}

// modelNames [names]：osConfLib.dll 两张属性名表，顺序即下标，不许重排。
type modelNames struct {
	Generic []string `toml:"generic"`
	Oem     []string `toml:"oem"`
}

// modelW [[writable]] 的一项：ep/name/t + 可选 lo/hi/choices。
type modelW struct {
	Ep      string `toml:"ep"`
	Name    string `toml:"name"`
	T       string `toml:"t"`
	Lo      *float64
	Hi      *float64
	Choices []int `toml:"choices"`
	Notes   string `toml:"notes"`
}

// modelKV [[factory]] / [[init_device]] 的一项。
type modelKV struct {
	Ep    string `toml:"ep"`
	Name  string `toml:"name"`
	Val   string `toml:"val"`
	Notes string `toml:"notes"`
}

// strSet 字符串切片 → 集合（本项目的名单全是集合语义）。
func strSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

func mustLoadModel() *modelFile {
	var m modelFile
	if _, err := toml.Decode(string(modelToml), &m); err != nil {
		fmt.Fprintf(os.Stderr, "[model] device-model.toml 解析失败: %v\n", err)
		os.Exit(2)
	}
	if m.Meta.SchemaVersion != 1 {
		fmt.Fprintf(os.Stderr, "[model] schema_version=%d，本程序要求 1\n", m.Meta.SchemaVersion)
		os.Exit(2)
	}
	// 自校验基线（与 Python 侧同一张表，漂移即拒绝启动）
	if n, u := len(m.Scene.Exclude), len(m.Scene.Unproven); n != 6 || u != 35 {
		fmt.Fprintf(os.Stderr, "[model] 排除名单规模漂移: exclude=%d(应6) unproven=%d(应35)\n", n, u)
		os.Exit(2)
	}
	if len(m.Scene.Builtin) != 6 {
		fmt.Fprintf(os.Stderr, "[model] 内置场景漂移: %d(应6)\n", len(m.Scene.Builtin))
		os.Exit(2)
	}
	if len(m.Scene.FactoryProfile) == 0 || len(m.Scene.FactoryScene) == 0 {
		fmt.Fprintln(os.Stderr, "[model] factory_profile / factory_scene 不能为空")
		os.Exit(2)
	}
	if len(m.GroupAlias) != 10 {
		fmt.Fprintf(os.Stderr, "[model] 分组别名漂移: %d(应10)\n", len(m.GroupAlias))
		os.Exit(2)
	}
	// S2 基线：出厂表 42（渲染 36 + 采集 6）+ 设备级 1
	if len(m.Factory) != 42 {
		fmt.Fprintf(os.Stderr, "[model] 出厂表漂移: %d(应42)\n", len(m.Factory))
		os.Exit(2)
	}
	if len(m.InitDevice) != 1 || m.InitDevice[0].Name != "Speaker_Channel_Setting" {
		fmt.Fprintln(os.Stderr, "[model] 初始化设备级漂移：应恰好 1 项 Speaker_Channel_Setting")
		os.Exit(2)
	}
	// S3 基线：名字表 208（112+96）+ 白名单 80（render 65 / capture 15，自检口径）
	if len(m.Names.Generic) != 112 || len(m.Names.Oem) != 96 {
		fmt.Fprintf(os.Stderr, "[model] 名字表漂移: generic=%d(应112) oem=%d(应96)\n",
			len(m.Names.Generic), len(m.Names.Oem))
		os.Exit(2)
	}
	if len(m.Writable) != 80 {
		fmt.Fprintf(os.Stderr, "[model] 白名单漂移: %d(应80)\n", len(m.Writable))
		os.Exit(2)
	}
	rc := map[string]int{"render": 0, "capture": 0}
	known := map[string]bool{"bool": true, "int": true, "float": true}
	for _, w := range m.Writable {
		rc[w.Ep]++
		if !known[w.T] || (w.Ep != "render" && w.Ep != "capture") {
			fmt.Fprintf(os.Stderr, "[model] 白名单条目非法: %s/%s t=%s ep=%s\n", w.Ep, w.Name, w.T, w.Ep)
			os.Exit(2)
		}
		if (w.Lo == nil) != (w.Hi == nil) {
			fmt.Fprintf(os.Stderr, "[model] 白名单 %s：lo/hi 必须成对\n", w.Name)
			os.Exit(2)
		}
	}
	if rc["render"] != 65 || rc["capture"] != 15 {
		fmt.Fprintf(os.Stderr, "[model] 白名单端点分布漂移: render=%d(应65) capture=%d(应15)\n", rc["render"], rc["capture"])
		os.Exit(2)
	}
	// S4 基线：分组 53 / 标签 28 / 顺序 27；顺序与标签必须互洽
	gn := 0
	for _, tbl := range m.ProfileGroups {
		gn += len(tbl)
	}
	if gn != 53 {
		fmt.Fprintf(os.Stderr, "[model] 分组映射漂移: %d(应53)\n", gn)
		os.Exit(2)
	}
	if len(m.GroupLabel) != 28 || len(m.Display.GroupOrder) != 27 {
		fmt.Fprintf(os.Stderr, "[model] 标签/顺序漂移: %d(应28) / %d(应27)\n",
			len(m.GroupLabel), len(m.Display.GroupOrder))
		os.Exit(2)
	}
	for _, g := range m.Display.GroupOrder {
		if _, ok := m.GroupLabel[g]; !ok {
			fmt.Fprintf(os.Stderr, "[model] 顺序里有组没有标签: %s\n", g)
			os.Exit(2)
		}
	}
	if len(m.ProfileEndpoint) != 2 || len(m.ProfileValueType) != 3 {
		fmt.Fprintf(os.Stderr, "[model] 端点元数据/ValueType 漂移\n")
		os.Exit(2)
	}
	return &m
}

// modelData 包级初始化：Go 依赖分析保证先于下面四个集合求值。
var modelData = mustLoadModel()
