import React from "react";
import { Composition } from "remotion";
import { FPS, Promo, TOTAL_FRAMES } from "./Promo";

export const RemotionRoot: React.FC = () => {
  return (
    <>
      <Composition
        id="IKF-Promo"
        component={Promo}
        durationInFrames={TOTAL_FRAMES}
        fps={FPS}
        width={1920}
        height={1080}
      />
    </>
  );
};
