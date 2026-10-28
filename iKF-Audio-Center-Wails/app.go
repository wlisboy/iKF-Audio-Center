//go:build windows

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"golang.org/x/sys/windows"
)

// API 是 /api/* 契约的实现，与 Python 版（iKF-Audio-Center.py）逐字段一致。
//
// 挂载方式：assetserver.Options.Handler。
// Wails 的 assetHandler 先尝试从内嵌前端资源里取同名文件，取不到
// （os.ErrNotExist）才回落到这里，于是：
//
//	GET  /             → 内嵌 index.html，不经过本处理器
//	GET  /api/status   → 本处理器
//	POST /api/set      → 本处理器（非 GET 请求直接进处理器）
//
// 契约（务必与 Python 版保持一致，前端 BRIDGE_JS 依赖它）：
//
//	GET  /api/status          → status() 字典，直接返回，不包壳
//	GET  /api/params[?ep=]    → {name: {r,v,w,t}} 映射，**直接返回，不包壳**
//	GET  /api/meter[?ep=]     → {ok, peak, ep, channels}
//	GET  /api/ping            → {ok, app, ver}
//	GET  /api/profiles        → {ok, dir, writable, factory, list:[…]}
//	                            list 每项含 {name, desc, builtin, dir, items,
//	                            has_data, groups:[中文短标签]}
//	GET  /api/profile/items?name= → {ok, name, builtin, dir, count,
//	                            groups:[{key,label,ep,n}], pairs:{"ep|属性":值}}
//	                            场景详情与「已修改未保存」脏标记的数据源
//	GET  /api/driver          → {ok, ready, devices, capture, cpl, dll, error,
//	                            setup, setup_ok}（主程序眼里的驱动现状，见 setup.go）
//	POST /api/driver/setup    → {action:'install'|'uninstall'|''} → 提权拉起同目录的
//	                            iKF-驱动管理.exe 并等它结束，再重连驱动。
//	                            **会阻塞数十秒**，前端要转圈等
//	POST /api/set             → {name, value[, ep][, kind]} → write() 结果
//	                            kind ∈ bool|int|float，只对白名单外属性生效
//	POST /api/test            → {action:'play'|'stop'|'close', mask} → 声道测试
//	POST /api/listen          → {on: bool} → 麦克风监听
//	POST /api/asio            → ShowAsioPanel
//	POST /api/profile/save    → {name, desc} → 快照成组态（同名即覆盖）
//	POST /api/profile/apply   → {name} → 把组态写回驱动
//	POST /api/profile/delete  → {name} → 删除用户自建组态
//	POST /api/profile/rename  → {name, new_name} → 用户自建组态改名
//
// 可视化页的音源（系统回环采集，见 loopback.go）：
//
//	GET  /api/loopback/devices → {ok, default, list:[{id,name,def}]}
//	                             可锁定的播放端点（仅耳机可视化的下拉数据源）
//	GET  /api/loopback/status → {ok, running, rate, ch, format, bins, frames,
//	                             level, dev[, error][, uptime]}
//	POST /api/loopback/start  → {device} 可选：端点 ID，空/缺省 = 默认播放设备
//	                             → {ok[, already], rate, ch, format, dev}
//	                             | {ok:false, error}
//	POST /api/loopback/stop   → {ok[, already]}
//	GET  /api/spectrum        → {ok, running, rate, ch, bins(base64, 512B),
//	                             level, frames}
//
// spectrum 的 512 个字节与浏览器 AnalyserNode.getByteFrequencyData 同语义
// （见 loopback.go 顶部），所以前端两条音源可以走同一套渲染。
//
// ep 取 "render"（默认）或 "capture"。麦克风侧参数只存在于采集端点。
type API struct{}

// lb 是全局唯一的系统回环采集实例。
//
// 与 drv 不同，它**不**在包初始化时就开始采集 —— 采集要等前端点「开始」，
// 而且会让系统托盘的「正在共享/播放」指示亮起，静默开着不合适。
var lb = NewLoopback()

// epOf 解析 ?ep=，非法值回落渲染端点
func epOf(r *http.Request) string {
	if r.URL.Query().Get("ep") == EpCapture {
		return EpCapture
	}
	return EpRender
}

func (a *API) json(w http.ResponseWriter, code int, obj interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	// 关掉 HTML 转义。Go 的默认行为会把 & < > 写成 \u0026 \u003c \u003e，
	// 设备 ID（usb\vid_0d8c&pid_0012&mi_00\...）会因此变成一串转义序列。
	// 解析后语义相同，但为了让线上契约与 Python 版字面一致，这里关掉。
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/status":
		a.json(w, http.StatusOK, drv.Status())
	case r.Method == http.MethodGet && r.URL.Path == "/api/params":
		// 直接返回映射本身。前端拿到的 PM 就是 {name: {...}}，
		// 多包一层 {ok, params} 会让 PM['EQ_Slider1'] 变成 undefined。
		a.json(w, http.StatusOK, drv.ParamMapEp(epOf(r)))
	case r.Method == http.MethodGet && r.URL.Path == "/api/meter":
		a.json(w, http.StatusOK, drv.Meter(epOf(r)))
	case r.Method == http.MethodGet && r.URL.Path == "/api/ping":
		a.json(w, http.StatusOK, map[string]interface{}{
			"ok": true, "app": appName, "ver": appVer,
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/profiles":
		a.json(w, http.StatusOK, drv.Profiles())
	case r.Method == http.MethodGet && r.URL.Path == "/api/driver":
		a.json(w, http.StatusOK, drv.DriverInfo())
	// ★ 2026-10-21：界面上「重新检测」的后端入口（与 Python 线同契约）。
	//   松开旧上下文 → 重新枚举端点，返回的就是 DriverInfo，ready 即结果。
	case r.Method == http.MethodPost && r.URL.Path == "/api/driver/reopen":
		a.json(w, http.StatusOK, drv.ReopenInfo())
	case r.Method == http.MethodPost && r.URL.Path == "/api/driver/setup":
		a.driverSetup(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/openurl":
		// 2026-10-20（用户要求）：品牌区作者名点击 → 打开抖音主页。
		// 自定义协议（snssdk1128://）不能让 WebView2 自己导航（会把整个
		// 应用导航走/白屏），必须走系统 ShellExecute —— OS 自动拉起抖音，
		// 未安装时弹系统的「选择应用」。★ 安全：只放行 snssdk1128:// 前缀，
		// 不做通用 openurl（否则等于给前端开了任意命令执行口子）。
		raw := r.URL.Query().Get("url")
		// 白名单（2026-10-20 用户改定）：抖音网页版主页 https 链接为主
		// （默认浏览器打开，不依赖装没装抖音客户端），snssdk1128:// 协议
		// 链接保留兼容。其余一律拒绝。
		if !strings.HasPrefix(raw, "https://www.douyin.com/user/") &&
			!strings.HasPrefix(raw, "snssdk1128://") {
			a.json(w, http.StatusOK, map[string]interface{}{
				"ok": false, "error": "仅支持抖音主页链接"})
			return
		}
		err := windows.ShellExecute(0,
			windows.StringToUTF16Ptr("open"),
			windows.StringToUTF16Ptr(raw),
			nil, nil, windows.SW_SHOWNORMAL)
		ok := err == nil
		resp := map[string]interface{}{"ok": ok}
		if !ok {
			resp["error"] = "无法打开（可能未安装抖音）"
		}
		a.json(w, http.StatusOK, resp)
	// ★ 2026-10-20 内嵌录音机（recorder.go）：官方捆绑 Recorder.exe 是
	//   依附官方面板 COM 环境的死件（脱离面板拉起必退 241，取证见
	//   工具/录音机取证-Recorder.exe死件.md），旧 POST /api/recorder
	//   「拉起官方 exe」语义废弃，改为 start/stop/status 三端点。
	//   晚间用户拍板迁移：声音页「耳机录音」=渲染端点 loopback，
	//   WAV 落「音乐\iKF Audio Center\」（原麦克风录音废弃）。
	case r.Method == http.MethodPost && r.URL.Path == "/api/recorder/start":
		a.json(w, http.StatusOK, rec.Start())
	case r.Method == http.MethodPost && r.URL.Path == "/api/recorder/stop":
		a.json(w, http.StatusOK, rec.Stop())
	case r.Method == http.MethodGet && r.URL.Path == "/api/recorder/status":
		a.json(w, http.StatusOK, rec.Status())
	// ★ 2026-10-20 晚：录音灵动岛「打开文件夹」—— 资源管理器打开录音
	//   所在目录并选中该 WAV（explorer /select,）。与 Python 线逐字段
	//   一致；ShellExecute 走系统 shell，路径不落任何解释器。
	case r.Method == http.MethodPost && r.URL.Path == "/api/recorder/open":
		a.recOpen(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/profile/items":
		a.json(w, http.StatusOK, drv.ProfileItems(r.URL.Query().Get("name")))
	case r.Method == http.MethodPost && r.URL.Path == "/api/set":
		a.set(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/test":
		a.test(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/listen":
		a.listen(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/listen/status":
		a.json(w, http.StatusOK, lis.Status())
	case r.Method == http.MethodPost && r.URL.Path == "/api/asio":
		a.json(w, http.StatusOK, drv.ShowAsio())
	case r.Method == http.MethodPost && r.URL.Path == "/api/profile/save":
		a.profileOp(w, r, "save")
	case r.Method == http.MethodPost && r.URL.Path == "/api/profile/apply":
		a.profileOp(w, r, "apply")
	case r.Method == http.MethodPost && r.URL.Path == "/api/profile/delete":
		a.profileOp(w, r, "delete")
	case r.Method == http.MethodPost && r.URL.Path == "/api/profile/rename":
		a.profileOp(w, r, "rename")
	case r.Method == http.MethodGet && r.URL.Path == "/api/loopback/devices":
		a.json(w, http.StatusOK, lb.Devices())
	case r.Method == http.MethodGet && r.URL.Path == "/api/loopback/status":
		a.json(w, http.StatusOK, lb.Status())
	case r.Method == http.MethodPost && r.URL.Path == "/api/loopback/start":
		a.loopbackStart(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/loopback/stop":
		a.json(w, http.StatusOK, lb.Stop())
	case r.Method == http.MethodGet && r.URL.Path == "/api/spectrum":
		a.json(w, http.StatusOK, lb.Spectrum())
	default:
		a.json(w, http.StatusNotFound, map[string]interface{}{
			"ok": false, "error": "not found",
		})
	}
}

// recOpen 处理 /api/recorder/open：资源管理器打开录音所在文件夹并**选中**
// 该 WAV（explorer /select,）。路径来自前端透传（stop 响应的 file），
// 只作 shell 参数不落解释器。ShellExecute 拉起 explorer，不引 os/exec。
func (a *API) recOpen(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var req struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Path == "" {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "缺少 path"})
		return
	}
	err := windows.ShellExecute(0,
		windows.StringToUTF16Ptr("open"),
		windows.StringToUTF16Ptr("explorer"),
		windows.StringToUTF16Ptr("/select,"+req.Path),
		nil, windows.SW_SHOWNORMAL)
	if err != nil {
		a.json(w, http.StatusOK, map[string]interface{}{
			"ok": false, "error": err.Error()})
		return
	}
	a.json(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// driverSetup 处理 /api/driver/setup。
//
// 会把同目录的驱动管理程序提权拉起来并**等它结束**，所以这是一个慢接口：
// 装驱动正常要 10–30 秒，卸载几秒，用户还可能对着 UAC 想一会儿。
// 返回的就是 DriverInfo()，只是多带了 action / exit_code，
// 前端可以直接拿 ready 判断「装完能不能用了」。
func (a *API) driverSetup(w http.ResponseWriter, r *http.Request) {
	action := ""
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 4096)); len(body) > 0 {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.json(w, http.StatusBadRequest, map[string]interface{}{
				"ok": false, "error": "请求体解析失败",
			})
			return
		}
		action = req.Action
	}
	// ★破坏性检测 P1-1：空 action 此前被放行 → RunSetup("") 走默认分支
	// = 主动关驱动上下文 + 弹提权安装器并阻塞 —— 一条误调用就能断驱动。
	// 非法值一律 400，驱动管理只允许显式调用。
	if action != "install" && action != "uninstall" {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "action 只能是 install 或 uninstall",
		})
		return
	}
	a.json(w, http.StatusOK, drv.RunSetup(action))
}

// loopbackStart 处理 /api/loopback/start。
//
// 请求体是可选的 {"device": "<端点 ID>"}：空体 / 空串 = 跟随默认播放设备
// （旧行为，老前端不带头也能用）；带了就锁定那个端点。请求体解析失败按
// 400 回，与其它 POST 端点的口径一致。
func (a *API) loopbackStart(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	device := ""
	if len(body) > 0 {
		var req struct {
			Device string `json:"device"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			a.json(w, http.StatusBadRequest, map[string]interface{}{
				"ok": false, "error": "请求体解析失败",
			})
			return
		}
		device = req.Device
	}
	a.json(w, http.StatusOK, lb.Start(device))
}

func (a *API) set(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))

	var req struct {
		Name  string      `json:"name"`
		Value interface{} `json:"value"`
		Ep    string      `json:"ep"`
		Kind  string      `json:"kind"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "请求体解析失败",
		})
		return
	}
	if req.Name == "" {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "缺少参数名",
		})
		return
	}
	ep := EpRender
	if req.Ep == EpCapture {
		ep = EpCapture
	}
	// WriteAuto：白名单内按界面控件钳制；白名单外但属于 208 项的
	// （如音箱方位 SS_*_ANGLE）也真写一次，由驱动决定收不收。
	a.json(w, http.StatusOK, drv.WriteAuto(ep, req.Name, req.Value, req.Kind))
}

func (a *API) test(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var req struct {
		Action string `json:"action"`
		Mask   int    `json:"mask"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "请求体解析失败",
		})
		return
	}
	switch req.Action {
	case "stop":
		a.json(w, http.StatusOK, drv.StopTest())
	case "close":
		a.json(w, http.StatusOK, drv.CloseTest())
	default:
		a.json(w, http.StatusOK, drv.PlayTest(req.Mask))
	}
}

func (a *API) listen(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var req struct {
		On   bool     `json:"on"`
		Gain *float64 `json:"gain"` // 可选：监听增益 1.0-8.0（滑杆实时调）
	}
	if err := json.Unmarshal(body, &req); err != nil {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "请求体解析失败",
		})
		return
	}
	// 2026-10-20：官方 RunListen2Device 依赖的 COM 服务器在驱动包里
	// 不存在（见 工具/监听取证-监听COM组件缺失.md），改内嵌 WASAPI
	// 监听（listen.go）。响应保留 {ok, on, error} 形状，前端零改动。
	if req.Gain != nil {
		a.json(w, http.StatusOK, lis.SetGain(*req.Gain))
		return
	}
	out := map[string]interface{}{"on": req.On}
	if req.On {
		out = lis.Start()
	} else {
		out = lis.Stop()
	}
	out["on"] = req.On
	a.json(w, http.StatusOK, out)
}

// profileOp 处理 /api/profile/{save,apply,delete,rename}。
// 四个动作共用一套请求体解析，避免四份重复的 400 分支。
func (a *API) profileOp(w http.ResponseWriter, r *http.Request, op string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
	var req struct {
		Name    string `json:"name"`
		Desc    string `json:"desc"`
		NewName string `json:"new_name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		a.json(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "请求体解析失败",
		})
		return
	}
	switch op {
	case "save":
		a.json(w, http.StatusOK, drv.ProfileSave(req.Name, req.Desc))
	case "apply":
		a.json(w, http.StatusOK, drv.ProfileApply(req.Name))
	case "rename":
		a.json(w, http.StatusOK, drv.ProfileRename(req.Name, req.NewName))
	default:
		a.json(w, http.StatusOK, drv.ProfileDelete(req.Name))
	}
}

// ★ 2026-10-20：recorderPath / launchRecorder 已删除 —— 官方捆绑
// Recorder.exe 是死件（见 工具/录音机取证-Recorder.exe死件.md），
// 录音机改为内嵌实现（recorder.go 的 rec 实例，三端点见上方路由）。
