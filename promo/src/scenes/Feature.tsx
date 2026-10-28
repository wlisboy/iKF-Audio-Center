import React from "react";
import { AbsoluteFill, interpolate, useCurrentFrame } from "remotion";
import { Chip, CLAMP, EASE, prog, RED_HOT, rise, Shot } from "../parts";

export type FeatureVariant = "right" | "left" | "bottom";

export const FeatureScene: React.FC<{
  dur: number;
  index: string;
  label: string;
  title: React.ReactNode;
  desc: string;
  chips: string[];
  src: string;
  shotWidth: number;
  /** 截图原始宽高比（宽/高） */
  aspect?: number;
  variant?: FeatureVariant;
  kenBurns?: number;
}> = ({
  dur,
  index,
  label,
  title,
  desc,
  chips,
  src,
  shotWidth,
  aspect = 1.6,
  variant = "right",
  kenBurns = 1.05,
}) => {
  const frame = useCurrentFrame();

  const enter = prog(frame, 0, 30, EASE);
  const zoom = interpolate(frame, [0, dur], [1, kenBurns], CLAMP);

  const shotH = shotWidth / aspect;

  const cardT =
    variant === "left"
      ? {
          rotateY: interpolate(enter, [0, 1], [14, 3]),
          rotateX: interpolate(enter, [0, 1], [6, 1]),
          tx: interpolate(enter, [0, 1], [-140, 0]),
          ty: 0,
        }
      : variant === "bottom"
        ? {
            rotateY: 0,
            rotateX: interpolate(enter, [0, 1], [12, 3]),
            tx: 0,
            ty: interpolate(enter, [0, 1], [90, 0]),
          }
        : {
            rotateY: interpolate(enter, [0, 1], [-14, -3]),
            rotateX: interpolate(enter, [0, 1], [7, 1]),
            tx: interpolate(enter, [0, 1], [150, 0]),
            ty: 0,
          };

  const cardTop =
    variant === "bottom" ? 500 : Math.round((1080 - shotH) / 2) + 14;

  const cardStyle: React.CSSProperties = {
    position: "absolute",
    top: cardTop,
    ...(variant === "right"
      ? { right: -26 }
      : variant === "left"
        ? { left: 96 }
        : { left: "50%", marginLeft: -shotWidth / 2 }),
    transform: `perspective(2400px) translate(${cardT.tx}px, ${cardT.ty}px) rotateY(${cardT.rotateY}deg) rotateX(${cardT.rotateX}deg) scale(${zoom})`,
    transformOrigin: variant === "left" ? "left center" : "center center",
    opacity: enter,
  };

  const textBlock: React.CSSProperties =
    variant === "bottom"
      ? {
          position: "absolute",
          left: 120,
          top: 92,
        }
      : {
          position: "absolute",
          top: 0,
          bottom: 0,
          width: 600,
          display: "flex",
          flexDirection: "column",
          justifyContent: "center",
          ...(variant === "left" ? { right: 82, left: "auto" } : { left: 118 }),
        };

  const alignRight = variant === "left";

  return (
    <AbsoluteFill>
      <div style={cardStyle}>
        <Shot
          src={src}
          width={shotWidth}
          glow={0.5}
          imgScale={1.01}
          style={{
            boxShadow:
              "0 60px 150px -40px rgba(0,0,0,0.95), 0 0 0 1px rgba(255,255,255,0.06) inset, 0 0 150px -35px rgba(230,0,18,0.55)",
          }}
        />
      </div>

      <div style={{ ...textBlock, alignItems: alignRight ? "flex-end" : "flex-start" }}>
        <div style={rise(frame, 4, { dur: 22 })}>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              gap: 16,
              fontSize: 18,
              letterSpacing: "0.36em",
              color: "rgba(255,255,255,0.42)",
              fontWeight: 500,
              flexDirection: alignRight ? "row-reverse" : "row",
            }}
          >
            <span
              style={{
                color: RED_HOT,
                fontWeight: 800,
                letterSpacing: "0.1em",
                fontSize: 20,
              }}
            >
              {index}
            </span>
            <span>{label}</span>
          </div>
        </div>

        <div
          style={{
            marginTop: 26,
            fontSize: variant === "bottom" ? 58 : 68,
            lineHeight: 1.16,
            fontWeight: 900,
            color: "#F7F7F9",
            letterSpacing: "-0.02em",
            textAlign: alignRight ? "right" : "left",
            ...rise(frame, 10, { dur: 30, dist: 34 }),
          }}
        >
          {title}
        </div>

        <div
          style={{
            marginTop: 24,
            width: 92,
            height: 3,
            borderRadius: 2,
            backgroundColor: RED_HOT,
            boxShadow: "0 0 24px rgba(255,42,51,0.9)",
            opacity: prog(frame, 22, 26),
            transform: `scaleX(${prog(frame, 22, 26)})`,
            transformOrigin: alignRight ? "right center" : "left center",
          }}
        />

        <div
          style={{
            marginTop: 26,
            fontSize: 23,
            lineHeight: 1.72,
            color: "rgba(255,255,255,0.56)",
            fontWeight: 400,
            maxWidth: 570,
            textAlign: alignRight ? "right" : "left",
            ...rise(frame, 26, { dur: 30, dist: 22 }),
          }}
        >
          {desc}
        </div>

        <div
          style={{
            marginTop: 34,
            display: "flex",
            gap: 12,
            flexWrap: "wrap",
            justifyContent: alignRight ? "flex-end" : "flex-start",
          }}
        >
          {chips.map((c, i) => (
            <div key={c} style={rise(frame, 38 + i * 7, { dur: 26, dist: 16 })}>
              <Chip>{c}</Chip>
            </div>
          ))}
        </div>
      </div>
    </AbsoluteFill>
  );
};
