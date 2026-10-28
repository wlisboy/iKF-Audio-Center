/* 录律动空间真实动画 + 同步记录频谱，用频谱低频跳变精确定位 drop 帧
 * 输出 public/vizrec/f0001.jpg…  meta2.json（含 dropFrame / dropTs / fps）
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
const frames = [];
const spec = [];
let ws = null;

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
      const name = "f" + String(frames.length + 1).padStart(4, "0") + ".jpg";
      writeFileSync(OUT + "/" + name, Buffer.from(m.params.data, "base64"));
      frames.push({ file: name, ts: m.params.metadata.timestamp });
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
  for (let i = 0; i < 40; i++) {
    const r = await (await fetch(BASE + "/api/loopback/status")).json();
    if (r.running) break;
    await sleep(300);
  }
  console.log("回环就绪，开始录");
  await send("Page.startScreencast", { format: "jpeg", quality: 92, maxWidth: 1760, maxHeight: 1100, everyNthFrame: 1 });
  await sleep(500);

  // 频谱采样（并行）
  let stop = false;
  const poll = (async () => {
    while (!stop) {
      try {
        const r = await (await fetch(BASE + "/api/spectrum")).json();
        const raw = Buffer.from(r.bins || "", "base64");
        let low = 0, all = 0;
        for (let i = 1; i < 24; i++) low += raw[i];          // 低频 bin
        for (let i = 0; i < raw.length; i++) all += raw[i];
        spec.push({ ts: mstime(), low, all });
      } catch (e) { /* 忽略 */ }
      await sleep(40);
    }
  })();
  function mstime() { return Date.now() / 1000; }

  const py = "C:/Program Files/Python313/python.exe";
  const clip = "C:/Users/wlisboy/Desktop/iKF headset-latest/宣传视频/audio/drop_clip.wav";
  const playStart = Date.now();
  const child = spawn(py, ["-c", `import winsound;winsound.PlaySound(r"${clip}",winsound.SND_FILENAME)`], { stdio: "ignore" });
  await sleep(22000);
  stop = true;
  await poll;
  await send("Page.stopScreencast");
  child.kill();

  // 找低频最大正跳变（drop 落点）
  let best = { i: 0, d: -1e9 };
  for (let i = 6; i < spec.length; i++) {
    const d = spec[i].low - spec[i - 6].low;
    if (d > best.d) best = { i, d };
  }
  const dropTs = spec[best.i].ts;
  // 该时刻最近的录屏帧
  let dropFrame = 0, bestDt = 1e9;
  frames.forEach((f, i) => {
    const dt = Math.abs(f.ts - dropTs);
    if (dt < bestDt) { bestDt = dt; dropFrame = i; }
  });
  const fps = (frames.length - 1) / (frames[frames.length - 1].ts - frames[0].ts);
  writeFileSync(OUT + "/meta2.json", JSON.stringify({
    count: frames.length, fps: +fps.toFixed(2), dropFrame, spec0Abs: spec[0].ts,
    dropTs, playStart: playStart / 1000, firstFrameTs: frames[0].ts,
    dropOffsetFromPlay: +(dropTs - playStart / 1000).toFixed(3),
    spec: spec.map((s) => ({ ...s, ts: +(s.ts - spec[0].ts).toFixed(3) })),
  }, null, 1));
  console.log("帧数", frames.length, "fps", fps.toFixed(1));
  console.log("drop 帧号", dropFrame, "距播放开始", (dropTs - playStart / 1000).toFixed(2), "s");
  console.log("低频跳变", best.d, "帧 ts 偏差", bestDt.toFixed(3));
  process.exit(0);
}
main().catch((e) => { console.error("失败:", e.message); process.exit(1); });
