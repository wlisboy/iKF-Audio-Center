import React from "react";
import { AbsoluteFill, Img, interpolate, staticFile, useCurrentFrame } from "remotion";
import { BEAT, kick } from "../beats";
import { clampVal, EASE, EASE_OUT, prog, RED_HOT } from "../parts";

/* ---------------- 01 开场 ---------------- */
export const Opener: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const p = prog(f, 0, 30, EASE);
  const titleIn = prog(f, 10, 26, EASE);
  const subIn = prog(f, 32, 26);
  const k = kick(g);

  return (
    <AbsoluteFill>
      {/* 产品背光 */}
      <AbsoluteFill
        style={{
          background: `radial-gradient(760px 520px at 50% 40%, rgba(230,0,18,${
            0.16 + k * 0.16
          }), rgba(230,0,18,0) 70%)`,
        }}
      />
      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          left: "50%",
          top: 70,
          height: 512,
          transform: `translateX(-50%) scale(${interpolate(p, [0, 1], [1.1, 1]) +
            k * 0.008})`,
          opacity: p,
          filter: "drop-shadow(0 40px 80px rgba(0,0,0,0.85))",
        }}
      />

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 648,
          textAlign: "center",
          fontSize: 100,
          fontWeight: 900,
          letterSpacing: "0.02em",
          color: "#FAFAFC",
          opacity: titleIn,
          transform: `translateY(${(1 - titleIn) * 30}px)`,
        }}
      >
        声音的控制台
      </div>

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 782,
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          gap: 22,
          opacity: subIn,
          transform: `translateY(${(1 - subIn) * 16}px)`,
        }}
      >
        {["8 声道", "10 段参数均衡", "195 项参数"].map((s, i) => (
          <React.Fragment key={s}>
            {i > 0 ? (
              <span style={{ color: "rgba(255,255,255,0.22)", fontSize: 22 }}>/</span>
            ) : null}
            <span style={{ fontSize: 26, color: "rgba(255,255,255,0.72)", letterSpacing: "0.06em" }}>
              {s}
            </span>
          </React.Fragment>
        ))}
      </div>

      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 868,
          textAlign: "center",
          fontSize: 17,
          letterSpacing: "0.5em",
          color: "rgba(255,255,255,0.34)",
          opacity: prog(f, 44, 24),
        }}
      >
        iKF AUDIO CENTER
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 02 宣言 ---------------- */
export const Manifesto: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const g = start + f;
  const l1 = prog(f, 0, 20);
  const l2 = prog(f, BEAT, 26, EASE);
  const l3 = prog(f, BEAT * 2, 24, EASE);
  const chips = prog(f, BEAT * 3.2, 22);
  const k = kick(g);

  return (
    <AbsoluteFill>
      <AbsoluteFill
        style={{
          background: `radial-gradient(900px 620px at 24% 52%, rgba(230,0,18,${
            0.1 + k * 0.1
          }), rgba(230,0,18,0) 72%)`,
        }}
      />
      {/* 左侧竖向红线，跟着拍子亮 */}
      <div
        style={{
          position: "absolute",
          left: 168,
          top: 306,
          width: 4,
          height: 372,
          borderRadius: 2,
          background: `linear-gradient(180deg, ${RED_HOT}, rgba(230,0,18,0.05))`,
          opacity: 0.25 + k * 0.55,
          transform: `scaleY(${clampVal(l1)})`,
          transformOrigin: "top",
          boxShadow: `0 0 30px rgba(255,42,51,${0.35 + k * 0.4})`,
        }}
      />

      <Img
        src={staticFile("product.webp")}
        style={{
          position: "absolute",
          right: -140,
          top: 60,
          height: 940,
          opacity: 0.085 * l1,
          filter: "blur(1.5px) saturate(0.7) brightness(1.1)",
          transform: "rotate(-4deg)",
        }}
      />
      <div style={{ position: "absolute", left: 226, top: 348 }}>
        <div
          style={{
            fontSize: 34,
            color: "rgba(255,255,255,0.5)",
            letterSpacing: "0.04em",
            opacity: l1,
            transform: `translateY(${(1 - l1) * 18}px)`,
          }}
        >
          系统设置里，只有一根音量条。
        </div>

        <div
          style={{
            marginTop: 34,
            fontSize: 92,
            lineHeight: 1.24,
            fontWeight: 900,
            color: "#FAFAFC",
            letterSpacing: "0.01em",
          }}
        >
          <div
            style={{
              opacity: l2,
              transform: `translateY(${(1 - l2) * 26}px)`,
            }}
          >
            这里，把整块声卡
          </div>
          <div
            style={{
              opacity: l3,
              transform: `translateY(${(1 - l3) * 26}px)`,
            }}
          >
            <span
              style={{
                color: RED_HOT,
                textShadow: `0 0 ${26 + k * 40}px rgba(255,42,51,${0.35 + k * 0.3})`,
              }}
            >
              摊开
            </span>
            给你看。
          </div>
        </div>

        <div
          style={{
            marginTop: 54,
            display: "flex",
            gap: 16,
            opacity: chips,
            transform: `translateY(${(1 - chips) * 18}px)`,
          }}
        >
          {["每一个声道", "每一个频点", "每一个参数"].map((s) => (
            <div
              key={s}
              style={{
                padding: "10px 20px",
                borderRadius: 999,
                border: "1px solid rgba(255,255,255,0.16)",
                background: "rgba(255,255,255,0.045)",
                fontSize: 21,
                color: "rgba(255,255,255,0.78)",
              }}
            >
              {s}
            </div>
          ))}
        </div>
      </div>
    </AbsoluteFill>
  );
};

/* ---------------- 03 蓄势（drop 前一小节的静场） ---------------- */
export const BuildUp: React.FC<{ dur: number; start: number }> = ({ start }) => {
  const f = useCurrentFrame();
  const line = prog(f, 0, 26, EASE_OUT);
  const text = prog(f, 8, 30);
  const glow = prog(f, 24, 32, EASE_OUT);
  return (
    <AbsoluteFill style={{ backgroundColor: "#05050A" }}>
      <AbsoluteFill
        style={{
          background: `radial-gradient(620px 420px at 50% 52%, rgba(230,0,18,${0.16 * glow}), rgba(230,0,18,0) 70%)`,
        }}
      />
      <div
        style={{
          position: "absolute",
          left: 0,
          right: 0,
          top: 470,
          textAlign: "center",
          fontSize: 40,
          letterSpacing: "0.2em",
          color: "rgba(255,255,255,0.85)",
          opacity: text,
        }}
      >
        现在，让它响起来
      </div>
      <div
        style={{
          position: "absolute",
          left: "50%",
          top: 552,
          width: 420 * line,
          height: 2,
          marginLeft: -(420 * line) / 2,
          background: `linear-gradient(90deg, rgba(255,42,51,0), ${RED_HOT}, rgba(255,42,51,0))`,
          boxShadow: "0 0 30px rgba(255,42,51,0.9)",
        }}
      />
    </AbsoluteFill>
  );
};
