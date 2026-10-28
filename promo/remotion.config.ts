import { Config } from "@remotion/cli/config";

// 复用本机已安装的 Chrome，避免联网下载 headless shell
Config.setBrowserExecutable(
  "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
);
Config.setVideoImageFormat("jpeg");
Config.setOverwriteOutput(true);
Config.setConcurrency(4);
Config.setChromiumOpenGlRenderer("angle");
