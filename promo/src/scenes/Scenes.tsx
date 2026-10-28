import React from "react";
import { AbsoluteFill, Img, interpolate, staticFile, useCurrentFrame } from "remotion";
import { CLAMP, EASE, EASE_OUT, RED_HOT, prog, rise, Shot } from "../parts";

/* ---------------- 律动空间 ---------------- */

export const SpaceScene: React.FC<{ dur: number }> = ({ dur }) => {
  const frame = useCurrentFrame();

  const enter = prog(frame, 0, 32, EASE);
  const zoom = interpolate(frame, [0, dur], [1, 1.07], CLAMP);
  const glow = 0.35 + Math.sin(frame / 9) * 0.12;

  return (
    <AbsoluteFill>
      {/* 顶部文案 */}
      <div style={{ position: "absolute", left: 120, top: 62 }}>
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: 16,
            fontSize: 18,
            letterSpacing: "0.36em",
            color: "rgba(255,255,255,0.42)",
            fontWeight: 500,
            ...rise(frame, 0, { dur: 22, dist: 14 }),
          }}
        >
          <span style={{ color: RED_HOT, fontWeight: 800, fontSize: 20, letterSpacing: "0.1em" }}>
            04
          </span>
          <span>MOTION SPACE</span>
        </div>
        <div
          style={{
            marginTop: 20,
            fontSize: 62,
            fontWeight: 900,
            color: "#FAFAFC",
            letterSpacing: "-0.01em",
            lineHeight: 1.15,
            ...rise(frame, 8, { dur: 30, dist: 26 }),
          }}
        >
          让声音，变成看得见的东西
        </div>
      </div>

      <div
        style={{
          position: "absolute",
          right: 120,
          top: 118,
          textAlign: "right",
          fontSize: 20,
          lineHeight: 1.7,
          color: "rgba(255,255,255,0.5)",
          maxWidth: 460,
          ...rise(frame, 20, { dur: 30, dist: 20 }),
        }}
      >
        3D 频谱地形随音乐起伏
        <br />
        配色、振幅、速度、视觉增益全部可调
      </div>

      {/* 卡片 */}
      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 200,
          marginLeft: -755,
          transform: `translateY(${(1 - enter) * 60}px) scale(${
            interpolate(enter, [0, 1], [0.955, 1]) * zoom
          })`,
          opacity: enter,
        }}
      >
        <div style={{ position: "relative" }}>
          <Shot src="space.png" width={1510} glow={glow} radius={14} />
          {/* 节拍脉冲：让静默截图也有呼吸感 */}
          <div
            style={{
              position: "absolute",
              inset: 0,
              borderRadius: 14,
              mixBlendMode: "screen",
              background: `radial-gradient(${620 + Math.sin(frame / 11) * 90}px ${
                400 + Math.sin(frame / 11) * 60
              }px at ${38 + Math.sin(frame / 23) * 6}% 62%, rgba(255,42,51,${
                0.16 + Math.sin(frame / 9) * 0.05
              }), rgba(255,42,51,0) 68%)`,
            }}
          />
        </div>
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 双主题 ---------------- */

export const ThemeScene: React.FC<{ dur: number }> = ({ dur }) => {
  const frame = useCurrentFrame();

  const l = prog(frame, 0, 46, EASE);
  const r = prog(frame, 8, 46, EASE);
  const split = prog(frame, 18, 40, EASE_OUT);

  const cardStyle = (p: number, dir: number): React.CSSProperties => ({
    position: "absolute",
    top: 300,
    width: 820,
    transform: `perspective(2400px) translateX(${(1 - p) * 120 * dir}px) rotateY(${
      (1 - p) * 12 * dir
    }deg) rotate(${dir * 2.2}deg) scale(${interpolate(p, [0, 1], [0.94, 1])})`,
    opacity: p,
    boxShadow: "0 60px 140px -40px rgba(0,0,0,0.9)",
  });

  return (
    <AbsoluteFill>
      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 96,
          textAlign: "center",
        }}
      >
        <div
          style={{
            fontSize: 18,
            letterSpacing: "0.36em",
            color: "rgba(255,255,255,0.42)",
            fontWeight: 500,
            ...rise(frame, 0, { dur: 22, dist: 12 }),
          }}
        >
          <span style={{ color: RED_HOT, fontWeight: 800, fontSize: 20 }}>06</span>
          <span style={{ marginLeft: 16 }}>THEMES</span>
        </div>
        <div
          style={{
            marginTop: 22,
            fontSize: 64,
            fontWeight: 900,
            color: "#FAFAFC",
            letterSpacing: "-0.01em",
            ...rise(frame, 6, { dur: 30, dist: 24 }),
          }}
        >
          深色、浅色，都打磨过一遍
        </div>
      </div>

      <div style={{ ...cardStyle(l, -1), left: 128 }}>
        <Shot src="sound-light.png" width={820} glow={0.18} />
        <div
          style={{
            position: "absolute",
            left: 0,
            right: 0,
            bottom: -46,
            textAlign: "center",
            fontSize: 19,
            letterSpacing: "0.3em",
            color: "rgba(255,255,255,0.45)",
          }}
        >
          LIGHT
        </div>
      </div>

      <div style={{ ...cardStyle(r, 1), right: 128 }}>
        <Shot src="sound-dark.png" width={820} glow={0.5} />
        <div
          style={{
            position: "absolute",
            left: 0,
            right: 0,
            bottom: -46,
            textAlign: "center",
            fontSize: 19,
            letterSpacing: "0.3em",
            color: "rgba(255,255,255,0.45)",
          }}
        >
          DARK
        </div>
      </div>

      {/* 中央分割红线 */}
      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 250,
          width: 2,
          height: 640 * split,
          marginLeft: -1,
          background: `linear-gradient(180deg, transparent, ${RED_HOT}, transparent)`,
          boxShadow: "0 0 30px rgba(255,42,51,0.9)",
          opacity: split * 0.9,
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          bottom: 66,
          textAlign: "center",
          fontSize: 22,
          letterSpacing: "0.05em",
          color: "rgba(255,255,255,0.5)",
          ...rise(frame, 56, { dur: 30, dist: 16 }),
        }}
      >
        同一份界面 · 两套配色 · 跟随系统一键切换
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 收尾 ---------------- */

export const OutroScene: React.FC<{ dur: number }> = ({ dur }) => {
  const frame = useCurrentFrame();

  const productIn = prog(frame, 0, 44, EASE);
  const titleIn = prog(frame, 24, 34);
  const subIn = prog(frame, 44, 30);
  const footIn = prog(frame, 62, 30);

  const ringAt = (start: number) => interpolate(frame, [start, start + 86], [0, 1], CLAMP);

  const ringStyle = (p: number): React.CSSProperties => {
    const size = 280 + p * 820;
    return {
      position: "absolute",
      left: "50%",
      top: 372,
      width: size,
      height: size,
      marginLeft: -size / 2,
      marginTop: -size / 2,
      borderRadius: "50%",
      border: `1.5px solid rgba(230,0,18,${0.5 * (1 - p)})`,
      boxShadow: `0 0 70px rgba(230,0,18,${0.3 * (1 - p)})`,
      opacity: p > 0.001 && p < 0.999 ? 1 : 0,
    };
  };

  return (
    <AbsoluteFill>
      <div style={ringStyle(ringAt(8))} />
      <div style={ringStyle(ringAt(34))} />
      <div style={ringStyle(ringAt(112))} />
      <div style={ringStyle(ringAt(140))} />

      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          left: "50%",
          top: 262,
          height: 380,
          transform: `translate(-50%, -50%) scale(${interpolate(productIn, [0, 1], [1.06, 1])})`,
          opacity: productIn * 0.96,
          filter: "drop-shadow(0 24px 50px rgba(0,0,0,0.8))",
        }}
      />

      <Img
        src={staticFile("logo.png")}
        style={{
          position: "absolute",
          left: "50%",
          top: 442,
          width: 132,
          transform: "translateX(-50%)",
          opacity: productIn * 0.45,
          filter: "brightness(1.4)",
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 552,
          textAlign: "center",
          fontSize: 92,
          fontWeight: 900,
          color: "#FAFAFC",
          letterSpacing: "0.03em",
          lineHeight: 1.1,
          opacity: titleIn,
          transform: `translateY(${(1 - titleIn) * 26}px)`,
        }}
      >
        控制，回到你手里
      </div>

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 700,
          display: "flex",
          justifyContent: "center",
          opacity: subIn,
        }}
      >
        <div
          style={{
            width: 300 + subIn * 60,
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
          top: 746,
          textAlign: "center",
          fontSize: 26,
          letterSpacing: "0.22em",
          color: "rgba(255,255,255,0.62)",
          opacity: subIn,
        }}
      >
        iKF AUDIO CENTER
      </div>

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 948,
          textAlign: "center",
          fontSize: 19,
          letterSpacing: "0.16em",
          color: "rgba(255,255,255,0.32)",
          opacity: footIn,
        }}
      >
        Windows · 便携版 · 无需单独安装运行组件　|　by wlisboy
      </div>
    </AbsoluteFill>
  );
};
