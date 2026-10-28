// 从整首歌里切出宣传片用的 56.25s 片段，做首尾淡变，输出 public/song.wav
// （Remotion 自带的 ffmpeg 是精简版，没有 afade/fps 滤镜，所以这段在 Node 里做）
const fs = require("fs");

const SRC = "audio/play.wav";
const OUT = "宣传视频/public/song.wav".replace("宣传视频/", "");
const START = 65.625;
const DUR = 56.25;
const FADE_IN = 0.20;
const FADE_OUT = 1.55;

const buf = fs.readFileSync(SRC);
let p = 12, ch = 2, sr = 48000, bits = 16, dataOff = 0, dataLen = 0;
while (p < buf.length - 8) {
  const id = buf.toString("ascii", p, p + 4);
  const sz = buf.readUInt32LE(p + 4);
  if (id === "fmt ") { ch = buf.readUInt16LE(p + 10); sr = buf.readUInt32LE(p + 12); bits = buf.readUInt16LE(p + 22); }
  if (id === "data") { dataOff = p + 8; dataLen = sz; break; }
  p += 8 + sz + (sz & 1);
}
const bytesPerSample = bits / 8;
const frameBytes = bytesPerSample * ch;
const totalFrames = Math.floor(dataLen / frameBytes);
const startFrame = Math.round(START * sr);
const nFrames = Math.min(Math.round(DUR * sr), totalFrames - startFrame);

const out = Buffer.alloc(44 + nFrames * frameBytes);
out.write("RIFF", 0, "ascii");
out.writeUInt32LE(36 + nFrames * frameBytes, 4);
out.write("WAVE", 8, "ascii");
out.write("fmt ", 12, "ascii");
out.writeUInt32LE(16, 16);
out.writeUInt16LE(1, 20);
out.writeUInt16LE(ch, 22);
out.writeUInt32LE(sr, 24);
out.writeUInt32LE(sr * frameBytes, 28);
out.writeUInt16LE(frameBytes, 32);
out.writeUInt16LE(bits, 34);
out.write("data", 36, "ascii");
out.writeUInt32LE(nFrames * frameBytes, 40);

const fi = Math.round(FADE_IN * sr), fo = Math.round(FADE_OUT * sr);
for (let i = 0; i < nFrames; i++) {
  let g = 1;
  if (i < fi) g = i / fi;
  const tail = nFrames - i;
  if (tail < fo) g = Math.min(g, tail / fo);
  // 轻微等功率曲线，淡出更自然
  g = Math.pow(g, g < 1 ? 1.6 : 1);
  for (let c = 0; c < ch; c++) {
    const s = buf.readInt16LE(dataOff + (startFrame + i) * frameBytes + c * 2);
    out.writeInt16LE(Math.max(-32768, Math.min(32767, Math.round(s * g))), 44 + i * frameBytes + c * 2);
  }
}
fs.writeFileSync("public/song.wav", out);
console.log("写入 public/song.wav", (out.length / 1024 / 1024).toFixed(1), "MB", "时长", (nFrames / sr).toFixed(2), "s");
