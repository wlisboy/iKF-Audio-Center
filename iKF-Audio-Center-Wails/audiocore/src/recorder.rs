//! 录音会话 —— Go recorder.go session() 的 Rust 移植。
//!
//! 链路：默认渲染端点 loopback 采集（与 loopback.rs 同款 5ms 轮询）→
//! 下混为 16bit 立体声（取 FL/FR；单声道源双写；四种源格式 + 未知格式写
//! 静音）→ 写 WAV（44 字节标准 PCM 头，数据长度占位，停止时 finalize 补写）。
//!
//! 路径策略（recDir：注册表 My Music → iKF Audio Center 子目录）留 Go 侧，
//! WAV 全路径由调用方传入 —— DLL 不碰注册表与用户目录。

use std::io::{Seek, SeekFrom, Write};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicI64, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use windows::core::{GUID, Interface as _, PCWSTR};
use windows::Win32::Media::Audio::{
    IAudioCaptureClient, IAudioClient, IMMDevice, IMMDeviceEnumerator,
    WAVEFORMATEX, WAVEFORMATEXTENSIBLE, AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK,
};
use windows::Win32::System::Com::{
    CoCreateInstance, CoInitializeEx, CoTaskMemFree, CLSCTX_ALL, COINIT_MULTITHREADED,
};

const CLSID_MMDeviceEnumerator: GUID =
    GUID::from_u128(0xBCDE0395_E52F_467C_8E3D_C4579291692E);

// 与 Go recorder.go 错误文案逐字对齐。
const ERR_ENUMERATOR: &str = "无法创建音频设备枚举器";
const ERR_NO_REN: &str = "找不到默认输出设备（耳机没插好？）";
const ERR_ACT: &str = "无法激活 IAudioClient";
const ERR_MIX: &str = "无法读取输出混音格式";
const ERR_MIX_BAD: &str = "输出混音格式异常";
const ERR_FILE: &str = "无法创建录音文件";
const ERR_INIT: &str = "IAudioClient::Initialize(loopback) 失败";
const ERR_SVC: &str = "无法获取 IAudioCaptureClient";
const ERR_START: &str = "IAudioClient::Start 失败";
const ERR_BROKEN: &str = "输出端数据流已中断（设备被拔出？），已保存已录部分";

const AUDCLNT_SILENT: u32 = 0x00000002;
const WAVE_FORMAT_EXT_TAG: u16 = 0xFFFE;

/// 会话运行态。
pub struct RecState {
    pub running: AtomicBool,
    pub rate: AtomicI32,
    pub ch: AtomicI32,
    pub frames: AtomicU64,
    pub size: AtomicI64,
    pub err_code: AtomicI32,
    pub kind: Mutex<String>,
    pub dev: Mutex<String>,
    pub file: Mutex<String>,
    pub err: Mutex<String>,
}

/// ABI 状态快照。
#[repr(C)]
pub struct RecStatusOut {
    pub running: i32,
    pub rate: i32,
    pub ch: i32,
    pub frames: u64,
    pub size: i64,
    pub err_code: i32,
    pub kind: [u8; 16],
    pub dev: [u8; 256],
    pub file: [u8; 512],
    pub err: [u8; 256],
}

pub struct RecSession {
    pub state: Arc<RecState>,
    stop: Arc<AtomicBool>,
    th: Option<std::thread::JoinHandle<()>>,
}

struct RecHandles {
    client: *mut core::ffi::c_void,
    capture: *mut core::ffi::c_void,
    dev: *mut core::ffi::c_void,
    mix: usize,
    /// WAV 文件句柄整体移交（占位头已写入；File: Send）。
    out_file: std::fs::File,
}
unsafe impl Send for RecHandles {}

/// Go srcKind 同款（诊断字段）。
fn src_kind(tag: i32, bits: i32) -> &'static str {
    match (tag, bits) {
        (3, 32) => "float32",
        (1, 16) => "pcm16",
        (1, 32) => "pcm32",
        (1, 24) => "pcm24",
        _ => "unsupported",
    }
}

/// 44 字节标准 PCM16 WAV 头（Go wavHeaderBytes 同款）。
pub fn wav_header(rate: i32, ch: i32, data_len: u32) -> [u8; 44] {
    let mut h = [0u8; 44];
    let byte_rate = (rate * ch * 2) as u32;
    let block_align = (ch * 2) as u16;
    h[0..4].copy_from_slice(b"RIFF");
    h[4..8].copy_from_slice(&(36u32 + data_len).to_le_bytes());
    h[8..12].copy_from_slice(b"WAVE");
    h[12..16].copy_from_slice(b"fmt ");
    h[16..20].copy_from_slice(&16u32.to_le_bytes());
    h[20..22].copy_from_slice(&1u16.to_le_bytes()); // PCM
    h[22..24].copy_from_slice(&(ch as u16).to_le_bytes());
    h[24..28].copy_from_slice(&(rate as u32).to_le_bytes());
    h[28..32].copy_from_slice(&byte_rate.to_le_bytes());
    h[32..34].copy_from_slice(&block_align.to_le_bytes());
    h[34..36].copy_from_slice(&16u16.to_le_bytes());
    h[36..40].copy_from_slice(b"data");
    h[40..44].copy_from_slice(&data_len.to_le_bytes());
    h
}

/// 一包交错的**多声道源**数据下混为 16bit 立体声（Go writeFrames 逐行移植）。
/// 返回 PCM 字节；未知格式返回 None（调用方写静音）。
pub fn downmix_to_pcm16(raw: &[u8], frames: usize, src_ch: usize, block_align: usize, tag: i32, bits: i32) -> Option<Vec<u8>> {
    let le_u16 = |b: &[u8]| u16::from_le_bytes([b[0], b[1]]);
    let le_u32 = |b: &[u8]| u32::from_le_bytes([b[0], b[1], b[2], b[3]]);
    let clamp_i16 = |v: i32| -> [u8; 2] {
        let v = v.clamp(-32768, 32767);
        (v as i16).to_le_bytes()
    };
    // ★max-abs 下混（与 spectrum.rs 同策略）：全声道平均会把信号稀释
    // 1/ch（8ch 端点上音乐只剩 1/8 音量≈无声——真机实证）；max-abs 对
    // 任何声道分布都不稀释。单声道源等价原行为。
    // ★破坏性检测 P0-2：block_align/ch 与实际 buffer 不一致时越界 panic
    // → abort → 0xC0000409 无日志。校验按**实际读宽**算（同 spectrum.rs
    // decode_block）：只查 frames*block_align 拦不住 block_align < ch×样本
    // 字节 的格式漂移，校验最后一帧 base + ch×宽度 ≤ raw.len() 覆盖全部帧。
    // 越界返回 None（调用方写静音）。
    let ch_usize = src_ch.max(1) as usize;
    let width: usize = match (tag, bits) {
        (3, 32) | (1, 32) => 4,
        (1, 16) => 2,
        (1, 24) => 3,
        _ => 0, // 未知格式：逐帧 match 本就返回 None
    };
    if frames == 0 || block_align == 0 || width == 0 {
        return None;
    }
    let last_base = (frames - 1).saturating_mul(block_align);
    if last_base.saturating_add(ch_usize * width) > raw.len() {
        return None;
    }
    let mut pcm: Vec<u8> = Vec::with_capacity(frames * 4);
    for f in 0..frames {
        let base = f * block_align;
        let mono: f32 = match (tag, bits) {
            (3, 32) => (0..ch_usize)
                .map(|c| f32::from_bits(le_u32(&raw[base + c * 4..])))
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 16) => (0..ch_usize)
                .map(|c| (le_u16(&raw[base + c * 2..]) as i16) as f32 / 32768.0)
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 32) => (0..ch_usize)
                .map(|c| (le_u32(&raw[base + c * 4..]) as i32) as f32 / 2147483648.0)
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 24) => (0..ch_usize)
                .map(|c| {
                    let o = base + c * 3;
                    let mut v = raw[o] as i32 | (raw[o + 1] as i32) << 8 | (raw[o + 2] as i32) << 16;
                    if v & 0x800000 != 0 {
                        v |= !0xFFFFFF;
                    }
                    v as f32 / 8388608.0
                })
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            _ => return None,
        };
        let v = clamp_i16((mono * 32767.0) as i32);
        pcm.extend_from_slice(&v); // FL
        pcm.extend_from_slice(&v); // FR（立体声输出双写）
    }
    Some(pcm)
}

impl RecSession {
    /// 打开 loopback 采集并写 WAV。wav_path 为 UTF-8 全路径（Go 侧已建目录）。
    pub fn open(wav_path: &str) -> Result<Self, String> {
        unsafe {
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
            let en: IMMDeviceEnumerator =
                CoCreateInstance(&CLSID_MMDeviceEnumerator, None, CLSCTX_ALL)
                    .map_err(|_| ERR_ENUMERATOR.to_string())?;
            let dev: IMMDevice = en
                .GetDefaultAudioEndpoint(windows::Win32::Media::Audio::EDataFlow(0), windows::Win32::Media::Audio::ERole(0))
                .map_err(|_| ERR_NO_REN.to_string())?;
            let client: IAudioClient = dev
                .Activate::<IAudioClient>(CLSCTX_ALL, None)
                .map_err(|_| ERR_ACT.to_string())?;
            let mix = client.GetMixFormat().map_err(|_| ERR_MIX.to_string())?;

            // 混音格式解析（与 loopback.rs 同口径：偏移直读）
            let wf = &*mix;
            let rate = wf.nSamplesPerSec as i32;
            let src_ch = wf.nChannels as i32;
            let bits = wf.wBitsPerSample as i32;
            let block_align = wf.nBlockAlign as i32;
            let mut tag = wf.wFormatTag as i32;
            if wf.wFormatTag == WAVE_FORMAT_EXT_TAG {
                let d1 = std::ptr::read_unaligned((mix as *const u8).add(24) as *const u32);
                tag = d1 as i32;
            }
            if rate <= 0 || src_ch <= 0 || block_align <= 0 {
                return Err(ERR_MIX_BAD.to_string());
            }
            let wav_ch = 2; // 输出恒立体声

            // 建 WAV：头先占位，停止时 finalize 补写
            let mut out = std::fs::File::create(wav_path).map_err(|_| ERR_FILE.to_string())?;
            out.write_all(&wav_header(rate, wav_ch, 0)).map_err(|_| ERR_FILE.to_string())?;
            out.flush().map_err(|_| ERR_FILE.to_string())?;

            let state = Arc::new(RecState {
                running: AtomicBool::new(true),
                rate: AtomicI32::new(rate),
                ch: AtomicI32::new(wav_ch),
                frames: AtomicU64::new(0),
                size: AtomicI64::new(0),
                err_code: AtomicI32::new(0),
                kind: Mutex::new(src_kind(tag, bits).to_string()),
                dev: Mutex::new(String::new()),
                file: Mutex::new(wav_path.to_string()),
                err: Mutex::new(String::new()),
            });

            client
                .Initialize(
                    AUDCLNT_SHAREMODE_SHARED,
                    AUDCLNT_STREAMFLAGS_LOOPBACK,
                    2_000_000,
                    0,
                    mix,
                    None,
                )
                .map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_INIT, e.code().0 as u32))?;
            let capture: IAudioCaptureClient =
                client.GetService().map_err(|_| ERR_SVC.to_string())?;
            client
                .Start()
                .map_err(|e| format!("{}，HRESULT=0x{:08X}", ERR_START, e.code().0 as u32))?;

            let stop = Arc::new(AtomicBool::new(false));
            let st2 = state.clone();
            let stop2 = stop.clone();
            let path2 = wav_path.to_string();
            let h = RecHandles {
                client: client.into_raw(),
                capture: capture.into_raw(),
                dev: dev.into_raw(),
                mix: mix as usize,
                out_file: out, // 句柄整体移交（避免二次 open 指针归零+覆盖头）
            };
            let th = std::thread::Builder::new()
                .name("ikf-recorder".into())
                .spawn(move || {
                    record_loop(h, path2, rate, wav_ch, src_ch, block_align as usize, tag, bits, &st2, &stop2);
                })
                .map_err(|e| format!("录音线程启动失败: {}", e))?;

            Ok(RecSession { state, stop, th: Some(th) })
        }
    }

    pub fn close(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.th.take() {
            let _ = h.join(); // 线程内 finalize
        }
        self.state.running.store(false, Ordering::SeqCst);
    }
}

impl Drop for RecSession {
    fn drop(&mut self) {
        self.close();
    }
}

/// 采集 + 写盘主循环。结尾 finalize 补 WAV 头。
unsafe fn record_loop(
    h: RecHandles,
    path: String,
    rate: i32,
    wav_ch: i32,
    src_ch: i32,
    block_align: usize,
    tag: i32,
    bits: i32,
    state: &Arc<RecState>,
    stop: &Arc<AtomicBool>,
) {
    let client: IAudioClient = std::mem::transmute(h.client);
    let capture: IAudioCaptureClient = std::mem::transmute(h.capture);
    let dev: IMMDevice = std::mem::transmute(h.dev);
    let mix = h.mix as *mut WAVEFORMATEX;

    {
        let mut d = state.dev.lock().unwrap_or_else(|e| e.into_inner());
        *d = device_name(&dev);
    }

    let mut out = h.out_file;
    let start = Instant::now();
    let mut broken = false;

    'outer: while !stop.load(Ordering::SeqCst) {
        std::thread::sleep(Duration::from_millis(5));
        for _ in 0..64 {
            let packet = match capture.GetNextPacketSize() {
                Ok(n) => n,
                Err(_) => {
                    broken = true;
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
                broken = true;
                break 'outer;
            }
            if frames > 0 {
                let f = frames as usize;
                let pcm: Option<Vec<u8>> = if flags & AUDCLNT_SILENT != 0 || data.is_null() {
                    Some(vec![0u8; f * 4]) // 静音包：2ch×2B
                } else {
                    let raw = std::slice::from_raw_parts(data, f * block_align);
                    downmix_to_pcm16(raw, f, src_ch as usize, block_align, tag, bits)
                };
                if let Some(pcm) = pcm {
                    if out.write_all(&pcm).is_err() {
                        set_err(state, "录音文件写入失败");
                        break 'outer;
                    }
                    state.size.fetch_add(pcm.len() as i64, Ordering::SeqCst);
                    state.frames.fetch_add(f as u64, Ordering::SeqCst);
                }
            }
            let _ = capture.ReleaseBuffer(frames);
        }
    }

    let _ = client.Stop();
    drop(capture);
    drop(client);
    drop(dev);
    CoTaskMemFree(Some(mix.cast()));

    // finalize：按「当前偏移 − 44」实算 data 长度（不信任记账值，Go 同款）
    let data_len = out
        .seek(SeekFrom::Current(0))
        .map(|off| (off - 44) as u32)
        .unwrap_or(0);
    let _ = out.seek(SeekFrom::Start(0));
    let _ = out.write_all(&wav_header(rate, wav_ch, data_len));
    let _ = out.flush();
    state.size.store(data_len as i64, Ordering::SeqCst);
    // 秒数 = dataLen / (rate*ch*2)
    let secs = data_len as f64 / (rate * wav_ch * 2) as f64;
    if let Ok(mut e) = state.err.lock() {
        if broken {
            *e = ERR_BROKEN.to_string();
        }
    }
    state.err_code.store(if broken { 1 } else { 0 }, Ordering::SeqCst);
    let _ = (secs, start, SystemTime::now().duration_since(UNIX_EPOCH).is_ok());
    state.running.store(false, Ordering::SeqCst);
}

fn set_err(state: &Arc<RecState>, msg: &str) {
    if let Ok(mut e) = state.err.lock() {
        *e = msg.to_string();
    }
}

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

/// PCWSTR 辅助（UTF-16 路径传递用）。
pub fn to_utf16z(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

// 抑制未用导入警告
#[allow(unused_imports)]
use PCWSTR as _PCWSTR_UNUSED;

#[cfg(test)]
mod tests {
    use super::*;

    /// 确定性单测：合成 float32 立体声块 → 下混 PCM16，锁定向量：
    /// 满幅正/负、半幅、静音——Go 单测用同一输入对比字节。
    #[test]
    fn downmix_float32_stereo_vector() {
        // 4 帧：+1.0 / -0.5 / +0.25 / 0.0（L=R 同值，验证双写与截断）
        let amps = [1.0f32, -0.5, 0.25, 0.0];
        let mut raw = Vec::new();
        for a in &amps {
            raw.extend_from_slice(&a.to_le_bytes()); // FL
            raw.extend_from_slice(&a.to_le_bytes()); // FR
        }
        let pcm = downmix_to_pcm16(&raw, 4, 2, 8, 3, 32).expect("float32 应受支持");
        // 期望：满幅 32767（clamp 挡住 32767*1.0 截断溢出）、-16383、8191、0
        let want: Vec<u8> = [32767i16, 32767, -16383, -16383, 8191, 8191, 0, 0]
            .iter()
            .flat_map(|v| v.to_le_bytes())
            .collect();
        assert_eq!(pcm, want);
    }

    #[test]
    fn downmix_pcm16_mono_doubles() {
        let raw: Vec<u8> = [100i16, -200i16].iter().flat_map(|v| v.to_le_bytes()).collect();
        let pcm = downmix_to_pcm16(&raw, 2, 1, 2, 1, 16).expect("pcm16 应受支持");
        // 单声道源双写：L=R 同值
        assert_eq!(&pcm[0..2], &pcm[2..4]);
        assert_eq!(&pcm[4..6], &pcm[6..8]);
    }

    /// ★破坏性检测 P0-2 回归：格式漂移（block_align < ch×样本字节）时
    /// downmix_to_pcm16 必须返回 None，不得越界 panic（→ 0xC0000409）。
    #[test]
    fn downmix_format_drift_returns_none() {
        // 声明 float32 8ch（读宽 32B）但 block_align=8、buffer=3×8：
        // frames*block_align ≤ raw.len() 的旧校验会放行，逐帧读必越界。
        let raw = [0u8; 24];
        assert!(downmix_to_pcm16(&raw, 3, 8, 8, 3, 32).is_none());
        // 未知格式保持 None
        assert!(downmix_to_pcm16(&raw, 3, 2, 8, 0xFF, 8).is_none());
    }

    #[test]
    fn wav_header_layout() {
        let h = wav_header(48000, 2, 1000);
        assert_eq!(&h[0..4], b"RIFF");
        assert_eq!(&h[8..12], b"WAVE");
        assert_eq!(u32::from_le_bytes(h[24..28].try_into().unwrap()), 48000);
        assert_eq!(u32::from_le_bytes(h[40..44].try_into().unwrap()), 1000);
        assert_eq!(u16::from_le_bytes(h[34..36].try_into().unwrap()), 16);
    }
}
