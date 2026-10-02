/* ============================================================
 * core.js — 全局状态 / 鉴权 / 外壳 / 通用组件
 * 页面模块（nodes / stats / logs）通过 registerPage 注册，
 * 操作按钮统一用 data-act，由 registerActions 注册处理函数。
 * ============================================================ */

let SAVED_TOKEN = localStorage.getItem('ep_token') || '';
let authGeneration = 0;
let loginInFlight = false;
let AUTH_STATUS = null;
let appBuild = null;
let currentTab = 'nodes';

const PAGES = {};
const ACTIONS = {};
const IMPERSONATE_LABELS = { yamby: 'Yamby Android', hills_android: 'Hills Android', hills_windows: 'Hills Windows' };

function registerPage(name, page) { PAGES[name] = page; }
function registerActions(map) { Object.assign(ACTIONS, map); }

/* ============================================================
 * 小工具
 * ============================================================ */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

function esc(s) {
  // 注意不能写 String(s||'')：那样数字 0 和 false 会被当成空值渲染成空白。
  return (s === null || s === undefined ? '' : String(s))
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function attr(s) {
  return esc(s).replace(/'/g, '&#39;');
}

function ico(id, cls = '') {
  return `<svg class="${cls}" aria-hidden="true"><use href="#${id}"/></svg>`;
}

/* opts.onCopied：复制成功后的回调；opts.quiet：不弹提示条（由调用方就地反馈） */
function copyText(text, title = '已复制', detail = '', opts = {}) {
  const done = () => { opts.onCopied?.(); if (!opts.quiet) toast('ok', title, detail); };
  const fallback = () => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let copied = false;
    try { copied = document.execCommand('copy'); } catch (_) { copied = false; }
    ta.remove();
    if (copied) done();
    else toast('bad', '复制失败', '浏览器拒绝访问剪贴板，请手动复制：' + text);
  };
  // 非 HTTPS（如局域网 IP 访问）下没有 navigator.clipboard，走 execCommand 兜底。
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fallback);
  else fallback();
}

function downloadJson(filename, data) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function formatStamp(d) {
  const p = n => String(n).padStart(2, '0');
  const utc8 = new Date(d.getTime() + 8 * 60 * 60 * 1000);
  return `${utc8.getUTCFullYear()}${p(utc8.getUTCMonth() + 1)}${p(utc8.getUTCDate())}-${p(utc8.getUTCHours())}${p(utc8.getUTCMinutes())}${p(utc8.getUTCSeconds())}`;
}

/* 十进制单位（1 MB = 10^6 B），与后端容量上限、Telegram 日报的换算口径一致 */
function formatBytes(bytes) {
  const n = Math.max(0, Number(bytes || 0));
  if (n < 1000) return `${Math.round(n)} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n / 1000;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  return `${v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)} ${units[i]}`;
}

/* 数值与单位拆开，供指标卡把单位渲染成小字 */
function bytesParts(bytes) {
  const [v, u] = formatBytes(bytes).split(' ');
  return [v, u];
}

function formatCount(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}

function formatPlaybackDuration(milliseconds) {
  const minutes = Math.round(Math.max(0, Number(milliseconds || 0)) / 60000);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 24) return rest ? `${hours} 小时 ${rest} 分钟` : `${hours} 小时`;
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  return restHours ? `${days} 天 ${restHours} 小时` : `${days} 天`;
}

/* 相对时长：「38 秒」「12 分钟」「3 小时」「2 天」 */
function formatSpan(ms) {
  const s = Math.max(0, Math.floor(Number(ms || 0) / 1000));
  if (s < 60) return `${s} 秒`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} 分钟`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} 小时`;
  return `${Math.floor(h / 24)} 天`;
}

function formatAgo(at) {
  if (!at) return '—';
  return formatSpan(Date.now() - at) + '前';
}

const BJ_TIME = new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', hour: '2-digit', minute: '2-digit', hour12: false });
const BJ_DATETIME = new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
const BJ_FULL = new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
function fmtTime(t) { return BJ_TIME.format(new Date(t)); }
function fmtDateTime(t) { return BJ_DATETIME.format(new Date(t)); }
function fmtFull(t) { return BJ_FULL.format(new Date(t)); }

function fmtPct(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—';
  return v >= 1 ? '100' : (v * 100).toFixed(2);
}

function hostOf(url) {
  try { return new URL(url).host; } catch (_) { return url; }
}

function byteLen(s) {
  return new TextEncoder().encode(String(s || '')).length;
}

function debounce(fn, ms) {
  let t = null;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
}

/* ============================================================
 * 状态呈现：颜色只是第三重编码，始终配合图标与文字
 * ============================================================ */

const STATUS = {
  ok: { label: '正常', icon: 'i-ok', dot: 'd-ok' },
  warn: { label: '降级', icon: 'i-warn', dot: 'd-warn' },
  bad: { label: '故障', icon: 'i-bad', dot: 'd-bad' },
  idle: { label: '未检测', icon: 'i-idle', dot: 'd-idle' },
  checking: { label: '检测中', icon: 'i-loader', dot: 'd-idle' },
};

function statusPill(st, label) {
  const s = STATUS[st] || STATUS.idle;
  return `<span class="status ${st}">${ico(s.icon)}${esc(label || s.label)}</span>`;
}

function statusDot(st) {
  if (st === 'checking') {
    return `<span class="dot checking" title="检测中"><svg viewBox="0 0 10 10" style="animation:spin 1s linear infinite"><path d="M5 1a4 4 0 1 0 4 4" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg></span>`;
  }
  const s = STATUS[st] || STATUS.idle;
  return `<span class="dot ${st}" title="${s.label}"><svg><use href="#${s.dot}"/></svg></span>`;
}

/* ============================================================
 * 提示条
 * ============================================================ */

function toast(kind, title, text = '') {
  const root = $('#toasts');
  if (!root) return;
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.setAttribute('role', kind === 'bad' ? 'alert' : 'status');
  el.innerHTML = `${ico(kind === 'ok' ? 'i-ok' : kind === 'bad' ? 'i-bad' : kind === 'warn' ? 'i-warn' : 'i-info')}<div><b></b>${text ? '<span></span>' : ''}</div>`;
  el.querySelector('b').textContent = title;
  if (text) el.querySelector('span').textContent = text;
  root.appendChild(el);
  while (root.children.length > 4) root.firstElementChild.remove();
  setTimeout(() => el.remove(), kind === 'bad' ? 6000 : 3500);
}

/* ============================================================
 * 请求
 * ============================================================ */

async function authFetch(path, options = {}) {
  try {
    const response = await fetch(path, { ...options, credentials: 'same-origin' });
    let data;
    try {
      data = await response.json();
    } catch (_) {
      data = { ok: false, error: 'INVALID_RESPONSE' };
    }
    data._status = response.status;
    return data;
  } catch (e) {
    return { ok: false, error: e.message || 'NETWORK_ERROR', _status: 0 };
  }
}

async function api(action, data = {}) {
  const generation = authGeneration;
  const r = await authFetch('/admin/api', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, ...data }),
  });

  if (generation !== authGeneration) return { ok: false, error: '', _status: 401, _stale: true };
  if (handleSessionAuthFailure(r, generation)) return { ok: false, error: '', _status: 401, _stale: true };
  return r;
}

function handleSessionAuthFailure(r, generation) {
  if (generation !== authGeneration || r?._status !== 401 || (r.error !== 'UNAUTHORIZED' && r.error !== 'SESSION_REQUIRED')) return false;
  authGeneration++;
  AUTH_STATUS = null;
  showLogin('登录会话已过期，请重新登录');
  return true;
}

/* 接口错误转成可读文案；401 已由 handleSessionAuthFailure 处理，不再提示 */
function apiError(r, fallback = '请求失败') {
  if (!r) return fallback;
  if (r._status === 0) return '无法连接服务器，请检查网络';
  return r.error || fallback;
}

/* ============================================================
 * 鉴权
 * ============================================================ */

function setLoginHint(text = '') {
  const el = document.getElementById('loginHint');
  if (!el) return;
  el.textContent = text;
  el.hidden = !text;
}

async function restoreSession() {
  const generation = ++authGeneration;
  const r = await authFetch('/admin/auth/status');
  if (generation !== authGeneration) return;
  if (r.ok && r.authenticated) {
    applyAuthStatus(r.twoFactor);
    initApp();
    return;
  }
  if (SAVED_TOKEN) {
    const saved = SAVED_TOKEN;
    const auto = await authFetch('/admin/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: saved, code: '' }),
    });
    if (generation !== authGeneration) return;
    if (auto.ok) {
      applyAuthStatus(auto.twoFactor);
      initApp();
      return;
    }
    if (auto.error === 'TOTP_REQUIRED') {
      clearSavedToken();
      document.getElementById('tokenInput').value = saved;
      document.getElementById('totpLoginWrap').hidden = false;
      setLoginHint('已检测到 2FA，请输入验证器中的 6 位验证码');
      showLoginError('请输入动态验证码');
      document.getElementById('totpInput').focus();
      return;
    }
    clearSavedToken();
  }
  showLogin();
}

async function doLogin() {
  if (loginInFlight) return;
  const tokenInput = document.getElementById('tokenInput');
  const codeInput = document.getElementById('totpInput');
  const token = tokenInput.value.trim();
  const code = codeInput.value.trim();
  if (!token) {
    showLoginError('请输入管理员 Token');
    return;
  }
  const button = document.getElementById('loginButton');
  loginInFlight = true;
  button.disabled = true;
  const generation = ++authGeneration;
  try {
    const r = await authFetch('/admin/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token, code }),
    });
    if (generation !== authGeneration) return;
    if (r.ok) {
      persistTokenForStatus(token, r.twoFactor);
      tokenInput.value = '';
      codeInput.value = '';
      document.getElementById('totpLoginWrap').hidden = true;
      document.getElementById('loginErr').style.display = 'none';
      applyAuthStatus(r.twoFactor);
      initApp();
      return;
    }
    if (r.error === 'TOTP_REQUIRED') {
      clearSavedToken();
      document.getElementById('totpLoginWrap').hidden = false;
      setLoginHint('Token 正确，请输入验证器中的 6 位验证码');
      showLoginError('请输入动态验证码');
      codeInput.focus();
      return;
    }
    if (r.twoFactor?.configured) clearSavedToken();
    showLoginError(authErrorMessage(r.error));
  } finally {
    loginInFlight = false;
    button.disabled = false;
  }
}

async function doLogout() {
  const r = await authFetch('/admin/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
  if (!r.ok) {
    toast('bad', '退出登录失败', authErrorMessage(r.error));
    return;
  }
  authGeneration++;
  clearSavedToken();
  AUTH_STATUS = null;
  showLogin();
}

function showLogin(message = '') {
  resetProtectedUI();
  document.getElementById('app').style.display = 'none';
  document.getElementById('loginWrap').style.display = '';
  document.getElementById('totpLoginWrap').hidden = true;
  document.getElementById('totpInput').value = '';
  if (!document.getElementById('tokenInput').value && SAVED_TOKEN) document.getElementById('tokenInput').value = SAVED_TOKEN;
  setLoginHint();
  if (message) showLoginError(message);
  else document.getElementById('loginErr').style.display = 'none';
}

/* 会话失效时清掉所有弹窗、定时器和表单里的敏感输入 */
function resetProtectedUI() {
  closeAllLayers();
  Object.values(PAGES).forEach(page => { try { page.reset?.(); } catch (_) { /* 页面自行容错 */ } });
}

function showLoginError(message) {
  const error = document.getElementById('loginErr');
  error.innerHTML = `${ico('i-bad')}<span></span>`;
  error.querySelector('span').textContent = message;
  error.style.display = 'flex';
}

function authErrorMessage(error) {
  const messages = {
    INVALID_CREDENTIALS: 'Token 或动态验证码错误，请重试',
    TOTP_REQUIRED: '请输入动态验证码',
    UNAUTHORIZED: '登录会话无效，请重新登录',
    TOO_MANY_REQUESTS: '失败次数过多，请稍后再试',
    TWO_FACTOR_UNAVAILABLE: '2FA 配置无法读取，请设置 ADMIN_2FA_DISABLED=true 并重启后恢复',
    CROSS_SITE_REQUEST: '请求来源校验失败，请从当前管理页面重试',
    SESSION_REQUIRED: '该操作需要重新登录',
    SETUP_EXPIRED: '绑定二维码已过期，请重新生成',
  };
  return messages[error] || '登录失败，请重试';
}

function initApp() {
  document.getElementById('loginWrap').style.display = 'none';
  document.getElementById('app').style.display = '';
  updateBreakpoint();
  switchTab('nodes');
  Object.values(PAGES).forEach(page => { try { page.afterLogin?.(); } catch (_) { /* 单个模块失败不影响其他模块 */ } });
}

function applyAuthStatus(twoFactor) {
  AUTH_STATUS = {
    configured: !!twoFactor?.configured,
    enforced: !!twoFactor?.enforced,
    emergencyDisabled: !!twoFactor?.emergencyDisabled,
    enrolledAt: Number(twoFactor?.enrolledAt || 0),
  };
  if (AUTH_STATUS.configured) clearSavedToken();
  document.getElementById('emergencyBanner').hidden = !AUTH_STATUS.emergencyDisabled;
}

function acceptRotatedSession(twoFactor) {
  authGeneration++;
  applyAuthStatus(twoFactor);
}

function persistTokenForStatus(token, twoFactor) {
  if (twoFactor?.configured) {
    clearSavedToken();
    return;
  }
  SAVED_TOKEN = String(token || '').trim();
  if (SAVED_TOKEN) localStorage.setItem('ep_token', SAVED_TOKEN);
}

function clearSavedToken() {
  SAVED_TOKEN = '';
  localStorage.removeItem('ep_token');
}

async function refreshAuthStatus(showToast = false) {
  const generation = authGeneration;
  const r = await authFetch('/admin/auth/status');
  if (generation !== authGeneration) return false;
  if (!r.ok) {
    if (handleSessionAuthFailure(r, generation)) return false;
    else if (showToast) toast('bad', '读取 2FA 状态失败', authErrorMessage(r.error));
    return false;
  }
  applyAuthStatus(r.twoFactor);
  return true;
}

/* ============================================================
 * 外壳：页签、版本号、断点
 * ============================================================ */

function switchTab(name) {
  if (!PAGES[name]) return;
  const prev = currentTab;
  if (prev !== name) PAGES[prev]?.leave?.();
  currentTab = name;
  $$('#mainNav [data-tab]').forEach(el => {
    if (el.dataset.tab === name) el.setAttribute('aria-current', 'page');
    else el.removeAttribute('aria-current');
  });
  $$('#main > .page').forEach(el => { el.hidden = el.dataset.page !== name; });
  PAGES[name].enter?.();
}

function setAppVersion(build) {
  appBuild = build || null;
  const el = document.getElementById('appVersion');
  if (!el) return;
  const version = build?.version || 'dev';
  el.textContent = version;
  const parts = [`版本 ${version}`];
  if (build?.commit) parts.push(`Commit ${build.commit}`);
  if (build?.builtAt) parts.push(`构建于 ${build.builtAt}`);
  el.title = parts.join(' · ');
}

/* 断点按容器宽度判断：wide ≥ 1360 / mid ≥ 1024 / tablet ≥ 720 / phone */
const BREAKPOINT = w => w >= 1360 ? 'wide' : w >= 1024 ? 'mid' : w >= 720 ? 'tablet' : 'phone';
function updateBreakpoint() {
  const app = document.getElementById('app');
  const bp = BREAKPOINT(window.innerWidth);
  if (app.dataset.bp === bp) return false;
  app.dataset.bp = bp;
  return true;
}
function currentBreakpoint() { return document.getElementById('app').dataset.bp; }
function isNarrow() { const bp = currentBreakpoint(); return bp === 'phone' || bp === 'tablet'; }

/* ============================================================
 * 浮层：弹窗、确认框、菜单
 * 弹窗渲染在 #overlay，确认框叠在 #overlay2。
 * 弹窗由各模块用模板重绘；入场动画只在首次打开时播放，
 * 同一标签页内重绘时保留内容区滚动位置。
 * ============================================================ */

let activeDialog = null; // { id, onClose, onDispose }

/* onClose：用户主动关闭时调用（可以先确认）；onDispose：弹窗被关闭或被别的弹窗替换时清理状态 */
/* 弹窗打开时背后的界面设为 inert：Tab 不会跑到导航上，也不会绕过「放弃修改」的确认 */
function syncInert() {
  const shell = document.querySelector('#app .shell');
  const overlay = document.getElementById('overlay');
  const hasDialog = !!overlay?.firstElementChild;
  const hasConfirm = !!document.getElementById('overlay2')?.firstElementChild;
  if (shell) shell.inert = hasDialog || hasConfirm;
  if (overlay) overlay.inert = hasConfirm;
}

function renderDialog(id, html, { scrollKey = id, onClose = null, onDispose = null } = {}) {
  const host = document.getElementById('overlay');
  const reopening = activeDialog && activeDialog.id === id;
  if (activeDialog && !reopening) { const dispose = activeDialog.onDispose; activeDialog = null; dispose?.(); }
  const body = host.querySelector('.dialog-body');
  const keepTop = reopening && body && body.dataset.scrollKey === String(scrollKey) ? body.scrollTop : 0;
  const focusId = document.activeElement && host.contains(document.activeElement) ? document.activeElement.id : '';
  const focusPos = focusId ? document.activeElement.selectionStart : null;
  host.innerHTML = `<div class="scrim" data-act="dialog-dismiss"></div>${html}`;
  const dialog = host.querySelector('.dialog');
  if (dialog && !reopening) dialog.classList.add('enter');
  const nb = host.querySelector('.dialog-body');
  if (nb) { nb.dataset.scrollKey = String(scrollKey); nb.scrollTop = keepTop; }
  if (focusId) {
    const el = document.getElementById(focusId);
    if (el) { el.focus(); try { if (focusPos !== null) el.setSelectionRange(focusPos, focusPos); } catch (_) { /* 部分输入类型不支持选区 */ } }
  }
  activeDialog = { id, onClose: onClose || (reopening ? activeDialog.onClose : null), onDispose: onDispose || (reopening ? activeDialog.onDispose : null) };
  syncInert();
  if (!reopening) {
    const first = host.querySelector('[autofocus]') || host.querySelector('.dialog-body input:not([type=hidden]):not([disabled]), .dialog-body select, .dialog-body textarea');
    if (first && !focusId) setTimeout(() => first.focus({ preventScroll: true }), 30);
  }
}

function closeDialog() {
  document.getElementById('overlay').innerHTML = '';
  const dispose = activeDialog?.onDispose;
  activeDialog = null;
  syncInert();
  dispose?.();
}

function dialogOpen(id) { return activeDialog?.id === id; }

/* 用户主动关闭（点遮罩、Esc、关闭按钮）时，交给弹窗自己的 onClose 决定是否需要确认 */
function requestCloseDialog() {
  if (!activeDialog) return;
  if (activeDialog.onClose) activeDialog.onClose();
  else closeDialog();
}

let confirmResolve = null;
function confirmDialog({ title, text, ok = '确认', danger = false, cancel = '取消' }) {
  return new Promise(resolve => {
    if (confirmResolve) confirmResolve(false);
    confirmResolve = resolve;
    const host = document.getElementById('overlay2');
    host.innerHTML = `<div class="scrim" data-act="confirm-cancel"></div>
      <div class="modal" role="alertdialog" aria-modal="true" aria-labelledby="confirmTitle" aria-describedby="confirmText">
        <h3 id="confirmTitle"></h3><p id="confirmText"></p>
        <div class="mfoot"><button class="btn" data-act="confirm-cancel"></button><button class="btn ${danger ? 'btn-danger' : 'btn-primary'}" data-act="confirm-ok" id="confirmOk"></button></div>
      </div>`;
    $('#confirmTitle', host).textContent = title;
    if (text instanceof Node) $('#confirmText', host).appendChild(text);
    else $('#confirmText', host).textContent = text || '';
    $('[data-act="confirm-cancel"].btn', host).textContent = cancel;
    $('#confirmOk', host).textContent = ok;
    if (danger) $('#confirmOk', host).style.cssText = 'background:var(--bad-wash);border-color:rgba(240,100,108,.5)';
    syncInert();
    $('#confirmOk', host).focus();
  });
}

function settleConfirm(value) {
  document.getElementById('overlay2').innerHTML = '';
  syncInert();
  const resolve = confirmResolve;
  confirmResolve = null;
  if (resolve) resolve(value);
}

let openMenuEl = null;
/* items: [{ act, icon, label, hint?, danger?, data? } | '-'] */
function openMenu(anchor, items) {
  closeMenu();
  const m = document.createElement('div');
  m.className = 'menu';
  m.setAttribute('role', 'menu');
  m.innerHTML = items.map(it => it === '-' ? '<hr>' :
    `<button role="menuitem" data-act="${attr(it.act)}" class="${it.danger ? 'danger' : ''}" ${Object.entries(it.data || {}).map(([k, v]) => `data-${k}="${attr(v)}"`).join(' ')}>${ico(it.icon)}<span></span>${it.hint ? `<span class="hint">${esc(it.hint)}</span>` : ''}</button>`).join('');
  items.filter(it => it !== '-').forEach((it, i) => { m.querySelectorAll('button')[i].querySelector('span').textContent = it.label; });
  const host = anchor.parentElement;
  host.style.position = 'relative';
  host.appendChild(m);
  m.style.right = '0';
  m.style.top = 'calc(100% + 6px)';
  m._anchor = anchor;
  openMenuEl = m;
  anchor.setAttribute('aria-expanded', 'true');
  m.querySelector('button')?.focus();
}

function closeMenu() {
  if (!openMenuEl) return;
  openMenuEl._anchor?.setAttribute('aria-expanded', 'false');
  openMenuEl.remove();
  openMenuEl = null;
}

function closeAllLayers() {
  closeMenu();
  settleConfirm(false);
  closeDialog();
}

/* ============================================================
 * 全局事件
 * ============================================================ */

registerActions({
  'dialog-dismiss': () => requestCloseDialog(),
  'dialog-close': () => requestCloseDialog(),
  'confirm-ok': () => settleConfirm(true),
  'confirm-cancel': () => settleConfirm(false),
  logout: () => doLogout(),
});

document.addEventListener('click', e => {
  const target = e.target;
  if (!target || !target.closest) return;
  const actEl = target.closest('[data-act]');
  const tabEl = target.closest('[data-tab]');
  if (openMenuEl && !target.closest('.menu')) {
    const anchor = openMenuEl._anchor;
    closeMenu();
    if (anchor && actEl === anchor) return; // 再次点击同一个按钮 = 收起
  }
  if (tabEl && tabEl.closest('#mainNav')) { switchTab(tabEl.dataset.tab); return; }
  if (!actEl) return;
  const fn = ACTIONS[actEl.dataset.act];
  if (!fn) return;
  if (actEl.tagName === 'A') e.preventDefault();
  if (actEl.closest('.menu')) closeMenu();
  fn(actEl, e);
});

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    if (openMenuEl) { const a = openMenuEl._anchor; closeMenu(); a?.focus(); return; }
    if (confirmResolve) { settleConfirm(false); return; }
    if (activeDialog) { requestCloseDialog(); return; }
  }
  if (e.key === '/' && !e.target.closest('input, textarea, select') && !activeDialog) {
    const search = document.querySelector('.page:not([hidden]) [data-search]');
    if (search) { e.preventDefault(); search.focus(); }
  }
  PAGES[currentTab]?.keydown?.(e);
});

window.addEventListener('resize', debounce(() => {
  if (document.getElementById('app').style.display === 'none') return;
  const changed = updateBreakpoint();
  PAGES[currentTab]?.resize?.(changed);
}, 80));

/* 回车提交登录 */
document.getElementById('loginForm').addEventListener('submit', e => {
  e.preventDefault();
  doLogin();
});

function bootApp() {
  updateBreakpoint();
  restoreSession();
}

document.addEventListener('DOMContentLoaded', bootApp);
