import React from "react";
import { AbsoluteFill, interpolate, staticFile, Img, useCurrentFrame } from "remotion";
import { CLAMP, EASE, EASE_OUT, RED, RED_HOT, Chip, prog } from "../parts";

export const OpenScene: React.FC<{ dur: number }> = ({ dur }) => {
  const frame = useCurrentFrame();

  const productIn = prog(frame, 4, 48, EASE);
  const ringIn = prog(frame, 12, 60, EASE_OUT);
  const spin = frame * 0.16;
  const breathe = 1 + Math.sin(frame / 26) * 0.006;

  const l1 = prog(frame, 46, 26);
  const l2 = prog(frame, 58, 32);
  const l3 = prog(frame, 78, 30);
  const lineW = prog(frame, 88, 34);

  return (
    <AbsoluteFill>
      {/* 产品背光 */}
      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 420,
          width: 1100,
          height: 700,
          marginLeft: -550,
          marginTop: -350,
          background:
            "radial-gradient(ellipse 50% 50% at 50% 50%, rgba(230,0,18,0.28), rgba(230,0,18,0) 70%)",
          opacity: productIn,
          filter: "blur(8px)",
        }}
      />

      {/* 装饰圆环 */}
      <svg
        width={1000}
        height={1000}
        viewBox="0 0 1000 1000"
        style={{
          position: "absolute",
          left: "50%",
          top: 420,
          marginLeft: -500,
          marginTop: -500,
          opacity: ringIn * 0.5,
          transform: `rotate(${spin}deg)`,
        }}
      >
        <circle
          cx={500}
          cy={500}
          r={392}
          fill="none"
          stroke={RED}
          strokeWidth={1.2}
          strokeDasharray="2 10"
          opacity={0.55}
        />
        <circle
          cx={500}
          cy={500}
          r={330}
          fill="none"
          stroke="rgba(255,255,255,0.14)"
          strokeWidth={1}
          strokeDasharray="1 6"
        />
      </svg>

      {/* 产品 */}
      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          left: "50%",
          top: 398,
          height: 720,
          transform: `translate(-50%, -50%) translateY(${
            (1 - productIn) * 46
          }px) scale(${interpolate(productIn, [0, 1], [1.09, 1]) * breathe})`,
          opacity: productIn,
          filter: "drop-shadow(0 34px 70px rgba(0,0,0,0.85))",
        }}
      />

      {/* 文案 */}
      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          bottom: 96,
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
        }}
      >
        <div
          style={{
            fontSize: 17,
            letterSpacing: "0.62em",
            color: "rgba(255,255,255,0.42)",
            fontWeight: 500,
            opacity: l1,
            transform: `translateY(${(1 - l1) * 14}px)`,
            marginLeft: "0.62em",
          }}
        >
          iKF · AUDIO CENTER
        </div>

        <div
          style={{
            marginTop: 22,
            fontSize: 86,
            fontWeight: 900,
            color: "#FAFAFC",
            letterSpacing: "0.02em",
            lineHeight: 1,
            opacity: l2,
            transform: `translateY(${(1 - l2) * 26}px)`,
          }}
        >
          声音的控制台
        </div>

        <div
          style={{
            marginTop: 26,
            width: 320 * lineW,
            height: 3,
            borderRadius: 2,
            background: `linear-gradient(90deg, transparent, ${RED_HOT}, transparent)`,
            boxShadow: "0 0 26px rgba(255,42,51,0.85)",
            opacity: lineW,
          }}
        />

        <div
          style={{
            marginTop: 24,
            fontSize: 22,
            letterSpacing: "0.2em",
            color: "rgba(255,255,255,0.5)",
            fontWeight: 400,
            opacity: l3,
            marginLeft: "0.2em",
          }}
        >
          为 iKF-V11 重新做的控制面板
        </div>
      </div>
    </AbsoluteFill>
  );
};

/* ------------------------------------------------------------------ */

const Hi: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <span style={{ color: RED_HOT, fontWeight: 900 }}>{children}</span>
);

export const ManifestoScene: React.FC<{ dur: number }> = ({ dur }) => {
  const frame = useCurrentFrame();

  const bgIn = prog(frame, 0, 40);
  const bgOut = interpolate(frame, [dur - 40, dur], [1, 0.55], CLAMP);
  const l1 = prog(frame, 0, 26);
  const l2 = prog(frame, 18, 30);
  const l3 = prog(frame, 44, 28);
  const chips = prog(frame, 62, 28);

  return (
    <AbsoluteFill>
      {/* 背景产品虚影 */}
      <div
        style={{
          position: "absolute",
          right: -260,
          top: 20,
          width: 1300,
          height: 1100,
          background:
            "radial-gradient(closest-side, rgba(230,0,18,0.22), rgba(230,0,18,0) 72%)",
          opacity: bgIn * bgOut,
        }}
      />
      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          right: -180,
          top: -60,
          height: 1150,
          opacity: bgIn * bgOut * 0.26,
          filter: "blur(30px) saturate(0.7)",
          transform: "rotate(-6deg)",
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 168,
          top: 0,
          bottom: 0,
          display: "flex",
          flexDirection: "column",
          justifyContent: "center",
        }}
      >
        <div
          style={{
            fontSize: 66,
            fontWeight: 700,
            color: "rgba(255,255,255,0.32)",
            letterSpacing: "-0.01em",
            lineHeight: 1.3,
            opacity: l1,
            transform: `translateY(${(1 - l1) * 26}px)`,
          }}
        >
          官方驱动，只给了你一个音量条
        </div>

        <div
          style={{
            marginTop: 18,
            fontSize: 66,
            fontWeight: 900,
            color: "#FAFAFC",
            letterSpacing: "-0.01em",
            lineHeight: 1.3,
            opacity: l2,
            transform: `translateY(${(1 - l2) * 30}px)`,
          }}
        >
          这里，<Hi>8</Hi> 声道、<Hi>10</Hi> 段均衡、
          <br />
          <Hi>200+</Hi> 项参数全部摊开
        </div>

        <div
          style={{
            marginTop: 40,
            height: 3,
            width: 640 * l3,
            background: `linear-gradient(90deg, ${RED_HOT}, rgba(255,42,51,0))`,
            boxShadow: "0 0 24px rgba(255,42,51,0.7)",
            opacity: l3,
          }}
        />

        <div
          style={{
            marginTop: 34,
            display: "flex",
            gap: 14,
            opacity: chips,
            transform: `translateY(${(1 - chips) * 18}px)`,
          }}
        >
          <Chip>拖动即写入设备</Chip>
          <Chip>深浅双主题</Chip>
          <Chip>便携免安装</Chip>
        </div>
      </div>
    </AbsoluteFill>
  );
};
