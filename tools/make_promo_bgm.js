// 生成 iKF 宣传片配乐（氛围电子，96 BPM，A 小调）
// 用法: node 工具/make_promo_bgm.js
const fs = require("fs");
const path = require("path");

const SR = 44100;
const FPS = 30;
const TOTAL_FRAMES = 1668;
const DUR = TOTAL_FRAMES / FPS; // 52.93s
const N = Math.ceil(SR * DUR);
const BEAT = 60 / 96; // 0.625s

const L = new Float32Array(N);
const R = new Float32Array(N);

const TWO_PI = Math.PI * 2;

function env(t, dur, atk, rel) {
  if (t < 0 || t > dur) return 0;
  if (t < atk) return t / atk;
  if (t > dur - rel) return Math.max(0, (dur - t) / rel);
  return 1;
}

function addTone(freq, start, dur, amp, opts = {}) {
  const atk = opts.atk ?? 0.01;
  const rel = opts.rel ?? Math.min(0.4, dur * 0.5);
  const detune = opts.detune ?? 0;
  const pan = opts.pan ?? 0;
  const i0 = Math.floor(start * SR);
  const n = Math.floor(dur * SR);
  const gl = amp * (1 - Math.max(0, pan));
  const gr = amp * (1 + Math.min(0, pan));
  for (let k = 0; k < n; k++) {
    const i = i0 + k;
    if (i < 0 || i >= N) continue;
    const t = k / SR;
    const e = env(t, dur, atk, rel);
    if (e <= 0) continue;
    const ph = TWO_PI * freq * t;
    let s = Math.sin(ph);
    if (opts.harm2) s += Math.sin(ph * 2) * opts.harm2;
    if (opts.harm3) s += Math.sin(ph * 3) * opts.harm3;
    if (detune) s += Math.sin(TWO_PI * (freq * (1 + detune)) * t) * 0.5;
    L[i] += s * e * gl;
    R[i] += s * e * gr;
  }
}

function addNoise(start, dur, amp, opts = {}) {
  const i0 = Math.floor(start * SR);
  const n = Math.floor(dur * SR);
  const atk = opts.atk ?? 0.001;
  let lp = 0;
  const c = opts.lp ?? 0.5;
  for (let k = 0; k < n; k++) {
    const i = i0 + k;
    if (i < 0 || i >= N) continue;
    const t = k / SR;
    const e = env(t, dur, atk, dur * 0.9);
    const white = Math.random() * 2 - 1;
    lp += (white - lp) * c;
    L[i] += lp * e * amp;
    R[i] += lp * e * amp;
  }
}

function kick(start, amp) {
  const i0 = Math.floor(start * SR);
  const n = Math.floor(0.26 * SR);
  for (let k = 0; k < n; k++) {
    const i = i0 + k;
    if (i < 0 || i >= N) continue;
    const t = k / SR;
    const f = 108 * Math.exp(-t * 26) + 42;
    const e = Math.exp(-t * 13);
    const s = Math.sin(TWO_PI * f * t) * e;
    L[i] += s * amp;
    R[i] += s * amp;
  }
}

/* ---------------- 和声进行 ---------------- */
// [起始秒, 根音, 和弦音(Hz)]
const CHORDS = [
  [0.0, 55.0, [110.0, 164.81, 220.0, 261.63]], // Am
  [9.5, 87.31, [87.31, 174.61, 220.0, 261.63]], // F
  [19.0, 65.41, [130.81, 196.0, 261.63, 329.63]], // C
  [28.5, 49.0, [98.0, 196.0, 246.94, 293.66]], // G
  [38.0, 55.0, [110.0, 164.81, 220.0, 261.63]], // Am
  [46.5, 55.0, [110.0, 220.0, 261.63, 329.63]], // Am (outro)
];

// Pad：每个和弦一段，交叉淡入淡出
CHORDS.forEach(([t0, root, notes], idx) => {
  const next = CHORDS[idx + 1];
  const segDur = (next ? next[0] : DUR) - t0 + 1.6;
  notes.forEach((f, j) => {
    addTone(f, t0, segDur, 0.052, {
      atk: 1.5,
      rel: 2.0,
      detune: j % 2 === 0 ? 0.0018 : -0.0014,
      harm2: 0.16,
      harm3: 0.06,
      pan: (j - 1.5) * 0.12,
    });
  });
  // 低音铺底
  addTone(root, t0, segDur, 0.1, { atk: 1.0, rel: 1.8, harm2: 0.1 });
});

// Sub 脉冲：每两拍一次，跟和弦根音
CHORDS.forEach(([t0, root, notes], idx) => {
  const next = CHORDS[idx + 1];
  const end = next ? next[0] : DUR - 3;
  if (t0 < 8) return; // 前 8 秒只有 pad
  for (let t = t0; t < end; t += BEAT * 4) {
    addTone(root, t, BEAT * 1.6, 0.13, { atk: 0.02, rel: 0.7, harm2: 0.12 });
  }
});

// Kick：第 12 秒后进入，每两拍
for (let t = 12; t < DUR - 5; t += BEAT * 2) {
  const fade = t > DUR - 9 ? Math.max(0, (DUR - 5 - t) / 4) : 1;
  kick(t, 0.2 * fade);
}

// Arp：八分音符，和弦音上八度
CHORDS.forEach(([t0, root, notes], idx) => {
  const next = CHORDS[idx + 1];
  const end = next ? next[0] : DUR - 6;
  if (t0 < 16) return;
  let step = 0;
  for (let t = t0; t < end; t += BEAT * 0.5) {
    const f = notes[step % notes.length] * 2;
    const amp = 0.028 * (0.7 + 0.3 * Math.sin(step * 0.9));
    addTone(f, t, 0.55, amp, { atk: 0.005, rel: 0.5, harm2: 0.2 });
    step++;
  }
});

// 高频微光：极轻的噪声铺底，只在段落交界处
[0, 9.5, 19, 28.5, 38].forEach((t) => {
  addNoise(t, 3.2, 0.0055, { lp: 0.28, atk: 1.4 });
});

/* ---------------- 简易空间混响 ---------------- */
const taps = [
  [0.089, 0.3],
  [0.157, 0.2],
  [0.263, 0.13],
  [0.397, 0.08],
];
const wetL = new Float32Array(N);
const wetR = new Float32Array(N);
taps.forEach(([d, g], idx) => {
  const off = Math.floor(d * SR);
  for (let i = off; i < N; i++) {
    if (idx % 2 === 0) {
      wetL[i] += L[i - off] * g;
      wetR[i] += R[i - off] * g * 0.9;
    } else {
      wetL[i] += L[i - off] * g * 0.9;
      wetR[i] += R[i - off] * g;
    }
  }
});

/* ---------------- 母带 ---------------- */
let peak = 0;
for (let i = 0; i < N; i++) {
  const l = L[i] + wetL[i] * 0.5;
  const r = R[i] + wetR[i] * 0.5;
  L[i] = l;
  R[i] = r;
  peak = Math.max(peak, Math.abs(l), Math.abs(r));
}

const gain = 0.82 / (peak || 1);

// 首尾淡入淡出
const fadeIn = Math.floor(1.2 * SR);
const fadeOut = Math.floor(3.2 * SR);

const buf = Buffer.alloc(44 + N * 4);
buf.write("RIFF", 0);
buf.writeUInt32LE(36 + N * 4, 4);
buf.write("WAVE", 8);
buf.write("fmt ", 12);
buf.writeUInt32LE(16, 16);
buf.writeUInt16LE(1, 20);
buf.writeUInt16LE(2, 22);
buf.writeUInt32LE(SR, 24);
buf.writeUInt32LE(SR * 4, 28);
buf.writeUInt16LE(4, 32);
buf.writeUInt16LE(16, 34);
buf.write("data", 36);
buf.writeUInt32LE(N * 4, 40);

for (let i = 0; i < N; i++) {
  let g = gain;
  if (i < fadeIn) g *= i / fadeIn;
  if (i > N - fadeOut) g *= (N - i) / fadeOut;
  const l = Math.max(-1, Math.min(1, L[i] * g));
  const r = Math.max(-1, Math.min(1, R[i] * g));
  buf.writeInt16LE(Math.round(l * 32767), 44 + i * 4);
  buf.writeInt16LE(Math.round(r * 32767), 44 + i * 4 + 2);
}

const out = path.join(__dirname, "..", "宣传视频", "public", "bgm.wav");
fs.writeFileSync(out, buf);
console.log("wrote", out, (buf.length / 1024 / 1024).toFixed(2), "MB", "dur", DUR.toFixed(2), "peak", peak.toFixed(3));
