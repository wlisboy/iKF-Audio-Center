// 生成「卡点图」：整曲结构 + 选用片段的小节/鼓点/分镜对位，输出自包含 HTML
const fs = require("fs");
const song = JSON.parse(fs.readFileSync("audio/song.json", "utf8"));
const META = JSON.parse(fs.readFileSync("audio/meta.json", "utf8"));

const W = 1560, H = 240, PAD = 44;

/* ---------- 1. 全曲能量 ---------- */
const e = song.energy;
const maxF = Math.max(...e.map((x) => x.flux));
const xOf = (t) => PAD + (t / song.duration) * (W - PAD * 2);
const yOf = (v) => H - PAD * 0.6 - (v / maxF) * (H - PAD * 1.6);

let path = "";
e.forEach((p, i) => {
  path += `${i ? "L" : "M"}${xOf(p.t).toFixed(1)},${yOf(p.flux).toFixed(1)}`;
});

const sections = [
  { t: 0, n: "前奏", c: "#5b5b66" },
  { t: 6.5, n: "入鼓", c: "#7a6a6a" },
  { t: 13.9, n: "铺底", c: "#8a6a6a" },
  { t: 29.9, n: "Drop 1", c: "#E60012" },
  { t: 59.9, n: "Breakdown", c: "#4a4a55" },
  { t: 71.3, n: "Build", c: "#9a6a6a" },
  { t: 90.0, n: "Drop 2", c: "#E60012" },
  { t: 119.3, n: "尾段", c: "#7a5a5a" },
  { t: 138.3, n: "Outro", c: "#3f3f48" },
];

const secLines = sections
  .map((s, i) => {
    const x = xOf(s.t);
    const next = sections[i + 1] ? xOf(sections[i + 1].t) : W - PAD;
    const w = next - x;
    return `<g>
      <rect x="${x.toFixed(0)}" y="${PAD * 0.7}" width="${(w - 2).toFixed(0)}" height="22" rx="4" fill="${s.c}" opacity="0.30"/>
      <text x="${(x + w / 2).toFixed(0)}" y="${PAD * 0.7 + 16}" text-anchor="middle" font-size="12" fill="${s.c === "#E60012" ? "#ff6b74" : "#c9c9d2"}">${s.n}</text>
      <line x1="${x.toFixed(0)}" y1="${PAD * 0.7 + 24}" x2="${x.toFixed(0)}" y2="${H - PAD * 0.5}" stroke="${s.c}" stroke-width="1" opacity="0.5" stroke-dasharray="3 4"/>
    </g>`;
  })
  .join("");

/* 选用窗口 */
const S = 65.625, D = 56.25;
const winX = xOf(S), winW = xOf(S + D) - winX;
const winBox = `<rect x="${winX.toFixed(0)}" y="${PAD * 0.7}" width="${winW.toFixed(0)}" height="${(H - PAD * 1.0).toFixed(0)}"
  fill="rgba(230,0,18,0.07)" stroke="#E60012" stroke-width="1.5"/>
  <text x="${(winX + 8).toFixed(0)}" y="${H - PAD * 0.55}" font-size="12" fill="#ff6b74">宣传片选用 65.6s – 121.9s（56.25s）</text>`;

const svg1 = `<svg viewBox="0 0 ${W} ${H}" width="100%" xmlns="http://www.w3.org/2000/svg">
  <rect width="${W}" height="${H}" fill="#0b0b10"/>
  ${secLines}
  <path d="${path}" fill="none" stroke="#ff5560" stroke-width="1.4" opacity="0.95"/>
  ${winBox}
  ${[0, 30, 60, 90, 120, 150].map((t) => `<text x="${xOf(t).toFixed(0)}" y="${H - 12}" text-anchor="middle" font-size="11" fill="#6a6a76">${t}s</text>`).join("")}
</svg>`;

/* ---------- 2. 片段：小节 + 鼓点 + 分镜 ---------- */
const W2 = 1560, H2 = 420, P2 = 48;
const BEAT = 60 / 128, BAR = BEAT * 4;
const tx = (t) => P2 + ((t - S) / D) * (W2 - P2 * 2);
const beatTicks = [];
for (let b = 0; b * BEAT + S < S + D; b++) {
  const t = S + b * BEAT;
  const isBar = b % 4 === 0;
  beatTicks.push(
    `<line x1="${tx(t).toFixed(1)}" y1="${isBar ? 60 : 200}" x2="${tx(t).toFixed(1)}" y2="262"
       stroke="${isBar ? "#6f6f7e" : "#33333c"}" stroke-width="${isBar ? 1.1 : 0.7}"/>` +
      (isBar ? `<text x="${tx(t).toFixed(1)}" y="54" text-anchor="middle" font-size="10" fill="#71717f">${b / 4 + 35}</text>` : "")
  );
}

const cuts = META.scenes;
const bars = cuts.map((c, i) => {
  const x1 = tx(S + c.from / 30);
  const x2 = tx(S + (c.from + c.dur) / 30);
  const y = 282 + (i % 2) * 62;
  const fill = c.drop ? "#E60012" : c.lift ? "#7a2630" : "#20202a";
  return `<g>
    <rect x="${x1.toFixed(1)}" y="${y}" width="${Math.max(2, x2 - x1 - 2).toFixed(1)}" height="44" rx="4" fill="${fill}" stroke="rgba(255,255,255,0.08)"/>
    <text x="${(x1 + 8).toFixed(1)}" y="${y + 18}" font-size="12" fill="${c.drop ? "#fff" : "#d8d8e0"}">${c.name}</text>
    <text x="${(x1 + 8).toFixed(1)}" y="${y + 34}" font-size="10" fill="${c.drop ? "rgba(255,255,255,0.8)" : "#8a8a96"}">${(c.from / 30).toFixed(2)}s</text>
  </g>`;
});

/* 鼓点强度柱（片段内逐拍） */
const kicks = [];
const kArr = META.kicks;
kArr.forEach((v, i) => {
  const x = tx(S + i * BEAT);
  const h = Math.max(2, (v / 0.71) * 120);
  kicks.push(`<rect x="${x.toFixed(1)}" y="${(196 - h).toFixed(1)}" width="3.2" height="${h.toFixed(1)}" fill="${v > 0.5 ? "#ff3b45" : "#5a3a40"}"/>`);
});

const dropX = tx(90);
const svg2 = `<svg viewBox="0 0 ${W2} ${H2}" width="100%" xmlns="http://www.w3.org/2000/svg">
  <rect width="${W2}" height="${H2}" fill="#0b0b10"/>
  <text x="${P2}" y="26" font-size="12" fill="#8a8a96">逐拍低频强度（红=Drop 后的鼓点）· 竖线=小节线（数字为小节号）</text>
  ${beatTicks.join("")}
  ${kicks.join("")}
  <line x1="${dropX.toFixed(1)}" y1="40" x2="${dropX.toFixed(1)}" y2="${H2 - 26}" stroke="#E60012" stroke-width="2"/>
  <text x="${(dropX + 8).toFixed(1)}" y="${H2 - 12}" font-size="12" fill="#ff6b74">90.00s · Drop 2（成片 24.375s）</text>
  ${bars.join("")}
</svg>`;

const html = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>Favorite Drug · 卡点图</title>
<style>
  body{margin:0;background:#07070a;color:#e8e8ef;font-family:"Microsoft YaHei",system-ui,sans-serif}
  .wrap{max-width:1640px;margin:0 auto;padding:36px 40px 60px}
  h1{font-size:26px;margin:0 0 6px}
  h2{font-size:16px;font-weight:600;color:#ff6b74;margin:34px 0 10px;letter-spacing:.06em}
  p{color:#9a9aa6;font-size:13px;line-height:1.8;margin:6px 0}
  .card{background:#0b0b10;border:1px solid #1c1c24;border-radius:10px;overflow:hidden}
  .kv{display:flex;gap:26px;flex-wrap:wrap;margin:14px 0 0}
  .kv div{font-size:13px;color:#b9b9c4}
  .kv b{color:#fff;font-weight:700}
  table{border-collapse:collapse;font-size:12.5px;margin-top:10px}
  td,th{border-bottom:1px solid #1c1c24;padding:7px 14px;text-align:left}
  th{color:#8a8a96;font-weight:600}
  code{color:#ff8a90;background:#1a1114;padding:1px 6px;border-radius:4px}
</style></head>
<body><div class="wrap">
  <h1>Favorite Drug · 卡点分析</h1>
  <p>片段：128 BPM / 4/4，每拍 0.46875s，每小节 1.875s。成片 30fps 下一拍 <b>14.0625 帧</b>、一小节 <b>56.25 帧</b>，所有切点都落在小节线上。</p>
  <div class="kv">
    <div>时长 <b>153.76s</b></div><div>BPM <b>128.0</b></div>
    <div>第一段 Drop <b>29.95s</b></div><div>第二段 Drop <b>90.00s</b></div>
    <div>选用窗口 <b>65.625–121.875s</b></div>
  </div>

  <h2>① 整曲能量与段落</h2>
  <div class="card">${svg1}</div>
  <p>选第二段而非第一段：第二段前有 1.9s 的「鼓点全撤」静场（88.1–90.0s），正好当宣传片 drop 前的留白；第一段前面是 30s 的慢热，剪不进 56s 的片子。</p>

  <h2>② 片段对位：小节 / 鼓点 / 分镜</h2>
  <div class="card">${svg2}</div>
  <p>建置段（0–24.4s）每 2 小节换一屏，最后 2 屏压到 1 小节加速；<code>24.375s</code> 落在 Drop 2 的落点上，律动空间整屏吃这一下；drop 之后每 1–3 小节一屏收尾。</p>

  <h2>③ 分镜表</h2>
  <table>
    <tr><th>成片时间</th><th>歌内时间</th><th>小节</th><th>画面</th></tr>
    ${cuts.map((c) => `<tr>
      <td>${(c.from / 30).toFixed(2)}s</td>
      <td>${(S + c.from / 30).toFixed(2)}s</td>
      <td>${Math.round((S + c.from / 30) / BAR)}</td>
      <td>${c.name}</td></tr>`).join("")}
  </table>
</div></body></html>`;

fs.writeFileSync("卡点图.html", html);
console.log("写入 卡点图.html", (html.length / 1024).toFixed(0), "KB");
