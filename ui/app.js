/*
 * HX-MI 插件自带界面（2026-11-09）。
 *
 * # 为什么需要它
 *
 * 平台曾在 66d5657 删掉了 HX-MI 的专属配置页（当时的理由是"插件不属于本仓"），
 * 于是这个插件只剩通用表单 —— 只能改 region / interval / polling / filter 四个
 * 字段。而**账号登录、验证码、账号列表、手动同步**这些核心能力没有入口，
 * 用户看到的就是「配置页面丢失不全」。
 *
 * 正确做法由用户裁定：插件自己的界面由插件自己带（平台零改动、不写死任何
 * 插件专属页）。平台协议已经就绪：plugin.config.get/save、plugin.tool.call、
 * plugin.ui.close、plugin.ui.resize。
 *
 * # 数据流
 *
 *   config.get/save  ←→  region / sync_interval_seconds / polling_enabled / device_filter_models
 *   tool.call        ←→  accounts / login / status / captcha / captcha_image / verify /
 *                        verify_web / logout / account_delete / sync_now / account_sync / regions
 *
 * # 安全边界
 *
 * 密码只在本页停留于 input 元素，提交即随 tool.call 交给宿主（不在 JS 里缓存、
 * 不写 localStorage）。账号口令最终由**宿主保险库**保管，本页从不回读明文。
 */
(function () {
  'use strict';

  /*
   * ⚠ 必须是数字 1，与平台 `PLUGIN_UI_PROTOCOL` 完全一致（2026-11-09 根修）。
   *
   * 这里原来写的是字符串 `'homex.plugin.ui/1'`，而平台发的是数字 `1`
   * （pluginUiProtocol.ts: `PLUGIN_UI_PROTOCOL = 1 as const`）。
   * 本文件的每一处守卫都是严格比较 `m.hx !== HX` ⇒ **平台发来的每一帧都被丢弃**：
   *   · `connect`（不建端口）→ port 永远为 null
   *   · `host-ready`（不发首个请求）→ 界面停在初始占位
   *   · RPC 应答（永远收不到）→ 所有调用超时
   *
   * 用户看到的一切（账号列表「读取中…」、点登录报
   * 「PLUGIN_UI_FRAME_NOT_ATTACHED 还没有连上平台」、界面"全都不对"）
   * 都是这一个字符差异造成的。HXCS 写的是 `var HX = 1`，所以它一直正常。
   */
  var HX = 1;
  var RPC_TIMEOUT_MS = 30000;

  var port = null;
  var connected = false;
  var pending = Object.create(null);
  var seq = 0;
  var pluginId = '';

  /* 账号清单缓存（同步行、添加同步表单都要用它；loadAccounts 填充） */
  var accountsCache = [];
  var accountNamesCache = {};
  /* 当前家庭（平台经 config.get 下发；为 null 时家庭列回落显示 id） */
  var household = null;
  /* 上一次**手动同步**的摘要（新增/更新/云端数）。只用于"本次新增"这一格。 */
  var lastSyncSummary = null;
  /* 未保存的账号级改动：account_id → {household_id?, sync_enabled?}。 */
  var draftEdits = {};
  var dirty = false;

  /* 登录会话状态（session_id 由 login 返回，后续 status/verify 要用） */
  var sessionId = '';
  /* 当前配置（config.get 的结果，保存时整体回写） */
  var config = {};

  var $ = function (id) { return document.getElementById(id); };

  function el(id) { return $(id); }

  function makeErr(code, message) {
    var e = new Error(message || code);
    e.code = code;
    return e;
  }

  /* ── 协议通道 ──────────────────────────────────────────── */

  function nextId() { seq += 1; return 'ui-' + seq + '-' + Math.random().toString(36).slice(2, 8); }

  function send(frame) {
    if (!port) return false;
    try { port.postMessage(frame); return true; } catch (e) { return false; }
  }

  /*
   * ## RPC 在通道就绪前**排队**，不直接失败（2026-11-09 修实测的
   *    `PLUGIN_UI_FRAME_NOT_ATTACHED：还没有连上平台`）
   *
   * 原来 `if (!port) reject(...)` 立刻失败。用户看到的是：刚打开配置页就出现
   * 红色报错「读取账号失败 / 登录失败：还没有连上平台」—— 而实际上只需要再等
   * 一两百毫秒通道就绪。
   *
   * 手抖点得快、iframe 重建、平台重新 mount 都会撞上这个窗口。**"还没连上"
   * 不是错误**，是"还没到时候"。正确语义：排队，通道一就绪立刻发出。
   *
   * 排队上限用 RPC_TIMEOUT_MS（30s）：真的没握手成功时仍要报错，不能让用户
   * 对着一个永远转圈的界面。
   */
  var preReadyQueue = [];

  function flushPreReady() {
    if (!port || !preReadyQueue.length) return;
    var q = preReadyQueue;
    preReadyQueue = [];
    q.forEach(function (job) { job(); });
  }

  function rpc(op, payload, attempt) {
    var tries = attempt || 0;
    return new Promise(function (resolve, reject) {
      if (!port) {
        // 还没连上：排队等通道。超时再如实报错（与"通道坏了"区分开）。
        var queued = true;
        var waitTimer = setTimeout(function () {
          if (!queued) return;
          queued = false;
          reject(makeErr('PLUGIN_UI_FRAME_NOT_ATTACHED',
            '平台通道在 ' + Math.round(RPC_TIMEOUT_MS / 1000) + ' 秒内没有建立（可重新打开配置页）'));
        }, RPC_TIMEOUT_MS);
        preReadyQueue.push(function () {
          if (!queued) return;
          queued = false;
          clearTimeout(waitTimer);
          resolve(rpc(op, payload, tries));
        });
        return;
      }
      var id = nextId();
      var timer = setTimeout(function () {
        if (pending[id]) {
          delete pending[id];
          reject(makeErr('PLUGIN_UI_TIMEOUT', '平台没有在超时内应答'));
        }
      }, RPC_TIMEOUT_MS);
      pending[id] = { resolve: resolve, reject: reject, timer: timer, op: op, payload: payload, attempt: tries };
      var frame = { hx: HX, id: id, op: op };
      if (payload !== undefined) frame.payload = payload;
      if (!send(frame)) {
        delete pending[id];
        clearTimeout(timer);
        reject(makeErr('PLUGIN_UI_FRAME_NOT_ATTACHED', '通信通道尚未建立'));
      }
    });
  }

  function onPortMessage(event) {
    var m = event && event.data;
    if (!m || typeof m !== 'object') return;
    if (m.type === 'ping') {
      send({ hx: HX, type: 'pong', plugin_id: pluginId });
      return;
    }
    // ⑥ 主题下发（2026-11-09）：插件页是 opaque origin，读不到父窗口的
    // document.theme；只能由宿主在消息里告诉它。收到就切 data-theme，
    // 未收到时由 CSS 的 prefers-color-scheme 兜底。
    if (m.type === 'theme' && typeof m.theme === 'string') {
      applyTheme(m.theme);
      return;
    }
    if (m.type === 'host-ready') {
      // 平台明确说明：插件若在 DOMContentLoaded 就发请求，那一帧可能早于
      // 宿主的 attach（rpc 会拿到 PLUGIN_UI_FRAME_NOT_ATTACHED）。
      // 所以**只有**收到这条之后才真正发首个业务请求。
      connected = true;
      startHeightReporting();
      requestBoot();
      // 这是"平台已 attach"的**权威信号**：把之前排队的 RPC 放行。
      flushPreReady();
      return;
    }
    if (typeof m.id === 'string' && typeof m.ok === 'boolean') {
      var p = pending[m.id];
      if (!p) return;
      delete pending[m.id];
      clearTimeout(p.timer);
      if (m.ok) p.resolve(m.result);
      else {
        var err = m.error || {};
        var code = err.code || 'PLUGIN_UI_HANDLER_FAILED';
        var msg = err.message || '平台拒绝了这次操作';
        /*
         * `PLUGIN_UI_FRAME_NOT_ATTACHED` 是**可重试**的（2026-11-09）。
         *
         * 它表示"宿主的通道还没 attach 完"—— 那是时序问题，不是业务结论。
         * 直接抛给用户会看到「读取账号失败：还没有连上平台」，而其实再等
         * 几百毫秒就好了。这里最多重试 3 次（间隔 300ms），仍失败才如实报错。
         *
         * 只对这一个码重试：别的错误（权限、参数、插件自身失败）重试毫无意义，
         * 反而会把一次失败放大成三次。
         */
        if (code === 'PLUGIN_UI_FRAME_NOT_ATTACHED' && (p.attempt || 0) < 3) {
          var next = (p.attempt || 0) + 1;
          setTimeout(function () {
            rpc(p.op, p.payload, next).then(p.resolve, p.reject);
          }, 300);
          // 这次响应已消费：不要走下面的 reject。
          return;
        }
        p.reject(makeErr(code, msg));
      }
    }
  }

  function onWindowMessage(event) {
    var m = event && event.data;
    if (!m || typeof m !== 'object' || m.type !== 'connect' || m.hx !== HX) return;
    if (port) return;
    var p = event.ports && event.ports[0];
    if (!p) return;
    port = p;
    pluginId = typeof m.plugin_id === 'string' ? m.plugin_id : '';
    connected = true;
    port.onmessage = onPortMessage;
    if (typeof port.start === 'function') { try { port.start(); } catch (e) {} }
    startHeightReporting();
    // 注意：这里**不**直接 boot。此刻宿主可能还没 attach 完（host-ready 未到），
    // 立即发 rpc 会拿到 PLUGIN_UI_FRAME_NOT_ATTACHED —— 用户实测的
    // 「读取账号失败：还没有连上平台」就是这么来的。
    // 只有超时兜底路径会在这里之后短暂等待再试（见 requestBoot）。
  }

  window.addEventListener('message', onWindowMessage);

  /* ── 内容高度上报（与语音识别页同一条平台滚动条） ────────── */

  var lastH = 0;
  var hTimer = null;

  function reportHeight() {
    var h = Math.ceil(document.body.getBoundingClientRect().height);
    if (h <= 0 || Math.abs(h - lastH) < 1) return;
    lastH = h;
    rpc('plugin.ui.resize', { height: h }).catch(function () {});
  }

  function scheduleHeight() {
    if (hTimer) clearTimeout(hTimer);
    hTimer = setTimeout(reportHeight, 150);
  }

  function startHeightReporting() {
    reportHeight();
    if (typeof ResizeObserver !== 'function') {
      window.addEventListener('resize', scheduleHeight);
      return;
    }
    var ro = new ResizeObserver(scheduleHeight);
    ro.observe(document.body);
    var wrap = document.querySelector('.wrap');
    if (wrap) ro.observe(wrap);
  }

  /* ── 提示 ──────────────────────────────────────────────── */

  function showBanner(text, kind) {
    var b = el('banner');
    b.textContent = text;
    b.className = 'banner banner--' + (kind || 'err');
    b.hidden = false;
    scheduleHeight();
  }

  function clearBanner() { el('banner').hidden = true; scheduleHeight(); }

  /**
   * 平台把 `/invoke/` 的两种结果都翻译成帧内形状：
   *   · 成功     → `data` 本体
   *   · 业务失败 → `{ok:false, kind, message}`（**200**，不是 HTTP 错误）
   *
   * 所以每个 `.then(res => ...)` 都必须先过这一关。旧代码多处只认成功形状，
   * 结果把"插件报告的业务失败"静默当成"没有数据"（2026-11-09 实测：
   * 账号列表卡在读取中 / 同步结果不显示原因）。
   */
  function failed(res) {
    return !!(res && res.ok === false);
  }

  /** 把失败帧转成一句可读的话（含 kind，便于排障）。 */
  function failText(res) {
    if (!res) return '插件未返回内容';
    var msg = res.message || '';
    var kind = res.kind || '';
    if (msg && kind) return msg + '（' + kind + '）';
    return msg || kind || '插件未说明原因';
  }

  function errText(e) {
    if (!e) return '未知错误';
    return (e.code ? e.code + '：' : '') + (e.message || String(e));
  }

  /* ── 配置读写 ──────────────────────────────────────────── */

  /*
   * 同步间隔在**界面**上用「分钟」，配置里存「秒」。
   *
   * 为什么：用户口径与既有设计都是「每 N 分钟」（mockup 的标题行写的是"每 5 分钟"）。
   * 秒是存储单位，不该出现在界面上 —— 之前那个"同步间隔（秒）"让人要自己换算。
   * 换算只在这两个函数里做，别处一律用分钟。
   */
  var MIN_PER_MINUTE = 60;

  function renderConfig() {
    el('f-region').value = config.region || 'cn';
    var f = config.device_filter_models;
    el('f-filter').value = Array.isArray(f) ? f.join(', ') : '';
  }

  /** 自动同步开关（全局：驱动宿主后台任务的 polling_enabled）。 */
  function setPolling(on) {
    el('sw-polling').setAttribute('aria-checked', on ? 'true' : 'false');
    el('polling-row').setAttribute('data-on', on ? '1' : '0');
    el('polling-t').textContent = on ? '自动同步' : '仅手动';
  }

  function pollingOn() {
    return el('sw-polling').getAttribute('aria-checked') === 'true';
  }

  function collectConfig() {
    var out = Object.assign({}, config);
    out.region = el('f-region').value || 'cn';
    // 全局节奏**由条目推导**（f-interval 现在是"添加同步"表单里的值，
    // 不再是全局设置 —— 从它读全局间隔会把最后填过的那个数字当成整体节奏）。
    var eff = effectiveCadence();
    out.sync_interval_seconds = eff.seconds;
    out.polling_enabled = eff.enabled;
    out.device_filter_models = el('f-filter').value
      .split(',')
      .map(function (s) { return s.trim(); })
      .filter(function (s) { return s !== ''; });
    return out;
  }

  /*
   * 保存配置（页头**唯一**的保存入口，2026-11-10 用户口径）。
   *
   * 一次点击要提交两处状态：
   *   ① 插件配置（区域 / 同步间隔 / 自动同步 / 型号过滤）→ plugin.config.save
   *   ② 列表里的账号级改动（家庭 / 自动同步）→ 逐条 account_settings
   *
   * 顺序上先存配置：配置里含 `sync_interval_seconds`，平台会据此重新下发配置并
   * 让宿主按新周期调度；账号级改动随后各存一次（带各自 revision，避免覆盖）。
   *
   * 失败要**分别**说：账号设置失败不该让人以为整份配置也没保存 —— 那是两件事，
   * 而且只有一处需要重试。
   */
  function saveConfig() {
    el('btn-save').disabled = true;
    clearBanner();
    var edits = [];
    for (var id in draftEdits) {
      if (Object.prototype.hasOwnProperty.call(draftEdits, id)) edits.push({ id: id, patch: draftEdits[id] });
    }
    rpc('plugin.config.save', { config: collectConfig() })
      .then(function () {
        if (!edits.length) return { saved: 0, failed: [] };
        return applyAccountEdits(edits);
      })
      .then(function (r) {
        draftEdits = {};
        markDirty(false);
        if (r.failed.length) {
          showBanner('配置已保存，但 ' + r.failed.length + ' 条账号设置未保存：' + r.failed[0], 'warn');
        } else {
          showBanner('配置已保存（热生效）' + (r.saved ? '，含 ' + r.saved + ' 条账号设置' : ''), 'ok');
        }
        loadAccounts();
      })
      .catch(function (e) { showBanner('保存失败：' + errText(e), 'err'); })
      .then(function () {
        el('btn-save').disabled = false;
        markDirty(dirty && Object.keys(draftEdits).length > 0);
      });
  }

  /** 逐条提交账号级改动。串行：同一账号的多次改动会互相覆盖 revision。 */
  function applyAccountEdits(edits) {
    var saved = 0;
    // 名字刻意不叫 failed：本文件里 `failed(res)` 是"业务失败帧"的判定函数，
    // 用同名局部变量会把它遮住（写这段时就是这么踩了一下）。
    var failures = [];
    function step(i) {
      if (i >= edits.length) return { saved: saved, failed: failures };
      var e = edits[i];
      var row = null;
      for (var k = 0; k < accountsCache.length; k += 1) {
        if ((accountsCache[k].account_id || '') === e.id) { row = accountsCache[k]; break; }
      }
      var args = { account_id: e.id };
      if (row && row.revision !== undefined) args.revision = row.revision;
      if (e.patch.household_id !== undefined) args.household_id = e.patch.household_id;
      if (e.patch.sync_enabled !== undefined) args.sync_enabled = e.patch.sync_enabled;
      if (e.patch.interval_seconds !== undefined) args.interval_seconds = e.patch.interval_seconds;
      return rpc('plugin.tool.call', { tool: 'account_settings', args: args, account_id: e.id })
        .then(function (res) {
          if (failed(res)) failures.push(failText(res)); else saved += 1;
          return step(i + 1);
        })
        .catch(function (err) { failures.push(errText(err)); return step(i + 1); });
    }
    return Promise.resolve(step(0)).then(function (r) { return r || { saved: saved, failed: failures }; });
  }

  /* ── 账号列表 ──────────────────────────────────────────── */

  /*
   * 账号状态文案（v2 多账号的 state 枚举 → 用户看得懂的字）。
   *
   * 为什么单独列：state 是**机器值**（ready/ready_unbound/auth_required…），
   * 直接显示会让用户看到"ready_unbound"这种内部措辞；而全角中文映射放这里
   * 一处，将来加状态也只改一张表。
   */
  var ACCOUNT_STATE_TEXT = {
    pending_login: { text: '登录中', warn: true },
    ready_unbound: { text: '待绑定家庭', warn: true },
    ready: { text: '就绪', warn: false },
    auth_required: { text: '需重新登录', warn: true },
    disabled: { text: '已停用', warn: true },
    deleting: { text: '删除中', warn: true },
    error: { text: '异常', warn: true }
  };

  function accountStateText(a) {
    var raw = (a && (a.state || a.State)) || '';
    if (!raw) return null;
    return ACCOUNT_STATE_TEXT[raw] || { text: String(raw), warn: true };
  }

  function renderAccounts(data) {
    var box = el('accounts');
    /*
     * ⚠ 必须处理业务失败帧（2026-11-09 实测「账号列表卡在读取中」）。
     *
     * 平台把 `/invoke/` 的两种结果都翻译成帧内形状：
     *   · 成功    → `data` 本体（如 `{accounts:[...]}`）
     *   · 业务失败 → `{ok:false, kind, message}`（200，不是 HTTP 错误）
     *
     * 旧代码只认 `data.accounts`，于是收到失败帧时 `items` 为空数组、
     * 走进"还没有登录任何小米账号"分支 —— 而**更糟的是**：真实故障
     * （如"账号主密钥不可用"）被静默说成"没有账号"，用户以为没登录过。
     *
     * 现在：失败帧如实显示原因，用户与排障者都能看到真话。
     */
    if (data && data.ok === false) {
      box.innerHTML = '';
      var errP = document.createElement('p');
      errP.className = 'banner banner--err';
      errP.textContent = '读取账号失败：' + (data.message || data.kind || '插件未说明原因');
      box.appendChild(errP);
      scheduleHeight();
      return;
    }
    var items = (data && data.accounts) || [];
    var names = (data && data.account_names) || {};
    if (!items.length) {
      box.innerHTML = '<p class="muted">还没有登录任何小米账号。</p>';
      scheduleHeight();
      return;
    }
    box.innerHTML = '';
    accountsCache = items;
    accountNamesCache = names;
    items.forEach(function (a) {
      var id = a.account_id || a.AccountID || '';
      var name = names[id] || a.masked_account || a.MaskedAccount || id;
      var row = document.createElement('div');
      row.className = 'acct';

      var ava = document.createElement('span');
      ava.className = 'ava';
      ava.setAttribute('aria-hidden', 'true');
      ava.textContent = '米';
      row.appendChild(ava);

      var t = document.createElement('div');
      t.className = 'acct__t';
      var b = document.createElement('b');
      b.textContent = name;
      var s = document.createElement('span');
      s.textContent = id;
      t.appendChild(b);
      t.appendChild(s);
      row.appendChild(t);

      var spacer = document.createElement('span');
      spacer.className = 'spacer';
      row.appendChild(spacer);

      var st = accountStateText(a);
      if (st) {
        var pill = document.createElement('span');
        pill.className = 'pill' + (a.state === 'ready' ? ' pill--ok' : (st.warn ? ' pill--warn' : ''));
        var dot = document.createElement('i');
        pill.appendChild(dot);
        pill.appendChild(document.createTextNode(st.text));
        row.appendChild(pill);
      }

      // 退出登录放在**行内**（2026-11-10 用户口径）：它操作的是这个账号那条
      // 会话，贴着账号比挂在卡头更好找。语义上是"清掉云端会话票据"，不是删账号。
      var lo = document.createElement('button');
      lo.type = 'button';
      lo.className = 'btn btn--sm';
      lo.textContent = '退出登录';
      lo.addEventListener('click', function () { logoutAccount(name); });
      row.appendChild(lo);

      var del = document.createElement('button');
      del.type = 'button';
      del.className = 'btn btn--sm btn--danger';
      del.textContent = '删除';
      del.addEventListener('click', function () { removeAccount(id, a.revision); });
      row.appendChild(del);

      box.appendChild(row);
    });
    renderSyncRows(items, names);
    scheduleHeight();
  }

  /* ── 同步：条目列表 / 结果 / 添加 ─────────────────────────
     一条同步 = 「账号 → 家庭」。表结构照既有设计（mockup config-preview
     与平台版 XiaomiAccountList）：账号、家庭**分列**，另有自动同步与操作列；
     家庭可就地改（选了就存，见 saveAccountSettingsFor）。 */

  /** 家庭显示名：平台没给名字时回落显示 id 的前 8 位（可核对，不误导）。 */
  function householdName(id) {
    if (!id) return '未绑定';
    if (household && household.id === id && household.name) return household.name;
    return '其他家庭（' + String(id).slice(0, 8) + '…）';
  }

  /** 家庭下拉：只能选**当前**家庭（插件是家庭作用域的，跨家庭调用平台不允许）。 */
  function fillHouseholdSelect(sel, currentId) {
    sel.innerHTML = '';
    var opt = document.createElement('option');
    if (!household) {
      opt.value = currentId || '';
      opt.textContent = currentId ? String(currentId).slice(0, 8) + '…' : '未绑定';
      sel.appendChild(opt);
      sel.disabled = true;
      return;
    }
    opt.value = household.id;
    opt.textContent = household.name;
    sel.appendChild(opt);
    sel.disabled = false;
  }

  /*
   * 读一条账号的同步设置。
   *
   * ⚠ 真实形状是**嵌套**的：`{"sync":{"enabled":true,"interval_seconds":300}}`
   * （account_store.go 的 account.Sync 是 syncSettings 结构体）。
   * 界面此前按扁平 `a.sync_enabled` 读 —— 永远取到 undefined，于是每一行都显示
   * 「关闭/仅手动」，实际开着也看不出来。这里两种形状都认（平台将来若改成扁平
   * 也不会立刻坏），并以嵌套为准。
   */
  function accountSync(a) {
    var s = (a && (a.sync || a.Sync)) || {};
    var enabled;
    if (typeof s.enabled === 'boolean') enabled = s.enabled;
    else if (typeof a.sync_enabled === 'boolean') enabled = a.sync_enabled;
    else enabled = false;
    var iv = parseInt(s.interval_seconds, 10);
    if (isNaN(iv) || iv <= 0) iv = parseInt(a.interval_seconds, 10);
    if (isNaN(iv) || iv <= 0) iv = 300;
    return { enabled: enabled, intervalSeconds: iv };
  }

  /** 一条同步的最终值（草稿优先于服务端返回）。 */
  function entryValue(a) {
    var d = draftEdits[a.account_id || ''] || {};
    var base = accountSync(a);
    return {
      householdId: d.household_id !== undefined ? d.household_id : (a.household_id || ''),
      enabled: d.sync_enabled !== undefined ? d.sync_enabled : base.enabled,
      intervalSeconds: d.interval_seconds !== undefined ? d.interval_seconds : base.intervalSeconds
    };
  }

  /*
   * 由**条目**推导全局节奏（宿主只注册一个后台任务）。
   *
   * 每条同步各有自己的周期（用户口径：在「添加同步」里设定），但宿主只有一个
   * account-sync 任务 —— 所以取"最勤快的那条"作为任务周期：这样没有任何一条
   * 会被饿着（取最慢的会让勤快的条目失效，取平均则两边都不对）。
   * 一条自动同步都没有 ⇒ 不注册任务（polling_enabled=false）。
   */
  function effectiveCadence() {
    var minSec = 0;
    var any = false;
    accountsCache.forEach(function (a) {
      var v = entryValue(a);
      if (!v.householdId || !v.enabled) return;
      any = true;
      if (minSec === 0 || v.intervalSeconds < minSec) minSec = v.intervalSeconds;
    });
    if (!any) return { enabled: false, seconds: fallbackInterval() };
    return { enabled: true, seconds: minSec };
  }

  function fallbackInterval() {
    var sec = parseInt(config.sync_interval_seconds, 10);
    return (isNaN(sec) || sec < 60) ? 300 : sec;
  }

  function renderSyncRows(items, names) {
    var box = el('sync-rows');
    box.innerHTML = '';
    /*
     * 列表 = **同步条目**（已绑定家庭的账号），不是账号清单。
     *
     * 为什么这样分：一个「同步」就是"某账号 → 某家庭"这条关系。没绑定家庭的
     * 账号在同步这里没有可执行的事，把它列进来只会让人以为它已经在同步。
     * 账号本身仍在上面「小米账号」卡里可见、可删。
     */
    var bound = items.filter(function (a) { return !!entryValue(a).householdId; });
    var cnt = el('sync-count');
    if (cnt) cnt.textContent = bound.length ? bound.length + ' 条同步' : '';

    // 空列表就**留空**（2026-11-10 用户口径：「同步列表空的就应该显示空白」）。
    // 不写占位文案：入口（＋ 添加同步）就在卡头，空态自己会说话；
    // 再补一句"还没有…"只是把一行噪音塞进一个本来就该干净的区域。
    if (!bound.length) return;
    bound.forEach(function (a) {
      var id = a.account_id || '';
      var name = names[id] || a.masked_account || id;
      var draft = draftEdits[id] || {};

      var row = document.createElement('div');
      row.className = 'srow';

      var left = document.createElement('div');
      left.className = 'srow__a';
      var b = document.createElement('b');
      b.textContent = name;
      var sub = document.createElement('span');
      var st = accountStateText(a);
      // 周期是这一条的属性，行内要看得见 —— 否则用户在列表里根本不知道它会多久跑一次。
      var mins = Math.max(1, Math.round(val.intervalSeconds / MIN_PER_MINUTE));
      sub.textContent = (a.region ? '区域 ' + a.region + ' · ' : '') +
        (st ? st.text + ' · ' : '') + '每 ' + mins + ' 分钟';
      left.appendChild(b);
      left.appendChild(sub);
      row.appendChild(left);

      var mid = document.createElement('div');
      mid.className = 'srow__h';
      var sel = document.createElement('select');
      sel.setAttribute('aria-label', '同步目标家庭');
      fillHouseholdSelect(sel, val.householdId);
      sel.addEventListener('change', function () {
        setDraft(id, { household_id: sel.value || null });
      });
      mid.appendChild(sel);
      row.appendChild(mid);

      var ops = document.createElement('div');
      ops.className = 'srow__o';

      var val = entryValue(a);
      var on = val.enabled;
      var swWrap = document.createElement('span');
      swWrap.className = 'switch-row';
      var sw = document.createElement('button');
      sw.type = 'button';
      sw.className = 'switch';
      sw.setAttribute('role', 'switch');
      sw.setAttribute('aria-checked', on ? 'true' : 'false');
      sw.appendChild(document.createElement('i'));
      if (on) swWrap.setAttribute('data-on', '1');
      var swT = document.createElement('span');
      swT.textContent = on ? '开启' : '关闭';
      sw.addEventListener('click', function () {
        var next = sw.getAttribute('aria-checked') !== 'true';
        setDraft(id, { sync_enabled: next });
        sw.setAttribute('aria-checked', next ? 'true' : 'false');
        swT.textContent = next ? '开启' : '关闭';
        if (next) swWrap.setAttribute('data-on', '1'); else swWrap.removeAttribute('data-on');
      });
      swWrap.appendChild(sw);
      swWrap.appendChild(swT);
      ops.appendChild(swWrap);

      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn--sm';
      btn.textContent = '手动同步';
      btn.addEventListener('click', function () { syncNowFor(id, btn); });
      ops.appendChild(btn);

      var del = document.createElement('button');
      del.type = 'button';
      del.className = 'btn btn--sm btn--danger';
      del.textContent = '删除';
      del.addEventListener('click', function () { removeSyncEntry(a, name); });
      ops.appendChild(del);

      row.appendChild(ops);
      box.appendChild(row);
    });
  }

  /*
   * 草稿机制（2026-11-10 用户口径：保存收敛到页头「保存配置」）。
   *
   * 列表里的改动（家庭 / 自动同步）先记在 draftEdits，**不立即发请求**；
   * 页头「保存配置」一次性提交：插件配置 + 每条账号设置。
   *
   * 代价必须说清：改完不点保存就不会生效。所以两件事一起做——
   *   ① 页头按钮加"有未保存改动"标记（见 markDirty）；
   *   ② 列表下方给一行提示。
   * 只做前者会让人以为按钮是装饰，只做后者会被忽略。
   */
  function setDraft(accountId, patch) {
    var d = draftEdits[accountId] || {};
    for (var k in patch) { if (Object.prototype.hasOwnProperty.call(patch, k)) d[k] = patch[k]; }
    draftEdits[accountId] = d;
    markDirty(true);
  }

  function markDirty(on) {
    dirty = !!on;
    var btn = el('btn-save');
    btn.textContent = dirty ? '保存配置 •' : '保存配置';
    btn.title = dirty ? '有未保存的改动' : '';
    var hint = el('dirty-hint');
    if (hint) hint.hidden = !dirty;
  }

  /*
   * 删除一条同步 = **解绑家庭**（不删账号凭据）。
   *
   * 为什么不复用「删除账号」：那是销毁凭据、要重新登录；用户想清除的往往
   * 只是"别把这个家同步进来"。两者代价差一个登录流程，必须分开并写清。
   */
  function removeSyncEntry(a, name) {
    if (!window.confirm('移除同步「' + name + ' → ' + householdName(a.household_id) + '」？\n\n' +
        '只解除这条同步关系并关闭它的自动同步；账号凭据保留，之后可以再添加回来。')) return;
    setDraft(a.account_id || '', { household_id: null, sync_enabled: false });
    // 立即从列表里去掉（草稿视角），让"删除"当场可见。
    renderSyncRows(accountsCache, accountNamesCache);
    scheduleHeight();
  }

  /*
   * 改账号级设置（家庭 / 自动同步）。
   *
   * # 为什么选完就存，而不是攒到「保存配置」
   *
   * 「这个账号同步进哪个家」是**一条关系**，不是一份草稿：用户选了就该生效。
   * 攒着不存会让他以为换了家庭、而下次同步仍进旧家庭（平台版实现当初就是
   * 因为这个原因改成即时保存并回读的）。
   *
   * # account_id 必须显式带上
   *
   * account_settings 在 manifest 里声明为 `scope: "account"`，平台要求调用
   * 上下文带 account_id（`plugin.tool.call` 的 account_id 字段）。
   * 不带就必然撞 `账号级工具 "account_settings" 缺少 account_id`。
   */
  function saveAccountSettingsFor(accountId, revision, patch) {
    var args = { account_id: accountId };
    if (revision !== undefined && revision !== null) args.revision = revision;
    if (patch.household_id !== undefined) args.household_id = patch.household_id;
    if (patch.sync_enabled !== undefined) args.sync_enabled = patch.sync_enabled;
    if (patch.interval_seconds !== undefined) args.interval_seconds = patch.interval_seconds;
    clearBanner();
    rpc('plugin.tool.call', { tool: 'account_settings', args: args, account_id: accountId })
      .then(function (res) {
        if (failed(res)) { showBanner('设置未保存：' + failText(res), 'err'); return; }
        showBanner('已保存', 'ok');
        // 回读：以服务端值重渲染，避免界面停在"我以为改了"的状态。
        loadAccounts();
      })
      .catch(function (e) { showBanner('设置未保存：' + errText(e), 'err'); })
      .then(function () { scheduleHeight(); });
  }

  function renderSyncResult(view) {
    var sync = (view && view.sync) || {};
    var tiles = el('res-tiles');
    tiles.innerHTML = '';
    addTile(tiles, sync.devices || 0, '云端设备', '');
    addTile(tiles, sync.controllable || 0, '可本地控制', 'ok');
    addTile(tiles, sync.uncontrollable || 0, '不可控', 'warn');
    // 第 4 格是**离线设备数**（2026-11-10 用户口径：原来那格「本次新增」换成它）。
    //
    // 数据来源是设备明细里的 online 标志 —— 与表格「在线状态」列**同一个判据**，
    // 所以这一格的数字和下面表里数出来的行数一定一致（两处各算一遍必然对不上）。
    // online 缺省视为未知，不计入离线。
    var offline = 0;
    (sync.device_list || []).forEach(function (d) { if (d.online === false) offline += 1; });
    addTile(tiles, offline, '离线设备', offline > 0 ? 'warn' : '');

    var when = el('res-when');
    var parts = [];
    parts.push('最近同步 ' + (sync.last_ok_at ? fmtTime(sync.last_ok_at) : '尚未同步'));
    parts.push('下次自动同步 ' + (sync.next_sync_at ? fmtTime(sync.next_sync_at) : (sync.enabled ? '计算中' : '已关闭')));
    when.textContent = parts.join(' · ');

    var pill = el('res-state');
    pill.className = 'pill' + (sync.running ? ' pill--ok' : '');
    el('res-state-t').textContent = sync.running ? '同步中' : '空闲';

    // 连续失败与最后错误：它们才是"为什么一直没同步上"的答案，藏起来只会
    // 让用户反复点「手动同步」。
    var fail = el('res-fail');
    if ((sync.consecutive_failures || 0) > 0 || sync.last_error) {
      fail.hidden = false;
      fail.textContent = '连续失败 ' + (sync.consecutive_failures || 0) + ' 次' +
        (sync.last_error ? '：' + sync.last_error : '');
    } else {
      fail.hidden = true;
    }

    renderDeviceTable(sync.device_list || []);
    scheduleHeight();
  }

  function addTile(box, value, label, kind) {
    var d = document.createElement('div');
    d.className = 'tile' + (kind ? ' tile--' + kind : '');
    var b = document.createElement('b');
    b.textContent = String(value);
    var sp = document.createElement('span');
    sp.textContent = label;
    d.appendChild(b);
    d.appendChild(sp);
    box.appendChild(d);
  }

  /** 设备明细表：在线状态用 online（**不是** available —— 那是"云端是否仍可见"）。 */
  function renderDeviceTable(list) {
    var box = el('res-devices');
    box.innerHTML = '';
    if (!list.length) {
      var p = document.createElement('p');
      p.className = 'muted small';
      p.textContent = '暂无设备结果。手动同步一次后这里会列出云端返回的设备。';
      box.appendChild(p);
      return;
    }
    var t = document.createElement('table');
    var thead = document.createElement('thead');
    var htr = document.createElement('tr');
    [['设备', ''], ['型号', ''], ['在线状态', 'col-online'], ['控制状态', 'col-ctl']].forEach(function (c) {
      var th = document.createElement('th');
      th.textContent = c[0];
      if (c[1]) th.className = c[1];
      htr.appendChild(th);
    });
    thead.appendChild(htr);
    t.appendChild(thead);
    var tb = document.createElement('tbody');
    list.forEach(function (d) {
      var tr = document.createElement('tr');
      var td1 = document.createElement('td');
      var b = document.createElement('b');
      b.textContent = d.name || d.device_key || '未命名';
      td1.appendChild(b);
      tr.appendChild(td1);

      var td2 = document.createElement('td');
      td2.className = 'mono';
      td2.textContent = d.model || '—';
      tr.appendChild(td2);

      var td3 = document.createElement('td');
      td3.className = 'col-online';
      var ol = document.createElement('span');
      ol.className = 'ol ' + (d.online ? 'ol--on' : 'ol--off');
      var dot = document.createElement('i');
      ol.appendChild(dot);
      ol.appendChild(document.createTextNode(d.online ? '在线' : '离线'));
      td3.appendChild(ol);
      tr.appendChild(td3);

      var td4 = document.createElement('td');
      td4.className = 'col-ctl';
      var tag = document.createElement('span');
      var controllable = d.controllable !== false && !d.reason && !d.reason_text;
      tag.className = 'tag ' + (controllable ? 'tag--ok' : 'tag--warn');
      tag.textContent = controllable ? '可控' : ('不可控 · ' + (d.reason_text || d.reason || '原因未说明'));
      td4.appendChild(tag);
      tr.appendChild(td4);

      tb.appendChild(tr);
    });
    t.appendChild(tb);
    box.appendChild(t);
  }

  function fmtTime(v) {
    if (!v) return '—';
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  /** 读一次"同步结果"（账号工具的家庭级视图：是运行事实，不是配置）。 */
  function loadSyncResult() {
    rpc('plugin.tool.call', { tool: 'account', args: {} })
      .then(function (res) {
        if (failed(res)) {
          el('res-devices').innerHTML = '<p class="banner banner--warn">读取同步结果失败：' +
            esc(failText(res)) + '</p>';
          scheduleHeight();
          return;
        }
        renderSyncResult(res);
      })
      .catch(function (e) {
        el('res-devices').innerHTML = '<p class="banner banner--warn">读取同步结果失败：' +
          esc(errText(e)) + '</p>';
        scheduleHeight();
      });
  }

  function esc(s) {
    return String(s === undefined || s === null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  function loadAccounts() {
    rpc('plugin.tool.call', { tool: 'accounts', args: {} })
      .then(function (res) { renderAccounts(res); })
      .catch(function (e) {
        el('accounts').innerHTML = '';
        var p = document.createElement('p');
        p.className = 'muted';
        p.textContent = '读取账号失败：' + errText(e);
        el('accounts').appendChild(p);
        scheduleHeight();
      });
  }

  function removeAccount(id, revision) {
    if (!id) return;
    if (!window.confirm('删除这个账号？其云端会话与凭据会一并清除。')) return;
    rpc('plugin.tool.call', {
      tool: 'account_delete',
      args: { account_id: id, revision: revision || 0, confirm: true },
    })
      .then(function () { loadAccounts(); showBanner('账号已删除', 'ok'); })
      .catch(function (e) { showBanner('删除失败：' + errText(e), 'err'); });
  }

  /* ── 登录流程 ──────────────────────────────────────────── */

  function startLogin() {
    var user = el('f-user').value.trim();
    var pass = el('f-pass').value;
    if (!user || !pass) {
      showBanner('账号和密码都不能为空', 'err');
      return;
    }
    el('btn-login').disabled = true;
    clearBanner();
    rpc('plugin.tool.call', {
      tool: 'login',
      args: { region: el('f-region').value || 'cn', user: user, password: pass },
    })
      .then(function (res) {
        // 提交后立刻清掉密码：既然已经交给宿主，本页不该再留着它。
        el('f-pass').value = '';
        // 业务失败帧（如"密码错误"）必须如实显示 —— 静默会把失败说成"已提交"。
        if (failed(res)) {
          el('login-hint').hidden = false;
          el('login-hint').textContent = '登录失败：' + failText(res);
          scheduleHeight();
          return;
        }
        sessionId = (res && res.session_id) || '';
        var state = (res && res.state) || '';
        el('login-hint').hidden = false;
        el('login-hint').textContent = '登录状态：' + (state || '已提交');
        scheduleHeight();
        refreshChallenge(state);
      })
      .catch(function (e) { showBanner('登录失败：' + errText(e), 'err'); })
      .then(function () { el('btn-login').disabled = false; });
  }

  function refreshChallenge(state) {
    if (!sessionId) return;
    rpc('plugin.tool.call', { tool: 'status', args: { session_id: sessionId } })
      .then(function (st) {
        var s = (st && st.state) || state || '';
        if (s === 'need_verify' || s === 'need_captcha') {
          showChallenge(s, st);
        } else if (s === 'completed' || s === 'success') {
          el('challenge').hidden = true;
          el('login-form').hidden = true;
          showBanner('登录成功', 'ok');
          loadAccounts();
        } else {
          el('challenge').hidden = true;
          if (s === 'failed') {
            showBanner('登录失败（' + ((st && st.reason) || '原因未提供') + '）', 'err');
          }
        }
        scheduleHeight();
      })
      .catch(function () {});
  }

  function showChallenge(state, st) {
    el('challenge').hidden = false;
    el('ch-title').textContent = state === 'need_captcha' ? '需要输入图形验证码' : '需要短信/邮件验证';
    el('ch-desc').textContent = (st && st.reason) || '请完成验证以继续登录。';
    el('ch-web-row').hidden = state !== 'need_verify';
    el('btn-verify-web').hidden = state !== 'need_verify';
    scheduleHeight();
    loadCaptchaImage();
  }

  function loadCaptchaImage() {
    rpc('plugin.tool.call', { tool: 'captcha_image', args: { session_id: sessionId } })
      .then(function (res) {
        if (failed(res)) {
          el('ch-img').hidden = true;
          showBanner('取验证码图片失败：' + failText(res), 'err');
          return;
        }
        var b64 = (res && (res.image_base64 || res.image)) || '';
        if (!b64) {
          showBanner('插件没有返回验证码图片', 'warn');
          return;
        }
        var img = el('ch-img');
        img.src = b64.indexOf('data:') === 0 ? b64 : 'data:image/png;base64,' + b64;
        img.hidden = false;
        scheduleHeight();
      })
      .catch(function () { /* 没有图片不算错误：短信渠道本来就没有 */ });
  }

  function submitVerify() {
    var code = el('f-code').value.trim();
    if (!code) { showBanner('请先输入验证码', 'err'); return; }
    el('btn-verify').disabled = true;
    rpc('plugin.tool.call', { tool: 'verify', args: { session_id: sessionId, code: code } })
      .then(function (res) {
        el('f-code').value = '';
        if (failed(res)) {
          showBanner('验证失败：' + failText(res), 'err');
          scheduleHeight();
          return;
        }
        refreshChallenge((res && res.state) || '');
      })
      .catch(function (e) { showBanner('验证失败：' + errText(e), 'err'); })
      .then(function () { el('btn-verify').disabled = false; });
  }

  function submitVerifyWeb() {
    var url = el('f-web').value.trim();
    if (!url) { showBanner('请先粘贴验证链接', 'err'); return; }
    el('btn-verify-web').disabled = true;
    rpc('plugin.tool.call', { tool: 'verify_web', args: { session_id: sessionId, url: url } })
      .then(function (res) {
        el('f-web').value = '';
        refreshChallenge((res && res.state) || '');
      })
      .catch(function (e) { showBanner('验证失败：' + errText(e), 'err'); })
      .then(function () { el('btn-verify-web').disabled = false; });
  }

  /* ── 同步与诊断 ────────────────────────────────────────── */

  /*
   * 手动同步一个账号。
   *
   * `account_sync` 在 manifest 里是 `scope: "account"` —— 必须显式带 account_id，
   * 否则平台回 `账号级工具 "account_sync" 缺少 account_id`（用户实测过的那条）。
   *
   * 同步完成后**重读账号与同步结果**：只显示一句"同步完成"而不刷新列表，
   * 用户看到的是"同步了但设备列表没动"—— 那正是旧实现的观感问题。
   */
  function syncNowFor(accountId, btn) {
    if (btn) { btn.disabled = true; btn.textContent = '同步中…'; }
    clearBanner();
    var args = accountId ? { account_id: accountId } : {};
    var payload = { tool: 'account_sync', args: args };
    if (accountId) payload.account_id = accountId;
    rpc('plugin.tool.call', payload)
      .then(function (res) {
        // 业务失败帧与"状态不是 succeeded"是两件不同的事，都要如实说。
        if (failed(res)) {
          showBanner('同步失败：' + failText(res), 'err');
        } else if (res && res.state === 'succeeded') {
          var sum = res.summary || {};
          lastSyncSummary = sum;
          showBanner('同步完成：云端 ' + (sum.cloud || 0) + ' 台 · 新增 ' + (sum.added || 0) +
            ' · 更新 ' + (sum.updated || 0) + (sum.failed ? ' · 失败 ' + sum.failed : ''), 'ok');
        } else {
          showBanner('同步失败：' + ((res && res.error) || '原因未提供'), 'err');
        }
        loadSyncResult();
        loadAccounts();
      })
      .catch(function (e) { showBanner('同步失败：' + errText(e), 'err'); })
      .then(function () {
        if (btn) { btn.disabled = false; btn.textContent = '手动同步'; }
        scheduleHeight();
      });
  }

  /* ── 添加同步（把某个已有账号绑到当前家庭）─────────────────
     语义与「添加账号」不同：那个是**登录一个新账号**，这个是把**已有账号**
     指到某个家庭。平台的插件调用是家庭作用域的，所以家庭下拉只有当前家庭。 */
  function openAddSync() {
    var accSel = el('f-sync-account');
    accSel.innerHTML = '';
    if (!accountsCache.length) {
      showBanner('还没有账号可绑定。先在上面「添加账号」登录一个小米账号。', 'warn');
      return;
    }
    accountsCache.forEach(function (a) {
      var id = a.account_id || '';
      var opt = document.createElement('option');
      opt.value = id;
      opt.textContent = accountNamesCache[id] || a.masked_account || id;
      accSel.appendChild(opt);
    });
    fillHouseholdSelect(el('f-sync-household'), '');
    var note = el('addsync-note');
    if (household) {
      note.textContent = '同一账号可同步到多个家庭；这里绑定的是当前家庭「' + household.name + '」。';
    } else {
      note.textContent = '平台尚未提供家庭信息，暂时无法绑定家庭。';
    }
    // 默认值取当前配置的周期与自动开关：多数情况下用户想加的就是同样的节奏，
    // 从零填一遍是多余的动作。
    var sec = parseInt(config.sync_interval_seconds, 10);
    if (isNaN(sec) || sec <= 0) sec = 300;
    el('f-interval').value = Math.max(1, Math.round(sec / MIN_PER_MINUTE));
    setPolling(config.polling_enabled !== false);
    el('addsync-form').hidden = false;
    scheduleHeight();
  }

  /*
   * 提交「添加同步」。
   *
   * 「同步时间」与「自动同步」是这一条同步的属性（用户口径：在添加时设定），
   * 与账号、家庭一起写进账号设置：
   *   · household_id      —— 这条同步指向哪个家
   *   · interval_seconds  —— 这条同步的周期（界面用分钟，存储用秒）
   *   · sync_enabled      —— 是否自动
   *
   * 值本身先记进草稿、随页头「保存配置」一起提交（用户口径：保存公用保存配置）。
   */
  function submitAddSync() {
    var id = el('f-sync-account').value;
    if (!id) { showBanner('请选择要绑定的账号', 'warn'); return; }
    var hh = el('f-sync-household').value;
    if (!hh) { showBanner('平台没有提供可选家庭，无法绑定', 'err'); return; }
    var min = parseInt(el('f-interval').value, 10);
    if (isNaN(min) || min < 1) min = 5;
    if (min > 60) min = 60;
    setDraft(id, {
      household_id: hh,
      interval_seconds: min * MIN_PER_MINUTE,
      sync_enabled: pollingOn()
    });
    el('addsync-form').hidden = true;
    // 立刻按草稿重渲染，让"加进去了"当场可见。
    renderSyncRows(accountsCache, accountNamesCache);
    scheduleHeight();
  }

  /* ── 设备目录 / 局域网探测（2026-11-09 补：原来界面没有入口）── */

  function loadCatalogStatus() {
    rpc('plugin.tool.call', { tool: 'catalog_status', args: {} })
      .then(function (r) {
        el('catalog-out').textContent = failed(r)
          ? '读取失败：' + failText(r)
          : JSON.stringify(r, null, 2);
        scheduleHeight();
      })
      .catch(function (e) { el('catalog-out').textContent = '读取失败：' + errText(e); scheduleHeight(); });
  }

  function refreshCatalog() {
    el('btn-catalog-refresh').disabled = true;
    rpc('plugin.tool.call', { tool: 'catalog_refresh', args: {} })
      .then(function (r) {
        el('catalog-out').textContent = failed(r)
          ? '刷新失败：' + failText(r)
          : '刷新完成：' + JSON.stringify(r, null, 2);
        scheduleHeight();
      })
      .catch(function (e) { el('catalog-out').textContent = '刷新失败：' + errText(e); scheduleHeight(); })
      .then(function () { el('btn-catalog-refresh').disabled = false; });
  }

  function runLanHealth() {
    el('btn-lan-health').disabled = true;
    rpc('plugin.tool.call', { tool: 'lan_health', args: {} })
      .then(function (r) {
        el('lan-out').textContent = failed(r)
          ? '探测失败：' + failText(r)
          : JSON.stringify(r, null, 2);
        scheduleHeight();
      })
      .catch(function (e) { el('lan-out').textContent = '探测失败：' + errText(e); scheduleHeight(); })
      .then(function () { el('btn-lan-health').disabled = false; });
  }

  /**
   * 退出登录（删账号）—— 原界面只有"删除"，没有"退出登录"这条路。
   *
   * 两者语义不同：退出登录只清当前会话（下次还能用同一账号再登），
   * 删除账号会清凭据与本地记录。用户要用的是前者时，只有后者是危险的。
   */
  function logoutAccount(who) {
    var sid = sessionId;
    if (!sid) {
      showBanner('当前没有进行中的登录会话可退出；如需清除已保存的账号请用该行的「删除」', 'warn');
      return;
    }
    if (!window.confirm('退出「' + (who || '当前账号') + '」的登录？\n\n' +
        '这会清除云端会话（Cookie 票据与会话密钥），已保存的账号条目仍然保留；' +
        '需要时重新登录即可。')) return;
    rpc('plugin.tool.call', { tool: 'logout', args: { session_id: sid } })
      .then(function () {
        sessionId = '';
        el('login-hint').hidden = true;
        el('challenge').hidden = true;
        showBanner('已退出登录会话', 'ok');
        loadAccounts();
      })
      .catch(function (e) { showBanner('退出失败：' + errText(e), 'err'); });
  }

  function loadDiag() {
    rpc('plugin.config.get', {})
      .then(function (res) {
        var lines = ['配置：' + JSON.stringify((res && res.config) || {}, null, 2)];
        return rpc('plugin.tool.call', { tool: 'accounts', args: {} })
          .then(function (acc) {
            lines.push('账号数：' + (((acc && acc.accounts) || []).length));
            el('diag').textContent = lines.join('\n\n');
            scheduleHeight();
          })
          .catch(function (e) {
            lines.push('账号读取失败：' + errText(e));
            el('diag').textContent = lines.join('\n\n');
            scheduleHeight();
          });
      })
      .catch(function (e) {
        el('diag').textContent = '读取失败：' + errText(e);
        scheduleHeight();
      });
  }

  /* ── 接线 ──────────────────────────────────────────────── */

  function bind() {
    el('btn-back').addEventListener('click', function () {
      // iframe 是 opaque origin，只有宿主能导航 —— 所以走协议 op。
      rpc('plugin.ui.close').catch(function () {});
    });
    el('btn-save').addEventListener('click', saveConfig);
    el('btn-add').addEventListener('click', function () {
      el('login-form').hidden = false;
      clearBanner();
      el('challenge').hidden = true;
      scheduleHeight();
    });
    el('btn-cancel-login').addEventListener('click', function () {
      el('login-form').hidden = true;
      el('f-pass').value = '';
      scheduleHeight();
    });
    el('btn-login').addEventListener('click', startLogin);
    el('btn-verify').addEventListener('click', submitVerify);
    el('btn-verify-web').addEventListener('click', submitVerifyWeb);
    el('btn-reload-accounts').addEventListener('click', function () { loadAccounts(); loadSyncResult(); });
    el('btn-diag').addEventListener('click', loadDiag);
    el('btn-add-sync').addEventListener('click', function () {
      // 已展开时再点＝收起（同一个按钮两种状态，不需要额外的取消入口）。
      var f = el('addsync-form');
      if (!f.hidden) { f.hidden = true; scheduleHeight(); return; }
      openAddSync();
    });
    el('btn-add-sync-cancel').addEventListener('click', function () {
      el('addsync-form').hidden = true;
      scheduleHeight();
    });
    el('btn-add-sync-ok').addEventListener('click', submitAddSync);
    el('btn-catalog-status').addEventListener('click', loadCatalogStatus);
    el('btn-catalog-refresh').addEventListener('click', refreshCatalog);
    el('btn-lan-health').addEventListener('click', runLanHealth);
    // 「退出登录」已移进账号行（每行一个），绑定在 renderAccounts 里 —— 卡头不再有该按钮。
    el('sw-polling').addEventListener('click', function () { setPolling(!pollingOn()); });
  }

  var booted = false;
  /** 首个业务请求是否还在等"能发"的信号（与 HXCS 的 firstLoadPending 同义）。 */
  var firstLoadPending = true;
  var bootWaiters = [];

  /**
   * 请求启动（首个业务请求）。
   *
   * # 为什么需要它而不是直接 boot()
   *
   * 端口建立与 host-ready 是**两条独立的就绪路径**，两者哪个先到不确定。
   * 但业务请求只能在宿主 attach **之后**发（否则 rpc 拿到 FRAME_NOT_ATTACHED）。
   *
   * 这里把"要 boot"记下来，等到**任一**就绪信号到达就立即执行；若两条信号
   * 都没来（平台版本差异），1.2 秒后兜底再试一次 —— 那时端口基本已可用，
   * 而"永远不发"会让用户看到永久的「读取中…」。
   */
  function requestBoot() {
    if (!firstLoadPending) return;
    firstLoadPending = false;
    boot();
  }

  /** 应用主题（宿主下发）：写 html[data-theme]，CSS 的变量随之切换。 */
  function applyTheme(theme) {
    if (theme === 'dark' || theme === 'light') {
      document.documentElement.setAttribute('data-theme', theme);
      scheduleHeight();
    }
  }

  function boot() {
    if (booted || !port) return;
    booted = true;
    /*
     * ## 账号列表必须**独立**加载（2026-11-09 修「卡在读取中」的结构性原因）
     *
     * 原来 `loadAccounts()` 被写在 `plugin.config.get` 的 `.then` 里。于是：
     *   · config.get 失败（bootstrap 网络/权限错）→ 走 catch → 账号列表永不加载
     *   · config.get 成功但 renderConfig 抛错 → then 内抛异常 → 同样不加载
     *
     * 表现就是「读取中…」永久停留 —— 那是 index.html 里的**初始占位**，
     * 没有任何代码去改它。
     *
     * 两块数据本无依赖关系（配置文件 vs 账号清单），UI 加载也不该有。
     * 现在各自独立发起，任一失败都不拖累另一个。
     */
    loadAccounts();
    loadSyncResult();
    rpc('plugin.config.get', {})
      .then(function (res) {
        config = (res && res.config) || {};
        // 当前家庭由平台下发（id + 显示名）。没有时家庭列回落显示 id 前 8 位 ——
        // 不假装知道名字。见平台 PluginUiHost 的 household 注释。
        household = (res && res.household) || null;
        renderConfig();
        loadAccounts();
        scheduleHeight();
      })
      .catch(function (e) { showBanner('读取配置失败：' + errText(e), 'err'); });
  }

  bind();
  scheduleHeight();

  // 兜底：两条就绪信号都没到也把首个请求发出去（宁可失败一次让人看到
  // 可操作的错误，也不要永久停在「读取中…」）。
  setTimeout(function () {
    if (firstLoadPending && port) requestBoot();
    // 兜底：平台版本差异导致 host-ready 没来时，也不能让排队的请求永远等。
    flushPreReady();
  }, 1200);
})();
