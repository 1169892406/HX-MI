/**
 * 插件自带界面的握手闸门（2026-11-09）。
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
//
// # 为什么这条必须单独断言
//
// 前面的行为级断言是直接 `new Function(app.js)` 跑的 —— 它能证明"脚本本身没问题"，
// 但**证明不了页面会加载它**。重写 index.html 时漏掉 `<script src="app.js">`，
// 页面照样"看起来正常"：HTML/CSS 都在、控制台一条错误都没有，而脚本从没运行 ——
// 表现是账号列表永远停在「读取中…」、返回按钮点不动、同步区全空。
// 用户实测就是这个形态；我这个闸门当时全绿。
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
 *
 * 假元素默认 hidden=false，而 index.html 里带 `hidden` 属性的元素初始就是隐藏的。
 * 不还原这一点，"点了展开/收起"就会被判反 —— 闸门第一版就是这么假红的。
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
 * 应答 RPC：**这一步是必需的**。
 *
 * 2026-11-10 的教训：本闸门此前只发 connect + host-ready 就收工，全部通过；
 * 而线上真实形态是「账号行渲染出来了、同步列表整块空白，并报
 * Cannot read properties of undefined」—— 一个 renderSyncRows 里的
 * `var` 提升顺序错误。那条路径**只在真的有 RPC 应答时才会跑到**。
 * 只验"帧发得出去"，等于把最常出事的那一段（拿数据 → 渲染）整个漏掉。
 */
const ACCOUNTS_OK = {
  accounts: [{
    account_id: 'acc_demo', masked_account: '1***@qq.com', region: 'cn',
    household_id: 'hh-1', state: 'ready', revision: 1,
    // ⚠ 真实形状是**嵌套**的（account_store.go 的 account.Sync 是结构体）。
    // 夹具必须照真实形状给：扁平夹具会让"读错字段"的缺陷假绿。
    sync: { enabled: true, interval_seconds: 300 },
  }],
  account_names: { acc_demo: '1169892406@qq.com' },
};
const ACCOUNT_VIEW = {
  state: 'no_session', device_count: 0,
  sync: { enabled: true, running: false, devices: 0, controllable: 0, uncontrollable: 0, device_list: [] },
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

/* ══ 渲染路径（2026-11-10 补）═════════════════════════════════════════
 *
 * 这一组是补课：前面的断言只证明"帧发得出去"，而线上出的是"数据回得来但渲染炸"。
 * 判据直接看桩 DOM 的结果 —— 账号区不得出现失败文案、同步列表要有行。
 */
console.log('== U19 回来的数据真的能渲染');
{
  const accountsText = collectText(getEl('accounts'));
  ok('U19a 账号区有内容', accountsText.length > 0,
    '空 = renderAccounts 没跑到（或第一个分支就 return）');
  ok('U19b 账号区没有失败文案', !accountsText.includes('失败'), `实际：${accountsText.slice(0, 120)}`);
  ok('U19c 账号区渲染出了行', getEl('accounts').children.length >= 1,
    `子节点 ${getEl('accounts').children.length}`);

  const syncRows = getEl('sync-rows').children.length;
  ok('U19d 同步列表按"已绑定家庭的账号"渲染出行', syncRows >= 1,
    `子节点 ${syncRows} —— 夹具里的账号有 household_id，应当出现一行；` +
    '为 0 时最常见的成因是渲染中途抛异常（被 loadAccounts 的 catch 吞掉，只留一句失败文案）');
  const syncText = collectText(getEl('sync-rows'));
  ok('U19e 行内显示该条周期（每 N 分钟）', syncText.includes('每'), `实际：${syncText.slice(0, 120)}`);
  ok('U19f 行内自动同步状态取自**嵌套** sync.enabled', syncText.includes('开启'),
    '夹具给的是嵌套形状；显示"关闭"说明还在按扁平字段读（那个字段不存在）');

  ok('U19g 第 4 格是"离线设备"', collectText(getEl('res-tiles')).includes('离线设备'),
    `实际：${collectText(getEl('res-tiles')).slice(0, 120)}`);
}

/* ══ U20 每个按钮点了都得有反应（2026-11-10 补）══════════════════════════
 *
 * 起因：用户报「点击删除无效」。真身不是删除逻辑错，而是插件界面跑在
 * `sandbox="allow-scripts"` 的 iframe 里 —— **没有 allow-modals**，
 * `window.confirm()` 不弹窗、直接返回 false，于是三处"先确认再执行"的按钮
 * 全哑，且**没有任何报错**（代码逻辑上等于"用户点了取消"）。
 *
 * 这一组做两件事：
 *   ① 静态：源码（剥注释后）里不得再出现 window.confirm / window.alert / window.prompt；
 *   ② 行为：逐个点一遍并断言效果 —— 发帧 / 展开表单 / 出现确认框。
 * 光有 ① 会漏"按钮根本没绑上"（alpha.58 的 script 标签事故），
 * 光有 ② 会漏"某条分支在别的条件下才哑"。两条一起才盖得住。
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

  const fired = [];
  const before = posted.length;
  /** 记录点击期间新发出的 op。 */
  const clickAndCollect = async (id, label) => {
    const n = posted.length;
    getEl(id).fire('click');
    await new Promise((r) => setTimeout(r, 40));
    const ops = posted.slice(n).filter((m) => m.op).map((m) => m.op);
    fired.push({ label, ops });
    return ops;
  };
  const hasConfirm = () => bodyEl.children.filter((c) => c.className === 'confirm').length > 0;

  // —— 发帧类 ——
  const backOps = await clickAndCollect('btn-back', '返回插件列表');
  ok('U20b 「返回插件列表」发出 plugin.ui.close',
    backOps.includes('plugin.ui.close'), `实际：${backOps.join(',') || '（无帧）'}`);

  const reloadOps = await clickAndCollect('btn-reload-accounts', '刷新');
  ok('U20c 「刷新」重新取账号', reloadOps.includes('plugin.tool.call'), `实际：${reloadOps.join(',') || '（无帧）'}`);

  const diagOps = await clickAndCollect('btn-diag', '诊断·读取');
  ok('U20d 「诊断·读取」发出请求', diagOps.length > 0, `实际：${diagOps.join(',') || '（无帧）'}`);

  const catOps = await clickAndCollect('btn-catalog-status', '设备目录·查看');
  ok('U20e 「设备目录·查看」发出请求', catOps.includes('plugin.tool.call'), `实际：${catOps.join(',') || '（无帧）'}`);
  const catROps = await clickAndCollect('btn-catalog-refresh', '设备目录·刷新目录');
  ok('U20f 「设备目录·刷新目录」发出请求', catROps.includes('plugin.tool.call'), `实际：${catROps.join(',') || '（无帧）'}`);

  const lanOps = await clickAndCollect('btn-lan-health', '局域网·立即探测');
  ok('U20g 「局域网·立即探测」发出请求', lanOps.includes('plugin.tool.call'), `实际：${lanOps.join(',') || '（无帧）'}`);

  // —— 展开类（不发帧，但要看到状态变化）——
  getEl('btn-add').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20h 「添加账号」展开登录表单', getEl('login-form').hidden === false, '点了没反应');

  getEl('btn-cancel-login').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20i 「取消」收起登录表单', getEl('login-form').hidden === true, '点了没反应');

  getEl('btn-add-sync').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20j 「＋ 添加同步」展开添加表单', getEl('addsync-form').hidden === false, '点了没反应');

  getEl('btn-add-sync-cancel').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  ok('U20k 「取消」（添加同步）收起表单', getEl('addsync-form').hidden === true, '点了没反应');

  const pollBefore = getEl('sw-polling').getAttribute('aria-checked');
  getEl('sw-polling').fire('click');
  await new Promise((r) => setTimeout(r, 20));
  const pollAfter = getEl('sw-polling').getAttribute('aria-checked');
  ok('U20l 「自动同步」开关可切换', pollBefore !== pollAfter && pollAfter !== null,
    `点击前后都是 ${pollBefore} —— 开关没接上或没换状态`);

  // —— 确认框类（本轮修的正是这一类）——
  const syncDel = getEl('sync-rows').children[0].children.find((c) => c.className === 'srow__o');
  const syncDelBtn = syncDel && syncDel.children.find((c) => c.textContent === '删除');
  ok('U20m 同步条目行内有「删除」按钮', !!syncDelBtn, '找不到行内删除按钮');
  if (syncDelBtn) {
    syncDelBtn.fire('click');
    await new Promise((r) => setTimeout(r, 30));
    ok('U20n 「删除」（同步条目）弹出**页面内**确认框', hasConfirm(),
      '没有确认框 = 又退回了 window.confirm（沙箱下永远返回 false，按钮会哑）');
    // 取消后不应改动草稿
    const cf = bodyEl.children.find((c) => c.className === 'confirm');
    if (cf) {
      const cancel = cf.children[0].children[1].children.find((b) => b.textContent === '取消');
      cancel.fire('click');
      await new Promise((r) => setTimeout(r, 30));
      ok('U20o 确认框「取消」不产生改动', !hasConfirm(), '确认框没关掉');
    }
  }

  // 账号行内的「退出登录」/「删除」也要弹确认框（同一处坑）
  const acctRow = getEl('accounts').children.find((c) => c.className === 'acct');
  const logoutBtn = acctRow && acctRow.children.find((c) => c.textContent === '退出登录');
  ok('U20p 账号行内有「退出登录」', !!logoutBtn, '找不到');
  if (logoutBtn) {
    logoutBtn.fire('click');
    await new Promise((r) => setTimeout(r, 30));
    ok('U20q 「退出登录」有反应（弹确认框或如实提示）',
      hasConfirm() || collectText(getEl('banner')).length > 0,
      '点了什么都不发生 —— 这正是 sandbox 下 window.confirm 的表现');
    const cf2 = bodyEl.children.find((c) => c.className === 'confirm');
    if (cf2) cf2.children[0].children[1].children.find((b) => b.textContent === '取消').fire('click');
  }

  const delAcctBtn = acctRow && acctRow.children.find((c) => c.textContent === '删除');
  if (delAcctBtn) {
    delAcctBtn.fire('click');
    await new Promise((r) => setTimeout(r, 30));
    ok('U20r 「删除账号」弹出页面内确认框', hasConfirm(), '没有确认框');
    const cf3 = bodyEl.children.find((c) => c.className === 'confirm');
    if (cf3) {
      // 顺带验证「确认」真的会发 account_delete（带 account_id）
      const n = posted.length;
      cf3.children[0].children[1].children.find((b) => b.textContent === '删除').fire('click');
      await new Promise((r) => setTimeout(r, 40));
      const delFrame = posted.slice(n).find((m) => m.op === 'plugin.tool.call' && m.payload.tool === 'account_delete');
      // ⚠ account_id 在 **payload** 里：rpc(op, payload) 的第二个参数整体成为帧的 payload，
      // 平台侧 readToolCallPayload 也正是从 payload 读 account_id。
      const delAccountID = delFrame && delFrame.payload && delFrame.payload.account_id;
      ok('U20s 确认后发出 account_delete 且带 account_id',
        delAccountID === 'acc_demo',
        delFrame ? `payload.account_id=${delAccountID}` : '没有发出 account_delete');
    }
  }
  void before; void fired;
}

console.log(failed === 0 ? '\nOK  插件界面握手闸门全部通过' : `\nFAIL ${failed} 条未通过`);
process.exit(failed === 0 ? 0 : 1);
