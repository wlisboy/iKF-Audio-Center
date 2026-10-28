import React from "react";
import {
  AbsoluteFill,
  Easing,
  Img,
  interpolate,
  OffthreadVideo,
  staticFile,
  useCurrentFrame,
} from "remotion";
import { kick } from "../beats";
import { CLAMP, EASE, EASE_OUT, prog, RED_HOT } from "../parts";

/* ---------------- 04 律动空间（drop 主体，真回环录屏） ---------------- */
export const VizDrop: React.FC<{ dur: number; start: number }> = ({ dur, start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);

  const flash = interpolate(f, [0, 3], [0.72, 0], CLAMP);
  /* drop 瞬间先「砸」一下：1.07 → 1.0，再缓慢推进 */
  const push =
    f < 12
      ? interpolate(f, [0, 12], [1.07, 1.0], Easing.out(Easing.cubic))
      : interpolate(f, [12, dur], [1.0, 1.05], Easing.linear);

  const beats = [
    { at: 56, text: "48 kHz / 8 声道，回环直采", sub: "REAL LOOPBACK" },
    { at: 168, text: "你听到的，变成看得见的", sub: "SPECTRUM TERRAIN" },
    { at: 281, text: "16 套配色，拖一下换个世界", sub: "16 PALETTES" },
  ];

  return (
    <AbsoluteFill style={{ backgroundColor: "#05050A", overflow: "hidden" }}>
      <div
        style={{
          position: "absolute",
          left: 0,
          top: 0,
          width: 1920,
          height: 1200,
          transform: `scale(${push})`,
          transformOrigin: "50% 12%",
        }}
      >
        <OffthreadVideo
          src={staticFile("vizdrop.mp4")}
          style={{ width: "100%", height: "100%", objectFit: "cover" }}
          muted
        />
      </div>

      {/* 底部压暗，压住读数区 */}
      <AbsoluteFill
        style={{
          background:
            "linear-gradient(0deg, rgba(4,4,8,0.96) 0%, rgba(4,4,8,0.72) 20%, rgba(4,4,8,0) 46%)",
        }}
      />

      {/* 三条字幕，按小节切换 */}
      {beats.map((b, i) => {
        const inP = prog(f, b.at, 18, EASE_OUT);
        const next = beats[i + 1];
        const outP = next
          ? interpolate(f, [next.at - 4, next.at + 4], [1, 0], CLAMP)
          : 1;
        if (f < b.at - 2) return null;
        return (
          <div
            key={b.sub}
            style={{
              position: "absolute",
              left: 96,
              bottom: 96,
              opacity: inP * outP,
              transform: `translateY(${(1 - inP) * 18}px)`,
            }}
          >
            <div
              style={{
                fontSize: 16,
                letterSpacing: "0.4em",
                color: "rgba(255,255,255,0.4)",
              }}
            >
              {b.sub}
            </div>
            <div
              style={{
                marginTop: 12,
                fontSize: 56,
                fontWeight: 900,
                color: "#FFFFFF",
                textShadow: `0 8px 40px rgba(0,0,0,0.95), 0 0 ${30 + k * 40}px rgba(255,42,51,0.35)`,
              }}
            >
              {b.text}
            </div>
          </div>
        );
      })}

      {/* drop 冲击白光 */}
      <AbsoluteFill
        style={{ backgroundColor: `rgba(255,138,148,${flash})`, mixBlendMode: "screen" }}
      />
    </AbsoluteFill>
  );
};

/* ---------------- 05 设备 ---------------- */
export const DeviceScene: React.FC<{ dur: number; start: number }> = ({ dur, start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);
  const p = prog(f, 0, 18, EASE);
  const drift = interpolate(f, [0, dur], [0, -54], Easing.linear);
  const noteP = prog(f, 10, 22, EASE_OUT);

  return (
    <AbsoluteFill style={{ overflow: "hidden", backgroundColor: "#07070A" }}>
      <div
        style={{
          position: "absolute",
          left: 96,
          top: 0,
          width: 1728,
          height: 1080,
          transform: `scale(${1 + (f / dur) * 0.03})`,
          opacity: p,
        }}
      >
        <Img
          src={staticFile("shots/dev-dark.png")}
          style={{ width: "100%", height: "100%", objectFit: "cover" }}
        />
      </div>
      <AbsoluteFill
        style={{
          background: "linear-gradient(0deg, rgba(4,4,8,0.8) 0%, rgba(4,4,8,0) 32%)",
        }}
      />
      <div style={{ position: "absolute", left: 96, bottom: 92, opacity: noteP }}>
        <div style={{ fontSize: 16, letterSpacing: "0.4em", color: RED_HOT, fontWeight: 700 }}>
          05 / DEVICE
        </div>
        <div
          style={{
            marginTop: 12,
            fontSize: 50,
            fontWeight: 900,
            color: "#FAFAFC",
            textShadow: "0 6px 40px rgba(0,0,0,0.9)",
          }}
        >
          7.1 虚拟环绕 · 8 声道 · 声道测试
        </div>
        <div
          style={{
            marginTop: 16,
            width: 300,
            height: 3,
            background: RED_HOT,
            boxShadow: `0 0 ${18 + k * 26}px rgba(255,42,51,${0.5 + k * 0.4})`,
          }}
        />
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 06 双主题 ---------------- */
export const ThemeScene: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);
  const l = prog(f, 0, 26, EASE);
  const r = prog(f, 7, 26, EASE);
  const divP = prog(f, 12, 26, EASE_OUT);
  const txt = prog(f, 26, 22, EASE_OUT);

  const cardW = 856;
  const cardH = (cardW * 1200) / 1920;

  const card = (src: string, p: number, dir: number) => (
    <div
      style={{
        position: "absolute",
        top: 262,
        [dir < 0 ? "left" : "right"]: 60,
        width: cardW,
        height: cardH,
        borderRadius: 12,
        overflow: "hidden",
        border: "1px solid rgba(255,255,255,0.12)",
        boxShadow: "0 50px 120px -40px rgba(0,0,0,0.95)",
        opacity: p,
        transform: `translateX(${(1 - p) * 40 * -dir}px) scale(${interpolate(p, [0, 1], [0.97, 1])})`,
      }}
    >
      <Img src={staticFile(`shots/${src}`)} style={{ width: "100%", display: "block" }} />
    </div>
  );

  return (
    <AbsoluteFill>
      <AbsoluteFill
        style={{
          background: `radial-gradient(700px 500px at 50% 40%, rgba(230,0,18,${0.1 + k * 0.1}), rgba(230,0,18,0) 70%)`,
        }}
      />
      {card("space-dark.png", l, -1)}
      {card("space-light.png", r, 1)}

      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 262,
          width: 2,
          height: cardH * divP,
          marginLeft: -1,
          background: `linear-gradient(180deg, rgba(255,42,51,0), ${RED_HOT}, rgba(255,42,51,0))`,
          boxShadow: "0 0 24px rgba(255,42,51,0.85)",
        }}
      />

      {[
        { t: "深色", x: 60, p: l },
        { t: "浅色", x: 1920 - 60 - cardW, p: r },
      ].map((c) => (
        <div
          key={c.t}
          style={{
            position: "absolute",
            left: c.x,
            top: 262 + cardH + 16,
            width: cardW,
            textAlign: "center",
            fontSize: 20,
            letterSpacing: "0.3em",
            color: "rgba(255,255,255,0.45)",
            opacity: c.p,
          }}
        >
          {c.t}
        </div>
      ))}

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 132,
          textAlign: "center",
          opacity: txt,
          transform: `translateY(${(1 - txt) * 16}px)`,
        }}
      >
        <div style={{ fontSize: 16, letterSpacing: "0.42em", color: RED_HOT, fontWeight: 700 }}>
          06 / THEME
        </div>
        <div style={{ marginTop: 12, fontSize: 44, fontWeight: 900, color: "#FAFAFC" }}>
          深色与浅色，同一套参数
        </div>
      </div>
    </AbsoluteFill>
  );
};
