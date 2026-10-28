/* 统计录屏每帧平均亮度，找 drop 落点；并输出帧时间轴 */
import { spawn } from "child_process";
import { readFileSync, writeFileSync } from "fs";

const dir = "public/vizrec";
const meta = JSON.parse(readFileSync(dir + "/meta.json", "utf8"));
const W = 48, H = 32;

const ff = spawn("C:/Users/wlisboy/Desktop/iKF headset-latest/宣传视频/node_modules/@remotion/compositor-win32-x64-msvc/ffmpeg.exe",
  ["-y", "-i", dir + "/f%04d.jpg", "-vf", `scale=${W}:${H},format=gray`, "-f", "rawvideo", "-pix_fmt", "gray", "-"],
  { stdio: ["ignore", "pipe", "ignore"] });

const chunks = [];
ff.stdout.on("data", (d) => chunks.push(d));
ff.on("close", () => {
  const buf = Buffer.concat(chunks);
  const per = W * H;
  const n = Math.floor(buf.length / per);
  const bright = [];
  for (let i = 0; i < n; i++) {
    let s = 0;
    for (let j = 0; j < per; j++) s += buf[i * per + j];
    bright.push(+(s / per).toFixed(2));
  }
  const out = meta.frames.slice(0, n).map((f, i) => ({
    n: f.n, ts: +(f.ts - meta.frames[0].ts).toFixed(3), b: bright[i],
  }));
  // 亮度差分，找最大跳跃（drop）
  let best = { i: 0, d: -1e9 };
  for (let i = 3; i < out.length; i++) {
    const d = out[i].b - out[i - 3].b;
    if (d > best.d) best = { i, d: +d.toFixed(2) };
  }
  writeFileSync(dir + "/bright.json", JSON.stringify({ frames: out, dropIndex: best.i }, null, 1));
  console.log("帧数", out.length, "跨度", out[out.length - 1].ts, "s");
  console.log("亮度最大跳跃 帧", best.i, "diff", best.d, "at", out[best.i].ts, "s");
  console.log("前 40 帧亮度:", out.slice(0, 40).map((o) => o.b).join(" "));
  const win = out.slice(Math.max(0, best.i - 6), best.i + 8);
  console.log("drop 附近:", win.map((o) => `${o.ts}s:${o.b}`).join("  "));
});
