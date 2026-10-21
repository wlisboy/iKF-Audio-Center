# -*- coding: utf-8 -*-
"""
构建 iKF-Audio-Center.py
========================

把三样东西合成一个单文件程序：
  1. tools/src/center_src.py   —— Python 主体（驱动桥接 + HTTP 服务）
  2. design/iKF-Audio-Center.html —— 已完成的设计稿
  3. 本文件里的 BRIDGE_JS      —— 注入设计稿的驱动桥接层

用法：
    python tools/build_center.py
"""

import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
SRC = os.path.join(HERE, "src", "center_src.py")
DESIGN = os.path.join(ROOT, "design", "iKF-Audio-Center.html")
# 属性名清单是**构建输入**，和本脚本放在一起。
# 原先它在 .workbuddy-ai/tmp/ 下 —— 那是临时目录，清一次缓存这个构建就断了，
# 而且断在「找不到文件」上，看不出是缓存被清掉的。
NAMES = os.path.join(HERE, "propnames.json")
OUT = os.path.join(ROOT, "iKF-Audio-Center.py")

# 内联 favicon：避免浏览器自动请求 /favicon.ico 产生 404
FAVICON = (
    '<link rel="icon" href="data:image/svg+xml,'
    "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>"
    "<rect width='32' height='32' rx='7' fill='%23E60012'/>"
    "<text x='16' y='22.5' font-family='sans-serif' font-size='13'"
    " font-weight='700' fill='%23ffffff' text-anchor='middle'>iKF</text>"
    '</svg>">'
)

BRIDGE_JS = r"""
/* ============================================================
   驱动桥接层
   ------------------------------------------------------------
   由单文件程序注入，不属于设计稿本身。
   职责：把 osConfLib.dll 的真实参数接到界面上 —— 读回真实值、
   把交互写回驱动、给未开放的参数置灰。
   ============================================================ */
(function(){
  'use strict';

  var PM = {};                 /* 渲染端点参数名 -> {r,v,w,t} */
  var PMC = {};                /* 采集端点参数名 -> {r,v,w,t}（麦克风侧） */
  var PMET = {};               /* 当前「参数映射」页正在看的端点表 */
  var ST = null;               /* /api/status */
  var METER = null;            /* /api/meter（渲染端点） */
  var MICMETER = null;         /* /api/meter?ep=capture */
  var CUR_EP = 'render';       /* 参数映射页当前端点 */

  /* ---------------- 回环 A/B「没测到变化」的参数 ----------------
     来源：内部取证报告（回环 A/B + 反编译双证据）

     ★★ 这张表**不是「本机不支持」名单**。它记的只是**测量结果**：
        信号经驱动推入耳机端点 → 回环采 512 bin 频谱 → 与基线逐 bin 相减，
        这些项与「把当前值再写一遍」的空操作落在同一量级
        （Δmean ≈ 0.2 字节，1 字节 ≈ 0.27 dB；同轮噪声底 Δmax 3~4）。
        阳性对照 keyshift / vocalcancel Δmax = 192、eq_1k = 85.7 —— 尺子是准的。

     ★ 但「回环没测到」≠「耳机里没变化」，两个已知盲区：
       ① 生产回环 `_push_frames` 把多声道下混成 (L+R)/2 ＝ Mid，会在数学上
          抹掉 Mid/Side 空间类效果。已用逐声道 MultiTap 补测（ab_ch.py 第 8 轮，
          改喂环绕/后置信号：掩码 512=SL / 16=BL / 4=C）—— 结论不变，
          盲区排除了，负结果依旧。
       ② **WASAPI 回环采的是音频引擎交给驱动之前的那份数据**（还早于端点主
          音量与静音）。驱动之后、USB 设备固件里做的处理，回环原理上看不见。
       反编译 CMEffectAPO_108B.dll 的 RTTI 类清单：主机侧实现了 CCMEffectGFX /
       CCMEffectLFX / CEQ15BandLib / CKeyShifterLib / CVocalCancellationLib /
       CMagicVoiceLib / CReverbLib 等；**没有**耳机虚拟环绕 / VirtualBass /
       Brilliant / SpeakerShifter / SonicMapper / NeoPC / Dolby / DTS。
       实测与之一一对应：APO 里有实现的都响，没有的都不响。
       而能力掩码来自**设备固件**（GetSupportFeature = 16801926），说明这套
       DSP 至少有一部分在设备侧 —— 那部分回环天然测不到。

     ⟹ 所以只做**提示**，不置灰、不写「本机不支持」：界面不替设备下没有
        证据的结论。官方态快照（旧项目 go_d1.txt）里 Enable_SS_LFX = 1，
        用户也反馈官方面板的虚拟环绕听得到 —— 没有证据推翻它。
     ★ 别往这张表里加「没测过」的项。Enable_RFX_GFX（环境音效）就曾因
       RFX_ENVIRONMENT 取值太弱（3 = 卧房）被误判成死功能，换成 9 后
       全频段抬起 24~55 字节。 */
  var UNPROVEN = {
    Enable_AUDIOBRILLIANT_LFX: 1,   /* 音频增艳 —— APO 里连 Brilliant 字都没有 */
    Enable_VIRTUALBASS_LFX: 1,
    /* ★ 2026-10-19 深夜：Enable_SURRSENS_LFX（环绕增强）移出本表 ——
       CXearSurrMaxLib 有实体（vftable + 0x4e748 对象构造，K 档 LFX 唯一
       真模块），标「未实现」与反编译事实不符。它 A/B 未测到生效有前置
       条件解释（需虚拟环绕/扬声器移位/真实多声道之一开启，开关副标题
       已写明）。仍留在 Go 侧 sceneUnproven（场景暂不收）与隐藏名单
       （开发者开关可开），等前置条件下的 A/B 实测后再定。 */
    Enable_SonicMapper: 1,
    Enable_DDL_GFX: 1,
    Enable_DHDVS_LFX: 1,
    Enable_PLIIX_LFX: 1,
    Enable_NEOPC_LFX: 1,
    Enable_BM_GFX: 1,
    Enable_SS_LFX: 1,                 /* 虚拟环绕 —— 四次测量均在噪声底，但见盲区② */
    /* ★ 2026-10-19 深夜：Enable_CONLAB_HPSPK_LFX（耳机环绕）移出本表 ——
       第 44 轮真·应用流 A/B 测到 19.9（伪影底 9~11 的两倍），且场景收录
       口径（Go 侧）已把它当生效项保留；悬浮文字再挂「官方 APO 未实现」
       自相矛盾（用户实测指出）。改走下方 SW_TIP 专属说明。
       （历史备注：第 48 轮更正过「环绕增强」原绑的 Enable_SURRMAX_FOR_HPSPK
       是零引用假名，真开关是 Enable_SURRSENS_LFX。） */
    Enable_VOICECLARITY_LFX: 1        /* 语音清晰 —— 2026-10-18 入表（A/B 回环实测
                                         双音+噪声源、LEVEL/NOISESUPP=100，全频段
                                         噪声底且可逆）。
                                         ★★ 2026-10-19 深夜 REA-OUT 复核修正：
                                         原证据②「APO 内 VoiceClar 字符串零命中
                                         → 模块未实现」**方法无效** —— APO 根本
                                         不含任何 osConfLib 参数名（RFX_ENVIRONMENT/
                                         KEYSHIFT_LEVEL 同样 0 命中），模块有无
                                         只能看 RTTI + 位分发。真实结论：RTTI 有
                                         CVoclaEffectLib（真 vftable + 0x528 对象，
                                         非空桩），但构建器 FUN_1801d3d50 只挂
                                         **采集侧 bit23**，本机采集掩码 0x00028200
                                         不含该位 ⟹ 本机不构建；渲染侧（GFX/LFX）
                                         无任何它的构建器。文案已按此修正
                                         （见 UNPROVEN_NOTE）。 */
    /* ★★ 第 56 轮（L 档）：本表与 bindDevPanel 里的 APO_IMPL 是**两张不同问题的表**：
         · 本表 UNPROVEN  = 「**回环 A/B 测没测到变化**」（测量事实）
         · APO_IMPL       = 「**主机侧 APO 建没建这个模块**」（反编译事实，K 档）
       两者可同时成立：模块建了，但只在设备输出端生效 ⟹ 回环测不到。
       ⟹ 本表**一项都不动**（既不加、也不删），只把提示文案改准 —— 见 __unprovenNote。
       ★ 明确不加 `Enable_EQ_GFX`：它对应的 `CEQ15BandLib` 确实建了，但我们**从没测过**
         改 EQ 滑杆能不能改变回环频响；本表开头的规矩就是「别往里加没测过的项」。
       ★ 明确不删 `Enable_BM_GFX`：它对应不上 APO_IMPL 只说明「主机侧没模块」，
         不改变「测过、没测到」这个事实。 */
  };

  /* ★ 第 56 轮（L 档）：本机「主机侧真的有实现」的 APO 模块表。
     来源 = 内部取证报告：逐位反编译 APO 的模块
     构造器、读完对象 vftable 才写进这里 —— 不靠猜，也不靠「参数名字像」。
       GFX 侧：bit1 CReverbLib(环境音效) · bit2 CEQ15BandLib(均衡) ·
               bit13 CKeyShifterLib(变调) · bit14 CVocalCancellationLib(消人声)
       LFX 侧：bit7 CXearSurrMaxLib(环绕增强)
       采集侧：CBeamFormingLib(阵列) · CMagicVoiceLib(魔音)
     ★ 它与 UNPROVEN 答的是**两个不同问题**（见 UNPROVEN 表头的说明）：
       UNPROVEN = 「回环 A/B 测没测到变化」；本表 = 「主机侧 APO 建没建模块」。
     两者可同时成立：模块建了，但只在设备输出端生效 ⟹ 回环测不到。 */
  var APO_IMPL = {
    'Enable_RFX_GFX': 'CReverbLib',
    'Enable_EQ_GFX': 'CEQ15BandLib',
    'Enable_KEYSHIFT_GFX': 'CKeyShifterLib',
    'Enable_VOCALCANCEL_GFX': 'CVocalCancellationLib',
    'Enable_SURRSENS_LFX': 'CXearSurrMaxLib'
  };
  window.__apoImpl = APO_IMPL;
  /* 提示文案（★ 2026-10-18 终稿：统一且简短——
     「同一件事只有一个说法」。
     ★ 2026-10-19 深夜修正：此前这里写「6 个置位里仅 bit1 真建模块，
     bit2/7/13/14 是空壳」——与 K 档取证原文不符，系笔误。K 档 §2.2/2.5：
     GFX 位 1/2/13/14（CReverbLib/CEQ15BandLib/CKeyShifterLib/
     CVocalCancellationLib）与 LFX 位 7（CXearSurrMaxLib）都是真模块
     （非空桩），只有 GFX bit3/4/20 与 LFX 其余位是空桩；采集侧
     CBeamFormingLib/CMagicVoiceLib 也是真模块。REA-OUT 反编译产物复核：
     CKeyShifterLib/CVocalCancellationLib/CXearSurrMaxLib 均有真实 vftable
     与构造体（CMEffectAPO_108B.dll.c 506603/507208/515290 行）。 */
  var UNPROVEN_TEXT = '官方 APO 未实现 · 反编译确认';
  /* ★ 2026-10-19 深夜（REA-OUT 复核）：个别项的专属修正文案 —— 覆盖默认句。
     语音清晰：官方 APO **有**模块类 CVoclaEffectLib（真 vftable + 0x528 对象，
     CMEffectAPO_108B.dll.c 515709 行构造器），但构建器 FUN_1801d3d50 只挂在
     **采集侧 bit23** 的位分发上，本机采集掩码 0x00028200 不含该位 ⟹ 本机
     不构建；渲染侧（GFX/LFX）无任何它的构建器。加上 A/B 回环未测到变化，
     维持隐藏/如实标注口径，只把文案改准。 */
  var UNPROVEN_NOTE = {
    'Enable_VOICECLARITY_LFX': '官方 APO 有模块（CVoclaEffectLib），本机固件能力掩码未启用 · A/B 未测到生效'
  };
  window.__unprovenNote = function(p){ return (p && UNPROVEN_NOTE[p]) || UNPROVEN_TEXT; };
  window.__unprovenNoteShort = function(p){ return (p && UNPROVEN_NOTE[p]) || UNPROVEN_TEXT; };
  /* ★ 2026-10-19：个别开关的专属悬浮说明 —— 覆盖默认的「· 可写」。
     ★ 2026-10-19 深夜（用户拍板）：耳机环绕仍用「· 可写」，此表暂空，
       保留机制供以后有需要时加行。 */
  var SW_TIP = {};
  /* 声场页两个已入 UNPROVEN 的旋钮（悬停提示与开关口径一致） */
  var DIAL_NOTE = { bass: '动态低音', clear: '语音清晰' };
  /* 旋钮 → 对应 UNPROVEN 参数名（专属修正文案分流用，2026-10-19 深夜） */
  var DIAL_NOTE_P = { bass: 'Enable_BM_GFX', clear: 'Enable_VOICECLARITY_LFX' };

  var LAST = { eq: null, room: null, vol: null };
  var EQ_W = false, ROOM_W = false, VOL_W = false;

  function $(id){ return document.getElementById(id); }
  function get(u){
    return fetch(u, {cache:'no-store'}).then(function(r){ return r.json(); });
  }
  function postRaw(u, b){
    return fetch(u, {method:'POST',
      headers:{'Content-Type':'application/json'},
      body: JSON.stringify(b)}).then(function(r){ return r.json(); });
  }
  function post(b){ return postRaw('/api/set', b); }

  /* ---------------- 浮层提示 ---------------- */
  function toast(msg, bad){
    var t = $('__toast');
    if (!t){
      t = document.createElement('div');
      t.id = '__toast';
      t.style.cssText =
        'position:absolute;left:50%;bottom:30px;transform:translateX(-50%);'
        + 'padding:9px 18px;border-radius:10px;font-size:12.5px;z-index:99;'
        + 'background:var(--glass2);border:1px solid var(--line);color:var(--t1);'
        + 'backdrop-filter:blur(14px);-webkit-backdrop-filter:blur(14px);'
        + 'transition:opacity .25s;opacity:0;pointer-events:none;white-space:nowrap';
      $('frame').appendChild(t);
    }
    t.textContent = msg;
    t.style.color = bad ? 'var(--accent)' : 'var(--t1)';
    t.style.opacity = '1';
    clearTimeout(t._h);
    t._h = setTimeout(function(){ t.style.opacity = '0'; }, 2100);
  }

  /* ---------------- 设备连接状态 ----------------
     第 26 轮：这里原来是左下角一个**独立浮层**（#__live，left:26px/bottom:74px）。
     问题是它和设备胶囊说的是同一件事 —— 胶囊写「iKF-V11 已连接」，
     浮层写「驱动已连接 · iKF7.1.dll」，同一句话分两处写；而左下角那个位置
     夹在左侧导航 dock 与主题切换按钮之间，孤零零一个胶囊，像掉出来的。
     现在并进胶囊：#devState 就是它的落点，圆点按状态变色（正常绿 / 异常灰）。
     第 28 轮：用户指出「已连接 · osConfLib.dll」这一串太突兀 ——
     名字里带 .dll 的文件名对普通用户没有意义，而「已连接」本身就已经表示
     驱动接通了（这正是用户的原话）。所以状态文字只留「已连接」，
     驱动文件名改挂 title，鼠标停上去才看得到。
     这样胶囊从两段信息变成一段：设备名 + 连接状态。 */
  function badge(text, ok, detail){
    var s = $('devState');
    if (!s) return;
    s.textContent = text;
    var p = s.parentNode;                       /* .devpill */
    var dot = p && p.querySelector ? p.querySelector('i') : null;
    if (dot){
      dot.style.background = ok ? 'var(--green)' : 'var(--t3)';
      dot.style.boxShadow  = ok ? '' : 'none';  /* 绿色光晕在异常时要撤掉 */
    }
    /* title 用「设备名 · 状态 · 驱动文件」拼，作为不占版面的备查信息。
       没给 detail 时退回纯状态文字，不留空段。 */
    var name = $('devName');
    var parts = [];
    if (name && name.textContent) parts.push(name.textContent.trim());
    parts.push(text);
    if (detail) parts.push(detail);
    if (p) p.title = parts.join(' · ');
  }

  /* ---------------- 未开放参数置灰 ----------------
     只把控件调暗是不够的：旋钮上仍显示 45 / 30 这样的数字，
     看起来像「有值、但拖了没反应」。这里把读数改成「—」、
     给标签补上「· 不可用」，让「本机调不了」一眼就能看出来，
     而不是让用户反复拖拽去猜。 */
  function dim(el, why){
    if (!el || el.getAttribute('data-dimmed')) return;
    el.setAttribute('data-dimmed', '1');
    el.style.opacity = '.34';
    el.style.pointerEvents = 'none';
    /* 原 title 也存一份：volume 旋钮的 title 里有「主音量 −12.34 dB」，
       undim 时不能直接清空（见 undim）。 */
    if (el.dataset.origTitle === undefined) el.dataset.origTitle = el.title || '';
    el.title = why || '当前设备不支持该参数';
    var n = el.querySelector('.dnum');
    if (n) n.textContent = '—';
    var l = el.querySelector('.dlab');
    if (l && l.textContent.indexOf('不可用') < 0){
      /* ★ 第 47 轮：原文存一份。旋钮会被反复 dim / undim（切端点、
         参数从不可读变可读），不存原文的话后缀会一路叠上去擦不干净。 */
      if (l.dataset.orig === undefined) l.dataset.orig = l.textContent;
      l.textContent += ' · 不可用';
    }
    /* 开关 button.sw 里没有文字，名字在同行 .nm 里 —— 只把圆点调暗的话，
       用户看到的是一个灰疙瘩，猜不出为什么点不动。 */
    if (el.classList && el.classList.contains('sw')) tagSwitch(el, true);
  }

  /* dim() 的逆操作。开关那条路走 tagSwitch(sw,false)，旋钮这条路的
     标签是 .dlab + 读数 .dnum，tagSwitch 管不到，所以单独一个。
     ★ 第 47 轮：旋钮现在会「先灰后亮」（第 46 轮接了参数、但 bindVolume
       里留着的硬置灰又把它按死了 —— 见 bindVolume 的注释），必须能还原。 */
  function undim(el){
    if (!el) return;
    el.removeAttribute('data-dimmed');
    el.style.opacity = '';
    el.style.pointerEvents = '';
    /* ★ 只在真的 dim 过时才动 title —— 否则会把 bindVolume 刚写进去的
       「主音量 −12.34 dB」清成空串。 */
    if (el.dataset.origTitle !== undefined) el.title = el.dataset.origTitle;
    var l = el.querySelector('.dlab');
    if (l && l.dataset.orig !== undefined) l.textContent = l.dataset.orig;
  }

  /* 给开关所在那行的名字补 / 撤「· 不可用」。
     ★ 名字是设计稿写死的：先存一份原文再改，还原时一模一样地放回去 ——
       否则反复切端点会把后缀叠上去又擦不干净。 */
  function tagSwitch(sw, off){
    var nm = sw.parentNode ? sw.parentNode.querySelector('.nm') : null;
    if (!nm) return;
    if (nm.dataset.orig === undefined) nm.dataset.orig = nm.textContent;
    nm.textContent = nm.dataset.orig + (off ? ' · 不可用' : '');
  }

  /* ---------------- 开关联动降暗（第 59 轮） ----------------
     开关关着时，它从属的滑杆/下拉整体降不透明度（.grp-dim，样式在设计稿
     第 59 轮段）—— 只表达「现在不生效」，**不禁用**：不设
     pointer-events:none，拖了/选了照常写驱动，行为与以前完全一致。
     映射 = data-p -> 从属控件 id 列表；从属框取最近容器
     （.slider / .row），降暗连标签一起降，口径与静态缩进一致。
     联动不挂在各个同步点上（同步/乐观翻转/失败回滚有四五处），
     而是挂一个 class 属性的 MutationObserver：**任何路径**改了开关的
     on 态都会自动刷新从属控件，以后加新开关也只需补一行映射。 */
  var LINK_DIM = {
    Enable_RFX_GFX:            ['roomRange','envSel'],
    Enable_AUDIOBRILLIANT_LFX: ['brRange'],
    Enable_KEYSHIFT_GFX:       ['ksRange'],
    Enable_VOCALCANCEL_GFX:    ['vcRange'],
    Enable_NOISEREDUCTION_LFX: ['nrRange'],
    Enable_MAGICVOICE:         ['mvSel'],
    Enable_MICECHO:            ['echoRange'],
    AGCControl:                ['aaRange'],
    Enable_CONLAB_HPSPK_LFX:   ['hrsModeSel','hrsRoomSel','sfxRange','spkRange'],
    Enable_DHDVS_LFX:          ['dhSel','dhHpSel','dhVsSel']
  };
  function linkTargets(sw){
    var ids = LINK_DIM[sw.dataset.p];
    if (!ids) return null;
    var out = [];
    ids.forEach(function(id){
      var el = $(id);
      if (!el) return;
      out.push(el.closest('.slider') || el.closest('.row') || el);
    });
    return out.length ? out : null;
  }
  function linkRefresh(sw){
    var ts = linkTargets(sw);
    if (!ts) return;
    /* 开关本身置灰（本机不支持）时也没有 on 态 ⟹ 从属控件一并降暗，
       与 dim() 的「不可用」语义一致。 */
    var off = !sw.classList.contains('on');
    ts.forEach(function(el){ el.classList.toggle('grp-dim', off); });
  }
  function linkSweep(){
    Array.prototype.forEach.call(
      document.querySelectorAll('button.sw[data-p]'), linkRefresh);
  }
  (function(){
    var mo = new MutationObserver(function(muts){
      for (var i = 0; i < muts.length; i++){
        var t = muts[i].target;
        if (t.classList && t.classList.contains('sw')) linkRefresh(t);
      }
    });
    mo.observe(document.body, { subtree:true, attributes:true, attributeFilter:['class'] });
    /* 初始态：参数还没回来时所有映射开关都是 off，从属控件先降暗，
       等驱动读回开态再点亮（applyParams 末尾的 linkSweep 兜底）。 */
    linkSweep();
  })();

  /* 开关状态 → 中文；读不到返回 null（区别于「关」） */
  function onoff(d){
    if (!d || d.r !== 0) return null;
    return d.v === '1' ? '开' : '关';
  }

  /* ---------------- 1. 拉取设备状态 ---------------- */
  badge('正在连接驱动 …', false);

  get('/api/status').then(function(st){
    ST = st;
    if (!st.ok){
      badge('未连接驱动', false);
      toast('未检测到 iKF 驱动：' + (st.error || ''), true);
      bindUnsupportedAll();
      return null;
    }
    /* ★ 第 28 轮：状态文字只写「已连接」，驱动文件名走 title。
       普通用户看不懂 osConfLib.dll，而「已连接」本身就表示驱动接通了。 */
    badge('已连接', true, '驱动 ' + (st.dll || '').split('\\').pop());

    /* 品牌区 / 设备名 */
    DEV.model  = st.model  || DEV.model;
    DEV.codec  = st.codec  || DEV.codec;
    DEV.driver = st.driver || DEV.driver;
    DEV.hwid   = st.hwid   || DEV.hwid;
    if ($('devName')) $('devName').textContent = DEV.model;

    bindDevPanel(st);
    bindVolume(st);
    bindRoom(st);
    bindSwitches(st);
    bindExtras(st);
    bindDials();
    bindVss();
    bindEqChips();
    bindDevButtons();
    bindHonesty();
    bindMicPanel(st);
    bindDevExtras(st);
    bindProfile();
    markUnprovenRows();

    /* 两个端点各拉一次。采集端点（麦克风）与渲染端点的属性可读性
       完全不同 —— 麦克风侧参数只在采集端点上存在。 */
    return Promise.all([get('/api/params'), get('/api/params?ep=capture')]);
  }).then(function(res){
    if (res){
      PM  = res[0] || {};
      PMC = res[1] || {};
      applyParams(PM, PMC);
    }
    wrapRender();
    startMeter();
  }).catch(function(e){
    badge('连接异常', false);
    toast('连接异常：' + e, true);
  });

  /* 供组态页调用的两个钩子。
     __relayout：面板里增删了行，右上读数区与右下控制面板的避让要重算
                 （layoutHud 定义在设计稿里，是全局函数）。
     __resync  ：组态应用后驱动参数变了，界面读数必须重新拉一遍，
                 否则显示的仍是应用前的值。 */
  if (typeof layoutHud === 'function') window.__relayout = layoutHud;
  window.__resync = function(){
    return Promise.all([get('/api/params'), get('/api/params?ep=capture')])
      .then(function(res){
        PM  = res[0] || {};
        PMC = res[1] || {};
        applyParams(PM, PMC);
      }).catch(function(){});
  };

  /* 托盘侧改了驱动参数（静音切换 / 音量滑杆）时，Go 推 tray:sync 事件
     （tray.go notifyFrontend），前端收到后整体重拉 —— 与 __resync 同路。
     防抖 300ms：音量滑杆拖动会连发多条。
     ★ serve 诊断模式没有 window.runtime（事件系统是 Wails 注入的），
     这里自然跳过 —— 那种模式下主界面本就不存在，无 UI 可同步。 */
  if (window.runtime && window.runtime.EventsOn){
    var traySyncAt = 0;
    window.runtime.EventsOn('tray:sync', function(){
      var now = Date.now();
      /* 本地写入守卫：600ms 内主界面刚写过驱动的，这次「外部变化」多半
         就是本地写自己的回声（watch 轮询 250ms 能看到它），忽略即可 ——
         否则重同步会重设控件，和拖拽打架。真正的外部变化（系统音量键等）
         下一次轮询仍会上报。 */
      if (now - (window.__lastLocalWrite || 0) < 600) return;
      if (now - traySyncAt < 100) return;
      traySyncAt = now;
      if (window.__resync) window.__resync();
    });
    window.__traySyncBound = true;   /* 探针可据此确认监听已注册 */
  }

  /* ---------------- 2. 设备页 ---------------- */
  function bindDevPanel(st){
    /* ★ 2026-10-18 根修：kv 行去重后数量变了，旧的按位置 setKv(i) 整体
       错位 —— 驱动版本被写成编解码器名（产品名式长串）、DirectX 行写成
       设备 ID、音量范围写成「来自设备描述符」。改为按 id 定位，行序以后
       随便调都不会错位。取值全部来自 status() 真实字段：
       驱动版本=st.driver（10.0.1.1）· 设备 ID=st.hwid（实例路径）·
       DirectX=st.dx · 音量范围=vol_min…vol_max。型号/编解码器在右上角
       读数区显示，卡片不再重复。 */
    function setKvId(id, val){
      var b = document.querySelector('#pg-dev #' + id);
      if (b) b.textContent = (val === null || val === undefined || val === '') ? '—' : val;
    }
    setKvId('drvVer', st.driver);
    /* 设备 ID / 型号来源 / DirectX 三行已随 2026-10-18 精简删除
       （参数页可查），对应写入一并移除。 */
    setKvId('devVol', (st.vol_min === null || st.vol_max === null)
      ? '—'
      : (st.vol_min.toFixed(1) + ' … ' + st.vol_max.toFixed(1) + ' dB'));
    /* ★ 2026-10-18：步进信息降为浅色小字后缀——主值干净，细节不丢。
       （全角括号挤在右对齐值里显得杂乱，用户反馈改版。） */
    var dvEl = document.querySelector('#pg-dev #devVol');
    if (dvEl && st.vol_step && st.vol_min !== null && st.vol_max !== null){
      dvEl.innerHTML = dvEl.textContent
        + ' <span style="font-weight:400;font-size:11px;color:var(--t3)">· 步进 '
        + st.vol_step + '</span>';
    }

    /* ★ 2026-10-18：说明写入器已删——它的 font-size:10.5px 选择器会命中
       声道配置的说明行并覆写成设备说明（用户截图抓到）。设备说明行本身
       已随精简删除，无再写入的必要。 */
  }

  /* ---------------- 3. 音量旋钮 ---------------- */
  function bindVolume(st){
    var dial = document.querySelector('.dial[data-dial="vol"]');
    if (!dial) return;
    if (st.vol_scalar === null || st.vol_scalar === undefined){
      dim(dial, '当前设备不支持主音量');
    } else {
      VOL_W = true;
      var v = Math.round(st.vol_scalar * 100);
      dial.dataset.val = v;
      if (window.paintDial) paintDial(dial);
      LAST.vol = v;
      dial.title = '主音量 ' + (st.vol_db === null ? '—' : st.vol_db.toFixed(2) + ' dB')
        + ' · 上下拖拽或滚轮调节';
    }
    /* ★ 第 47 轮删除：这里原来无条件把「动态低音」「语音清晰」两个旋钮
       dim() 成「本机 APO 未实现此效果」。那句话在**第 46 轮接好线之后就
       不成立了** —— 两个旋钮现在分别绑 BM_Slider_BassLevel（配 Enable_BM_GFX）
       与 VOICECLARITY_LEVEL（配 Enable_VOICECLARITY_LFX），两项都已进白名单
       且 write 探测通过。留着这句硬置灰的后果是：旋钮带 data-val 却
       pointerEvents:none —— 数字在、拖不动，正是这一版截图里看到的样子。
       现在改成**由 syncDials 按驱动实测值决定**：读得到就点亮，读不到才灰。
       （读数也不再是设计稿写死的 45 / 30，而是驱动的真实值。） */

    /* 写回策略：数值一变就写（180ms 防抖），不再只在 pointerup 写。
       音量旋钮贴在面板底边，向下拖很容易把指针拖出窗口 ——
       旧写法只挂 pointerup，一旦指针在窗口外松开就收不到事件，
       表现正是「界面数字变了、设备音量没变」。 */
    var timer = null, pending = null;
    function flush(){
      if (pending === null) return;
      var nv = pending; pending = null;
      LAST.vol = nv;
      post({name:'VolumeScalarControl', value: nv / 100}).then(function(r){
        if (r.ok) toast('主音量 ' + nv + '%');
        else toast('音量设置失败，请稍后重试', true);
      });
    }
    dial.addEventListener('dialchange', function(){
      if (!VOL_W) return;
      var nv = +dial.dataset.val;
      if (nv === LAST.vol) return;
      pending = nv;
      clearTimeout(timer);
      timer = setTimeout(flush, 180);
    });
    window.addEventListener('pointerup', function(){ clearTimeout(timer); flush(); });
    window.addEventListener('pointercancel', function(){ clearTimeout(timer); flush(); });
  }

  /* ---------------- 4. 房间大小 ----------------
     设计稿的画布半径是 60..130 的连续百分比，驱动只有 3 档
     （0 小 / 1 中 / 2 大）。这里保持滑杆语义不变，只在写驱动时
     把百分比吸附到最近的档位，并把右侧读数换成中文档位名。 */
  var ROOM_MAP = [72, 100, 124];           /* 驱动 0/1/2 → 画布百分比 */
  function roomToStep(v){ return v < 86 ? 0 : (v < 112 ? 1 : 2); }
  function roomLabel(step){
    return (ST && ST.room_sizes && ST.room_sizes[step]) || String(step);
  }
  function bindRoom(st){
    var rr = $('roomRange'), rv = $('roomVal');
    if (!rr) return;
    if (!PM0(st, 'RFX_ROOMSIZE')){
      dim(rr.parentNode, '当前设备不支持房间大小');
      return;
    }
    ROOM_W = true;
    var step = (st.room >= 0 && st.room <= 2) ? st.room : 0;
    rr.value = ROOM_MAP[step];
    S.room = ROOM_MAP[step];
    LAST.room = step;
    if (rv) rv.textContent = roomLabel(step);
    rr.title = '房间大小（驱动 3 档）· ' + (st.room_sizes || []).join(' / ');
    /* 设计稿的 input 监听会把读数写成百分比数字，这里改回档位名 */
    rr.addEventListener('input', function(){
      if (rv) rv.textContent = roomLabel(roomToStep(S.room));
    });
  }
  function PM0(st, name){ return true; }   /* 房间参数在本机实测可用 */

  /* ---------------- 5. 开关 ----------------
     所有开关统一走「属性直连」：设计稿里每个 button.sw 自带
     data-p / data-label（麦克风侧再加 data-ep="capture"），
     状态由下方统一处理器按驱动读回值同步，点击也由全局委托处理。

     ★ 第 30 轮撤掉了原来那张 SWITCH_MAP。它按「#pg-* 内 button.sw
     的出现顺序」索引着填 data-p，隐患是**界面一改顺序就悄悄错位**；
     而这轮恰好把四组开关从均衡页搬到了声场页。既然统一处理器本来就
     支持属性直连，那张表就是纯负担，故删除 —— 顺序不再有任何隐式约束。

     语义备忘：**「智能音量」绑 Enable_SMART_VOLUME_GFX，不再绑 GFSControl**。
     第 46 轮查 osConfLib.dll 的字符串表才看穿原先的错判 —— 那个表分成
     两块：功能参数表（Enable_* / *_LEVEL / SS_* …）和**设备控制表**
     （GetDeviceFriendlyName / GetSupportFeature / AmplifierControl /
     IsSupportXearSurround71 / GFSControl / SpectrumControl…）。
     GFSControl 在**控制表**里，不是音效参数；它确实是可写的布尔开关
     （分发表 ID 0x10029 → RegSetValueExW 写 HKCU\SOFTWARE\C-Media\Hook\
     EnableGFS），但语义与「智能音量」无关，此前是**按名字猜的绑定**。
     官方 Xear Smart Volume 页的开关在功能表里叫 Enable_SMART_VOLUME_GFX，
     且与控件名 EnableSmartVol 在 XearAudioCenter_x64.exe 的字符串表中相邻
     （… EnableGX | Enable_SMART_VOLUME_GFX | SVN | EnableSmartVol …），配对确凿。
     ★ 当初判它「ret=2 = 本机不支持」是只读探测的误读（见 MEMORY 的 ret 语义）。 */
  function bindSwitches(st){
    /* 声场页补真实控件（挂在「环境类型」那一组下，
       见 addSpaceSwitch 里的锚点说明）。

       ★ 第 47 轮（B 档 · 环绕族）：把官方面板三张环绕页翻完之后，
         真正有界面可接的只有下面这些：
           apoSpeakerVirtual.cml              7.1 声场扩展（Enable_SS_LFX，第 30 轮已接）
           apoSpeakerXearSurroundHeadPhone.cml 开关 + 模式单选 + 房间大小三选
           apoSpeakerSurroundMax.cml          **整页只有一个开关**
         参数表里那 12 项 SURRSENS_* 与 4 项 DHDVS 官方从来没做过界面 ⟹ 不接。
         ★ 房间大小在耳机页是 RadioButton Tag 0=大 / 1=中 / 2=小（**与直觉反序**），
         所以下面选项顺序必须显式写成「大 / 中 / 小」，不能照「小中大」的习惯排。

       ★ 第 47 轮续：官方**耳机菜单**（MenuHeadset.cml）里除了
         `HeadsetXearSurroundHeadPhone` 还有 `HeadsetXearSurroundSpeakers`
         （Xear Surround Speaker），那张页有两根滑杆：声场扩展 0~3、
         扬声器角度 5~30（步进 5）。XearAudioCenter_x64.exe 的字符串表里
         `SPKAngle` / `SFExpansion` 与 `XearSurroundSpeakersHandler` 同块，
         而 `CONLAB_HPSPK_SFX` / `CONLAB_HPSPK_SPKANGLE` 也落在同一块里 ——
         `SPKAngle↔SPKANGLE` 是逐字同名，`SFExpansion↔SFX`（Sound Field
         eXpansion）是排除法剩下的一项。这两条属于**名字+邻接推断**，
         证据等级比 MODE/ROOMSIZE 低一档（那两条有 GroupName 同组佐证），
         所以下面的注释与报告里都如实标注，没有当成铁证。

       ★ 环绕增强的副标抄自官方词条 ml_HeadPhoneSurroundMax_ChannelRequirement：
         「启动 Xear Surround Max，必须先打开 Virtual Surround、Speaker Shifter
         或是 Real Multi-Channel 输出」—— 它是有前置条件的，必须写出来。 */
    addSpaceSwitch('Enable_SS_LFX', '虚拟环绕', '');
    /* ★ 第 49 轮：紧接着补「虚拟音箱摆位」编辑器（官方 7.1 Virtual Speaker
       Shifter 页）。必须紧跟开关之后 —— 下方所有动态行都插在 #spFxStart
       之前，所以插入顺序就是显示顺序，「虚拟环绕 → 摆位编辑器 → 环境音效」。 */
    addSpaceVssEditor();
    addSpaceSwitch('Enable_RFX_GFX', '环境音效', '');
    /* ★ 2026-10-19（用户拍板）：「房间大小」滑杆归属「环境音效」组 ——
       把设计稿静态放在「环境」段的滑杆行搬到环境音效开关行之后，
       加 .ctl-sub 缩进（与耳机环绕从属控件同一口径）。绑定/同步都走
       roomRange/roomVal 两个 ID，DOM 搬家不影响任何逻辑；sortSpaceRows
       会把它随环境音效组一起排序。画布虚线圈照旧写同一参数。 */
    (function(){
      var sw = document.querySelector('#pg-space button.sw[data-p="Enable_RFX_GFX"]');
      var swRow = sw && sw.closest('.row');
      var rr = $('roomRange');
      var row = rr && rr.closest('.slider');
      if (swRow && row && swRow.parentNode === row.parentNode){
        row.classList.add('ctl-sub');
        swRow.parentNode.insertBefore(row, swRow.nextSibling);
      }
    })();
    addSpaceSwitch('Enable_CONLAB_HPSPK_LFX', '耳机环绕', '');
    addSpaceSelect('hrsModeSel', 'CONLAB_HPSPK_MODE', '环绕模式',
      '', ['音乐/游戏', '电影']);
    addSpaceSelect('hrsRoomSel', 'CONLAB_HPSPK_ROOMSIZE', '环绕房间',
      '', ['大房间', '中房间', '小房间']);
    addSpaceSlider('sfxRange', 'CONLAB_HPSPK_SFX', '声场扩展', 0, 3, 1);
    addSpaceSlider('spkRange', 'CONLAB_HPSPK_SPKANGLE', '扬声器角度', 5, 30, 5);
    /* ★★ 第 48 轮更正：这一项原先绑 `Enable_SURRMAX_FOR_HPSPK` —— 绑错了。
       该名字在 XearAudioCenter_x64.exe 的字符串表里出现 **0 次**，只在
       osConfLib.dll 的参数表（第 136 项）里躺着，官方面板从不用它。
       Surround Max 页真正的总开关是 `Enable_SURRSENS_LFX` —— 它在 exe 与 dll
       里都有，且与 `Enable_DHDVS_LFX` / `Enable_SS_LFX` / `Enable_PLIIX_LFX` /
       `Enable_NEOPC_LFX` 排在同一块，那正是「每个官方页一个 LFX 总开关」的清单。
       （SURRSENS = Surround Sensation，Surround Max 的前身名。）
       教训与第 46 轮 GFSControl 同类：**名字像 ≠ 就是它**，必须落到 exe 引用上。 */
    addSpaceSwitch('Enable_SURRSENS_LFX', '环绕增强', '');
    /* ---- C 档：Dolby Headphone / Dolby Virtual Speaker（第 48 轮）----
       官方页 apoSpeakerDolbyHPVS.cml（**耳机菜单里有**）控件结构：
         总开关 + 「VS 还是 HP」二选一 + VS 组(Reference/Wide) + HP 组(Studio/Cinema/Hall)
       对应参数由 exe 字符串表坐实：总开关 Enable_DHDVS_LFX、二选一 DHDVS_USEDH、
       VS 档位 DVS_Mode、HP 档位 DH_Mode。 */
    addSpaceSwitch('Enable_DHDVS_LFX', 'Dolby 环绕', '');
    /* ★ 顺序有讲究：`DHDVS_USEDH` 字面就是「use DH」，所以 **1 = Dolby Headphone、
       0 = Dolby Virtual Speaker**。选项下标直接当值用，因此下面必须把 VS 排在前面
       —— 照「HP 更重要所以放前面」的直觉排就全反了（同 RoomSize 那次一个坑）。 */
    addSpaceSelect('dhSel', 'DHDVS_USEDH', 'Dolby 方式',
      '', ['Dolby Virtual Speaker', 'Dolby Headphone']);
    addSpaceSelect('dhHpSel', 'DH_Mode', 'Dolby 耳机空间',
      '', ['工作室', '影院', '大厅']);
    addSpaceSelect('dhVsSel', 'DVS_Mode', 'Dolby 扬声器模式',
      '', ['Reference', 'Wide']);
    /* ★★ 第 53 轮（I 档）：Dolby 还有一条**官方自己写出来的前置条件** ——
       喇叭组态必须是立体声(2 声道)，而且官方给它做了界面（见 addDolbyWarn 注释）。
       这里紧跟 Dolby 组把提示块建出来：动态行都是 insertBefore(#spFxStart)，
       所以这一行调用就落在 Dolby 组正下方。门控在 syncExtras 里的 dolbyGate()。 */
    addDolbyWarn();
    /* ★ 2026-10-18：声音页行序整理——可写/实测有效的功能排在前，
       UNPROVEN（实测无效·官方未实现）的整组沉到卡片末尾；
       纯 DOM 重排，界面不显示任何分组标记。排序在全部动态注入之后执行。 */
    sortSpaceRows();
  }

  /* 行序整理（见 addDolbyWarn 后的调用点）：按组重排 pg-space 直接子元素。
     组 = 一个功能开关行 + 其后续从属控件（ctl-sub / vss-tools / 提示行），
     直到下一个开关行或分节标题。rank 越小越靠前：
       0 = 保持原位的输出/环境参数区（音量旋钮、静音、左右声道、环境类型；
           房间大小已随环境音效组排序，不再落在这组）
       1 = 环境音效（实测有效）   2 = 升降调 / 人声消除（实测有效）
       3 = 智能音量 / 全景音效 / 声道复制 / 声道互换（可写）
      10 = UNPROVEN 沉底（虚拟环绕 / 耳机环绕 / 环绕增强 / Dolby / 音频增艳）
     隐藏的 ptitle 落在哪组都不可见，跟着组走即可。 */
  function sortSpaceRows(){
    var g = $('pg-space');
    if (!g || g.dataset.ssorted) return;
    g.dataset.ssorted = '1';
    var RANK = {
      'Enable_RFX_GFX': 1,
      'Enable_KEYSHIFT_GFX': 2, 'Enable_VOCALCANCEL_GFX': 2,
      'Enable_SMART_VOLUME_GFX': 3, 'GFSControl': 3,
      'Enable_AUDIOBRILLIANT_LFX': 10,
      'Enable_SS_LFX': 10, 'Enable_CONLAB_HPSPK_LFX': 10,
      'Enable_SURRSENS_LFX': 10, 'Enable_DHDVS_LFX': 10
    };
    var groups = [], cur = { rank: 0, els: [] };
    Array.prototype.forEach.call(g.children, function(el){
      var sw = el.querySelector ? el.querySelector('button.sw[data-p]') : null;
      var p = sw ? sw.dataset.p : null;
      if (p && RANK[p] !== undefined){
        groups.push(cur);
        cur = { rank: RANK[p], els: [el] };
      } else if (el.classList.contains('ptitle') && cur.rank !== 0){
        groups.push(cur);
        cur = { rank: 0, els: [el] };
      } else {
        cur.els.push(el);
      }
    });
    groups.push(cur);
    groups.sort(function(a, b){ return a.rank - b.rank; });
    groups.forEach(function(gr){
      gr.els.forEach(function(el){ g.appendChild(el); });
    });
    /* ★ 重排后第 3 个 ptitle（音效组分隔线）会成为孤儿线——不管落在
       「环境类型」后还是「智能音量」后，都与相邻行底线构成双线
       （扁平化布局里它已无分节职责）。直接隐藏。 */
    var pts = g.querySelectorAll('.ptitle');
    if (pts.length >= 3) pts[2].style.display = 'none';
  }

  /* ---------------- 5.5 扩展功能：静音 / 环境类型 / 变调 / 人声消除 / GFS
     这 5 组是本机 23 项可写参数里此前没有界面入口的部分。

     RFX_ENVIRONMENT 的索引语义已由静态反编译确认（2026-10-14 第三轮）：
       · CMEffectAPO_108B.dll 内 FUN_18021efd0 对 *(this+0x27C) 做 switch，
         case 0="Default"、1="Generic"、2="Paddedcell"…23="Underwater"、24="KTV"；
       · 原面板 Skin/FaceLift/apoSpeakerEM.cml 的 RadioButton Tag 与之吻合
         （Bathroom=4 / ConcertHall=8 / UnderWater=23 / Psychotic=26），
         即 Tag = 下拉位置（含被注释掉的 Default 项），0 号位 = 预设；
       · 本机 RFX_ENVIRONMENT 出厂值 = 1，按此映射即「一般环境」，合理。
     因此界面按 0..27 走：0=预设（原厂默认），1..23 为三重证据确认的名字，
     24..27（迷幻/晕眩/疯狂/KTV）取自原面板下拉定义，APO 调试表在 24 处
     标注为 KTV（存疑），故 24 之后仅作可选扩展。 */
  var ENV_FALLBACK = ["预设（原厂默认）", "一般环境", "吸音室", "卧房", "浴室",
    "客厅", "石室", "观众席", "音乐厅", "洞穴", "竞技场", "飞机棚",
    "地毯走廊", "走廊", "石头回廊", "胡同", "森林", "城市", "山中",
    "采石场", "平原", "停车场", "下水道", "水底",
    "迷幻", "晕眩", "疯狂", "KTV"];
  function bindExtras(){
    var env = $('envSel');
    if (env){
      var names = (ST && ST.environments && ST.environments.length)
        ? ST.environments : ENV_FALLBACK;
      env.innerHTML = names.map(function(n, i){
        return '<option value="' + i + '">' + n + '</option>';
      }).join('');
      env.addEventListener('change', function(){
        var v = +env.value;
        post({name:'RFX_ENVIRONMENT', value:v}).then(function(r){
          if (r.ok){
            if (PM['RFX_ENVIRONMENT']) PM['RFX_ENVIRONMENT'].v = r.value;
            toast('环境类型 → ' + (names[+r.value] || r.value));
          } else {
            toast('环境类型设置失败，请稍后重试', true);
            if (PM['RFX_ENVIRONMENT']) env.value = PM['RFX_ENVIRONMENT'].v;
          }
        });
      });
    }
    /* 旧版「GFS 模式 0–7」下拉已废弃：静态反编译确认 GFSControl 就是
       HKCU\SOFTWARE\C-Media\Hook\EnableGFS 这个 DWORD 的读写接口
       （osConfLib.dll 分发表 ID 0x10029 → FUN_180011ff0，内部只做
       RegQueryValueExW / RegSetValueExW），它是布尔开关而不是 8 档模式。
       ★ 第 46 轮更正：它**不是**「智能音量」（见 bindSwitches 上方的
         语义备忘），所以现在界面上不再给它专用入口 —— 参数页仍可读可写。
       这里兜底清掉残留的旧下拉。 */
    var gfs = $('gfsSel');
    if (gfs){
      var oldRow = gfs.closest('.row');
      if (oldRow) oldRow.remove(); else gfs.remove();
    }
    /* ★ 第 52 轮（H 档）：官方 apoSpeakerVol.cml（输出页）的**逐声道音量**。
       参数名带 "#<声道索引>" 后缀，由驱动层的 _pc() 拆成「真实属性名 + 声道
       索引」再调 PropertyControl —— 与其它滑杆共用同一条读 / 写 / 置灰路径，
       没有第二套机制。滑杆刻度 0..100 ↔ 驱动标量 0..1（scale = 100）。
       本机耳机端点只有声道 0/1 存在（2..7 读回 0x10004），所以只接两根。 */
    bindSlider('volLRange', 'volLVal', 'VolumeScalarControl#0', 'render',
               function(v){ return Math.round(v * 100) + '%'; }, null, null, 100);
    bindSlider('volRRange', 'volRVal', 'VolumeScalarControl#1', 'render',
               function(v){ return Math.round(v * 100) + '%'; }, null, null, 100);

    /* 平衡锁定 = 官方同页 FrontBalance 那个 LockButton（官方默认勾选）。
       ★ data-p 故意不写：它是纯界面开关，不映射任何驱动参数，所以不会被
         applyParams 的 button.sw[data-p] 同步循环误判成「死参数」置灰。
       锁定后：input 阶段把另一根拉到同刻度，change 阶段再把另一声道写一遍；
       解锁则两根完全独立。 */
    BAL_LOCK = true;
    var bl = $('balLock');
    /* ★ 2026-10-17（可发现性）：默认锁定是模仿官方 LockButton 的默认勾选，
       但实测用户会以为「左右声道没法单独控制」。三处补信号 ——
       ① 行标签常显「左右声道 · 已联动/独立」；② 锁定态下拖滑杆给一次性
          toast 指路；③ 开关 title 原本就有联动/独立文案。 */
    var balHinted = false;
    function setBalUi(){
      var nm = document.querySelector('#chVolStart .nm');
      if (nm) nm.textContent = '左右声道 · ' + (BAL_LOCK ? '平衡调节' : '独立调节');
    }
    setBalUi();
    if (bl){
      BAL_LOCK = bl.classList.contains('on');
      setBalUi();
      bl.title = '平衡锁定' + (BAL_LOCK ? ' · 左右声道联动' : ' · 左右声道独立');
      bl.addEventListener('click', function(){
        BAL_LOCK = !BAL_LOCK;
        bl.classList.toggle('on', BAL_LOCK);
        bl.title = '平衡锁定' + (BAL_LOCK ? ' · 左右声道联动' : ' · 左右声道独立');
        setBalUi();
        toast('平衡锁定 ' + (BAL_LOCK ? '已开启 · 左右联动' : '已关闭 · 左右独立'));
      });
    }
    [['volLRange', 'volRRange', 'volRVal', 'VolumeScalarControl#1'],
     ['volRRange', 'volLRange', 'volLVal', 'VolumeScalarControl#0']]
    .forEach(function(t){
      var el = $(t[0]);
      if (!el) return;
      el.addEventListener('input', function(){
        if (!BAL_LOCK) return;
        if (!balHinted){ balHinted = true;
          toast('平衡锁定中：两声道联动。点上方「平衡锁定」可分别调节'); }
        var o = $(t[1]);
        if (o) o.value = el.value;
        var ol = $(t[2]);
        if (ol) ol.textContent = Math.round(+el.value) + '%';
      });
      el.addEventListener('change', function(){
        if (!BAL_LOCK) return;
        var v = +el.value / 100;
        post({name: t[3], value: v}).then(function(r){
          if (r.ok && PM[t[3]]) PM[t[3]].v = r.value;
        });
      });
    });
    /* 变调 / 人声消除 / 增艳强度 滑杆：绝对值直接写。input 只刷读数，
       change（松手）才写驱动，避免拖动时连续写寄存器。
       ★ 第 47 轮起这份名单由 sliderCfgs() 统一给出：静态三根 + 声场页
         动态补的滑杆（addSpaceSlider 建的）走同一条路径，不会漏接同步。 */
    sliderCfgs()
    .forEach(function(cfg){
      var el = $(cfg[0]);
      if (!el) return;
      el.addEventListener('input', function(){
        var lab = $(cfg[1]);
        if (lab) lab.textContent = cfg[3](+el.value);
      });
      el.addEventListener('change', function(){
        var v = +el.value;
        post({name:cfg[2], value:v}).then(function(r){
          if (r.ok){
            if (PM[cfg[2]]) PM[cfg[2]].v = r.value;
            var lab = $(cfg[1]);
            if (lab) lab.textContent = cfg[3](+r.value);
            toast(cfg[2] + ' → ' + r.value);
          } else {
            toast(cfg[2] + ' 设置失败，请稍后重试', true);
            if (PM[cfg[2]] && PM[cfg[2]].v !== ''){
              el.value = PM[cfg[2]].v;
              var lab = $(cfg[1]);
              if (lab) lab.textContent = cfg[3](+PM[cfg[2]].v);
            }
          }
        });
      });
    });
  }

  /* ---------------- 5.7 三个旋钮接线（第 46 轮） ----------------
     设计稿里的 .dial 原先只是本地视觉 —— 拖拽 / 滚轮改数字、派发
     dialchange，但不写任何驱动参数。可它们标的偏偏正好是三个**有对应**
     的 Xear 音效：「音量」「动态低音」「语音清晰」。第 46 轮按官方页接上：
       vol   → VolumeScalarControl     标量音量 0..1（= 刻度 / 100）
       bass  → BM_Slider_BassLevel     官方 Xear Dynamic Bass 页，0..10
       clear → VOICECLARITY_LEVEL      官方 Xear Voice Clarity 页，0..6
     ★ 刻度是归一化的 0..100（paintDial 只认这个量程），写驱动前用
       to/from 双向换算，别把 0..6 直接塞进去。
     ★ 动态低音 / 语音清晰另有独立总开关（Enable_BM_GFX /
       Enable_VOICECLARITY_LFX）。旋钮拖离 0 时替用户打开一次、拖回 0
       关掉；状态记在 dialOn 里，避免高频抖动反复写开关。
     ★ 必须防抖：dialchange 在滚轮 / 拖拽时**每个刻度都派发**，而
       osConfLib 的写是同步 IO（属性存储 + 注册表），逐次写会打爆驱动。 */
  var DIAL_CFG = {
    vol:   {p: 'VolumeScalarControl', en: null,
            to:   function(t){ return Math.round(t) / 100; },
            from: function(v){ return Math.round(v * 100); }},
    bass:  {p: 'BM_Slider_BassLevel', en: 'Enable_BM_GFX',
            to:   function(t){ return Math.round(t / 10); },
            from: function(v){ return Math.round(v * 10); }},
    clear: {p: 'VOICECLARITY_LEVEL', en: 'Enable_VOICECLARITY_LFX',
            to:   function(t){ return Math.round(t * 6 / 100); },
            from: function(v){ return Math.round(v * 100 / 6); }}
  };
  var dialOn = {};          /* data-dial -> 上一次写过的开关状态 */
  var dialAt = {};          /* data-dial -> 防抖定时器 id */
  var DIAL_DEBOUNCE = 260;
  /* ★ 第 52 轮（H 档）：左右声道是否联动。纯界面状态，不落盘、不写驱动
     （驱动侧没有对应的「平衡锁定」参数 —— 官方那个是 .cml 里的
     LockButton，由面板自己维护）。 */
  var BAL_LOCK = true;

  function dialWrite(key, dial){
    var cfg = DIAL_CFG[key];
    if (!cfg) return;
    var raw = +dial.dataset.val;
    post({name: cfg.p, value: cfg.to(raw)}).then(function(r){
      if (r.ok){ if (PM[cfg.p]) PM[cfg.p].v = r.value; }
      else { toast('旋钮设置失败，请稍后重试', true); }
    });
    if (!cfg.en) return;
    var want = raw > 0 ? 1 : 0;
    if (dialOn[key] === want) return;
    dialOn[key] = want;
    post({name: cfg.en, value: want}).then(function(r){
      if (!r.ok){ dialOn[key] = want ? 0 : 1; return; }
      toast((want ? '已开启 · ' : '已关闭 · ')
            + (key === 'bass' ? '动态低音' : '语音清晰'));
    });
  }

  function bindDials(){
    var list = document.querySelectorAll('.dial[data-dial]');
    if (!list.length) return;
    Array.prototype.forEach.call(list, function(dial){
      var key = dial.dataset.dial;
      if (!DIAL_CFG[key]) return;
      if (DIAL_NOTE[key]) dial.title = DIAL_NOTE[key] + ' · '
        + (UNPROVEN_NOTE[DIAL_NOTE_P[key]] || UNPROVEN_TEXT);
      dial.addEventListener('dialchange', function(){
        if (dialAt[key]) clearTimeout(dialAt[key]);
        dialAt[key] = setTimeout(function(){
          dialAt[key] = null;
          dialWrite(key, dial);
        }, DIAL_DEBOUNCE);
      });
      dial.addEventListener('dialend', function(){
        if (dialAt[key]){ clearTimeout(dialAt[key]); dialAt[key] = null; }
        dialWrite(key, dial);
      });
    });
  }

  /* applyParams 阶段：把驱动读回的初始状态同步到扩展控件上 */
  function syncExtras(pm){
    [['MuteControl', '静音'],
     ['Enable_KEYSHIFT_GFX', '升降调'],
     ['Enable_VOCALCANCEL_GFX', '人声消除'],
     ['Enable_SMART_VOLUME_GFX', '智能音量']]
    .forEach(function(pair){
      var sw = document.querySelector('button.sw[data-p="' + pair[0] + '"]');
      if (!sw) return;
      var d = pm[pair[0]];
      if (d && d.r === 0) sw.classList.toggle('on', d.v === '1');
      else if (!d || !d.w){ sw.classList.remove('on'); dim(sw, pair[1] + ' · 当前设备不支持'); }
    });
    var p;
    p = pm['RFX_ENVIRONMENT'];
    var env = $('envSel');
    if (env){
      if (p && p.r === 0 && p.v !== '' && +p.v <= env.options.length - 1){
        env.value = p.v;
      } else if (!p || !p.w){
        /* ★ 第 48 轮改：原先这里判的是 `p.r !== 0`，于是**从未写过（ret=2）**的
           参数会被判成「本机不支持」并置灰 —— 与开关那条路（只在不可写时置灰）
           不一致，也违背项目红线：ret=2 只是「没写过」，写一次就 ret=0。
           现在只在**确实不可写**时置灰；r=2 时保持可点，让用户自己去写第一次。 */
        dim(env.parentNode, '环境类型 · 当前设备不支持');
      }
    }
    /* ★ 第 52 轮（H 档）：左右声道滑杆同步。与其它滑杆同一条路径 ——
       读不到就整行置灰（syncSlider 内部处理），不额外写第二套判据。 */
    syncSlider('volLRange', 'volLVal', 'VolumeScalarControl#0', pm,
               function(v){ return Math.round(v * 100) + '%'; }, 100);
    syncSlider('volRRange', 'volRVal', 'VolumeScalarControl#1', pm,
               function(v){ return Math.round(v * 100) + '%'; }, 100);
    /* B 档环绕族下拉（第 47 轮）：环绕模式 / 环绕房间。
       C 档 Dolby 三档（第 48 轮）：Dolby 方式 / 耳机空间 / 扬声器模式。
       这些 select 都是 bindSwitches 里动态建的，值就是驱动 Tag，直接对位。 */
    [['hrsModeSel', 'CONLAB_HPSPK_MODE', '环绕模式'],
     ['hrsRoomSel', 'CONLAB_HPSPK_ROOMSIZE', '环绕房间'],
     ['dhSel', 'DHDVS_USEDH', 'Dolby 方式'],
     ['dhHpSel', 'DH_Mode', 'Dolby 耳机空间'],
     ['dhVsSel', 'DVS_Mode', 'Dolby 扬声器模式']]
    .forEach(function(cfg){
      var el = $(cfg[0]);
      if (!el) return;
      var d = pm[cfg[1]];
      if (d && d.r === 0 && d.v !== '' && +d.v <= el.options.length - 1){
        el.value = d.v;
      } else if (!d || !d.w){
        dim(el.parentNode, cfg[2] + ' · 当前设备不支持');
      }
    });
    sliderCfgs()
    .forEach(function(cfg){
      var el = $(cfg[0]);
      if (!el) return;
      var d = pm[cfg[2]];
      if (d && d.r === 0 && d.v !== ''){
        el.value = d.v;
        var lab = $(cfg[1]);
        if (lab) lab.textContent = cfg[3](+d.v);
      } else if (!d || !d.w){
        /* ★ 第 48 轮改：判据由 `d.r !== 0` 改成 `!d.w`，理由同上面的下拉 ——
           ret=2（存在但从未写过）不该被当成本机不支持而锁死滑杆。
           新加的「声场扩展 / 扬声器角度」两根滑杆出厂就是 ret=2，
           照老判据会一上来就被按死。 */
        dim(el.parentNode, cfg[2] + ' · 当前设备不支持');
      }
    });

    /* 声道配置 chips（第 50 轮 / E 档）。
       写入值是 Tag 本身（2/4/6/8），所以这里也**不做任何换算**：直接比对数值。
       置灰判据照第 48 轮的红线 —— 一律用 `!d.w`，**不许**用 `r !== 0`：
       Speaker_Channel_Setting 在本机是 ret=0，但换一台机器完全可能是 ret=2
       （存在、可写、只是没人写过），照 r 判会把它一上来就锁死。 */
    var cbox = $('chChips');
    if (cbox){
      var dch = pm['Speaker_Channel_Setting'];
      if (dch && dch.w){
        var cur = (dch.v === '' || dch.v === null) ? null : +dch.v;
        cbox.querySelectorAll('.chip[data-ch]').forEach(function(x){
          x.classList.toggle('on', cur !== null && +x.dataset.ch === cur);
        });
        cbox.title = '当前：' + (cur === null ? '未知' : chanLabel(cur));
      } else {
        dim(cbox, '声道配置 · 当前设备不支持');
      }
    }
    /* ★ 2026-10-19 深夜：房间大小滑杆补同步 —— 用户报障「场景里设置的
       数值，切到对应页面 UI 还保持默认」的最后一处实锤。roomRange 只在
       首帧 bindRoom(status) 里设过一次值，applyParams 的同步清单一直
       没有它：应用场景后 RFX_ROOMSIZE 变了，滑杆与读数却停在旧位置。
       与 bindRoom 用同一张映射表（0/1/2 档 → 画布百分比 72/100/124），
       回填时同步 S.room 与 LAST.room，防止 render 循环误判「用户改过」。 */
    var rroom = pm['RFX_ROOMSIZE'];
    var rrr = $('roomRange');
    if (rrr && rroom && rroom.r === 0 && rroom.v !== ''){
      var rstep = Math.max(0, Math.min(2, parseInt(rroom.v, 10) || 0));
      var rmap = [72, 100, 124];
      rrr.value = rmap[rstep];
      if (typeof S !== 'undefined' && S) S.room = rmap[rstep];
      if (typeof LAST !== 'undefined' && LAST) LAST.room = rstep;
      var rrv = $('roomVal');
      if (rrv) rrv.textContent = (typeof roomLabel === 'function')
        ? roomLabel(rstep) : String(rstep);
      rrr.removeAttribute('data-dimmed');
      rrr.style.opacity = '';
      rrr.style.pointerEvents = '';
    } else if (rrr && rroom && !rroom.w){
      dim(rrr.parentNode, '房间大小 · 当前设备不支持');
    }
    /* ★ 第 53 轮（I 档）：Dolby 组的前置条件门控放在最后 ——
       它依赖上面那块 Speaker_Channel_Setting 的读回值（同一个 pm）。 */
    dolbyGate(pm);
  }

  /* 把驱动读回的三个旋钮参数刷到旋钮刻度上（0..100 归一化）
     ★ 第 47 轮：这里同时负责**点亮 / 置灰**。判据只看驱动读得回读不回
       （d.r === 0），不再是写死的「本机没实现」—— 第 46 轮接参数那天，
       bindVolume 里遗留的硬置灰把刚接好的两个旋钮按死了，本函数看不出来。 */
  function syncDials(pm){
    var list = document.querySelectorAll('.dial[data-dial]');
    Array.prototype.forEach.call(list, function(dial){
      var key = dial.dataset.dial, cfg = DIAL_CFG[key];
      if (!cfg) return;
      var d = pm[cfg.p];
      /* ★ 2026-10-17（用户拍板「默认值可交互」）：三态判据 ——
         ① !d.w → 置灰；② 读得到 → 驱动真值；③ w 但读不到（ret=2）
         → 保留设计稿默认刻度、可交互，title 如实标注。
         ret=2 只是「端点还没写过值」（铁律 2），端点重建后旋钮不再
         大面积「·不可用」。 */
      if (d && !d.w){
        dim(dial, cfg.p + ' · 本机不支持');
        return;
      }
      undim(dial);
      var tick = (d && d.r === 0 && d.v !== '') ? +cfg.from(+d.v) : NaN;
      /* ★ 2026-10-18：UNPROVEN 旋钮（动态低音/语音清晰）在操作提示后
         追加统一悬停句——不覆盖，否则 bindDials 设的提示会被冲掉。 */
      var dialTail = DIAL_NOTE[dial.dataset.dial]
        ? ' · ' + (UNPROVEN_NOTE[DIAL_NOTE_P[dial.dataset.dial]] || UNPROVEN_TEXT)
        : '';
      if (isFinite(tick)){
        tick = Math.max(0, Math.min(100, tick));
        dial.dataset.val = tick;
        dial.title = cfg.p + ' · 上下拖拽或滚轮调节' + dialTail;
      } else {
        /* 2026-10-20（用户拍板）：动态低音/语音清晰读不到驱动记忆值时
           默认刻度 45（两参数 r=2 读不回，设计稿刻度就是初始位置）。 */
        if (key === 'bass' || key === 'clear') dial.dataset.val = 45;
        dial.title = cfg.p + ' · 默认值（驱动暂无记忆值，调节后生效）' + dialTail;
      }
      if (typeof paintDial === 'function') paintDial(dial);
      if (cfg.en){
        var e = pm[cfg.en];
        dialOn[key] = (e && e.v === '1') ? 1 : 0;
      }
    });
  }

  /* ================= 7.1 虚拟音箱摆位（第 49 轮 D 档） =================
     官方 apoSpeakerVirtual.cml 那张页的控件是：
       开关(Enable_SS_LFX) + 4 个模式单选钮 + Reset + 8 个可拖拽音箱 + 8 根只读 dB 条
     页面上**没有角度/距离滑杆** —— 角度距离全靠拖动图标改。所以本面板也不做滑杆，
     直接让设计稿的声场图可拖（vssRotate / vssReset / stage 拖拽都在设计稿里）。

     ★★ 那 4 个模式实测**不是驱动属性**：`__Manual` / `__Clockwise` /
       `__CounterClockwise` / `__WiseArround` 原始读取一律 ret=-1（属性名未登记）；
       同块的 `SpeakerShifterControl` 虽是属性（ret=0）但载荷恒为全 0，
       写 0…6 八个 `SS_*_ANGLE` 一个都不动 ⟹ 它们是纯界面动作，
       永远不登记进写白名单，只在界面上改状态、再把结果写回 `SS_*_ANGLE`。

     16 个真参数（8 角度 + 8 距离）的值全部取自官方安装态的 --dump 快照：
       LF=-32 C=0 RF=32 RS=90 RB=148 LB=-148 LS=-90 LFE=180，距离全 0。  */
  var VSS_DEBOUNCE = 260, vssTimer = 0, VSS_LAST = {}, VSS_PEND = {};

  function vssSpk(i){
    return (typeof S !== 'undefined' && S.spk && S.spk[i]) ? S.spk[i] : null;
  }
  function vssWrap(a){
    while (a > 180)  a -= 360;
    while (a < -180) a += 360;
    return Math.round(a);
  }
  function vssSet(name, val){
    VSS_PEND[name] = val;
    post({name: name, value: val}).then(function(r){
      delete VSS_PEND[name];
      if (r && r.ok){
        VSS_LAST[name] = val;
      } else {
        VSS_LAST[name] = undefined;          /* 允许下次重试 */
        /* 第 60 轮：用户 toast 不再暴露 ret= 码（那是给日志的）；
           只有后端给了可读 error 才带上，方便报障时口述。 */
        toast(name + ' 设置失败，已恢复原值'
              + (r && r.error ? '（' + r.error + '）' : ''), true);
        /* ★ 回滚必须问驱动要真值，不能拿我们记的旧值凑 ——
           驱动可能整批接受、也可能只拒这一项。 */
        if (window.__resync) window.__resync();
      }
    });
  }
  /* ★★ 把界面状态**立刻**快照进 VSS_PEND，不能等防抖到点再读 S.spk。
     第 49 轮的教训：任何人在这 260ms 内调一次 __resync()（老代码里
     「松手立刻写角度 + 成功后重拉参数」就是这么干的），syncSpk 就会用
     驱动里的旧值把 s.dist 覆盖回 0，等定时器到点写出去的已是 0。
     快照 + VSS_PEND 双保险：syncSpk 见到挂起的名字就跳过，不覆盖界面。 */
  function vssSnapshot(i){
    var s = vssSpk(i);
    if (!s || !s.key) return;
    VSS_PEND[s.key + '_ANGLE'] = vssWrap(Math.round(s.ang));
    VSS_PEND[s.key + '_DIST']  = Math.round(s.dist || 0);
  }
  function vssFlush(){
    Object.keys(VSS_PEND).forEach(function(name){
      var v = VSS_PEND[name];
      if (VSS_LAST[name] !== v) vssSet(name, v);
      else delete VSS_PEND[name];            /* 与驱动现值一致，无须打扰驱动 */
    });
  }
  /* 设计稿的拖拽松手 / 旋转 / 重置都调这两个钩子。
     防抖 260 ms：拖拽与连点旋转会连续触发，驱动写是同步 IO。 */
  window.__vssCommit = function(i){
    vssSnapshot(i);
    if (vssTimer) clearTimeout(vssTimer);
    vssTimer = setTimeout(function(){ vssTimer = 0; vssFlush(); }, VSS_DEBOUNCE);
  };
  window.__vssCommitAll = function(){
    var n = (typeof S !== 'undefined' && S.spk) ? S.spk.length : 0;
    for (var k = 0; k < n; k++) vssSnapshot(k);
    if (vssTimer) clearTimeout(vssTimer);
    vssTimer = setTimeout(function(){ vssTimer = 0; vssFlush(); }, VSS_DEBOUNCE);
  };

  /* 把驱动读回的 16 项刷到声场图 + 读数区。
     ★ 读得到的才覆盖：读不回（ret!=0）说明这项本机没有值，保持界面原样，
       绝不因为读不到就把音箱锁死 —— ret=2 的语义是「存在但从没写过」。
     ★ VSS_PEND 里挂着的名字跳过：那是刚拖完、还没写下去的值，
       用驱动的旧值覆盖它就会把用户刚做的操作吃掉（见 vssSnapshot 的注释）。 */
  function syncSpk(pm){
    if (typeof S === 'undefined' || !S.spk) return;
    var ch = false;
    S.spk.forEach(function(s){
      var na = s.key + '_ANGLE', nd = s.key + '_DIST';
      var a = pm[na], d = pm[nd];
      if (a && a.r === 0 && a.v !== '' && VSS_PEND[na] === undefined){
        var av = Math.round(+a.v);
        if (isFinite(av)){ s.ang = av; VSS_LAST[na] = av; ch = true; }
      }
      if (d && d.r === 0 && d.v !== '' && VSS_PEND[nd] === undefined){
        var dv = Math.round(+d.v);
        if (isFinite(dv)){ s.dist = dv; VSS_LAST[nd] = dv; ch = true; }
      }
    });
    if (ch && typeof render === 'function'){ render(); hud(); }
  }

  /* 模式按钮 / 旋转 / 重置的点击绑定。这些**不写驱动**（见上），
     只改设计稿里的界面状态；真正的写回由 __vssCommitAll 统一做。 */
  function bindVss(){
    var mr = $('vssModeRot'), mm = $('vssModeMove'), hint = $('vssHint');
    function modeText(m){
      /* ★ 2026-10-18：底部说明按官方词条（apoHeadPhoneVirtual.xaml）的
         用语改写 —— 官方把拖拽语义表达为「音量减弱 / 音量增强」（8 根
         dB 条的标签），聆听者而非听者。工程细节（±100 刻度 / 驱动不钳位）
         留在源码注释里，界面上不再堆给用户。 */
      return (m === 'move')
        ? '手动平移：拖动音箱推远 / 拉近——距离越远音量减弱，越近音量增强'
        : '手动旋转：拖动音箱绕聆听者转动，调整环绕方位';
    }
    function setM(m){
      if (typeof vssMode === 'function') vssMode(m);
      if (mr) mr.classList.toggle('on', m === 'rot');
      if (mm) mm.classList.toggle('on', m === 'move');
      if (hint) hint.textContent = modeText(m);
    }
    if (mr) mr.addEventListener('click', function(){ setM('rot');  });
    if (mm) mm.addEventListener('click', function(){ setM('move'); });
    if (hint) hint.textContent = modeText('rot');
    var cw = $('vssCW'), ccw = $('vssCCW'), rs = $('vssReset');
    if (cw  && typeof vssRotate === 'function') cw.addEventListener('click',
      function(){ vssRotate(1);  });
    if (ccw && typeof vssRotate === 'function') ccw.addEventListener('click',
      function(){ vssRotate(-1); });
    if (rs  && typeof vssReset  === 'function') rs.addEventListener('click',
      function(){ vssReset(); });
  }

  /* 声场页动态补「虚拟音箱摆位」编辑器（第 49 轮）。
     ★ 2026-10-18 重设计（用户：旧的三颗文字按钮有点丑）：原来的
       「两个 chip + 一排三个文字按钮」合并成一条工具条 ——
       左边是「旋转 | 平移」分段选择器（pill 胶囊、accent 填充激活态），
       右边是三颗图标圆钮（逆时钟 ↺ / 顺时钟 ↻ / 重置 ⟲，lucide 线性
       图标，title 带说明）。id 全部不变（bindVss 绑定与去重守卫依赖），
       模式切换仍走 vssMode()，图标钮仍走 vssRotate()/vssReset()。 */
  function addSpaceVssEditor(){
    var g = $('pg-space');
    if (!g || $('vssModeRow')) return;
    var IC = {
      ccw: '<svg viewBox="0 0 24 24"><path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/></svg>',
      cw:  '<svg viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/></svg>',
      rst: '<svg viewBox="0 0 24 24"><path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5 5.5 5.5 0 0 1-5.5 5.5H11"/></svg>'
    };
    var mode = document.createElement('div');
    mode.className = 'vss-tools';
    mode.id = 'vssModeRow';
    mode.innerHTML = '<div class="seg" role="group" aria-label="摆位交互模式">'
      + '<button type="button" class="seg-b on" id="vssModeRot" title="拖动音箱绕聆听者转动，调整环绕方位（写 SS_*_ANGLE）">手动旋转</button>'
      + '<button type="button" class="seg-b" id="vssModeMove" title="拖动音箱推远 / 拉近，改变该声道音量强弱（写 SS_*_DIST）">手动平移</button>'
      + '</div>'
      + '<span class="vss-sep"></span>'
      + '<button type="button" class="ibtn" id="vssCCW" title="逆时钟环绕（每次 15°）">' + IC.ccw + '</button>'
      + '<button type="button" class="ibtn" id="vssCW" title="顺时钟环绕（每次 15°）">' + IC.cw + '</button>'
      + '<button type="button" class="ibtn" id="vssReset" title="重置为官方默认摆位">' + IC.rst + '</button>';
    var hint = document.createElement('div');
    hint.id = 'vssHint';
    hint.style.cssText = 'font-size:10.5px;color:var(--t3);line-height:1.55;margin-top:7px';
    var anchor = $('spFxStart');
    [mode, hint].forEach(function(el){
      if (anchor && anchor.parentNode === g) g.insertBefore(el, anchor);
      else g.appendChild(el);
    });
  }

  /* 声场页动态补一个开关。
     ★ 第 30 轮改成插入到 #spFxStart 之前，而不是 appendChild ——
     声场页末尾现在有一组从均衡页挪过来的音效开关，appendChild 会把
     「虚拟环绕 / 环境音效」排到它们后面，跟同属环境类的「环境类型」
     隔断。锚在 #spFxStart（那条 border-top 所在的行）之前，
     两次调用天然保序：第二次插在第一次的后面。 */
  /* 静态滑杆（设计稿里就写好的）+ 动态补的滑杆，统一一份配置。
     第 46 轮补 AUDIOBRILLIANT_LEVEL：官方 Xear Audio Brilliant 页除开关外
     还有一根 Brilliant强度 滑杆（0~6），此前只有开关、强度没入口。 */
  var EXTRA_SLIDERS = [
    ['ksRange', 'ksVal', 'KEYSHIFT_LEVEL', function(v){ return (v > 0 ? '+' : '') + v; }],
    ['vcRange', 'vcVal', 'VOCALCANCEL_LEVEL', function(v){ return v; }],
    ['brRange', 'brVal', 'AUDIOBRILLIANT_LEVEL', function(v){ return v; }]
  ];
  var SPACE_SLIDERS = [];

  function sliderCfgs(){ return EXTRA_SLIDERS.concat(SPACE_SLIDERS); }

  function addSpaceSwitch(param, name, sub){
    var g = $('pg-space');
    if (!g || g.querySelector('button.sw[data-p="' + param + '"]')) return;
    var row = document.createElement('div');
    row.className = 'row';
    /* 第 62 轮：行内说明全部取消（副标接过说明职责），sub 传空即不渲染 */
    row.innerHTML = '<div><div class="nm">' + name + '</div>'
      + (sub ? '<div class="sub">' + sub + '</div>' : '')
      + '</div>'
      + '<button class="sw" data-p="' + param + '" data-label="' + name + '"></button>';
    var anchor = $('spFxStart');
    if (anchor && anchor.parentNode === g) g.insertBefore(row, anchor);
    else g.appendChild(row);
  }

  /* 声场页动态补一个下拉（第 47 轮 B 档 · 环绕族）。
     与 addSpaceSwitch 共用同一个锚点，所以插入顺序 = 调用顺序；
     选项值直接用下标（0/1/2），正好等于驱动里的 Tag，无需再换算。
     建完立刻交给统一的 bindSelect，选完即写、失败回滚。 */
  function addSpaceSelect(id, param, name, sub, options){
    var g = $('pg-space');
    if (!g || $(id)) return;
    var row = document.createElement('div');
    /* 第 59 轮：动态下拉都跟在某个开关后面（环绕模式/房间跟「耳机环绕」、
       Dolby 三项跟「Dolby 环绕」），按静态滑杆同一口径加 .ctl-sub 缩进。 */
    row.className = 'row ctl-sub';
    var cap = document.createElement('div');
    /* 第 62 轮：同 addSpaceSwitch，sub 传空即不渲染 */
    cap.innerHTML = '<div class="nm">' + name + '</div>'
      + (sub ? '<div class="sub">' + sub + '</div>' : '');
    var sel = document.createElement('select');
    sel.id = id;
    sel.style.minWidth = '92px';
    sel.innerHTML = options.map(function(n, i){
      return '<option value="' + i + '">' + n + '</option>';
    }).join('');
    row.appendChild(cap);
    row.appendChild(sel);
    var anchor = $('spFxStart');
    if (anchor && anchor.parentNode === g) g.insertBefore(row, anchor);
    else g.appendChild(row);
    bindSelect(id, param, 'render', options, name);
  }

  /* 声场页动态补一根滑杆（第 47 轮 B 档）。
     值域由调用方从官方 .cml 的 Slider Minimum/Maximum 抄进来，**不是猜的**；
     建完把配置推进 SPACE_SLIDERS，绑定/同步交给 bindExtras / syncExtras。 */
  function addSpaceSlider(id, param, label, min, max, step){
    var g = $('pg-space');
    if (!g || $(id)) return;
    var vid = id + 'Val';
    var row = document.createElement('div');
    /* 第 59 轮：动态滑杆（声场扩展/扬声器角度）跟在「耳机环绕」开关后面，
       同静态滑杆口径加 .ctl-sub。 */
    row.className = 'slider ctl-sub';
    row.innerHTML =
      '<span style="font-size:11.5px;color:var(--t2);white-space:nowrap">'
      + label + '</span>'
      + '<input type="range" id="' + id + '" min="' + min + '" max="' + max
      + '" step="' + step + '" value="' + min + '">'
      + '<span class="v" id="' + vid + '">' + min + '</span>';
    var anchor = $('spFxStart');
    if (anchor && anchor.parentNode === g) g.insertBefore(row, anchor);
    else g.appendChild(row);
    SPACE_SLIDERS.push([id, vid, param, function(v){ return v; }]);
  }

  /* ---------- 5.7 Dolby 的声道前置条件（第 53 轮 I 档） ----------
     ---- 取证（i_probe3.py）----
     Skin/FaceLift/apoSpeakerDolbyHPVS.cml 尾部原文：
       <Canvas Width="643" Height="455" Name="WarningContent" Visibility="Hidden">
         <Button Name="ChannelsSetting" Style="ClickButton" Text="ml_SpeakerDolbyHPVS_ChannelsSetting"/>
     词条 lang/zh-CN/apoSpeakerDolbyHPVS.xaml：
       ChannelRequirement =「Dolby Surround 效果 只支持喇叭组态为立体声(2声道),
                             请按下方按钮至喇叭设定页面调整声道设定」
       ChannelsSetting    =「频道设定」
     ⟹ 官方在**喇叭组态 ≠ 立体声**时，把该页内容换成这条警告 + 一枚跳转按钮。

     ---- 粒度换算 ----
     官方那一页就一个功能，所以「换页」= 把整页内容换掉；本面板这一组是嵌在
     「声音」页里的，所以只隐藏 Dolby 这 4 行（总开关 / 方式 / 耳机空间 /
     扬声器模式），不动其它组。
     ---- 失败方向是**放行** ----
     读不到（驱动未连接）或不可写时不门控 —— 与置灰那条红线同源：
     拿不到真值就不要替驱动下结论（ret=2 ≠ 不支持）。
     ---- 取值口径 ----
     Speaker_Channel_Setting 的写入值就是声道数本身（2/4/6/8，E 档实测），
     所以这里只比一个数，不做任何换算。 */
  function addDolbyWarn(){
    var g = $('pg-space');
    if (!g || $('dolbyChWarn')) return;
    var box = document.createElement('div');
    box.className = 'warn';
    box.id = 'dolbyChWarn';
    box.hidden = true;
    box.innerHTML =
      '<div>'
      + '<div class="wt">Dolby Surround 效果只支持立体声（2 声道）</div>'
      + '<div class="ws">官方要求喇叭组态为立体声。点下面「频道设定」到「设备 → 声道配置」改过来，再回到这里。</div>'
      + '</div>'
      + '<button class="btn" id="dolbyChBtn">频道设定</button>';
    var anchor = $('spFxStart');
    if (anchor && anchor.parentNode === g) g.insertBefore(box, anchor);
    else g.appendChild(box);
    var btn = $('dolbyChBtn');
    if (btn){
      btn.addEventListener('click', function(){
        /* 复用 dock 的原生点击路径（标题 / VIS.setActive / 布局重算都在那里），
           不另写一份切页逻辑 —— 否则就是「同一个动作两套实现」这个老坑。 */
        var b = document.querySelector('#dock button[data-s="dev"]');
        if (b) b.click();
        /* 顺手把「声道配置」滚进视野：官方那边点一下也是直接落到设定页 */
        setTimeout(function(){
          var c = $('chChips');
          if (c && c.scrollIntoView) c.scrollIntoView({block: 'center'});
        }, 90);
      });
    }
  }

  /* Dolby 组的门控。pm = /api/params 的**渲染端点**表。
     判据只有一条：Speaker_Channel_Setting == 2。 */
  function dolbyGate(pm){
    var warn = $('dolbyChWarn');
    if (!warn) return;
    var d = pm && pm['Speaker_Channel_Setting'];
    var cur = (d && d.w && d.v !== '' && d.v !== null) ? +d.v : null;
    var ok = (cur === null) || (cur === 2);
    var rows = [];
    var sw = document.querySelector('button.sw[data-p="Enable_DHDVS_LFX"]');
    if (sw && sw.closest('.row')) rows.push(sw.closest('.row'));
    ['dhSel', 'dhHpSel', 'dhVsSel'].forEach(function(id){
      var el = $(id);
      if (el && el.closest('.row')) rows.push(el.closest('.row'));
    });
    rows.forEach(function(r){ r.style.display = ok ? '' : 'none'; });
    warn.hidden = ok;
    if (!ok){
      var t = warn.querySelector('.wt');
      if (t) t.textContent = 'Dolby Surround 效果只支持立体声（2 声道）—— 当前是 '
        + (chanLabel(cur) || (cur + ' 声道'));
    }
  }

  /* ---------------- 5.6 麦克风页（采集端点） ----------------
     麦克风侧参数只在采集端点上存在。早期实现只在渲染端点查，
     于是整页被误判成「本机驱动未向应用层开放麦克风处理参数」——
     那句话是错的，参数一直是开放的，只是问错了端点。 */
  function bindMicPanel(st){
    var mv = $('mvSel');
    if (mv){
      var names = (st.magic_voices && st.magic_voices.length)
        ? st.magic_voices : ['怪兽', '卡通', '男人', '女人'];
      mv.innerHTML = names.map(function(n, i){
        return '<option value="' + i + '">' + n + '</option>';
      }).join('');
      bindSelect('mvSel', 'MagicVoice_Selection', 'capture', names, '魔音类型');
    }
    var rs = $('micRateSel');
    if (rs){
      rs.innerHTML = [44100, 48000].map(function(v){
        return '<option value="' + v + '">' + (v / 1000) + ' kHz</option>';
      }).join('');
      bindSelect('micRateSel', 'DevSampleRateControl', 'capture',
                 {'44100': '44.1 kHz', '48000': '48 kHz'}, '采集采样率');
    }
    bindSlider('micVolRange', 'micVolVal', 'VolumeScalarControl', 'capture',
               function(v){ return Math.round(v * 100) + '%'; }, null, null, 100);
    bindSlider('nrRange', 'nrVal', 'NoiseReduction_Level', 'capture', null);
    bindSlider('echoRange', 'echoVal', 'MICECHO_Level', 'capture',
               function(v){ return (v > 0 ? '+' : '') + v; });
    bindSlider('aaRange', 'aaVal', 'AAVolumeControl', 'capture',
               function(v){ return v + ' dB'; });

    /* 麦克风监听：2026-10-20 起为内嵌 WASAPI 监听（POST /api/listen
       {on}）。旧的 RunListen2Device 官方路径依赖的 COM 服务器驱动包里
       没有（官方原版同样开不了），取证见内部报告（未随仓库分发） */
    var ls = $('listenSw');
    if (ls){
      ls.addEventListener('click', function(){
        if (ls.style.pointerEvents === 'none') return;
        var on = !ls.classList.contains('on');
        ls.classList.toggle('on', on);
        postRaw('/api/listen', {on: on}).then(function(r){
          if (r.ok){
            toast('麦克风监听' + (on ? ' 已开启' : ' 已关闭'));
          } else {
            ls.classList.toggle('on', !on);
            toast('麦克风监听设置失败，请稍后重试', true);
          }
        });
      });
    }
  }

  /* ---------------- 5.7 设备页扩展：采样率 / 声道配置 / 声道测试 / ASIO ---------------- */

  /* E 档（第 50 轮）：官方「设定」页的声道单选。
     apoSpeakerSetting.cml 里 Channels2/4/6/8 的 **Tag 就是声道数本身**
     （2 / 4 / 6 / 8），所以选项值与 Tag 一一对位，没有任何下标换算 ——
     这是本档唯一一处「驱动值 = 界面值」的控件，别照 D 档 RoomSize 那种反序去套。
     官方中文取自 lang/zh-CN/apoSpeakerSetting.xaml：
       ml_SpeakerSetting_2Channels      立体声
       ml_SpeakerSetting_4Channels      四声道
       ml_SpeakerSetting_5dot1Channels  5.1 环场音效
       ml_SpeakerSetting_7dot1Channels  7.1 环场音效 */
  var CHANNEL_OPTS = [[2, '立体声'], [4, '四声道'],
                      [6, '5.1 环场音效'], [8, '7.1 环场音效']];
  function chanLabel(v){
    for (var i = 0; i < CHANNEL_OPTS.length; i++){
      if (CHANNEL_OPTS[i][0] === +v) return CHANNEL_OPTS[i][1];
    }
    return v + ' 声道';
  }

  function bindDevExtras(st){
    var rs = $('devRateSel');
    if (rs){
      rs.innerHTML = [44100, 48000].map(function(v){
        return '<option value="' + v + '">' + (v / 1000) + ' kHz</option>';
      }).join('');
      bindSelect('devRateSel', 'DevSampleRateControl', 'render',
                 {'44100': '44.1 kHz', '48000': '48 kHz'}, '输出采样率');
    }

    var box = $('testChips');
    if (box){
      var chans = st.test_channels || [];
      /* 第四项是测试音 wav 文件名（第 28 轮加的）。挂进 tooltip：
         万一以后某一档不响，界面上能直接看出它在找哪个文件。 */
      box.innerHTML = chans.map(function(c){
        return '<button class="chip" data-mask="' + c[1] + '" title="'
          + c[0] + ' · 掩码 ' + c[1] + (c[3] ? ' · ' + c[3] : '')
          + '">' + c[2] + '</button>';
      }).join('');
      box.addEventListener('click', function(e){
        var c = e.target.closest('.chip[data-mask]');
        if (!c) return;
        box.querySelectorAll('.chip').forEach(function(x){
          x.classList.toggle('on', x === c);
        });
        postRaw('/api/test', {action: 'play', mask: +c.dataset.mask})
          .then(function(r){
            /* 失败时优先用后端给的原因（「找不到测试音资源（…）」/
               「驱动拒绝了这次调用（0x…）」），拿不到再退回错误码 ——
               只甩一个 65537 给用户看是查不出问题的。 */
            toast(r.ok ? ('正在播放 ' + c.textContent)
                       : ('声道测试不可用 · ' + (r.error || r.ret)), !r.ok);
          });
      });
    }
    /* 声道配置（第 50 轮 / E 档）。
       ★ 为什么**没有**「会中断播放」确认框 —— 这一条是本次实测推翻的旧结论：
         原先记的是「写 Speaker_Channel_Setting 会让端点重初始化、活动流当场
         AUDCLNT_E_DEVICE_INVALIDATED」。第 50 轮把当年那个脚本拆开逐项归因
         （.workbuddy-ai/tmp/e_stream_kill2.py）后发现：
           · 当年 stream_kill.py 在 t=5.0s 把 Enable_SS_LFX=0 **和**
             Speaker_Channel_Setting=2 捆在同一次写入，归因本身就是混的；
           · 逐项单测：写 2→8→6→2 期间渲染流**一路在写**、无 err；
           · 真凶是另一时间片的 XearSurround71Control=1（hr=0x88890004）。
         又量了端点混音格式：设置 2/4/6/8 时新开流都是 48000 Hz / 8 声道，
         即该设置**不改变流格式**，重初始化无从发生。
         ⟹ 按普通设置项处理；它是语义性变更（声道路由），不是会打断播放的操作，
           所以只留一行静态说明，不弹框。 */
    var cbox = $('chChips');
    if (cbox){
      cbox.innerHTML = CHANNEL_OPTS.map(function(o){
        return '<button class="chip" data-ch="' + o[0] + '" title="'
          + o[1] + '（写入 ' + o[0] + '）">' + o[1] + '</button>';
      }).join('');
      cbox.addEventListener('click', function(e){
        var c = e.target.closest('.chip[data-ch]');
        if (!c || c.classList.contains('on')) return;
        var v = +c.dataset.ch;
        cbox.querySelectorAll('.chip').forEach(function(x){
          x.classList.toggle('on', x === c);
        });
        post({name: 'Speaker_Channel_Setting', value: v}).then(function(r){
          if (r && r.ok){
            toast('声道配置已改为 ' + chanLabel(v));
            /* ★ 第 53 轮：Dolby 组的门控跟着这个值走，写完立刻重判一次，
               不等下一次轮询（否则会看到「警告已成立、Dolby 行却还在」）。 */
            if (PM['Speaker_Channel_Setting']) PM['Speaker_Channel_Setting'].v = String(v);
            dolbyGate(PM);
          } else {
            /* 失败不靠我们记的旧值回滚 —— 向驱动要真值，见 D 档那次的教训 */
            toast('声道配置写入失败（' + ((r && (r.error || r.ret)) || '?')
                  + '）已按驱动实际值回滚', true);
            if (window.__resync) window.__resync();
          }
        });
      });
    }
    var stp = $('testStop');
    if (stp){
      stp.addEventListener('click', function(){
        var box2 = $('testChips');
        if (box2) box2.querySelectorAll('.chip').forEach(function(x){
          x.classList.remove('on');
        });
        postRaw('/api/test', {action: 'stop'}).then(function(r){
          toast(r.ok ? '已停止声道测试' : '停止失败，请稍后重试', !r.ok);
        });
      });
    }
    var asio = $('asioBtn');
    if (asio){
      asio.addEventListener('click', function(){
        postRaw('/api/asio', {}).then(function(r){
          toast(r.ok ? 'ASIO 面板已打开'
                     : '当前设备不支持 ASIO', !r.ok);
        });
      });
    }
  }

  /* ---------------- 5.8 自定义场景（官方 Profile 系统） ----------------
     一份 .profile（参数快照）+ 一份 .cml（列表项），格式与
     CPL/profile/* 完全一致，可与官方面板互相阅读。

     ★ 第二十一轮改版：「应用」和「保存」分进了两个空间 ——
       中央场景墙（设计稿的 renderProfile()，数据来自这里的 PROFILES）
       只做「点一下整套切过去」；右侧面板只做管理：状态与脏标记 /
       场景详情 / 新建 · 覆盖 · 改名 · 删除。
       旧版底部那排「原厂默认 / 游戏 / 音乐 / 电影」chip 其实全部是
       「把当前状态存成它」的保存动作，却长得像场景名，已删除。

     ★ 只展示真快照：官方档一律隐藏 —— 「预设」只是本机驱动拒收的
       恢复出厂指令、「HiFi」是 0 参数空壳，都没有快照意义。
       取而代之的是启动时自动生成的自建「原厂默认」与
       游戏 / 音乐 / 电影预置场景（后端 EnsureSeedScenes 生成）。 */
  var PROFILES = {
    list: [], dir: '', writable: false,
    active: '',              /* 最近一次成功应用的场景（本地记忆） */
    dirty: false,            /* 当前驱动状态是否已偏离 active */
    dead: {}, deadWhy: {},   /* 应用失败过的场景 → 原因 */
    ref: null,               /* active 的条目（pairs:{…}）—— 脏标记只借它的**键名** */
    base: null,              /* 基线：应用/保存那一刻的**驱动实读值** {键: 归一化值} */
    detail: null, detailFor: '', detailErr: '',
    mode: 'new', editName: '' /* 表单当前是「新建」还是「重命名」 */
  };

  /* 归一化：驱动读回来的 v 可能是 true/false/数字，组态文件里存的永远是
     文本（"1" / "0" / "4" / "-13.5"）。不归一化就会满屏假脏。 */
  function profNorm(v){
    if (v === true) return '1';
    if (v === false) return '0';
    if (v === null || v === undefined) return '';
    if (v !== '' && isFinite(Number(v))) return String(Math.round(Number(v) * 1000) / 1000);
    return String(v).trim();
  }
  function profParams(ep){ return ep === 'capture' ? PMC : PM; }

  function pesc(s){
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  /* active（「使用中 · X」）不再用 localStorage：那是浏览器本地记忆，
     与设备无关、跨驱动重装残留 —— 会显示撒谎（2026-10-20 用户实测）。
     现在由后端 /api/profiles 的 active 字段按设备真值识别（设备参数与
     哪个场景快照完全一致就是哪个，都不一致 = 自定义状态）；
     apply 成功后本地即时更新，下次拉列表时以真值识别为准。 */
  function profSaveActive(){ /* 兼容占位：active 已改为设备真值识别 */ }
  try { localStorage.removeItem('iKF.activeProfile'); } catch(_){}

  function profSelected(){
    for (var i = 0; i < PROFILES.list.length; i++){
      if (PROFILES.list[i].name === PROFILES.active) return PROFILES.list[i];
    }
    return null;
  }

  /* 出厂档（驱动自带 + 自建「默认」，2026-10-20 改名）不给改名 / 删除：
     它是回到基线的唯一入口，删掉就再也回不去了。 */
  function profFactory(sel){
    return !sel || sel.builtin || sel.name === '默认';
  }

  /* 中央场景墙画在设计稿的 #stage 上，只有停在场景页时才需要重画 */
  function profRefreshWall(){
    if (typeof S !== 'undefined' && S.section === 'profile'){ render(); hud(); }
  }

  var delArm = '';   /* 「删除」的两段式确认：第一次点击只上膛，不真删 */

  /* ---------------- 右侧面板绘制 ---------------- */
  function paintProfPanel(){
    var act = $('profActive'), dirt = $('profDirty'), det = $('profDetail'),
        foot = $('profFoot'), ow = $('profOverwrite'),
        rn = $('profRename'), dl = $('profDelete'), add = $('profAdd');
    var sel = profSelected();

    /* 选中的场景换了（或没了），待确认的删除要撤掉，否则会把上一枪打出去 */
    if (delArm && (!sel || delArm !== sel.name)) delArm = '';
    /* sel 可能是 null（还没应用过任何场景）—— 上面那行靠 delArm 短路躲开了，
       这里必须自己挡一次，否则 paintProfPanel() 会读 null.name 抛异常，
       而它是在 loadProfiles().then() 里调的，异常会变成 unhandled rejection。 */
    if (dl && (!sel || delArm !== sel.name)) dl.textContent = '删除';

    if (act){
      act.textContent = PROFILES.active || '尚未应用场景';
      act.className = 'v' + (PROFILES.active ? '' : ' none');
    }
    if (dirt) dirt.hidden = !PROFILES.dirty;

    if (det){
      if (!sel){
        det.innerHTML = '<div class="empty">还没有应用任何场景。'
          + '点中央的场景卡片即可整套应用，这里会列出它动过哪些设置。</div>';
      } else if (PROFILES.detailFor !== sel.name){
        det.innerHTML = '<div class="empty">正在读取「' + pesc(sel.name) + '」…</div>';
      } else if (PROFILES.detailErr){
        det.innerHTML = '<div class="empty">读不到内容：'
          + pesc(PROFILES.detailErr) + '</div>';
      } else {
          var d = PROFILES.detail || { count: 0, groups: [] };
          /* 2026-10-20（用户要求）：详情卡改显「实际应用参数」——
             开启 = 值为 1 的开关型参数（Enable_* / AGCControl，Go 侧算好），
             档位/滑杆跟随所属开关不重复计数。共 N 项 = 档内全量快照条数。
          ★ 曾按「底部遮挡」误解收拢过一轮（合并采集组/折叠零组），12:40
             用户澄清真实诉求是渐隐提示后拍板完整恢复 15 行——底部按钮
             需滚动才能看到，由减半后的渐隐（12px）提示。 */
          var onN = (typeof d.on === 'number') ? d.on : 0;
          var h = '<div class="pr"><span>开启</span><b>' + onN
             + ' 项 / 共 ' + d.count + ' 项</b></div>';
          (d.groups || []).forEach(function(g){
            /* ★ 2026-10-19 深夜：分组行从纯统计升级成「页面选项」—— 点击
               跳到主界面对应设置页（麦克风组→麦克风页、EQ→均衡页、其余
               渲染组→声场页）。用户反馈：详情列了分组却只能看不能去，
               「切到页面发现开关不同步」的观感一部分来自这里没有入口。 */
            var nav = g.ep === 'capture' ? 'mic' : (g.label === 'EQ' ? 'eq' : 'space');
            h += '<div class="pr pr-nav" data-nav="' + nav
               + '" title="点击查看「' + pesc(g.label) + '」设置页"><span>' + pesc(g.label)
               + (g.ep === 'capture' ? '（麦克风）' : '')
               + '</span><b>开启 ' + (g.on || 0) + '/' + g.n + ' 项 ›</b></div>';
          });
        if (!(d.groups || []).length){
          h += '<div class="empty">这份场景不含参数快照。</div>';
        }
        det.innerHTML = h;
      }
    }

    var fo = profFactory(sel);
    if (ow){
      /* 覆盖 = 把当前状态写回这个场景。当前状态跟它一致时没有意义，
         所以只在脏的时候才可点 —— 按钮自己解释了自己的启用条件。 */
      ow.disabled = !sel || fo || !PROFILES.writable || !PROFILES.dirty;
      ow.textContent = sel ? ('用当前状态覆盖「' + sel.name + '」')
                           : '用当前状态覆盖场景';
      ow.title = !PROFILES.writable ? '场景目录不可写'
               : (sel && fo ? '默认场景不能覆盖'
               : (sel && !PROFILES.dirty ? '当前状态与该场景一致，无需覆盖' : ''));
    }
    if (rn){
      rn.disabled = !sel || fo || !PROFILES.writable;
      rn.title = (sel && fo) ? '默认场景不能重命名'
               : (PROFILES.writable ? '' : '场景目录不可写');
    }
    if (dl){
      dl.disabled = !sel || fo || !PROFILES.writable;
      dl.title = (sel && fo) ? '默认场景不能删除'
               : (PROFILES.writable ? '' : '场景目录不可写');
    }
    if (add){
      add.disabled = !PROFILES.writable;
      add.title = PROFILES.writable ? '' : '场景目录不可写';
    }
    if (foot){
      foot.textContent = '共 ' + PROFILES.list.length + ' 个场景 · 保存在'
        + (PROFILES.writable ? '' : '（只读）') + ' ' + (PROFILES.dir || '—');
    }
  }

  /* ---------------- 「已修改，未保存」 ----------------
     含义只有一句：**此刻的读数，是否已经离开了上次「应用 / 保存」时的样子**。

     ★ 基线必须是「那一刻的驱动实读值」，不能是场景文件里写的值。
       两者并不总是相等：场景里有些属性本机根本写不进去 —— 设备不支持的、
       或被驱动明确拒绝的，FactoryDefault.xml 就是极端例子，它整档只有
       一条 DefaultDeviceControl（一键恢复出厂），而本机 OEM 驱动未实现
       该命令（--serve 实测 0x10003）。拿文件值当基线，等于要求驱动去匹配
       一个它拒绝执行的值 —— 于是「一点应用就立刻显示已修改，未保存」。
       实测确认过：应用自带的「原厂默认」后必然误报。

     比对只做在基线里有的键上，且当前读数必须 r===0（读得到）——
     读不到就不下结论，宁可漏报不可误报。 */
  function profSnapshotBaseline(){
    var ref = PROFILES.ref;
    if (!ref || !ref.pairs){ PROFILES.base = null; return; }
    var base = null, k, i, row;
    for (k in ref.pairs){
      if (!Object.prototype.hasOwnProperty.call(ref.pairs, k)) continue;
      i = k.indexOf('|');
      if (i < 0) continue;
      row = profParams(k.slice(0, i))[k.slice(i + 1)];
      if (!row || row.r !== 0) continue;
      if (!base) base = {};
      base[k] = profNorm(row.v);
    }
    /* 一项都读不到（首帧参数表还没回来）就先不建基线，留 null 让下一次
       参数刷新再试 —— 不能趁这时候下「没改过」的结论。 */
    PROFILES.base = base;
  }

  function profRecompute(){
    if (!PROFILES.base && PROFILES.ref) profSnapshotBaseline();
    var base = PROFILES.base, dirty = false;
    if (base){
      for (var k in base){
        if (!Object.prototype.hasOwnProperty.call(base, k)) continue;
        var i = k.indexOf('|');
        if (i < 0) continue;
        var row = profParams(k.slice(0, i))[k.slice(i + 1)];
        if (!row || row.r !== 0) continue;        /* 读不到就不断言 */
        if (profNorm(row.v) !== base[k]){ dirty = true; break; }
      }
    }
    if (dirty === PROFILES.dirty) return;
    PROFILES.dirty = dirty;
    paintProfPanel();
    profRefreshWall();
  }

  /* ---------------- 读取 ---------------- */
  function loadProfiles(){
    return fetch('/api/profiles').then(function(r){ return r.json(); })
      .then(function(p){
        /* 场景墙显示全部场景：官方档（游戏/音乐/电影/原厂默认…）由
           renderProfile 打「出厂默认」徽标，用户自建卡可改可删。
           ★ 之前这里 filter(!x.builtin) 把官方场景全滤掉 —— 便携包下
           用户自建与内置档同住 CPL\profile，Go 侧按目录标 builtin 时
           连官方场景一起消失，场景墙空白（2026-10-19 真机确认）。
           现在 Go 侧按内置名单判定，这里不再二次过滤。 */
        PROFILES.list = p.list || [];
        PROFILES.dir = p.dir || '';
        PROFILES.writable = !!p.writable;
        /* active 以设备真值识别为准（p.active，lastscene.go/center_src.py）。
           本地 apply 后的即时值只在后端识别结果为空时保留（如用户刚手动
           改了参数、设备已不匹配任何场景 —— 此时显示「自定义状态」前的
           短暂窗口）。 */
        if (typeof p.active === 'string') PROFILES.active = p.active;
        /* active 指向的场景没了（被删、或换了一台机器）：基线一起作废，
           否则会出现「尚未应用场景」和「已修改，未保存」同时显示。 */
        if (!profSelected()){
          PROFILES.active = '';
          PROFILES.ref = null; PROFILES.base = null; PROFILES.dirty = false;
          profSaveActive();
        }
        window.PROFILES = PROFILES;      /* 设计稿的 renderProfile() 读它 */
        paintProfPanel();
        profRefreshWall();
        return p;
      });
  }

  /* 读一份场景的实际内容。asBaseline=true 表示「此刻驱动状态 == 这份快照」，
     于是顺手重建脏标记的基线 —— 只有「刚应用」「刚保存」「刚覆盖」三种
     情况成立。基线取的是**当前实读值**，不是 d.pairs 里的文件值，
     原因见 profSnapshotBaseline 上方那段。 */
  function loadDetail(name, asBaseline){
    PROFILES.detailFor = name;
    PROFILES.detailErr = '';
    return fetch('/api/profile/items?name=' + encodeURIComponent(name))
      .then(function(r){ return r.json(); })
      .then(function(d){
        if (d && d.ok){
          PROFILES.detail = d;
          if (asBaseline){
            PROFILES.ref = d;
            profSnapshotBaseline();
          }
        } else {
          PROFILES.detail = null;
          PROFILES.detailErr = (d && d.error) || '读取失败';
        }
        PROFILES.dirty = false;        /* 先清零，交给 profRecompute 按实情置回 */
        profRecompute();
        paintProfPanel();
        return d;
      })
      .catch(function(e){
        PROFILES.detail = null;
        PROFILES.detailErr = String(e);
        paintProfPanel();
      });
  }

  /* ---------------- 动作：应用 ---------------- */
  function profApply(name){
    if (!name) return;
    PROFILES.active = name;              /* 先给中央卡片一个按压反馈 */
    /* 应用过程中读数会变，旧基线立刻失效。不清掉的话，__resync 拉回新读数
       那一轮比对会先亮一下「已修改，未保存」，再被随后的基线重建压下去 ——
       用户看到的就是闪一下的假警报。清空后交给下面重拉读数时重建。 */
    PROFILES.base = null;
    PROFILES.dirty = false;
    paintProfPanel();
    profRefreshWall();
    postRaw('/api/profile/apply', {name: name}).then(function(r){
      if (r.ok){
        PROFILES.active = name;
        profSaveActive();
        var extra = r.applied ? ('，已恢复 ' + r.applied + ' 项设置')
                              : (r.note ? '（' + r.note + '）' : '');
        toast('场景「' + name + '」已应用' + extra);
        /* 场景改的是驱动参数：先把界面读数整个重拉一遍，再把这份快照
           设成脏标记基线。顺序不能反 —— 反了就是拿旧读数当基线。 */
        var rs = window.__resync ? window.__resync() : null;
        (rs && rs.then ? rs : Promise.resolve()).then(function(){
          return loadDetail(name, true);
        });
      } else {
        PROFILES.dead = PROFILES.dead || {};
        PROFILES.deadWhy = PROFILES.deadWhy || {};
        if (r.error){ PROFILES.dead[name] = true; PROFILES.deadWhy[name] = r.error; }
        /* 失败说明驱动可能只写进去一部分，原来的 active 已不再成立 */
        PROFILES.active = '';
        profSaveActive();
        paintProfPanel();
        profRefreshWall();
        toast('应用失败：' + (r.error || '设备未响应')
          + (r.failed ? ('，有 ' + r.failed + ' 项设置不受当前设备支持') : ''), true);
      }
    }).catch(function(e){ toast('应用失败：' + e, true); });
  }
  window.__applyProfile = profApply;     /* 设计稿的场景墙点卡片时调它 */

  /* ---------------- 表单 ---------------- */
  function profOpenForm(mode, name, desc){
    var form = $('profForm'), n = $('profName'), d = $('profDesc'), ok = $('profOK');
    if (!form) return;
    PROFILES.mode = mode;
    PROFILES.editName = name || '';
    if (n) n.value = name || '';
    if (d){ d.value = desc || ''; d.hidden = (mode === 'rename'); }
    if (ok) ok.textContent = (mode === 'rename') ? '确认改名' : '保存场景';
    form.hidden = false;
    if (n) n.focus();
    if (window.__relayout) window.__relayout();
  }
  function profCloseForm(){
    var form = $('profForm');
    if (form) form.hidden = true;
    PROFILES.editName = '';
    if (window.__relayout) window.__relayout();
  }
  window.__profileSaveNew = function(){
    if (!PROFILES.writable){ toast('场景目录不可写，无法保存', true); return; }
    profOpenForm('new', '', '');
  };

  /* ---------------- APO 未实现控件的默认隐藏（2026-10-19 用户拍板） ----------------
     场景已经不收这些参数（Go 侧 sceneUnproven 名单），主界面把它们默认
     隐藏。★ 调整入口 = **参数页现有 UI**（208 项全量参数表，未实现参数
     照常可搜可读写），不再设统一开发者开关（用户拍板「统一开关还是算了」）。
     ★ 2026-10-19 深夜按 REA-OUT 反编译复核修正口径：
       - 升降调（CKeyShifterLib）/ 人声消除（CVocalCancellationLib）是
         **真模块**（vftable + 构造体，K 档 GFX bit13/14）—— 常显
         （之前依据的「空壳」注释是笔误）；
       - 环绕增强（CXearSurrMaxLib）也有实体，但 A/B 未测到生效（有前置
         条件），继续隐藏等实测；场景同样暂不收；
       - 常显：环境音效（CReverbLib）、EQ、升降调、人声消除、耳机环绕
         （A/B 实测到变化）、采集侧全部、智能音量 / 声道互换（未测过不判死）；
       - 隐藏（入口在参数页）：SS 虚拟环绕、Dolby、动态低音、音频增艳、
         语音清晰度、虚拟低音 / SonicMapper / DDL / PLIIX / NeoPC（真·未实现）。 */
  var UNPROVEN_HIDE = {
    'Enable_SS_LFX': 1,            /* 虚拟环绕（RTTI 无类名 + A/B 噪声底） */
    'Enable_DHDVS_LFX': 1,         /* Dolby 环绕 */
    'Enable_SURRSENS_LFX': 1,      /* 环绕增强（模块有实体但 A/B 未确认生效，有前置条件） */
    'Enable_BM_GFX': 1,            /* 动态低音 */
    'Enable_AUDIOBRILLIANT_LFX': 1,/* 音频增艳 */
    'Enable_VOICECLARITY_LFX': 1,  /* 语音清晰度 */
    'Enable_VIRTUALBASS_LFX': 1,
    'Enable_SonicMapper': 1,
    'Enable_DDL_GFX': 1,
    'Enable_PLIIX_LFX': 1,
    'Enable_NEOPC_LFX': 1
  };
  /* 非开关形态的隐藏控件：Dolby 三下拉 + 虚拟环绕摆位编辑器。
     ★ 升降调/人声消除/增艳强度滑杆（ksRange/vcRange/brRange）已随开关
     一起处理：升降调/人声消除恢复常显（真模块），brRange（增艳）仍藏。 */
  var UNPROVEN_HIDE_EL = ['dhSel', 'dhHpSel', 'dhVsSel', 'brRange',
                          'vssModeRow', 'vssHint'];
  /* ★ 2026-10-19 深夜：动态低音 / 语音清晰两个旋钮**加回显示**（用户拍板）——
     从隐藏清单移除，恢复常显；悬浮提示仍走 UNPROVEN 口径（如实标注）。 */
  var UNPROVEN_HIDE_DIAL = [];

  function markUnprovenRows(){
    var st = document.createElement('style');
    st.id = 'unprovenHideStyle';
    st.textContent = '.unproven-row{display:none!important}'; /* 主界面无条件隐藏；调整入口在参数页 208 项参数表 */
    document.head.appendChild(st);
    var mark = function(el){
      if (!el) return;
      var row = el.closest('.row') || el.closest('.slider');
      if (row) row.classList.add('unproven-row');
      else el.classList.add('unproven-row');
      /* ★ 无行容器时必须藏元素自身，绝不能兜底到 parentNode ——
         旋钮卡片（.dial）共用 .dials 容器，兜底到父容器会把主音量
         一起藏掉（2026-10-19 实测报障：声场页旋钮卡片一个不剩）。 */
    };
    document.querySelectorAll('button.sw[data-p]').forEach(function(sw){
      if (UNPROVEN_HIDE[sw.dataset.p]) mark(sw);
    });
    UNPROVEN_HIDE_EL.forEach(function(id){ mark(document.getElementById(id)); });
    UNPROVEN_HIDE_DIAL.forEach(function(k){
      mark(document.querySelector('.dial[data-dial="' + k + '"]'));
    });
  }

  function bindProfile(){
    /* 分组行 → 设置页跳转（事件委托，paintProfPanel 每次 innerHTML 重建
       也无需重绑）。点击 = 模拟点导航 dock 的对应按钮，复用现有切页
       handler —— 页面控件在应用场景时已被 applyParams 全量同步过，
       跳过去看到的就是场景生效后的状态。 */
    var det = $('profDetail');
    if (det && !det.__navBound){
      det.__navBound = true;
      var st = document.createElement('style');
      st.textContent = '.pr-nav{cursor:pointer}.pr-nav:hover{background:rgba(255,255,255,.05)}'
        + '.pr-nav b{color:var(--t3)}';
      document.head.appendChild(st);
      det.addEventListener('click', function(e){
        var row = e.target && e.target.closest ? e.target.closest('.pr-nav') : null;
        if (!row) return;
        var b = document.querySelector('[data-s="' + row.dataset.nav + '"]');
        if (b) b.click();
      });
    }
    var add = $('profAdd');
    if (add) add.addEventListener('click', window.__profileSaveNew);
    var cancel = $('profCancel');
    if (cancel) cancel.addEventListener('click', profCloseForm);

    var ok = $('profOK');
    if (ok) ok.addEventListener('click', function(){
      var n = $('profName'), d = $('profDesc');
      var name = n ? n.value.trim() : '';
      if (!name){ toast('请先填写场景名称', true); return; }

      if (PROFILES.mode === 'rename'){
        var from = PROFILES.editName;
        postRaw('/api/profile/rename', {name: from, new_name: name})
          .then(function(r){
            if (!r.ok){ toast('改名失败：' + (r.error || '请稍后重试'), true); return; }
            if (PROFILES.active === from) PROFILES.active = r.name;
            if (PROFILES.ref) PROFILES.ref.name = r.name;
            profSaveActive();
            profCloseForm();
            toast('已重命名为「' + r.name + '」');
            /* 改名不动驱动状态，所以 asBaseline 必须是 false ——
               传 true 会把用户改到一半的参数误判成「未修改」。 */
            loadProfiles().then(function(){ loadDetail(r.name, false); });
          });
        return;
      }

      if (!PROFILES.writable){ toast('场景目录不可写，无法保存', true); return; }
      var desc = d ? d.value.trim() : '';
      postRaw('/api/profile/save', {name: name, desc: desc}).then(function(r){
        if (!r.ok){ toast('保存失败：' + (r.error || '请稍后重试'), true); return; }
        PROFILES.active = r.name;
        profSaveActive();
        profCloseForm();
        toast('已保存场景「' + r.name + '」（' + r.items + ' 项参数）');
        loadProfiles().then(function(){ loadDetail(r.name, true); });
      });
    });

    var ow = $('profOverwrite');
    if (ow) ow.addEventListener('click', function(){
      var sel = profSelected();
      if (!sel || !PROFILES.writable) return;
      postRaw('/api/profile/save', {name: sel.name, desc: sel.desc || ''})
        .then(function(r){
          if (!r.ok){ toast('覆盖失败：' + (r.error || '请稍后重试'), true); return; }
          toast('已用当前状态覆盖「' + r.name + '」（' + r.items + ' 项参数）');
          /* 覆盖后快照 == 当前状态，基线可以直接重建，脏标记随之落下 */
          loadProfiles().then(function(){ loadDetail(r.name, true); });
        });
    });

    var rn = $('profRename');
    if (rn) rn.addEventListener('click', function(){
      var sel = profSelected();
      if (!sel || !PROFILES.writable || profFactory(sel)) return;
      profOpenForm('rename', sel.name, sel.desc || '');
    });

    var dl = $('profDelete');
    if (dl) dl.addEventListener('click', function(){
      var sel = profSelected();
      if (!sel || !PROFILES.writable || profFactory(sel)) return;
      if (delArm !== sel.name){                 /* 第一次点击只上膛 */
        delArm = sel.name;
        dl.textContent = '再点一次确认删除';
        setTimeout(function(){
          if (delArm === sel.name){ delArm = ''; paintProfPanel(); }
        }, 3500);
        return;
      }
      delArm = '';
      var nm = sel.name;
      postRaw('/api/profile/delete', {name: nm}).then(function(r){
        if (!r.ok){ toast('删除失败：' + (r.error || '请稍后重试'), true); return; }
        if (PROFILES.active === nm){
          PROFILES.active = '';
          PROFILES.ref = null; PROFILES.base = null; PROFILES.dirty = false;
          PROFILES.detail = null; PROFILES.detailFor = '';
          profSaveActive();
        }
        toast('已删除场景「' + nm + '」');
        loadProfiles();
      });
    });

    /* Enter 直接提交表单，少一次「挪到按钮上」 */
    var pn = $('profName');
    if (pn) pn.addEventListener('keydown', function(e){
      if (e.key === 'Enter' && ok) ok.click();
      if (e.key === 'Escape') profCloseForm();
    });

    /* 脏标记的触发点之一：读数刷新。
       首帧、__resync、设备页「重新读取」都走 applyParams。只包一次，
       防止重复叠加。 */
    if (!bindProfile.wrapped){
      bindProfile.wrapped = true;
      var _applyParams = applyParams;
      applyParams = function(a, b){
        _applyParams(a, b);
        try { profRecompute(); } catch(_){}
      };
    }

    /* 脏标记的触发点之二：写驱动。
       ★ 原先这里写着「applyParams 是所有参数读数的唯一汇入口，写回后的
       回读也走它」—— 这个假设是错的：参数写入**不经过** applyParams，
       开关（d.v = r.value）、滑杆、下拉都是在各自的 .then 里就地改
       PM/PMC 的本地副本。于是改了参数也不会重算，脏标记永远不亮。
       所以在 post（即 /api/set）上再补一层：写成功后让出一轮宏任务再
       重算，此时调用方的 .then 早已把本地副本改完，比对到的是真正写下去
       的值。 */
    if (!bindProfile.postWrapped){
      bindProfile.postWrapped = true;
      var _post = post;
      post = function(body){
        return _post(body).then(function(r){
          if (r && r.ok){
            /* 2026-10-20：记录「主界面刚自己写过」。参数 watch 轮询会
               把这次写当成「外部变化」发 tray:sync，重同步会重设控件、
               和正在进行的拖拽打架 —— 600ms 内的 watch 事件由监听端忽略。 */
            window.__lastLocalWrite = Date.now();
            setTimeout(function(){ try { profRecompute(); } catch(_){} }, 0);
          }
          return r;
        });
      };
    }

    window.__toast = toast;    /* 设计稿的场景墙要用（它不在这个闭包里） */

    loadProfiles().then(function(){
      if (PROFILES.active) loadDetail(PROFILES.active, true);
      else { PROFILES.detail = null; PROFILES.detailFor = ''; paintProfPanel(); }
    });
  }

  /* 开关点击 → 写驱动。
     data-ep="capture" 的开关打在采集端点上（麦克风侧参数只存在于那里）。 */
  document.addEventListener('click', function(e){
    var sw = e.target.closest('button.sw[data-p]');
    if (!sw) return;
    if (sw.style.pointerEvents === 'none') return;
    var p = sw.dataset.p;
    var ep = sw.dataset.ep === 'capture' ? 'capture' : 'render';
    var was = sw.classList.contains('on');
    var d = (ep === 'capture' ? PMC : PM)[p];
    if (!d || !d.w){
      toast((sw.dataset.label || p) + ' · 当前设备不支持', true);
      return;
    }
    var want = was ? 0 : 1;
    sw.classList.toggle('on', want === 1);        /* 乐观更新 */
    var body = {name:p, value:want};
    if (ep === 'capture') body.ep = 'capture';
    post(body).then(function(r){
      if (r.ok){
        sw.classList.toggle('on', r.value === '1');   /* 以驱动读回为准 */
        if (d) d.v = r.value;
        toast((sw.dataset.label || p) + (r.value === '1' ? ' 已开启' : ' 已关闭'));
      } else {
        sw.classList.toggle('on', was);               /* 失败回滚 */
        toast((sw.dataset.label || p) + ' 设置失败，请稍后重试', true);
      }
    });
  });

  /* 通用「滑杆 → 驱动」绑定：input 只刷读数，change（松手）才写。
     麦克风侧的四根滑杆与渲染端点的两根共用这一段。 */
  function bindSlider(id, labId, param, ep, fmt, lo, hi, scale){
    var el = $(id);
    if (!el) return null;
    var fmtv = fmt || function(v){ return v; };
    var k = scale || 1;              /* 滑杆刻度 → 参数值 的换算（标量音量是 100:1） */
    el.addEventListener('input', function(){
      var l = $(labId);
      if (l) l.textContent = fmtv(+el.value / k);
    });
    el.addEventListener('change', function(){
      var v = +el.value / k;
      var body = {name:param, value:v};
      if (ep === 'capture') body.ep = 'capture';
      post(body).then(function(r){
        var l = $(labId);
        if (r.ok){
          if (l) l.textContent = fmtv(+r.value);
          toast((el.dataset.lab || param) + ' → ' + r.value);
        } else {
          toast(param + ' 设置失败，请稍后重试', true);
        }
      });
    });
    return el;
  }

  /* 通用「下拉 → 驱动」绑定 */
  function bindSelect(id, param, ep, names, label){
    var el = $(id);
    if (!el) return null;
    el.addEventListener('change', function(){
      var v = +el.value;
      var body = {name:param, value:v};
      if (ep === 'capture') body.ep = 'capture';
      post(body).then(function(r){
        if (r.ok){
          toast((label || param) + ' → ' + (names && names[+r.value] !== undefined
                ? names[+r.value] : r.value));
        } else {
          toast((label || param) + ' 设置失败，请稍后重试', true);
          el.value = (ep === 'capture' ? PMC : PM)[param] ? (ep === 'capture' ? PMC : PM)[param].v : el.value;
        }
      });
    });
    return el;
  }

  /* ---------------- 6. 均衡器预设 + 自定义 EQ 组态 ----------------
     预设名与官方面板 apoSpeakerEQ.cml 的 12 个单选钮一致。
     自定义 EQ 组态对应官方面板的 EQProfileList / EQProfileName /
     BtnSave / BtnDelete 四个控件 —— 官方把它们存在面板自己的配置里，
     我们存在 WebView 的 localStorage（随应用数据目录持久化）。 */
  var EQPROF_KEY = 'ikf.eqprof';

  function eqProfLoad(){
    try {
      var s = window.localStorage.getItem(EQPROF_KEY);
      var o = s ? JSON.parse(s) : {};
      return (o && typeof o === 'object') ? o : {};
    } catch (e){ return {}; }
  }
  function eqProfStore(o){
    try { window.localStorage.setItem(EQPROF_KEY, JSON.stringify(o)); }
    catch (e){ toast('自定义 EQ 无法保存到本地存储', true); }
  }
  function eqProfRender(sel){
    var box = $('eqProfSel');
    if (!box) return;
    var o = eqProfLoad(), ks = Object.keys(o);
    box.innerHTML = '<option value="">（未选择）</option>'
      + ks.map(function(k){
          return '<option value="' + k + '"' + (k === sel ? ' selected' : '')
            + '>' + k + '</option>';
        }).join('');
  }

  function bindEqChips(){
    var box = $('eqChips');
    if (box){
      /* 设计稿自身的 chip 点击已会把 S.eq 写成预设曲线，
         再由下面的 push() 写回驱动的 10 段 EQ。这里只补一条提示。 */
      box.addEventListener('click', function(e){
        var c = e.target.closest('.chip');
        if (c) toast('预设「' + c.textContent + '」已写入 10 段 EQ');
      });
    }

    var sel = $('eqProfSel');
    if (!sel) return;
    eqProfRender('');

    sel.addEventListener('change', function(){
      var o = eqProfLoad(), cur = o[sel.value];
      if (!cur) return;
      S.eq = cur.slice();
      S.pre = sel.value;
      render(); hud();
      /* 复用第 8 节的 push()：它按 LAST.eq 做差量，只写变化的频段。
         必须先把 EQ_W 打开，否则桥接层认为曲线不是本面板改的。 */
      EQ_W = true;
      push();
      toast('已载入自定义 EQ「' + sel.value + '」');
    });

    var sv = $('eqProfSave');
    if (sv){
      sv.addEventListener('click', function(){
        var nm = $('eqProfName');
        var name = nm ? nm.value.trim() : '';
        if (!name){ toast('请先填写自定义 EQ 名称', true); return; }
        var o = eqProfLoad();
        var existed = Object.prototype.hasOwnProperty.call(o, name);
        o[name] = (S.eq || []).slice(0, 10);
        eqProfStore(o);
        eqProfRender(name);
        if (nm) nm.value = '';
        toast(existed ? ('已覆盖自定义 EQ「' + name + '」')
                      : ('已新增自定义 EQ「' + name + '」'));
        if (window.__relayout) window.__relayout();
      });
    }

    var dl = $('eqProfDel');
    if (dl){
      dl.addEventListener('click', function(){
        var name = sel.value;
        if (!name){ toast('请先在下拉框里选中一项', true); return; }
        var o = eqProfLoad();
        delete o[name];
        eqProfStore(o);
        eqProfRender('');
        toast('已删除自定义 EQ「' + name + '」');
        if (window.__relayout) window.__relayout();
      });
    }
  }

  /* ---------------- 7. 应用驱动读回的参数 ----------------
     pm  = 渲染端点参数表，pmc = 采集端点参数表（麦克风侧）。 */
  function applyParams(pm, pmc){
    pmc = pmc || {};
    /* 均衡器 */
    var eq = [], anyEq = false;
    for (var i = 1; i <= 10; i++){
      var p = pm['EQ_Slider' + i];
      if (p && p.r === 0){
        var n = parseInt(p.v, 10);
        eq.push(isNaN(n) ? 0 : Math.max(-20, Math.min(20, n)));
        anyEq = true;
      } else { eq.push(0); }
    }
    if (anyEq){ S.eq = eq; LAST.eq = eq.slice(); EQ_W = true; }

    /* 均衡器开关 */
    var eqs = document.querySelector('button.sw[data-p="Enable_EQ_GFX"]');
    if (eqs && pm['Enable_EQ_GFX'] && pm['Enable_EQ_GFX'].r === 0){
      eqs.classList.toggle('on', pm['Enable_EQ_GFX'].v === '1');
    }
    /* 空间页开关 */
    ['Enable_SS_LFX', 'Enable_RFX_GFX'].forEach(function(p){
      var sw = document.querySelector('button.sw[data-p="' + p + '"]');
      if (!sw) return;
      if (pm[p] && pm[p].r === 0) sw.classList.toggle('on', pm[p].v === '1');
      else if (pm[p] && pm[p].r === 2) sw.classList.toggle('on', pm[p].v === '1');
    });

    /* 扩展控件（静音/环境类型/变调/人声消除/智能音量）的初始状态 */
    syncExtras(pm);
    syncDials(pm);
    /* ★ 第 49 轮：把驱动读回的 8 角度 + 8 距离刷到声场图与读数区 */
    syncSpk(pm);

    /* 统一同步所有带 data-p 的开关：按 data-ep 选表，可写即接线，
       不可写即置灰并标注 —— 而不是整块隐藏。 */
    document.querySelectorAll('button.sw[data-p]').forEach(function(sw){
      var ep = sw.dataset.ep === 'capture' ? 'capture' : 'render';
      var d = (ep === 'capture' ? pmc : pm)[sw.dataset.p];
      var label = sw.dataset.label || sw.dataset.p;
      if (UNPROVEN[sw.dataset.p]){
        /* 回环 A/B 没测到变化（见 UNPROVEN 注释）——**不置灰**：
           这只是我们这条测量路径看不到，不代表耳机里没效果。
           开关照常可点（驱动收写入），只在提示里说明测量结论。 */
        if (d) sw.classList.toggle('on', d.v === '1');
        if (sw.getAttribute('data-dimmed')) tagSwitch(sw, false);
        sw.removeAttribute('data-dimmed');
        sw.style.opacity = '';
        sw.style.pointerEvents = '';
        /* ★ 第 56 轮（L 档）：这句提示现在分两种情况 —— 见 __unprovenNote。
           名字在 APO_IMPL 里（主机侧模块**建了**）就不该暗示「没实现」。 */
        sw.title = label + ' · ' + (window.__unprovenNote
          ? window.__unprovenNote(sw.dataset.p)
          : '回环 A/B 未测到变化（可能由设备侧处理）');
        return;
      }
      /* ★ 修（P0 后）：判据必须只有 !d.w —— ret=2 是「从未写过」不是
         「不支持」（铁律）。这里此前写成 d.w && d.r===0，被「闸脚本先跑
         把参数写活」的测试顺序掩盖；P3 干净机上全新端点全 ret=2 时会
         大面积误置灰（P0 断依赖验收实测踩中）。 */
      if (d && d.w){
        sw.classList.toggle('on', d.v === '1');
        if (sw.getAttribute('data-dimmed')) tagSwitch(sw, false);
        sw.removeAttribute('data-dimmed');
        sw.style.opacity = '';
        sw.style.pointerEvents = '';
        /* ★ 2026-10-19：专属说明优先 —— 「可写」是开发者视角的词，
           用户悬停看到的应该是功能说明（耳机环绕，用户实测指出）。 */
        sw.title = SW_TIP[sw.dataset.p] || (label + ' · 可写');
      } else {
        /* 置灰同时摘掉设计稿写死的 on：死参数不允许显示假开态，
           否则用户会看到「常开且点不动」的误导界面。 */
        sw.classList.remove('on');
        dim(sw, label + ' · 当前设备不支持该参数');
      }
    });

    /* 麦克风页滑杆 / 下拉：同步驱动读回值 */
    syncSlider('micVolRange', 'micVolVal', 'VolumeScalarControl', pmc,
               function(v){ return Math.round(v * 100) + '%'; }, 100);
    syncSlider('nrRange', 'nrVal', 'NoiseReduction_Level', pmc, null);
    syncSlider('echoRange', 'echoVal', 'MICECHO_Level', pmc,
               function(v){ return (v > 0 ? '+' : '') + v; });
    syncSlider('aaRange', 'aaVal', 'AAVolumeControl', pmc,
               function(v){ return v + ' dB'; });
    syncSelect('mvSel', 'MagicVoice_Selection', pmc);
    syncSelect('micRateSel', 'DevSampleRateControl', pmc);
    syncSelect('devRateSel', 'DevSampleRateControl', pm);

    /* ★ 2026-10-18：原「麦克风音量」大读数行已并入滑杆行（ micVolVal 一处
       显示），micVolNum 元素随设计稿删除，这里的写入一并移除。
       设备页「驱动版本」的写入归 bindDevPanel（st.driver），这里不再写
       GetDriverVer —— 同一元素只保留一个写回主人。 */

    /* 采集端点整体不可用（机型只有渲染端点）时如实说明 */
    if (!(ST && ST.mic_ready)){
      var mg = $('pg-mic');
      if (mg && !$('__micnote')){
        var t = document.createElement('div');
        t.id = '__micnote';
        t.style.cssText = 'font-size:10.5px;color:var(--t3);line-height:1.6;margin-top:10px';
        t.textContent = '当前设备没有麦克风输入通道，麦克风设置暂不可用；'
          + '播放设置不受影响。';
        mg.appendChild(t);
      }
    }

    render(); hud(); renderTable(); patchHud();
    /* 第 59 轮：开关联动降暗的全量刷新。class 变更才有的 MutationObserver
       盖不住「值没变 ⟹ toggle 不产生变更」的同步（典型：初始 off→off），
       所以在每次全量同步末尾主动扫一遍。 */
    linkSweep();
    /* 就绪标记：扩展控件已完成首次驱动值同步（自动化测试用） */
    document.documentElement.dataset.bridgeReady = '1';
  }

  /* 滑杆同步。★ 2026-10-17 重设计（用户拍板「默认值可交互」）：
     三态而非两态 ——
       ① !d.w                    → 置灰（真不可写，铁律 1）
       ② w 且读得到值（r=0 v≠''） → 显示驱动真值
       ③ w 但读不到（ret=2 等）   → 保留设计稿默认刻度、可交互，
                                    title 如实标注「默认值」。
     ③是关键：ret=2 只是「端点还没写过值」，端点重建后界面不再大面积
     「·不可用」——用户拖一下写进去，从此就是真值。 */
  function syncSlider(id, labId, param, tbl, fmt, scale){
    var el = $(id);
    if (!el) return;
    var d = tbl[param];
    var fmtv = fmt || function(v){ return v; };
    var k = scale || 1;              /* 参数值 → 滑杆刻度 的换算（标量音量是 100:1） */
    if (d && !d.w){
      dim(el.parentNode, param + ' · 当前设备不支持');
      return;
    }
    el.dataset.lab = param;
    if (d && d.r === 0 && d.v !== ''){
      el.value = parseFloat(d.v) * k;
      var l = $(labId);
      if (l) l.textContent = fmtv(parseFloat(d.v));
      el.title = param + ' · 可调';
    } else {
      /* 保留设计稿默认值（HTML 里的 value），只换 title 说明来源 */
      el.title = param + ' · 默认值（驱动暂无记忆值，调节后生效）';
    }
    el.removeAttribute('data-dimmed');
    el.style.opacity = '';
    el.style.pointerEvents = '';
  }

  /* 下拉同步：三态判据同 syncSlider（见上）。 */
  function syncSelect(id, param, tbl){
    var el = $(id);
    if (!el) return;
    var d = tbl[param];
    if (d && !d.w){
      dim(el.parentNode, param + ' · 当前设备不支持');
      return;
    }
    if (d && d.r === 0 && d.v !== ''){
      el.value = d.v;
      el.title = param + ' · 可调';
    } else {
      el.title = param + ' · 默认值（驱动暂无记忆值，选择后生效）';
    }
    el.removeAttribute('data-dimmed');
    el.style.opacity = '';
    el.style.pointerEvents = '';
  }

  /* ---------------- 8. 把界面变化写回驱动 ----------------
     注意：设计稿自带一个 110ms 的动画循环，在「声场」页每帧都调
     render()。因此不能把写回挂在 render() 的防抖上 —— 定时器会被
     无限推迟，永远不触发。改为固定周期轮询，push() 内部用 LAST
     去重，只有真正变化时才发请求。 */
  function wrapRender(){
    if (window.__wrapped) return;
    window.__wrapped = true;
    /* 麦克风波形改用采集端点的真实电平 */
    var origMic = window.renderMic;
    window.renderMic = function(){
      if (MICMETER && MICMETER.ok && S.section === 'mic'){
        var pk = Math.max(0, Math.min(1, MICMETER.peak));
        S.mic = 0.22 + pk * 0.72;
      }
      return origMic.apply(this, arguments);
    };
    /* 设备页右侧面板：设计稿写死了「7.1 虚拟 / 48 kHz / 24 bit」，
       这里换成驱动实测值 */
    var origHud = window.hud;
    window.hud = function(){
      var r = origHud.apply(this, arguments);
      patchHud();
      /* patchHud 会改写读数区文案（声场页副标由「8 个音箱独立可调」
         换成「拖动音箱写回方位（由驱动决定）」），
         文字长度变了就可能改变高度，所以避让要重算一次。 */
      if (typeof layoutHud === 'function') layoutHud();
      return r;
    };
    setInterval(push, 280);
  }

  function patchHud(){
    if (!ST || !ST.ok) return;
    var el = $('hud');
    if (!el) return;
    if (S.section === 'space'){
      /* ★ 第 49 轮：这里原来把副标改写成「8 声道声场 · 拖动音箱写回方位
         （由驱动决定）」。现在设计稿的 hud() 自己就按当前交互模式写
         「8 个音箱 · 拖动改角度 / 拖动改距离」——**比这句更准**，
         而且它随模式切换即时更新。再覆盖回去只会把模式信息抹掉，
         所以这一支不再改文字（保留分支是为了以后要补真·设备读数时有落点）。 */
    } else if (S.section === 'dev'){
      /* 设计稿写死了「7.1 虚拟 / 48 kHz / 24 bit」，换成驱动实测值 */
      var rows = el.querySelectorAll('.rows .r b');
      if (rows[0] && ST.channels) rows[0].textContent = ST.channels + ' 声道（7.1）';
      if (rows[1] && ST.codec)    rows[1].textContent = ST.codec;
      if (rows[2] && ST.rate_text)
        rows[2].textContent = ST.rate_text + ' / ' + (ST.bit_depth || '?') + ' bit';
    } else if (S.section === 'mic'){
      /* 麦克风侧状态一律取采集端点。早期这里用渲染端点查，
         一律返回「不支持」，于是读数区被写成「未开放」——
         参数其实一直是开放的，只是问错了端点。 */
      var vals = [
        onoff(PMC['Enable_NOISEREDUCTION_LFX']),
        onoff(PMC['Enable_MAGICVOICE']),
        onoff(PMC['Enable_MICECHO']),
        onoff(PMC['AGCControl'])
      ];
      el.querySelectorAll('.rows .r').forEach(function(row, i){
        var b = row.querySelector('b');
        var v = vals[i];
        row.classList.toggle('hi', v === '开');
        if (b) b.textContent = (v === null) ? '设备不支持' : v;
      });
    }
  }

  function push(){
    if (!ST || !ST.ok) return;
    /* 均衡器：只写回发生变化的频段 */
    if (EQ_W){
      var prev = LAST.eq, changed = false, i;
      for (i = 0; i < S.eq.length; i++){
        if (!prev || prev[i] !== S.eq[i]){ changed = true; break; }
      }
      if (changed){
        S.eq.forEach(function(v, k){
          if (!prev || prev[k] !== v) post({name:'EQ_Slider' + (k + 1), value: v});
        });
        LAST.eq = S.eq.slice();
      }
    }
    if (ROOM_W){
      var step = roomToStep(S.room);
      if (LAST.room !== step){
        LAST.room = step;
        post({name:'RFX_ROOMSIZE', value: step}).then(function(r){
          var rv = $('roomVal');
          if (rv && r.ok) rv.textContent = roomLabel(+r.value);
        });
      }
    }
  }

  /* ---------------- 9. 实时电平 ----------------
     两个端点各有一条电平：渲染 8 声道、采集 1 声道。
     麦克风页画的是**采集**电平，早期误用了渲染端点。 */
  function startMeter(){
    function tick(){
      get('/api/meter').then(function(m){ METER = m; }).catch(function(){});
      get('/api/meter?ep=capture').then(function(m){ MICMETER = m; }).catch(function(){});
    }
    tick();
    setInterval(tick, 900);
  }

  /* ---------------- 10. 设备页按钮 ---------------- */
  function bindDevButtons(){
    var btns = [$('devReload'), $('devEqReset')];
    if (btns[0]){
      btns[0].textContent = '重新读取设备信息';
      btns[0].addEventListener('click', function(){
        badge('正在重新读取 …', true);
        Promise.all([get('/api/status'), get('/api/params'),
                     get('/api/params?ep=capture')]).then(function(a){
          ST = a[0]; PM = a[1] || {}; PMC = a[2] || {};
          applyParams(PM, PMC);
          badge('已连接', true, '驱动 ' + (ST.dll || '').split('\\').pop());
          toast('已重新读取 ' + Object.keys(PM).length + ' + '
                + Object.keys(PMC).length + ' 项参数（渲染 + 采集）');
        });
      });
    }
    if (btns[1]){
      btns[1].textContent = '重置均衡器';
      btns[1].addEventListener('click', function(){
        S.eq = [0,0,0,0,0,0,0,0,0,0];
        LAST.eq = null;
        S.pre = '平直';
        document.querySelectorAll('#eqChips .chip').forEach(function(x){
          x.classList.toggle('on', x.textContent === '平直');
        });
        render(); hud();
        toast('10 段 EQ 已归零');
      });
    }
  }

  /* ---------------- 10.5 音箱方位：真写驱动，由硬件决定 ----------------
     官方面板 apoSpeakerVirtual.cml 里的音箱方位，对应驱动的 SS_*_ANGLE / SS_*_DIST。

     ★ 实测更正（此前误判为「本机不支持」）：这些属性是**写后即可读**的 ——
     没写过时读返回 2（存在但无值），写一次就变成可读，且跨进程持久。
     SS_C/LF/RF/LS/RS/LB/RB/LFE_ANGLE/DIST 全部接受任意 int（含负数，实测
     写 −32 读回 −32），驱动不做值域校验。所以这里必须真写，
     不能在前端替硬件下「不支持」的结论。

     ★★ 第 49 轮：这里原本还有一份 SPK_PROP 映射 + 一套「松手就写角度、
     成功后 __resync()」的即时写回逻辑（第 30 轮写的），**已整块删除**。
     原因是它和新加的虚拟音箱摆位路径**互相打架**，而且只有 CDP 真鼠标测试
     才看得出来：
       新路径松手后要等 260ms 防抖才写（角度+距离 16 项）；
       旧路径松手立刻只写角度，成功后马上 __resync() 重拉参数 ——
       此时距离还没写出去，驱动回的是旧值 0，syncSpk 就把界面上的
       s.dist 覆盖成 0；等防抖到点，写出去的已经是 0。表现是
       「拖得动、界面数字也在变，一松手全没了，驱动里始终 0」。
     ⟹ 同一个交互只能有**一个**写回主人。现在全部归 bindVss/__vssCommit*
     （见上面 5.5 节），角度与距离一起走同一条防抖 + VSS_PEND 通道。
     （那份 SPK_PROP 映射也一并删了 —— 设计稿现在每个音箱自带 key 前缀。）*/

  function bindHonesty(){
    /* ★ 第 30 轮删掉了这里对页头副标的覆盖。
       它原来把 TITLES.space[1] 改写成「拖动虚线圈改变房间大小 ·
       拖动音箱写回方位（由驱动决定）」—— 那是「怎么操作」，而第 28 轮
       已经定下副标只写「这一页是干什么的」。更要命的是它**只改运行时、
       不改设计稿**：稿子上写着「房间、音箱与音效」，装到机器上却变成
       那一长句，两边对不上（这轮改副标时才暴露出来）。
       现在操作提示挂在音箱元素自己的 <title> 上（见设计稿里生成
       <g class="spk"> 的那段），副标归 TITLES 管。 */
    /* ★ 第 49 轮：这里原来那段「记住按在哪个音箱上、松手写回 SS_*_ANGLE、
       成功就 __resync()」的写法**整块搬走了** —— 它只写角度、且与新的
       虚拟音箱摆位路径抢同一个交互（详见 10.5 节的说明）。
       现在拖拽的统一出口是设计稿的 window.__vssCommit() → bindVss */
    /* 立刻刷一次读数区，别等下一次 hud() 才改文案 */
    patchHud();
  }

  /* ---------------- 11. 未连接时的整体处理 ---------------- */
  function bindUnsupportedAll(){
    document.querySelectorAll('button.sw').forEach(function(s){
      /* ★ 2026-10-20 晚：「耳机录音」开关是纯 WASAPI 功能，不依赖
         C-Media 驱动 —— 驱动未连接照样能录（录的是系统播放端点回环）。
         把它排除在整体置灰外，否则 data-dimmed 的 pointer-events:none
         会吞掉点击（CDP 闸实测：点开关命中 recRow，录音永不启动）。 */
      if (s.id === 'recBtn') return;
      dim(s, '驱动未连接');
    });
    document.querySelectorAll('#pg-mic select, #pg-dev select').forEach(function(s){
      dim(s.parentNode, '驱动未连接');
    });
    ['micVolRange', 'nrRange', 'echoRange', 'aaRange'].forEach(function(id){
      var el = $(id);
      if (el) dim(el.parentNode, '驱动未连接');
    });
    /* 第 52 轮（H 档）：左右声道滑杆同样属于「驱动未连接就不可用」 */
    ['volLRange', 'volRRange'].forEach(function(id){
      var el = $(id);
      if (el) dim(el.parentNode, '驱动未连接');
    });
    var rr = $('roomRange');
    if (rr) dim(rr.parentNode, '驱动未连接');
    var ls = $('listenSw');
    if (ls) dim(ls, '驱动未连接');
  }

  /* ---------------- 12. 参数表：叠加实时状态 + 可编辑 ---------------- */
  var EXTRA_RANGE = {
    VOCALCANCEL_LEVEL: [0, 100, 1],
    KEYSHIFT_LEVEL:    [-4, 4, 1],
    RFX_ENVIRONMENT:   [0, 27, 1],
    RFX_ROOMSIZE:      [0, 2, 1],
    GFSControl:        [0, 1, 1],
    VolumeScalarControl: [0, 1, 0.01],
    /* A 档 Xear 音效族（第 46 轮移植）—— 值域逐条取自官方面板 .cml 的
       Slider Minimum/Maximum，不是猜的：
         apoSpeakerXearAudioBrilliant.cml  BrilliantLevel      Minimum 0 Maximum 6
         apoSpeakerXearDynamicBass.cml     BassLevel           Minimum 0 Maximum 10
         apoSpeakerXearDynamicBass.cml     CutoffFrequency     Minimum 100 Maximum 400
         apoSpeakerXearVoiceClarity.cml    VoiceClarityLevel   Minimum 0 Maximum 6
         apoSpeakerXearVoiceClarity.cml    SuppLevel           Minimum 0 Maximum 20（步进 5）
       ★ 官方没做界面的（VIRTUALBASS_* / VOCALEFFECT_LEVEL / SonicMapper）
         不给值域 —— 没有依据就不编范围，保持 null（参数页只读显示）。 */
    AUDIOBRILLIANT_LEVEL:         [0, 6, 1],
    BM_Slider_BassLevel:          [0, 10, 1],
    BM_Slider_Frequency:          [100, 400, 10],
    VOICECLARITY_LEVEL:           [0, 6, 1],
    VOICECLARITY_NOISESUPP_LEVEL: [0, 20, 5],
    /* B 档 Xear 环绕族（第 47 轮）—— 档位取自
       apoSpeakerXearSurroundHeadPhone.cml 的 RadioButton Tag：
         HPSPK_Mode    Music/Gaming=0 / Movie=1
         HP RoomSize   Large=0 / Medium=1 / Small=2（★ 大在前，与直觉反序）
       ★ Surround Max 只有一个开关，没有可调值域可给。 */
    CONLAB_HPSPK_MODE:     [0, 1, 1],
    CONLAB_HPSPK_ROOMSIZE: [0, 2, 1],
    /* Xear Surround Speaker 页的两根滑杆（第 47 轮）。值域同样取自 .cml：
         apoSpeakerXearSurroundSpeakers.cml  SFExpansion  Minimum 0  Maximum 3
         apoSpeakerXearSurroundSpeakers.cml  SPKAngle     Minimum 5  Maximum 30（步进 5）
       ★ 绑定靠「控件名↔参数名同名 + exe 字符串表同块」推断，见 bindSwitches 注释。 */
    CONLAB_HPSPK_SFX:      [0, 3, 1],
    CONLAB_HPSPK_SPKANGLE: [5, 30, 5],
    /* C 档 Dolby Headphone / Virtual Speaker（第 48 轮）—— 档位数是**数
       apoSpeakerDolbyHPVS.cml 里 RadioButton 的个数**定的（这几个 RadioButton
       没写 Tag，所以数值顺序按声明顺序推，与上面 SFX/SPKANGLE 同属「顺序推断」）：
         DolbySurroundEffect 组  BtnEnableVS=0 / BtnEnableHP=1   → DHDVS_USEDH 0..1
         SpeakerDolbyVirtualSpeaker 组  VSSmall(Reference)=0 / VSLarge(Wide)=1 → DVS_Mode 0..1
         SpeakerDolbyHeadphone 组  DH1(Studio)=0 / DH2(Cinema)=1 / DH3(Hall)=2   → DH_Mode 0..2
       ★ `DHDVS_USEDH` 字面是「use Dolby Headphone」，所以 **1=HP、0=VS**；
         界面下拉的选项顺序必须让下标 0 落在 VS 上。 */
    DHDVS_USEDH: [0, 1, 1],
    DVS_Mode:    [0, 1, 1],
    DH_Mode:     [0, 2, 1],
    /* D 档 7.1 虚拟音箱摆位（第 49 轮）—— 8 个角度 + 8 个距离。
       ★★ 值域要说清楚是哪来的，别混：
         · 角度 −180…180 是**符号约定的规范化区间**，依据是官方安装态的
           真实值集恰好横跨这个范围（LF=-32 C=0 RF=32 RS=90 RB=148
           LB=-148 LS=-90，LFE=180 正落在边界上）；
         · 距离 ±100 是**本面板自己的相对刻度**，不是官方值域 ——
           驱动实测对 SS_*_DIST 与 SS_*_ANGLE **都不钳位**（写 ±999 原样回读），
           官方页又没有滑杆/刻度可抄（只能靠拖动图标改），
           所以这里给的是本面板为了让「推远/拉近」有个可用手感而定的刻度。
       两线白名单里：角度给 RANGES 硬钳到 −180…180（概念上就是一个圆），
       距离**不给** RANGES —— 驱动不钳位，我们也不假装它有个官方上限。 */
    SS_LF_ANGLE:  [-180, 180, 1], SS_C_ANGLE:    [-180, 180, 1],
    SS_RF_ANGLE:  [-180, 180, 1], SS_RS_ANGLE:   [-180, 180, 1],
    SS_RB_ANGLE:  [-180, 180, 1], SS_LB_ANGLE:   [-180, 180, 1],
    SS_LS_ANGLE:  [-180, 180, 1], SS_LFE_ANGLE:  [-180, 180, 1],
    SS_LF_DIST:   [-100, 100, 1], SS_C_DIST:     [-100, 100, 1],
    SS_RF_DIST:   [-100, 100, 1], SS_RS_DIST:    [-100, 100, 1],
    SS_RB_DIST:   [-100, 100, 1], SS_LB_DIST:    [-100, 100, 1],
    SS_LS_DIST:   [-100, 100, 1], SS_LFE_DIST:   [-100, 100, 1],
    /* 麦克风侧（采集端点）实测值域 */
    NoiseReduction_Level: [8, 20, 1],
    MICECHO_Level:        [-12, 12, 1],
    MagicVoice_Selection: [0, 3, 1],
    AAVolumeControl:      [-23, 8, 1]
  };
  function rangeOf(n){
    var m = /^EQ_Slider(\d+)$/.exec(n);
    if (m) return [-20, 20, 1];
    if (n === 'VolumeControl'){
      /* 同一个属性名在两个端点上语义不同：渲染端点 = 扬声器音量，
         采集端点 = 麦克风音量。值域也不一样 —— 采集端点实测
         GetMinVol = −12 / GetMaxVol = +23。 */
      if (CUR_EP === 'capture') return [-12, 23, 1];
      if (ST) return [ST.vol_min || -37, ST.vol_max || 0, ST.vol_step || 1];
    }
    /* 离散取值（只有 44100/48000 两个合法值）：不给滑杆，用专用下拉 */
    if (n === 'DevSampleRateControl') return null;
    return EXTRA_RANGE[n] || null;
  }

  /* 参数状态徽章。
     ★ 名字要一起进来：UNPROVEN 里的项 ret 是 0（写得进去），单看返回码
     会标成「支持」—— 但回环 A/B 没测到变化，所以显「未验证」。
     ★ 徽章只说「我们验过没有」，不下「本机不支持」的结论。 */
  function rstate(n, d){
    if (!d) return { t:'未识别', c:'no' };
    if (UNPROVEN[n]) return { t:'未验证', c:'no' };
    if (d.r === 0) return { t:'支持', c:'ok' };
    if (d.r === 2) return { t:'不支持', c:'no' };
    return { t:'异常', c:'no' };
  }

  /* 写入成功后只刷新该行的数值文本，不重建整张表（避免打断拖拽） */
  function setValText(n, v){
    var el = document.querySelector('#tb span[data-v="' + n + '"]');
    if (el) el.textContent = v;
  }

  window.renderTable = function(){
    var tb = document.getElementById('tb');
    var qn = document.getElementById('qn');
    if (!tb) return;

    /* 端点切换：麦克风侧参数只在采集端点上可读 */
    var TBL = (CUR_EP === 'capture') ? PMC : PM;

    /* 合并：设计稿的 P（带中文说明）+ 驱动里多出来的名字。
       采集端点的属性面与渲染端点完全不同，P 那套中文分类只适用于渲染端点。 */
    var rows = [], seen = {};
    /* 参数页的 UNPROVEN 说明也走同一条文案（第 56 轮 L 档）—— 之前这里有**两处**
       写死的说法：静态分类列表那条和渲染表那条。同一件事两套文案，改一处漏一处。
       现在两处都调本函数。 */
    function noteShort(n){
      if (window.__unprovenNoteShort) return window.__unprovenNoteShort(n);
      return '回环 A/B 未测到变化';
    }
    function unprovenNote(n){
      if (window.__unprovenNote) return window.__unprovenNote(n);
      return '回环 A/B 未测到变化（可能由设备侧处理）';
    }
    if (CUR_EP !== 'capture'){
      P.forEach(function(p){
        seen[p[0]] = 1;
        /* ★ 第 56 轮（L 档）：这里原来写的是**固定一句**「（回环 A/B 未测到变化）」，
           与开关那边（走 __unprovenNote 分流）不是一套说法 —— 同一件事两套文案。
           现在统一走同一个函数：模块建了的会带上模块名。
           见 lint 闸的 E7 项（它检查的不只是「有没有用函数」，还要求这两条路径
           都在用；上一版只改了渲染表那条，这条漏了）。 */
        rows.push({ n:p[0], cat:p[1], rw:p[2],
                    desc:p[3] + (UNPROVEN[p[0]]
                      ? '（' + noteShort(p[0]) + '）' : '') });
      });
    }
    Object.keys(TBL).forEach(function(n){
      if (seen[n]) return;
      var d = TBL[n];
      var desc;
      if (UNPROVEN[n]) desc = '可读可写；' + unprovenNote(n);
      else if (d.r === 0) desc = '当前设备可用';
      else if (d.r === 2) desc = '当前设备不支持';
      else if (d.r === -1) desc = '暂无信息';
      else desc = '读取异常';
      if (CUR_EP === 'capture' && d.w) desc += ' · 麦克风侧可设置';
      rows.push({ n:n, cat: CUR_EP === 'capture' ? '麦克风' : '其他',
                  rw: d.w ? '读写' : '只读', desc: desc });
    });

    /* 分类 chips（按端点重建） */
    var cats = ['全部'].concat(rows.map(function(r){ return r.cat; })
      .filter(function(v, i, a){ return a.indexOf(v) === i; }));
    var chipBox = document.getElementById('catChips');
    if (chipBox){
      var sig = CUR_EP + '|' + cats.join(',');
      if (chipBox.dataset.sig !== sig){
        chipBox.dataset.sig = sig;
        chipBox.innerHTML = cats.map(function(c, i){
          return '<button class="chip' + (i === 0 ? ' on' : '')
            + '" data-cat="' + c + '" style="padding:5px 11px;font-size:11px">'
            + c + '</button>';
        }).join('');
        if (!chipBox.dataset.wired){
          chipBox.dataset.wired = '1';
          chipBox.addEventListener('click', function(e){
            var c = e.target.closest('.chip');
            if (!c) return;
            window.__CAT = c.dataset.cat;
            chipBox.querySelectorAll('.chip').forEach(function(x){
              x.classList.toggle('on', x === c);
            });
            window.renderTable();
          });
        }
      }
    }

    var CAT = window.__CAT || '全部';
    var k = (window.__KW || '').trim().toLowerCase();
    var out = rows.filter(function(r){
      if (CAT !== '全部' && r.cat !== CAT) return false;
      if (k && r.n.toLowerCase().indexOf(k) < 0
          && r.cat.indexOf(k) < 0 && (r.desc || '').indexOf(k) < 0) return false;
      return true;
    });

    var hit = [];
    rows.forEach(function(r, i){ if (out.indexOf(r) >= 0) hit.push(i); });
    S.hit = (out.length === rows.length) ? null : hit;
    if (S.section === 'api') render();

    tb.innerHTML = out.map(function(r){
      var d = TBL[r.n];
      var st = rstate(r.n, d);
      var val = (d && d.v !== '') ? d.v : '—';
      var ctrl = '';
      if (d && d.w){
        if (d.t === 'bool'){
          ctrl = '<button class="sw" data-set="' + r.n + '" data-kind="bool"'
            + (val === '1' ? ' class="sw on"' : '') + '></button>';
          ctrl = ctrl.replace('class="sw" data-kind="bool" class="sw on"',
                              'class="sw on" data-kind="bool"');
        } else {
          var rg = rangeOf(r.n);
          if (rg){
            ctrl = '<input type="range" data-set="' + r.n + '" data-kind="num"'
              + ' min="' + rg[0] + '" max="' + rg[1] + '" step="' + rg[2]
              + '" value="' + (parseFloat(val) || 0) + '" style="width:74px">';
          }
        }
      }
      return '<div style="display:flex;align-items:center;gap:8px;padding:7px 2px;'
        + 'border-bottom:1px solid var(--line)">'
        + '<div style="min-width:0;flex:1">'
        + '<div style="font-family:ui-monospace,Consolas,monospace;font-size:11px;'
        + 'color:var(--t1);white-space:nowrap;overflow:hidden;text-overflow:ellipsis">'
        + r.n + '</div>'
        + '<div style="font-size:10.5px;color:var(--t3);margin-top:2px">'
        + (r.desc || '') + '</div></div>'
        + '<span data-v="' + r.n + '" style="flex:0 0 auto;'
        + 'font-family:ui-monospace,monospace;font-size:10.5px;'
        + 'color:var(--t2);max-width:96px;overflow:hidden;text-overflow:ellipsis;'
        + 'white-space:nowrap">' + val + '</span>'
        + ctrl
        + '<span style="flex:0 0 auto;font-size:10px;padding:2px 8px;border-radius:999px;'
        + 'background:var(--ghost);color:var(--t2)">' + r.cat + '</span>'
        + '<span style="flex:0 0 auto;font-size:10px;padding:2px 7px;border-radius:999px;'
        + (st.c === 'ok' ? 'color:#fff;background:var(--accent)'
                         : 'color:var(--t3);border:1px solid var(--line)')
        + '">' + st.t + '</span></div>';
    }).join('') || '<div style="padding:24px;text-align:center;color:var(--t3);'
      + 'font-size:12px">无匹配结果</div>';

    if (qn){
      var okN = Object.keys(TBL).filter(function(n){
        return TBL[n].r === 0 && !UNPROVEN[n]; }).length;
      var wN  = Object.keys(TBL).filter(function(n){ return TBL[n].w; }).length;
      qn.textContent = '共 ' + out.length + ' / ' + rows.length
        + ' 项 · 已确认可用 ' + okN + ' 项 · 可设置 ' + wN + ' 项 · '
        + (CUR_EP === 'capture' ? '麦克风' : '扬声器');
    }
  };

  /* 参数映射页的端点切换 */
  function buildEpChips(){
    var box = $('epChips');
    if (!box) return;
    box.innerHTML = [
      ['render',  '扬声器（播放）'],
      ['capture', '麦克风（录音）']
    ].map(function(e, i){
      return '<button class="chip' + (i === 0 ? ' on' : '') + '" data-ep="' + e[0]
        + '" style="padding:5px 11px;font-size:11px">' + e[1] + '</button>';
    }).join('');
    box.addEventListener('click', function(ev){
      var c = ev.target.closest('.chip[data-ep]');
      if (!c) return;
      CUR_EP = c.dataset.ep;
      box.querySelectorAll('.chip').forEach(function(x){
        x.classList.toggle('on', x === c);
      });
      window.__CAT = '全部';
      window.renderTable();
      toast('已切换到' + (CUR_EP === 'capture' ? '麦克风（录音）' : '扬声器（播放）'));
    });
  }

  /* 搜索框 */
  var q = document.getElementById('q');
  if (q){
    q.addEventListener('input', function(e){
      window.__KW = e.target.value;
      window.renderTable();
    });
  }

  /* 表内控件：开关 / 滑杆 */
  document.addEventListener('click', function(e){
    var sw = e.target.closest('button.sw[data-set]');
    if (!sw) return;
    var n = sw.dataset.set;
    var TBL = (CUR_EP === 'capture') ? PMC : PM;
    var was = sw.classList.contains('on');
    var want = was ? 0 : 1;                     /* 目标状态，不是当前状态 */
    sw.classList.toggle('on', want === 1);
    var body = {name:n, value:want};
    if (CUR_EP === 'capture') body.ep = 'capture';
    post(body).then(function(r){
      if (r.ok){
        sw.classList.toggle('on', r.value === '1');
        if (TBL[n]) TBL[n].v = r.value;
        setValText(n, r.value);
        toast(n + ' → ' + r.value);
      } else {
        sw.classList.toggle('on', was);         /* 失败回滚 */
        toast(n + ' 设置失败，请稍后重试', true);
      }
    });
  });

  var rTimer = null;
  document.addEventListener('input', function(e){
    var el = e.target.closest('input[data-set][data-kind="num"]');
    if (!el) return;
    var n = el.dataset.set;
    clearTimeout(rTimer);
    rTimer = setTimeout(function(){
      var TBL = (CUR_EP === 'capture') ? PMC : PM;
      var body = {name:n, value:parseFloat(el.value)};
      if (CUR_EP === 'capture') body.ep = 'capture';
      post(body).then(function(r){
        if (r.ok){
          if (TBL[n]) TBL[n].v = r.value;
          setValText(n, r.value);
          toast(n + ' → ' + r.value);
          /* 渲染端点的 VolumeScalarControl 与左下角音量旋钮是同一个量，
             写回后要把旋钮也同步过去（采集端点的同名参数是麦克风音量，不同一个）。 */
          if (n === 'VolumeScalarControl' && CUR_EP !== 'capture'){
            var d = document.querySelector('.dial[data-dial="vol"]');
            if (d){ d.dataset.val = Math.round(parseFloat(r.value) * 100);
                    if (window.paintDial) paintDial(d); }
          }
        } else {
          toast(n + ' 设置失败，请稍后重试', true);
        }
      });
    }, 240);
  });

  /* 开关（表内）的视觉同步由上面的 click 处理；设计稿的全局 [data-sw]
     处理器不会命中 data-set 按钮，互不干扰。 */

  /* ---------------- 13. 首次渲染 ---------------- */
  buildEpChips();
  setTimeout(function(){ render(); hud(); renderTable(); }, 60);
})();
"""


def main():
    for p in (SRC, DESIGN, NAMES):
        if not os.path.isfile(p):
            print("缺少文件：%s" % p)
            return 1

    src = open(SRC, encoding="utf-8").read()
    html = open(DESIGN, encoding="utf-8").read()
    names = json.load(open(NAMES, encoding="utf-8"))

    # ---- 1. 注入桥接层 ----
    if "</body>" not in html:
        print("设计稿里没有 </body>，无法注入。")
        return 1
    if "/* ============================================================\n   驱动桥接层" in html:
        print("设计稿里已经注入过桥接层，跳过注入。")
    else:
        html = html.replace("</body>", "<script>" + BRIDGE_JS + "</script>\n</body>", 1)

    # ---- 1b. 注入内联 favicon（消除 /favicon.ico 404） ----
    if "rel=\"icon\"" not in html and "</title>" in html:
        html = html.replace("</title>", "</title>\n" + FAVICON, 1)

    # ---- 2. 安全检查：嵌入用的原始字符串不能含三引号 ----
    if '"""' in html:
        print("设计稿含三引号，无法用原始字符串嵌入。")
        return 1

    # ---- 3. 填充占位符 ----
    def fmt_list(lst, per_line=3):
        out = []
        for i in range(0, len(lst), per_line):
            out.append("    " + " ".join('"%s",' % x for x in lst[i:i + per_line]))
        return "[\n" + "\n".join(out) + "\n]"

    out = src
    out = out.replace("__GENERIC__", fmt_list(names["generic"]))
    out = out.replace("__OEM__", fmt_list(names["oem"]))
    out = out.replace("__HTML__", html)

    with open(OUT, "w", encoding="utf-8", newline="\n") as f:
        f.write(out)

    print("已生成：%s" % OUT)
    print("  体积   : %.1f KB" % (os.path.getsize(OUT) / 1024.0))
    print("  通用参数: %d 项" % len(names["generic"]))
    print("  OEM 参数: %d 项" % len(names["oem"]))
    print("  界面    : %.1f KB" % (len(html) / 1024.0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
