import React from "react";
import { AbsoluteFill, Easing, Img, interpolate, staticFile, useCurrentFrame } from "remotion";
import { kick } from "../beats";
import { EASE, EASE_OUT, prog, RED_HOT } from "../parts";

const Kicker: React.FC<{ index: string; label: string; p: number }> = ({ index, label, p }) => (
  <div
    style={{
      display: "flex",
      alignItems: "center",
      gap: 14,
      fontSize: 18,
      letterSpacing: "0.34em",
      color: "rgba(255,255,255,0.42)",
      opacity: p,
      transform: `translateY(${(1 - p) * 12}px)`,
    }}
  >
    <span style={{ color: RED_HOT, fontWeight: 800, letterSpacing: "0.1em" }}>{index}</span>
    <span>{label}</span>
  </div>
);

/** 左文右图 / 左图右文 */
export const Feature: React.FC<{
  start: number;
  index: string;
  label: string;
  title: React.ReactNode;
  desc: string;
  chips: string[];
  src: string;
  side: "left" | "right";
  shotWidth?: number;
}> = ({ start, index, label, title, desc, chips, src, side, shotWidth = 1180 }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);

  const kickerP = prog(f, 0, 14, EASE_OUT);
  const titleP = prog(f, 3, 20, EASE);
  const descP = prog(f, 9, 20);
  const chipP = prog(f, 14, 18);
  const shotP = prog(f, 0, 16, EASE);

  const shotH = (shotWidth * 1200) / 1920;

  const text = (
    <div
      style={{
        position: "absolute",
        top: 244,
        width: side === "right" ? 560 : 600,
        left: side === "right" ? 112 : 1258,
      }}
    >
      <Kicker index={index} label={label} p={kickerP} />
      <div
        style={{
          marginTop: 26,
          fontSize: 66,
          lineHeight: 1.22,
          fontWeight: 900,
          color: "#FAFAFC",
          letterSpacing: "0.01em",
          opacity: titleP,
          transform: `translateY(${(1 - titleP) * 24}px)`,
        }}
      >
        {title}
      </div>
      <div
        style={{
          marginTop: 26,
          fontSize: 23,
          lineHeight: 1.78,
          color: "rgba(255,255,255,0.6)",
          opacity: descP,
          transform: `translateY(${(1 - descP) * 16}px)`,
        }}
      >
        {desc}
      </div>
      <div
        style={{
          marginTop: 30,
          display: "flex",
          flexWrap: "wrap",
          gap: 12,
          opacity: chipP,
        }}
      >
        {chips.map((c) => (
          <div
            key={c}
            style={{
              padding: "9px 17px",
              borderRadius: 999,
              border: "1px solid rgba(255,255,255,0.14)",
              background: "rgba(255,255,255,0.04)",
              fontSize: 19,
              color: "rgba(255,255,255,0.72)",
            }}
          >
            {c}
          </div>
        ))}
      </div>
    </div>
  );

  return (
    <AbsoluteFill>
      <AbsoluteFill
        style={{
          background:
            side === "right"
              ? `radial-gradient(900px 640px at 76% 46%, rgba(230,0,18,${0.12 + k * 0.12}), rgba(230,0,18,0) 72%)`
              : `radial-gradient(900px 640px at 24% 46%, rgba(230,0,18,${0.12 + k * 0.12}), rgba(230,0,18,0) 72%)`,
        }}
      />

      {text}

      <div
        style={{
          position: "absolute",
          top: 540 - shotH / 2,
          width: shotWidth,
          [side === "right" ? "right" : "left"]: 24,
          transform: `translateY(${(1 - shotP) * 46}px) scale(${
            interpolate(shotP, [0, 1], [0.965, 1]) + k * 0.002
          })`,
          opacity: shotP,
          borderRadius: 14,
          overflow: "hidden",
          border: "1px solid rgba(255,255,255,0.12)",
          background: "#08080b",
          boxShadow:
            "0 60px 140px -40px rgba(0,0,0,0.95), 0 0 120px -50px rgba(230,0,18,0.75)",
        }}
      >
        <Img
          src={staticFile(`shots/${src}`)}
          style={{ width: "100%", display: "block" }}
        />
      </div>
    </AbsoluteFill>
  );
};

/** 整屏截图 + 左下角一行说明（一小节内读完） */
export const FullBleed: React.FC<{
  start: number;
  src: string;
  index: string;
  label: string;
  note: string;
  zoom?: number;
}> = ({ start, src, index, label, note, zoom = 0.06 }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const k = kick(g);
  const p = prog(f, 0, 16, EASE);
  const drift = interpolate(f, [0, 56], [0, -46], Easing.linear);
  const noteP = prog(f, 8, 20, EASE_OUT);

  return (
    <AbsoluteFill style={{ overflow: "hidden", backgroundColor: "#07070A" }}>
      <div
        style={{
          position: "absolute",
          left: 96,
          top: 0,
          width: 1728,
          height: 1080,
          transform: `scale(${1 + (f / 56) * (zoom + 0.02)})`,
          opacity: p,
        }}
      >
        <Img
          src={staticFile(`shots/${src}`)}
          style={{ width: "100%", height: "100%", objectFit: "cover", display: "block" }}
        />
      </div>

      {/* 左下角压暗，文字才看得清 */}
      <AbsoluteFill
        style={{
          background:
            "linear-gradient(90deg, rgba(4,4,8,0.95) 0%, rgba(4,4,8,0.7) 30%, rgba(4,4,8,0) 60%)",
        }}
      />
      <AbsoluteFill
        style={{
          background:
            "linear-gradient(0deg, rgba(4,4,8,0.92) 0%, rgba(4,4,8,0.34) 13%, rgba(4,4,8,0) 32%)",
        }}
      />

      <div style={{ position: "absolute", left: 92, bottom: 78, width: 700 }}>
        <Kicker index={index} label={label} p={noteP} />
        <div
          style={{
            marginTop: 18,
            fontSize: 42,
            fontWeight: 900,
            color: "#FAFAFC",
            opacity: noteP,
            transform: `translateY(${(1 - noteP) * 16}px)`,
            textShadow: "0 4px 30px rgba(0,0,0,0.9)",
          }}
        >
          {note}
        </div>
        <div
          style={{
            marginTop: 18,
            width: 260,
            height: 3,
            background: RED_HOT,
            boxShadow: `0 0 ${18 + k * 26}px rgba(255,42,51,${0.5 + k * 0.4})`,
          }}
        />
      </div>
    </AbsoluteFill>
  );
};
