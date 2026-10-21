# iKF Audio Center

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Windows%2010%20%2F%2011-lightgrey)](#)
[![Python](https://img.shields.io/badge/Python-3.13%2B-3776AB?logo=python&logoColor=white)](https://www.python.org)
[![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

iKF Audio Center 是一个面向 iKF 耳机（C-Media 方案）的 Windows 桌面音频控制中心，提供更完整的设备参数调校、场景恢复、频谱分析、监听与回环录音能力。

## 部分页面预览

<table>
  <tr>
    <td width="33%"><img src="design/screenshots/audio-controls.png" alt="声音设置页面" width="100%"></td>
    <td width="33%"><img src="design/screenshots/equalizer.png" alt="均衡器页面" width="100%"></td>
    <td width="33%"><img src="design/screenshots/microphone.png" alt="麦克风页面" width="100%"></td>
  </tr>
  <tr>
    <td align="center">声音设置</td>
    <td align="center">均衡器</td>
    <td align="center">麦克风</td>
  </tr>
</table>

## 功能

- EQ / 环境音效调节
- 虚拟环绕与耳机虚拟化
- 7.1 声道布局与逐声道音量
- 麦克风监听与实时采集
- 耳机端点 loopback 录音
- 实时频谱可视化
- 场景管理与默认配置恢复
- Windows 桌面应用，支持托盘式启动

## 技术栈

- Go + Wails
- WebView2 前端
- Windows 驱动桥接层
- Python 构建/打包脚本

## 快速开始

```bash
git clone https://github.com/wlisboy/iKF-Audio-Center.git
cd iKF-Audio-Center

python tools/build_center.py
python iKF-Audio-Center-Wails/tools/build_frontend.py
python iKF-Audio-Center-Wails/tools/build.py
```

## 目录

```text
.
├── LICENSE
├── README.md
├── design/                     # HTML 页面预览
├── tools/                      # Python 构建脚本
├── iKF-Audio-Center-Wails/     # Go/Wails 桌面应用
│   ├── app.go
│   ├── driver.go
│   ├── main.go
│   ├── frontend/
│   └── tools/
└── ...
```

## 要求

- Windows 10 / 11
- Python 3.13+
- Go 1.22+
- Wails v2
- 已安装官方 iKF / C-Media 驱动

## 免责声明

本项目仅供学习、研究和个人使用，不与 iKF、C-Media 或官方厂商建立关联。

## License

[MIT](LICENSE)
