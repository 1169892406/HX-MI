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

  function rpc(op, payload) {
    return new Promise(function (resolve, reject) {
      if (!port) {
        reject(makeErr('PLUGIN_UI_FRAME_NOT_ATTACHED', '还没有连上平台（稍候或重新打开配置页）'));
        return;
      }
      var id = nextId();
      var timer = setTimeout(function () {
        if (pending[id]) {
          delete pending[id];
          reject(makeErr('PLUGIN_UI_TIMEOUT', '平台没有在超时内应答'));
        }
      }, RPC_TIMEOUT_MS);
      pending[id] = { resolve: resolve, reject: reject, timer: timer };
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
    if (m.type === 'host-ready') {
      connected = true;
      startHeightReporting();
      boot();
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
        p.reject(makeErr(err.code || 'PLUGIN_UI_HANDLER_FAILED', err.message || '平台拒绝了这次操作'));
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
    boot();
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
        var b64 = (res && (res.image_base64 || res.image)) || '';
        if (!b64) return;
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
        h.textContent = res && res.state === 'succeeded'
          ? '同步完成：' + JSON.stringify(res.summary || {})
          : '同步失败：' + ((res && res.error) || '原因未提供');
        scheduleHeight();
      })
      .catch(function (e) { showBanner('同步失败：' + errText(e), 'err'); })
      .then(function () { el('btn-sync-now').disabled = false; });
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
    el('sw-polling').addEventListener('click', function () {
      var cur = el('sw-polling').getAttribute('aria-checked') === 'true';
      el('sw-polling').setAttribute('aria-checked', cur ? 'false' : 'true');
    });
  }

  var booted = false;

  function boot() {
    if (booted || !port) return;
    booted = true;
    rpc('plugin.config.get', {})
      .then(function (res) {
        config = (res && res.config) || {};
        renderConfig();
        loadAccounts();
        scheduleHeight();
      })
      .catch(function (e) { showBanner('读取配置失败：' + errText(e), 'err'); });
  }

  bind();
  scheduleHeight();
})();
