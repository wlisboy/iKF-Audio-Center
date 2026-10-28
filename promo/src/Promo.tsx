import React, { useEffect, useState } from "react";
import {
  AbsoluteFill,
  continueRender,
  delayRender,
  interpolate,
  staticFile,
  Sequence,
  useCurrentFrame,
} from "remotion";
import { Audio } from "@remotion/media";
import { Backdrop, CLAMP, SceneShell } from "./parts";
import { BuildUp, Manifesto, Opener } from "./scenes/Opener";
import { Feature, FullBleed } from "./scenes/Build";
import { DeviceScene, ThemeScene, VizDrop } from "./scenes/Drop";
import { EndCard, Outro } from "./scenes/End";

export const FPS = 30;

/* 时间轴取自实测节拍：128 BPM，一小节 56.25 帧，片段起点 65.625s。
   0/113/225/338/450/563/619/675 都落在小节线上；731 = drop 落点（90.0s） */
export const TIMELINE = [
  { from: 0, dur: 113, name: "01 开场" },
  { from: 113, dur: 112, name: "02 宣言" },
  { from: 225, dur: 113, name: "03 声音" },
  { from: 338, dur: 112, name: "04 均衡器" },
  { from: 450, dur: 113, name: "05 麦克风" },
  { from: 563, dur: 56, name: "06 场景" },
  { from: 619, dur: 56, name: "07 参数" },
  { from: 675, dur: 56, name: "08 蓄势" },
  { from: 731, dur: 338, name: "09 律动空间" },
  { from: 1069, dur: 169, name: "10 设备" },
  { from: 1238, dur: 112, name: "11 双主题" },
  { from: 1350, dur: 169, name: "12 收尾" },
  { from: 1519, dur: 169, name: "13 落版" },
];

export const TOTAL_FRAMES = 1688;

const FontLoader: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [handle] = useState(() => delayRender("loading iKF Sans"));
  useEffect(() => {
    let finished = false;
    const done = () => {
      if (finished) return;
      finished = true;
      try {
        continueRender(handle);
      } catch {
        /* already */
      }
    };
    try {
      const face = new FontFace(
        "iKF Sans",
        `url(${staticFile("NotoSansSC-VF.ttf")})`,
        { weight: "100 900" },
      );
      face
        .load()
        .then((loaded) => {
          document.fonts.add(loaded);
          done();
        })
        .catch(() => done());
    } catch {
      done();
    }
  }, [handle]);
  return <>{children}</>;
};

/** 片尾压黑 */
const Tail: React.FC = () => {
  const f = useCurrentFrame();
  const p = interpolate(f, [TOTAL_FRAMES - 24, TOTAL_FRAMES - 1], [0, 1], CLAMP);
  if (p <= 0) return null;
  return <AbsoluteFill style={{ backgroundColor: `rgba(3,3,6,${p})` }} />;
};

export const Promo: React.FC = () => {
  const t = TIMELINE;

  return (
    <FontLoader>
      <AbsoluteFill className="ikf-root" style={{ backgroundColor: "#07070A" }}>
        <Audio src={staticFile("song.wav")} volume={0.92} />
        <Backdrop />

        <Sequence from={t[0].from} durationInFrames={t[0].dur} name={t[0].name}>
          <SceneShell dur={t[0].dur} fadeIn={0} fadeOut={4}>
            <Opener dur={t[0].dur} start={t[0].from} />
          </SceneShell>
        </Sequence>

        <Sequence from={t[1].from} durationInFrames={t[1].dur} name={t[1].name}>
          <SceneShell dur={t[1].dur} fadeIn={0} fadeOut={4}>
            <Manifesto dur={t[1].dur} start={t[1].from} />
          </SceneShell>
        </Sequence>

        <Sequence from={t[2].from} durationInFrames={t[2].dur} name={t[2].name}>
          <SceneShell dur={t[2].dur} fadeIn={0} fadeOut={4}>
            <Feature
              start={t[2].from}
              index="01"
              label="SOUND"
              title={
                <>
                  一根滑杆之外
                  <br />
                  还有很多
                </>
              }
              desc="音量、动态压低、语音增强三档并排；环境音效、智能音量、左右平衡，各管一摊。"
              chips={["三档联动", "环境音效", "左右平衡"]}
              src="space-dark.png"
              side="right"
            />
          </SceneShell>
        </Sequence>

        <Sequence from={t[3].from} durationInFrames={t[3].dur} name={t[3].name}>
          <SceneShell dur={t[3].dur} fadeIn={0} fadeOut={4}>
            <Feature
              start={t[3].from}
              index="02"
              label="EQUALIZER"
              title={
                <>
                  十段参数均衡
                  <br />
                  拖曲线就是拖声音
                </>
              }
              desc="30 Hz 到 16 kHz 十个频点，拖动即写回设备；预设一键换听感，曲线实时重画。"
              chips={["10 频点", "预设切换", "实时写回"]}
              src="eq-dark.png"
              side="left"
            />
          </SceneShell>
        </Sequence>

        <Sequence from={t[4].from} durationInFrames={t[4].dur} name={t[4].name}>
          <SceneShell dur={t[4].dur} fadeIn={0} fadeOut={4}>
            <Feature
              start={t[4].from}
              index="03"
              label="MICROPHONE"
              title={
                <>
                  输入这一侧
                  <br />
                  也看得见
                </>
              }
              desc="麦克风增益、环境降噪、回声消除、魔音变声，外加实时电平——设备给了什么、没给什么，一眼看清。"
              chips={["实时电平", "环境降噪", "魔音变声"]}
              src="mic-dark.png"
              side="right"
            />
          </SceneShell>
        </Sequence>

        <Sequence from={t[5].from} durationInFrames={t[5].dur} name={t[5].name}>
          <SceneShell dur={t[5].dur} fadeIn={0} fadeOut={3}>
            <FullBleed
              start={t[5].from}
              src="profile-dark.png"
              index="04"
              label="SCENES"
              note="8 套场景，点一下整体换声"
            />
          </SceneShell>
        </Sequence>

        <Sequence from={t[6].from} durationInFrames={t[6].dur} name={t[6].name}>
          <SceneShell dur={t[6].dur} fadeIn={0} fadeOut={3}>
            <FullBleed
              start={t[6].from}
              src="api-dark.png"
              index="05"
              label="PARAMETERS"
              note="195 项参数，7 个分组，改完即写回"
            />
          </SceneShell>
        </Sequence>

        <Sequence from={t[7].from} durationInFrames={t[7].dur} name={t[7].name}>
          <BuildUp dur={t[7].dur} start={t[7].from} />
        </Sequence>

        <Sequence from={t[8].from} durationInFrames={t[8].dur} name={t[8].name}>
          <VizDrop dur={t[8].dur} start={t[8].from} />
        </Sequence>

        <Sequence from={t[9].from} durationInFrames={t[9].dur} name={t[9].name}>
          <DeviceScene dur={t[9].dur} start={t[9].from} />
        </Sequence>

        <Sequence from={t[10].from} durationInFrames={t[10].dur} name={t[10].name}>
          <ThemeScene dur={t[10].dur} start={t[10].from} />
        </Sequence>

        <Sequence from={t[11].from} durationInFrames={t[11].dur} name={t[11].name}>
          <Outro dur={t[11].dur} start={t[11].from} />
        </Sequence>

        <Sequence from={t[12].from} durationInFrames={t[12].dur} name={t[12].name}>
          <EndCard dur={t[12].dur} start={t[12].from} />
        </Sequence>

        <Tail />
      </AbsoluteFill>
    </FontLoader>
  );
};
