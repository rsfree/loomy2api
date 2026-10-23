'use strict';
/* Loomy2API 控制台前端。
 *
 * 结构、组件与交互语义完整复刻蓝本 workbuddy2api-panel（账号池 / 用量 / 积分构成 /
 * 模型 / 配置 / 日志六视图 + trange 时间控件 + SVG 用量图 + 模型锁池 + 积分账本 +
 * 请求记录 + 错误汇总 + 备注 + 主题三态），数据端点全部接 Loomy 真实上游：
 *   - 账号运维：登录 / 签到（first-login 幂等）/ 余额 / 暂停选号 / 解冻 / 禁用 / 移除；
 *   - 积分口径：每日赠送（登录后刷新 5000、次日周期到期）+ 长期积分（不到期），
 *     扣费顺序（活动/兑换码 → 长期）由上游执行，面板只做拆分镜像；
 *   - 蓝本专有功能（OAuth 浏览器登录、成长任务、旅行、活跃上报、开学季券码）
 *     在 Loomy 抓包核对后确认上游无对应端点，不搬空壳。
 *
 * 蓝本的踩坑注释一并移植并保留出处（TDZ、CSP、SVG title、innerHTML 转义等）。 */

/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'loomy2api.key', LS_THEME = 'loomy2api.theme';
/* 账号备注的来源是后端（overview 每行的 remark 字段），这里只放内存索引。 */
let remarksCache = null;
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true;
let refTimer = null;
let smsMsgID = '';
/* 视图级筛选状态（模块级声明放文件顶部，避开顶层 go() 早于声明执行的 TDZ）。 */
let mdAll = [];
let mdFilter = { q: '', cap: '', effort: '', promo: '', sort: 'default' };
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
/* 账号池的筛选/分页（121 个号一屏放不下）：q=关键词，state=状态桶，
   sort=排序，page/size=分页。渲染层永远从 overviewData 重新派生，不缓存行。 */
let accFilter = { q: '', state: '', sort: 'default', page: 1, size: 20 };
/* 用量明细与积分扣除的分页互相独立（维度不同、行数不同）。 */
let usDimFilter = { q: '', page: 1, size: 20 };
let usCreditFilter = { page: 1, size: 20 };

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 三态翻转（跟随系统 → 浅色 → 深色），点击总是切到下一个明确态。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = '当前：' + ({ auto: '跟随系统', light: '浅色', dark: '深色' }[theme]) + '（点击切换）';
}
$('btnTheme').onclick = () => {
  theme = theme === 'auto' ? 'light' : theme === 'light' ? 'dark' : 'auto';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 手机抽屉导航（≤760px；桌面按钮隐藏、类名无副作用） ─────────────── */
const navEl = document.querySelector('.nav'), navScrim = $('navScrim'), btnNav = $('btnNav');
function navSet(open) {
  if (!navEl || !navScrim) return;
  navEl.classList.toggle('open', open);
  navScrim.classList.toggle('on', open);
  document.body.classList.toggle('nav-open', open);
  if (btnNav) btnNav.setAttribute('aria-expanded', String(open));
}
if (btnNav && navScrim) {
  btnNav.onclick = () => navSet(!navEl.classList.contains('open'));
  navScrim.onclick = () => navSet(false);
}
addEventListener('keydown', e => { if (e.key === 'Escape') navSet(false); });

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
  // status 挂到错误上：调用方靠它区分「可重试的抖动」与「重试无意义的终态」。
  if (r.status === 401) { openKey(); const e = new Error('密钥无效或未填写'); e.status = 401; throw e; }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) { const e = new Error(d.error || ('HTTP ' + r.status)); e.status = r.status; throw e; }
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso) return '—';
  const t = new Date(iso).getTime();
  if (isNaN(t)) return '—';
  const sec = Math.max(0, (Date.now() - t) / 1000);
  if (sec < 60) return '刚刚';
  if (sec < 3600) return Math.floor(sec / 60) + ' 分钟前';
  if (sec < 86400) return Math.floor(sec / 3600) + ' 小时前';
  return Math.floor(sec / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(Number(sec || 0)));
  if (!sec) return '0s';
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return (d ? d + 'd' : '') + (h ? h + 'h' : '') + (!d ? m + 'm' : '');
}

/* ── 时间范围控件（用量 / 请求记录共用）────────────────────────────────
   为什么区间一律由前端算好再发：
     - 「今天」必须是**浏览器本地时区**的 00:00 起。服务端时区未必与浏览器一致
       （容器常挂 TZ=Asia/Shanghai，而浏览器可能在任何时区），让服务端算"今天"
       会在跨时区时切错日子。
     - 「自定义」本来就是用户挑的具体时刻，没有任何服务端推导空间。
   滚动预设（近 N 小时/天）则保留 hours 参数：服务端整点对齐的滚动窗口与旧
   行为逐位一致，前端自己减 N 小时会多算/少算一个边界桶。 */
const TRANGE_PRESETS = [
  ['today', '今天'],
  ['24', '近 24 小时'],
  ['72', '近 3 天'],
  ['168', '近 7 天'],
  ['720', '近 30 天'],
  ['0', '全部历史'],
  ['custom', '自定义…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

function dtLocalValue(d) {
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
    p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
  if (!s) return null;
  const d = new Date(s);
  return isNaN(d.getTime()) ? null : d;
}
function trangeMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}
function trangeState(id) {
  if (!trangeStates.has(id)) {
    trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
  }
  return trangeStates.get(id);
}
// trangeRender 画出控件骨架（幂等：重复调用会保留当前状态）。
function trangeRender(id) {
  const host = $(id);
  if (!host) return;
  const st = trangeState(id);
  const custom = st.preset === 'custom';
  host.innerHTML =
    '<select aria-label="时间范围">' +
    TRANGE_PRESETS.map(([v, label]) =>
      '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
    '</select>' +
    '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
    '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="起始时间">' +
    '<span class="tr-sep">→</span>' +
    '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="结束时间">' +
    '</span>';
  const preset = host.querySelector('select');
  if (preset) preset.onchange = () => {
    st.preset = preset.value;
    // 从别的预设切到自定义时，把区间重置为"今天 00:00 → 现在"，
    // 免得用户上次留下的半年区间被无声沿用。
    if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
    trangeRender(id);
    trangeEmit(id);
  };
  const fromEl = host.querySelector('.tr-from');
  const toEl = host.querySelector('.tr-to');
  const readCustom = () => {
    st.from = dtLocalParse(fromEl.value);
    st.to = dtLocalParse(toEl.value);
    // 起止颠倒就地标红（不静默纠正：用户可能正输到一半）。
    const bad = st.from && st.to && st.from > st.to;
    fromEl.classList.toggle('tr-bad', !!bad);
    toEl.classList.toggle('tr-bad', !!bad);
    if (bad) return;
    trangeEmit(id);
  };
  if (fromEl) fromEl.onchange = readCustom;
  if (toEl) toEl.onchange = readCustom;
}
const trangeHandlers = new Map();
// trangeBind 渲染控件并登记变化回调。**不**在绑定时触发回调：各视图的首次加载
// 由 go() 统一驱动，这里再触发一次会让打开页面时打两遍接口。
function trangeBind(id, onChange, preset) {
  trangeHandlers.set(id, onChange);
  if (preset) trangeState(id).preset = preset;
  trangeRender(id);
}
function trangeEmit(id) {
  const fn = trangeHandlers.get(id);
  if (fn) fn();
}
// trangeQuery 把当前选择翻译成查询参数。
//   rolling=true  → 滚动预设发 hours（服务端整点对齐），今天/自定义发 from/to
//   rolling=false → 一律发 from/to（线性日志，前端算区间更直观）。
// 「全部历史」：**用量**必须显式发 hours=0 —— 后端「不给参数」的默认是短窗口，
// 空查询会让「全部历史」静默变成 24h（蓝本 issue #121 的同族坑）。
function trangeQuery(id, rolling) {
  const st = trangeState(id);
  const q = new URLSearchParams();
  const sec = d => Math.floor(d.getTime() / 1000);
  if (st.preset === 'custom') {
    if (st.from) q.set('from', sec(st.from));
    if (st.to) q.set('to', sec(st.to));
    return q;
  }
  if (st.preset === 'today') {
    q.set('from', sec(trangeMidnight()));
    return q;
  }
  if (st.preset === '0') {
    if (rolling) q.set('hours', '0');
    return q;
  }
  if (rolling) { q.set('hours', st.preset); return q; }
  q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
  return q;
}
// trangeLabel 人读口径，用于「用量总览」右上角这类需要回显区间的位置。
function trangeLabel(id) {
  const st = trangeState(id);
  const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
  if (st.preset !== 'custom') return found ? found[1] : '';
  if (!st.from && !st.to) return '自定义';
  const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
    String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
  return f(st.from) + ' → ' + f(st.to);
}

/* ── 数字格式 ─────────────────────────────────────────────────────── */
function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtRate(v) {
  const n = Number(v || 0);
  if (!n || !Number.isFinite(n)) return '—';
  return (n >= 100 ? Math.round(n) : n.toFixed(1)) + ' tok/s';
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function trimFixed(s) {
  if (!String(s).includes('.')) return String(s);
  return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtPoints(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return v.toLocaleString('zh-CN');
}
function fmtLocalDateTime(ms) {
  if (!ms) return '—';
  return new Date(ms).toLocaleString('zh-CN', { hour12: false });
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

// 登出：api_key 是唯一凭据（服务端只做 Bearer 比对，无会话无 cookie 可失效），
// 所以"登出"= 清掉本机保存的密钥并重载——重载把内存里的定时器与已渲染数据一并清空，
// 无密钥时 start() 的首次请求 401 会自动弹回密钥门（data-lock="1"，不可绕过）。
$('btnLogout').onclick = () => {
  if (!confirm('登出将清空本机保存的密钥，需要重新输入才能进入面板。确认登出？')) return;
  localStorage.removeItem(LS_KEY);
  location.reload();
};

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', usage: '用量', packages: '积分构成', tasks: '任务中心', invites: '邀请码', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  navSet(false);
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'usage') loadUsage();
  if (v === 'packages') loadPackages();
  if (v === 'tasks') loadTasks();
  if (v === 'invites') loadInvites();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* 首次进入延到本轮脚本求值之后再 go()。
   go() 会同步触发视图数据加载，而这些函数读到的模块级变量在文件后半段才初始化——
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"，点导航进去却正常（蓝本实证坑）。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 账号池 ───────────────────────────────────────────────────────── */
function accountStatus(s, now) {
  if (s.disabled) return { cls: 'off', tag: '<span class="tag bad">已禁用</span>' };
  // 暂停选号：与禁用同属「不可选」，但照常签到/刷额度保号。
  if (s.paused) return { cls: 'off', tag: '<span class="tag warn">已暂停选号</span>' };
  const cool = Math.max(Number(s.cooldown_seconds_left || 0), Number(s.breaker_seconds_left || 0));
  if (cool > 0) {
    const bl = Number(s.breaker_seconds_left || 0);
    const kind = bl >= Number(s.cooldown_seconds_left || 0) && bl > 0 ? '熔断'
      : (s.cooldown_kind === 'hard' ? '额度冷却' : '限流冷却');
    return { cls: 'cool', tag: '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>' };
  }
  if (!s.session_valid) return { cls: 'off', tag: '<span class="tag bad">登录态失效</span>' };
  return { cls: '', tag: '<span class="tag ok">可用</span>' };
}

// modelLocksHtml 账号行内的模型级限流小框（数据是 account.model_cooldowns =
// {model: {seconds_left, reason}}）。
function modelLocksHtml(cooldowns, now) {
  const entries = Object.entries(cooldowns || {}).filter(([, v]) => v && Number(v.seconds_left) > 0);
  if (!entries.length) return '';
  return '<div class="rate-limits">' + entries.map(([model, v]) => {
    const until = now + Number(v.seconds_left) * 1000;
    const title = model + '\n模型级限流' + (v.reason ? '\n原因：' + v.reason : '') +
      '\n预计恢复：' + fmtLocalDateTime(until) + '（剩余 ' + dur(v.seconds_left) + '）';
    return '<div class="rate-limit" title="' + esc(title) + '"><b>' + esc(model) +
      '</b><span>' + esc('剩余 ' + dur(v.seconds_left)) + '</span></div>';
  }).join('') + '</div>';
}

/* ── 分页 ─────────────────────────────────────────────────────────── */
/* pageSlice 按页切片，并把越界页码拉回最后一页：筛选变窄或账号被移除后，
   原页码常常超出范围，不修正会得到一张空表。 */
function pageSlice(rows, state) {
  const size = Math.max(1, Number(state.size) || 20);
  const pages = Math.max(1, Math.ceil(rows.length / size));
  const page = Math.min(Math.max(1, Number(state.page) || 1), pages);
  state.page = page;
  return { rows: rows.slice((page - 1) * size, page * size), page, pages, size, total: rows.length };
}

/* renderPager 生成「上一页 / 页码 / 下一页」。按钮走 data-pg + 委托点击，
   不用内联 onclick（CSP script-src 'self' 会拦掉）。每次重写 onclick 而不是
   addEventListener：容器会被反复渲染，累加监听会一次点击翻多页。 */
function renderPager(elId, pg, state, rerender) {
  const el = $(elId);
  if (!el) return;
  if (pg.total <= pg.size) {
    el.innerHTML = '<span class="info">共 ' + pg.total + ' 条</span>';
    return;
  }
  const nums = [];
  for (const n of [1, pg.pages, pg.page - 2, pg.page - 1, pg.page, pg.page + 1, pg.page + 2]) {
    if (n >= 1 && n <= pg.pages && !nums.includes(n)) nums.push(n);
  }
  nums.sort((a, b) => a - b);
  let html = '<span class="info">共 ' + pg.total + ' 条 · 第 ' + pg.page + ' / ' + pg.pages + ' 页</span>' +
    '<button class="xs ghost" data-pg="prev"' + (pg.page <= 1 ? ' disabled' : '') + '>上一页</button>';
  let last = 0;
  for (const n of nums) {
    if (last && n - last > 1) html += '<span class="gap">…</span>';
    html += '<button class="xs ' + (n === pg.page ? 'primary' : 'ghost') + '" data-pg="' + n + '">' + n + '</button>';
    last = n;
  }
  html += '<button class="xs ghost" data-pg="next"' + (pg.page >= pg.pages ? ' disabled' : '') + '>下一页</button>';
  el.innerHTML = html;
  el.onclick = ev => {
    const b = ev.target.closest('button[data-pg]');
    if (!b || b.disabled) return;
    const v = b.dataset.pg;
    state.page = v === 'prev' ? pg.page - 1 : v === 'next' ? pg.page + 1 : Number(v);
    rerender();
  };
}

/* accountBucket 把展示状态折成筛选项。accountStatus 的 cls 只有四种，但
   「暂停 / 禁用 / 登录态失效」在运营上是三件事，分开筛才有用。 */
function accountBucket(s) {
  if (s.disabled) return 'disabled';
  if (s.paused) return 'paused';
  if (Math.max(Number(s.cooldown_seconds_left || 0), Number(s.breaker_seconds_left || 0)) > 0) return 'cool';
  if (!s.session_valid) return 'invalid';
  return 'ok';
}
function accMatches(s, q) {
  if (!q) return true;
  const hay = [s.name, s.loginid, s.userid, remarkOf(s.name)].filter(Boolean).join(' ').toLowerCase();
  return hay.includes(q);
}
const ACC_SORTS = {
  cred: (a, b) => Number(b.available || 0) - Number(a.available || 0),
  err: (a, b) => Number(b.err_total || 0) - Number(a.err_total || 0),
  recent: (a, b) => Number(b.last_success || 0) - Number(a.last_success || 0),
  name: (a, b) => String(a.name || '').localeCompare(String(b.name || '')),
};

/* accRowHtml 单行渲染。抽出来是为了让分页只重算切片，不必改动行内逻辑。 */
function accRowHtml(s, now, maxCred) {
  const st = accountStatus(s, now);
  const err = s.last_error ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.last_error) + '</div>' : '';
  const locks = modelLocksHtml(s.model_cooldowns, now);
  const avail = Number(s.available || 0);
  const pct = Math.min(100, Math.round(avail / maxCred * 100));
  // 额度口径 tooltip（抓包核对过的三分法）：每日登录后刷新 5000，扣费先活动后长期。
  let credTip = '可用 ' + fmtPoints(avail);
  if (s.permanent != null) credTip += '\n长期积分：' + fmtPoints(s.permanent);
  if (s.daily_balance != null) credTip += '\n每日剩余：' + fmtPoints(s.daily_balance) +
    (s.daily_quota ? ' / ' + fmtPoints(s.daily_quota) : '');
  if (s.daily_cycle_date) credTip += '\n每日周期：' + s.daily_cycle_date + (s.checkin_done ? '（已签到）' : '（未签到）');
  credTip += '\n更新于：' + (s.quota_updated_at ? ago(new Date(s.quota_updated_at * 1000).toISOString()) : '未刷新');
  const tu = s.token_usage || {};
  const req = tu.request_count || 0;
  const latency = fmtMs(tu.last_latency_ms);
  const rate = fmtRate(tu.last_tokens_per_second);
  // 参考面板「用量」是累计口径（台账全历史），不是某个时间窗。
  const usageTitle = '累计：' + req + ' 次 / ' + fmtTok(tu.total_tokens) + ' tok / 均延迟 ' + latency +
    (rate === '—' ? '' : ' / ' + rate);
  const days = (s.session_days_left == null) ? '—' : s.session_days_left.toFixed(1) + 'd';
  // 出口 IP 放 title 而不是行内文字：账号列宽度固定，行内会被省略号吃掉（实测截断）。
  const egressTip = s.egress ? '\n出口: ' + s.egress : '';
  return '<tr class="' + st.cls + '" title="' + esc('账号 ' + s.name + (s.userid ? '\nuserid: ' + s.userid : '') + egressTip) + '">' +
    '<td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"><div class="nm">' + remarkName(s.name, esc(s.name || '未命名')) + '</div>' +
    '<div class="id">' + esc(s.loginid || (s.userid ? 'uid ' + s.userid : '—')) +
    (s.session_days_left == null ? '' : ' · 登录态 ' + days) + '</div></td>' +
    '<td>' + st.tag + err + locks + '</td>' +
    '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + fmtPoints(avail) +
    (s.permanent != null && s.daily_balance != null
      ? '<div class="id" style="font-size:11px">长期 ' + fmtPoints(s.permanent) + ' · 每日 ' + fmtPoints(s.daily_balance) + '</div>' : '') +
    '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
    '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
    '<td class="num">' + (s.inflight || 0) + '</td>' +
    '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
      '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
      '<span class="usage-item usage-total"><b>' + fmtTok(tu.total_tokens) + '</b><em>tok</em></span>' +
      '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
      '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
    '</span></td>' +
    '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success ? new Date(s.last_success * 1000).toISOString() : '') + '</td>' +
    '<td class="acts">' +
      '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.name) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>' +
      '<button class="xs ghost" data-a="balance" data-u="' + esc(s.name) + '">余额</button>' +
      (s.has_password ? '<button class="xs ghost" data-a="login" data-u="' + esc(s.name) + '">登录</button>' : '') +
      (s.disabled || s.in_cooldown || s.in_breaker
        ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.name) + '">解冻</button>'
        : (s.paused
          ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.name) + '">恢复选号</button>'
          : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.name) + '" title="退出选号，但照常签到 / 刷新余额">暂停选号</button>')) +
      (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.name) + '">禁用</button>') +
      '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.name) + '">移除</button>' +
    '</td></tr>';
}

function renderAccounts(list) {
  const tb = $('accBody');
  const pager = $('accPager');
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用手机号 + 密码或验证码登录一个 Loomy 账号</div></td></tr>';
    if (pager) pager.innerHTML = '';
    $('accFilterNote').textContent = '';
    return;
  }
  const now = Date.now();
  const q = accFilter.q.trim().toLowerCase();
  const filtered = list.filter(s => {
    if (!accMatches(s, q)) return false;
    switch (accFilter.state) {
      case 'ok': return accountBucket(s) === 'ok';
      case 'cool': return accountBucket(s) === 'cool';
      case 'paused': return accountBucket(s) === 'paused';
      case 'disabled': return accountBucket(s) === 'disabled';
      case 'invalid': return accountBucket(s) === 'invalid';
      case 'err': return Number(s.err_total || 0) > 0;
      default: return true;
    }
  });
  const sorter = ACC_SORTS[accFilter.sort];
  if (sorter) filtered.sort(sorter);
  const pg = pageSlice(filtered, accFilter);
  // 进度条基准用「全池最高」而不是当前页最高：翻页时同一账号的条长不该跳变。
  const maxCred = Math.max(1, ...list.map(s => Number(s.available || 0)));
  tb.innerHTML = pg.rows.length
    ? pg.rows.map(s => accRowHtml(s, now, maxCred)).join('')
    : '<tr><td colspan="9"><div class="empty">没有符合筛选条件的账号</div></td></tr>';
  $('accFilterNote').textContent = filtered.length === list.length
    ? list.length + ' 个账号'
    : '筛选 ' + filtered.length + ' / ' + list.length;
  renderPager('accPager', pg, accFilter, () => renderAccounts(list));
}

// 模型锁池：账号池回答「哪些号不能用」，这张表回答「哪些模型不能用、还要锁多久」。
// 数据来自 /overview 的 model_locks（pool.ModelLockView，Loomy 单域版），无锁时为 null。
const ML_STATE = { locked: ['bad', '全池锁定'], starved: ['warn', '此刻无号'], partial: ['ok', '部分限流'] };
function renderModelLocks(rows) {
  const tb = $('mlBody');
  const list = Array.isArray(rows) ? rows : [];
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7" style="color:var(--ink-3)">没有模型处于锁定/限流</td></tr>';
    $('mlNote').textContent = '';
    return;
  }
  const now = Date.now();
  tb.innerHTML = list.map(r => {
    const [cls, label] = ML_STATE[r.state] || ['mute', r.state];
    return '<tr><td class="num">' + esc(r.model) + '</td>' +
      '<td><span class="tag ' + cls + '">' + esc(label) + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + esc(r.unlock_at ? fmtLocalDateTime(r.unlock_at * 1000) + '（' + dur((r.unlock_at * 1000 - now) / 1000) + '）' : '—') + '</td>' +
      '<td class="num">' + esc(r.fully_unlock_at ? fmtLocalDateTime(r.fully_unlock_at * 1000) : '—') + '</td>' +
      '<td class="clip" title="' + esc(r.reason || '') + '">' + esc(r.reason || '—') + '</td></tr>';
  }).join('');
  $('mlNote').textContent = list.length + ' 个模型受限';
}

// renderDeadModels 已下架模型（上游 404 台账）。与「模型锁池」互补：
// 锁池是限流（等一会儿会好），这里是模型本身没了（换号、等冷却都不会好）。
function renderDeadModels(rows) {
  const tb = $('dmBody');
  const list = Array.isArray(rows) ? rows : [];
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="6" style="color:var(--ink-3)">没有模型被判失效</td></tr>';
    $('dmNote').textContent = '';
    return;
  }
  const now = Date.now();
  tb.innerHTML = list.map(r => {
    const left = (Number(r.until) * 1000 - now) / 1000;
    return '<tr><td class="num">' + esc(r.model) + '</td>' +
      '<td class="num">' + (r.hits || 0) + '</td>' +
      '<td class="clip" title="' + esc(r.reason || '') + '">' + esc(r.reason || '—') + '</td>' +
      '<td class="num">' + esc(r.seen_at ? fmtLocalDateTime(r.seen_at * 1000) : '—') + '</td>' +
      '<td class="num">' + esc(r.until ? fmtLocalDateTime(r.until * 1000) + '（' + dur(left) + '）' : '—') + '</td>' +
      '<td><button class="xs" data-dm="' + esc(r.model) + '">解除</button></td></tr>';
  }).join('');
  $('dmNote').textContent = list.length + ' 个模型不可用';
  tb.querySelectorAll('button[data-dm]').forEach(btn => {
    btn.onclick = async () => {
      btn.disabled = true;
      try {
        await api('dead_models/' + encodeURIComponent(btn.getAttribute('data-dm')) + '/clear', { method: 'POST' });
        toast('已解除 ' + btn.getAttribute('data-dm'), 'ok');
      } catch (e) { toast(e.message, 'err'); }
      finally { loadOverview(true); }
    };
  });
}

// renderExpiry 积分到期提醒（参考面板同区块）：Loomy 只有「每日赠送」会随周期失效，
// 长期积分不到期。到期日 = daily_cycle_date 的次日 00:00；同一天的多个账号合并成一行。
function renderExpiry(accounts) {
  const rows = [];
  for (const a of (accounts || [])) {
    const bal = Number(a.daily_balance || 0);
    if (!bal || !a.daily_cycle_date) continue;
    const cyc = new Date(a.daily_cycle_date + 'T00:00:00');
    if (isNaN(cyc.getTime())) continue;
    const exp = new Date(cyc.getTime() + 86400000);
    const days = Math.max(0, Math.ceil((exp.getTime() - Date.now()) / 86400000));
    rows.push({ name: a.name, exp, days, remain: bal });
  }
  rows.sort((x, y) => x.exp - y.exp);
  const tb = $('expBody');
  if (!tb) return;
  tb.innerHTML = !rows.length
    ? '<tr><td colspan="5"><div class="empty">暂无未来到期批次（长期积分不到期，每日额度为 0 时不入表）</div></td></tr>'
    : rows.map(r => '<tr><td class="who"><div class="nm">' + esc(r.name) + '</div></td>' +
        '<td>' + r.exp.toLocaleDateString('zh-CN') + '</td>' +
        '<td class="num">' + (r.days === 0 ? '今天' : r.days === 1 ? '明天' : r.days + ' 天') + '</td>' +
        '<td class="num">' + fmtPoints(r.remain) + '</td>' +
        '<td class="num">' + fmtPoints(Math.ceil(r.remain / Math.max(1, r.days))) + '</td></tr>').join('');
  const total = rows.reduce((s, r) => s + r.remain, 0);
  if ($('expNote')) $('expNote').textContent = rows.length ? rows.length + ' 个账号 · 合计 ' + fmtPoints(total) : '—';
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    // 备注索引跟着 overview 重建：它是后端下发字段的派生视图，不是本地真值。
    remarksCache = null;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    // 暂停选号单列（蓝本 issue #125）：它只关选号、照常保号，与禁用是两种状态。
    $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const availSum = (d.accounts || []).reduce((a, s) => a + (Number(s.available) || 0), 0);
    $('sCredits').textContent = fmtPoints(availSum);
    $('sSticky').textContent = d.sticky_sessions == null ? '-' : d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    renderExpiry(d.accounts);
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    const up = Math.floor(d.uptime_sec || 0);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    $('accNote').textContent = d.total ? d.total + ' 个账号' : '';
    // 出口分布：把「均匀不均匀」直接写在账号池标题行，不用去数表格。
    const eg = d.egress || {};
    $('egressNote').textContent = eg.enabled
      ? '出口 ' + eg.ips + ' 个 IP · 每 IP ' + eg.min_per_ip + '-' + eg.max_per_ip + ' 个账号'
      : '';
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    renderDeadModels(d.dead_models);
  } catch (e) { if (!quiet) toast(e.message, 'err'); throw e; }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，也停止签到/刷额度（需手动启用）。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      const c = r.checkin || {};
      toast(c.already_processed
        ? '本周期已签到（每日 ' + fmtPoints((c.daily_quota || 0) - (c.daily_consumed || 0)) + '/' + fmtPoints(c.daily_quota) + '，可用 ' + fmtPoints(c.current_balance) + '）'
        : '签到完成：已领每日 ' + fmtPoints(c.daily_quota) + '（可用 ' + fmtPoints(c.current_balance) + '）', 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      const acc = r.account || {};
      toast('余额已更新：' + fmtPoints(acc.available) + (acc.permanent != null ? '（长期 ' + fmtPoints(acc.permanent) + ' · 每日 ' + fmtPoints(acc.daily_balance) + '）' : ''), 'ok');
    } else if (a === 'login') {
      await api('accounts/' + encodeURIComponent(u) + '/login', { method: 'POST' });
      toast(u + ' 登录成功（登录成功后已自动签到）', 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 刷额度照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'remove') {
      await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast('已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally {
    b.disabled = false;
    invalidatePackages();
    loadOverview(true);
  }
});

/* 账号池筛选：输入即时过滤（121 行量级，直接重渲染比防抖更简单也更跟手）。 */
function rerenderAccounts() {
  renderAccounts((overviewData && overviewData.accounts) || []);
}
$('accQ').addEventListener('input', () => { accFilter.q = $('accQ').value; accFilter.page = 1; rerenderAccounts(); });
$('accState').onchange = () => { accFilter.state = $('accState').value; accFilter.page = 1; rerenderAccounts(); };
$('accSort').onchange = () => { accFilter.sort = $('accSort').value; accFilter.page = 1; rerenderAccounts(); };
$('accSize').onchange = () => { accFilter.size = Number($('accSize').value) || 20; accFilter.page = 1; rerenderAccounts(); };
$('accReset').onclick = () => {
  accFilter = { q: '', state: '', sort: 'default', page: 1, size: accFilter.size };
  $('accQ').value = ''; $('accState').value = ''; $('accSort').value = 'default';
  rerenderAccounts();
};

$('btnCheckinAll').onclick = async () => {
  const btn = $('btnCheckinAll');
  btn.disabled = true;
  try {
    const d = await api('checkin_all', { method: 'POST' });
    const rs = d.results || [];
    const ok = rs.filter(r => r.ok).length;
    const granted = rs.filter(r => r.granted).length;
    toast('全部签到完成：成功 ' + ok + '/' + rs.length + (granted ? '，其中 ' + granted + ' 个新领额度' : '（均为本周期已领）'), 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; loadOverview(true); }
};
$('btnOnboardingAll').onclick = async () => {
  const btn = $('btnOnboardingAll');
  btn.disabled = true; btn.textContent = '领取中…';
  try {
    const d = await api('onboarding_all', { method: 'POST' });
    const rs = d.results || [];
    const fail = rs.filter(r => !r.ok);
    const pts = Number(d.points || 0);
    toast(pts > 0
      ? '新手任务：' + rs.length + ' 个账号共领 ' + fmtPoints(pts) + ' 积分'
      : '新手任务：无可领取（' + rs.length + ' 个账号均已完成）',
      pts > 0 ? 'ok' : 'ok');
    if (fail.length) toast('其中 ' + fail.length + ' 个失败 — ' + fail[0].error, 'err');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '领新手任务'; loadOverview(true); }
};
$('btnRefreshAll').onclick = async () => {
  const btn = $('btnRefreshAll');
  btn.disabled = true; btn.textContent = '刷新中…';
  try {
    const d = await api('balance_all', { method: 'POST' });
    const rs = d.results || [];
    toast('额度刷新完成：' + rs.filter(r => r.ok).length + '/' + rs.length, 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部刷新额度'; loadOverview(true); }
};

/* ── 任务中心 ─────────────────────────────────────────────────────── */
/* 只在进入视图时查询上游（每号一个 GET）；「刷新状态」显式重查。
   矩阵是只读快照，领取仍走账号级 / 全量既有端点——不在这里复制第二套领取逻辑，
   否则幂等判据（alreadyCompleted）会出现两个实现，早晚对不上。 */
async function loadTasks() {
  const body = $('tkBody');
  if (!body) return;
  body.innerHTML = '<tr><td colspan="5"><div class="tk-empty"><div class="big">正在查询各账号任务状态…</div>'
    + '<div>每号一次 GET /onboarding/tasks · 并发 4 路 · 单号 20s 超时</div></div></td></tr>';
  try {
    renderTasks(await api('onboarding'));
  } catch (e) {
    body.innerHTML = '<tr><td colspan="5"><div class="tk-empty"><div class="big">读取失败</div>'
      + '<div>' + esc(e.message) + '</div></div></td></tr>';
  }
}

function renderTasks(d) {
  const tasks = d.tasks || [], rows = d.accounts || [], sum = d.summary || {};
  const totalPts = Number(d.total || 0) * Number(sum.queryable || 0);
  $('tkAccounts').textContent = sum.accounts || 0;
  $('tkQueryable').textContent = sum.queryable || 0;
  $('tkDone').textContent = (sum.tasks_done || 0) + ' / ' + (sum.tasks_total || 0);
  $('tkEarned').textContent = fmtPoints(sum.earned || 0);
  $('tkClaimable').textContent = fmtPoints(sum.claimable || 0);
  $('tkTotal').textContent = fmtPoints(totalPts);
  $('tkRate').textContent = (sum.tasks_total ? Math.round(sum.tasks_done / sum.tasks_total * 100) : 0) + '%';
  // 自动补领状态：新手任务原先只在登录/手动签到时领一次，单项失败会让该号
  // 永久停在 5/8、7/8；后台现在每 N 小时补一轮，这里把节奏与上次时间显示出来。
  const auto = d.auto || {};
  let tkNote = sum.queryable ? '可查询 ' + sum.queryable : '无可查询账号';
  if (auto.enabled) {
    // 紧凑写法：标题行还要放两个按钮，长文案会把它们挤到换行（实测截图）。
    tkNote += ' · 自动补领 ' + auto.interval_hours + 'h';
    if (auto.last_run) {
      const t = new Date(auto.last_run);
      tkNote += '（' + t.toLocaleTimeString('zh-CN', { hour12: false }).slice(0, 5) + '）';
    }
  }
  $('tkNote').textContent = tkNote;

  const body = $('tkBody');
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="5"><div class="tk-empty"><div class="big">还没有账号</div>'
      + '<div>先在「账号池」添加账号，登录成功会自动领一次新手任务。</div></div></td></tr>';
    return;
  }
  body.innerHTML = rows.map(r => {
    const t = r.tasks || {};
    /* 查询失败的号（禁用 / 登录态失效 / 上游报错）不能把 8 项全画成「未完成」：
       那是「状态未知」，不是「一项都没做」。两者混同会让待领积分看起来虚高。 */
    const unknown = !!r.error;
    const chips = unknown
      ? '<span class="tk-na">未查询到状态</span>'
      : tasks.map(x => {
        const done = !!t[x.key];
        return '<span class="tk-chip' + (done ? ' done' : '') + '" title="' + esc(x.title + ' · ' + x.points + ' 积分') + '">'
          + esc(x.short) + '<i>' + (done ? '✓' : '+' + x.points) + '</i></span>';
      }).join('');
    const done = Number(r.done || 0), all = tasks.length || 1;
    const prog = unknown
      ? '<div class="tk-prog"><div class="n">—</div></div>'
      : '<div class="tk-prog"><div class="n">' + done + ' / ' + all + '</div>'
        + '<div class="bar"><i style="width:' + Math.round(done / all * 100) + '%"></i></div></div>';
    let act;
    if (!unknown && r.enabled && r.session_ok && done < all) {
      act = '<button class="xs" data-tk="' + esc(r.name) + '">领取</button>';
    } else {
      const why = unknown ? (r.enabled ? '需重登' : '已禁用') : '已领完';
      act = '<span class="tag mute">' + why + '</span>';
    }
    const err = r.error
      ? '<div style="color:var(--warn);font-size:11.5px;margin-top:2px">' + esc(r.error) + '</div>' : '';
    return '<tr>'
      + '<td class="who"><div class="nm">' + esc(r.name) + '</div>'
      + (r.remark ? '<div class="id">' + esc(r.remark) + '</div>' : '') + err + '</td>'
      + '<td>' + prog + '</td>'
      + '<td class="tasks"><div class="tk-chips">' + chips + '</div></td>'
      + '<td class="num">' + (unknown ? '—' : fmtPoints(r.earned || 0)) + '</td>'
      + '<td class="c-acts"><div class="acts">' + act + '</div></td>'
      + '</tr>';
  }).join('');
}

$('btnTkReload').onclick = loadTasks;
$('btnTkAll').onclick = async () => {
  const btn = $('btnTkAll');
  btn.disabled = true; btn.textContent = '领取中…';
  try {
    const d = await api('onboarding_all', { method: 'POST' });
    const rs = d.results || [];
    const pts = Number(d.points || 0);
    toast(pts > 0
      ? '新手任务：' + rs.length + ' 个账号共领 ' + fmtPoints(pts) + ' 积分'
      : '新手任务：无可领取（' + rs.length + ' 个账号均已完成）', 'ok');
    const fail = rs.filter(r => !r.ok);
    if (fail.length) toast('其中 ' + fail.length + ' 个失败 — ' + fail[0].error, 'err');
    invalidatePackages();
  } catch (e) { toast(e.message, 'err'); }
  finally {
    btn.disabled = false; btn.textContent = '全部领取';
    loadOverview(true).catch(() => {});
    if (view === 'tasks') loadTasks();
  }
};
$('tkBody').onclick = async e => {
  const btn = e.target.closest('button[data-tk]');
  if (!btn || btn.disabled) return;
  const name = btn.dataset.tk;
  btn.disabled = true; btn.textContent = '领取中…';
  try {
    const d = await api('accounts/' + encodeURIComponent(name) + '/onboarding', { method: 'POST' });
    const on = d.onboarding || {};
    toast((on.points > 0 ? name + '：领到 ' + fmtPoints(on.points) + ' 积分' : name + '：没有可领的任务'), 'ok');
    invalidatePackages();
  } catch (err) { toast(err.message, 'err'); }
  finally {
    loadOverview(true).catch(() => {});
    if (view === 'tasks') loadTasks();
  }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }

async function loadModels(force) {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    const d = await api('models' + (force ? '?refresh=1' : ''));
    mdAll = d.models || [];
    if (!mdAll.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型（池内需要至少一个有效登录态）</div></td></tr>';
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    $('mdNote').textContent = mdAll.length + ' 个模型';
    renderModels();
  } catch (e) {
    mdAll = [];
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全，筛选与排序全部在前端完成：改条件零延迟，也不会因为调一次
   筛选就打一次上游。条件之间是 AND；每个条件为空即不参与判定。 */
function mdSearchText(m) {
  return [m.id, m.name, (m.capabilities || []).join(' '), (m.reasoning_efforts || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"spark 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.cap === 'tool' && !(m.capabilities || []).includes('function_calling') &&
      !(m.capabilities || []).includes('tool')) return false;
  // 端点维度：图像模型只能走 /v1/images/generations，对话端点会 400，
  // 所以「对话模型」筛选必须把图像模型排除掉，否则用户照列表选就会踩 400。
  if (f.cap === 'image' && !isImageModel(m)) return false;
  if (f.cap === 'chat' && isImageModel(m)) return false;
  if (f.cap === 'vision' && !(m.capabilities || []).includes('vision') &&
      !(m.capabilities || []).includes('image')) return false;
  if (f.cap === 'reasoning' && !(m.reasoning_efforts || []).length) return false;
  if (f.cap === 'default' && !m.is_default_model) return false;
  const effs = m.reasoning_efforts || [];
  if (f.effort === 'off' && !effs.includes('none')) return false;
  if (f.effort && f.effort !== 'off' && !effs.includes(f.effort)) return false;
  // 价格口径：限免 = is_free；打折 = 有倍率且 < 1（GLM x0.8 这类）；无倍率不算打折。
  if (f.promo === 'free' && !m.is_free) return false;
  if (f.promo === 'discount' && (m.is_free || !(Number(m.multiplier) > 0 && Number(m.multiplier) < 1))) return false;
  if (f.promo === 'promo' && !(m.is_free || (Number(m.multiplier) > 0 && Number(m.multiplier) < 1))) return false;
  return true;
}
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => (num(a.multiplier) || Infinity) - (num(b.multiplier) || Infinity));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}
/* mdBaseName 去掉展示名里的「(x2.0)」「(限时免费)」标注：倍率与限免各有独立列，
   名字里再重复一遍只会把行撑宽。上游没有结构化字段，只能在这里剥。 */
function mdBaseName(name) {
  return String(name || '')
    .replace(/[（(]\s*[xX×]\s*[0-9]+(?:\.[0-9]+)?\s*[）)]/g, '')
    .replace(/[（(]\s*限时免费\s*[）)]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}
/* isImageModel 上游 type=image 的模型只能走 /v1/images/generations：
   对话端点会被网关 400 拒绝（上游本身也只回 404），列表和筛选都按这个口径分。 */
function isImageModel(m) { return String(m.type || '').toLowerCase() === 'image'; }
function mdRowHtml(m) {
  const eff = (m.reasoning_efforts || []);
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">不支持思考</span>';
  const caps = [];
  for (const c of (m.capabilities || [])) {
    caps.push('<span class="tag mute">' + esc(c) + '</span>');
  }
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  // 倍率列：限免模型显示「限时免费」标签（上游倍率为 0，直接写 x0.0 反而看不出是优惠）。
  const rate = m.is_free
    ? '<span class="tag ok" title="上游标注限时免费，倍率 0">限免</span>'
    : (m.multiplier ? 'x' + Number(m.multiplier).toFixed(1) : '—');
  const defTag = m.is_default_model ? '<span class="tag ok" title="上游默认模型">默认</span>' : '';
  const typeTag = isImageModel(m)
    ? '<span class="tag mute" title="图像模型：只能 POST /v1/images/generations，对话端点会被拒绝">图像</span>'
    : '';
  const dflt = m.default_reasoning_effort
    ? '<span class="tag mute" title="上游默认思考档">' + esc(m.default_reasoning_effort) + '</span>'
    : '—';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"><div class="nm"><span class="mid">' + esc(m.id) + '</span>' + defTag + typeTag + '</div>' +
    '<div class="id">' + esc(mdBaseName(m.name) || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rate + '</td>' +
    '<td>' + dflt + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? fmtK(m.context_length) : '—') + '</td>' +
    '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td></tr>';
}
function renderModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(mdRowHtml).join('');
  }
  // 限免数单独报出来：这是「现在用哪个模型不花积分」的直接答案。
  const freeCount = mdAll.filter(m => m.is_free).length;
  const filtered = list.length !== mdAll.length;
  $('mdCount').textContent = !mdAll.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型') + (freeCount ? ' · 限免 ' + freeCount : '');
  $('mdCount').className = filtered ? 'note src-off' : 'note';
}
function resetModelFilter() {
  mdFilter = { q: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdCap').value = ''; $('mdEffort').value = '';
  $('mdPromo').value = ''; $('mdSort').value = 'default';
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = () => loadModels(true);

/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
// logLevel 日志行的颜色级别。先抹掉「空赋值」字段再判：
// `error=""`（值为空 = 这一项没有错误）里带 error 字样，裸匹配会把成功的行
// 整行标红；非空赋值（`error="task failed"`）仍然照常标红。
function logLevel(text) {
  const t = String(text == null ? '' : text).replace(/\b[a-z_]+=(?:""|'')/gi, '');
  if (/error|失败|错误|⚠️/.test(t)) return ' e';
  if (/warn|冷却|熔断|限流/.test(t)) return ' w';
  return '';
}

// loadCreditHistory 单独拉积分账本（带 25s 上限）：慢接口不阻塞日志渲染。
function loadCreditHistory() {
  const opts = (typeof AbortSignal !== 'undefined' && AbortSignal.timeout)
    ? { signal: AbortSignal.timeout(25000) } : {};
  api('credit_history?limit=' + creditLimitValue(), opts)
    .then(renderCreditHistory)
    .catch(() => {});
}

// loadLogs 拉日志视图。withCredit=false 时不拉积分账本（轮询用：账本 5 分钟
// 缓存一次足够，没必要跟着 5s 轮询刷）。
async function loadLogs(withCredit) {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const limit = $('reqLimit').value || 100;
  // 时间范围由服务端过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, requestRows, errSum] = await Promise.all([
      api('logs'),
      api('requests?' + rq.toString()).catch(() => ({ records: [] })),
      api('error_summary').catch(() => null),
    ]);
    renderRequests(requestRows.records || []);
    const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = logLevel(e.text);
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 对话 ' + (counts.chat || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : logCh === 'chat' ? '对话' : '系统') + ' ' + entries.length + ' 行';
    renderErrorSummary(errSum);
    // 积分历史要逐账号打上游（121 个号 = 200+ 次调用），单独异步拉：
    // 它慢不该把日志一起拖白（2026-10-23 线上事故：日志页永远「暂无日志」）。
    if (withCredit !== false) loadCreditHistory();
  } catch (e) { /* 概览已提示 */ }
}

// reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
// outcome 精确匹配。两者都在已拉取的条目上做（最多 2000 条），不发新请求。
function reqMatch(e, f) {
  f = f || reqFilter;
  const ok = Number(e && e.status || 0) >= 200 && Number(e && e.status || 0) < 300;
  if (f.outcome === 'ok' && !ok) return false;
  if (f.outcome === 'fail' && ok) return false;
  if (f.q) {
    const text = [e && e.ip, e && e.egress, e && e.ua, e && e.model, e && e.account, e && e.rid, e && e.error]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}
function reqOutcomeTag(e) {
  const ok = Number(e && e.status || 0) >= 200 && Number(e && e.status || 0) < 300;
  const label = ok ? '成功' : 'HTTP ' + (e && e.status || '—');
  const cls = ok ? 'ok' : 'bad';
  return '<span class="tag ' + cls + '" title="' + esc(e && e.error || '') + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}
function requestLogText(e) {
  const when = e && e.t ? new Date(e.t * 1000).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const ok = Number(e && e.status || 0) >= 200 && Number(e && e.status || 0) < 300;
  const tok = Number(e && e.prompt || 0) + Number(e && e.completion || 0);
  return [when, (e && e.status || '—') + (ok ? ' 成功' : ' 失败'), e && e.model || '—', e && e.account || '—',
    '来源 ' + (e && e.ip || '—') + ' → 出口 ' + (e && e.egress || '—'), e && e.ua || '—',
    fmtMs(e && e.latency_ms), fmtTok(tok) + ' tok',
    e && e.points ? fmtPoints(e.points) + ' 积分' : '', e && e.ttfb_ms ? 'TTFB ' + fmtMs(e.ttfb_ms) : '',
    e && e.rid || '—'].filter(Boolean).join(' | ');
}
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  tb.innerHTML = list.map(e => {
    const when = e && e.t ? new Date(e.t * 1000).toLocaleString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.ip ? e.ip : '';
    const eg = e && e.egress ? e.egress : '';
    const ua = e && e.ua ? e.ua : '';
    const rid = e && e.rid ? e.rid : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      // 来源在上、出口在下：两个方向相反的 IP 堆成一列两行，比并排两列好读。
      '<td><div class="src2">' +
      (ip ? '<span class="from" title="来源 ' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="from muted">—</span>') +
      (eg ? '<span class="via" title="出口 ' + esc(eg) + '">↳ ' + esc(eg) + '</span>'
        : '<span class="via" title="未记录出口（egress.mode=direct 或旧版本台账）">↳ —</span>') +
      '</div></td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.latency_ms) + '</td>' +
      '<td class="num">' + fmtTok(Number(e && e.prompt || 0) + Number(e && e.completion || 0)) + '</td>' +
      '<td class="num">' + (e && e.points ? fmtPoints(e.points) : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>';

  const filtered = list.length !== reqEntries.length;
  const hasSource = reqEntries.some(e => e && (e.ip || e.ua));
  const hasEgress = reqEntries.some(e => e && e.egress);
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录') + (hasEgress ? '' : ' · 出口未记录');
  $('reqCount').className = (filtered || !hasSource || !hasEgress) ? 'note src-off' : 'note';
}
function renderRequests(records) {
  reqEntries = records || [];
  // reqStats：请求指标行（本页窗口内计算，避免再打一次聚合端点）。
  const rows = reqEntries;
  const okCount = rows.filter(e => Number(e.status || 0) >= 200 && Number(e.status || 0) < 300).length;
  const tokSum = rows.reduce((n, e) => n + Number(e.prompt || 0) + Number(e.completion || 0), 0);
  const latSum = rows.reduce((n, e) => n + Number(e.latency_ms || 0), 0);
  const ptsSum = rows.reduce((n, e) => n + Number(e.points || 0), 0);
  const rate = rows.length ? (okCount / rows.length * 100) : null;
  $('reqStats').innerHTML =
    usStat(fmtTok(rows.length), '请求') +
    usStat(rate == null ? '—' : rate.toFixed(1) + '%', '成功率') +
    usStat(fmtTok(tokSum), 'Token') +
    usStat(fmtPoints(ptsSum), '积分') +
    usStat(rows.length ? fmtMs(latSum / rows.length) : '—', '平均延迟') +
    usStat(String(rows.length - okCount), '失败');
  $('reqNote').textContent = '内存台账 · 最近 ' + rows.length + ' 条';
  renderRequestTable();
}

/* 错误汇总（后端 /panel/api/error_summary 已聚合+排序）：只渲染，不在这里再算。 */
function renderErrorSummary(d) {
  const rows = (d && d.errors) || [];
  $('errBody').innerHTML = rows.length ? rows.map(r => {
    const sev = r.status >= 500 ? 'lv-e' : r.status === 429 ? 'lv-w' : '';
    return '<tr class="' + sev + '">' +
      '<td>' + esc(r.account || '—') + '</td>' +
      '<td>' + esc(String(r.status || '—')) + '</td>' +
      '<td>' + esc(r.model || '—') + '</td>' +
      '<td class="num">' + (r.count || 0) + '</td>' +
      '<td>' + esc(r.last || '—') + '</td>' +
      '<td class="sample" title="' + esc(r.sample || '') + '">' + esc(r.sample || '') + '</td></tr>';
  }).join('') : '<tr><td colspan="6" style="color:var(--ink-3)">窗口内没有失败请求</td></tr>';
  $('errNote').textContent = d
    ? rows.length + ' 类 · 统计窗口 24h'
    : '—';
}

/* ── 积分历史（上游 points/records 流水账本，跨账号按时间混排）─────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212）。 */
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 序列化成 RFC3339；这里挡的是手工改文件/换格式等脏数据。
  return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString('zh-CN', { hour12: false });
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + fmtPoints(d);
  if (d < 0) return '−' + fmtPoints(-d);
  return '0';
}
function creditDescText(e) {
  const bits = [e && e.desc, e && e.model, e && e.source].filter(Boolean);
  return (bits.length ? bits.join(' · ') : '') + ' | 余额 ' + fmtPoints(e && e.before) + ' → ' + fmtPoints(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + (e && e.account || '—') + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + fmtPoints(e && e.after) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分流水';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + fmtPoints(Math.abs(net));
}
function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!d) {
    note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5" style="color:var(--ink-3)">积分历史不可用</td></tr>';
    return;
  }
  const entries = d.entries || [];
  note.textContent = creditHistoryNote(entries);
  body.innerHTML = entries.length
    ? entries.map(e => '<tr title="' + esc(creditEntryText(e)) + '">' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(e.account || '—') + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e.delta)) + '</td>' +
      '<td class="num">' + esc(fmtPoints(e.after)) + '</td>' +
      '<td class="clip" title="' + esc(creditDescText(e)) + '">' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5" style="color:var(--ink-3)">暂无积分流水（新注册账号只有首登 5000，消耗后才有流水）</td></tr>';
}
function creditLimitValue() {
  const n = Number($('creditLimit').value);
  return n === 300 || n === 1000 ? n : 100;
}
async function loadCreditHistory() {
  let d = null;
  try { d = await api('credit_history?limit=' + creditLimitValue()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

// 请求记录筛选控件。搜索框防抖 150ms。
let reqQTimer = null;
$('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
$('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
$('reqLimit').onchange = loadLogs;
$('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
trangeBind('reqRange', loadLogs, '0');
$('creditLimit').onchange = loadCreditHistory;
$('btnCreditReload').onclick = loadCreditHistory;

$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
/* CFG_MAP：面板字段 → config.json 键。Loomy 配置是扁平 JSON（蓝本是嵌套分组），
   直接键名映射；api_keys 走专用控件（脱敏值不下发，见下）。面板登录与 /v1/* 用同一把 key。 */
const CFG_MAP = {
  listen: 'listen', default_model: 'default_model', image_fallback_model: 'image_fallback_model', strategy: 'strategy',
  pick_top_n: 'pick_top_n', max_inflight_per_account: 'max_inflight_per_account',
  balance_floor: 'balance_floor',
  sticky_ttl_seconds: 'sticky_ttl_seconds', sticky_max_entries: 'sticky_max_entries',
  sessions_from_client: 'sessions_from_client', identity_mode: 'identity_mode',
  quota_refresh_minutes: 'quota_refresh_minutes',
  session_renew_before_days: 'session_renew_before_days', auto_relogin: 'auto_relogin',
  keepalive_min_tokens: 'keepalive_min_tokens',
  soft_rate_base_seconds: 'soft_rate_base_seconds', soft_rate_max_seconds: 'soft_rate_max_seconds',
  cooldown_seconds: 'cooldown_seconds', not_found_cooldown_seconds: 'not_found_cooldown_seconds',
  breaker_threshold: 'breaker_threshold',
  breaker_cooldown_seconds: 'breaker_cooldown_seconds',
  breaker_cooldown_max_seconds: 'breaker_cooldown_max_seconds',
  timeout_seconds: 'timeout_seconds', max_retries: 'max_retries',
  usage_max_entries: 'usage_max_entries',
  upstream: 'upstream', account_base: 'account_base', account_appid: 'account_appid',
  access_key_id: 'access_key_id', access_key_secret: 'access_key_secret',
  loomy_version: 'loomy_version', proxy: 'proxy',
  auth_dir: 'auth_dir', state_file: 'state_file', log_dir: 'log_dir',
  strip_model_prefix: 'strip_model_prefix',
  log_requests: 'log_requests', log_client_info: 'log_client_info',
};
const NUM_FIELDS = ['pick_top_n', 'max_inflight_per_account', 'balance_floor', 'sticky_ttl_seconds',
  'sticky_max_entries', 'quota_refresh_minutes', 'session_renew_before_days', 'keepalive_min_tokens',
  'soft_rate_base_seconds', 'soft_rate_max_seconds', 'cooldown_seconds', 'not_found_cooldown_seconds',
  'breaker_threshold', 'breaker_cooldown_seconds', 'breaker_cooldown_max_seconds',
  'timeout_seconds', 'max_retries', 'usage_max_entries'];
// 保存后本机密钥可能要换（追加了新 key）：collectConfig 里把控件值透传后端。
function isMasked(v) { return typeof v === 'string' && v.indexOf('已设置') >= 0; }

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config || {};
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, key] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = cfgLoaded[key];
      if (el.type === 'checkbox') el.checked = !!v;
      else el.value = v == null ? '' : v;
    }
    // API 密钥单独填：服务端已改成明文下发（参考面板同款），有「显示」按钮。
    const keys = Array.isArray(cfgLoaded.api_keys) ? cfgLoaded.api_keys : [];
    if ($('cfgKey')) $('cfgKey').value = keys[0] || '';
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, key] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    if (el.type === 'checkbox') { out[key] = el.checked; continue; }
    const raw = el.value.trim();
    if (raw === '' || isMasked(raw)) continue; // 空 / 脱敏值 = 不修改（防误清空，蓝本同款保护）
    if (NUM_FIELDS.includes(name)) {
      const n = Number(raw);
      if (Number.isFinite(n)) out[key] = n;
    } else out[key] = raw;
  }
  const key = ($('cfgKey') && $('cfgKey').value || '').trim();
  if (key) out.api_keys = key; // 服务端把单值转成数组；面板登录与 /v1 共用
  return out;
}
// 数值字段即时校验：非空必须是有限数（保存被拒不如就地标红）。
function markNumberFields() {
  const f = $('cfgForm');
  for (const name of NUM_FIELDS) {
    const el = f.elements[name];
    if (!el) continue;
    const v = el.value.trim();
    const bad = v !== '' && !Number.isFinite(Number(v));
    el.classList.toggle('invalid', bad);
    el.title = bad ? '必须是数字' : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (NUM_FIELDS.includes(ev.target.name)) markNumberFields();
});
$('btnCfgReload').onclick = loadConfig;
// 显示/隐藏 API 密钥（参考面板同款交互）
if ($('btnEye')) $('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  markNumberFields();
  const f = $('cfgForm');
  const firstBad = NUM_FIELDS.find(n => {
    const el = f.elements[n];
    return el && el.value.trim() !== '' && !Number.isFinite(Number(el.value));
  });
  if (firstBad) {
    f.elements[firstBad].focus();
    toast('「' + firstBad + '」必须是数字', 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const patch = collectConfig();
    const r = await api('config', { method: 'POST', body: JSON.stringify(patch) });
    const fields = r.restart_required || [];
    toast(r.message || (fields.length ? '配置已保存，其中 ' + fields.length + ' 项需重启进程生效' : '配置已保存并立即生效'), 'ok');
    const k = ($('cfgKey') && $('cfgKey').value || '').trim();
    if (k) localStorage.setItem(LS_KEY, k); // 改密钥后本机继续持有凭据，不必重登
    await loadConfig();
    $('cfgNote').textContent = fields.length ? '需重启生效：' + fields.join('、') : '';
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号（密码 / 短信验证码 / 导入 JSON）──────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  switchAddTab('password');
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabPwd').hidden = tab !== 'password';
  $('addTabSms').hidden = tab !== 'sms';
  $('addTabImport').hidden = tab !== 'import';
  $('btnAddSave').hidden = tab !== 'password';
  $('btnSmsSend').hidden = tab !== 'sms';
  $('btnSmsLogin').hidden = tab !== 'sms';
  $('btnImportFile').hidden = tab !== 'import';
  ['addPwdDone', 'addPwdErr', 'addSmsDone', 'addSmsErr', 'addImportErr', 'addImportDone']
    .forEach(id => { $(id).hidden = true; });
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
$('btnCloseAdd').onclick = () => $('addVeil').classList.remove('on');

$('btnAddSave').onclick = async () => {
  const f = $('addTabPwd');
  const body = {
    name: (f.querySelector('[name=addName]').value || '').trim(),
    loginid: (f.querySelector('[name=addLogin]').value || '').trim(),
    password: f.querySelector('[name=addPassword]').value || '',
    proxy: (f.querySelector('[name=addProxy]').value || '').trim(),
    login: f.querySelector('[name=addLoginNow]').checked,
  };
  if (!body.loginid || !body.password) { toast('需要手机号 + 密码（可先不勾选「立即登录」只存凭证）', 'err'); return; }
  const btn = $('btnAddSave'); btn.disabled = true;
  $('addPwdErr').hidden = true; $('addPwdDone').hidden = true;
  try {
    await api('accounts', { method: 'POST', body: JSON.stringify(body) });
    $('addPwdDone').hidden = false;
    $('addPwdDone').textContent = '已添加 ' + (body.name || body.loginid) + (body.login ? '（已登录 + 自动签到）' : '（凭证已入池）');
    f.querySelector('[name=addPassword]').value = '';
    invalidatePackages();
    setTimeout(() => { $('addVeil').classList.remove('on'); loadOverview(true); }, 1400);
  } catch (e) {
    $('addPwdErr').hidden = false;
    $('addPwdErr').textContent = e.message;
  } finally { btn.disabled = false; }
};

$('btnSmsSend').onclick = async () => {
  const f = $('addTabSms');
  const body = {
    name: (f.querySelector('[name=smsName]').value || '').trim(),
    phone: (f.querySelector('[name=smsPhone]').value || '').trim(),
  };
  if (!body.phone) { toast('需要手机号', 'err'); return; }
  const btn = $('btnSmsSend'); btn.disabled = true; btn.textContent = '发送中…';
  $('addSmsErr').hidden = true; $('addSmsDone').hidden = true;
  try {
    const r = await api('sms/send', { method: 'POST', body: JSON.stringify(body) });
    smsMsgID = r.msg_id || '';
    $('addSmsDone').hidden = false;
    $('addSmsDone').textContent = '验证码已发送到 ' + body.phone + '；收到后填码点「登录」';
  } catch (e) {
    $('addSmsErr').hidden = false;
    $('addSmsErr').textContent = '发送失败：' + e.message;
  } finally {
    btn.disabled = false; btn.textContent = '发送验证码';
  }
};
$('btnSmsLogin').onclick = async () => {
  const f = $('addTabSms');
  const body = {
    name: (f.querySelector('[name=smsName]').value || '').trim(),
    phone: (f.querySelector('[name=smsPhone]').value || '').trim(),
    code: (f.querySelector('[name=smsCode]').value || '').trim(),
    msg_id: smsMsgID,
  };
  if (!body.phone || !body.code) { toast('需要手机号与验证码', 'err'); return; }
  const btn = $('btnSmsLogin'); btn.disabled = true;
  $('addSmsErr').hidden = true; $('addSmsDone').hidden = true;
  try {
    const r = await api('sms/login', { method: 'POST', body: JSON.stringify(body) });
    $('addSmsDone').hidden = false;
    $('addSmsDone').textContent = '已登录并加入池：' + (r.account && r.account.name || body.name) + '（已自动签到）';
    invalidatePackages();
    setTimeout(() => { $('addVeil').classList.remove('on'); loadOverview(true); }, 1400);
  } catch (e) {
    $('addSmsErr').hidden = false;
    $('addSmsErr').textContent = '登录失败：' + e.message;
  } finally { btn.disabled = false; }
};

$('btnImportFile').onclick = async () => {
  const file = $('importFile').files[0];
  if (!file) { toast('先选择一个 JSON 文件', 'err'); return; }
  const btn = $('btnImportFile'); btn.disabled = true;
  $('addImportErr').hidden = true; $('addImportDone').hidden = true;
  try {
    const parsed = JSON.parse(await file.text());
    const arr = Array.isArray(parsed) ? parsed : [parsed];
    let ok = 0; const errs = [];
    for (const it of arr) {
      try {
        await api('accounts', { method: 'POST', body: JSON.stringify(it) });
        ok++;
      } catch (e) { errs.push((it && it.name || '?') + '：' + e.message); }
    }
    $('addImportDone').hidden = false;
    $('addImportDone').textContent = '导入完成：成功 ' + ok + ' 个' + (errs.length ? '，失败 ' + errs.length + ' 个' : '');
    if (errs.length) $('addImportErr').hidden = false, $('addImportErr').textContent = errs.join('；');
    invalidatePackages();
    loadOverview(true);
  } catch (e) {
    $('addImportErr').hidden = false;
    $('addImportErr').textContent = '导入失败：' + e.message;
  } finally { btn.disabled = false; }
};

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.title = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('额度已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.title = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 折叠区块 ─────────────────────────────────────────────────────── */
/* 次要区块（模型锁池 / 已下架 / 按小时）默认收起，把首屏让给账号池和用量明细；
   状态写 localStorage，刷新与切页都保持。 */
const LS_FOLD = 'loomy2api.folds';
const FOLD_DEFAULT = { expiry: true, modellocks: true, deadmodels: true, hourly: true };
function foldState() {
  try { return Object.assign({}, FOLD_DEFAULT, JSON.parse(localStorage.getItem(LS_FOLD) || '{}')); }
  catch (_) { return Object.assign({}, FOLD_DEFAULT); }
}
function applyFolds() {
  const st = foldState();
  document.querySelectorAll('button[data-fold]').forEach(b => {
    const on = !!st[b.dataset.fold];
    const box = b.closest('.box');
    if (box) box.classList.toggle('folded', on);
    b.textContent = on ? '展开' : '收起';
    b.setAttribute('aria-expanded', String(!on));
  });
}
document.querySelectorAll('button[data-fold]').forEach(b => {
  b.addEventListener('click', () => {
    const st = foldState();
    st[b.dataset.fold] = !st[b.dataset.fold];
    localStorage.setItem(LS_FOLD, JSON.stringify(st));
    applyFolds();
  });
});
applyFolds();

/* ── 轮询 ─────────────────────────────────────────────────────────── */
function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'logs') loadLogs(false);
}
function start() {
  loadOverview(true).catch(() => {});
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, 5000);
}
start();

/* ── 邀请码 ───────────────────────────────────────────────────────── */
/* 三条铁律（都由后端事实推出来，前端只负责不让人误触）：
   1. 一个号只能绑一次别人的码，绑完不可撤销 —— 所以「执行」必须经过预览 + 二次确认；
   2. 收益是双边的（被邀 5000 / 邀请 5000），所以「两两互绑」比「全灌一个码」平均多一倍；
   3. 批量是后台任务（118 条 × 1.2s ≈ 2.5 分钟），同步请求必然超时 —— 走轮询出进度。 */
let ivData = null, ivPlan = null, ivPoll = null;
/* 121 个号一屏放不下（同账号池）：筛选 + 分页状态放在模块级，
   渲染永远从 ivData 重新派生，不缓存行。 */
let ivFilter = { q: '', page: 1, size: 20 };

async function loadInvites() {
  const body = $('ivBody');
  if (!body) return;
  body.innerHTML = '<tr><td colspan="6"><div class="tk-empty"><div class="big">正在读取各账号邀请码…</div>'
    + '<div>每号两次 GET（activation + invitation-codes）· 并发 8 路 · 单号 15s 超时</div></div></td></tr>';
  $('btnIvPlan').disabled = true;
  try {
    renderInvites(await api('invites'));
  } catch (e) {
    body.innerHTML = '<tr><td colspan="6"><div class="tk-empty"><div class="big">读取失败</div>'
      + '<div>' + esc(e.message) + '</div></div></td></tr>';
    $('btnIvPlan').disabled = false;
  }
}

function renderInvites(d) {
  ivData = d;
  const sum = d.summary || {}, rows = d.accounts || [];
  $('ivAccounts').textContent = sum.accounts || 0;
  $('ivBindable').textContent = sum.bindable || 0;
  $('ivBound').textContent = sum.bound || 0;
  $('ivNoCode').textContent = sum.no_code || 0;
  $('ivFailed').textContent = sum.failed || 0;
  $('ivPotential').textContent = fmtPoints(sum.potential || 0);
  $('ivMatrixNote').textContent = (sum.bindable || 0) + ' 个号还能绑 · 单次双边各 ' + fmtPoints(sum.reward_each || 5000);
  $('btnIvPlan').disabled = false;

  renderIvRows();

  // 刷新页面时若后台任务还在跑，直接把进度接上（否则用户会以为任务丢了）。
  if (d.job && d.job.running) startIvPoll();

  const q = ($('ivQ').value || '').trim().toLowerCase();
  $('ivNote').textContent = q ? '筛选 ' + q : (sum.bindable ? '可绑定 ' + sum.bindable + ' 个' : '无可绑定账号');
}

function renderIvRows() {
  const body = $('ivBody');
  if (!body || !ivData) return;
  const q = ($('ivQ').value || '').trim().toLowerCase();
  const rows = (ivData.accounts || []).filter(r => !q
    || String(r.name || '').toLowerCase().includes(q)
    || String(r.remark || '').toLowerCase().includes(q)
    || String(r.code || '').toLowerCase().includes(q));
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="6"><div class="tk-empty"><div class="big">没有匹配的账号</div>'
      + '<div>换个关键词试试，或点「刷新」重新读取上游状态。</div></div></td></tr>';
    renderPager('ivPager', { total: 0, size: ivFilter.size, page: 1, pages: 1 }, ivFilter, renderIvRows);
    return;
  }
  const pg = pageSlice(rows, ivFilter);
  renderPager('ivPager', pg, ivFilter, renderIvRows);
  body.innerHTML = pg.rows.map(r => {
    const codes = r.codes || [];
    const main = codes[0] || null;
    const used = main ? (Number(main.usedCount) || 0) : 0;
    const cap = main ? (Number(main.maxUses) || 0) : 0;
    const st = r.error
      ? '<span class="tag bad" title="' + esc(r.error) + '">查询失败</span>'
      : r.bound
        ? '<span class="tag mute">已绑定</span>'
        : (r.code ? '<span class="tag ok">可绑定</span>' : '<span class="tag warn">无自持码</span>');
    return '<tr>'
      + '<td><div class="nm">' + esc(r.name) + '</div>'
      + (r.remark ? '<div class="sub">' + esc(r.remark) + '</div>' : '') + '</td>'
      + '<td>' + (r.code
        ? '<span class="tag mute" title="被邀请数 / 上限">' + esc(r.code) + '</span>'
        : '<span class="dim">—</span>') + '</td>'
      + '<td>' + (r.applied ? '<span class="tag ok">' + esc(r.applied) + '</span>' : '<span class="dim">未绑定</span>') + '</td>'
      + '<td class="num">' + (main ? used + ' / ' + cap : '—') + '</td>'
      + '<td class="mono dim">' + esc(r.egress || '—') + '</td>'
      + '<td>' + st + '</td>'
      + '</tr>';
  }).join('');
}

$('ivQ').oninput = () => { ivFilter.q = $('ivQ').value; ivFilter.page = 1; if (ivData) renderIvRows(); };
$('btnIvReload').onclick = () => loadInvites();

// 预览：把「谁绑谁的码」摊开给人看。不可逆操作不能只给一个按钮。
$('btnIvPlan').onclick = async () => {
  const btn = $('btnIvPlan');
  btn.disabled = true; btn.textContent = '核对中…';
  $('btnIvRun').disabled = true;
  const mode = $('ivMode').value, target = ($('ivTarget').value || '').trim();
  if (mode === 'target' && !target) { toast('统一绑码模式需要填写目标邀请码', 'err'); btn.disabled = false; btn.textContent = '预览计划'; return; }
  try {
    const d = await api('invites/bind', { method: 'POST', body: JSON.stringify({ mode, target, confirm: false }) });
    ivPlan = d;
    renderIvPlan(d);
    $('btnIvRun').disabled = !(d.plan || []).length;
    toast((d.plan || []).length ? '计划已生成：' + d.plan.length + ' 条绑定' : '没有可绑定的账号', (d.plan || []).length ? 'ok' : 'err');
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '预览计划'; }
};

function renderIvPlan(d) {
  const box = $('ivPlanBox'), plan = d.plan || [], un = d.unpaired || [];
  if (!plan.length) {
    box.innerHTML = '<div class="tk-empty"><div class="big">没有可执行的绑定</div>'
      + '<div>候选号要么都绑过了，要么没有自持码 / 登录态失效。</div></div>';
    return;
  }
  const gained = Number(d.estimate || 0);
  let html = '<div class="note" style="margin-bottom:10px">模式 <b>' + (d.mode === 'pair' ? '两两互绑' : '统一绑码')
    + '</b> · ' + plan.length + ' 条 · 每条让被邀方到账 ' + fmtPoints((d.summary || {}).reward_each || 5000)
    + '，池子合计 <b>' + fmtPoints(gained * 2) + '</b>（双边）</div>';
  html += '<div class="tbl-wrap"><table class="acc"><thead><tr><th>账号</th><th>绑定到</th><th>码的持有者</th><th>说明</th></tr></thead><tbody>'
    + plan.map(a => '<tr><td>' + esc(a.account) + '</td><td class="iv-pair">' + esc(a.code) + '</td>'
      + '<td class="iv-pair">' + (a.from ? esc(a.from) : '<em>外部码</em>') + '</td>'
      + '<td class="dim">' + esc(a.note || '') + '</td></tr>').join('')
    + '</tbody></table></div>';
  if (un.length) {
    html += '<div class="note" style="margin-top:10px">落单未绑：' + un.map(esc).join('、')
      + '（奇数个 / 无可配对码）。填一个「奇数补位码」再预览，可把这 1 次的 5000 也换出来。</div>';
  }
  box.innerHTML = html;
}

$('btnIvRun').onclick = async () => {
  if (!ivPlan || !(ivPlan.plan || []).length) return;
  const n = ivPlan.plan.length;
  if (!confirm('即将执行 ' + n + ' 条绑定，每个号只能绑一次且不可撤销。\n\n执行前会逐号复查上游状态，已绑过的自动跳过。确认执行？')) return;
  const btn = $('btnIvRun');
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const d = await api('invites/bind', {
      method: 'POST',
      body: JSON.stringify({ mode: ivPlan.mode, target: ($('ivTarget').value || '').trim(), confirm: true }),
    });
    if (!d.started) { toast(d.message || '没有启动任务', 'err'); btn.disabled = false; btn.textContent = '执行绑定'; return; }
    toast('已启动：共 ' + d.total + ' 条，逐条执行中…', 'ok');
    startIvPoll();
  } catch (e) {
    toast(e.message, 'err');
    btn.disabled = false; btn.textContent = '执行绑定';
  }
};

function startIvPoll() {
  if (ivPoll) clearInterval(ivPoll);
  const tick = async () => {
    let p;
    try { p = await api('invites/progress'); } catch (e) { return; }
    renderIvProgress(p);
    if (!p.running) {
      clearInterval(ivPoll); ivPoll = null;
      $('btnIvRun').disabled = false; $('btnIvRun').textContent = '执行绑定';
      if (Number(p.done) > 0) toast('绑定完成：成功 ' + p.done + ' 条，到账 ' + fmtPoints(p.gained || 0) + ' 积分', 'ok');
      if (Number(p.failed) > 0) toast('其中 ' + p.failed + ' 条失败，见下方结果表', 'err');
      loadInvites(); // 结束时必须重取矩阵：额度与绑定状态都变了
    }
  };
  tick();
  ivPoll = setInterval(tick, 2000);
}

function renderIvProgress(p) {
  const box = $('ivProgress');
  box.hidden = false;
  const total = Number(p.total || 0), done = Number(p.done || 0) + Number(p.skipped || 0) + Number(p.failed || 0);
  const pct = total ? Math.min(100, Math.round(done / total * 100)) : 0;
  let html = '<div class="note">' + (p.running ? '执行中' : '已结束') + '：' + done + ' / ' + total
    + ' · 成功 <b>' + (p.done || 0) + '</b> · 跳过 ' + (p.skipped || 0) + ' · 失败 <b>' + (p.failed || 0) + '</b>'
    + ' · 本次到账 ' + fmtPoints(p.gained || 0) + ' 积分</div>';
  html += '<div class="iv-bar"><i style="width:' + pct + '%"></i></div>';
  const rs = (p.results || []).slice().reverse();
  if (rs.length) {
    html += '<div class="tbl-wrap" style="max-height:320px"><table class="acc"><thead><tr>'
      + '<th>账号</th><th>码</th><th>结果</th><th>说明</th><th class="num">到账</th></tr></thead><tbody>'
      + rs.map(r => '<tr><td>' + esc(r.account) + '</td><td class="iv-pair">' + esc(r.code) + '</td>'
        + '<td>' + (r.ok ? '<span class="tag ok">成功</span>' : r.skipped ? '<span class="tag mute">跳过</span>' : '<span class="tag bad">失败</span>') + '</td>'
        + '<td class="dim">' + esc(r.reason || '') + '</td>'
        + '<td class="num">' + (r.invitee_reward ? '+' + fmtPoints(r.invitee_reward) : '—') + '</td></tr>').join('')
      + '</tbody></table></div>';
  }
  box.innerHTML = html;
}

/* ── 用量 ─────────────────────────────────────────────────────────── */
/* 图表用原生 SVG 手绘：面板是 go:embed 单文件、无构建步骤，引入图表库
   就得带上打包器，得不偿失。这里只需要堆叠柱状图，几十行足够。 */
function usStat(v, k, cls) {
  return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
         '</div><div class="k">' + esc(k) + '</div></div>';
}
/* usKpi 用量页的指标卡。比账号池的 .stat 多两样：语义色轨（cls）与副标题（sub）。 */
function usKpi(v, k, cls, sub, bar) {
  return '<div class="kpi ' + (cls || '') + '">' +
    '<div class="k">' + esc(k) + '</div>' +
    '<div class="v">' + esc(v) + '</div>' +
    (bar || '') +
    (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
    '</div>';
}
/* usMixBar prompt/completion 占比条。宽度按百分比而不是固定像素：表列宽随窗口变化，
   像素宽度在窄屏会溢出、宽屏又显得没信息。 */
function usMixBar(prompt, completion, total) {
  const t = Number(total || 0);
  if (!t) return '';
  const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
  const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
  return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
    '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
    '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
    '</span>';
}
/* usPct 占比文案（0 值不显示 "0.0%"，直接 —，避免一行全是零）。 */
function usPct(part, total) {
  const t = Number(total || 0);
  if (!t) return '—';
  return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* 归一化后端用量响应：/panel/api/usage 返回 {window, hourly}，蓝本渲染层要的是
   totals + by_* 数组 + series。在这里做一层适配，前端其余部分保持蓝本原样。 */
function normalizeUsage(d) {
  const w = d.window || {};
  const total = Number(w.prompt_tokens || 0) + Number(w.completion_tokens || 0);
  const byKey = (m) => Object.entries(m || {}).map(([key, b]) => ({
    key,
    requests: (b && b.requests) || 0, errors: (b && b.fail) || 0,
    prompt_tokens: (b && b.prompt_tokens) || 0, completion_tokens: (b && b.completion_tokens) || 0,
    total_tokens: ((b && b.prompt_tokens) || 0) + ((b && b.completion_tokens) || 0),
    points: (b && b.points) || 0, avg_latency_ms: (b && b.avg_latency_ms) || 0,
  }));
  return {
    totals: {
      requests: w.total || 0, errors: w.fail || 0,
      prompt_tokens: w.prompt_tokens || 0, completion_tokens: w.completion_tokens || 0,
      reasoning_tokens: w.reasoning_tokens || 0,
      total_tokens: total, points: w.points || 0,
      avg_latency_ms: w.avg_latency_ms || 0,
    },
    by_account: byKey(w.by_account),
    by_model: byKey(w.by_model),
    hourly: d.hourly || [],
    from: w.from, to: w.to,
  };
}

function renderUsage(d) {
  usageData = d || {};
  const t = usageData.totals || {};
  const total = Number(t.total_tokens || 0);
  const pt = Number(t.prompt_tokens || 0);
  const ct = Number(t.completion_tokens || 0);
  const reqs = Number(t.requests || 0);
  const errs = Number(t.errors || 0);
  const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
  // 构成条要的是合法 CSS 宽度，usPct 在无样本时返回 "—"，不能直接拼进 style。
  const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
  // 卡片、明细表与时序图全部按所选窗口统计（切窗口数字随之变化）。
  $('usStats').innerHTML =
    usKpi(fmtTok(reqs), '请求数', 'c-accent',
      errs ? '其中失败 ' + errs + ' 次' : '全部成功') +
    usKpi(fmtTok(total), '总 token', 'c-accent',
      'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
      '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
      '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
    usKpi(fmtTok(pt), 'prompt', 'c-soft', '占比 ' + usPct(pt, total)) +
    usKpi(fmtTok(ct), 'completion', 'c-ok', '占比 ' + usPct(ct, total)) +
    usKpi(String(errs), '失败尝试', errs ? 'c-warn' : 'c-mute',
      okRate == null ? '—' : (errs ? '成功率 ' + okRate.toFixed(1) + '%' : '成功率 100%')) +
    usKpi(fmtPoints(t.points), '消耗积分', 'c-warn', t.reasoning_tokens ? '思考 ' + fmtTok(t.reasoning_tokens) + ' tok' : '上游观测计量') +
    usKpi(fmtMs(t.avg_latency_ms), '平均延迟', 'c-soft');

  const winLabel = trangeLabel('usRange');
  const rangeEcho = usageData.from
    ? new Date(usageData.from * 1000).toLocaleString('zh-CN', { hour12: false }).slice(0, 16) +
      ' → ' + (usageData.to ? new Date(usageData.to * 1000).toLocaleString('zh-CN', { hour12: false }).slice(0, 16) : '现在')
    : '';
  const note = ((rangeEcho || winLabel) + ' · ' + (usageData.hourly || []).length + ' 个分桶');
  $('usNote').textContent = note;
  $('usNote').title = note; // 窄屏单行截断时靠悬停看全

  renderUsageDim();
  renderCreditDim();
  renderHourlyTable(usageData.hourly || []);
  renderUsageChart(usageData.hourly || []);
}

// renderHourlyTable 积分与 token 按小时（蓝本 usHourBody 表位的 Loomy 实现）。
function renderHourlyTable(hourly) {
  const rows = (hourly || []).filter(h => (h.requests || 0) > 0 || (h.tokens || 0) > 0);
  const tb = $('usHourBody');
  tb.innerHTML = rows.length ? rows.map(h => {
    const tok = Number(h.tokens || 0);
    return '<tr>' +
      '<td class="num">' + esc(h.hour || '') + '</td>' +
      '<td class="num">' + fmtTok(h.requests || 0) + '</td>' +
      '<td class="num">' + fmtTok(h.prompt_tokens || 0) + '</td>' +
      '<td class="num">' + fmtTok(h.completion_tokens || 0) + '</td>' +
      '<td class="num">' + fmtTok(tok) + usMixBar(h.prompt_tokens, h.completion_tokens, tok) + '</td>' +
      '<td class="num">' + fmtPoints(h.points) + '</td>' +
      '</tr>';
  }).join('') : '<tr><td colspan="6" class="empty">该窗口暂无用量</td></tr>';
}

/* ── 用量明细：维度页签共用一张表 ──────────────────────────────────── */
const US_DIMS = {
  account: { key: 'by_account', title: '账号', withPerf: true, span: 9 },
  model: { key: 'by_model', title: '模型', withPerf: false, span: 8 },
};
function usSortRows(rows, sort) {
  const out = rows.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  const val = a => sort === 'requests' ? num(a.requests)
    : sort === 'errors' ? num(a.errors)
    : sort === 'latency' ? num(a.avg_latency_ms)
    : sort === 'points' ? num(a.points)
    : num(a.total_tokens);
  out.sort((a, b) => val(b) - val(a));
  return out;
}
function usDimHead(dim) {
  const m = US_DIMS[dim] || US_DIMS.account;
  return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
    '<th class="num">请求</th><th class="num">失败</th>' +
    '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th><th class="num">积分</th>' +
    (m.withPerf ? '<th class="num">均延迟</th>' : '') +
    '</tr>';
}
/* usTabsHtml 维度切换按钮（带条数徽标）。整段 innerHTML 重写而不是逐个改 class：
   容器的 click 监听是委托式的，换掉子节点不会丢事件，代码也更短。 */
function usTabsHtml(dims, active, counts) {
  return dims.map(([k, label]) => {
    const n = counts ? counts[k] : null;
    return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
      esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
  }).join('');
}
const US_DIM_TABS = [['account', '按账号'], ['model', '按模型']];
function renderUsageDim() {
  const d = usageData || {};
  const m = US_DIMS[usDim] || US_DIMS.account;
  const q = usDimFilter.q.trim().toLowerCase();
  const all = usSortRows(d[m.key] || [], usSort);
  const rows = q
    ? all.filter(x => (String(x.key || '') + ' ' + remarkOf(x.key)).toLowerCase().includes(q))
    : all;
  const pg = pageSlice(rows, usDimFilter);
  $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
    account: (d.by_account || []).length,
    model: (d.by_model || []).length,
  });
  $('usDimHead').innerHTML = usDimHead(usDim);
  $('usDimBody').innerHTML = pg.rows.map(x =>
    '<tr><td class="mark" aria-hidden="true"></td>' +
    '<td>' + remarkName(x.key, esc(x.key || '—')) + '</td>' +
    '<td class="num">' + fmtTok(x.requests) + '</td>' +
    '<td class="num">' + (x.errors ? '<span style="color:var(--warn)">' + fmtTok(x.errors) + '</span>' : '—') + '</td>' +
    '<td class="num">' + fmtTok(x.prompt_tokens) + '</td>' +
    '<td class="num">' + fmtTok(x.completion_tokens) + '</td>' +
    '<td class="num">' + fmtTok(x.total_tokens) + usMixBar(x.prompt_tokens, x.completion_tokens, x.total_tokens) + '</td>' +
    '<td class="num">' + fmtPoints(x.points) + '</td>' +
    (m.withPerf ? '<td class="num">' + fmtMs(x.avg_latency_ms) + '</td>' : '') +
    '</tr>'
  ).join('') || '<tr><td colspan="' + m.span + '" class="empty">' +
    (q ? '没有匹配「' + esc(usDimFilter.q) + '」的' + esc(m.title) : '暂无数据') + '</td></tr>';
  $('usDimNote').textContent = rows.length + ' 行 · 请求数含失败尝试';
  if ($('usFilterNote')) $('usFilterNote').textContent = q ? '筛选 ' + rows.length + ' / ' + all.length : '';
  renderPager('usDimPager', pg, usDimFilter, renderUsageDim);
}
$('usDimTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usDim = b.dataset.dim;
  renderUsageDim();
});
$('usSort').onchange = () => { usSort = $('usSort').value; renderUsageDim(); };
$('usQ').addEventListener('input', () => { usDimFilter.q = $('usQ').value; usDimFilter.page = 1; renderUsageDim(); });
$('usSize').onchange = () => { usDimFilter.size = Number($('usSize').value) || 20; usDimFilter.page = 1; renderUsageDim(); };
$('usCreditSize').onchange = () => { usCreditFilter.size = Number($('usCreditSize').value) || 20; usCreditFilter.page = 1; renderCreditDim(); };

/* ── 积分扣除维度（按账号/按模型；数据同 usage 响应，Loomy 里 points 即积分）── */
function renderCreditDim() {
  const d = usageData || {};
  const dimKey = usCreditDim === 'model' ? 'by_model' : 'by_account';
  const all = (d[dimKey] || []).filter(x => Number(x.points || 0) > 0);
  const pg = pageSlice(all, usCreditFilter);
  const tokSum = all.reduce((n, x) => n + Number(x.total_tokens || 0), 0);
  const ptSum = all.reduce((n, x) => n + Number(x.points || 0), 0);
  $('usCreditStats').innerHTML =
    usKpi(fmtPoints(ptSum), '消耗积分', 'c-accent', '台账观测值') +
    usKpi(fmtTok(tokSum), '匹配 Token', 'c-mute', '与积分同时观测') +
    usKpi(tokSum ? trimFixed((ptSum / tokSum * 1e6).toFixed(2)) + ' / 1M' : '—', '平均积分 / 1M Token', 'c-ok', '越低越划算') +
    usKpi(String(all.length), '有扣费口径', 'c-mute', usCreditDim === 'model' ? '个模型' : '个账号');
  $('usCreditHead').innerHTML = '<tr><th class="mark" aria-hidden="true"></th><th>' +
    (usCreditDim === 'model' ? '模型' : '账号') + '</th><th class="num">请求</th><th class="num">扣除积分</th><th class="num">Token</th><th class="num">积分 / 1M Token</th></tr>';
  $('usCreditBody').innerHTML = pg.rows.map(x =>
    '<tr><td class="mark" aria-hidden="true"></td>' +
    '<td>' + remarkName(x.key, esc(x.key || '—')) + '</td>' +
    '<td class="num">' + fmtTok(x.requests) + '</td>' +
    '<td class="num">' + fmtPoints(x.points) + '</td>' +
    '<td class="num">' + fmtTok(x.total_tokens) + '</td>' +
    '<td class="num">' + (x.total_tokens ? trimFixed((x.points / x.total_tokens * 1e6).toFixed(2)) : '—') + '</td>' +
    '</tr>'
  ).join('') || '<tr><td colspan="6" class="empty">该窗口内没有积分消耗记录</td></tr>';
  $('usCreditTabs').innerHTML = usTabsHtml(US_DIM_TABS, usCreditDim, {
    account: (d.by_account || []).filter(x => Number(x.points || 0) > 0).length,
    model: (d.by_model || []).filter(x => Number(x.points || 0) > 0).length,
  });
  renderPager('usCreditPager', pg, usCreditFilter, renderCreditDim);
}
$('usCreditTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-dim]');
  if (!b) return;
  usCreditDim = b.dataset.dim;
  renderCreditDim();
});

/* 图表点位：后端 hour 桶 {hour:"MM-DD HH:00", requests, prompt_tokens,
   completion_tokens, tokens, points}。 */
function parseHourPoint(s) {
  const text = String(s || '');
  let m = text.match(/^(\d{2})-(\d{2}) (\d{2}):00$/);
  const now = new Date();
  if (m) {
    // "MM-DD HH:00" → 当年本地时区；跨年窗口按"更早就是明年"修正（30 天窗口够用）。
    let d = new Date(Number(now.getFullYear()), Number(m[1]) - 1, Number(m[2]), Number(m[3]), 0, 0);
    if (d.getTime() > now.getTime() + 3600 * 1000) d = new Date(d.getFullYear() - 1, d.getMonth(), d.getDate(), d.getHours());
    return isNaN(d.getTime()) ? null : d.getTime();
  }
  m = text.match(/^(\d{2})-(\d{2})$/);
  if (m) {
    let d = new Date(Number(now.getFullYear()), Number(m[1]) - 1, Number(m[2]), 0, 0, 0);
    if (d.getTime() > now.getTime() + 86400 * 1000) d = new Date(d.getFullYear() - 1, d.getMonth(), d.getDate());
    return isNaN(d.getTime()) ? null : d.getTime();
  }
  return null;
}
// 跨日窗口用日期标签，纯日内窗口用小时标签。
function fmtTokTimeLabel(t, minute) {
  const d = new Date(t);
  return minute
    ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(hourly) {
  const host = $('usChart');

  // 丢掉时间解析不出来的点，而不是让 NaN 传染整张图。
  const pts = [];
  for (const h of hourly) {
    const t = parseHourPoint(h.hour);
    if (t == null) continue;
    const pt = Number(h.prompt_tokens || 0);
    const ct = Number(h.completion_tokens || 0);
    const tt = Number(h.tokens || 0) || (pt + ct);
    pts.push({ t, raw: h.hour, pt, ct, tt, pts: Number(h.points || 0), req: h.requests || 0 });
  }
  if (!pts.length) {
    host.innerHTML = '<div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>';
    $('usChartNote').textContent = '—';
    return;
  }

  const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
  const iw = W - PL - PR, ih = H - PT - PB;

  const t0 = pts[0].t;
  const t1 = pts[pts.length - 1].t;
  const span = Math.max(1, t1 - t0);

  const max = Math.max(1, ...pts.map(p => p.tt));
  const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
  const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
  $('usChartNote').textContent =
    pts.length + ' 个点 · 峰值 ' + fmtTok(peak.tt) + ' @ ' + peak.raw +
    ' · 均值 ' + fmtTok(avg);

  // 柱宽取「最小真实间隔」的 70%，并夹在合理区间内——窗口拉到 30 天时柱子会
  // 变细，但不会细到看不见。
  let minGap = Infinity;
  for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
  if (!isFinite(minGap) || minGap <= 0) minGap = span;
  const slot = iw * (minGap / span);
  const bw = Math.max(2, Math.min(30, slot * 0.7));

  // 首尾各让出半个柱宽：否则第一个点和最后一个点的柱子会各有一半跑到绘图区外。
  const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
  const yOf = v => PT + ih - ih * (v / max);

  let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
            'preserveAspectRatio="xMidYMid meet">';

  // 柱体渐变：顶部实、底部略透，堆叠时两段仍能一眼分清（纯色块并排会糊成一片）。
  // 注意 stop-color 必须走 style 而不是 presentation 属性——Blink/WebKit 不解析
  // 属性里的 var()，写成 stop-color="var(--accent)" 会整条渐变失效（柱子全透明）。
  out += '<defs>' +
    '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
    '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
    '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
    '</defs>';

  // y 轴网格 + 刻度
  for (let i = 0; i <= 4; i++) {
    const y = PT + ih - (ih * i / 4);
    out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
           '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
  }

  // 均值参考线：一眼看出"这根是不是异常高"，比只给刻度省心。
  if (avg > 0 && avg < max) {
    const y = yOf(avg);
    out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
           '" y2="' + y.toFixed(1) + '"/>';
    out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
           '" text-anchor="start">均值 ' + fmtTok(avg) + '</text>';
  }

  // 柱子。每根一个 <g>，<title> 作为它的**子元素**：
  //   - <title> 只有作为元素的子节点才是那个元素的悬停提示；挂在 <svg> 根下时浏览器
  //     把它当整图的说明，于是**所有柱子都显示最后一条数据**（蓝本 issue #128）。
  //   - 两个 rect（prompt/completion 堆叠）共用一个 <title>，悬停任一段都是同一份明细。
  // 类名用 usbar 而不是 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
  // height 是 rect 的 CSS 几何属性，同名类会把每根柱子压成 3px 高（踩过）。
  const yBase = PT + ih;
  for (const p of pts) {
    const x = xOf(p.t) - bw / 2;
    const hTot = ih * (p.tt / max);
    const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
    const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
    out += '<g><title>' + esc(p.raw) + '  ' + fmtTok(p.pt) + ' prompt / ' +
           fmtTok(p.ct) + ' completion / ' + p.req + ' 次 / ' + fmtPoints(p.pts) + ' 积分</title>';
    if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
      '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
    if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
      '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
      '" fill="url(#usGradC)" rx="1.5"/>';
    out += '</g>';
  }

  // 峰值标注：柱子够窄时文字压在柱顶，够宽时贴右侧避免和柱体重叠。
  {
    const px = xOf(peak.t);
    const py = yOf(peak.tt);
    const anchor = px > W - PR - 90 ? 'end' : 'middle';
    out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
           '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
           '峰值 ' + fmtTok(peak.tt) + '</text>';
  }

  // x 轴基线画在柱子之后，避免压在柱底
  out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
         '" y2="' + yBase + '"/>';

  // x 轴刻度：按真实时间等距取 6 个位置，取该位置**最近的实际柱子**做标签，
  // 所以标签永远落在有数据的点上，不会指到空档里。
  const TICKS = Math.min(6, pts.length);
  const usedLabel = new Set();
  const multiDay = new Set(pts.map(p => new Date(p.t).getDate())).size > 1;
  for (let k = 0; k < TICKS; k++) {
    const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
    let bi = 0, best = Infinity;
    for (let i = 0; i < pts.length; i++) {
      const d = Math.abs(pts[i].t - target);
      if (d < best) { best = d; bi = i; }
    }
    if (usedLabel.has(bi)) continue;
    usedLabel.add(bi);
    const p = pts[bi];
    const cx = xOf(p.t);
    const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
    out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
           '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' +
           esc(fmtTokTimeLabel(p.t, multiDay)) + '</text>';
  }

  // 跨天时补一条日期分隔线，让「日界」在长窗口里可见
  let prevDay = null;
  for (const p of pts) {
    const d = new Date(p.t).getDate();
    if (prevDay !== null && d !== prevDay) {
      const x = xOf(p.t).toFixed(1);
      out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
             (PT + ih) + '" style="opacity:.45"/>';
    }
    prevDay = d;
  }

  out += '</svg>';
  host.innerHTML = out;
}

$('btnUsage').onclick = loadUsage;
// 时间范围控件绑定：任何改动（预设切换 / 自定义起止）都重新拉一次用量。
trangeBind('usRange', loadUsage);

async function loadUsage() {
  const q = trangeQuery('usRange', true);
  try {
    const d = await api('usage?' + q.toString());
    renderUsage(normalizeUsage(d));
  } catch (e) {
    // 失败时三块都要清干净：只改图表会留下上一次窗口的数字，看起来像"刷新成功"。
    usageData = null;
    $('usChart').innerHTML = '<div class="us-empty">读取用量失败：' + esc(e.message) + '</div>';
    $('usChartNote').textContent = '—';
    $('usStats').innerHTML = '';
    $('usCreditStats').innerHTML = '';
    $('usNote').textContent = '—';
    $('usDimNote').textContent = '—';
    $('usDimHead').innerHTML = '';
    $('usCreditHead').innerHTML = '';
    $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
    $('usCreditTabs').innerHTML = usTabsHtml(US_DIM_TABS, usCreditDim, null);
    $('usDimBody').innerHTML = '<tr><td colspan="9" class="empty">读取用量失败</td></tr>';
    $('usCreditBody').innerHTML = '<tr><td colspan="6" class="empty">读取用量失败</td></tr>';
    $('usHourBody').innerHTML = '<tr><td colspan="6" class="empty">读取用量失败</td></tr>';
  }
}

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* Loomy 口径（抓包核对）：余额 = 每日赠送（登录后刷新 5000，次日周期到期）+
   长期积分（注册奖励/邀请，不到期）；扣费先活动/兑换码、后长期（上游执行）。
   后端 packages 已把额度折算成「包」，这里吃蓝本的包渲染。 */
const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
                   '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
                           '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
                           '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];
function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }
const PK_DAY_MS = 24 * 3600 * 1000;

// pkAccountColorMap 按 UID 稳定分配颜色：排序后分配，账号刷新/重排不会换色。
function pkAccountColorMap(list) {
  const uids = (list || [])
    .filter(a => a && !a.error && a.uid)
    .map(a => String(a.uid))
    .sort();
  const colors = new Map();
  uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
  return colors;
}

// pkExpiryMs 包到期毫秒时间戳（Loomy 每日包的 end_time 是 "YYYY-MM-DD"，
// 无时区后缀 → 按 +08:00 解析，与上游运营口径一致）。
function pkExpiryMs(p) {
  const text = String((p && p.end_time) || '').trim();
  if (!text) return null;
  let iso = text.includes('T') ? text : text.replace(' ', 'T');
  if (!/(?:Z|[+-]\d\d:?\d\d)$/.test(iso)) iso += '+08:00';
  const parsed = Date.parse(iso);
  return Number.isFinite(parsed) ? parsed : null;
}
function pkExpiryText(expiresAt) {
  if (!expiresAt) return '无到期时间';
  const diff = expiresAt - Date.now();
  if (diff <= 0) return '已到期';
  const minutes = Math.max(1, Math.ceil(diff / 60000));
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟';
  const hours = Math.ceil(diff / 3600000);
  if (hours < 24) return '剩余 ' + hours + ' 小时';
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天';
}
function pkCreditOpacity(days) {
  if (days == null || !Number.isFinite(Number(days))) return 1;
  return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

/* pkBySource 把包按名称归并：「来源 → 面额/剩余/个数」。 */
function pkBySource(packs) {
  const m = new Map();
  for (const p of packs) {
    const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    };
    e.n += 1;
    e.remain += Number(p.remain || 0);
    e.size += Number(p.size || 0);
    e.used += Number(p.used || 0);
    const t = String(p.end_time || '').slice(0, 10);
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
    const c = String(p.created_at || '').slice(0, 10);
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    m.set(k, e);
  }
  return [...m.values()].sort((a, b) => b.size - a.size);
}

function pkAccountSegments(a, now) {
  let balance = Math.max(0, Number(a.remain || 0));
  const out = [];
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0);
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
    const amount = Math.min(balance, remain);
    const expiresAt = pkExpiryMs(p);
    out.push({
      amount, expiresAt, uid: String(a.uid || ''),
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      accountName: a.nickname || String(a.uid || '') || '未命名账号',
    });
    balance -= amount;
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1;
    if (x.expiresAt != null && y.expiresAt == null) return -1;
    return (x.expiresAt || 0) - (y.expiresAt || 0);
  });
}

function summarizeCreditDays(list, now) {
  const buckets = new Map();
  let unavailable = 0;
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) { unavailable++; continue; }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue;
      let row = buckets.get(segment.days);
      if (!row) { row = { days: segment.days, credits: 0, segments: [] }; buckets.set(segment.days, row); }
      row.credits += segment.amount;
      row.segments.push(segment);
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
  return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
  const summary = summarizeCreditDays(list, now);
  const colors = pkAccountColorMap(list);
  const rows = summary.rows.map(row => {
    const total = row.credits || 1;
    const nodes = row.segments.map(segment => {
      const color = colors.get(segment.uid) || 'var(--accent)';
      const title = segment.source + '\n' + fmtTok(segment.amount) + ' 积分\n' +
        (segment.expiresAt ? '到期 ' + new Date(segment.expiresAt).toLocaleString('zh-CN', { hour12: false }) + '（' + pkExpiryText(segment.expiresAt) + '）' : '不到期') + '\n' +
        segment.accountName;
      return '<span class="pk-expiry-seg" style="--seg-color:' + color +
        ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
        ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
        ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
    }).join('');
    return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? '今天到期' : row.days + ' 天') +
      '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
      '</b></div>';
  }).join('');
  const foot = summary.accountCount + ' 个账号' +
    (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '');
  const legend = (list || []).filter(a =>
    a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
  ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
    '"></i>' + esc(a.nickname || String(a.uid)) + '</span>').join('');
  const hdr = '<div class="pk-expiry-hdr"><span>剩余天数</span><span style="text-align:center">各账号该批剩余</span><b>剩余积分</b></div>';
  $('pkExpiry').innerHTML = (rows
    ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
    : '<div class="pk-expiry-empty">暂无将到期积分（每日赠送次日到期；长期积分不到期、不入图）</div>') +
    (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
    '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function pkDetailCompare(a, b) {
  const sizeOf = p => { const n = Number(p && p.size); return Number.isFinite(n) ? n : 0; };
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
  if (ea == null && eb != null) return 1;
  if (ea != null && eb == null) return -1;
  if (ea != null && eb != null && ea !== eb) return ea - eb;
  return sizeOf(b) - sizeOf(a);
}
function pkDetailGroups(packs) {
  const active = [], used = [];
  let usedSize = 0;
  for (const p of packs || []) {
    const remain = Number(p && p.remain);
    if (remain > 0) { active.push(p); continue; }
    used.push(p);
    const size = Number(p && p.size);
    if (Number.isFinite(size)) usedSize += size;
  }
  active.sort(pkDetailCompare);
  used.sort(pkDetailCompare);
  return { active, used, usedSize };
}

function renderPackages(d) {
  const list = (d.accounts || []);
  const now = Date.now();
  renderExpiryDistribution(list, now);
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    $('pkDetail').innerHTML = '';
    return;
  }
  const names = [];
  for (const a of list) for (const s of pkBySource(a.packages || [])) {
    if (!names.includes(s.key)) names.push(s.key);
  }
  names.sort((x, y) => {
    const sz = n => Math.max(...list.map(a => {
      const f = pkBySource(a.packages || []).find(s => s.key === n);
      return f ? f.size : 0;
    }));
    return sz(y) - sz(x);
  });
  const colorOf = n => pkColor(names.indexOf(n));
  const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' + esc(a.nickname || a.uid) + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const srcs = pkBySource(a.packages || []);
    const total = Math.max(1, Number(a.size || 0));
    const bar = srcs.map(s =>
      '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' + colorOf(s.key) +
      '" title="' + esc(s.name) + ' ' + fmtPoints(s.size) + '"></i>').join('');
    const legend = srcs.map(s =>
      '<span><i style="background:' + colorOf(s.key) + '"></i>' + esc(s.name) + ' ×' + s.n +
      ' · ' + fmtPoints(s.size) + (s.minCreated ? ' · 首发 ' + esc(s.minCreated.slice(5)) : '') + '</span>').join('');
    const inv = (a.invitation_codes || []).map(c =>
      '<span class="tag mute" title="邀请码 ' + esc(c.inviteCode || '') + ' 已用 ' + (c.usedCount || 0) + '/' + (c.maxUses || 0) + '">' +
      esc(c.inviteCode || '') + '</span>').join(' ');
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + remarkName(a.uid, esc(a.nickname || a.uid)) + '</span>' +
      (a.daily_cycle_date ? '<span class="realm">' + esc(a.daily_cycle_date) + (a.checkin_done ? ' · 已签到' : ' · 未签到') + '</span>' : '') + '</div>' +
      '<div class="big">' + fmtPoints(a.remain) + '</div>' +
      '<div class="sub">共 ' + fmtPoints(a.size) + ' · ' + (a.packages || []).length + ' 个口径 · 占最高 ' +
      (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
      '<div class="mixbar">' + bar + '</div>' +
      '<div class="pk-legend">' + legend + '</div>' +
      (inv ? '<div class="pk-legend">' + inv + '</div>' : '') +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游';

  $('pkDetail').innerHTML = list.map(a => {
    if (a.error) return '';
    const groups = pkDetailGroups(a.packages || []);
    const rowOf = p => {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      return '<tr><td class="mark" aria-hidden="true"><i style="background:' + colorOf(k) + '"></i></td>' +
        '<td>' + esc(p.name || '(未命名)') + '<div class="note">' + esc(p.package_code || '') + '</div></td>' +
        '<td class="num">' + fmtPoints(p.size) + '</td>' +
        '<td class="num">' + fmtPoints(p.remain) + '</td>' +
        '<td class="num">' + fmtPoints(p.used) + '</td>' +
        '<td class="num">' + esc(String(p.created_at || '').slice(0, 16) || '—') + '</td>' +
        '<td class="num">' + esc(String(p.end_time || '').slice(0, 10) || '不到期') + '</td></tr>';
    };
    const usedNote = groups.used.length
      ? '<tr class="pk-group-summary"><td colspan="7">已入账来源 ' + groups.used.length + ' 项（面额合计 ' + fmtPoints(groups.usedSize) + '，余额已并入上方口径，不重复计数）</td></tr>' +
        groups.used.map(rowOf).join('')
      : '';
    return '<div class="box"><header><h3>' + remarkName(a.uid, esc(a.nickname || a.uid)) + '</h3><span class="grow"></span>' +
      '<span class="note">可用 ' + fmtPoints(a.remain) + ' / 历史总额 ' + fmtPoints(a.size) +
      ' · 每日 ' + esc(a.daily_cycle_date || '—') + '</span></header>' +
      '<div class="tbl-wrap"><table class="acc"><thead><tr>' +
      '<th class="mark" aria-hidden="true"></th><th>口径 / 来源</th>' +
      '<th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>' +
      '<th class="num">发放</th><th class="num">到期</th>' +
      '</tr></thead><tbody>' + groups.active.map(rowOf).join('') + usedNote + '</tbody></table></div></div>';
  }).join('');
}

// 按需拉取（蓝本 8391a05）：packages 是**逐账号实时查上游**，切进本视图就打一次，
// 账号一多既慢又压上游。有缓存先直接渲染（标注数据年龄），只有首次进入或显式刷新生效。
let lastPackages = null, lastPackagesAt = 0;
function invalidatePackages() { lastPackages = null; lastPackagesAt = 0; }

async function loadPackages(force) {
  if (!force && lastPackages) {
    renderPackages(lastPackages);
    const ageMin = Math.floor((Date.now() - lastPackagesAt) / 60000);
    if (ageMin > 0) $('pkNote').textContent = lastPackages.accounts.length + ' 个账号 · ' + ageMin + ' 分钟前的数据，点「刷新」更新';
    return;
  }
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  $('pkDetail').innerHTML = '';
  $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">查询中…</div>';
  try {
    const d = await api('packages');
    lastPackages = d;
    lastPackagesAt = Date.now();
    renderPackages(d);
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
    $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">读取失败：' + esc(e.message) + '</div>';
  }
}

// 「刷新」始终实时查：必须显式 force —— 直接 `onclick = loadPackages` 会把事件对象
// 当真值传进去，恒等于强刷。
$('btnPk').onclick = () => loadPackages(true);

/* ══ 账号备注（存后端 auths/<账号>.json 的 remark 键；账号池 / 用量 / 积分构成共用）══
   备注是纯展示信息，不参与选号 / 计费 / 鉴权，但**属于账号资料**：换浏览器、换入口
   （127.0.0.1 与局域网 IP 是两个 origin）、清站点数据都不该丢，所以跟着账号文件走。
   前端只在内存里按 name 建索引（remarksAll），真值一律以 overview 下发的 remark 为准。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  const map = {};
  for (const s of (overviewData && overviewData.accounts) || []) {
    const r = String(s.remark || '');
    if (r) map[String(s.name || s.uid || '')] = r;
  }
  remarksCache = map;
  return map;
}
function remarkOf(key) {
  key = String(key == null ? '' : key);
  return key ? String(remarksAll()[key] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(key, nameHtml) {
  const r = remarkOf(key);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
/* invalidateRemarks 只丢内存索引：真值在后端，重新拉 overview 就能拿回最新的。 */
function invalidateRemarks() { remarksCache = null; }
function remarkFormRows() {
  const rows = ((overviewData && overviewData.accounts) || [])
    .map(s => ({ uid: String(s.name || ''), who: s.name || '未命名' }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  $('remarkVeil').classList.add('on');
}
function closeRemarks() { $('remarkVeil').classList.remove('on'); }
/* saveRemarkForm 只提交真正改动过的账号：一个号一条 POST，失败逐条报，
   不让「其中一个写得进、另一个写不进」被一句「已保存」盖掉。 */
async function saveRemarkForm() {
  const prev = remarksAll();
  const changed = Array.from(document.querySelectorAll('#remarkBody input[data-uid]'))
    .map(inp => ({ name: inp.getAttribute('data-uid'), remark: inp.value.trim() }))
    .filter(it => (prev[it.name] || '') !== it.remark);
  if (!changed.length) { closeRemarks(); toast('备注无改动', 'ok'); return; }
  const btn = $('btnRemarkSave');
  btn.disabled = true;
  const errs = [];
  let ok = 0;
  try {
    for (const it of changed) {
      try {
        await api('accounts/' + encodeURIComponent(it.name) + '/remark',
          { method: 'POST', body: JSON.stringify({ remark: it.remark }) });
        ok++;
      } catch (e) { errs.push(it.name + '：' + e.message); }
    }
  } finally { btn.disabled = false; }
  invalidateRemarks();
  closeRemarks();
  // 备注已落盘到账号文件：必须重新拉 overview 才能拿到后端真值（本地不再缓存）。
  await loadOverview(true);
  if (view === 'usage' && usageData) renderUsage(usageData);
  if (errs.length) toast('备注保存失败 ' + errs.length + ' 个 — ' + errs[0], 'err');
  else toast('备注已保存（' + ok + ' 个账号）', 'ok');
}
$('btnRemarks').onclick = openRemarks;
$('btnRemarkClose').onclick = closeRemarks;
/* 「清空全部」= 把当前有备注的账号逐个置空（后端以空串表示清除），
   而不是只清本地索引——否则刷新页面备注会全部回来。 */
$('btnRemarkClear').onclick = async () => {
  const names = Object.keys(remarksAll());
  if (!names.length) { renderRemarkBody(); return; }
  const btn = $('btnRemarkClear');
  btn.disabled = true;
  const errs = [];
  try {
    for (const name of names) {
      try {
        await api('accounts/' + encodeURIComponent(name) + '/remark',
          { method: 'POST', body: JSON.stringify({ remark: '' }) });
      } catch (e) { errs.push(name + '：' + e.message); }
    }
  } finally { btn.disabled = false; }
  invalidateRemarks();
  await loadOverview(true);
  renderRemarkBody();
  if (errs.length) toast('清除失败 ' + errs.length + ' 个 — ' + errs[0], 'err');
  else toast('已清除全部备注', 'ok');
};
$('btnRemarkSave').onclick = saveRemarkForm;
