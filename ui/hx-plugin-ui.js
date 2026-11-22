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

  /**
   * 自适应布局（2026-11-23 用户：「所有上下页面以及右侧滑动自适应，统一 UI 风格」）。
   *
   * ## 它解决什么
   *
   * 插件页跑在 iframe 里，宿主给的是**整块可用宽度**。各插件自己写 `.wrap
   * { max-width: 880px; margin: 0 auto }` 之类的上限，于是宽屏下内容缩在中间
   * 一条、右侧大片空白 —— 用户在 HXCS、HX-MI 上都报过这个问题。
   *
   * 平台侧的口径（已统一）：`width:100%; max-width:1920px`，超宽屏才居中。
   * 插件要跟它一致，就不能再写自己的 max-width。
   *
   * ## 两件事
   *
   *   ① **横向**：把页面根容器从"固定/居中宽度"改成"撑满可用宽度"，
   *      `box-sizing: border-box` 保证 padding 不撑出横向滚动条；
   *   ② **纵向**：滚动**全部交给平台页**（`scrollbar:'host'` 时插件文档
   *      `overflow-y: hidden`），插件内不造第二个滚动容器 —— 否则会出现
   *      "页面里还有一条内滚动条"，用户明确反对过（双层滚动条）。
   *
   * ## 边界（不砍功能）
   *
   * 只调整**根容器**的宽度与滚动归属；表格、列表这些本来就需要横向滚动的
   * 区域（`overflow-x:auto`）保持原样 —— 把它们的 overflow 关掉会把内容
   * 裁掉，那是"改 UI 砍功能"，用户明令禁止。
   */
  function applyAdaptive(root) {
    if (!root || !root.style) return;
    try {
      // ① 横向撑满：清掉插件自己写的居中上限
      root.style.maxWidth = 'none';
      root.style.width = '100%';
      root.style.marginLeft = '0';
      root.style.marginRight = '0';
      root.style.boxSizing = 'border-box';
      /*
       * ⚠ 这里**不能**碰 overflowY（2026-11-23 实测踩到）。
       *
       * 写成 `root.style.overflowY = 'visible'` 会覆盖 applyHostScrollbar() 刚设好
       * 的 hidden（当 root 就是 body 时尤其致命），结果宿主"滚动归我"的接管控不住
       * 插件文档 —— 实测 getComputedStyle(document.body).overflowY 从 hidden 变回
       * visible，又出现内层滚动条。
       * 纵向归 applyHostScrollbar 一家管，这里只做横向。
       */
    } catch (e) {}
  }

  /** 找页面根容器：优先 .wrap / #app / main，都没有就 body。 */
  function findRoot() {
    var cands = ['.wrap', '#app', 'main', '.container', '.page'];
    for (var i = 0; i < cands.length; i++) {
      var n = document.querySelector(cands[i]);
      if (n) return n;
    }
    return document.body;
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
       * 自适应布局跟随宿主声明（2026-11-23）：宿主说滚动归它，就同时把根容器
       * 撑满可用宽度 —— 两件事是一体的（"上下一条滚动条 + 右侧用满"）。
       */
      if (frame.scrollbar === 'host') applyAdaptive(findRoot());
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
    global.addEventListener('load', function () {
      if (hostScrollbar) applyAdaptive(findRoot());
      schedule();
    });
    // 首屏先做一次自适应 + 上报（等 DOM 就绪再量，避免量到一半的布局）
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function () {
        if (hostScrollbar) applyAdaptive(findRoot());
        schedule();
      });
    } else {
      if (hostScrollbar) applyAdaptive(findRoot());
      schedule();
    }

    return {
      /** 立刻重算并上报（插件自己知道内容变了时可直接调）。 */
      report: report,
      /** 透传宿主 theme 帧（主题 / 滚动条归属 / 高度区间）。 */
      acceptHostFrame: acceptHostFrame,
      /** 当前是否已把滚动交给宿主。 */
      get hostScrollbar() { return hostScrollbar; },
      /** 当前生效的高度区间（排障用）。 */
      get range() { return { min: min, max: max }; },
      /** 手动应用自适应布局（插件自己知道内容变了、或想立刻套用时可调）。 */
      adapt: function () { applyAdaptive(findRoot()); },
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
