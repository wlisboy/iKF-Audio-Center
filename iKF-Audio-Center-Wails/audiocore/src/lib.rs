//! ikf-audio —— iKF Audio Center 音频热路径的 Rust 实现（P1）。
//!
//! S1 骨架：只含 ABI 协商与自检，无任何 COM/WASAPI 代码（S2 起）。
//! 约定（全库统一）：
//!   * 所有导出函数 `extern "system"`（stdcall，与 Win32/Go syscall 默认一致）；
//!   * 所有导出函数不得 panic —— panic="abort" 下没有 catch_unwind 可言，
//!     兜底是 ensure_panic_hook 落盘取证（见 handle_reg 段之后）；
//!   * 字符串一律 UTF-8（错误串）/ UTF-16（Windows 端点名，S2 起），与
//!     osConfLib.dll / 既有 Go 绑定层的约定逐项对齐。
//!
//! ABI 版本策略：`ikf_abi_version` 返回接口纪元。Go 侧加载后先比对，
//! 不一致 = DLL 与 exe 版本错配 = 拒绝使用（降级到 Go 路径），绝不猜。

use std::ffi::c_char;
use std::sync::atomic::Ordering;

pub mod listen;
pub mod loopback;
pub mod recorder;
pub mod spectrum;

/// 接口纪元：接口签名/结构体布局任何变更 +1。S1 = 1。
pub const ABI_VERSION: u32 = 1;

// ---------------- 会话句柄注册表（破坏性检测 P0-1） ----------------
// ABI 边界上的 handle 此前是裸 Box 指针：野句柄/双重释放直接 UAF（实测
// 0xC0000005 / 0xC0000374 堆破坏）。注册表只存**地址值**（查询不解引用）：
// open 登记、close 注销（第二次 remove 为空 = 幂等返回）、其余导出先查表
// 再解引用 —— 任何违约句柄都拿不到解引用许可。
use std::collections::HashSet;
use std::sync::{Mutex, Once, OnceLock};
macro_rules! handle_reg {
    ($name:ident) => {
        static $name: OnceLock<Mutex<HashSet<usize>>> = OnceLock::new();
    };
}
handle_reg!(LB_LIVE);
handle_reg!(LS_LIVE);
handle_reg!(REC_LIVE);

fn reg_of<'a>(reg: &'a OnceLock<Mutex<HashSet<usize>>>) -> std::sync::MutexGuard<'a, HashSet<usize>> {
    reg.get_or_init(|| Mutex::new(HashSet::new()))
        .lock()
        .unwrap_or_else(|e| e.into_inner())
}
fn reg_take(reg: &OnceLock<Mutex<HashSet<usize>>>, h: usize) -> bool {
    reg_of(reg).remove(&h)
}
fn reg_put(reg: &OnceLock<Mutex<HashSet<usize>>>, h: usize) {
    reg_of(reg).insert(h);
}
fn reg_has(reg: &OnceLock<Mutex<HashSet<usize>>>, h: usize) -> bool {
    reg_of(reg).contains(&h)
}

// ---------------- Rust panic 落盘（破坏性检测 P0-2 收尾） ----------------
// panic="abort" 下任何 panic 直接 __fastfail（0xC0000409），GUI 进程没有
// stderr，panic 消息全部丢失 —— 这正是历史报错"查不到故障原因"的直接
// 原因。hook 在 abort 前把现场追加到本 DLL 旁 log\yyyy-mm-dd.txt —— 与
// 主程序 setupFileLog 同一份按天日志（★2026-10-25 用户要求 log 目录只
// 保留按天文件），panic 行自带 [时间][pid] 前缀，与 exe 的行容易区分。
// 注意 hook 是进程级的：本 DLL 只被自家 exe / 测试进程加载，不存在
// 覆盖宿主 hook 的问题。
static PANIC_HOOK: Once = Once::new();

#[repr(C)]
struct WinSystemTime {
    year: u16,
    month: u16,
    day_of_week: u16,
    day: u16,
    hour: u16,
    minute: u16,
    second: u16,
    millis: u16,
}

extern "system" {
    fn GetLocalTime(out: *mut WinSystemTime);
    fn GetModuleHandleExW(flags: u32, name: *const u16, out: *mut *mut std::ffi::c_void) -> i32;
    fn GetModuleFileNameW(module: *mut std::ffi::c_void, buf: *mut u16, len: u32) -> u32;
}

const MOD_FROM_ADDRESS: u32 = 0x0000_0004;
const MOD_UNCHANGED_REFCOUNT: u32 = 0x0000_0002;

/// 本 DLL 的目录。按 DLL 内函数地址反查模块句柄 —— 不能传 null，
/// 那返回的是主 exe。
fn dll_dir() -> Option<std::path::PathBuf> {
    const CAP: usize = 512;
    let fp: extern "system" fn() -> u32 = ikf_abi_version;
    let mut module: *mut std::ffi::c_void = std::ptr::null_mut();
    let ok = unsafe {
        GetModuleHandleExW(
            MOD_FROM_ADDRESS | MOD_UNCHANGED_REFCOUNT,
            fp as usize as *const u16,
            &mut module,
        )
    };
    if ok == 0 || module.is_null() {
        return None;
    }
    let mut buf = [0u16; CAP];
    let n = unsafe { GetModuleFileNameW(module, buf.as_mut_ptr(), CAP as u32) } as usize;
    if n == 0 || n >= CAP {
        return None;
    }
    let path = String::from_utf16_lossy(&buf[..n]);
    std::path::Path::new(&path).parent().map(|p| p.to_path_buf())
}

fn local_time() -> WinSystemTime {
    let mut st = WinSystemTime {
        year: 0,
        month: 0,
        day_of_week: 0,
        day: 0,
        hour: 0,
        minute: 0,
        second: 0,
        millis: 0,
    };
    unsafe { GetLocalTime(&mut st) };
    st
}

/// log\yyyy-mm-dd.txt，与主程序 setupFileLog 同名同目录 —— panic 行和
/// exe 行落同一个文件才好对时间线。日期取 hook 安装时刻：主程序自己也是
/// 启动时开一次文件、跨天不换名，两者口径一致。log 目录建不出来就直接
/// 放弃（主程序日志同样写不了；往发布包根目录落文件只会弄脏目录）。
fn panic_log_path() -> Option<std::path::PathBuf> {
    let dir = dll_dir()?;
    let log_dir = dir.join("log");
    std::fs::create_dir_all(&log_dir).ok()?;
    let st = local_time();
    Some(log_dir.join(format!("{}-{:02}-{:02}.txt", st.year, st.month, st.day)))
}

/// 在对 Go 暴露的入口调用（幂等）。panic=abort 下 hook 先于 abort 执行，
/// write_all 直达系统调用、无用户态缓冲，消息能落盘。
fn ensure_panic_hook() {
    PANIC_HOOK.call_once(|| {
        let path = panic_log_path();
        std::panic::set_hook(Box::new(move |info| {
            let Some(path) = path.as_ref() else { return };
            let payload = if let Some(s) = info.payload().downcast_ref::<&'static str>() {
                (*s).to_string()
            } else if let Some(s) = info.payload().downcast_ref::<String>() {
                s.clone()
            } else {
                "<非字符串 payload>".to_string()
            };
            let loc = match info.location() {
                Some(l) => format!("{}:{}:{}", l.file(), l.line(), l.column()),
                None => "<未知位置>".to_string(),
            };
            let thread = std::thread::current();
            let st = local_time();
            let line = format!(
                "[{:04}-{:02}-{:02} {:02}:{:02}:{:02}.{:03}] [pid {}] 线程 {:?} 在 {} panic：{}\n",
                st.year, st.month, st.day, st.hour, st.minute, st.second, st.millis,
                std::process::id(),
                thread.name().unwrap_or("<未命名>"),
                loc,
                payload,
            );
            if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(path) {
                use std::io::Write as _;
                let _ = f.write_all(line.as_bytes());
            }
        }));
    });
}

#[no_mangle]
pub extern "system" fn ikf_abi_version() -> u32 {
    // Go 侧加载 DLL 后第一个调用的就是它（先比对 ABI 再用）—— hook 在
    // 这里挂上，之后任何 panic 都有落盘。
    ensure_panic_hook();
    ABI_VERSION
}

/// 版本串写入调用方缓冲（UTF-8 + '\0'）。
/// 返回需要的完整长度（含 '\0'）；buf 为空或 cap 不足时只返回长度不写入。
#[no_mangle]
pub extern "system" fn ikf_version(buf: *mut c_char, cap: u32) -> u32 {
    let v = concat!(env!("CARGO_PKG_NAME"), " ", env!("CARGO_PKG_VERSION"), "\0");
    let n = v.len() as u32;
    if !buf.is_null() && cap >= n {
        unsafe {
            std::ptr::copy_nonoverlapping(v.as_ptr() as *const c_char, buf, n as usize);
        }
    }
    n
}

/// 链路自检：纯计算，验证「DLL 加载 → 调用 → 返回」整条 FFI 通畅。
/// 返回 0xF00D 哨兵（明显不是巧合值）。
#[no_mangle]
pub extern "system" fn ikf_selftest() -> u32 {
    // 顺带验证栈上小缓冲写读（S2 的状态结构会依赖同样路径）
    let mut probe = [0u8; 16];
    let v = b"ikf-selftest-ok";
    probe[..v.len()].copy_from_slice(v);
    if &probe[..v.len()] != v {
        return 0;
    }
    0xF00D
}

/// S2 双跑对比护栏：解析解频谱（Go synthSpectrum 的逐字节复刻）。
/// 把 amp 幅度的 937.5 Hz 正弦（bin 中心）按同一算法算成 512 字节频谱，
/// Go 单测用六组幅度与本函数逐字节对拍 —— 不一致 = 频谱链路漂移。
/// ★ amp 按位传（u64 = f64 bits）：syscall 整型边界不碰 xmm 寄存器。
/// 返回 0 成功；buf 为空或 cap 不足返回需要的长度（不写入）。
#[no_mangle]
pub extern "system" fn ikf_fft_probe(rate: u32, amp_bits: u64, buf: *mut u8, cap: u32) -> u32 {
    if buf.is_null() || cap < spectrum::BINS as u32 {
        return spectrum::BINS as u32;
    }
    let mut sp = spectrum::Spectrum::new();
    let bins = sp.synth(rate, f64::from_bits(amp_bits));
    unsafe {
        std::ptr::copy_nonoverlapping(bins.as_ptr(), buf, spectrum::BINS);
    }
    0
}

// ---------------- 会话 ABI（S2 后半） ----------------
// 句柄式：open 返回 Box<LoopbackSession> 的裸指针，close 收回。
// endpoint_id 为 UTF-16（与 Go 侧传 LPWSTR 的习惯一致），空指针 = 默认设备。

/// 打开一路回环。成功返回句柄；失败 err 写入 UTF-8 错误串（返回 0）。
#[no_mangle]
pub extern "system" fn ikf_loopback_open(
    endpoint_id: *const u16,
    err_buf: *mut u8,
    err_cap: u32,
) -> usize {
    ensure_panic_hook();
    let ep = if endpoint_id.is_null() {
        None
    } else {
        unsafe {
            let mut len = 0usize;
            while *endpoint_id.add(len) != 0 {
                len += 1;
            }
            Some(String::from_utf16_lossy(std::slice::from_raw_parts(endpoint_id, len)))
        }
    };
    match loopback::LoopbackSession::open(ep.as_deref()) {
        Ok(s) => {
            let h = Box::into_raw(Box::new(s)) as usize;
            reg_put(&LB_LIVE, h);
            h
        }
        Err(e) => {
            write_err(err_buf, err_cap, &e);
            0
        }
    }
}

#[no_mangle]
pub extern "system" fn ikf_loopback_close(handle: usize) {
    if handle != 0 && reg_take(&LB_LIVE, handle) {
        unsafe {
            drop(Box::from_raw(handle as *mut loopback::LoopbackSession));
        }
    }
}

/// 运行状态快照（调用方逐字段读）。
/// 返回 0 成功；句柄无效返回 1。
#[no_mangle]
pub extern "system" fn ikf_loopback_status(
    handle: usize,
    out: *mut loopback::LbStatusOut,
) -> u32 {
    if handle == 0 || out.is_null() {
        return 1;
    }
    if !reg_has(&LB_LIVE, handle) {
        return 1;
    }
    let s = unsafe { &*(handle as *const loopback::LoopbackSession) };
    let st = &s.state;
    let o = unsafe { &mut *out };
    o.running = st.running.load(Ordering::SeqCst) as i32;
    o.rate = st.rate.load(Ordering::SeqCst);
    o.ch = st.ch.load(Ordering::SeqCst);
    o.bits = st.bits.load(Ordering::SeqCst);
    o.raw_tag = st.raw_tag.load(Ordering::SeqCst) as u16;
    o.sub_tag = st.sub_tag.load(Ordering::SeqCst) as u16;
    o.chan_mask = st.chan_mask.load(Ordering::SeqCst);
    o.block_align = st.block_align.load(Ordering::SeqCst);
    o.frames = st.frames.load(Ordering::SeqCst);
    o.level = s.level();
    o.err_code = st.err_code.load(Ordering::SeqCst);
    copy_status_str(&st.kind.lock().unwrap_or_else(|e| e.into_inner()), &mut o.kind);
    copy_status_str(&st.dev.lock().unwrap_or_else(|e| e.into_inner()), &mut o.dev);
    copy_status_str(&st.err.lock().unwrap_or_else(|e| e.into_inner()), &mut o.err);
    0
}

/// 拉最新 512 字节频谱。返回 0 成功。
#[no_mangle]
pub extern "system" fn ikf_loopback_spectrum(handle: usize, buf: *mut u8, cap: u32) -> u32 {
    if handle == 0 || buf.is_null() || cap < loopback::BINS as u32 {
        return 1;
    }
    if !reg_has(&LB_LIVE, handle) {
        return 1;
    }
    let s = unsafe { &*(handle as *const loopback::LoopbackSession) };
    let mut out = [0u8; loopback::BINS];
    if !s.spectrum(&mut out) {
        return 1;
    }
    unsafe {
        std::ptr::copy_nonoverlapping(out.as_ptr(), buf, loopback::BINS);
    }
    0
}

/// 播放端点列表，写 "id\tname\n" 行（UTF-8）。
/// 返回需要的长度（buf 为空/不足时不写入）；0 = 失败。
#[no_mangle]
pub extern "system" fn ikf_loopback_devices(buf: *mut u8, cap: u32) -> u32 {
    let list = match loopback::list_render_endpoints() {
        Ok(l) => l,
        Err(e) => {
            write_err(buf, cap, &e);
            return 0;
        }
    };
    let mut packed = Vec::new();
    for (id, name, is_def) in list {
        packed.extend_from_slice(id.as_bytes());
        packed.push(b'\t');
        packed.extend_from_slice(name.as_bytes());
        packed.push(b'\t');
        packed.push(if is_def { b'1' } else { b'0' });
        packed.push(b'\n');
    }
    let n = packed.len() as u32;
    if !buf.is_null() && cap >= n {
        unsafe {
            std::ptr::copy_nonoverlapping(packed.as_ptr(), buf, n as usize);
        }
    }
    n
}

// ---------------- 监听 ABI（S3） ----------------

/// 打开麦克风监听。成功返回句柄；失败 err 写入 UTF-8 错误串（返回 0）。
#[no_mangle]
pub extern "system" fn ikf_listen_open(err_buf: *mut u8, err_cap: u32) -> usize {
    ensure_panic_hook();
    match listen::ListenSession::open() {
        Ok(s) => {
            let h = Box::into_raw(Box::new(s)) as usize;
            reg_put(&LS_LIVE, h);
            h
        }
        Err(e) => {
            write_err(err_buf, err_cap, &e);
            0
        }
    }
}

#[no_mangle]
pub extern "system" fn ikf_listen_close(handle: usize) {
    if handle != 0 && reg_take(&LS_LIVE, handle) {
        unsafe {
            drop(Box::from_raw(handle as *mut listen::ListenSession));
        }
    }
}

/// 返回 0 成功；句柄无效返回 1。
/// 设置监听增益（f32 按位传 u64，x64 syscall 不走 xmm）。实时生效。
#[no_mangle]
pub extern "system" fn ikf_listen_set_gain(handle: usize, gain_bits: u64) -> u32 {
    if handle == 0 {
        return 1;
    }
    if !reg_has(&LS_LIVE, handle) {
        return 1;
    }
    let s = unsafe { &*(handle as *const listen::ListenSession) };
    let gain = f32::from_bits(gain_bits as u32);
    let gain = if gain.is_finite() && gain > 0.0 { gain } else { 1.0 };
    s.state.gain_bits.store(gain.to_bits(), Ordering::SeqCst);
    0
}

#[no_mangle]
pub extern "system" fn ikf_listen_status(handle: usize, out: *mut listen::LsStatusOut) -> u32 {
    if handle == 0 || out.is_null() {
        return 1;
    }
    if !reg_has(&LS_LIVE, handle) {
        return 1;
    }
    let s = unsafe { &*(handle as *const listen::ListenSession) };
    let st = &s.state;
    let o = unsafe { &mut *out };
    o.running = st.running.load(Ordering::SeqCst) as i32;
    o.rate = st.rate.load(Ordering::SeqCst);
    o.ch = st.ch.load(Ordering::SeqCst);
    o.sent = st.sent.load(Ordering::SeqCst);
    o.pkts = st.pkts.load(Ordering::SeqCst);
    o.silent_pkts = st.silent_pkts.load(Ordering::SeqCst);
    o.cap_peak = f32::from_bits(st.cap_peak_bits.load(Ordering::SeqCst));
    copy_status_str(&st.dev.lock().unwrap_or_else(|e| e.into_inner()), &mut o.dev);
    copy_status_str(&st.out.lock().unwrap_or_else(|e| e.into_inner()), &mut o.out);
    copy_status_str(&st.err.lock().unwrap_or_else(|e| e.into_inner()), &mut o.err);
    0
}

fn write_err(buf: *mut u8, cap: u32, msg: &str) {
    if buf.is_null() || cap == 0 {
        return;
    }
    let mut b = msg.as_bytes();
    let cap = cap as usize;
    if b.len() + 1 > cap {
        b = &b[..cap.saturating_sub(1)];
    }
    unsafe {
        std::ptr::copy_nonoverlapping(b.as_ptr(), buf, b.len());
        *buf.add(b.len()) = 0;
    }
}

fn copy_status_str(src: &str, dst: &mut [u8]) {
    let b = src.as_bytes();
    let n = b.len().min(dst.len() - 1);
    dst[..n].copy_from_slice(&b[..n]);
    dst[n] = 0;
}

// ---------------- 录音 ABI（S4） ----------------

/// 打开录音会话。wav_path 为 UTF-8 全路径。成功返回句柄；失败 err 写错误串。
#[no_mangle]
pub extern "system" fn ikf_recorder_open(
    wav_path: *const u16,
    err_buf: *mut u8,
    err_cap: u32,
) -> usize {
    ensure_panic_hook();
    if wav_path.is_null() {
        write_err(err_buf, err_cap, "缺少 WAV 路径");
        return 0;
    }
    unsafe {
        let mut len = 0usize;
        while *wav_path.add(len) != 0 {
            len += 1;
        }
        let path = String::from_utf16_lossy(std::slice::from_raw_parts(wav_path, len));
        match recorder::RecSession::open(&path) {
            Ok(s) => {
                let h = Box::into_raw(Box::new(s)) as usize;
                reg_put(&REC_LIVE, h);
                h
            }
            Err(e) => {
                write_err(err_buf, err_cap, &e);
                0
            }
        }
    }
}

#[no_mangle]
pub extern "system" fn ikf_recorder_close(handle: usize) {
    if handle != 0 && reg_take(&REC_LIVE, handle) {
        unsafe {
            drop(Box::from_raw(handle as *mut recorder::RecSession));
        }
    }
}

/// 返回 0 成功；句柄无效返回 1。
#[no_mangle]
pub extern "system" fn ikf_recorder_status(handle: usize, out: *mut recorder::RecStatusOut) -> u32 {
    if handle == 0 || out.is_null() {
        return 1;
    }
    if !reg_has(&REC_LIVE, handle) {
        return 1;
    }
    let s = unsafe { &*(handle as *const recorder::RecSession) };
    let st = &s.state;
    let o = unsafe { &mut *out };
    o.running = st.running.load(Ordering::SeqCst) as i32;
    o.rate = st.rate.load(Ordering::SeqCst);
    o.ch = st.ch.load(Ordering::SeqCst);
    o.frames = st.frames.load(Ordering::SeqCst);
    o.size = st.size.load(Ordering::SeqCst);
    o.err_code = st.err_code.load(Ordering::SeqCst);
    copy_status_str(&st.kind.lock().unwrap_or_else(|e| e.into_inner()), &mut o.kind);
    copy_status_str(&st.dev.lock().unwrap_or_else(|e| e.into_inner()), &mut o.dev);
    copy_status_str(&st.file.lock().unwrap_or_else(|e| e.into_inner()), &mut o.file);
    copy_status_str(&st.err.lock().unwrap_or_else(|e| e.into_inner()), &mut o.err);
    0
}

#[cfg(test)]
mod loopback_tests {
    use super::*;

    /// 真机测试：默认播放设备枚举必须成功（不采集，纯枚举）。
    #[test]
    fn devices_list_succeeds() {
        let list = loopback::list_render_endpoints().expect("端点枚举失败");
        assert!(!list.is_empty(), "至少有一个渲染端点");
        for (id, name, _) in &list {
            assert!(id.starts_with('{'), "端点 ID 形如 {{0.0.1.00000000}}.{{guid}}: {}", id);
            assert!(!name.is_empty(), "友好名不应为空");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn selftest_sentinel() {
        assert_eq!(ikf_selftest(), 0xF00D);
    }

    #[test]
    fn version_fits_and_roundtrips() {
        let n = ikf_version(std::ptr::null_mut(), 0);
        let mut buf = vec![0i8; n as usize];
        assert_eq!(ikf_version(buf.as_mut_ptr(), n), n);
        let s: String = buf[..(n - 1) as usize].iter().map(|&c| c as u8 as char).collect();
        assert!(s.starts_with("ikf-audio 0."));
    }
}

/// panic 落盘端到端：hook 挂上后，一次真实 panic 必须在 panic 日志里
/// 留下现场（位置 + 消息）。只在 panic="unwind"（dev 套件）下可测 ——
/// abort 策略下无法回捕，这正是生产选择落盘取证而非 catch_unwind 的原因。
/// libtest 自身装有输出捕获 hook，测完还原，避免影响其他用例的输出。
#[cfg(all(test, panic = "unwind"))]
mod panic_hook_tests {
    use super::*;

    #[test]
    fn panic_lands_in_log_file() {
        let prev = std::panic::take_hook();
        ensure_panic_hook();
        let path = panic_log_path().expect("panic 日志路径应可解析");
        let marker = format!("panic-hook 自检 pid={}", std::process::id());
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            panic!("{}", marker);
        }));
        std::panic::set_hook(prev);
        let txt = std::fs::read_to_string(&path).unwrap_or_default();
        assert!(txt.contains(&marker), "panic 现场未落盘：{}", path.display());
        assert!(txt.contains("lib.rs"), "落盘内容应含触发位置 lib.rs");
    }
}

#[cfg(test)]
mod listen_tests {
    use super::*;

    /// 真机：监听会话打开 + 2 秒内 sent 增长 + 干净关闭。
    #[test]
    fn listen_open_and_stream() {
        let mut s = listen::ListenSession::open().expect("监听打开失败");
        std::thread::sleep(std::time::Duration::from_secs(2));
        let sent1 = s.state.sent.load(Ordering::SeqCst);
        let pkts = s.state.pkts.load(Ordering::SeqCst);
        std::thread::sleep(std::time::Duration::from_secs(1));
        let sent2 = s.state.sent.load(Ordering::SeqCst);
        let running = s.state.running.load(Ordering::SeqCst);
        s.close();
        eprintln!("sent: {} → {} pkts={} running={}", sent1, sent2, pkts, running);
        assert!(running, "会话应仍在跑");
        assert!(sent2 > sent1, "sent 应增长");
        assert!(pkts > 0, "应有采集包");
    }
}

#[cfg(test)]
mod lbdiag_tests {
    use crate::loopback::LoopbackSession;
    use std::sync::atomic::Ordering;
    use std::time::Duration;

    /// 真机诊断：打印混音格式解析值 + 采样统计（定位"满速静默"回归）。
    #[test]
    fn lb_diag_print() {
        let mut s = LoopbackSession::open(None).expect("open 失败");
        let st = &s.state;
        eprintln!(
            "rate={} ch={} bits={} tag(raw={:#06x} sub={:#06x}) mask={:#x} align={}",
            st.rate.load(Ordering::SeqCst),
            st.ch.load(Ordering::SeqCst),
            st.bits.load(Ordering::SeqCst),
            st.raw_tag.load(Ordering::SeqCst),
            st.sub_tag.load(Ordering::SeqCst),
            st.chan_mask.load(Ordering::SeqCst),
            st.block_align.load(Ordering::SeqCst),
        );
        std::thread::sleep(Duration::from_secs(2));
        let frames = st.frames.load(Ordering::SeqCst);
        let sp = s.spectrum(&mut [0u8; crate::spectrum::BINS]); // 占位：拿锁看 level
        let level = s.level();
        s.close();
        eprintln!("frames={frames} spectrum_ok={sp} level={level}");
    }
}
