//! 麦克风监听会话 —— Go listen.go session() 的 Rust 移植。
//!
//! 链路：默认麦克风（capture，自身混音格式）→ 软件上混 → 默认渲染端点
//! （render，自身混音格式，样本写**全部声道**——多声道端点的物理声道
//! 映射可能不在 FL/FR（2026-10-24 实证 winmm 上混写 FC），全声道写入
//! 保证任何映射都能听到）。
//!
//! ★ 2026-10-20 静音 bug 教训（Go 版注释逐条继承）：两端都必须用各自的
//! 原生混音格式初始化，**不依赖 WASAPI 引擎的任何格式转换**（AUTOCONVERTPCM
//! 两种摆法都实测全静音）。共享模式混合格式必为 float32，其他直接拒绝。
//!
//! 与 loopback.rs 同构的线程模型：open() 同步打开两端，主循环在自带线程。

use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use windows::core::{GUID, Interface as _};
use windows::Win32::Media::Audio::{
    IAudioCaptureClient, IAudioClient, IAudioRenderClient, IMMDevice, IMMDeviceEnumerator,
    WAVEFORMATEX, WAVEFORMATEXTENSIBLE, AUDCLNT_SHAREMODE_SHARED, DEVICE_STATE_ACTIVE,
};
use windows::Win32::System::Com::{
    CoCreateInstance, CoInitializeEx, CoTaskMemFree, CLSCTX_ALL, COINIT_MULTITHREADED,
};

use windows::Win32::Foundation::WAIT_OBJECT_0;

const CLSID_MMDeviceEnumerator: GUID =
    GUID::from_u128(0xBCDE0395_E52F_467C_8E3D_C4579291692E);

// 与 Go listen.go 错误文案逐字对齐。
const ERR_ENUMERATOR: &str = "无法创建音频设备枚举器";
const ERR_NO_MIC: &str = "找不到默认麦克风（耳机没插好？）";
const ERR_ACT_CAP: &str = "无法激活采集端 IAudioClient";
const ERR_MIX_CAP: &str = "无法读取麦克风混音格式";
const ERR_MIX_CAP_BAD: &str = "麦克风混音格式异常";
const ERR_MIX_CAP_UNSUP: &str = "麦克风混音格式不受支持";
const ERR_INIT_CAP: &str = "采集端 Initialize 失败";
const ERR_SVC_CAP: &str = "无法获取 IAudioCaptureClient";
const ERR_NO_REN: &str = "找不到默认输出设备";
const ERR_ACT_REN: &str = "无法激活输出端 IAudioClient";
const ERR_MIX_REN: &str = "无法读取输出混音格式";
const ERR_MIX_REN_BAD: &str = "输出混音格式异常";
const ERR_MIX_REN_UNSUP: &str = "输出混音格式不受支持";
const ERR_INIT_REN: &str = "输出端 Initialize 失败";
const ERR_BUFSZ: &str = "无法读取输出缓冲大小";
const ERR_SVC_REN: &str = "无法获取 IAudioRenderClient";
const ERR_START_CAP: &str = "采集端 Start 失败";
const ERR_START_REN: &str = "输出端 Start 失败";
const ERR_BROKEN: &str = "麦克风数据流已中断（设备被拔出？）";
const ERR_REN_BROKEN: &str = "输出端数据流已中断";
const ERR_REN_GETBUF: &str = "输出端 GetBuffer 失败";

const AUDCLNT_SILENT: u32 = 0x00000002;
const BUF_DURATION: i64 = 2_000_000; // 200ms，100ns 单位

/// 会话运行态（Go MicListen.mu 保护区对应物）。
pub struct ListenState {
    pub running: AtomicBool,
    pub rate: AtomicI32,
    pub ch: AtomicI32,
    pub sent: AtomicU64,
    pub pkts: AtomicU64,
    pub silent_pkts: AtomicU64,
    pub cap_peak_bits: AtomicU32, // f32 按位存
    /// 监听增益（f32 按位存，1.0=原始电平）。前端滑杆可调 1.0-8.0。
    pub gain_bits: AtomicU32,
    pub dev: Mutex<String>,
    pub out: Mutex<String>,
    pub err: Mutex<String>,
}

/// ABI 状态快照（repr(C) 定长 UTF-8）。
#[repr(C)]
pub struct LsStatusOut {
    pub running: i32,
    pub rate: i32,
    pub ch: i32,
    pub sent: u64,
    pub pkts: u64,
    pub silent_pkts: u64,
    pub cap_peak: f32,
    pub dev: [u8; 256],
    pub out: [u8; 256],
    pub err: [u8; 256],
}

/// 一路监听会话。
pub struct ListenSession {
    pub state: Arc<ListenState>,
    stop: Arc<AtomicBool>,
    th: Option<std::thread::JoinHandle<()>>,
}

/// 两端 COM 指针打包（整体 move 跨线程，见 loopback.rs 注记）。
struct LsHandles {
    cap_client: *mut core::ffi::c_void,
    capture: *mut core::ffi::c_void,
    ren_client: *mut core::ffi::c_void,
    render: *mut core::ffi::c_void,
    cap_dev: *mut core::ffi::c_void,
    ren_dev: *mut core::ffi::c_void,
    cap_mix: usize,
    ren_mix: usize,
    buf_frames: u32,
    cap_block_align: i32,
    ren_block_align: i32,
}
unsafe impl Send for LbHandlesAlias {}
type LbHandlesAlias = LsHandles;

impl ListenSession {
    /// 同步打开两端并启动主循环线程。
    pub fn open() -> Result<Self, String> {
        unsafe {
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
eprintln!("[ls] 1 cocreate");
            let en: IMMDeviceEnumerator =
                CoCreateInstance(&CLSID_MMDeviceEnumerator, None, CLSCTX_ALL)
                    .map_err(|_| ERR_ENUMERATOR.to_string())?;

eprintln!("[ls] 2 enum ok");
            // ---- 采集端：麦克风自己的混音格式 ----
            let dev_c: IMMDevice = en
                .GetDefaultAudioEndpoint(windows::Win32::Media::Audio::EDataFlow(1), windows::Win32::Media::Audio::ERole(0))
                .map_err(|_| ERR_NO_MIC.to_string())?;
            let cli_c: IAudioClient = dev_c
                .Activate::<IAudioClient>(CLSCTX_ALL, None)
                .map_err(|_| ERR_ACT_CAP.to_string())?;
eprintln!("[ls] 3 act cap");
            let mix_c = cli_c.GetMixFormat().map_err(|_| ERR_MIX_CAP.to_string())?;
eprintln!("[ls] 4 mixfmt");
            let (rate, ch, tag, block_align) = parse_tag(mix_c);
            if rate <= 0 || ch <= 0 || block_align <= 0 {
                return Err(ERR_MIX_CAP_BAD.to_string());
            }
            if tag != 3 {
                // 共享模式混合格式必为 float32；别的情况直接拒绝（不猜格式）
                return Err(format!("{}（tag={} bits）", ERR_MIX_CAP_UNSUP, tag));
            }
            cli_c
                .Initialize(AUDCLNT_SHAREMODE_SHARED, 0, BUF_DURATION, 0, mix_c, None)
                .map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_INIT_CAP, e.code().0 as u32))?;
eprintln!("[ls] 5 init cap");
            let capture: IAudioCaptureClient =
                cli_c.GetService().map_err(|_| ERR_SVC_CAP.to_string())?;

eprintln!("[ls] 6 svc cap");
            // ---- 输出端：渲染自己的混音格式（软件上混） ----
            let dev_r: IMMDevice = en
                .GetDefaultAudioEndpoint(windows::Win32::Media::Audio::EDataFlow(0), windows::Win32::Media::Audio::ERole(0))
                .map_err(|_| ERR_NO_REN.to_string())?;
            let cli_r: IAudioClient = dev_r
                .Activate::<IAudioClient>(CLSCTX_ALL, None)
                .map_err(|_| ERR_ACT_REN.to_string())?;
eprintln!("[ls] 7 act ren");
            let mix_r = cli_r.GetMixFormat().map_err(|_| ERR_MIX_REN.to_string())?;
            let (_rr, ren_ch, ren_tag, ren_block) = parse_tag(mix_r);
            if ren_ch <= 0 || ren_block <= 0 {
                return Err(ERR_MIX_REN_BAD.to_string());
            }
            if ren_tag != 3 {
                return Err(format!("{}（tag={}）", ERR_MIX_REN_UNSUP, ren_tag));
            }
            cli_r
                .Initialize(AUDCLNT_SHAREMODE_SHARED, 0, BUF_DURATION, 0, mix_r, None)
                .map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_INIT_REN, e.code().0 as u32))?;
eprintln!("[ls] 8 init ren");
            let buf_frames = cli_r.GetBufferSize().map_err(|_| ERR_BUFSZ.to_string())?;
            let render: IAudioRenderClient =
                cli_r.GetService().map_err(|_| ERR_SVC_REN.to_string())?;

eprintln!("[ls] 9 svc ren+bufsz");
            cli_c.Start().map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_START_CAP, e.code().0 as u32))?;
            cli_r.Start().map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_START_REN, e.code().0 as u32))?;

            let state = Arc::new(ListenState {
                running: AtomicBool::new(true),
                rate: AtomicI32::new(rate),
                ch: AtomicI32::new(ch),
                sent: AtomicU64::new(0),
                pkts: AtomicU64::new(0),
                silent_pkts: AtomicU64::new(0),
                cap_peak_bits: AtomicU32::new(0),
                gain_bits: AtomicU32::new((1.0f32).to_bits()),
                dev: Mutex::new(String::new()),
                out: Mutex::new(String::new()),
                err: Mutex::new(String::new()),
            });

            let stop = Arc::new(AtomicBool::new(false));
            let st2 = state.clone();
            let stop2 = stop.clone();
eprintln!("[ls] 10 started");
            let h = LsHandles {
                cap_client: cli_c.into_raw(),
                capture: capture.into_raw(),
                ren_client: cli_r.into_raw(),
                render: render.into_raw(),
                cap_dev: dev_c.into_raw(),
                ren_dev: dev_r.into_raw(),
                cap_mix: mix_c as usize,
                ren_mix: mix_r as usize,
                buf_frames,
                cap_block_align: block_align,
                ren_block_align: ren_block,
            };
            let th = std::thread::Builder::new()
                .name("ikf-listen".into())
                .spawn(move || { eprintln!("[ls] 11 spawn"); listen_loop(h, &st2, &stop2) })
                .map_err(|e| format!("监听线程启动失败: {}", e))?;

            Ok(ListenSession { state, stop, th: Some(th) })
        }
    }

    pub fn close(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.th.take() {
            let _ = h.join();
        }
        self.state.running.store(false, Ordering::SeqCst);
    }
}

impl Drop for ListenSession {
    fn drop(&mut self) {
        self.close();
    }
}

/// 读混音格式摘要：(rate, ch, 编码 tag（EXTENSIBLE 时取 SubFormat.Data1 低 16）, blockAlign)。
unsafe fn parse_tag(mix: *mut WAVEFORMATEX) -> (i32, i32, i32, i32) {
    let wf = &*mix;
    let mut tag = wf.wFormatTag as i32;
    if wf.wFormatTag == 0xFFFE {
        // SubFormat 在 EXTENSIBLE 偏移 24（见 loopback.go wfSubFormatOffset）；
        // 读 Data1 低 16 位 —— 直接按内存布局取，避免 GUID::to_u128 的字节序歧义
        // （与 Go uint16(ext.SubFormat.Data1) 等价）。
        let d1 = std::ptr::read_unaligned((mix as *const u8).add(24) as *const u32);
        tag = (d1 & 0xFFFF) as i32;
    }
    (wf.nSamplesPerSec as i32, wf.nChannels as i32, tag, wf.nBlockAlign as i32)
}

/// 设备友好名（与 loopback.rs 的 device_name 同逻辑，独立持有避免跨模块依赖）。
unsafe fn device_name(dev: &IMMDevice) -> String {
    use windows::Win32::Foundation::PROPERTYKEY;
    use windows::Win32::System::Com::StructuredStorage::PropVariantClear;
    use windows::Win32::System::Com::{STGM, STGM_READ};
    use windows::Win32::UI::Shell::PropertiesSystem::IPropertyStore;
    const PKEY: PROPERTYKEY = PROPERTYKEY {
        fmtid: GUID::from_u128(0xa45c254e_df1c_4efd_8020_67d146a850e0),
        pid: 14,
    };
    let Ok(store): Result<IPropertyStore, _> = dev.OpenPropertyStore(STGM_READ) else {
        return String::new();
    };
    let Ok(pv) = store.GetValue(&PKEY) else {
        return String::new();
    };
    let mut s = String::new();
    if pv.Anonymous.Anonymous.vt.0 == 31 {
        let p = pv.Anonymous.Anonymous.Anonymous.pwszVal.0;
        if !p.is_null() {
            let mut len = 0usize;
            while *p.add(len) != 0 {
                len += 1;
            }
            s = String::from_utf16_lossy(std::slice::from_raw_parts(p, len));
        }
    }
    let _ = PropVariantClear(&pv as *const _ as *mut _);
    s
}

/// 主循环（Go session() 主循环的逐行移植）。
unsafe fn listen_loop(h: LsHandles, state: &Arc<ListenState>, stop: &Arc<AtomicBool>) {
    let cli_c: IAudioClient = std::mem::transmute(h.cap_client);
    let capture: IAudioCaptureClient = std::mem::transmute(h.capture);
    let cli_r: IAudioClient = std::mem::transmute(h.ren_client);
    let render: IAudioRenderClient = std::mem::transmute(h.render);
    let dev_c: IMMDevice = std::mem::transmute(h.cap_dev);
    let dev_r: IMMDevice = std::mem::transmute(h.ren_dev);
    let mix_c = h.cap_mix as *mut WAVEFORMATEX;
    let mix_r = h.ren_mix as *mut WAVEFORMATEX;
    let buf_frames = h.buf_frames;
    let block_align = h.cap_block_align as usize;
    let ren_block = h.ren_block_align as usize;

    eprintln!("[ls] 12 thread enter");
    {
        let mut d = state.dev.lock().unwrap_or_else(|e| e.into_inner());
        *d = device_name(&dev_c);
    }
    {
        let mut o = state.out.lock().unwrap_or_else(|e| e.into_inner());
        *o = device_name(&dev_r);
    }

    let mut pending: Vec<u8> = Vec::with_capacity(1 << 16);

    'outer: while !stop.load(Ordering::SeqCst) {
        std::thread::sleep(Duration::from_millis(5));

        // 1) 排干采集包（静音包补零，保证渲染时钟一直有料）
        for _ in 0..64 {
            let packet = match capture.GetNextPacketSize() {
                Ok(n) => n,
                Err(_) => {
                    set_err(state, ERR_BROKEN);
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
                .GetBuffer(&mut data, &mut frames, &mut flags, Some(&mut dev_pos), Some(&mut qpc))
                .is_err()
            {
                set_err(state, ERR_BROKEN);
                break 'outer;
            }
            if frames > 0 {
                state.pkts.fetch_add(1, Ordering::SeqCst);
                let f = frames as usize;
                if flags & AUDCLNT_SILENT != 0 || data.is_null() {
                    state.silent_pkts.fetch_add(1, Ordering::SeqCst);
                    pending.extend(std::iter::repeat(0u8).take(f * block_align));
                } else {
                    let raw = std::slice::from_raw_parts(data, f * block_align);
                    // 采集数据峰值（float32 绝对值最大）：诊断「采集侧就是静音」
                    let mut peak = f32::from_bits(state.cap_peak_bits.load(Ordering::SeqCst));
                    for chunk in raw.chunks_exact(4) {
                        let v = f32::from_le_bytes([chunk[0], chunk[1], chunk[2], chunk[3]]).abs();
                        if v > peak {
                            peak = v;
                        }
                    }
                    state.cap_peak_bits.store(peak.to_bits(), Ordering::SeqCst);
                    pending.extend_from_slice(raw);
                }
            }
            let _ = capture.ReleaseBuffer(frames);
        }

        // 2) 软件上混推渲染端：单声道 float32 → 写进渲染端点的**全部声道**。
        //    积压超过一个缓冲说明消费不掉：丢弃到最新，避免延迟无限增长。
        let byte_avail = buf_frames as usize * block_align;
        if pending.len() > byte_avail {
            pending.drain(..pending.len() - byte_avail);
        }
        while pending.len() >= block_align {
            let pad = match cli_r.GetCurrentPadding() {
                Ok(v) => v,
                Err(_) => {
                    set_err(state, ERR_REN_BROKEN);
                    break 'outer;
                }
            };
            let free = buf_frames as i64 - pad as i64;
            if free <= 0 {
                break;
            }
            let mut n = free as usize;
            if n > pending.len() / block_align {
                n = pending.len() / block_align;
            }
            let data = match render.GetBuffer(n as u32) {
                Ok(p) if !p.is_null() => p,
                _ => {
                    set_err(state, ERR_REN_GETBUF);
                    break 'outer;
                }
            };
            let dst = std::slice::from_raw_parts_mut(data, n * ren_block);
            // 单声道样本写进渲染端点的**每一个声道**：多声道端点的物理
            // 声道映射不保证在 FL/FR（取证见文件头注），全声道写入保证
            // 任何映射都出声。样本从 pending 的声道 0 取（麦克风源）。
            let gain = f32::from_bits(state.gain_bits.load(Ordering::SeqCst));
            for f in 0..n {
                let o = f * ren_block;
                let s = f32::from_le_bytes([
                    pending[f * block_align],
                    pending[f * block_align + 1],
                    pending[f * block_align + 2],
                    pending[f * block_align + 3],
                ]) * gain;
                let le = s.to_le_bytes();
                let chans = ren_block / 4;
                for c in 0..chans {
                    dst[o + c * 4..o + c * 4 + 4].copy_from_slice(&le);
                }
            }
            let _ = render.ReleaseBuffer(n as u32, 0);
            pending.drain(..n * block_align);
            state.sent.fetch_add((n * ren_block) as u64, Ordering::SeqCst);
        }
    }

    let _ = cli_c.Stop();
    let _ = cli_r.Stop();
    drop(capture);
    drop(render);
    drop(cli_c);
    drop(cli_r);
    drop(dev_c);
    drop(dev_r);
    CoTaskMemFree(Some(mix_c.cast()));
    CoTaskMemFree(Some(mix_r.cast()));
    state.running.store(false, Ordering::SeqCst);
}

fn set_err(state: &Arc<ListenState>, msg: &str) {
    if let Ok(mut e) = state.err.lock() {
        *e = msg.to_string();
    }
}

// WAIT_OBJECT_0 未用（占位避免误 import）
#[allow(dead_code)]
const _: u32 = WAIT_OBJECT_0.0;
