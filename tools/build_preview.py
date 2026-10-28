# -*- coding: utf-8 -*-
"""生成离线交互预览（评审道具）。

用法：
    python tools/build_preview.py            → design/自定义场景-预览.html
    python tools/build_preview.py --viz      → design/律动空间-预览.html

产物 = frontend/dist/index.html + 一层假后端（拦 fetch），
不需要装驱动、不需要起服务，双击就能点：
  · 自定义场景页：点中央卡片 → 应用场景，右侧「使用中 / 场景详情」跟着变
  · 律动空间页：WebGL 地形在浏览器里直接跑（音频通路是模拟的，所以地形是平的），
    拖拽转视角 / 滚轮缩放 / 双击复位都能试

它只是评审用的道具，不进构建链路 —— build_frontend.py / build.py 都不读它。

★ 切页必须走 `#dock button[data-s=...]` 的 click。`.pgroup` 的显隐、标题与副标
  文案、`render(); hud();`、`VIS.setActive()` 全挂在 dock 的那个 click 监听里，
  只写 `S.section = 'profile'` 会得到「画布/舞台对了但右侧面板还是声音页内容」
  的半成品 —— 本文件早先就是这么写的，预览里面板一直是声音页。
"""

import io
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "iKF-Audio-Center-Wails", "frontend", "dist", "index.html")
OUT_PROFILE = os.path.join(ROOT, "design", "自定义场景-预览.html")
OUT_VIZ = os.path.join(ROOT, "design", "律动空间-预览.html")

MOCK = r"""<script>
/* ---------------- 假后端：只为把界面跑起来看 ---------------- */
(function(){
  var SCENES = [
    {name:'电竞 · 低延迟', desc:'关掉一切音效，只留定位', groups:['声音','均衡器'],    n:19},
    {name:'影音 · 宽声场', desc:'看片用的宽声场',         groups:['声音','均衡器','麦克风'], n:34},
    {name:'播客 · 人声',   desc:'人声靠前，低切干净',     groups:['均衡器','麦克风'],    n:23},
    {name:'会议 · 降噪',   desc:'麦克风强降噪',           groups:['麦克风'],           n:11},
    {name:'夜间 · 轻听',   desc:'低频收敛，不吵人',       groups:['声音','均衡器'],      n:17},
    {name:'通勤 · 隔音',   desc:'地铁上用',               groups:['声音'],             n:9},
    {name:'古典 · 均衡',   desc:'不染色',                 groups:['均衡器'],             n:14},
    {name:'摇滚 · 低音',   desc:'低频抬 3dB',             groups:['均衡器'],             n:12}
  ];
  var BUILTIN = '原厂默认';
  var DIR = 'C:\\Users\\你\\AppData\\Roaming\\iKF Audio Center\\profile';
  var active = '', dirty = false;

  function list(){
    var out = SCENES.map(function(s){
      return {name:s.name, desc:s.desc, builtin:false, items:s.n, groups:s.groups};
    });
    out.push({name:BUILTIN, desc:'驱动自带的基线', builtin:true, items:41,
              groups:['声音','均衡器','麦克风','降噪']});
    return out;
  }
  function items(name){
    var s = null, i;
    for (i=0;i<SCENES.length;i++) if (SCENES[i].name === name) s = SCENES[i];
    if (!s) return {ok:true, name:name, builtin:true, count:41,
      groups:[{key:'space',label:'声音',ep:'line',n:9},
              {key:'eq',label:'均衡器',ep:'line',n:12},
              {key:'mic',label:'麦克风',ep:'capture',n:13},
              {key:'denoise',label:'降噪',ep:'capture',n:7}], pairs:{}};
    return {ok:true, name:name, builtin:false, count:s.n,
      groups:s.groups.map(function(g){
        return {key:g, label:g, ep:(g==='麦克风'?'capture':'line'),
                n: Math.max(4, Math.round(s.n / s.groups.length))};
      }), pairs:{}};
  }

  window.fetch = function(url, opt){
    url = String(url);
    var body = {ok:true}, method = (opt && opt.method) || 'GET';
    if (method === 'POST'){
      var req = {};
      try { req = JSON.parse((opt && opt.body) || '{}'); } catch(_){}
      if (url.indexOf('/api/profile/apply') >= 0){ active = req.name || ''; dirty = false; body = {ok:true}; }
      else if (url.indexOf('/api/profile/save') >= 0){
        SCENES.push({name:req.name || '新场景', desc:req.desc || '', groups:['声音','均衡器'],
                     n:11 + Math.floor(Math.random()*20)});
        active = req.name || '新场景'; dirty = false; body = {ok:true};
      }
      else if (url.indexOf('/api/profile/delete') >= 0){
        SCENES = SCENES.filter(function(s){ return s.name !== req.name; });
        active = ''; dirty = false; body = {ok:true};
      }
      else if (url.indexOf('/api/profile/rename') >= 0){
        SCENES.forEach(function(s){ if (s.name === req.name) s.name = req.new_name || s.name; });
        if (active === req.name) active = req.new_name || active;
        body = {ok:true};
      }
      else if (url.indexOf('/api/set') >= 0){ dirty = true; body = {ok:true}; }
    } else {
      if (url.indexOf('/api/profiles') >= 0)
        body = {ok:true, dir:DIR, writable:true, list:list()};
      else if (url.indexOf('/api/profile/items') >= 0){
        var nm = decodeURIComponent(url.split('name=')[1] || '');
        body = items(nm.replace(/\+/g, ' '));
      }
      else if (url.indexOf('/api/status') >= 0)
        /* ★ 第 28 轮补：/api/status 多了 test_channels（每项四字段，第四项是
           测试音 wav 文件名），设备页的声道 chip 行全靠它渲染 —— 预览里不补
           就是一片空白，看不到这一轮修好的东西。第四项同时进了 chip 的 tooltip。 */
        body = {ok:true, connected:true, device:'iKF King Pro', model:'iKF King Pro',
                test_channels:[['LF',1,'左前','L-Front.wav'],
                               ['RF',2,'右前','R-Front.wav'],
                               ['C',4,'中置','Center.wav'],
                               ['W',8,'低音','Subwoofer.wav'],
                               ['LB',16,'左后','L-BackSurround.wav'],
                               ['RB',32,'右后','R-BackSurround.wav'],
                               ['LS',512,'左环绕','L-Surround.wav'],
                               ['RS',1024,'右环绕','R-Surround.wav']]};
      /* ★ 第 30 轮补：/api/params 的真实契约是 {name:{r,v,w,t}}，**直接返回、
         不包壳**（见 app.go 的契约表与 center_src.py 的 param_map）。这里原来
         返回 {ok, values:{}, dirty} —— 前端拿到的 PM['Enable_EQ_GFX'] 是
         undefined，统一处理器于是把所有开关判成「不可写」整片置灰，
         预览页看起来像驱动一样都不支持。现在按真机 --dump 的读数补齐这几组，
         预览与桌面表现一致：ret=2 的三项（音频增艳 / 动态低音 / 语音清晰）
         本来就该是灰的。 */
      else if (url.indexOf('/api/params') >= 0){
        var cap = url.indexOf('ep=capture') >= 0;
        var PV = {},
            R0 = function(v, t){ return {r:0, v:String(v), w:true, t:t || 'int'}; },
            RX = function(){ return {r:2, v:'', w:true, t:'int'}; };
        PV['VolumeScalarControl'] = cap ? {r:0, v:'0.5', w:true, t:'float'}
                                        : {r:0, v:'0.78', w:true, t:'float'};
        PV['VolumeControl'] = {r:0, v: (cap ? '0' : '-3.5'), w:true, t:'float'};
        PV['MuteControl'] = R0(0);
        PV['Enable_EQ_GFX'] = R0(1);
        PV['RFX_ENVIRONMENT'] = R0(1);
        PV['RFX_ROOMSIZE'] = R0(1);
        PV['Enable_RFX_GFX'] = R0(0);
        PV['Enable_SS_LFX'] = R0(1);
        PV['GFSControl'] = R0(0);
        PV['Enable_AUDIOBRILLIANT_LFX'] = RX();   /* 本机 ret=2 */
        PV['Enable_BM_GFX'] = RX();               /* 动态低音：本机 ret=2 */
        PV['Enable_VOICECLARITY_LFX'] = RX();     /* 语音清晰：本机 ret=2 */
        PV['Enable_KEYSHIFT_GFX'] = R0(0);
        PV['KEYSHIFT_LEVEL'] = R0(0);
        PV['Enable_VOCALCANCEL_GFX'] = R0(0);
        PV['VOCALCANCEL_LEVEL'] = R0(50);
        PV['DevSampleRateControl'] = R0(48000);
        PV['NoiseReduction_Level'] = R0(14);
        PV['MICECHO_Level'] = R0(0);
        PV['AAVolumeControl'] = R0(0);
        PV['MagicVoice_Selection'] = R0(0);
        PV['AGCControl'] = R0(1);
        PV['Enable_NOISEREDUCTION_LFX'] = R0(1);
        PV['Enable_MAGICVOICE'] = R0(0);
        PV['Enable_MICECHO'] = R0(0);
        for (var ei = 1; ei <= 10; ei++) PV['EQ_Slider' + ei] = R0(0);
        body = PV;
      }
      /* 律动空间页的音频通路：预览里没有真音频，一律回「空但结构正确」的响应，
         免得桥接层/帧循环读到 undefined 抛错。地形会因此渲染成平地。 */
      else if (url.indexOf('/api/loopback/devices') >= 0)
        body = {ok:true, list:[]};
      else if (url.indexOf('/api/loopback/status') >= 0)
        body = {ok:true, playing:false};
      else if (url.indexOf('/api/spectrum') >= 0)
        body = {ok:true, playing:false, bins:[]};
    }
    return new Promise(function(res){
      setTimeout(function(){
        res({ok:true, status:200,
             json:function(){ return Promise.resolve(body); },
             text:function(){ return Promise.resolve(JSON.stringify(body)); }});
      }, 60);
    });
  };

  try {
    var keep = localStorage.getItem('iKF.activeProfile');
    if (keep && keep !== BUILTIN){ active = keep; }
    else { active = '电竞 · 低延迟'; }
    localStorage.setItem('iKF.activeProfile', active);
  } catch(_){ active = '电竞 · 低延迟'; }

  /* ★ 切页走真的 dock 点击 —— .pgroup 的显隐、标题/副标文案、render()/hud()、
     VIS.setActive() 全挂在那个 click 监听里。只写 S.section 会得到
     「舞台对了但右侧面板还是声音页内容」的半成品。 */
  window.addEventListener('load', function(){ setTimeout(function(){
    var b = document.querySelector('#dock button[data-s="__PAGE__"]');
    if (b) b.click();
  }, 220); });
})();
</script>
"""

BANNER = """<div style="position:fixed;left:50%;transform:translateX(-50%);top:14px;z-index:99;
font:12px/1.6 -apple-system,'Segoe UI','Microsoft YaHei',sans-serif;color:#9aa0a6;
background:rgba(0,0,0,.45);padding:4px 12px;border-radius:999px;pointer-events:none">
离线预览 · 后端为模拟数据，__HINT__
</div>"""

HINTS = {
    "profile": "点中央卡片试试",
    "vis": "音频通路是模拟的，所以地形是平的；拖拽 / 滚轮 / 双击都能试",
}


def main():
    viz = "--viz" in sys.argv
    page = "vis" if viz else "profile"
    out = OUT_VIZ if viz else OUT_PROFILE
    html = io.open(DIST, encoding="utf-8").read()
    assert "</head>" in html and "</body>" in html
    mock = MOCK.replace("__PAGE__", page)
    banner = BANNER.replace("__HINT__", HINTS[page])
    html = html.replace("</head>", mock + "</head>", 1)
    html = html.replace("</body>", banner + "</body>", 1)
    with io.open(out, "w", encoding="utf-8", newline="\n") as f:
        f.write(html)
    print("已生成：%s" % out)
    print("  体积：%.1f KB" % (len(html) / 1024.0))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
