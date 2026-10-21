// 分析配乐：BPM、节拍网格、能量曲线、段落切点、强起音点
// 纯 Node，无第三方依赖（自带基 2 FFT）
const fs = require("fs");
const path = require("path");

const file = process.argv[2] || "audio/song.wav";
const outJson = process.argv[3] || "audio/song.json";

// ---------- WAV ----------
const buf = fs.readFileSync(file);
if (buf.toString("ascii", 0, 4) !== "RIFF") throw new Error("not RIFF");
let p = 12, fmt = null, dataOff = 0, dataLen = 0;
while (p < buf.length - 8) {
  const id = buf.toString("ascii", p, p + 4);
  const sz = buf.readUInt32LE(p + 4);
  if (id === "fmt ") fmt = { ch: buf.readUInt16LE(p + 10), sr: buf.readUInt32LE(p + 12), bits: buf.readUInt16LE(p + 22) };
  if (id === "data") { dataOff = p + 8; dataLen = sz; break; }
  p += 8 + sz + (sz & 1);
}
const SR = fmt.sr, CH = fmt.ch, BITS = fmt.bits;
const n = Math.floor(dataLen / (BITS / 8) / CH);
const x = new Float32Array(n);
for (let i = 0; i < n; i++) {
  if (BITS === 16) x[i] = buf.readInt16LE(dataOff + i * 2 * CH) / 32768;
  else x[i] = buf.readFloatLE(dataOff + i * 4 * CH);
}
const dur = n / SR;

// ---------- FFT ----------
function fft(re, im) {
  const N = re.length;
  for (let i = 1, j = 0; i < N; i++) {
    let bit = N >> 1;
    for (; j & bit; bit >>= 1) j ^= bit;
    j ^= bit;
    if (i < j) { [re[i], re[j]] = [re[j], re[i]]; [im[i], im[j]] = [im[j], im[i]]; }
  }
  for (let len = 2; len <= N; len <<= 1) {
    const ang = (-2 * Math.PI) / len;
    const wr = Math.cos(ang), wi = Math.sin(ang);
    for (let i = 0; i < N; i += len) {
      let cr = 1, ci = 0;
      for (let k = 0; k < len / 2; k++) {
        const ur = re[i + k], ui = im[i + k];
        const vr = re[i + k + len / 2] * cr - im[i + k + len / 2] * ci;
        const vi = re[i + k + len / 2] * ci + im[i + k + len / 2] * cr;
        re[i + k] = ur + vr; im[i + k] = ui + vi;
        re[i + k + len / 2] = ur - vr; im[i + k + len / 2] = ui - vi;
        const ncr = cr * wr - ci * wi;
        ci = cr * wi + ci * wr; cr = ncr;
      }
    }
  }
}

// ---------- STFT 谱通量 ----------
const NFFT = 1024, HOP = 256; // 11.6ms per frame
const nFrames = Math.floor((n - NFFT) / HOP);
const win = new Float32Array(NFFT);
for (let i = 0; i < NFFT; i++) win[i] = 0.5 - 0.5 * Math.cos((2 * Math.PI * i) / NFFT);
const NB = NFFT / 2;
let prevMag = new Float32Array(NB);
const flux = new Float32Array(nFrames);
const lowE = new Float32Array(nFrames); // 低频能量（kick/bass）
const midE = new Float32Array(nFrames);
const highE = new Float32Array(nFrames);
const rms = new Float32Array(nFrames);
const binHz = SR / NFFT;

for (let f = 0; f < nFrames; f++) {
  const re = new Float32Array(NFFT), im = new Float32Array(NFFT);
  const s = f * HOP;
  let sum = 0;
  for (let i = 0; i < NFFT; i++) { const v = x[s + i]; re[i] = v * win[i]; sum += v * v; }
  rms[f] = Math.sqrt(sum / NFFT);
  fft(re, im);
  let fl = 0;
  for (let k = 1; k < NB; k++) {
    const mag = Math.hypot(re[k], im[k]);
    const d = mag - prevMag[k];
    if (d > 0) fl += d;
    prevMag[k] = mag;
    const hz = k * binHz;
    if (hz >= 30 && hz < 160) lowE[f] += mag;
    else if (hz >= 160 && hz < 2000) midE[f] += mag;
    else if (hz >= 2000 && hz < 12000) highE[f] += mag;
  }
  flux[f] = fl;
}
const fr = SR / HOP; // frames per second

// 归一化
function norm(a) {
  let mx = 0;
  for (const v of a) if (v > mx) mx = v;
  const o = new Float32Array(a.length);
  for (let i = 0; i < a.length; i++) o[i] = a[i] / (mx || 1);
  return o;
}
const fx = norm(flux), lo = norm(lowE), md = norm(midE), hi = norm(highE);

// ---------- 包络平滑 ----------
function smooth(a, rad) {
  const o = new Float32Array(a.length);
  for (let i = 0; i < a.length; i++) {
    let s = 0, c = 0;
    for (let j = Math.max(0, i - rad); j <= Math.min(a.length - 1, i + rad); j++) { s += a[j]; c++; }
    o[i] = s / c;
  }
  return o;
}
const fxs = smooth(fx, 8);

// ---------- 节拍检测 ----------
// 用低频 + 谱通量合成节拍强度
const beatStr = new Float32Array(nFrames);
for (let i = 0; i < nFrames; i++) beatStr[i] = 0.55 * lo[i] + 0.45 * fx[i];

const BPM_MIN = 60, BPM_MAX = 180;
let best = { bpm: 0, score: 0 };
for (let bpm = BPM_MIN; bpm <= BPM_MAX; bpm += 0.05) {
  const period = (60 / bpm) * fr;
  if (period < 2) continue;
  let sc = 0;
  for (let ph = 0; ph < period; ph += 1) {
    let s = 0, c = 0;
    for (let t = ph; t < nFrames; t += period) { s += beatStr[Math.round(t)] || 0; c++; }
    sc = Math.max(sc, s / Math.max(1, c));
  }
  if (sc > best.score) best = { bpm, score: sc };
}
// 用 2 倍/0.5 倍校正到常见区间（70~160）
let bpm = best.bpm;
if (bpm < 70) bpm *= 2;
if (bpm < 70) bpm *= 2;
if (bpm > 165) bpm /= 2;
// 精修 bpm（在 ±1 内细扫）
let fine = { bpm, score: 0 };
const period0 = (60 / bpm) * fr;
for (let b = bpm - 1.2; b <= bpm + 1.2; b += 0.01) {
  const period = (60 / b) * fr;
  let s = 0, c = 0;
  for (let ph = 0; ph < period; ph += 1) {
    let t0 = 0;
    for (let t = ph; t < nFrames; t += period) { t0 += beatStr[Math.round(t)] || 0; c++; }
    if (t0 > s) s = t0;
  }
  if (s > fine.score) fine = { bpm: b, score: s };
}
bpm = fine.bpm;

// 最佳相位：beat 网格起点
const period = (60 / bpm) * fr;
let bestPh = 0, bestPhScore = -1;
for (let ph = 0; ph < period; ph += 0.5) {
  let s = 0;
  for (let t = ph; t < nFrames; t += period) s += beatStr[Math.round(t)] || 0;
  if (s > bestPhScore) { bestPhScore = s; bestPh = ph; }
}
const beatTimes = [];
for (let t = bestPh; t < nFrames; t += period) beatTimes.push(+(t / fr).toFixed(4));

// 每拍强度（用于找 drop / 段落）
const beatStrength = beatTimes.map((t) => (beatStr[Math.min(nFrames - 1, Math.round(t * fr))] || 0));

// ---------- 能量曲线（每 0.5s） ----------
const step = Math.round(fr * 0.5);
const energy = [];
for (let i = 0; i < nFrames; i += step) {
  let a = 0, l = 0, h = 0, m = 0, c = 0;
  for (let j = i; j < Math.min(nFrames, i + step); j++) { a += fx[j]; l += lo[j]; m += md[j]; h += hi[j]; c++; }
  energy.push({ t: +(i / fr).toFixed(2), flux: +(a / c).toFixed(4), low: +(l / c).toFixed(4), mid: +(m / c).toFixed(4), high: +(h / c).toFixed(4) });
}

// ---------- 段落边界：能量突变 ----------
const winS = 8; // 8 * 0.5s = 4s
const segs = [];
for (let i = winS; i < energy.length - winS; i++) {
  let before = 0, after = 0;
  for (let j = i - winS; j < i; j++) before += energy[j].flux + energy[j].low * 0.8;
  for (let j = i; j < i + winS; j++) after += energy[j].flux + energy[j].low * 0.8;
  before /= winS; after /= winS;
  const jump = (after - before) / (before + 0.02);
  segs.push({ t: energy[i].t, jump: +jump.toFixed(3) });
}
segs.sort((a, b) => b.jump - a.jump);
const cuts = [];
for (const s of segs) {
  if (s.t < 3 || !isFinite(s.jump)) continue;
  if (cuts.some((c) => Math.abs(c.t - s.t) < 4)) continue;
  cuts.push(s);
  if (cuts.length >= 14) break;
}
cuts.sort((a, b) => a.t - b.t);

// ---------- 报告 ----------
const out = {
  file, sr: SR, duration: +dur.toFixed(2), bpm: +bpm.toFixed(2),
  beatPeriod: +(60 / bpm).toFixed(4), firstBeat: +(bestPh / fr).toFixed(3),
  beatCount: beatTimes.length, beatTimes: beatTimes.slice(0, 400), beatStrength,
  energy, cuts, frames: nFrames, fps: +fr.toFixed(2),
};
fs.writeFileSync(outJson, JSON.stringify(out, null, 1));

console.log("dur(s)      ", dur.toFixed(2));
console.log("BPM         ", bpm.toFixed(2), " period", (60 / bpm).toFixed(4), "s");
console.log("beats       ", beatTimes.length, " first", (bestPh / fr).toFixed(3));
console.log("drops/cuts  ");
cuts.forEach((c) => console.log("   ", c.t.toFixed(2) + "s", "jump", c.jump));
console.log("energy every 2s:");
for (let i = 0; i < energy.length; i += 4) {
  const e = energy[i];
  const bar = "#".repeat(Math.round(e.flux * 40));
  console.log(`  ${String(e.t).padStart(6)}s  ${e.flux.toFixed(3)} L${e.low.toFixed(2)} M${e.mid.toFixed(2)} H${e.high.toFixed(2)} ${bar}`);
}
