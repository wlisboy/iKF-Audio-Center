import React from "react";
import {
  AbsoluteFill,
  Easing,
  Img,
  interpolate,
  staticFile,
  useCurrentFrame,
} from "remotion";

export const EASE = Easing.bezier(0.16, 1, 0.3, 1);
export const EASE_OUT = Easing.bezier(0.22, 0.61, 0.36, 1);

export const RED = "#E60012";
export const RED_HOT = "#FF2A33";

export const CLAMP = {
  extrapolateLeft: "clamp" as const,
  extrapolateRight: "clamp" as const,
};

/** 0→1 progress with custom easing */
export const prog = (
  frame: number,
  start: number,
  duration: number,
  easing: (t: number) => number = EASE,
) =>
  interpolate(frame, [start, start + duration], [0, 1], {
    ...CLAMP,
    easing,
  });

/** fade + rise entrance, returns style */
export const rise = (
  frame: number,
  start: number,
  opts?: { dur?: number; dist?: number; easing?: (t: number) => number },
): React.CSSProperties => {
  const dur = opts?.dur ?? 24;
  const dist = opts?.dist ?? 28;
  const p = prog(frame, start, dur, opts?.easing ?? EASE);
  return {
    opacity: p,
    transform: `translateY(${(1 - p) * dist}px)`,
  };
};

export const clampVal = (v: number, lo = 0, hi = 1) =>
  Math.min(hi, Math.max(lo, v));

/* ------------------------------------------------------------------ */
/* 背景                                                                */
/* ------------------------------------------------------------------ */

export const Backdrop: React.FC<{ warm?: number }> = ({ warm = 1 }) => {
  const frame = useCurrentFrame();
  const t = frame / 30;
  const driftX = Math.sin(t / 7.3) * 5;
  const driftY = Math.cos(t / 9.1) * 3;
  const pulse = 0.9 + Math.sin(t / 3.4) * 0.1;

  return (
    <AbsoluteFill style={{ backgroundColor: "#07070A", overflow: "hidden" }}>
      {/* 顶部品牌红光 */}
      <AbsoluteFill
        style={{
          background: `radial-gradient(1100px 700px at ${50 + driftX}% ${
            6 + driftY
          }%, rgba(230,0,18,${0.15 * warm * pulse}), rgba(230,0,18,0) 66%)`,
        }}
      />
      {/* 左下冷光，制造层次 */}
      <AbsoluteFill
        style={{
          background: `radial-gradient(900px 620px at ${16 - driftX}% ${
            96 - driftY
          }%, rgba(255,42,51,${0.065 * warm}), rgba(255,42,51,0) 70%)`,
        }}
      />
      {/* 网格 */}
      <AbsoluteFill
        style={{
          backgroundImage:
            "linear-gradient(rgba(255,255,255,0.035) 1px, transparent 1px), linear-gradient(90deg, rgba(255,255,255,0.035) 1px, transparent 1px)",
          backgroundSize: "88px 88px",
          backgroundPosition: `${driftX * 4}px ${driftY * 4}px`,
          maskImage:
            "radial-gradient(ellipse 78% 70% at 50% 46%, rgba(0,0,0,1), rgba(0,0,0,0) 76%)",
          WebkitMaskImage:
            "radial-gradient(ellipse 78% 70% at 50% 46%, rgba(0,0,0,1), rgba(0,0,0,0) 76%)",
        }}
      />
      {/* 颗粒 */}
      <AbsoluteFill
        style={{
          opacity: 0.05,
          backgroundImage:
            "url(\"data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='160' height='160'><filter id='n'><feTurbulence type='fractalNoise' baseFrequency='0.85' numOctaves='3'/></filter><rect width='160' height='160' filter='url(%23n)' opacity='0.6'/></svg>\")",
          backgroundSize: "160px 160px",
        }}
      />
      {/* 暗角 */}
      <AbsoluteFill
        style={{
          background:
            "radial-gradient(ellipse 100% 90% at 50% 50%, rgba(0,0,0,0) 48%, rgba(0,0,0,0.48) 100%)",
        }}
      />
    </AbsoluteFill>
  );
};

/* ------------------------------------------------------------------ */
/* 场景外壳：交叉溶解                                                  */
/* ------------------------------------------------------------------ */

export const SceneShell: React.FC<{
  dur: number;
  children: React.ReactNode;
  fadeIn?: number;
  fadeOut?: number;
}> = ({ dur, children, fadeIn = 8, fadeOut = 6 }) => {
  const frame = useCurrentFrame();
  const inP = fadeIn === 0 ? 1 : prog(frame, 0, fadeIn, EASE_OUT);
  const outP =
    fadeOut === 0
      ? 1
      : interpolate(frame, [dur - fadeOut, dur - 1], [1, 0], {
          ...CLAMP,
          easing: Easing.out(Easing.cubic),
        });
  const scaleIn = fadeIn === 0 ? 1 : interpolate(inP, [0, 1], [1.028, 1]);
  const scaleOut = fadeOut === 0 ? 1 : interpolate(outP, [0, 1], [0.972, 1]);
  return (
    <AbsoluteFill
      style={{
        opacity: Math.min(inP, outP),
        transform: `scale(${scaleIn * scaleOut})`,
      }}
    >
      {children}
    </AbsoluteFill>
  );
};

/* ------------------------------------------------------------------ */
/* 界面截图卡片                                                        */
/* ------------------------------------------------------------------ */

export const Shot: React.FC<{
  src: string;
  width: number;
  style?: React.CSSProperties;
  radius?: number;
  glow?: number;
  imgScale?: number;
  imgShiftX?: number;
  imgShiftY?: number;
}> = ({
  src,
  width,
  style,
  radius = 12,
  glow = 0.5,
  imgScale = 1,
  imgShiftX = 0,
  imgShiftY = 0,
}) => {
  return (
    <div
      style={{
        width,
        borderRadius: radius,
        overflow: "hidden",
        border: "1px solid rgba(255,255,255,0.13)",
        backgroundColor: "#0b0b0f",
        boxShadow: `0 50px 130px -30px rgba(0,0,0,0.95), 0 0 0 1px rgba(255,255,255,0.04) inset, 0 0 120px -40px rgba(230,0,18,${glow})`,
        ...style,
      }}
    >
      <Img
        src={staticFile(src)}
        style={{
          width: "100%",
          display: "block",
          filter: "brightness(1.05) saturate(1.06)",
          transform: `scale(${imgScale}) translate(${imgShiftX}px, ${imgShiftY}px)`,
        }}
      />
    </div>
  );
};

/* ------------------------------------------------------------------ */
/* 排版                                                                */
/* ------------------------------------------------------------------ */

export const Kicker: React.FC<{
  index?: string;
  children: React.ReactNode;
  style?: React.CSSProperties;
}> = ({ index, children, style }) => {
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 14,
        fontSize: 19,
        letterSpacing: "0.34em",
        color: "rgba(255,255,255,0.45)",
        fontWeight: 500,
        ...style,
      }}
    >
      {index ? (
        <span style={{ color: RED_HOT, fontWeight: 800, letterSpacing: "0.12em" }}>
          {index}
        </span>
      ) : null}
      <span>{children}</span>
    </div>
  );
};

export const Rule: React.FC<{ p: number; width?: number; color?: string }> = ({
  p,
  width = 96,
  color = RED_HOT,
}) => (
  <div
    style={{
      width: width * clampVal(p),
      height: 3,
      backgroundColor: color,
      borderRadius: 2,
      boxShadow: `0 0 22px ${color}`,
    }}
  />
);

export const Chip: React.FC<{
  children: React.ReactNode;
  style?: React.CSSProperties;
}> = ({ children, style }) => (
  <div
    style={{
      padding: "9px 18px",
      borderRadius: 999,
      border: "1px solid rgba(255,255,255,0.16)",
      backgroundColor: "rgba(255,255,255,0.05)",
      color: "rgba(255,255,255,0.82)",
      fontSize: 21,
      fontWeight: 500,
      letterSpacing: "0.02em",
      whiteSpace: "nowrap",
      ...style,
    }}
  >
    {children}
  </div>
);
