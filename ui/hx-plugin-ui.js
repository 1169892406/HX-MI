/*
 * HOMEX 插件 UI 的官方小工具（2026-11-22）。
 *
 * ============================ 它解决什么 ============================
 *
 * 用户口径：「平台侧 SDK 增加 UI 自适应和平台侧滚动条声明，这样保持 UI 统一性」。
 *
 * 在此之前每个插件界面都自己写一遍「ResizeObserver + 去抖 + plugin.ui.resize」，
 * 结果三处细节各不相同，用户看到的就是"每个插件页面滚动行为都不一样"：
 *
 *   ① 有的报 Math.ceil(body 高度)，有的报 offsetHeight，差 1~2px 就会多出一条
 *      永远存在的滚动条（或者内容被切掉一条）；
 *   ② 有的算上 margin 有的不算 —— body 有 margin 时，高度报少了，页面底部被裁；
 *   ③ clamp 区间各写一个数字，宿主 clamp 到 [320,2000]，插件自己写 300/1800，
 *      于是同一份内容在两个插件里高度不同。
 *
 * 这个文件把这三件事收敛成一份实现：**宿主下发的区间** + **同一套测量口径**。
 *
 * ============================ 怎么用 ============================
 *
 *   <script src="./hx-plugin-ui.js"></script>
 *   <script>
 *     // rpc 用插件自己的传输层（本工具不碰通信协议，只算高度/管滚动条）
 *     var ui = HxPluginUi.start({
 *       rpc: function (op, payload) { return myRpc(op, payload); }
 *     });
 *     // 收到宿主的 theme 帧时透传一次（主题 / 滚动条归属都在这一帧里）
 *     function onHostFrame(m) { ui.acceptHostFrame(m); }
 *   </script>
 *
 * 约束（与协议一致）：
 *   · 不设 `id`、不参与 RPC 的 pending/超时 —— 它只发 `plugin.ui.resize`；
 *   · 不发任何网络请求（插件 iframe 的 CSP `connect-src 'none'`）；
 *   · 失败静默：上报高度是体验优化，任何异常都不该打断插件自己的功能。
 */
(function (global) {
  'use strict';

  /** 宿主默认区间（与 platformsvc 的 clamp 同源；收到 theme 帧后以帧里的为准）。 */
  var DEFAULT_MIN = 320;
  var DEFAULT_MAX = 12000;   // 与宿主 clamp 同源（2026-11-22 由 2000 上调，见 pluginUiProtocol）
  /** 去抖：内容连着变（表格填充、展开描述）时只报最后一次。 */
  var DEBOUNCE_MS = 150;

  function toInt(v, fallback) {
    var n = parseInt(v, 10);
    return isNaN(n) ? fallback : n;
  }

  function clamp(h, min, max) {
    if (typeof h !== 'number' || !isFinite(h)) return min;
    if (h < min) return min;
    if (h > max) return max;
    return Math.round(h);
  }

  /**
   * 内容区高度。
   *
   * ## 为什么**不能**用 documentElement.scrollHeight（2026-11-22 修）
   *
   * scrollHeight 至少等于视口高度。宿主把 iframe 设成插件上报的高度之后，
   * 视口就变成了那个高度 ⇒ 下一轮测量拿到的 scrollHeight ≥ 上一轮上报值
   * ⇒ **只增不减**。实测后果：内容收缩（详情区清空、列表折叠）后仍有几百像素
   * 空白留在页面里，用户截图就是"内容下面一大片空的"。
   *
   * 所以只取**内容自身**的高度：
   *   · body 盒高 + body 上下 margin（getBoundingClientRect 不含 margin）；
   *   · body.scrollHeight（同样是内容口径，不被视口撑大）；
   * 两者取大。子元素绝对定位/浮动溢出的情况由 scrollHeight 兜住。
   */
  function measure() {
    var body = document.body;
    if (!body) return 0;
    var rect = body.getBoundingClientRect();
    var h = rect.height;
    var cs = global.getComputedStyle ? global.getComputedStyle(body) : null;
    if (cs) {
      h += (parseFloat(cs.marginTop) || 0) + (parseFloat(cs.marginBottom) || 0);
    }
    if (body.scrollHeight > h) h = body.scrollHeight;
    return h;
  }

  function start(opts) {
    opts = opts || {};
    var rpc = typeof opts.rpc === 'function' ? opts.rpc : null;
    var min = toInt(opts.min, DEFAULT_MIN);
    var max = toInt(opts.max, DEFAULT_MAX);
    var lastReported = -1;
    var timer = null;
    var stopped = false;
    var observer = null;
    var hostScrollbar = false;
    var hostBackProvided = false;

    /**
     * 隐藏插件自带的「返回插件列表」按钮（宿主已提供同一个入口）。
     *
     * 判据刻意窄：文本包含「返回」且是按钮/链接 —— 宁可漏掉（多一颗按钮），
     * 也不要把插件里别的按钮误藏（那是"改 UI 砍功能"，用户明令禁止）。
     */
    function hideOwnBackButton() {
      try {
        var nodes = document.querySelectorAll('button, a, [role="button"]');
        for (var i = 0; i < nodes.length; i++) {
          var t = (nodes[i].textContent || '').trim();
          if (t.indexOf('返回') !== -1 && t.length <= 16) {
            nodes[i].setAttribute('hidden', 'hidden');
          }
        }
      } catch (e) {}
    }

    function report() {
      if (stopped || !rpc) return;
      var raw = measure();
      var h = clamp(raw, min, max);
      if (h === lastReported) return;   // 整数像素去重：子像素抖动不该触发上报
      lastReported = h;
      try {
        var p = rpc('plugin.ui.resize', { height: h });
        if (p && typeof p.catch === 'function') p.catch(function () {});
      } catch (e) {
        /* 通道未就绪/插件不支持该 op：静默（上报是增强，不是功能） */
      }
    }

    function schedule() {
      if (stopped) return;
      if (timer) clearTimeout(timer);
      timer = setTimeout(report, DEBOUNCE_MS);
    }

    /**
     * 把滚动交给宿主：插件文档自己不再滚。
     *
     * 只在宿主声明 `scrollbar: 'host'` 时调用。两层都设 `overflow: hidden`：
     * documentElement 管视口那一层，body 管内容那一层 —— 只设一层时某些浏览器
     * 仍会出现嵌套滚动。**不动任何子元素的 overflow**（表格、列表该滚的照样滚）。
     */
    function applyHostScrollbar() {
      hostScrollbar = true;
      try {
        document.documentElement.style.overflowY = 'hidden';
        document.body.style.overflowY = 'hidden';
      } catch (e) {}
    }

    function acceptHostFrame(frame) {
      if (!frame || typeof frame !== 'object') return;
      if (frame.type !== 'theme') return;
      // 高度区间以宿主为准（与 clampPluginUiHeight 同源，避免两边各写一个数字）
      if (frame.height_range && typeof frame.height_range === 'object') {
        min = toInt(frame.height_range.min, min);
        max = toInt(frame.height_range.max, max);
        schedule();                     // 区间变了，之前的高度可能已被旧区间截断
      }
      if (frame.scrollbar === 'host') applyHostScrollbar();
      /*
       * host_back：宿主工具条已带「返回」，插件内那颗同名按钮要隐藏
       * （2026-11-22 用户截图：两颗按钮上下堆在右上角）。
       * 只隐藏"返回"这一个入口，不碰插件的其它按钮。
       */
      if (frame.host_back) {
        hostBackProvided = true;
        hideOwnBackButton();
      }
    }

    if (typeof global.ResizeObserver === 'function') {
      observer = new global.ResizeObserver(schedule);
      observer.observe(document.body);
    } else {
      global.addEventListener('resize', schedule);
    }
    global.addEventListener('load', schedule);
    schedule();                          // 首屏先报一次

    return {
      /** 立刻重算并上报（插件自己知道内容变了时可直接调）。 */
      report: report,
      /** 透传宿主 theme 帧（主题 / 滚动条归属 / 高度区间）。 */
      acceptHostFrame: acceptHostFrame,
      /** 当前是否已把滚动交给宿主。 */
      get hostScrollbar() { return hostScrollbar; },
      /** 当前生效的高度区间（排障用）。 */
      get range() { return { min: min, max: max }; },
      stop: function () {
        stopped = true;
        if (timer) clearTimeout(timer);
        if (observer) observer.disconnect();
        global.removeEventListener('resize', schedule);
      }
    };
  }

  global.HxPluginUi = { start: start, version: 1 };
})(window);
