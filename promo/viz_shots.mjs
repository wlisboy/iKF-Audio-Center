/* 律动空间截图：真音频（外部已在播）+ 真回环，按时间点连续取帧
 * 用法：node viz_shots.mjs   （需先在后台播放 audio/drop_clip.wav）
 */
import { writeFileSync, mkdirSync } from "fs";

const CDP_PORT = 9333;
const BASE = "http://127.0.0.1:18500";
const OUT = process.env.SHOT_DIR || "public/shots";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
mkdirSync(OUT, { recursive: true });

let seq = 0;
const pending = new Map();
function connect(url) {
  return new Promise((res, rej) => {
    const ws = new WebSocket(url);
    ws.addEventListener("open", () => res(ws));
    ws.addEventListener("error", () => rej(new Error("CDP 连接失败")));
    ws.addEventListener("close", () => { console.log("!! CDP 连接已关闭"); process.exit(3); });
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
const ev = async (ws, expr) =>
  (await send(ws, "Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true })).result.value;

async function peak() {
  try {
    const r = await (await fetch(BASE + "/api/spectrum")).json();
    const raw = Buffer.from(r.bins || "", "base64");
    return raw.length ? Math.max(...raw) : 0;
  } catch (e) { return -1; }
}

async function main() {
  const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
  const target = list.find((t) => t.type === "page" && t.webSocketDebuggerUrl);
  if (!target) throw new Error("no page");
  const ws = await connect(target.webSocketDebuggerUrl);
  await send(ws, "Runtime.enable");
  await send(ws, "Page.enable");
  await send(ws, "Emulation.setDeviceMetricsOverride",
    { width: 1920, height: 1200, deviceScaleFactor: 1, mobile: false });
  await send(ws, "Page.navigate", { url: BASE + "/" });
  for (let i = 0; i < 90; i++) {
    if (await ev(ws, "document.documentElement.dataset.bridgeReady === '1'")) break;
    await sleep(400);
  }
  await ev(ws, `document.querySelector('.thm button[data-th="dark"]').click()`);
  await ev(ws, `document.querySelector('.dock button[data-s="vis"]').click()`);
  await sleep(1800);
  await ev(ws, `document.getElementById('vizPlay').click()`);
  await sleep(1500);
  console.log("回环起，静音峰值", await peak());

  // 等外部音频进入（最多 12s），看到峰值 > 60 认为音乐在响
  let p = 0;
  for (let i = 0; i < 60; i++) {
    p = await peak();
    if (p > 60) break;
    await sleep(200);
  }
  console.log("音频已进入，峰值", p);

  const shots = [
    ["viz-hi-1", 0.0],
    ["viz-hi-2", 0.35],
    ["viz-hi-3", 0.75],
    ["viz-hi-4", 1.6],
    ["viz-hi-5", 2.8],
    ["viz-hi-6", 4.4],
  ];
  for (const [name, delay] of shots) {
    if (delay) await sleep(delay * 1000);
    const pk = await peak();
    const r = await send(ws, "Page.captureScreenshot", { format: "png" });
    writeFileSync(OUT + "/" + name + ".png", Buffer.from(r.data, "base64"));
    console.log("  ", name, "峰值", pk);
  }

  // 浅色主题再来一组
  await ev(ws, `document.querySelector('.thm button[data-th="light"]').click()`);
  for (let i = 0; i < 3; i++) {
    await sleep(900);
    const pk = await peak();
    const r = await send(ws, "Page.captureScreenshot", { format: "png" });
    writeFileSync(OUT + "/viz-light-" + (i + 1) + ".png", Buffer.from(r.data, "base64"));
    console.log("   viz-light-" + (i + 1), "峰值", pk);
  }
  console.log("完成");
  process.exit(0);
}
main().catch((e) => { console.error("失败", e); process.exit(1); });
