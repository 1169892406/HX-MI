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

  var HX = 'homex.plugin.ui/1';
  var RPC_TIMEOUT_MS = 30000;

  var port = null;
  var connected = false;
  var pending = Object.create(null);
  var seq = 0;
  var pluginId = '';

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

  function renderConfig() {
    el('f-region').value = config.region || 'cn';
    el('f-interval').value = config.sync_interval_seconds || 300;
    el('sw-polling').setAttribute('aria-checked', config.polling_enabled === false ? 'false' : 'true');
    var f = config.device_filter_models;
    el('f-filter').value = Array.isArray(f) ? f.join(', ') : '';
  }

  function collectConfig() {
    var out = Object.assign({}, config);
    out.region = el('f-region').value || 'cn';
    var iv = parseInt(el('f-interval').value, 10);
    out.sync_interval_seconds = isNaN(iv) ? 300 : iv;
    out.polling_enabled = el('sw-polling').getAttribute('aria-checked') === 'true';
    out.device_filter_models = el('f-filter').value
      .split(',')
      .map(function (s) { return s.trim(); })
      .filter(function (s) { return s !== ''; });
    return out;
  }

  function saveConfig() {
    el('btn-save').disabled = true;
    clearBanner();
    rpc('plugin.config.save', { config: collectConfig() })
      .then(function () {
        showBanner('配置已保存（热生效）', 'ok');
      })
      .catch(function (e) { showBanner('保存失败：' + errText(e), 'err'); })
      .then(function () { el('btn-save').disabled = false; });
  }

  /* ── 账号列表 ──────────────────────────────────────────── */

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
    items.forEach(function (a) {
      var id = a.account_id || a.AccountID || '';
      var name = names[id] || a.masked_account || a.MaskedAccount || id;
      var row = document.createElement('div');
      row.className = 'acc';

      var t = document.createElement('div');
      t.className = 'acc__t';
      var b = document.createElement('b');
      b.textContent = name;
      var s = document.createElement('span');
      s.textContent = '账号 ID：' + id;
      t.appendChild(b);
      t.appendChild(s);
      row.appendChild(t);

      var spacer = document.createElement('span');
      spacer.className = 'spacer';
      row.appendChild(spacer);

      var pill = document.createElement('span');
      pill.className = 'pill' + (a.sync_enabled ? ' pill--ok' : '');
      var dot = document.createElement('i');
      pill.appendChild(dot);
      pill.appendChild(document.createTextNode(a.sync_enabled ? '自动同步' : '仅手动'));
      row.appendChild(pill);

      var del = document.createElement('button');
      del.type = 'button';
      del.className = 'btn btn--sm btn--danger';
      del.textContent = '删除';
      del.addEventListener('click', function () { removeAccount(id, a.revision); });
      row.appendChild(del);

      box.appendChild(row);
    });
    scheduleHeight();
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

  function syncNow() {
    el('btn-sync-now').disabled = true;
    rpc('plugin.tool.call', { tool: 'account_sync', args: {} })
      .then(function (res) {
        var h = el('sync-hint');
        h.hidden = false;
        // 业务失败帧与"状态不是 succeeded"是两件不同的事，都要如实说。
        if (failed(res)) {
          h.textContent = '同步失败：' + failText(res);
        } else if (res && res.state === 'succeeded') {
          h.textContent = '同步完成：' + JSON.stringify(res.summary || {});
        } else {
          h.textContent = '同步失败：' + ((res && res.error) || '原因未提供');
        }
        scheduleHeight();
      })
      .catch(function (e) { showBanner('同步失败：' + errText(e), 'err'); })
      .then(function () { el('btn-sync-now').disabled = false; });
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
  function logoutAccount() {
    var sid = sessionId;
    if (!sid) {
      showBanner('当前没有进行中的登录会话可退出；如需清除已登录账号请用账号行的「删除」', 'warn');
      return;
    }
    if (!window.confirm('退出当前登录会话？已保存的账号凭据不会被删除。')) return;
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
    el('btn-reload-accounts').addEventListener('click', loadAccounts);
    el('btn-sync-now').addEventListener('click', syncNow);
    el('btn-diag').addEventListener('click', loadDiag);
    el('btn-catalog-status').addEventListener('click', loadCatalogStatus);
    el('btn-catalog-refresh').addEventListener('click', refreshCatalog);
    el('btn-lan-health').addEventListener('click', runLanHealth);
    el('btn-logout').addEventListener('click', logoutAccount);
    el('sw-polling').addEventListener('click', function () {
      var cur = el('sw-polling').getAttribute('aria-checked') === 'true';
      el('sw-polling').setAttribute('aria-checked', cur ? 'false' : 'true');
    });
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
    rpc('plugin.config.get', {})
      .then(function (res) {
        config = (res && res.config) || {};
        renderConfig();
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
