/**
 * 插件自带界面的握手闸门（2026-11-09；2026-11-20 随新设计图重做断言目标）。
 *
 * # 为什么需要它（这个 bug 花了十几轮才找到）
 *
 * `ui/app.js` 的协议常量曾写成字符串 `'homex.plugin.ui/1'`，而平台发的是数字
 * `1`（`PLUGIN_UI_PROTOCOL`）。每一处守卫都是严格比较 `m.hx !== HX`，于是
 * **平台发来的每一帧都被丢弃**：connect 不建端口、host-ready 不触发首个请求、
 * RPC 应答永远收不到。
 *
 * 用户看到的形态是"界面全都不对"：账号列表卡「读取中…」、点按钮报
 * `PLUGIN_UI_FRAME_NOT_ATTACHED：还没有连上平台`。这些现象**指向平台**，
 * 而真身在插件里 —— 靠读代码很难发现，靠真机调试要来回好几轮。
 *
 * # 这个脚本做什么
 *
 * 用桩环境（假 window/document/port）加载**生产 app.js 本体**，模拟平台发
 * `connect` + `host-ready`，然后断言插件确实发出了首个业务请求。
 * 常量写错、守卫写错、boot 被卡住，都会在这里当场失败。
 *
 * 2026-11-20 起界面按新设计图重做（同步任务表 + 行内编辑条、设备管理统计
 * 磁贴 + 列表 + 详情、即时保存模型），U19/U20 断言目标随之更新 —— 但闸门的
 * 精神不变：**每个按钮点了都得有反应，业务失败不得被说成"没有数据"**。
 *
 * 运行：node tools/check-ui-handshake.mjs
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const APP_JS = path.join(HERE, '..', 'ui', 'app.js');

/** 平台侧协议常量（与 pluginUiProtocol.ts 的 PLUGIN_UI_PROTOCOL 同值）。 */
const PLATFORM_HX = 1;

let failed = 0;
const HTML = fs.readFileSync(path.join(HERE, '..', 'ui', 'index.html'), 'utf8');
/** 递归收集桩元素的文本（假 DOM 不算 textContent，只能自己走一遍）。 */
function collectText(node) {
  if (!node) return '';
  if (Array.isArray(node)) return node.map(collectText).join(' ');
  const own = typeof node.textContent === 'string' ? node.textContent : '';
  const kids = Array.isArray(node.children) ? node.children.map(collectText).join(' ') : '';
  return (own + ' ' + kids).trim();
}
/** 递归找第一个满足条件的后代（闸门用它定位动态渲染出的按钮）。 */
function deepFind(node, pred) {
  if (!node) return null;
  if (pred(node)) return node;
  const kids = Array.isArray(node.children) ? node.children : [];
  for (const k of kids) {
    const r = deepFind(k, pred);
    if (r) return r;
  }
  return null;
}
function deepFindAll(node, pred, out) {
  if (!node) return out;
  if (pred(node)) out.push(node);
  const kids = Array.isArray(node.children) ? node.children : [];
  for (const k of kids) deepFindAll(k, pred, out);
  return out;
}

const ok = (name, cond, detail) => {
  if (cond) console.log(`  ✔ ${name}`);
  else {
    failed += 1;
    console.error(`  ✘ ${name}  ${detail || ''}`);
  }
};

const code = fs.readFileSync(APP_JS, 'utf8');

// ① 源码级：协议常量必须与平台同值（数字，不是字符串）。
{
  // 先剥离注释：注释里也写了 `var HX = 1` 作为说明，直接正则会被它误匹配。
  const codeNoComments = code
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1');
  const m = codeNoComments.match(/var\s+HX\s*=\s*([^;]+);/);
  ok('协议常量存在', !!m, '找不到 var HX = ...');
  if (m) {
    const raw = m[1].trim();
    ok(
      `HX 与平台同值（期望 ${PLATFORM_HX}，实际 ${raw}）`,
      raw === String(PLATFORM_HX),
      `必须写数字 ${PLATFORM_HX}：平台用严格比较，字符串会被丢弃每一帧`,
    );
  }
}

// ② 行为级：桩环境跑一遍真实握手。
class FakeEl {
  constructor(id) {
    this.id = id; this.style = {}; this.attrs = {}; this.children = [];
    this.hidden = false; this.disabled = false; this.value = '';
    this.textContent = ''; this.innerHTML = ''; this.listeners = {};
    this.classList = { add() {}, remove() {}, toggle() {} };
  }
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener() {}
  /** 模拟点击（闸门用它逐个按钮验证"点了有没有反应"）。 */
  fire(t) { (this.listeners[t] || []).forEach((fn) => fn({ type: t, preventDefault() {} })); }
  get parentNode() { return this._parent || null; }
  removeChild(c) { if (Array.isArray(this.children)) this.children = this.children.filter((x) => x !== c); return c; }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return this.attrs[k] ?? null; }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(c) { if (c) c._parent = this; this.children.push(c); return c; }
  querySelector() { return null; } querySelectorAll() { return []; }
  getBoundingClientRect() { return { height: 900, width: 1080, left: 86, top: 0 }; }
  focus() {} blur() {} click() {}
}

const els = new Map();
const bodyEl = new FakeEl('body');
els.set('body', bodyEl);

const getEl = (id) => { if (!els.has(id)) els.set(id, new FakeEl(id)); return els.get(id); };
const winListeners = {};

global.window = {
  addEventListener(t, fn) { (winListeners[t] ||= []).push(fn); },
  removeEventListener() {}, confirm: () => true,
  location: { origin: 'http://localhost', href: 'http://localhost/' },
  setTimeout, clearTimeout, setInterval, clearInterval,
};
const fakeTextNode = (t) => ({
  nodeType: 3, textContent: String(t), children: [], style: {},
  appendChild() {}, setAttribute() {}, removeAttribute() {},
});
global.document = {
  getElementById: getEl, querySelector: () => null, querySelectorAll: () => [],
  createElement: (t) => new FakeEl('new-' + t),
  createTextNode: (t) => fakeTextNode(t),
  documentElement: new FakeEl('html'), body: bodyEl,
  addEventListener() {},
};
global.ResizeObserver = class { observe() {} disconnect() {} };

const posted = [];
let portHandler = null;
const fakePort = {
  postMessage(m) { posted.push(m); }, start() {},
  set onmessage(fn) { portHandler = fn; }, get onmessage() { return portHandler; },
};

// ③ 页面**真的会加载**这份脚本吗（2026-11-10 踩过）。
{
  const hasScript = /<script[^>]+src=["']app\.js["'][^>]*>/i.test(HTML);
  ok('index.html 引用了 app.js', hasScript, '没有 <script src="app.js"> ⇒ 页面永远停在初始占位，且无任何报错');
  const hasCss = /<link[^>]+href=["']style\.css["'][^>]*>/i.test(HTML);
  ok('index.html 引用了 style.css', hasCss, '没有样式表 ⇒ 页面会裸奔（功能在但长得像坏了）');
  // 反向自证：把 script 标签拿掉，判据必须转红。
  ok(!/<script[^>]+src=["']app\.js["'][^>]*>/i.test(HTML.replace(/<script[^>]+src=["']app\.js["'][^>]*><\/script>/i, '')),
    '自证：剥掉 script 标签后同一条判据会转红', '否则这条断言只是把今天的写法供起来');
}

/*
 * 按真实 HTML 初始化 `hidden`。
 */
for (const m of HTML.matchAll(/<[^>]*\bid="([^"]+)"[^>]*>/g)) {
  const tag = m[0];
  if (/\shidden(\s|>|=)/.test(tag)) getEl(m[1]).hidden = true;
}

let threw = null;
try {
  new Function('window', 'document', 'ResizeObserver', code)(
    global.window, global.document, global.ResizeObserver,
  );
} catch (e) { threw = e; }
ok('脚本可加载（无运行时异常）', !threw, threw && threw.message);

ok('已注册 message 监听（收 connect 的入口）',
  (winListeners.message || []).length > 0, '没有 window.addEventListener("message")');

/*
 * 应答 RPC：**这一步是必需的**。只验"帧发得出去"，等于把最常出事的那一段
 * （拿数据 → 渲染）整个漏掉。
 */
const ACCOUNTS_OK = {
  accounts: [{
    account_id: 'acc_demo', masked_account: '1***@qq.com', region: 'cn',
    household_id: 'hh-1', state: 'ready', revision: 1,
    // ⚠ 真实形状是**嵌套**的（account_store.go 的 account.Sync 是结构体）。
    sync: { enabled: true, interval_seconds: 300 },
  }],
  account_names: { acc_demo: '1169892406@qq.com' },
  // 2026-11-20 用户要求账号列显示票据状态：更新时间 + 会话归属账号。
  token_updated_at: '2026-11-20T07:36:00Z',
  token_account: '1169892406@qq.com',
};
const ACCOUNT_VIEW = {
  state: 'no_session', device_count: 0,
  sync: {
    enabled: true, running: false, devices: 2, controllable: 1, uncontrollable: 1,
    last_ok_at: '2026-11-20T06:30:14Z', next_sync_at: '2026-11-20T06:35:14Z',
    device_list: [
      { did: '100', name: '台灯', model: 'chuangmi.plug.m3', controllable: true, available: true, online: true },
      { did: '101', name: '补光灯', model: 'chuangmi.plug.m1', controllable: true, available: true, online: false },
    ],
  },
};
const CONFIG_OK = {
  plugin_id: 'homex.xiaomi_miot', version: '0.0.0-test', legacy: false,
  config: { region: 'cn', sync_interval_seconds: 300, polling_enabled: true, device_filter_models: [] },
  config_schema: {}, secret_fields: [], config_secrets_set: [],
  household: { id: 'hh-1', name: '我的HOMEX家' },
};

const rawPost = fakePort.postMessage.bind(fakePort);
fakePort.postMessage = (frame) => {
  rawPost(frame);
  if (!frame || typeof frame.op !== 'string') return;
  let result = { ok: true };
  if (frame.op === 'plugin.config.get') result = CONFIG_OK;
  else if (frame.op === 'plugin.tool.call') {
    const tool = frame.payload && frame.payload.tool;
    if (tool === 'accounts') result = ACCOUNTS_OK;
    else if (tool === 'account') result = ACCOUNT_VIEW;
    else if (tool === 'health_list') result = { devices: [
      { name: '台灯', model: 'chuangmi.plug.m3', version: '1.2.3', path: '局域网直连', status: '在线', latency_ms: 12,
        account: '1169892406@qq.com' },
      { name: '补光灯', model: 'chuangmi.plug.m1', version: '1.0.9', path: '云端下发', status: '离线', latency_ms: 0 },
    ] };
  }
  // 真的投递回插件（真实 MessagePort 会做这件事；假端口不会）。
  if (portHandler) portHandler({ data: { hx: PLATFORM_HX, id: frame.id, ok: true, result } });
};

// 平台发 connect
for (const fn of (winListeners.message || [])) {
  fn({ data: { hx: PLATFORM_HX, type: 'connect', plugin_id: 'homex.xiaomi_miot' }, ports: [fakePort], source: {} });
}
ok('connect 被接受（端口建立）', portHandler !== null,
  'hx 不匹配时 connect 会被静默丢弃 —— 这就是"全都不对"的根因');

// 平台发 host-ready
if (portHandler) portHandler({ data: { hx: PLATFORM_HX, type: 'host-ready' } });

await new Promise((r) => setTimeout(r, 400));

const calls = posted.filter((m) => m.op === 'plugin.tool.call');
ok('收到 host-ready 后发出首个业务请求', calls.length > 0,
  '未发出任何 plugin.tool.call：首屏会永久停在初始占位');
ok('首个请求是账号列表（不依赖 config.get）',
  calls[0]?.payload?.tool === 'accounts',
  `实际首个工具: ${calls[0]?.payload?.tool}`);
ok('也请求了配置', posted.some((m) => m.op === 'plugin.config.get'), '未请求配置');
ok('所有出站帧的 hx 与平台同值',
  posted.every((m) => m.hx === PLATFORM_HX),
  '出站帧的 hx 与平台不一致，宿主会丢弃它们');

/* ══ 渲染路径：拿数据 → 渲染，判据直接看桩 DOM ══════════════════════ */
console.log('== U19 回来的数据真的能渲染');
{
  const accountsText = collectText(getEl('accounts'));
  ok('U19a 账号区有内容', accountsText.length > 0,
    '空 = renderAccounts 没跑到（或第一个分支就 return）');
  ok('U19b 账号区没有失败文案', !accountsText.includes('失败'), `实际：${accountsText.slice(0, 120)}`);
  ok('U19c 账号区渲染出了行', getEl('accounts').children.length >= 1,
    `子节点 ${getEl('accounts').children.length}`);

  const syncText = collectText(getEl('sync-table'));
  ok('U19d 同步任务表渲染出该条周期（每 N 分钟）', syncText.includes('每'),
    `实际：${syncText.slice(0, 120)}`);
  ok('U19e 行内状态取自**嵌套** sync.enabled（已启用）', syncText.includes('已启用'),
    '夹具给的是嵌套形状；显示"已停用"说明还在按扁平字段读（那个字段不存在）');
  ok('U19f 同步表有操作按钮（立即同步）', syncText.includes('立即同步'),
    '操作入口必须常显');

  const tilesText = collectText(getEl('res-tiles'));
  ok('U19g 统计磁贴有"离线"格', tilesText.includes('离线'),
    `实际：${tilesText.slice(0, 120)}`);
  ok('U19h 设备列表渲染出设备', collectText(getEl('dev-list')).includes('台灯'),
    `实际：${collectText(getEl('dev-list')).slice(0, 120)}`);

  // 2026-11-20 结构修正（用户裁定）：同步任务独立卡片；「同步结果」与
  // 「局域网检查」是**两个列表**的并排页签，挂在结果卡片上。
  // （桩 document.querySelector 恒 null —— 这里按 index.html 源码断言结构。）
  const sec3 = HTML.split('<section class="sec">')[3] || '';
  ok('U19i 同步任务是独立卡片（标题无页签）',
    sec3.includes('<h2>同步任务</h2>') && !sec3.includes('class="seg"'),
    '第③区应为「同步任务」且不带 seg 页签');
  const sec4 = HTML.split('<section class="sec">')[4] || '';
  ok('U19i2 结果卡片标题为「同步结果与局域网检查」且带页签',
    sec4.includes('<h2>同步结果与局域网检查</h2>')
      && sec4.includes('id="tab-dev"') && sec4.includes('id="tab-health"'),
    '第④区应挂同步结果 / 局域网检查并排页签');
  // 健康表走 innerHTML 路径（FakeEl 不解析 HTML，文本抓不到）——
  // 判据改为「renderHealth 真的跑了」：innerHTML 非空且包含表头列名。
  const hth = getEl('health-table');
  ok('U19j 局域网检查列表真的渲染（表头+行数据）',
    typeof hth.innerHTML === 'string' && hth.innerHTML.includes('设备名称')
      && hth.innerHTML.includes('台灯') && hth.innerHTML.includes('局域网直连'),
    `innerHTML 长度 ${String(hth.innerHTML || '').length}`);

  // 2026-11-20 用户要求三连：账号列显示票据状态；页面删冗余小字；
  // 局域网检查列表挂账号/家庭筛选框。
  ok('U19k 账号行显示「Token 更新于」（会话归属行）',
    collectText(getEl('accounts')).includes('Token 更新于'),
    'accounts 工具给了 token_updated_at/token_account，对应行必须显示');
  ok('U19l 健康表行带同步来源账号副行',
    typeof hth.innerHTML === 'string' && hth.innerHTML.includes('1169892406@qq.com'),
    '健康行 account 字段要显示出来，否则按账号过滤没法核对');
  ok('U19m 冗余小字已删（风险盒/探测备注/绑定备注/拆分备注）',
    !HTML.includes('riskbox') && !HTML.includes('nohint')
      && !HTML.includes('addsync-note') && !HTML.includes('filters__note'),
    '2026-11-20 用户裁定：页面小字太多，说明性重复文案删除');
  ok('U19n 局域网检查列表有账号/家庭筛选框',
    HTML.includes('id="f-health-account"') && HTML.includes('id="f-health-household"'),
    '与同步结果一致的筛选框（多账号时过滤）');
}

/* ══ U20 每个按钮点了都得有反应 ══════════════════════════════════════
 *
 * 起因：用户报「点击删除无效」。真身不是删除逻辑错，而是插件界面跑在
 * `sandbox="allow-scripts"` 的 iframe 里 —— **没有 allow-modals**，
 * `window.confirm()` 不弹窗、直接返回 false。两条一起盖：
 *   ① 静态：源码（剥注释后）里不得再出现 window.confirm / alert / prompt；
 *   ② 行为：逐个点一遍并断言效果 —— 发帧 / 展开表单 / 出现确认框。
 */
console.log('== U20 按钮点击都有反应');
{
  const noComments = code
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/.*$/gm, '$1');
  for (const fn of ['confirm', 'alert', 'prompt']) {
    ok(`U20a 源码不使用 window.${fn}`,
      !new RegExp(`(window\\.|\\b)${fn}\\s*\\(`).test(noComments),
      `插件界面在 sandbox="allow-scripts" 的 iframe 里没有 allow-modals，` +
      `window.${fn} 不弹窗直接返回 false ⇒ 按钮会变哑且无任何报错`);
  }

  const clickAndCollect = async (id, label) => {
    const n = posted.length;
    getEl(id).fire('click');
    await new Promise((r) => setTimeout(r, 40));
    const ops = posted.slice(n).filter((m) => m.op).map((m) => m.op);
    return ops;
  };
  const hasConfirm = () => bodyEl.children.filter((c) => c.className === 'confirm').length > 0;

  // —— 发帧类 ——
  const backOps = await clickAndCollect('btn-back', '返回插件列表');
  ok('U20b 「返回插件列表」发出 plugin.ui.close',
    backOps.includes('plugin.ui.close'), `实际：${backOps.join(',') || '（无帧）'}`);

  const reloadOps = await clickAndCollect('btn-reload-accounts', '刷新');
  ok('U20c 「刷新」重新取账号', reloadOps.includes('plugin.tool.call'), `实际：${reloadOps.join(',') || '（无帧）'}`);

  const devROps = await clickAndCollect('btn-dev-refresh', '刷新设备');
  ok('U20d 「刷新设备」发出请求', devROps.includes('plugin.tool.call'), `实际：${devROps.join(',') || '（无帧）'}`);

  const healthOps = await clickAndCollect('btn-health-refresh', '立即检查');
  ok('U20e 「立即检查」发出请求', healthOps.includes('plugin.tool.call'), `实际：${healthOps.join(',') || '（无帧）'}`);

  // —— 列表页签（2026-11-20 用户裁定：同步结果 / 局域网检查并排页签）——
  ok('U20r 默认显示「同步结果」页签',
    getEl('pane-dev').hidden === false && getEl('pane-health').hidden === true,
    '初始态应为 pane-dev 可见、pane-health 隐藏');
  getEl('tab-health').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20s 点「局域网检查」切到健康列表',
    getEl('pane-dev').hidden === true && getEl('pane-health').hidden === false
      && getEl('tab-health').getAttribute('aria-selected') === 'true',
    'pane-dev 应隐藏、pane-health 应可见');
  getEl('tab-dev').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20t 点回「同步结果」切回设备列表',
    getEl('pane-dev').hidden === false && getEl('pane-health').hidden === true
      && getEl('tab-dev').getAttribute('aria-selected') === 'true',
    '往返切换后应恢复初始态');

  // —— 展开类 ——
  getEl('btn-add').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20f 「添加账号」展开登录表单', getEl('login-form').hidden === false, '点了没反应');

  getEl('btn-cancel-login').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20g 「取消」收起登录表单', getEl('login-form').hidden === true, '点了没反应');

  const pollBefore = getEl('sw-polling').getAttribute('aria-checked');
  getEl('sw-polling').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  const pollAfter = getEl('sw-polling').getAttribute('aria-checked');
  ok('U20h 「开启同步」开关可切换', pollBefore !== pollAfter && pollAfter !== null,
    `点击前后都是 ${pollBefore} —— 开关没接上或没换状态`);

  // —— 添加任务（即时保存：直接发 account_settings）——
  getEl('f-sync-account').value = 'acc_demo';
  getEl('f-sync-household').value = 'hh-1';
  getEl('f-interval-sel').value = '5';
  const addOps = await clickAndCollect('btn-add-sync-ok', '添加任务');
  const addFrame = posted.find((m) => m.op === 'plugin.tool.call' && m.payload?.tool === 'account_settings');
  ok('U20i 「添加任务」发出 account_settings', addOps.includes('plugin.tool.call') && !!addFrame,
    `实际：${addOps.join(',') || '（无帧）'}`);
  ok('U20j account_settings 带 account_id 与家庭',
    addFrame && addFrame.payload?.account_id === 'acc_demo' && addFrame.payload?.args?.household_id === 'hh-1',
    addFrame ? `payload=${JSON.stringify(addFrame.payload).slice(0, 160)}` : '没有发出 account_settings');

  // —— 确认框类（本轮修的正是这一类）——
  const syncDelBtn = deepFind(getEl('sync-table'), (n) => n.textContent === '删除');
  ok('U20k 同步任务行内有「删除」按钮', !!syncDelBtn, '找不到行内删除按钮');
  if (syncDelBtn) {
    syncDelBtn.fire('click');
    await new Promise((r) => setTimeout(r, 30));
    ok('U20l 「删除」（同步条目）弹出**页面内**确认框', hasConfirm(),
      '没有确认框 = 又退回了 window.confirm（沙箱下永远返回 false，按钮会哑）');
    const cf = bodyEl.children.find((c) => c.className === 'confirm');
    if (cf) {
      const cancel = cf.children[0].children[1].children.find((b) => b.textContent === '取消');
      cancel.fire('click');
      await new Promise((r) => setTimeout(r, 30));
      ok('U20m 确认框「取消」不产生改动', !hasConfirm(), '确认框没关掉');
    }
  }

  // 账号行内的「解绑」要弹确认框，确认后真的发 account_delete（带 account_id）
  const unbindBtn = deepFind(getEl('accounts'), (n) => n.textContent === '解绑');
  ok('U20n 账号行内有「解绑」', !!unbindBtn, '找不到');
  if (unbindBtn) {
    unbindBtn.fire('click');
    await new Promise((r) => setTimeout(r, 30));
    ok('U20o 「解绑」弹出页面内确认框', hasConfirm(), '没有确认框');
    const cf2 = bodyEl.children.find((c) => c.className === 'confirm');
    if (cf2) {
      const n = posted.length;
      cf2.children[0].children[1].children.find((b) => b.textContent === '解绑').fire('click');
      await new Promise((r) => setTimeout(r, 40));
      const delFrame = posted.slice(n).find((m) => m.op === 'plugin.tool.call' && m.payload?.tool === 'account_delete');
      // ⚠ account_id 在 **payload** 里：rpc(op, payload) 的第二个参数整体成为帧的 payload。
      const delAccountID = delFrame && delFrame.payload && delFrame.payload.account_id;
      ok('U20p 确认后发出 account_delete 且带 account_id',
        delAccountID === 'acc_demo',
        delFrame ? `payload.account_id=${delAccountID}` : '没有发出 account_delete');
    }
  }

  // 「退出当前登录会话」：无会话时必须如实提示（不是无声无息）
  const nBefore = posted.length;
  getEl('btn-logout-session').fire('click');
  await new Promise((r) => setTimeout(r, 30));
  ok('U20q 「退出当前登录会话」有反应（提示或确认框）',
    hasConfirm() || collectText(getEl('banner')).length > 0 || posted.length > nBefore,
    '点了什么都不发生');
}
void collectText; void deepFindAll;

console.log(failed === 0 ? '\nOK  插件界面握手闸门全部通过' : `\nFAIL ${failed} 条未通过`);
process.exit(failed === 0 ? 0 : 1);
