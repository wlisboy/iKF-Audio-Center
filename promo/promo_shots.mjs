/* 用真实运行的产品页面出宣传片素材截图（零依赖 CDP）
 * 用法：node promo_shots.mjs basic   ← 各页深/浅色截图（无需音频）
 *       node promo_shots.mjs viz     ← 律动空间：真放音频 + 真回环，截 drop 段
 */
import { writeFileSync, mkdirSync } from "fs";
import { spawn } from "child_process";

const CDP_PORT = Number(process.env.CDP_PORT || 9333);
const BASE = "http://127.0.0.1:" + (process.env.IKF_PORT || 18500);
const OUT = process.env.SHOT_DIR || "public/shots";
const MODE = process.argv[2] || "basic";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

mkdirSync(OUT, { recursive: true });

let seq = 0;
const pending = new Map();
function connect(url) {
  return new Promise((res, rej) => {
    const ws = new WebSocket(url);
    ws.addEventListener("open", () => res(ws));
    ws.addEventListener("error", () => rej(new Error("CDP 连接失败")));
    ws.addEventListener("message", (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pending.has(m.id)) {
        const p = pending.get(m.id);
        pending.delete(m.id);
        m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
      }
    });
  });
}
function send(ws, method, params) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });
}
async function ev(ws, expr) {
  const r = await send(ws, "Runtime.evaluate", {
    expression: expr, returnByValue: true, awaitPromise: true,
  });
  return r.result.value;
}
async function shot(ws, name) {
  const r = await send(ws, "Page.captureScreenshot", { format: "png", captureBeyondViewport: false });
  writeFileSync(OUT + "/" + name + ".png", Buffer.from(r.data, "base64"));
  console.log("  截图", name);
}

let target = null;
for (let i = 0; i < 80; i++) {
  try {
    const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
    target = list.find((t) => t.type === "page" && t.webSocketDebuggerUrl);
    if (target) break;
  } catch (e) { /* 等 Chrome 起来 */ }
  await sleep(500);
}
if (!target) throw new Error("CDP 里找不到页面");
const ws = await connect(target.webSocketDebuggerUrl);
await send(ws, "Runtime.enable");
await send(ws, "Page.enable");
await send(ws, "Emulation.setDeviceMetricsOverride", {
  width: Number(process.env.CAP_W || 1920),
  height: Number(process.env.CAP_H || 1200),
  deviceScaleFactor: 2,
  mobile: false,
});
await send(ws, "Page.navigate", { url: BASE + "/" });
await sleep(1500);

// 等桥接就绪（驱动值首同步完成）
let ready = false;
for (let i = 0; i < 90; i++) {
  ready = await ev(ws, "document.documentElement.dataset.bridgeReady === '1'");
  if (ready) break;
  await sleep(500);
}
console.log("bridgeReady", ready);
if (!ready) throw new Error("界面没就绪");

const goto = async (s) => {
  await ev(ws, `document.querySelector('.dock button[data-s="${s}"]').click()`);
  await sleep(1400);
};
const theme = async (t) => {
  await ev(ws, `document.querySelector('.thm button[data-th="${t}"]').click()`);
  await sleep(1600);
};

if (MODE === "basic") {
  const pages = ["space", "eq", "mic", "profile", "dev"];
  for (const t of ["dark", "light"]) {
    await theme(t);
    for (const p of pages) {
      await goto(p);
      await shot(ws, `${p}-${t}`);
    }
  }
  // 参数页需要开发者模式（logo 连点 5 次 / Ctrl+Alt+P）
  await ev(ws, `document.dispatchEvent(new KeyboardEvent('keydown',
    {key:'P', ctrlKey:true, altKey:true, bubbles:true}))`);
  await sleep(900);
  const apiVisible = await ev(ws, `(function(){var b=document.querySelector('.dock button[data-s="api"]');
    return b ? getComputedStyle(b).display !== 'none' : false;})()`);
  console.log("参数页入口可见", apiVisible);
  if (apiVisible) {
    await theme("dark");
    await goto("api");
    await shot(ws, "api-dark");
    await theme("light");
    await sleep(600);
    await shot(ws, "api-light");
  }
}

if (MODE === "viz") {
  await theme("dark");
  await goto("vis");
  await ev(ws, `document.getElementById('vizPlay').click()`);
  // 等回环起来 + 频谱非零
  let mx = 0;
  for (let i = 0; i < 40; i++) {
    await sleep(400);
    try {
      const r = await (await fetch(BASE + "/api/spectrum")).json();
      const raw = Buffer.from(r.bins || "", "base64");
      mx = raw.length ? Math.max(...raw) : 0;
    } catch (e) { mx = -1; }
    if (mx > 40) break;
  }
  console.log("静音时频谱峰值", mx, "（回环已通）");

  // 真放音频（84.5s→126.5s，drop 在片段第 5.51s）
  const py = "C:/Program Files/Python313/python.exe";
  const clip = "C:/Users/wlisboy/Desktop/iKF headset-latest/宣传视频/audio/drop_clip.wav";
  const child = spawn(py, ["-c", `import winsound;winsound.PlaySound(r"${clip}",winsound.SND_FILENAME)`], {
    stdio: "ignore",
  });
  const t0 = Date.now();
  const at = async (sec, name) => {
    const wait = sec * 1000 - (Date.now() - t0);
    if (wait > 0) await sleep(wait);
    let peak = 0;
    try {
      const r = await (await fetch(BASE + "/api/spectrum")).json();
      const raw = Buffer.from(r.bins || "", "base64");
      peak = raw.length ? Math.max(...raw) : 0;
    } catch (e) { peak = -1; }
    await shot(ws, name);
    console.log("    片段 " + sec.toFixed(2) + "s 频谱峰值", peak);
  };
  await at(5.6, "viz-drop-a");
  await at(6.3, "viz-drop-b");
  await at(7.2, "viz-drop-c");
  await at(9.0, "viz-drop-d");
  await at(11.5, "viz-drop-e");
  await at(15.0, "viz-drop-f");

  // 浅色主题 + 换配色再来一张
  await theme("light");
  await at(18.3, "viz-light");
  await ev(ws, `document.querySelector('.thm button[data-th="dark"]').click()`);
  await sleep(1200);
  await ev(ws, `document.getElementById('vizPlay').click()`); // 停
  child.kill();
}
console.log("完成");
