/*
 * HX-MI 插件自带界面（2026-11-09 首版；2026-11-20 按用户下发的新设计图重做视觉）。
 *
 * # 数据流（与平台协议，一律不许改动语义）
 *
 *   config.get/save  ←→  region / sync_interval_seconds / polling_enabled /
 *                        device_filter_models / save_password / lan_priority /
 *                        lan_check_interval_seconds
 *   tool.call        ←→  accounts / login / status / captcha_image / verify /
 *                        verify_web / logout / account_delete / account_sync /
 *                        account_settings / account / health_list
 *
 * # 2026-11-20 重做的关键口径
 *
 *  · 设计图没有全局「保存配置」按钮 ⇒ 改为**改动即存**：每个开关/下拉/间隔改动
 *    立即 plugin.config.save 或 account_settings（带 revision），每次保存都有提示。
 *    原草稿机制（draftEdits + 统一保存）随之移除 —— 保存语义从"攒一批"变为"即时"，
 *    这是设计图直接要求的交互形态。
 *  · 账号行内开关视觉上按行显示，但 save_password 是**全插件共享**的配置 ——
 *    所有行的开关与顶部开关显示同一份真实状态，改任一处即全量保存并重渲染。
 *  · 同步任务表内「编辑」展开行内编辑条（家庭 / 周期 / 自动同步），保存即生效。
 *  · 「同步设备 / 设备管理」合并了旧"同步结果"卡：统计磁贴（总数/在线/离线/
 *    能力待完善）+ 设备列表 + 详情（控制 / 传感器 / 配置 三个页签）。
 *    插件页不下发设备命令（平台协议没有该工具），详情如实展示状态与路径。
 *
 * # 安全边界
 *
 * 密码只在本页停留于 input 元素，提交即随 tool.call 交给宿主（不在 JS 里缓存、
 * 不写 localStorage）。账号口令最终由宿主保险库保管，本页从不回读明文。
 */
(function () {
  'use strict';

  /*
   * ⚠ 必须是数字 1，与平台 `PLUGIN_UI_PROTOCOL` 完全一致（2026-11-09 根修）。
   * 平台发的是数字 1，本文件每处守卫都是严格比较 `m.hx !== HX`。
   */
  var HX = 1;
  var RPC_TIMEOUT_MS = 30000;

  var port = null;
  var connected = false;
  var pending = Object.create(null);
  var seq = 0;
  var pluginId = '';

  /* 账号清单缓存（同步表、添加任务表单都要用它；loadAccounts 填充） */
  var accountsCache = [];
  var accountNamesCache = {};
  /* 当前家庭（平台经 config.get 下发；为 null 时家庭列回落显示 id） */
  var household = null;
  /* 当前配置（config.get 的结果；每次即时保存后整体更新） */
  var config = {};
  /* 登录会话状态（session_id 由 login 返回，后续 status/verify 要用） */
  var sessionId = '';
  /* account 工具的家庭级视图（同步状态 + 设备列表） */
  var syncView = null;
  /* health_list 缓存（健康表 + 局域网状态行 + 设备详情的通信路径） */
  var healthCache = null;
  var healthLoaded = false;
  /* 设备管理选中态与过滤 */
  var devSelDid = '';
  var devFilterAccount = '';
  /* 局域网检查列表的账号过滤（2026-11-20 用户要求） */
  var healthFilterAccount = '';
  /* 设备详情缓存：did -> device_detail 应答（或 {error}）；pending 防重复请求 */
  var devDetailCache = {};
  var devDetailPending = {};
  /* 同步表当前展开编辑条的账号 id */
  var openEditId = '';

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
   * RPC 在通道就绪前**排队**，不直接失败：手抖点得快、iframe 重建、平台重新
   * mount 都会撞上"还没连上"的窗口。排队上限 RPC_TIMEOUT_MS（30s），真的没
   * 握手成功时仍要报错，不能让用户对着一个永远转圈的界面。
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
    // 主题下发：插件页是 opaque origin，读不到父窗口的 document.theme；
    // 只能由宿主在消息里告诉它。收到就切 data-theme，未收到时由 CSS 兜底。
    if (m.type === 'theme' && typeof m.theme === 'string') {
      applyTheme(m.theme);
      return;
    }
    if (m.type === 'host-ready') {
      // 平台已 attach 的权威信号：发出首个业务请求并放行排队的 RPC。
      connected = true;
      startHeightReporting();
      requestBoot();
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
        // FRAME_NOT_ATTACHED 是**可重试**的时序问题：最多 3 次（间隔 300ms）。
        if (code === 'PLUGIN_UI_FRAME_NOT_ATTACHED' && (p.attempt || 0) < 3) {
          var next = (p.attempt || 0) + 1;
          setTimeout(function () {
            rpc(p.op, p.payload, next).then(p.resolve, p.reject);
          }, 300);
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
    // 这里**不**直接 boot：此刻宿主可能还没 attach 完（host-ready 未到）。
  }

  window.addEventListener('message', onWindowMessage);

  /* ── 内容高度上报（与平台同一条滚动条）────────────────── */

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

  /* ── 提示条（可带操作按钮）────────────────────────────── */

  function banner(text, kind, opts) {
    var b = el('banner');
    var k = kind === 'error' ? 'err' : (kind || 'err');
    b.textContent = text;
    b.className = 'banner banner--' + k;
    if (opts && opts.action) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = opts.action;
      btn.addEventListener('click', function () {
        b.hidden = true;
        scheduleHeight();
        if (opts.onAction) opts.onAction();
      });
      b.appendChild(btn);
    }
    b.hidden = false;
    scheduleHeight();
  }
  var showBanner = banner;
  function clearBanner() { el('banner').hidden = true; scheduleHeight(); }

  /*
   * 页面内确认框：插件界面跑在 `sandbox="allow-scripts"` 的 iframe 里，没有
   * allow-modals —— window.confirm 不弹窗直接返回 false，"先确认再执行"的按钮
   * 全部变哑且无报错。自己画 overlay + 卡片 + 取消/确认，返回 Promise<boolean>。
   * 结构（ov > card > [msg, act > [cancel, ok] ]）被握手闸门逐节点断言，别改层级。
   */
  function confirmDialog(message, confirmText, opts) {
    var o = opts || {};
    return new Promise(function (resolve) {
      var ov = document.createElement('div');
      ov.className = 'confirm';

      var card = document.createElement('div');
      card.className = 'confirm__card';

      var p = document.createElement('p');
      p.className = 'confirm__msg';
      p.textContent = message;
      card.appendChild(p);

      var act = document.createElement('div');
      act.className = 'confirm__act';

      var cancel = document.createElement('button');
      cancel.type = 'button';
      cancel.className = 'btn btn--sm';
      cancel.textContent = '取消';

      var okBtn = document.createElement('button');
      okBtn.type = 'button';
      okBtn.className = 'btn btn--sm ' + (o.danger ? 'btn--danger-fill' : 'btn--primary');
      okBtn.textContent = confirmText || '确认';

      var done = false;
      function close(v) {
        if (done) return;
        done = true;
        if (ov.parentNode) ov.parentNode.removeChild(ov);
        scheduleHeight();
        resolve(v);
      }
      cancel.addEventListener('click', function () { close(false); });
      okBtn.addEventListener('click', function () { close(true); });

      act.appendChild(cancel);
      act.appendChild(okBtn);
      card.appendChild(act);
      ov.appendChild(card);
      document.body.appendChild(ov);
      scheduleHeight();
    });
  }

  /* ── 通用小工具 ───────────────────────────────────────── */

  function esc(v) {
    return String(v == null ? '' : v).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  /** 平台把 /invoke/ 的两种结果都翻译成帧内形状：成功 → data 本体；失败 → {ok:false,...}（200）。 */
  function failed(res) { return !!(res && res.ok === false); }

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

  function fmtTime(v) {
    if (!v) return '—';
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function fmtDateTime(v) {
    if (!v) return '—';
    var d = new Date(v);
    if (isNaN(d.getTime())) return String(v);
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
      ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  var MIN_PER_MINUTE = 60;

  /** 家庭显示名：平台没给名字时回落显示 id 前 8 位（可核对，不误导）。 */
  function householdName(id) {
    if (!id) return '未绑定';
    if (household && household.id === id && household.name) return household.name;
    return '其他家庭（' + String(id).slice(0, 8) + '…）';
  }

  function findAccount(id) {
    for (var i = 0; i < accountsCache.length; i++) {
      if ((accountsCache[i].account_id || '') === id) return accountsCache[i];
    }
    return null;
  }

  /* ── 配置读写（即时保存）──────────────────────────────── */

  /** 全量回写合并后的配置；成功后更新本地 config。 */
  function saveConfigNow(patch, okMsg) {
    var merged = Object.assign({}, config, patch);
    return rpc('plugin.config.save', { config: merged }).then(function () {
      config = merged;
      if (okMsg) banner(okMsg, 'ok');
    }).catch(function (e) {
      banner('保存失败：' + errText(e), 'err');
    });
  }

  function renderConfig() {
    var savePw = config.save_password !== false;
    el('f-save-password').checked = savePw;
    applyTokenLinkage(savePw);
    // 命令优先通道：默认 lan
    var pri = config.lan_priority || 'lan';
    var radios = document.querySelectorAll('input[name="lan-priority"]');
    for (var i = 0; i < radios.length; i++) {
      radios[i].checked = (radios[i].value === pri);
    }
    // 局域网健康检查间隔：默认 5 秒（范围 5~3600，后端校验）
    var li = config.lan_check_interval_seconds || config.healthcheck_interval_seconds || 5;
    el('f-lan-interval').value = li;
    var lbl = el('health-interval-label');
    if (lbl) lbl.textContent = '自动检查周期：每 ' + li + ' 秒';
    // 家庭过滤下拉（设备管理）
    fillDevHousehold();
    // 添加任务表单的默认值
    fillAddTaskDefaults();
  }

  /*
   * 自动续 token 与「自动同步」的联动（2026-11-17 用户明确要求）。
   *
   * 不开自动续 token ⇒ Token 过期后自动同步会静默失败。所以：
   *   · 各处「自动同步」开关与周期下拉一起置灰不可用
   *   · 「手动同步 / 立即同步」按钮仍可用；Token 过期时点击会明确提示
   *     「登录已过期，请重新登录」并给出「重新登录」入口
   */
  function applyTokenLinkage(savePw) {
    var sw = el('sw-polling');
    var row = el('polling-row');
    if (sw) {
      sw.setAttribute('aria-disabled', savePw ? 'false' : 'true');
      sw.setAttribute('tabindex', savePw ? '0' : '-1');
    }
    if (row) {
      if (savePw) row.removeAttribute('data-disabled');
      else row.setAttribute('data-disabled', '1');
    }
    var sels = document.querySelectorAll('select[data-role="interval"]');
    for (var i = 0; i < sels.length; i++) sels[i].disabled = !savePw;
  }

  /** 自动同步开关（添加任务表单里那个）。 */
  function setPolling(on) {
    el('sw-polling').setAttribute('aria-checked', on ? 'true' : 'false');
    el('polling-t').textContent = on ? '开启' : '仅手动';
  }

  function pollingOn() {
    return el('sw-polling').getAttribute('aria-checked') === 'true';
  }

  /* ── 账号列表 ─────────────────────────────────────────── */

  var ACCOUNT_STATE_TEXT = {
    pending_login: { text: '登录中', warn: true },
    ready_unbound: { text: '待绑定家庭', warn: true },
    ready: { text: '已授权', warn: false },
    auth_required: { text: '登录已过期，请重新登录', warn: true },
    disabled: { text: '已停用', warn: true },
    deleting: { text: '删除中', warn: true },
    error: { text: '异常', warn: true }
  };

  function accountStateText(a) {
    var raw = (a && (a.state || a.State)) || '';
    if (!raw) return null;
    return ACCOUNT_STATE_TEXT[raw] || { text: String(raw), warn: true };
  }

  /*
   * 读一条账号的同步设置：真实形状是嵌套的 {"sync":{"enabled":..,"interval_seconds":..}}
   * （account_store.go 的 account.Sync）。两种形状都认，以嵌套为准。
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

  /** 一条同步的当前值（即时保存模型：没有草稿，以服务端返回为准）。 */
  function entryValue(a) {
    var base = accountSync(a);
    return {
      householdId: (a && a.household_id) || '',
      enabled: base.enabled,
      intervalSeconds: base.intervalSeconds
    };
  }

  /*
   * 由**条目**推导全局节奏（宿主只注册一个后台任务）。
   * 取"最勤快的那条"作为任务周期：这样没有任何一条会被饿着。
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

  /** 条目变化后把派生节奏写回插件配置（宿主后台任务按它调度）。 */
  function syncDerivedCadence() {
    var eff = effectiveCadence();
    if (config.polling_enabled !== eff.enabled || config.sync_interval_seconds !== eff.seconds) {
      return saveConfigNow({ polling_enabled: eff.enabled, sync_interval_seconds: eff.seconds });
    }
  }

  function loadAccounts() {
    return rpc('plugin.tool.call', { tool: 'accounts', args: {} })
      .then(function (res) { renderAccounts(res); })
      .catch(function (e) {
        var box = el('accounts');
        box.innerHTML = '';
        var p = document.createElement('p');
        p.className = 'banner banner--err';
        p.textContent = '读取账号失败：' + errText(e);
        box.appendChild(p);
        scheduleHeight();
      });
  }

  function renderAccounts(data) {
    var box = el('accounts');
    /*
     * 必须处理业务失败帧：平台把失败也翻译成 200 + {ok:false,...}。
     * 只认成功形状会把"账号主密钥不可用"这类真实故障静默说成"没有账号"。
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
    box.innerHTML = '';
    accountsCache = items;
    accountNamesCache = names;
    if (!items.length) {
      var p0 = document.createElement('p');
      p0.className = 'muted';
      p0.textContent = '还没有登录任何小米账号。点右上角「添加账号」开始。';
      box.appendChild(p0);
    } else {
      items.forEach(function (a) {
        var id = a.account_id || '';
        var name = names[id] || a.masked_account || id;
        var row = document.createElement('div');
        row.className = 'acct';

        var ava = document.createElement('span');
        ava.className = 'acct__ava';
        ava.setAttribute('aria-hidden', 'true');
        ava.textContent = 'mi';
        row.appendChild(ava);

        var t = document.createElement('div');
        t.className = 'acct__t';
        var b = document.createElement('b');
        b.textContent = name;
        var meta = document.createElement('span');
        meta.className = 'acct__meta';
        meta.textContent = '授权时间: ' + fmtDateTime(a.updated_at || a.created_at) +
          ' · 家庭: ' + householdName(a.household_id);
        t.appendChild(b);
        t.appendChild(meta);
        row.appendChild(t);

        /*
         * Token 状态（2026-11-20 用户要求在账号列显示票据信息）。
         * 小米不给 serviceToken 有效期（票据寿命不可预估），如实可给的是
         * 票据最近一次签发/自动续期的时间（saved_at，登录与自动重登都会刷新）。
         * 会话是单份的、属于当前登录账号 —— 只显示在对应行，不冒充别的账号。
         * 是否真的过期以状态 pill（真实探针结论）为准。
         */
        if (data.token_updated_at && data.token_account && name === data.token_account) {
          var tok = document.createElement('span');
          tok.className = 'acct__token';
          tok.textContent = 'Token 更新于 ' + fmtDateTime(data.token_updated_at);
          tok.setAttribute('title', '票据最近一次签发/自动续期的时间。小米不提供有效期，是否过期以状态为准；开启自动续期后过期会自动重登并刷新此时间。');
          row.appendChild(tok);
        }

        var spacer = document.createElement('span');
        spacer.className = 'spacer';
        row.appendChild(spacer);

        var st = accountStateText(a);
        if (st) {
          var pill = document.createElement('span');
          pill.className = 'pill' + (a.state === 'ready' ? ' pill--ok' : (st.warn ? ' pill--warn' : ''));
          pill.appendChild(document.createElement('i'));
          pill.appendChild(document.createTextNode(st.text));
          row.appendChild(pill);
        }

        // Token 自动续期开关：视觉按行显示，实际是**全插件共享**的 save_password。
        var swLabel = document.createElement('label');
        swLabel.className = 'sw';
        swLabel.setAttribute('title', 'Token 自动续期（全插件共享设置）');
        var inp = document.createElement('input');
        inp.type = 'checkbox';
        inp.checked = config.save_password !== false;
        var ui = document.createElement('span');
        ui.className = 'sw__ui';
        swLabel.appendChild(inp);
        swLabel.appendChild(ui);
        inp.addEventListener('change', function () {
          var on = inp.checked;
          el('f-save-password').checked = on;
          saveConfigNow({ save_password: on }, on ? 'Token 自动续期已开启' : 'Token 自动续期已关闭')
            .then(function () {
              applyTokenLinkage(on);
              renderAccounts(accountsCache, accountNamesCache);
            });
        });
        row.appendChild(swLabel);

        // 重新授权：打开登录表单（过期 / 换密码后从这里进入）。
        var reauth = document.createElement('button');
        reauth.type = 'button';
        reauth.className = 'btn btn--sm';
        reauth.textContent = '重新授权';
        reauth.addEventListener('click', function () { openLogin(); });
        row.appendChild(reauth);

        // 解绑：删除账号（清凭据）。确认框里写清代价。
        var del = document.createElement('button');
        del.type = 'button';
        del.className = 'btn btn--sm btn--danger-soft';
        del.textContent = '解绑';
        del.addEventListener('click', function () { removeAccount(id, a.revision); });
        row.appendChild(del);

        box.appendChild(row);
      });
    }
    fillAddTaskDefaults();
    renderSyncTable();
    scheduleHeight();
  }

  function removeAccount(id, revision) {
    if (!id) return;
    confirmDialog('解绑并删除这个账号？\n\n其云端会话与已保存凭据会一并清除，需要重新登录才能再加回来。',
      '解绑', { danger: true }).then(function (ok) {
      if (!ok) return;
      rpc('plugin.tool.call', {
        tool: 'account_delete',
        args: { account_id: id, revision: revision || 0, confirm: true },
        account_id: id
      })
        .then(function (res) {
          if (failed(res)) { banner('删除失败：' + failText(res), 'err'); return; }
          loadAccounts();
          banner('账号已解绑删除', 'ok');
        })
        .catch(function (e) { banner('删除失败：' + errText(e), 'err'); });
    });
  }

  /* ── 登录流程 ─────────────────────────────────────────── */

  function openLogin() {
    el('login-form').hidden = false;
    el('challenge').hidden = true;
    clearBanner();
    scheduleHeight();
  }

  function startLogin() {
    var user = el('f-user').value.trim();
    var pass = el('f-pass').value;
    if (!user || !pass) {
      banner('账号和密码都不能为空', 'err');
      return;
    }
    el('btn-login').disabled = true;
    clearBanner();
    rpc('plugin.tool.call', {
      tool: 'login',
      args: { region: el('f-region').value || 'cn', user: user, password: pass }
    })
      .then(function (res) {
        // 提交后立刻清掉密码：既然已经交给宿主，本页不该再留着它。
        el('f-pass').value = '';
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
        // 区域选择随登录一并持久化（即时保存模型）。
        saveConfigNow({ region: el('f-region').value || 'cn' });
        scheduleHeight();
        refreshChallenge(state);
      })
      .catch(function (e) { banner('登录失败：' + errText(e), 'err'); })
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
          banner('登录成功', 'ok');
          loadAccounts();
        } else {
          el('challenge').hidden = true;
          if (s === 'failed') {
            banner('登录失败（' + ((st && st.reason) || '原因未提供') + '）', 'err');
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
          banner('取验证码图片失败：' + failText(res), 'err');
          return;
        }
        var b64 = (res && (res.image_base64 || res.image)) || '';
        if (!b64) {
          banner('插件没有返回验证码图片', 'warn');
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
    if (!code) { banner('请先输入验证码', 'err'); return; }
    el('btn-verify').disabled = true;
    rpc('plugin.tool.call', { tool: 'verify', args: { session_id: sessionId, code: code } })
      .then(function (res) {
        el('f-code').value = '';
        if (failed(res)) {
          banner('验证失败：' + failText(res), 'err');
          scheduleHeight();
          return;
        }
        refreshChallenge((res && res.state) || '');
      })
      .catch(function (e) { banner('验证失败：' + errText(e), 'err'); })
      .then(function () { el('btn-verify').disabled = false; });
  }

  function submitVerifyWeb() {
    var url = el('f-web').value.trim();
    if (!url) { banner('请先粘贴验证链接', 'err'); return; }
    el('btn-verify-web').disabled = true;
    rpc('plugin.tool.call', { tool: 'verify_web', args: { session_id: sessionId, url: url } })
      .then(function (res) {
        el('f-web').value = '';
        refreshChallenge((res && res.state) || '');
      })
      .catch(function (e) { banner('验证失败：' + errText(e), 'err'); })
      .then(function () { el('btn-verify-web').disabled = false; });
  }

  /*
   * 退出登录 = 清当前登录会话（保留账号条目），语义与「解绑」不同。
   * 它依赖一次**进行中**的登录会话；没有会话时如实提示而不是假装成功。
   */
  function logoutAccount() {
    if (!sessionId) {
      banner('当前没有进行中的登录会话可退出；如需移除已保存的账号，请在该账号行点「解绑」。', 'warn');
      return;
    }
    confirmDialog('退出当前登录会话？\n\n这会清除云端会话（Cookie 票据与会话密钥），' +
      '已保存的账号条目仍然保留；需要时重新登录即可。', '退出登录').then(function (ok) {
      if (!ok) return;
      rpc('plugin.tool.call', { tool: 'logout', args: { session_id: sessionId } })
        .then(function () {
          sessionId = '';
          el('login-hint').hidden = true;
          el('challenge').hidden = true;
          banner('已退出登录会话', 'ok');
          loadAccounts();
        })
        .catch(function (e) { banner('退出失败：' + errText(e), 'err'); });
    });
  }

  /* ── 同步任务表 ───────────────────────────────────────── */

  /** 家庭下拉：可选项 = 「未绑定」+ 当前家庭（插件是家庭作用域的，列别的家庭只会选了失败）。 */
  function fillHouseholdSelect(sel, currentId) {
    sel.innerHTML = '';
    var none = document.createElement('option');
    none.value = '';
    none.textContent = '未绑定';
    sel.appendChild(none);
    if (household) {
      var opt = document.createElement('option');
      opt.value = household.id;
      opt.textContent = household.name;
      sel.appendChild(opt);
      sel.disabled = false;
    } else {
      if (currentId) {
        var keep = document.createElement('option');
        keep.value = currentId;
        keep.textContent = String(currentId).slice(0, 8) + '…';
        sel.appendChild(keep);
      }
      sel.disabled = true;
    }
    sel.value = currentId || '';
  }

  /** 周期下拉：固定几档 + 当前值（调度是 5 分钟一轮的粒度）。 */
  function intervalOptions(sel, curSec) {
    var curMin = Math.max(1, Math.round((curSec || 300) / MIN_PER_MINUTE));
    var opts = [5, 10, 15, 30, 60, 120];
    if (opts.indexOf(curMin) < 0) opts.push(curMin);
    opts.sort(function (a, b) { return a - b; });
    sel.innerHTML = '';
    opts.forEach(function (m) {
      var o = document.createElement('option');
      o.value = String(m);
      o.textContent = '每 ' + m + ' 分钟';
      if (m === curMin) o.selected = true;
      sel.appendChild(o);
    });
  }

  /** 添加任务表单：账号 / 家庭 / 周期 / 开关默认值。 */
  function fillAddTaskDefaults() {
    var accSel = el('f-sync-account');
    if (!accSel) return;
    accSel.innerHTML = '';
    if (!accountsCache.length) {
      var o = document.createElement('option');
      o.value = '';
      o.textContent = '暂无账号';
      accSel.appendChild(o);
      accSel.value = '';
    } else {
      accountsCache.forEach(function (a) {
        var id = a.account_id || '';
        var opt = document.createElement('option');
        opt.value = id;
        opt.textContent = accountNamesCache[id] || a.masked_account || id;
        accSel.appendChild(opt);
      });
      accSel.value = accountsCache[0].account_id || '';
    }
    fillHouseholdSelect(el('f-sync-household'), '');
    var sec = parseInt(config.sync_interval_seconds, 10);
    if (isNaN(sec) || sec <= 0) sec = 300;
    intervalOptions(el('f-interval-sel'), sec);
    setPolling(config.polling_enabled !== false);
  }

  /*
   * 同步任务表：一条同步 = 「账号 → 家庭」。
   * 列**所有账号**（未绑定的也列出 —— 否则重登后变成未绑定的账号没有入口绑回来），
   * 已绑定的排前面。行内「编辑」展开编辑条：家庭 / 周期 / 自动同步，保存即生效。
   */
  function renderSyncTable() {
    var box = el('sync-table');
    if (!box) return;
    box.innerHTML = '';
    var items = accountsCache;
    var cnt = el('sync-count');
    var bound = items.filter(function (a) { return !!entryValue(a).householdId; });
    if (cnt) {
      cnt.textContent = bound.length ? bound.length + ' 条同步' : (items.length ? '未绑定' : '');
    }
    if (!items.length) {
      // 空列表就留空（2026-11-10 用户口径）：入口（添加任务表单）就在上方，空态自己会说话。
      scheduleHeight();
      return;
    }
    var all = items.slice();
    all.sort(function (a, b) {
      var ha = entryValue(a).householdId ? 0 : 1;
      var hb = entryValue(b).householdId ? 0 : 1;
      return ha - hb;
    });

    var t = document.createElement('table');
    var thead = document.createElement('thead');
    var htr = document.createElement('tr');
    ['账号', '家庭', '同步周期', '状态', '上次同步', '操作'].forEach(function (h) {
      var th = document.createElement('th');
      th.textContent = h;
      htr.appendChild(th);
    });
    thead.appendChild(htr);
    t.appendChild(thead);
    var tb = document.createElement('tbody');

    var savePw = config.save_password !== false;

    all.forEach(function (a) {
      var id = a.account_id || '';
      var name = accountNamesCache[id] || a.masked_account || id;
      var val = entryValue(a);
      var st = accountStateText(a);
      var mins = Math.max(1, Math.round(val.intervalSeconds / MIN_PER_MINUTE));

      var tr = document.createElement('tr');

      var td1 = document.createElement('td');
      var b1 = document.createElement('b');
      b1.textContent = name;
      td1.appendChild(b1);
      if (st && st.warn) {
        var sub = document.createElement('span');
        sub.className = 'sub';
        sub.textContent = st.text;
        td1.appendChild(sub);
      }
      tr.appendChild(td1);

      var td2 = document.createElement('td');
      td2.textContent = householdName(val.householdId);
      tr.appendChild(td2);

      var td3 = document.createElement('td');
      td3.textContent = '每 ' + mins + ' 分钟';
      tr.appendChild(td3);

      var td4 = document.createElement('td');
      var pill = document.createElement('span');
      pill.className = 'pill ' + (val.enabled ? 'pill--ok' : 'pill--off');
      pill.appendChild(document.createElement('i'));
      pill.appendChild(document.createTextNode(val.enabled ? '已启用' : '已停用'));
      td4.appendChild(pill);
      tr.appendChild(td4);

      var td5 = document.createElement('td');
      td5.className = 'muted';
      td5.textContent = '—';
      td5.setAttribute('title', '以「设备管理」区的最近同步时间为准（当前为家庭级同步会话）');
      tr.appendChild(td5);

      var td6 = document.createElement('td');
      td6.className = 'ops';
      var syncBtn = document.createElement('button');
      syncBtn.type = 'button';
      syncBtn.className = 'btn btn--sm';
      syncBtn.textContent = '立即同步';
      syncBtn.addEventListener('click', function () { syncNowFor(id, syncBtn); });
      td6.appendChild(syncBtn);

      var editBtn = document.createElement('button');
      editBtn.type = 'button';
      editBtn.className = 'btn btn--sm';
      editBtn.textContent = openEditId === id ? '收起' : '编辑';
      editBtn.addEventListener('click', function () {
        openEditId = (openEditId === id) ? '' : id;
        renderSyncTable();
      });
      td6.appendChild(editBtn);

      var delBtn = document.createElement('button');
      delBtn.type = 'button';
      delBtn.className = 'btn btn--sm btn--danger-soft';
      delBtn.textContent = '删除';
      delBtn.addEventListener('click', function () { removeSyncEntry(a, name); });
      td6.appendChild(delBtn);
      tr.appendChild(td6);

      tb.appendChild(tr);

      // 编辑条（展开在该行下方）
      if (openEditId === id) {
        var etr = document.createElement('tr');
        var etd = document.createElement('td');
        etd.setAttribute('colSpan', '6');
        var wrap = document.createElement('div');
        wrap.className = 'editwrap';

        var f1 = document.createElement('label');
        f1.className = 'fld';
        var s1 = document.createElement('span');
        s1.textContent = '家庭';
        var hhSel = document.createElement('select');
        hhSel.setAttribute('aria-label', '同步目标家庭');
        fillHouseholdSelect(hhSel, val.householdId);
        f1.appendChild(s1);
        f1.appendChild(hhSel);
        wrap.appendChild(f1);

        var f2 = document.createElement('label');
        f2.className = 'fld';
        var s2 = document.createElement('span');
        s2.textContent = '同步周期';
        var ivSel = document.createElement('select');
        ivSel.setAttribute('data-role', 'interval');
        ivSel.disabled = !savePw;
        intervalOptions(ivSel, val.intervalSeconds);
        f2.appendChild(s2);
        f2.appendChild(ivSel);
        wrap.appendChild(f2);

        var f3 = document.createElement('span');
        f3.className = 'switch-row';
        var swRow = document.createElement('span');
        swRow.className = 'muted small';
        swRow.textContent = '自动同步';
        var sw = document.createElement('button');
        sw.type = 'button';
        sw.className = 'switch';
        sw.setAttribute('role', 'switch');
        sw.setAttribute('aria-checked', val.enabled ? 'true' : 'false');
        sw.disabled = !savePw;
        sw.appendChild(document.createElement('i'));
        var swT = document.createElement('span');
        swT.textContent = val.enabled ? '开启' : '关闭';
        sw.addEventListener('click', function () {
          var next = sw.getAttribute('aria-checked') !== 'true';
          sw.setAttribute('aria-checked', next ? 'true' : 'false');
          swT.textContent = next ? '开启' : '关闭';
        });
        f3.appendChild(swRow);
        f3.appendChild(sw);
        f3.appendChild(swT);
        wrap.appendChild(f3);

        var saveBtn = document.createElement('button');
        saveBtn.type = 'button';
        saveBtn.className = 'btn btn--sm btn--primary';
        saveBtn.textContent = '保存';
        saveBtn.addEventListener('click', function () {
          var min = parseInt(ivSel.value, 10);
          if (isNaN(min) || min < 1) min = 5;
          var patch = {
            household_id: hhSel.value || null,
            interval_seconds: min * MIN_PER_MINUTE,
            sync_enabled: sw.getAttribute('aria-checked') === 'true'
          };
          openEditId = '';
          saveAccountSettingsFor(id, a.revision, patch, '同步任务已保存');
        });
        wrap.appendChild(saveBtn);

        var cancelBtn = document.createElement('button');
        cancelBtn.type = 'button';
        cancelBtn.className = 'btn btn--sm';
        cancelBtn.textContent = '取消';
        cancelBtn.addEventListener('click', function () {
          openEditId = '';
          renderSyncTable();
        });
        wrap.appendChild(cancelBtn);

        etd.appendChild(wrap);
        etr.appendChild(etd);
        tb.appendChild(etr);
      }
    });

    t.appendChild(tb);
    box.appendChild(t);
    applyTokenLinkage(config.save_password !== false);
    scheduleHeight();
  }

  /*
   * 改账号级设置（家庭 / 周期 / 自动同步）—— 即时保存。
   *
   * # account_id 必须显式带上
   *
   * account_settings 在 manifest 里声明为 `scope: "account"`，平台要求调用
   * 上下文带 account_id（plugin.tool.call 的 account_id 字段）。
   * 成功后：重读账号 + 把派生节奏（最勤快条目的周期）写回插件配置。
   */
  function saveAccountSettingsFor(accountId, revision, patch, okMsg) {
    var args = { account_id: accountId };
    if (revision !== undefined && revision !== null) args.revision = revision;
    if (patch.household_id !== undefined) args.household_id = patch.household_id;
    if (patch.sync_enabled !== undefined) args.sync_enabled = patch.sync_enabled;
    if (patch.interval_seconds !== undefined) args.interval_seconds = patch.interval_seconds;
    clearBanner();
    return rpc('plugin.tool.call', { tool: 'account_settings', args: args, account_id: accountId })
      .then(function (res) {
        if (failed(res)) { banner('设置未保存：' + failText(res), 'err'); return; }
        if (okMsg) banner(okMsg, 'ok');
        return loadAccounts().then(syncDerivedCadence);
      })
      .catch(function (e) { banner('设置未保存：' + errText(e), 'err'); })
      .then(function () { scheduleHeight(); });
  }

  /*
   * 删除一条同步 = **解绑家庭**（不删账号凭据）。
   * 为什么不复用「解绑账号」：那是销毁凭据、要重新登录；这里清除的只是
   * "别把这个家同步进来"。两者代价差一个登录流程，必须分开并写清。
   */
  function removeSyncEntry(a, name) {
    confirmDialog(
      '移除同步「' + name + ' → ' + householdName(entryValue(a).householdId) + '」？\n\n' +
      '只解除这条同步关系并关闭它的自动同步；账号凭据保留，之后可以再添加回来。',
      '移除', { danger: true }
    ).then(function (ok) {
      if (!ok) return;
      saveAccountSettingsFor(a.account_id || '', a.revision,
        { household_id: null, sync_enabled: false }, '同步任务已移除');
    });
  }

  /*
   * 提交「添加任务」：把某个已有账号绑到当前家庭（语义与「添加账号」不同）。
   * 家庭 / 周期 / 自动同步一起写进账号设置，保存即生效。
   */
  function submitAddTask() {
    var id = el('f-sync-account').value;
    if (!id) { banner('还没有账号可绑定。先在「已授权账号」添加账号。', 'warn'); return; }
    var hh = el('f-sync-household').value;
    if (!hh) { banner('平台没有提供可选家庭，无法绑定', 'err'); return; }
    var min = parseInt(el('f-interval-sel').value, 10);
    if (isNaN(min) || min < 1) min = 5;
    var a = findAccount(id);
    saveAccountSettingsFor(id, a ? a.revision : undefined, {
      household_id: hh,
      interval_seconds: min * MIN_PER_MINUTE,
      sync_enabled: pollingOn()
    }, '同步任务已添加');
  }

  /* ── 手动同步 ─────────────────────────────────────────── */

  /*
   * 手动同步一个账号。
   *
   * account_sync 在 manifest 里是 `scope: "account"` —— 必须显式带 account_id；
   * 但**不能**带顶层 account_id（那会让平台把这次调用判成账号上下文，而
   * account_sync 要读的是家庭级会话材料，会被拒）。
   * 同步完成后重读账号与同步结果，避免"同步了但列表没动"的观感。
   */
  function syncNowFor(accountId, btn) {
    if (btn) { btn.disabled = true; btn.textContent = '同步中…'; }
    clearBanner();
    var args = accountId ? { account_id: accountId } : {};
    rpc('plugin.tool.call', { tool: 'account_sync', args: args })
      .then(function (res) {
        if (failed(res)) {
          if (!manualSyncExpiredHint(failText(res))) {
            banner('同步失败：' + failText(res), 'err');
          }
        } else if (res && res.state === 'succeeded') {
          var sum = res.summary || {};
          banner('同步完成：云端 ' + (sum.cloud || 0) + ' 台 · 新增 ' + (sum.added || 0) +
            ' · 更新 ' + (sum.updated || 0) + (sum.failed ? ' · 失败 ' + sum.failed : ''), 'ok');
        } else {
          var why = (res && res.error) || '原因未提供';
          if (!manualSyncExpiredHint(why)) {
            banner('同步失败：' + why, 'err');
          }
        }
        loadSyncResult();
        loadAccounts();
      })
      .catch(function (e) { banner('同步失败：' + errText(e), 'err'); })
      .then(function () {
        if (btn) { btn.disabled = false; btn.textContent = '立即同步'; }
        scheduleHeight();
      });
  }

  /** Token 过期是可自愈的一种失败：单独给"重新登录"入口，不和普通失败混在一起。 */
  function manualSyncExpiredHint(msg) {
    var m = String(msg || '');
    if (m.indexOf('SERVICETOKEN') < 0 && m.indexOf('登录已过期') < 0 &&
        m.indexOf('auth') < 0 && m.indexOf('Token') < 0) {
      return false;
    }
    banner('登录已过期，请重新登录 —— Token 失效后无法读取云端设备列表（红外设备也因此不可控）。',
      'error', { action: '重新登录', onAction: function () { openLogin(); } });
    return true;
  }

  /* ── 列表切换页签 ─────────────────────────────────────── */

  /** 「同步结果与局域网检查」卡里两个并排页签：点击切到对应列表。 */
  function showTab(which) {
    var sync = which !== 'health';
    el('pane-dev').hidden = !sync;
    el('pane-health').hidden = sync;
    el('tab-dev').className = 'seg__btn' + (sync ? ' seg__btn--on' : '');
    el('tab-dev').setAttribute('aria-selected', sync ? 'true' : 'false');
    el('tab-health').className = 'seg__btn' + (sync ? '' : ' seg__btn--on');
    el('tab-health').setAttribute('aria-selected', sync ? 'false' : 'true');
    scheduleHeight();
  }

  /* ── 同步状态 + 设备管理 ──────────────────────────────── */

  /** 读一次"同步结果"（账号工具的家庭级视图：是运行事实，不是配置）。 */
  function loadSyncResult() {
    return rpc('plugin.tool.call', { tool: 'account', args: {} })
      .then(function (res) {
        if (failed(res)) {
          syncView = null;
          var box = el('dev-list');
          box.innerHTML = '';
          var p = document.createElement('p');
          p.className = 'banner banner--warn';
          p.textContent = '读取同步结果失败：' + failText(res);
          box.appendChild(p);
          scheduleHeight();
          return;
        }
        syncView = res;
        renderSyncMeta();
        renderDevices();
      })
      .catch(function (e) {
        var box = el('dev-list');
        box.innerHTML = '';
        var p = document.createElement('p');
        p.className = 'banner banner--warn';
        p.textContent = '读取同步结果失败：' + errText(e);
        box.appendChild(p);
        scheduleHeight();
      });
  }

  function renderSyncMeta() {
    var sync = (syncView && syncView.sync) || {};
    var pill = el('sync-state-pill');
    if (pill) pill.className = 'pill' + (sync.running ? ' pill--ok' : '');
    el('res-state-t').textContent = sync.running ? '同步中' : '空闲';
    var when = el('res-when');
    if (when) {
      when.textContent = '最近同步 ' + (sync.last_ok_at ? fmtTime(sync.last_ok_at) : '尚未同步') +
        ' · 下次 ' + (sync.next_sync_at ? fmtTime(sync.next_sync_at) : (sync.enabled ? '计算中' : '已关闭'));
    }
    // 连续失败与最后错误：它们才是"为什么一直没同步上"的答案。
    var fail = el('res-fail');
    if (fail) {
      if ((sync.consecutive_failures || 0) > 0 || sync.last_error) {
        fail.hidden = false;
        fail.textContent = '连续失败 ' + (sync.consecutive_failures || 0) + ' 次' +
          (sync.last_error ? '：' + sync.last_error : '');
      } else {
        fail.hidden = true;
      }
    }
  }

  function addStat(box, value, label, kind) {
    var d = document.createElement('div');
    d.className = 'stat' + (kind ? ' stat--' + kind : '');
    var b = document.createElement('b');
    b.textContent = String(value);
    var sp = document.createElement('span');
    sp.textContent = label;
    d.appendChild(b);
    d.appendChild(sp);
    box.appendChild(d);
  }

  function fillDevHousehold() {
    var sel = el('f-dev-household');
    if (!sel) return;
    sel.innerHTML = '';
    if (household) {
      var opt = document.createElement('option');
      opt.value = household.id;
      opt.textContent = household.name;
      sel.appendChild(opt);
      sel.value = household.id;
    } else {
      var o2 = document.createElement('option');
      o2.value = '';
      o2.textContent = '当前家庭';
      sel.appendChild(o2);
      sel.value = '';
    }
  }

  /*
   * 设备管理：统计磁贴 + 设备列表 + 详情。
   * 设备明细来自 account 工具的 sync.device_list（家庭级同步会话拉取）。
   */
  function renderDevices() {
    var sync = (syncView && syncView.sync) || {};
    var list = sync.device_list || [];
    /*
     * 按账号过滤（2026-11-20 用户要求）：后端给每台设备带了同步来源账号
     * （sync.device_list[].account）。选中账号后只显示该账号的设备；
     * 统计磁贴保持全局口径（描述最近一次同步的整体结果，不随筛选变化）。
     */
    if (devFilterAccount) {
      var selName = accountNamesCache[devFilterAccount] || '';
      var fList = [];
      for (var fi = 0; fi < list.length; fi++) {
        if ((list[fi] && list[fi].account) === selName) fList.push(list[fi]);
      }
      list = fList;
    }

    var tiles = el('res-tiles');
    tiles.innerHTML = '';
    var nOnline = 0, nOffline = 0;
    list.forEach(function (d) {
      if (d.online === true) nOnline += 1;
      else if (d.online === false) nOffline += 1;
    });
    addStat(tiles, sync.devices || 0, '设备总数', '');
    addStat(tiles, nOnline, '在线', nOnline > 0 ? 'ok' : '');
    addStat(tiles, nOffline, '离线', nOffline > 0 ? 'warn' : '');
    // 能力待完善 = 云端声明里缺可控能力的设备（与"可控"判据同源）
    addStat(tiles, sync.uncontrollable || 0, '能力待完善', (sync.uncontrollable || 0) > 0 ? 'warn' : '');

    // 过滤下拉：所属账号（设备由家庭级会话拉取，暂不按账号拆分 —— 见界面备注）
    var accSel = el('f-dev-account');
    if (accSel) {
      accSel.innerHTML = '';
      var allOpt = document.createElement('option');
      allOpt.value = '';
      allOpt.textContent = '全部账号';
      accSel.appendChild(allOpt);
      accountsCache.forEach(function (a) {
        var id = a.account_id || '';
        var opt = document.createElement('option');
        opt.value = id;
        opt.textContent = accountNamesCache[id] || a.masked_account || id;
        accSel.appendChild(opt);
      });
      accSel.value = devFilterAccount || '';
    }
    fillDevHousehold();

    // 设备列表
    var listBox = el('dev-list');
    listBox.innerHTML = '';
    if (!list.length) {
      var p = document.createElement('p');
      p.className = 'muted small';
      p.textContent = devFilterAccount
        ? '所选账号在最近一次同步结果里没有设备。'
        : '暂无同步设备。手动同步一次后这里会列出云端返回的设备。';
      listBox.appendChild(p);
    } else {
      var selOk = false;
      list.forEach(function (d) { if (d.did === devSelDid) selOk = true; });
      if (!selOk) devSelDid = list[0].did || '';
      list.forEach(function (d) {
        var item = document.createElement('div');
        item.className = 'devi' + (d.did === devSelDid ? ' devi--sel' : '');
        item.setAttribute('data-did', d.did || '');

        var t = document.createElement('div');
        t.className = 'devi__t';
        var b = document.createElement('b');
        b.textContent = d.name || d.device_key || '未命名';
        var sub = document.createElement('span');
        sub.className = 'devi__sub';
        sub.textContent = d.model || '—';
        t.appendChild(b);
        t.appendChild(sub);
        item.appendChild(t);

        var pill = document.createElement('span');
        pill.className = 'pill ' + (d.online === true ? 'pill--ok' : (d.online === false ? 'pill--warn' : 'pill--off'));
        pill.appendChild(document.createElement('i'));
        pill.appendChild(document.createTextNode(d.online === true ? '在线' : (d.online === false ? '离线' : '未知')));
        item.appendChild(pill);

        var did = d.did || '';
        item.addEventListener('click', function () {
          devSelDid = did;
          renderDevices();
        });
        listBox.appendChild(item);
      });
    }

    renderDetail();
    scheduleHeight();
  }

  function healthByName(name) {
    var devs = (healthCache && healthCache.devices) || [];
    for (var i = 0; i < devs.length; i++) {
      if (devs[i].name === name) return devs[i];
    }
    return null;
  }

  /*
   * 设备详情：控制 / 传感器 / 配置 三个页签。
   * 插件页**不下发设备命令**（平台协议没有该工具）—— 控制页签如实展示
   * 可控性与通信路径；命令在平台设备页下发。
   */
  /*
   * 设备详情：控制 / 传感器 / 配置 三个页签 —— 数据来自 device_detail 工具
   * （2026-11-20 用户「台灯控制 传感器 配置没有数据啊：插件侧可以获取到的啊
   * 实际数据是--开关 温度 指示灯开关」）：
   *   · 控制 = 规格里可读可写的属性（开关 / 指示灯开关…），带实时值；
   *   · 传感器 = 规格里纯只读的属性（温度…），带实时值；
   *   · 配置 = 设备元数据 + 实时值来源。
   * 插件页**不下发设备命令**（平台协议没有该工具）—— 命令在平台设备页下发，
   * 这里如实展示规格与实时状态。实时值读不到时如实报"不可达"，不装作有值。
   */
  /** 把属性原始值排成人话：bool→开/关、数值带单位、枚举用 value-list 描述。 */
  function fmtDetailValue(item, raw, boolYes, boolNo) {
    if (raw === undefined || raw === null) return '—';
    var yes = boolYes || '开';
    var no = boolNo || '关';
    if (typeof raw === 'boolean') {
      if (item && item.values) {
        var vb = item.values[raw ? 1 : 0];
        if (vb) return vb;
      }
      return raw ? yes : no;
    }
    if (typeof raw === 'number') {
      if (item && item.values && item.values[raw] !== undefined) return item.values[raw];
      var s = String(raw);
      if (item && item.unit) s += ' ' + item.unit;
      return s;
    }
    return String(raw);
  }

  /** 详情数据懒加载：缓存命中不重发；应答回来仍选中同一台才重渲染。 */
  function loadDeviceDetail(d) {
    var key = d.did || d.name || '';
    if (!key || devDetailCache[key] || devDetailPending[key]) return;
    devDetailPending[key] = true;
    rpc('plugin.tool.call', { tool: 'device_detail', args: { did: d.did, name: d.name } })
      .then(function (res) {
        delete devDetailPending[key];
        devDetailCache[key] = failed(res) ? { error: failText(res) } : res;
        if (devSelDid === (d.did || '')) renderDetail();
        scheduleHeight();
      })
      .catch(function (e) {
        delete devDetailPending[key];
        devDetailCache[key] = { error: errText(e) };
        if (devSelDid === (d.did || '')) renderDetail();
        scheduleHeight();
      });
  }

  /*
   * 详情页三块**独立卡片**（2026-11-21 用户拿 HA 对照：「三个独立的页面独立的
   * 显示几个能力数」）：
   *
   *   控制 ─ 图标 + 名称 + 右侧开关/读数      （只放主开关、动作、数值）
   *   传感器 ─ 图标 + 名称 + 右侧读数          （只读）
   *   配置 ─ 辅助开关行 + 设备信息卡
   *
   * 每个页签里是一张卡：`detailCard` 出「标题 + 计数」的头，`detailItem` 出行。
   * 插件页不下发设备命令，所以右侧只呈现当前状态（命令在平台设备页下发）——
   * 不画一个点了没反应的开关。
   */
  function detailCard(pane, title, count, hint) {
    var card = document.createElement('div');
    card.className = 'dcard';
    /*
     * 标题只在**没有页签**时才画（2026-11-21 用户裁定：三个页签并排、点击切换）。
     * 页签已经写着「控制 (1) / 传感器 (1) / 配置 (1)」，卡片里再写一遍同一个标题
     * 就是同一句话说两遍 —— 页签在场时传空 title，只留行。
     */
    if (title) {
      var hd = document.createElement('div');
      hd.className = 'dcard__hd';
      var t = document.createElement('span');
      t.className = 'dcard__title';
      t.textContent = title;
      hd.appendChild(t);
      var badge = document.createElement('span');
      badge.className = 'dcard__count';
      badge.textContent = String(count);
      hd.appendChild(badge);
      card.appendChild(hd);
    }
    if (hint) {
      var h = document.createElement('p');
      h.className = 'dcard__hint';
      h.textContent = hint;
      card.appendChild(h);
    }
    pane.appendChild(card);
    return card;
  }

  /** 卡片里的一行：图标 | 名称 | 右侧值。icon 取 dtIcon() 的键。 */
  function detailItem(card, icon, name, value, empty) {
    var row = document.createElement('div');
    row.className = 'ditem';
    var ic = document.createElement('span');
    ic.className = 'ditem__ico';
    ic.innerHTML = dtIcon(icon);
    var nm = document.createElement('span');
    nm.className = 'ditem__name';
    nm.textContent = name;
    var vl = document.createElement('span');
    vl.className = 'ditem__val' + (empty ? ' is-empty' : '');
    vl.textContent = value;
    row.appendChild(ic);
    row.appendChild(nm);
    row.appendChild(vl);
    card.appendChild(row);
    return row;
  }

  /** 行图标（内联 SVG，不用 emoji：跨平台渲染差异太大）。 */
  function dtIcon(kind) {
    var paths = {
      power: '<path d="M12 3v9"/><path d="M6.4 6.4a8 8 0 1 0 11.2 0"/>',
      bulb: '<path d="M9 18h6"/><path d="M10 21h4"/><path d="M8 14a4 4 0 1 1 8 0c0 1.6-1 2.3-1.6 3.2H9.6C9 16.3 8 15.6 8 14z"/>',
      thermo: '<path d="M10 4a2 2 0 1 1 4 0v9a3 3 0 1 1-4 0V4z"/><path d="M12 8v6"/>',
      slider: '<path d="M4 8h10M18 8h2M4 16h4M12 16h8"/><circle cx="16" cy="8" r="2.2"/><circle cx="10" cy="16" r="2.2"/>',
      button: '<rect x="4" y="7" width="16" height="10" rx="5"/><path d="M9 12h6"/>',
      info: '<circle cx="12" cy="12" r="8"/><path d="M12 8v.5M12 11v5"/>'
    };
    return '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" ' +
      'stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">' + (paths[kind] || paths.button) + '</svg>';
  }

  /** 控件 → 图标：开关看是不是主开关，数值看滑杆，动作看按钮。 */
  function dtIconOf(c) {
    if (c.kind === 'action') return 'button';
    if (c.capability === 'number') return 'slider';
    if (c.primary === true) return 'power';
    return 'bulb';
  }

  /** 传感器 → 图标：温度/湿度优先，其余用通用读数图标。 */
  function dtIconOfSensor(sn) {
    var n = String((sn && sn.name) || '');
    if (/温度|温度|temp/i.test(n)) return 'thermo';
    if (/湿度|humid/i.test(n)) return 'thermo';
    return 'thermo';
  }

  /** 详情页里的一行「名 | 值」（复用 kv 行样式）。 */
  function detailRow(pane, k, v) {
    var row = document.createElement('div');
    row.className = 'kv__row';
    var kEl = document.createElement('span');
    kEl.className = 'kv__k';
    kEl.textContent = k;
    var vEl = document.createElement('span');
    vEl.className = 'kv__v';
    vEl.textContent = v;
    row.appendChild(kEl);
    row.appendChild(vEl);
    pane.appendChild(row);
  }

  /**
   * 配置页签里的辅助开关（指示灯开关 / 麦克风静音 / 睡眠模式…）。
   *
   * 布局与「控制」里的主开关一致：图标位 + 名称 + 当前值。插件页不下发命令
   * （命令在平台设备页），所以这里只呈现状态。
   */
  function panelsConfigToggles(pane, items, values) {
    var grid = document.createElement('div');
    grid.className = 'dgrid';
    items.forEach(function (c) {
      var key = c.siid + '.' + c.piid;
      var card = document.createElement('div');
      card.className = 'dcell';
      var nm = document.createElement('span');
      nm.className = 'dcell__k';
      nm.textContent = c.name || key;
      var vl = document.createElement('span');
      vl.className = 'dcell__v';
      var raw = values[key];
      vl.textContent = fmtDetailValue(c, raw);
      if (raw === undefined || raw === null) vl.className = 'dcell__v is-empty';
      card.appendChild(nm);
      card.appendChild(vl);
      grid.appendChild(card);
    });
    pane.appendChild(grid);
  }

  function renderDetail() {
    var box = el('dev-detail');
    box.innerHTML = '';
    var sync = (syncView && syncView.sync) || {};
    var list = sync.device_list || [];
    var d = null;
    list.forEach(function (x) { if (x.did === devSelDid) d = x; });
    if (!d) {
      var p0 = document.createElement('p');
      p0.className = 'muted small';
      p0.textContent = '左侧选择设备查看详情。';
      box.appendChild(p0);
      scheduleHeight();
      return;
    }

    var hd = document.createElement('div');
    hd.className = 'dd__hd';
    var b = document.createElement('b');
    b.textContent = d.name || '未命名';
    hd.appendChild(b);
    var pill = document.createElement('span');
    pill.className = 'pill ' + (d.online === true ? 'pill--ok' : (d.online === false ? 'pill--warn' : 'pill--off'));
    pill.appendChild(document.createElement('i'));
    pill.appendChild(document.createTextNode(d.online === true ? '在线' : (d.online === false ? '离线' : '未知')));
    hd.appendChild(pill);
    box.appendChild(hd);

    var meta = document.createElement('p');
    meta.className = 'dd__meta';
    meta.textContent = '型号 ' + (d.model || '—') + ' · 设备ID ' + (d.did || '—');
    box.appendChild(meta);

    var h = healthByName(d.name);
    // 详情数据：缓存命中直接用；没有就懒加载（回来重渲染）。
    var dkey = d.did || d.name || '';
    loadDeviceDetail(d);
    var detail = devDetailCache[dkey];
    var allControls = (detail && detail.controls) || [];
    var sensors = (detail && detail.sensors) || [];
    var values = (detail && detail.values) || {};
    /*
     * 三栏归属（2026-11-21 用户裁定，与 HA / 平台设备详情同一口径）：
     *   · 控制 = 主开关（primary）+ 动作按钮 + 数值控件
     *   · 配置 = 辅助开关（指示灯开关 / 麦克风静音 / 睡眠模式…）+ 设备信息
     *   · 传感器 = 只读读数
     * 之前的实现把**所有**可写开关都塞进控制，台灯因此出现两个开关；
     * 用户明确：「控制应该只有开关」「配置里放指示灯开关」。
     */
    var controls = allControls.filter(function (c) {
      if (c.kind === 'action') return true;
      if (c.capability === 'number') return true;
      return c.primary === true;
    });
    var auxSwitches = allControls.filter(function (c) {
      return c.kind !== 'action' && c.capability === 'switch' && c.primary !== true;
    });

    var tabs = document.createElement('div');
    tabs.className = 'tabs';
    var panes = {};
    var defs = [
      { key: 'ctl', label: detail ? ('控制 (' + controls.length + ')') : '控制 (…)' },
      { key: 'cfg', label: detail ? ('配置 (' + auxSwitches.length + ')') : '配置 (…)' },
      { key: 'sns', label: detail ? ('传感器 (' + sensors.length + ')') : '传感器 (…)' }
    ];
    defs.forEach(function (def) {
      var tabBtn = document.createElement('button');
      tabBtn.type = 'button';
      tabBtn.className = 'tab';
      tabBtn.textContent = def.label;
      tabBtn.setAttribute('data-tab', def.key);
      tabs.appendChild(tabBtn);

      var pane = document.createElement('div');
      pane.className = 'pane';
      pane.hidden = def.key !== 'ctl';
      panes[def.key] = pane;
      box.appendChild(pane);

      tabBtn.addEventListener('click', function () {
        defs.forEach(function (x) {
          panes[x.key].hidden = (x.key !== def.key);
        });
        var btns = tabs.children;
        for (var i = 0; i < btns.length; i++) {
          btns[i].className = 'tab' + (btns[i].getAttribute('data-tab') === def.key ? ' tab--on' : '');
        }
        scheduleHeight();
      });
    });
    box.appendChild(tabs);

    /*
     * 「页面简单点」（2026-11-21 用户）：控制页签**只列实际能力**。
     * 原来的绿色说明条（"✓ 规格里声明了 N 个控制项…"）与底部"通信路径"都撤掉 ——
     * 它们不是能力本身，占掉的是本该给行的地方；通信路径移到配置里的设备信息。
     */
    if (!detail) {
      var pWait = document.createElement('p');
      pWait.className = 'muted small';
      pWait.textContent = '读取规格与实时值…';
      panes.ctl.appendChild(pWait);
    } else if (detail.error) {
      var pErr = document.createElement('p');
      pErr.className = 'muted small';
      pErr.textContent = '读取失败：' + detail.error;
      panes.ctl.appendChild(pErr);
    } else {
      var ctlCard = detailCard(panes.ctl, '', controls.length);
      controls.forEach(function (c) {
        if (c.kind === 'action') {
          detailItem(ctlCard, dtIconOf(c), c.name || c.instance_id, '按钮 · 平台设备页下发', true);
          return;
        }
        var key = c.siid + '.' + c.piid;
        var v = values[key];
        detailItem(ctlCard, dtIconOf(c), c.name || key, fmtDetailValue(c, v), v === undefined || v === null);
      });
    }
    if (detail && !detail.error && !controls.length) {
      var cNone = document.createElement('p');
      cNone.className = 'muted small';
      cNone.textContent = '该型号规格里没有可写控制项。';
      panes.ctl.appendChild(cNone);
    }

    // —— 传感器页签 ——
    if (!detail) {
      var sWait = document.createElement('p');
      sWait.className = 'muted small';
      sWait.textContent = '读取规格与实时值…';
      panes.sns.appendChild(sWait);
    } else if (detail.error) {
      var sErr = document.createElement('p');
      sErr.className = 'muted small';
      sErr.textContent = '读取失败：' + detail.error;
      panes.sns.appendChild(sErr);
    } else if (!sensors.length) {
      var sNone = document.createElement('p');
      sNone.className = 'muted small';
      sNone.textContent = '规格里没有只读传感器属性。';
      panes.sns.appendChild(sNone);
    } else {
      var snsCard = detailCard(panes.sns, '', sensors.length);
      sensors.forEach(function (sn) {
        var key = sn.siid + '.' + sn.piid;
        var raw = values[key];
        detailItem(snsCard, dtIconOfSensor(sn), sn.name || key,
          fmtDetailValue(sn, raw, '是', '否'), raw === undefined || raw === null);
      });
    }

    // —— 配置页签：辅助开关卡在上，设备信息卡在下 ——
    if (auxSwitches.length) {
      var auxCard = detailCard(panes.cfg, '', auxSwitches.length);
      auxSwitches.forEach(function (c) {
        var key = c.siid + '.' + c.piid;
        var raw = values[key];
        detailItem(auxCard, dtIconOf(c), c.name || key, fmtDetailValue(c, raw), raw === undefined || raw === null);
      });
    }
    var kvCard = document.createElement('div');
    kvCard.className = 'dcard';
    var kvHd = document.createElement('div');
    kvHd.className = 'dcard__hd';
    var kvT = document.createElement('span');
    kvT.className = 'dcard__title';
    kvT.textContent = '设备信息';
    kvHd.appendChild(kvT);
    kvCard.appendChild(kvHd);
    panes.cfg.appendChild(kvCard);
    function addKv(k, v) {
      var row = document.createElement('div');
      row.className = 'ditem';
      var ic = document.createElement('span');
      ic.className = 'ditem__ico';
      ic.innerHTML = dtIcon('info');
      var nm = document.createElement('span');
      nm.className = 'ditem__name';
      nm.textContent = k;
      var vl = document.createElement('span');
      vl.className = 'ditem__val ditem__val--text';
      vl.textContent = v;
      row.appendChild(ic); row.appendChild(nm); row.appendChild(vl);
      kvCard.appendChild(row);
    }
    addKv('设备ID', d.did || '—');
    addKv('型号', d.model || '—');
    addKv('实时在线', d.online === true ? '在线' : (d.online === false ? '离线' : '未知'));
    addKv('通信路径', h ? ((h.path || '—') + (h.path_detail ? ' · ' + h.path_detail : '')) : '—');
    if (detail && detail.values_source) {
      addKv('实时值来源', detail.values_source === 'lan' ? '局域网直连'
        : detail.values_source === 'cloud' ? '云端'
        : '不可达' + (detail.values_error ? '：' + detail.values_error : ''));
    }

    // 默认选中第一个页签
    tabs.children[0].className = 'tab tab--on';
    scheduleHeight();
  }

  /* ── 健康检查 + 局域网状态 ────────────────────────────── */

  function loadHealth() {
    return rpc('plugin.tool.call', { tool: 'health_list', args: {} })
      .then(function (res) {
        if (failed(res)) {
          var box = el('health-table');
          box.innerHTML = '';
          var p = document.createElement('p');
          p.className = 'muted small';
          p.textContent = '读取失败：' + failText(res);
          box.appendChild(p);
          updateLanStatus(failText(res));
          scheduleHeight();
          return;
        }
        healthCache = res;
        healthLoaded = true;
        renderHealth(res);
        updateLanStatus();
        // 设备详情里的「通信路径」取自健康表 —— 健康数据晚于设备列表到达时，
        // 重渲染设备区让路径当场补上（否则详情一直显示"暂无健康检查数据"）。
        if (syncView) renderDevices();
      })
      .catch(function (e) {
        updateLanStatus(errText(e));
        scheduleHeight();
      });
  }

  /*
   * 健康检查表：三条真实通信路径分别检测，状态语义必须分清：
   *   在线     —— 局域网 UDP 探测通（有独立 IP 的设备）
   *   离线     —— 超时无响应
   *   网关读取 —— 蓝牙/Zigbee 设备，数据经音箱网关取
   *   云端可用 —— 红外遥控，单向发射无回读，"账号有效即可用"
   * 「云端可用」绝不是「离线」—— 前者正常，后者是故障。
   */
  function renderHealth(data) {
    var box = el('health-table');
    if (!box) return;
    fillHealthFilters();
    var all = (data && data.devices) || [];
    /*
     * 按账号过滤（2026-11-20 用户要求：局域网检查列表与同步结果一样，
     * 多账号时按账号筛）。设备行的 account 是同步来源账号（登记戳）。
     */
    var selName = healthFilterAccount ? (accountNamesCache[healthFilterAccount] || '') : '';
    var list = all;
    if (selName) {
      list = [];
      for (var fi = 0; fi < all.length; fi++) {
        if ((all[fi] && all[fi].account) === selName) list.push(all[fi]);
      }
    }
    box.innerHTML = '';
    if (!all.length) {
      var p = document.createElement('p');
      p.className = 'muted small';
      p.textContent = '暂无设备。';
      box.appendChild(p);
      scheduleHeight();
      return;
    }
    if (!list.length) {
      var pe = document.createElement('p');
      pe.className = 'muted small';
      pe.textContent = '所选账号没有健康检查数据（该账号的设备尚未同步进本机登记）。';
      box.appendChild(pe);
      scheduleHeight();
      return;
    }
    var html = '<table><thead><tr>' +
      '<th>设备名称</th><th>型号</th><th style="width:120px">版本</th>' +
      '<th style="width:100px">健康状态</th><th style="width:92px">耗时</th><th style="width:170px">通信路径</th>' +
      '</tr></thead><tbody>';
    for (var i = 0; i < list.length; i++) {
      var d = list[i] || {};
      var status = d.status || '';
      var cls = status === '在线' ? 'on'
        : status === '离线' ? 'off'
        : status === '云端可用' ? 'cloud' : 'gw';
      var lat = d.latency || '不适用';
      var latCls = lat === '不适用' ? 'na' : (status === '在线' ? 'ok' : 'bad');
      var pathText = (d.path || '—') + (d.path_detail ? ' · ' + d.path_detail : '');
      html += '<tr>' +
        '<td><span class="hname">' + esc(d.name) + '</span>' +
        (d.account ? '<span class="hsub">' + esc(d.account) + '</span>' : '') +
        (d.path_detail ? '<span class="hsub">' + esc(d.path_detail) + '</span>' : '') + '</td>' +
        '<td><span class="hver">' + esc(d.model) + '</span></td>' +
        '<td><span class="hver">' + esc(d.version || '-') + '</span></td>' +
        '<td><span class="hst hst--' + cls + '">' + esc(status) + '</span></td>' +
        '<td><span class="hlat hlat--' + latCls + '">' + esc(lat) + '</span></td>' +
        '<td class="muted small">' + esc(pathText) + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    box.innerHTML = html;
    scheduleHeight();
  }

  /*
   * 健康表过滤下拉（2026-11-20 用户要求：局域网检查也要有账号/家庭筛选）。
   * 与设备管理同一套选项；选择保存在 healthFilterAccount，renderHealth 按它过滤。
   */
  function fillHealthFilters() {
    var accSel = el('f-health-account');
    if (accSel) {
      accSel.innerHTML = '';
      var allOpt = document.createElement('option');
      allOpt.value = '';
      allOpt.textContent = '全部账号';
      accSel.appendChild(allOpt);
      accountsCache.forEach(function (a) {
        var id = a.account_id || '';
        var opt = document.createElement('option');
        opt.value = id;
        opt.textContent = accountNamesCache[id] || a.masked_account || id;
        accSel.appendChild(opt);
      });
      accSel.value = healthFilterAccount || '';
    }
    var hh = el('f-health-household');
    if (hh) {
      hh.innerHTML = '';
      if (household) {
        var o = document.createElement('option');
        o.value = household.id;
        o.textContent = household.name;
        hh.appendChild(o);
        hh.value = household.id;
      } else {
        var o2 = document.createElement('option');
        o2.value = '';
        o2.textContent = '当前家庭';
        hh.appendChild(o2);
        hh.value = '';
      }
    }
  }

  /** 控制通道卡里的局域网状态行：由健康检查结果聚合。 */
  function updateLanStatus(errMsg) {
    var box = el('lan-status');
    if (!box) return;
    if (errMsg) {
      box.className = 'lanstat lanstat--none';
      box.textContent = '局域网状态暂不可用：' + errMsg;
      scheduleHeight();
      return;
    }
    if (!healthLoaded) {
      box.className = 'lanstat lanstat--none';
      box.textContent = '⏳ 正在检测局域网状态…';
      scheduleHeight();
      return;
    }
    var devs = (healthCache && healthCache.devices) || [];
    var lan = [];
    devs.forEach(function (d) { if (d.path === '局域网直连') lan.push(d); });
    if (!devs.length) {
      box.className = 'lanstat lanstat--none';
      box.textContent = '暂无设备 — 同步设备后这里会显示局域网状态。';
    } else if (!lan.length) {
      box.className = 'lanstat lanstat--none';
      box.textContent = '当前设备均不经局域网直连（网关 / 云端路径）。';
    } else {
      var on = 0;
      lan.forEach(function (d) { if (d.status === '在线') on += 1; });
      if (on > 0) {
        box.className = 'lanstat lanstat--ok';
        box.textContent = '✓ 局域网连接正常（' + on + '/' + lan.length + ' 台在线）';
      } else {
        box.className = 'lanstat lanstat--warn';
        box.textContent = '⚠ 局域网连接异常（0/' + lan.length + ' 台在线）— 命令将自动走云端转发';
      }
    }
    scheduleHeight();
  }

  /* ── 接线 ─────────────────────────────────────────────── */

  function bind() {
    el('btn-back').addEventListener('click', function () {
      // iframe 是 opaque origin，只有宿主能导航 —— 走协议 op。
      rpc('plugin.ui.close').catch(function () {});
    });
    el('btn-reload-accounts').addEventListener('click', function () {
      loadAccounts();
      loadSyncResult();
    });
    el('btn-add').addEventListener('click', openLogin);
    el('btn-cancel-login').addEventListener('click', function () {
      el('login-form').hidden = true;
      el('f-pass').value = '';
      scheduleHeight();
    });
    el('btn-login').addEventListener('click', startLogin);
    el('btn-logout-session').addEventListener('click', logoutAccount);
    el('btn-verify').addEventListener('click', submitVerify);
    el('btn-verify-web').addEventListener('click', submitVerifyWeb);

    // Token 自动续期（页头开关）：即时保存 + 联动
    var sp = el('f-save-password');
    if (sp) {
      sp.addEventListener('change', function () {
        var on = !!sp.checked;
        saveConfigNow({ save_password: on }, on ? 'Token 自动续期已开启' : 'Token 自动续期已关闭')
          .then(function () {
            applyTokenLinkage(on);
            renderAccounts(accountsCache, accountNamesCache);
          });
      });
    }
    // 控制通道：优先级即时保存
    var radios = document.querySelectorAll('input[name="lan-priority"]');
    for (var i = 0; i < radios.length; i++) {
      (function (r) {
        r.addEventListener('change', function () {
          if (r.checked) saveConfigNow({ lan_priority: r.value }, '控制通道已保存');
        });
      })(radios[i]);
    }
    // 检测间隔：即时保存（范围 5~3600，后端校验）
    var li = el('f-lan-interval');
    if (li) {
      li.addEventListener('change', function () {
        var v = parseInt(li.value, 10);
        if (!(v >= 5 && v <= 3600)) { v = 5; li.value = 5; }
        var lbl = el('health-interval-label');
        if (lbl) lbl.textContent = '自动检查周期：每 ' + v + ' 秒';
        saveConfigNow({ lan_check_interval_seconds: v }, '检测间隔已保存');
      });
    }
    // 同步任务
    el('sw-polling').addEventListener('click', function () { setPolling(!pollingOn()); });
    el('btn-add-sync-ok').addEventListener('click', submitAddTask);
    // 列表切换页签：同步结果 / 局域网检查（并排放置，点击切到对应列表）
    el('tab-dev').addEventListener('click', function () { showTab('dev'); });
    el('tab-health').addEventListener('click', function () { showTab('health'); });
    // 设备管理
    var dr = el('btn-dev-refresh');
    if (dr) dr.addEventListener('click', function () { loadSyncResult(); });
    var fa = el('f-dev-account');
    if (fa) {
      fa.addEventListener('change', function () {
        devFilterAccount = fa.value;
        renderDevices();
      });
    }
    // 健康检查
    var hb = el('btn-health-refresh');
    if (hb) hb.addEventListener('click', function () { loadHealth(); });
    // 健康表账号过滤（2026-11-20 用户要求：局域网检查列表按账号筛选）
    var fha = el('f-health-account');
    if (fha) {
      fha.addEventListener('change', function () {
        healthFilterAccount = fha.value;
        renderHealth(healthCache);
      });
    }
  }

  var booted = false;
  var firstLoadPending = true;

  /*
   * 请求启动（首个业务请求）。
   * 端口建立与 host-ready 是两条独立的就绪路径；业务请求只能在宿主 attach
   * 之后发。任一信号到达即执行；两条都没来时 1.2 秒后兜底再试。
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
     * 账号列表必须**独立**加载（2026-11-09 修「卡在读取中」的结构性原因）：
     * config.get 失败或 renderConfig 抛错都不能拖累账号列表 ——
     * 两块数据本无依赖关系，各自独立发起，任一失败都不拖累另一个。
     */
    loadAccounts();
    loadSyncResult();
    loadHealth();
    rpc('plugin.config.get', {})
      .then(function (res) {
        config = (res && res.config) || {};
        household = (res && res.household) || null;
        renderConfig();
        return loadAccounts();
      })
      .catch(function (e) { banner('读取配置失败：' + errText(e), 'err'); });
  }

  bind();
  scheduleHeight();

  // 兜底：两条就绪信号都没到也把首个请求发出去（宁可失败一次让人看到
  // 可操作的错误，也不要永久停在「读取中…」）。
  setTimeout(function () {
    if (firstLoadPending && port) requestBoot();
    flushPreReady();
  }, 1200);
})();
