import React from "react";
import {
  AbsoluteFill,
  Easing,
  Img,
  interpolate,
  staticFile,
  useCurrentFrame,
} from "remotion";
import { BAR, kick } from "../beats";
import { CLAMP, EASE, EASE_OUT, prog, RED_HOT } from "../parts";

/* ---------------- 07 收尾 ---------------- */
export const Outro: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);

  const prodP = prog(f, 0, 30, EASE);
  const logoP = prog(f, 14, 26);
  const titleP = prog(f, 22, 30, EASE);
  const subP = prog(f, 48, 26);

  const ring = (delay: number) => {
    const p = prog(f, delay, 46, EASE_OUT);
    const size = 420 + p * 900;
    return (
      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 392,
          width: size,
          height: size,
          marginLeft: -size / 2,
          marginTop: -size / 2,
          borderRadius: "50%",
          border: `1.5px solid rgba(230,0,18,${0.5 * (1 - p)})`,
          opacity: p > 0 ? 1 : 0,
          boxShadow: `0 0 60px rgba(230,0,18,${0.3 * (1 - p)})`,
        }}
      />
    );
  };

  return (
    <AbsoluteFill>
      <AbsoluteFill
        style={{
          background: `radial-gradient(760px 560px at 50% 36%, rgba(230,0,18,${0.14 + k * 0.14}), rgba(230,0,18,0) 70%)`,
        }}
      />
      {ring(6)}
      {ring(26)}

      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          left: "50%",
          top: 122,
          height: 420,
          transform: `translateX(-50%) scale(${interpolate(prodP, [0, 1], [1.06, 1])})`,
          opacity: prodP * 0.98,
          filter: "drop-shadow(0 30px 60px rgba(0,0,0,0.85))",
        }}
      />
      <Img
        src={staticFile("logo.png")}
        style={{
          position: "absolute",
          left: "50%",
          top: 592,
          width: 168,
          transform: "translateX(-50%)",
          opacity: logoP * 0.6,
          filter: "brightness(1.4)",
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 708,
          textAlign: "center",
          fontSize: 88,
          fontWeight: 900,
          color: "#FAFAFC",
          letterSpacing: "0.03em",
          opacity: titleP,
          transform: `translateY(${(1 - titleP) * 24}px)`,
          textShadow: `0 0 ${30 + k * 40}px rgba(255,42,51,${0.18 + k * 0.2})`,
        }}
      >
        控制，回到你手里
      </div>

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 842,
          display: "flex",
          justifyContent: "center",
          opacity: subP,
        }}
      >
        <div
          style={{
            width: 320,
            height: 2,
            background: `linear-gradient(90deg, transparent, ${RED_HOT}, transparent)`,
            boxShadow: "0 0 24px rgba(255,42,51,0.8)",
          }}
        />
      </div>
      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 878,
          textAlign: "center",
          fontSize: 24,
          letterSpacing: "0.24em",
          color: "rgba(255,255,255,0.6)",
          opacity: subP,
        }}
      >
        iKF AUDIO CENTER
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 08 落版 ---------------- */
export const EndCard: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);
  const bg = prog(f, 0, 30);
  const mark = prog(f, 4, 28, EASE);
  const line = prog(f, 22, 26, EASE_OUT);
  const foot = prog(f, 40, 26);

  return (
    <AbsoluteFill style={{ backgroundColor: "#05050A" }}>
      <Img
        src={staticFile("shots/viz-hi-4.png")}
        style={{
          position: "absolute",
          width: "100%",
          height: "100%",
          objectFit: "cover",
          opacity: bg * 0.16,
          filter: "blur(26px) saturate(0.9)",
        }}
      />
      <AbsoluteFill
        style={{
          background: `radial-gradient(900px 620px at 50% 46%, rgba(230,0,18,${0.1 + k * 0.1}), rgba(0,0,0,0.55) 70%)`,
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 396,
          textAlign: "center",
          opacity: mark,
          transform: `translateY(${(1 - mark) * 20}px)`,
        }}
      >
        <div
          style={{
            fontSize: 96,
            fontWeight: 900,
            letterSpacing: "0.06em",
            color: "#FAFAFC",
          }}
        >
          iKF Audio Center
        </div>
        <div
          style={{
            marginTop: 20,
            fontSize: 24,
            letterSpacing: "0.42em",
            color: "rgba(255,255,255,0.42)",
          }}
        >
          二〇二六 · 桌面声卡控制台
        </div>
      </div>

      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 606,
          width: 620 * line,
          height: 3,
          marginLeft: -(520 * line) / 2,
          background: `linear-gradient(90deg, rgba(255,42,51,0), ${RED_HOT}, rgba(255,42,51,0))`,
          boxShadow: "0 0 26px rgba(255,42,51,0.8)",
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 662,
          textAlign: "center",
          fontSize: 22,
          letterSpacing: "0.16em",
          color: "rgba(255,255,255,0.5)",
          opacity: foot,
        }}
      >
        Windows 10 / 11　·　便携版，无需单独安装运行组件　·　跟随设备 8 声道
      </div>
      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 736,
          textAlign: "center",
          fontSize: 16,
          letterSpacing: "0.2em",
          color: "rgba(255,255,255,0.26)",
          opacity: foot,
        }}
      >
        by wlisboy
      </div>
    </AbsoluteFill>
  );
};
