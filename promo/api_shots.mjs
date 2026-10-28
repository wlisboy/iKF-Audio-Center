/* 参数页（开发者模式）深/浅色截图：直接置 localStorage('ikf.dev') 后重载 */
import { writeFileSync } from "fs";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const BASE = "http://127.0.0.1:18500";
let seq = 0;
const pending = new Map();
const connect = (url) => new Promise((res, rej) => {
  const ws = new WebSocket(url);
  ws.addEventListener("open", () => res(ws));
  ws.addEventListener("error", () => rej(new Error("CDP 失败")));
  ws.addEventListener("message", (e) => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) {
      const p = pending.get(m.id); pending.delete(m.id);
      m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
    }
  });
});
const send = (ws, method, params) => {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });
};
async function ev(ws, expr) {
  const r = await Promise.race([
    send(ws, "Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true }),
    sleep(8000).then(() => { throw new Error("evaluate 超时: " + expr.slice(0, 60)); }),
  ]);
  return r.result.value;
}

async function main() {
  const list = await (await fetch("http://127.0.0.1:9333/json/list")).json();
  const t = list.find((x) => x.type === "page");
  const ws = await connect(t.webSocketDebuggerUrl);
  await send(ws, "Runtime.enable");
  await send(ws, "Page.enable");
  await send(ws, "Emulation.setDeviceMetricsOverride", { width: 1920, height: 1200, deviceScaleFactor: 2, mobile: false });
  await send(ws, "Page.navigate", { url: BASE + "/" });
  await sleep(2500);
  console.log("置 devmode", await ev(ws, "localStorage.setItem('ikf.dev','1'), document.documentElement.classList.add('devmode'), 1"));
  await ev(ws, "document.querySelector('#dock button[data-s=\"api\"]').title='参数（开发者模式）'");
  await send(ws, "Page.reload");
  for (let i = 0; i < 90; i++) {
    if (await ev(ws, "document.documentElement.dataset.bridgeReady === '1'")) break;
    await sleep(400);
  }
  console.log("ready, devmode =", await ev(ws, "document.documentElement.classList.contains('devmode')"));
  await ev(ws, `document.querySelector('.thm button[data-th="dark"]').click()`);
  await ev(ws, `document.querySelector('.dock button[data-s="api"]').click()`);
  await sleep(2200);
  let r = await send(ws, "Page.captureScreenshot", { format: "png" });
  writeFileSync("public/shots/api-dark.png", Buffer.from(r.data, "base64"));
  console.log("api-dark ok");
  await ev(ws, `document.querySelector('.thm button[data-th="light"]').click()`);
  await sleep(2200);
  r = await send(ws, "Page.captureScreenshot", { format: "png" });
  writeFileSync("public/shots/api-light.png", Buffer.from(r.data, "base64"));
  console.log("api-light ok");
  process.exit(0);
}
main().catch((e) => { console.error("失败:", e.message); process.exit(1); });
