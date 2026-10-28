/* 录一段律动空间的真实画面（CDP screencast），用于 drop 段
 * 用法：node rec_viz.mjs        → 输出 public/vizrec/f0001.jpg … + meta.json
 */
import { writeFileSync, mkdirSync, rmSync } from "fs";
import { spawn } from "child_process";

const BASE = "http://127.0.0.1:18500";
const OUT = "public/vizrec";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });

let seq = 0;
const pending = new Map();
let ws = null;
const frames = [];

const connect = (url) => new Promise((res, rej) => {
  ws = new WebSocket(url);
  ws.addEventListener("open", () => res(ws));
  ws.addEventListener("error", () => rej(new Error("CDP 失败")));
  ws.addEventListener("message", (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) {
      const p = pending.get(m.id); pending.delete(m.id);
      m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
      return;
    }
    if (m.method === "Page.screencastFrame") {
      const n = frames.length + 1;
      const name = "f" + String(n).padStart(4, "0") + ".jpg";
      writeFileSync(OUT + "/" + name, Buffer.from(m.params.data, "base64"));
      frames.push({ n, file: name, ts: m.params.metadata.timestamp });
      // ack 必须及时，否则 Chrome 不再出帧
      send("Page.screencastFrameAck", { sessionId: m.params.sessionId }).catch(() => {});
    }
  });
});
const send = (method, params) => {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });
};
async function ev(expr) {
  const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
  return r.result.value;
}

async function main() {
  const list = await (await fetch("http://127.0.0.1:9333/json/list")).json();
  const t = list.find((x) => x.type === "page");
  await connect(t.webSocketDebuggerUrl);
  await send("Runtime.enable");
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 1920, height: 1200, deviceScaleFactor: 1, mobile: false });
  await send("Page.navigate", { url: BASE + "/" });
  for (let i = 0; i < 90; i++) {
    if (await ev("document.documentElement.dataset.bridgeReady === '1'")) break;
    await sleep(400);
  }
  await ev(`document.querySelector('.thm button[data-th="dark"]').click()`);
  await ev(`document.querySelector('.dock button[data-s="vis"]').click()`);
  await sleep(1800);
  await ev(`document.getElementById('vizPlay').click()`);
  // 等回环稳定
  for (let i = 0; i < 40; i++) {
    const r = await (await fetch(BASE + "/api/loopback/status")).json();
    if (r.running) break;
    await sleep(300);
  }
  console.log("回环就绪，开始录屏");
  await send("Page.startScreencast", {
    format: "jpeg", quality: 92, maxWidth: 1760, maxHeight: 1100, everyNthFrame: 1,
  });
  await sleep(600);
  const py = "C:/Program Files/Python313/python.exe";
  const clip = "C:/Users/wlisboy/Desktop/iKF headset-latest/宣传视频/audio/drop_clip.wav";
  const child = spawn(py, ["-c", `import winsound;winsound.PlaySound(r"${clip}",winsound.SND_FILENAME)`], { stdio: "ignore" });
  const t0 = Date.now();
  await sleep(15000);
  await send("Page.stopScreencast");
  child.kill();
  writeFileSync(OUT + "/meta.json", JSON.stringify({
    frames, t0, count: frames.length,
    span: frames.length ? frames[frames.length - 1].ts - frames[0].ts : 0,
  }, null, 1));
  console.log("帧数", frames.length, "跨度", (frames[frames.length - 1].ts - frames[0].ts).toFixed(2), "s");
  console.log("实际录制", ((Date.now() - t0) / 1000).toFixed(1), "s");
  process.exit(0);
}
main().catch((e) => { console.error("失败:", e.message); process.exit(1); });
