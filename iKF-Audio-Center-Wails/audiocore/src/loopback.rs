//! WASAPI 回环采集会话 —— Go loopback.go session() 的 Rust 移植。
//!
//! 线程模型：open() 在调用线程上完成设备打开（同步，失败即 Err），
//! 采集循环跑在自带线程上（MTA 无 apartment 亲和，指针移交安全）。
//!
//! 逐条复刻的行为（改语义 = 破坏双跑对比，不许）：
//!   * 共享模式**必须**用 GetMixFormat 初始化
//!   * Initialize: SHARED | LOOPBACK，200ms 缓冲（100ns 单位），周期参数 0
//!   * 5ms 轮询而非 EVENTCALLBACK（事件驱动回环历史版本返回 E_INVALIDARG）
//!   * 每轮最多 64 个包；SILENT 标志或空指针 → 写静音
//!   * downmix 只取前左/前右平均；四种格式 + 未知格式写静音
//!   * GetNextPacketSize 失败 → 会话结束，err = "回环流已中断"
//!   * 20ms 节拍算一次频谱（平滑每调用一次施加一次）

use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use windows::core::{GUID, Interface as _, PCWSTR};
use windows::Win32::Foundation::PROPERTYKEY;
use windows::Win32::Media::Audio::{
    IAudioCaptureClient, IAudioClient, IMMDevice, IMMDeviceCollection, IMMDeviceEnumerator,
    WAVEFORMATEX, WAVEFORMATEXTENSIBLE, AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK,
    DEVICE_STATE_ACTIVE, EDataFlow, ERole,
};
use windows::Win32::System::Com::{
    CoCreateInstance, CoInitializeEx, CoTaskMemFree, CLSCTX_ALL, COINIT_MULTITHREADED, STGM,
    STGM_READ,
};
use windows::Win32::System::Com::StructuredStorage::{PropVariantClear, PROPVARIANT};
use windows::Win32::UI::Shell::PropertiesSystem::IPropertyStore;

use crate::spectrum::{decode_block, Spectrum};

pub const RING_LEN: usize = crate::spectrum::RING_LEN;
pub const FFT_SIZE: usize = crate::spectrum::FFT_SIZE;
pub const BINS: usize = crate::spectrum::BINS;

const CLSID_MMDeviceEnumerator: GUID =
    GUID::from_u128(0xBCDE0395_E52F_467C_8E3D_C4579291692E);

// 与 Go 版错误文案逐字对齐（前端/闸直接匹配这些串）。
pub const ERR_ENUMERATOR: &str = "无法创建音频设备枚举器";
pub const ERR_DEVICE_NOT_FOUND: &str = "找不到指定播放设备（已拔出或端点 ID 失效？）";
pub const ERR_NO_DEFAULT: &str = "找不到默认播放设备（耳机没插好？）";
pub const ERR_ACTIVATE: &str = "无法激活 IAudioClient";
pub const ERR_MIX_FORMAT: &str = "无法读取混音格式";
pub const ERR_MIX_BAD: &str = "混音格式异常";
pub const ERR_INIT: &str = "打开耳机音频通道失败，请重新插拔耳机后重试";
pub const ERR_SERVICE: &str = "无法获取 IAudioCaptureClient";
pub const ERR_START: &str = "启动耳机音频通道失败，请重新插拔耳机后重试";
pub const ERR_BROKEN: &str = "回环流已中断";

const E_RENDER: i32 = 0; // eDataFlow eRender
const E_CONSOLE: i32 = 0; // eRole eConsole
const AUDCLNT_SILENT: u32 = 0x00000002;
const WAVE_FORMAT_EXT_TAG: u16 = 0xFFFE;

/// 打开后的格式快照（与 Go Loopback 可见字段一一对应）。
#[derive(Clone)]
pub struct SessionInfo {
    pub rate: i32,
    pub ch: i32,
    pub kind: String,
    pub bits: i32,
    pub raw_tag: u16,
    pub sub_tag: u16,
    pub chan_mask: u32,
    pub block_align: i32,
    pub dev_name: String,
}

/// 会话运行态（Go Loopback.mu 保护区的对应物）。
pub struct LoopbackState {
    pub running: AtomicBool,
    pub rate: AtomicI32,
    pub ch: AtomicI32,
    pub bits: AtomicI32,
    pub raw_tag: AtomicU32,
    pub sub_tag: AtomicU32,
    pub chan_mask: AtomicU32,
    pub block_align: AtomicI32,
    pub frames: AtomicU64,
    pub err_code: AtomicI32, // 0=无 1=错误
    pub kind: Mutex<String>,
    pub dev: Mutex<String>,
    pub err: Mutex<String>,
}

/// ABI 状态快照（repr(C)，Go 侧同名结构逐字段映射）。
/// 字符串字段定长 UTF-8 + NUL 截断，避开跨语言分配。
#[repr(C)]
pub struct LbStatusOut {
    pub running: i32,
    pub rate: i32,
    pub ch: i32,
    pub bits: i32,
    pub raw_tag: u16,
    pub sub_tag: u16,
    pub chan_mask: u32,
    pub block_align: i32,
    pub frames: u64,
    pub level: f32,
    pub err_code: i32,
    pub kind: [u8; 16],
    pub dev: [u8; 256],
    pub err: [u8; 256],
}

/// COM 会话句柄跨线程载体（MTA 指针本就无亲和；Send 只是绕过编译器保守检查）。
/// ★ 按整体 move 进闭包（2021 版字段级捕获会对 `h.0` 单独做 Send 检查）。
struct LbHandles {
    client: *mut core::ffi::c_void,
    capture: *mut core::ffi::c_void,
    dev: *mut core::ffi::c_void,
    mix: usize,
}
unsafe impl Send for LbHandles {}

/// 一路会话。Go 的 Loopback + worker(stop,done) 合体。
pub struct LoopbackSession {
    pub state: Arc<LoopbackState>,
    /// ★Arc 共享：capture_loop 与 spectrum()/level()/close() 必须是**同一个**
    /// Spectrum 实例——S5 前版曾用局部实例导致"采集在跑、频谱全零"的断链。
    spec: Arc<Mutex<Spectrum>>,
    stop: Arc<AtomicBool>,
    th: Option<std::thread::JoinHandle<()>>,
}

impl LoopbackSession {
    /// 打开一路回环并启动采集线程。endpoint 为空/None = 默认播放设备。
    /// 同步完成打开（几毫秒量级）；失败返回中文错误串（与 Go 同文案）。
    pub fn open(endpoint: Option<&str>) -> Result<Self, String> {
        unsafe {
            // MTA；已初始化（S_FALSE）也继续用
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);

            let en: IMMDeviceEnumerator =
                CoCreateInstance(&CLSID_MMDeviceEnumerator, None, CLSCTX_ALL)
                    .map_err(|_| ERR_ENUMERATOR.to_string())?;

            let dev: IMMDevice = match endpoint.filter(|s| !s.is_empty()) {
                Some(id) => {
                    let w: Vec<u16> = id.encode_utf16().chain(std::iter::once(0)).collect();
                    en.GetDevice(PCWSTR(w.as_ptr()))
                        .map_err(|_| ERR_DEVICE_NOT_FOUND.to_string())?
                }
                None => en
                    .GetDefaultAudioEndpoint(EDataFlow(E_RENDER), ERole(E_CONSOLE))
                    .map_err(|_| ERR_NO_DEFAULT.to_string())?,
            };

            let client: IAudioClient = dev
                .Activate::<IAudioClient>(CLSCTX_ALL, None)
                .map_err(|_| ERR_ACTIVATE.to_string())?;

            let mix = client.GetMixFormat().map_err(|_| ERR_MIX_FORMAT.to_string())?;
            let (info, tag, block_align) = parse_mix_format(mix);

            client
                .Initialize(
                    AUDCLNT_SHAREMODE_SHARED,
                    AUDCLNT_STREAMFLAGS_LOOPBACK,
                    2_000_000, // 200ms，100ns 单位
                    0,
                    mix,
                    None,
                )
                .map_err(|_| ERR_INIT.to_string())?;

            let capture: IAudioCaptureClient =
                client.GetService().map_err(|_| ERR_SERVICE.to_string())?;

            client.Start().map_err(|_| ERR_START.to_string())?;

            let state = Arc::new(LoopbackState {
                running: AtomicBool::new(true),
                rate: AtomicI32::new(info.rate),
                ch: AtomicI32::new(info.ch),
                bits: AtomicI32::new(info.bits),
                raw_tag: AtomicU32::new(info.raw_tag as u32),
                sub_tag: AtomicU32::new(info.sub_tag as u32),
                chan_mask: AtomicU32::new(info.chan_mask),
                block_align: AtomicI32::new(info.block_align),
                frames: AtomicU64::new(0),
                err_code: AtomicI32::new(0),
                kind: Mutex::new(info.kind),
                dev: Mutex::new(String::new()), // 线程里填（拿 dev 友好名）
                err: Mutex::new(String::new()),
            });

            let spec_shared: Arc<Mutex<Spectrum>> = Arc::new(Mutex::new(Spectrum::new()));
            let stop = Arc::new(AtomicBool::new(false));
            let st2 = state.clone();
            let stop2 = stop.clone();
            let spec2 = spec_shared.clone();
            let (rate, ch) = (info.rate as usize, info.ch as usize);
            let h = LbHandles {
                client: client.into_raw(),
                capture: capture.into_raw(),
                dev: dev.into_raw(),
                mix: mix as usize,
            };
            let th = std::thread::Builder::new()
                .name("ikf-lb-capture".into())
                .spawn(move || {
                    capture_loop(h, tag, block_align, rate, ch, &st2, &spec2, &stop2);
                })
                .map_err(|e| format!("采集线程启动失败: {}", e))?;

            Ok(LoopbackSession {
                state,
                spec: spec_shared,
                stop,
                th: Some(th),
            })
        }
    }

    /// 最新 512 字节频谱（Go Spectrum() 的数据源）。
    pub fn spectrum(&self, out: &mut [u8; BINS]) -> bool {
        match self.spec.lock() {
            Ok(sp) => {
                out.copy_from_slice(&sp.bins);
                true
            }
            Err(_) => false,
        }
    }

    pub fn level(&self) -> f32 {
        self.spec.lock().map(|s| s.level).unwrap_or(0.0)
    }

    /// 停止并回收采集线程，清空会话可见字段（Go Stop() 的语义）。
    pub fn close(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.th.take() {
            let _ = h.join();
        }
        let s = &self.state;
        s.running.store(false, Ordering::SeqCst);
        s.rate.store(0, Ordering::SeqCst);
        s.ch.store(0, Ordering::SeqCst);
        s.bits.store(0, Ordering::SeqCst);
        s.raw_tag.store(0, Ordering::SeqCst);
        s.sub_tag.store(0, Ordering::SeqCst);
        s.chan_mask.store(0, Ordering::SeqCst);
        s.block_align.store(0, Ordering::SeqCst);
        s.frames.store(0, Ordering::SeqCst);
        s.err_code.store(0, Ordering::SeqCst);
        if let Ok(mut sp) = self.spec.lock() {
            sp.ring.iter_mut().for_each(|v| *v = 0.0);
            sp.prev.iter_mut().for_each(|v| *v = 0.0);
            sp.bins.iter_mut().for_each(|v| *v = 0);
            sp.ring_w = 0;
            sp.ring_n = 0;
            sp.level = 0.0;
        }
        if let Ok(mut k) = s.kind.lock() {
            *k = String::new();
        }
        if let Ok(mut d) = s.dev.lock() {
            *d = String::new();
        }
        if let Ok(mut e) = s.err.lock() {
            *e = String::new();
        }
    }
}

impl Drop for LoopbackSession {
    fn drop(&mut self) {
        self.close();
    }
}

/// 解析混音格式（GetMixFormat 的 WAVEFORMATEX；EXTENSIBLE 时读 SubFormat）。
unsafe fn parse_mix_format(mix: *mut WAVEFORMATEX) -> (SessionInfo, u16, i32) {
    let wf = &*mix;
    let rate = wf.nSamplesPerSec as i32;
    let ch = wf.nChannels as i32;
    let bits = wf.wBitsPerSample as i32;
    let block_align = wf.nBlockAlign as i32;
    let raw_tag = wf.wFormatTag;
    let mut tag = raw_tag;
    let mut sub_tag: u16 = 0;
    let mut mask: u32 = 0;
    if raw_tag == WAVE_FORMAT_EXT_TAG {
        // SubFormat 在 EXTENSIBLE 偏移 24；按内存布局直读 Data1
        //（避免 GUID::to_u128 的字节序歧义，与 Go uint16(SubFormat.Data1) 等价）
        let d1 = std::ptr::read_unaligned((mix as *const u8).add(24) as *const u32);
        sub_tag = d1 as u16;
        // dwChannelMask 在偏移 20
        mask = std::ptr::read_unaligned((mix as *const u8).add(20) as *const u32);
        tag = sub_tag;
    }
    let kind = if rate <= 0 || ch <= 0 || block_align <= 0 {
        "unsupported"
    } else {
        match (tag, bits) {
            (3, 32) => "float32",
            (1, 16) => "pcm16",
            (1, 32) => "pcm32",
            (1, 24) => "pcm24",
            _ => "unsupported",
        }
    };
    (
        SessionInfo {
            rate,
            ch,
            kind: kind.to_string(),
            bits,
            raw_tag,
            sub_tag,
            chan_mask: mask,
            block_align,
            dev_name: String::new(),
        },
        tag,
        block_align,
    )
}

/// 采集循环（Go session() 的 5ms 轮询段）。COM 指针的所有权在这里，
/// 结束时逆序释放。
unsafe fn capture_loop(
    h: LbHandles,
    tag: u16,
    block_align: i32,
    _rate: usize,
    ch: usize,
    state: &Arc<LoopbackState>,
    spec_shared: &Arc<Mutex<Spectrum>>,
    stop: &Arc<AtomicBool>,
) {
    let client: IAudioClient = std::mem::transmute(h.client);
    let capture: IAudioCaptureClient = std::mem::transmute(h.capture);
    let dev: IMMDevice = std::mem::transmute(h.dev);
    let mix = h.mix as *mut WAVEFORMATEX;
    // ★按需短锁：MutexGuard 不能横跨轮询循环持有（spectrum()/close() 会
    // 永远等锁）。push/compute 各自短暂加锁，微秒级不构成争用。
    let mut last_fft = Instant::now();
    if let Ok(mut d) = state.dev.lock() {
        *d = device_name(&dev);
    }
    let mut broken: Option<String> = None;

    'outer: while !stop.load(Ordering::SeqCst) {
        std::thread::sleep(Duration::from_millis(5));
        for _ in 0..64 {
            let packet = match capture.GetNextPacketSize() {
                Ok(n) => n,
                Err(_) => {
                    broken = Some(ERR_BROKEN.to_string());
                    break 'outer;
                }
            };
            if packet == 0 {
                break;
            }
            let mut data: *mut u8 = std::ptr::null_mut();
            let mut frames: u32 = 0;
            let mut flags: u32 = 0;
            let mut dev_pos: u64 = 0;
            let mut qpc: u64 = 0;
            if capture
                .GetBuffer(
                    &mut data,
                    &mut frames,
                    &mut flags,
                    Some(&mut dev_pos),
                    Some(&mut qpc),
                )
                .is_err()
            {
                break;
            }
            if frames > 0 {
                let f = frames as usize;
                if flags & AUDCLNT_SILENT != 0 || data.is_null() {
                    spec_shared.lock().unwrap_or_else(|e| e.into_inner()).push(&vec![0.0f32; f]);
                } else {
                    let raw = std::slice::from_raw_parts(data, f * block_align as usize);
                    // bits 参数 Go 版从会话字段取；decode_block 只用 (tag,bits)
                    // 分派 —— 传 0 会被 default 分支吃掉，必须给真值。
                    let bits = state.bits.load(Ordering::SeqCst) as u16;
                    // 锁中毒兜底（同上方静音分支）：采集线程 panic 后锁中毒，
                    // unwrap 会在这里连环 abort。
                    spec_shared
                        .lock()
                        .unwrap_or_else(|e| e.into_inner())
                        .push(&decode_block(raw, f, ch, block_align as usize, tag, bits));
                }
                state.frames.fetch_add(frames as u64, Ordering::SeqCst);
                let _ = capture.ReleaseBuffer(frames);
            }
        }
        if last_fft.elapsed() >= Duration::from_millis(20) {
            last_fft = Instant::now();
            spec_shared.lock().unwrap_or_else(|e| e.into_inner()).compute();
        }
    }

    let _ = client.Stop();
    drop(capture);
    drop(client);
    drop(dev);
    CoTaskMemFree(Some(mix.cast()));

    if let Some(e) = broken {
        if let Ok(mut m) = state.err.lock() {
            *m = e;
        }
        state.err_code.store(1, Ordering::SeqCst);
    }
    state.running.store(false, Ordering::SeqCst);
    // rate 等字段保留（Go 版 Status 在流断后仍读得到最后格式），Stop() 才清零。
}

/// 端点枚举：(id, name, is_default)（Go Devices() 的数据同源）。
pub fn list_render_endpoints() -> Result<Vec<(String, String, bool)>, String> {
    unsafe {
        let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
        let en: IMMDeviceEnumerator =
            CoCreateInstance(&CLSID_MMDeviceEnumerator, None, CLSCTX_ALL)
                .map_err(|_| ERR_ENUMERATOR.to_string())?;
        let coll: IMMDeviceCollection = en
            .EnumAudioEndpoints(EDataFlow(E_RENDER), DEVICE_STATE_ACTIVE)
            .map_err(|_| ERR_ENUMERATOR.to_string())?;
        let n = coll.GetCount().map_err(|_| ERR_ENUMERATOR.to_string())?;
        let def_id = en
            .GetDefaultAudioEndpoint(EDataFlow(E_RENDER), ERole(E_CONSOLE))
            .ok()
            .and_then(|d| Some(device_id(&d)))
            .unwrap_or_default();
        let mut out = Vec::new();
        for i in 0..n {
            let Ok(dev) = coll.Item(i) else { continue };
            let id = device_id(&dev);
            if !id.is_empty() {
                let is_def = id == def_id;
                out.push((id, device_name(&dev), is_def));
            }
        }
        Ok(out)
    }
}

unsafe fn pwstr_to_string(p: windows::core::PWSTR) -> String {
    if p.0.is_null() {
        return String::new();
    }
    let mut len = 0usize;
    while *p.0.add(len) != 0 {
        len += 1;
    }
    String::from_utf16_lossy(std::slice::from_raw_parts(p.0, len))
}

unsafe fn device_id(dev: &IMMDevice) -> String {
    match dev.GetId() {
        Ok(pw) => {
            let s = pwstr_to_string(pw);
            CoTaskMemFree(Some(pw.0.cast()));
            s
        }
        Err(_) => String::new(),
    }
}

unsafe fn device_name(dev: &IMMDevice) -> String {
    let Ok(store): Result<IPropertyStore, _> = dev.OpenPropertyStore(STGM(STGM_READ.0)) else {
        return String::new();
    };
    let Ok(pv) = store.GetValue(&PKEY_DEVICE_FRIENDLYNAME) else {
        return String::new();
    };
    let s = propvariant_lpstr(&pv);
    let _ = PropVariantClear(&pv as *const PROPVARIANT as *mut PROPVARIANT);
    s
}

/// PKEY_Device_FriendlyName {a45c254e-df1c-4efd-8020-67d146a850e0}, 14
const PKEY_DEVICE_FRIENDLYNAME: PROPERTYKEY = PROPERTYKEY {
    fmtid: GUID::from_u128(0xa45c254e_df1c_4efd_8020_67d146a850e0),
    pid: 14,
};

unsafe fn propvariant_lpstr(pv: &PROPVARIANT) -> String {
    // VT_LPWSTR = 31；值在偏移 8（x64 PROPVARIANT 布局，与 Go 版同一注记）
    if pv.Anonymous.Anonymous.vt.0 != 31 {
        return String::new();
    }
    let p = pv.Anonymous.Anonymous.Anonymous.pwszVal.0;
    if p.is_null() {
        return String::new();
    }
    let mut len = 0usize;
    while *p.add(len) != 0 {
        len += 1;
    }
    String::from_utf16_lossy(std::slice::from_raw_parts(p, len))
}
