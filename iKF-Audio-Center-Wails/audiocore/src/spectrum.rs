//! 频谱计算 —— Go loopback.go 的逐字节复刻。
//!
//! 目标不是"像"，是**与 Go 版输出 512 字节完全一致**（双跑对比护栏）。
//! 因此：运算顺序、f32/f64 混合点、查表公式全部照抄 Go 源；
//! hypot 用 Go 的公式 x*sqrt(1+(y/x)^2)（x=min），不用 libm 的 hypot。

pub const RING_LEN: usize = 8192;
pub const FFT_SIZE: usize = 1024;
pub const BINS: usize = FFT_SIZE / 2;

const WA_MIN_DB: f64 = -100.0;
const WA_MAX_DB: f64 = -30.0;
const WA_SMOOTHING: f64 = 0.8;
/// 与 Go 一致：|X[k]| / N（实测反解的 Chromium 口径，见 loopback.go waMagScale）
const WA_MAG_SCALE: f64 = 1.0 / FFT_SIZE as f64;

/// Go math.Hypot 的公式复刻（x=min, y=max, x*sqrt(1+(y/x)^2)），非 libm。
fn hypot_go(x: f64, y: f64) -> f64 {
    let (mut x, mut y) = (x.abs(), y.abs());
    if x > y {
        std::mem::swap(&mut x, &mut y);
    }
    if x == 0.0 {
        return y;
    }
    let r = y / x;
    x * (1.0 + r * r).sqrt()
}

/// 表驱动：cos/sin/Blackman 窗，公式与 Go init() 逐字一致。
pub struct Tables {
    pub cos: Vec<f64>,
    pub sin: Vec<f64>,
    pub win: Vec<f64>,
}

impl Tables {
    pub fn new() -> Self {
        let mut t = Tables {
            cos: vec![0.0; FFT_SIZE],
            sin: vec![0.0; FFT_SIZE],
            win: vec![0.0; FFT_SIZE],
        };
        for i in 0..FFT_SIZE {
            let a = 2.0 * std::f64::consts::PI * i as f64 / FFT_SIZE as f64;
            t.cos[i] = a.cos();
            t.sin[i] = a.sin();
        }
        for n in 0..FFT_SIZE {
            let x = 2.0 * std::f64::consts::PI * n as f64 / FFT_SIZE as f64;
            t.win[n] = 0.42 - 0.5 * x.cos() + 0.08 * (2.0 * x).cos();
        }
        t
    }
}

/// Go fftRadix2 的逐行复刻（位反转置换 + 查表蝶形）。
fn fft_radix2(t: &Tables, re: &mut [f64], im: &mut [f64]) {
    let n = re.len();
    if n < 2 || n & (n - 1) != 0 {
        return;
    }
    let mut j = 0usize;
    for i in 1..n {
        let mut bit = n >> 1;
        while j & bit != 0 {
            j ^= bit;
            bit >>= 1;
        }
        j ^= bit;
        if i < j {
            re.swap(i, j);
            im.swap(i, j);
        }
    }
    let mut length = 2usize;
    while length <= n {
        let half = length >> 1;
        let step = n / length;
        let mut i = 0usize;
        while i < n {
            for k in 0..half {
                let idx = k * step;
                let cr = t.cos[idx];
                let ci = -t.sin[idx];
                let (ar, ai) = (re[i + k], im[i + k]);
                let (br, bi) = (re[i + k + half], im[i + k + half]);
                let tr = br * cr - bi * ci;
                let ti = br * ci + bi * cr;
                re[i + k] = ar + tr;
                im[i + k] = ai + ti;
                re[i + k + half] = ar - tr;
                im[i + k + half] = ai - ti;
            }
            i += length;
        }
        length <<= 1;
    }
}

/// 频谱状态：环形缓冲 + 平滑幅度 + 输出字节。
/// 字段语义与 Go 的 Loopback（ring/prev/bins/level）一一对应。
pub struct Spectrum {
    pub ring: Vec<f32>,
    pub ring_w: usize,
    pub ring_n: usize,
    pub prev: Vec<f32>,
    pub bins: Vec<u8>,
    pub level: f32,
    t: Tables,
    re: Vec<f64>,
    im: Vec<f64>,
}

impl Spectrum {
    pub fn new() -> Self {
        Spectrum {
            ring: vec![0.0; RING_LEN],
            ring_w: 0,
            ring_n: 0,
            prev: vec![0.0; BINS],
            bins: vec![0u8; BINS],
            level: 0.0,
            t: Tables::new(),
            re: vec![0.0; FFT_SIZE],
            im: vec![0.0; FFT_SIZE],
        }
    }

    /// 追加单声道样本（含静音帧：调用方传全 0 切片）。
    pub fn push(&mut self, samples: &[f32]) {
        for &s in samples {
            self.ring[self.ring_w] = s;
            self.ring_w = (self.ring_w + 1) % RING_LEN;
        }
        self.ring_n += samples.len();
    }

    /// Go computeSpectrum 的逐行复刻。每调用一次施加一次平滑（与墙钟无关）。
    pub fn compute(&mut self) {
        if self.ring_n < FFT_SIZE {
            return;
        }
        let start = (self.ring_w + RING_LEN - FFT_SIZE) % RING_LEN;
        for i in 0..FFT_SIZE {
            self.re[i] = self.ring[(start + i) % RING_LEN] as f64 * self.t.win[i];
            self.im[i] = 0.0;
        }
        fft_radix2(&self.t, &mut self.re, &mut self.im);
        let norm = WA_MAG_SCALE;
        let mut level: f32 = 0.0;
        for k in 0..BINS {
            let mag = hypot_go(self.re[k], self.im[k]) * norm;
            let s = WA_SMOOTHING * self.prev[k] as f64 + (1.0 - WA_SMOOTHING) * mag;
            self.prev[k] = s as f32;
            if s > level as f64 {
                level = s as f32;
            }
            let db = if s > 0.0 { 20.0 * s.log10() } else { -1000.0 };
            let mut v = (db - WA_MIN_DB) / (WA_MAX_DB - WA_MIN_DB) * 255.0;
            if v < 0.0 {
                v = 0.0;
            } else if v > 255.0 {
                v = 255.0;
            }
            self.bins[k] = v as u8; // Go byte() 同为截断
        }
        if level > 1.0 {
            level = 1.0;
        }
        self.level = level;
    }

    /// Go synthSpectrum 的复刻：937.5 Hz 正弦（bin 中心），平滑收敛 40 次。
    pub fn synth(&mut self, rate: u32, amp: f64) -> &[u8] {
        self.ring_w = FFT_SIZE;
        self.ring_n = FFT_SIZE;
        for i in 0..FFT_SIZE {
            let x = 2.0 * std::f64::consts::PI * 937.5 * i as f64 / rate as f64;
            self.ring[i] = (amp * x.sin()) as f32;
        }
        self.prev.iter_mut().for_each(|p| *p = 0.0);
        for _ in 0..40 {
            self.compute();
        }
        &self.bins
    }
}

/// Go lbDownmix 复刻：只取前左/前右平均（与立体声下混等价），不是全声道平均。
#[inline]
pub fn downmix(ch: usize) -> (usize, f32) {
    // ★max-abs 策略的兼容入口：多声道时 take=ch、inv=1（由 decode_block
    // 内部逐帧选绝对值最大的声道样本——平均会把信号稀释 1/ch，真机实测
    // 8ch 端点上音乐被稀释到 1/8 音量≈无声）。单声道语义不变。
    if ch >= 2 {
        (ch, 1.0)
    } else {
        (1, 1.0)
    }
}

/// 一帧内跨声道选绝对值最大的样本（保符号）。
#[inline]
fn frame_max_abs<'a, I: Iterator<Item = &'a [u8]>>(samples: I) -> f32 {
    let mut best = 0f32;
    for b in samples {
        let v = f32::from_le_bytes([b[0], b[1], b[2], b[3]]);
        if v.abs() > best.abs() {
            best = v;
        }
    }
    best
}

/// 交错多声道块 → 单声道 f32 序列。四种格式 + 未知格式写静音，与 Go pushFrames 一致。
pub fn decode_block(raw: &[u8], frames: usize, ch: usize, block_align: usize, tag: u16, bits: u16) -> Vec<f32> {
    let (take, _inv) = downmix(ch);
    // ★破坏性检测 P0-2：WASAPI 报的 block_align/ch 与实际 buffer 不一致
    // （设备被外部切格式、端点重建）时，raw[base+c*4..] 会越界 panic →
    // panic=abort 下就是 0xC0000409 且无日志。校验必须按**实际读宽**算：
    // 只查 frames*block_align 拦不住 block_align < ch×样本字节 的格式漂移
    // （那时最后一帧 base + ch×宽度 仍会出界）。逐帧读的最大偏移出现在
    // 最后一帧，故校验 (frames-1)*block_align + ch×宽度 ≤ raw.len() 即
    // 覆盖所有帧。越界按静音处理。
    let width: usize = match (tag, bits) {
        (3, 32) | (1, 32) => 4,
        (1, 16) => 2,
        (1, 24) => 3,
        _ => 0, // 未知格式：逐帧 match 本就写静音
    };
    if frames == 0 || block_align == 0 || ch == 0 || width == 0 {
        return vec![0.0f32; frames];
    }
    let last_base = (frames - 1).saturating_mul(block_align);
    if last_base.saturating_add(ch * width) > raw.len() {
        return vec![0.0f32; frames];
    }
    let mut out = Vec::with_capacity(frames);
    let le_u16 = |b: &[u8]| u16::from_le_bytes([b[0], b[1]]);
    let le_u32 = |b: &[u8]| u32::from_le_bytes([b[0], b[1], b[2], b[3]]);
    for f in 0..frames {
        let base = f * block_align;
        let v: f32 = match (tag, bits) {
            (3, 32) => (0..take)
                .map(|c| f32::from_bits(le_u32(&raw[base + c * 4..])))
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 16) => (0..take)
                .map(|c| (le_u16(&raw[base + c * 2..]) as i16) as f32 / 32768.0)
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 32) => (0..take)
                .map(|c| (le_u32(&raw[base + c * 4..]) as i32) as f32 / 2147483648.0)
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            (1, 24) => (0..take)
                .map(|c| {
                    let o = base + c * 3;
                    let mut v = raw[o] as i32 | (raw[o + 1] as i32) << 8 | (raw[o + 2] as i32) << 16;
                    if v & 0x800000 != 0 {
                        v |= !0xFFFFFF;
                    }
                    v as f32 / 8388608.0
                })
                .fold(0f32, |best, v| if v.abs() > best.abs() { v } else { best }),
            _ => 0.0, // 未知格式写静音
        };
        out.push(v);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn downmix_pairs() {
        // max-abs：多声道 take=ch、inv=1；立体声 (2,1)；单声道 (1,1)
        assert_eq!(downmix(8), (8, 1.0));
        assert_eq!(downmix(2), (2, 1.0));
        assert_eq!(downmix(1), (1, 1.0));
    }

    #[test]
    fn silence_is_zero() {
        let mut sp = Spectrum::new();
        let b = sp.synth(48000, 0.0).to_vec();
        assert!(b.iter().all(|&v| v == 0));
    }

    /// ★破坏性检测 P0-2 回归：格式漂移（block_align < ch×样本字节）时
    /// decode_block 必须静音返回，不得越界 panic（panic=abort → 0xC0000409）。
    #[test]
    fn decode_block_format_drift_is_silence() {
        // 声明 float32 8ch（读宽 32B）但 block_align=8、buffer=3×8：旧校验
        // frames*block_align ≤ raw.len() 放行，逐帧读必越界；现在应整块静音。
        let raw = [0u8; 24];
        let out = decode_block(&raw, 3, 8, 8, 3, 32);
        assert_eq!(out, vec![0.0f32; 3]);
        // 自洽格式仍正常解码：1 帧 2ch float32，FL=1.0 FR=-0.5 → max-abs=1.0
        let mut ok = vec![0u8; 8];
        ok[..4].copy_from_slice(&1.0f32.to_le_bytes());
        ok[4..].copy_from_slice(&(-0.5f32).to_le_bytes());
        assert_eq!(decode_block(&ok, 1, 2, 8, 3, 32), vec![1.0]);
        // 未知格式写静音（width=0 提前返回同形状）
        assert_eq!(decode_block(&ok, 1, 2, 8, 0xFF, 8), vec![0.0]);
    }
}
