/* ============================================================
 * settings.js — 系统配置 / 通知与告警 / 管理员双重验证
 * 都是居中弹窗；保存后不关闭，可以继续修改。
 * ============================================================ */

let TWO_FACTOR_ACTION = 'setup';
let TWO_FACTOR_SETUP_ID = '';
let twoFactorFlowGeneration = 0;

let configInitialTab = 'play'; // 从其他页面跳进来时要打开的标签页，例如统计页的「图片缓存 · 设置」
let G = null; // 当前设置弹窗：{ kind, tab, v, saving, dirty, savedAt, cache, capture }

function safeLogLevel(value) {
  const level = String(value || 'info').trim().toLowerCase();
  return ['silent', 'error', 'warn', 'info', 'debug'].includes(level) ? level : 'info';
}

function flagEnabled(value) {
  if (value === true) return true;
  return ['1', 'true', 'yes', 'on'].includes(String(value || '').trim().toLowerCase());
}

/* 后端用逗号存列表，表单里一行一个 */
function formatCommaList(value) {
  return String(value || '').split(',').map(x => x.trim()).filter(Boolean).join('\n');
}

function settingsFootNote() {
  if (!G || G.saving) return '';
  if (G.dirty) return `<span style="color:var(--warn)">有未保存的修改${G.kind === 'config' ? ' · 所有分组一起保存' : ''}</span>`;
  if (G.savedAt) return `<span style="color:var(--ok)">已保存 · ${esc(G.savedAt)}</span>`;
  return G.kind === 'config' ? '保存后立即生效，无需重启' : '';
}

function settingsSaveButton(label) {
  return `<button class="btn btn-primary" data-act="settings-save" ${G.saving || !G.dirty ? 'disabled' : ''} title="${G.dirty ? '' : '没有需要保存的修改'}">${G.saving ? '<span class="spin"></span>保存中…' : label}</button>`;
}

function switchRow(k, label, desc, extra = '') {
  return `<div class="toggle-row"><div class="tx"><b>${label}${extra}</b><span>${desc}</span></div><label class="switch"><input type="checkbox" id="g-${k}" data-g="${k}" ${G.v[k] ? 'checked' : ''} aria-label="${attr(label)}"><span></span></label></div>`;
}

function numberField(k, label, unit, help, min, max) {
  return `<div class="field"><label class="field-label" for="g-${k}">${label}</label><div class="input-group"><input class="input num" id="g-${k}" type="number" min="${min}" max="${max}" step="1" data-g="${k}" value="${attr(G.v[k])}"><span class="btn" style="pointer-events:none">${unit}</span></div><div class="field-help">${help}</div></div>`;
}

function disposeSettings() { G = null; }

async function requestCloseSettings() {
  if (!G || G.saving) return;
  if (G.dirty) {
    const ok = await confirmDialog({ title: '放弃未保存的修改？', text: '关闭后本次修改不会保存，已保存的内容不受影响。', ok: '放弃修改', danger: true });
    if (!ok) return;
  }
  G = null;
  closeDialog();
}

/* 文本输入只更新底部状态，不重绘整个弹窗，避免打断输入 */
function onSettingsInput(el) {
  const k = el.dataset.g;
  G.dirty = true;
  G.v[k] = el.type === 'checkbox' ? el.checked : el.type === 'number' ? el.value : el.value;
  if (el.type === 'checkbox' || el.tagName === 'SELECT') { renderSettings(); return; }
  const note = $('#overlay .dialog-foot .note');
  if (note) note.innerHTML = settingsFootNote();
  const save = $('#overlay [data-act="settings-save"]');
  if (save) { save.disabled = false; save.title = ''; }
  const close = $('#overlay .dialog-foot [data-act="dialog-close"]');
  if (close) close.textContent = '取消';
  if (G.kind === 'notify' && (k === 'token' || k === 'chat')) {
    // 填好 Token 与 Chat ID 后立即可以测试，不必先保存
    const test = $('#overlay [data-act="tg-test-menu"]');
    const filled = String(G.v.token).trim() && String(G.v.chat).trim();
    if (test && !G.testing) { test.disabled = !filled; test.title = filled ? '' : '先填写 Bot Token 与 Chat ID'; }
    const warn = $('#overlay #tgNotReady');
    if (warn) warn.hidden = !!(G.v.enabled && filled);
  }
    if (k === 'logHistoryEntriesPerFile' || k === 'logHistoryMaxFiles') {
    const hint = $('#logHistoryHint');
    if (hint) hint.textContent = logHistoryHint();
  }
}

function renderSettings() {
  if (!G) return;
  if (G.kind === 'notify') renderNotifyDialog();
  else renderConfigDialog();
}

/* ============================================================
 * 系统配置
 * ============================================================ */

async function openConfigModal() {
  if (!await refreshAuthStatus(false)) return;
  const r = await api('config.get');
  if (!r.ok || !r.config) {
    if (r._status !== 401) toast('bad', '读取系统配置失败', r.error || '响应无效');
    return;
  }
  const c = r.config;
  G = {
    kind: 'config', tab: configInitialTab, saving: false, dirty: false, savedAt: '', cache: null, capture: null,
    v: {
      logLevel: safeLogLevel(c.logLevel),
      logAccess: c.logAccess !== false,
      logHistoryEntriesPerFile: c.logHistoryEntriesPerFile ?? 2000,
      logHistoryMaxFiles: c.logHistoryMaxFiles ?? 20,
      capyStripEmby: flagEnabled(c.capyStripEmby),
      emosCompat: !!c.emosCompat,
      emosMatchHosts: formatCommaList(c.emosMatchHosts),
      emosProxyId: c.emosProxyId || '',
      emosProxyName: c.emosProxyName || '',
      imageProxyLimitEnabled: c.imageProxyLimitEnabled === true,
      imageProxyMaxConcurrent: c.imageProxyMaxConcurrent ?? 4,
      imageProxyRequestIntervalMs: c.imageProxyRequestIntervalMs ?? 250,
      imageCacheEnabled: c.imageCacheEnabled === true,
      imageCacheTtlDays: c.imageCacheTtlDays ?? 30,
      imageCacheMaxMb: c.imageCacheMaxMb ?? 2048,
      corsAllowOrigin: formatCommaList(c.corsAllowOrigin),
      externalAllowHosts: formatCommaList(c.externalAllowHosts),
      externalAllowAny: !!c.externalAllowAny,
      trustProxy: c.trustProxy !== false,
      trafficCaptureEnabled: !!c.trafficCaptureEnabled,
      trafficCaptureFile: c.trafficCaptureFile || './data/traffic-captures.jsonl',
    },
  };
  renderConfigDialog();
  refreshImageCacheStats(false);
  refreshTrafficCaptureStats(false);
}

function logHistoryHint() {
  const per = Number(G?.v.logHistoryEntriesPerFile || 0);
  const files = Number(G?.v.logHistoryMaxFiles || 0);
  return per > 0 && files > 0 ? `按当前设置，磁盘上最多保留约 ${formatCount(per * files)} 条日志。` : '填写两项后显示磁盘上最多保留的日志条数。';
}

function imageCacheCard() {
  const c = G.cache;
  const maxBytes = Math.max(0, Number(c?.maxBytes || 0));
  const ratio = c && maxBytes > 0 ? Math.min(Math.max(0, Number(c.bytes || 0)) / maxBytes, 1) : 0;
  const usage = !c ? '—' : maxBytes > 0 ? `${Math.round(ratio * 100)}%` : '不限制';
  return `<div class="statcard"><div><span>缓存大小</span><b>${c ? esc(formatBytes(c.bytes)) : '—'}</b></div><div><span>缓存条目</span><b>${c ? esc(formatCount(c.entries)) : '—'}</b></div><div><span>容量占用</span><b style="${maxBytes > 0 && ratio > 0.9 ? 'color:var(--warn)' : ''}">${usage}</b></div></div>
    <div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn btn-sm" data-act="cfg-cache-refresh">${ico('i-probe')}刷新状态</button><button class="btn btn-sm btn-danger" data-act="cfg-cache-clear" id="imageCacheClearBtn">${ico('i-trash')}清理缓存</button></div>`;
}

function captureCard() {
  const c = G.capture;
  const stages = c?.stages || [];
  return `<div class="statcard" style="grid-template-columns:repeat(2,minmax(0,1fr))"><div><span>记录大小</span><b>${c ? esc(formatBytes(c.bytes)) : '—'}</b></div><div><span>记录条数</span><b>${c ? esc(formatCount(c.records)) : '—'}</b></div></div>
    ${stages.length ? `<div class="kv" style="padding:0">${stages.map(s => `<dt>${esc(String(s.stage || '').trim() || '未分类')}</dt><dd><span class="mono faint" style="font-size:12px">${esc(formatBytes(s.bytes))} · ${esc(formatCount(s.records))} 条</span> <button class="btn btn-ghost btn-sm btn-danger" data-act="cfg-capture-clear-stage" data-stage="${attr(s.stage)}">清除</button></dd>`).join('')}</div>` : '<div class="field-help">暂无代理流量记录。</div>'}
    <div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn btn-sm" data-act="cfg-capture-refresh">${ico('i-probe')}刷新状态</button><button class="btn btn-sm btn-danger" data-act="cfg-capture-clear" id="trafficCaptureClearBtn">${ico('i-trash')}清空记录</button></div>`;
}

function twoFactorCard() {
  const s = AUTH_STATUS || {};
  let state;
  let title;
  let text;
  if (s.configured && s.emergencyDisabled) {
    state = 'emergency';
    title = '双重验证已临时停用';
    text = '当前只验证管理员 Token。可重新绑定或关闭 2FA，完成后请移除环境变量并重启。';
  } else if (s.configured) {
    state = 'enabled';
    title = '双重验证保护中';
    text = `所有管理入口均要求动态验证码，绑定时间：${s.enrolledAt ? fmtFull(s.enrolledAt) : '未知时间'}`;
  } else {
    state = 'disabled';
    title = '为后台增加第二道保护';
    text = s.emergencyDisabled ? '紧急停用变量已生效，但当前没有已保存的 2FA 绑定。' : '开启后，登录时除管理员 Token 外，还需要验证器生成的 6 位动态验证码。';
  }
  const style = state === 'enabled' ? '' : state === 'emergency'
    ? 'border-color:rgba(233,177,60,.35);background:var(--warn-wash)'
    : 'border-color:var(--line-strong);background:var(--bg-sunken)';
  const iconStyle = state === 'enabled' ? '' : state === 'emergency' ? 'color:var(--warn)' : 'color:var(--text-3)';
  return `<div class="secbox" id="twoFactorCard" data-state="${state}" style="${style}"><svg style="${iconStyle}" aria-hidden="true"><use href="#${state === 'enabled' ? 'i-shield' : state === 'emergency' ? 'i-warn' : 'i-lock'}"/></svg><div class="tx"><b>${esc(title)}</b><span>${esc(text)}</span></div>
    <button class="btn btn-sm" data-act="cfg-2fa-setup">${s.configured ? '重新绑定' : '立即开启'}</button>${s.configured ? `<button class="btn btn-sm btn-ghost btn-danger" data-act="cfg-2fa-disable">关闭 2FA</button>` : ''}</div>`;
}

function renderConfigDialog() {
  const v = G.v;
  const tabs = [['play', '播放兼容'], ['image', '图片'], ['log', '日志'], ['security', '安全访问'], ['diag', '运维诊断']];
  let body = '';
  if (G.tab === 'play') {
    body = `<div class="fgroup" id="config-play"><div class="fgroup-title"><span>客户端兼容</span></div><div>
        ${switchRow('capyStripEmby', '兼容 CapyPlayer 播放器', '改写部分播放请求头，使 CapyPlayer 能通过代理正常播放。')}
        ${switchRow('emosCompat', 'EMOS 兼容模式', '按匹配域名改写 EMOS 相关请求。')}
      </div>
      ${v.emosCompat ? `<div class="field"><label class="field-label" for="g-emosMatchHosts">EMOS 匹配域名</label><textarea class="input mono" id="g-emosMatchHosts" rows="3" data-g="emosMatchHosts" placeholder="emos.example.com&#10;media.example.net" spellcheck="false">${esc(v.emosMatchHosts)}</textarea><div class="field-help">每行一个。</div></div>
        <div class="frow"><div class="field"><label class="field-label" for="g-emosProxyId">EMOS Proxy ID</label><input class="input mono" id="g-emosProxyId" data-g="emosProxyId" value="${attr(v.emosProxyId)}" spellcheck="false"></div>
        <div class="field"><label class="field-label" for="g-emosProxyName">EMOS Proxy Name</label><input class="input" id="g-emosProxyName" data-g="emosProxyName" value="${attr(v.emosProxyName)}"></div></div>` : ''}
    </div>`;
  }
  if (G.tab === 'image') {
    body = `<div class="fgroup" id="config-image"><div class="fgroup-title"><span>图片请求</span></div>
        <div>${switchRow('imageProxyLimitEnabled', '启用图片请求限流', '限制同时回源的图片请求，避免海报墙一次打满上游。')}</div>
        ${v.imageProxyLimitEnabled ? `<div class="frow">${numberField('imageProxyMaxConcurrent', '上游并发数', '个', '1–32，默认 4。', 1, 32)}${numberField('imageProxyRequestIntervalMs', '请求间隔', '毫秒', '0–5000，默认 250。', 0, 5000)}</div>` : ''}
      </div>
      <div class="fgroup"><div class="fgroup-title"><span>图片缓存</span></div>
        <div>${switchRow('imageCacheEnabled', '启用图片缓存', '把上游图片缓存在本机，重复请求不再回源。')}</div>
        ${v.imageCacheEnabled ? `<div class="frow">${numberField('imageCacheTtlDays', '缓存保留时长', '天', '1–365，默认 30。', 1, 365)}${numberField('imageCacheMaxMb', '容量上限', 'MB', '0 表示不限，默认 2048。', 0, 1048576)}</div>` : ''}
        ${imageCacheCard()}
      </div>`;
  }
  if (G.tab === 'log') {
    body = `<div class="fgroup" id="config-log"><div class="fgroup-title"><span>输出</span></div>
        <div class="field" style="max-width:260px"><label class="field-label" for="g-logLevel">控制台输出等级</label><select class="select" id="g-logLevel" data-g="logLevel">${['silent', 'error', 'warn', 'info', 'debug'].map(x => `<option value="${x}" ${v.logLevel === x ? 'selected' : ''}>${x}</option>`).join('')}</select><div class="field-help">低于该等级的日志不输出到控制台。</div></div>
        <div>${switchRow('logAccess', '记录客户端访问日志', '每个代理请求记一条，日志页「按请求」视图依赖它。')}</div>
      </div>
      <div class="fgroup"><div class="fgroup-title"><span>落盘保留量</span></div>
        <div class="frow">${numberField('logHistoryEntriesPerFile', '单个日志文件条数', '条', '200–100000，默认 2000。', 200, 100000)}${numberField('logHistoryMaxFiles', '保留文件数', '个', '1–200，默认 20。超出后删除最旧的文件。', 1, 200)}</div>
        <div class="summary-box" id="logHistoryHint">${esc(logHistoryHint())}</div>
      </div>`;
  }
  if (G.tab === 'security') {
    body = `<div class="fgroup" id="config-access"><div class="fgroup-title"><span>管理员登录</span></div>
        ${twoFactorCard()}
      </div>
      <div class="fgroup"><div class="fgroup-title"><span>来源与外部连接</span></div>
        <div>${switchRow('trustProxy', '信任反向代理传入的真实 IP', '仅在本程序前面还有 Nginx / Caddy 等反向代理时开启，否则客户端可以伪造 IP 绕过登录限流。')}</div>
        <div class="field"><label class="field-label" for="g-corsAllowOrigin">CORS 允许来源 <span class="opt">选填</span></label><textarea class="input mono" id="g-corsAllowOrigin" rows="3" data-g="corsAllowOrigin" placeholder="留空：自动使用请求来源&#10;*：允许任意来源&#10;https://app.example.com" spellcheck="false">${esc(v.corsAllowOrigin)}</textarea><div class="field-help">每行一个，最多 50 个。</div></div>
        <div class="field"><label class="field-label" for="g-externalAllowHosts">允许的外部地址</label><textarea class="input mono" id="g-externalAllowHosts" rows="4" data-g="externalAllowHosts" placeholder="cdn.example.com&#10;media.example.net:8443&#10;*.example.com" spellcheck="false" ${v.externalAllowAny ? 'disabled' : ''}>${esc(v.externalAllowHosts)}</textarea><div class="field-help">代理可以主动连接的外部域名，支持 *.example.com 通配，每行一个，最多 200 个。</div></div>
        <div>${switchRow('externalAllowAny', '允许任意外部连接', '不再检查上面的白名单。')}</div>
        ${v.externalAllowAny ? `<div class="warnline">${ico('i-warn')}<span>开启后，任何能访问代理的人都可以让它连接任意地址，包括内网服务。只在完全可信的网络里开启。</span></div>` : ''}
      </div>`;
  }
  if (G.tab === 'diag') {
    body = `<div class="fgroup" id="config-ops"><div class="fgroup-title"><span>代理流量记录</span></div>
        <div>${switchRow('trafficCaptureEnabled', '启用代理流量记录', '把请求和响应的头部写入文件，用于排查兼容问题。排查完请关闭。')}</div>
        <div class="field"><label class="field-label" for="g-trafficCaptureFile">记录位置</label><input class="input mono" id="g-trafficCaptureFile" data-g="trafficCaptureFile" value="${attr(v.trafficCaptureFile)}" placeholder="./data/traffic-captures.jsonl" spellcheck="false"><div class="field-help">必须位于 data/ 目录下。</div></div>
        ${captureCard()}
      </div>`;
  }
  renderDialog('config', `<div class="dialog wide" role="dialog" aria-modal="true" aria-labelledby="cfgTitle">
      <div class="dialog-head"><div><h2 id="cfgTitle">系统配置</h2><p>播放兼容、图片、日志、安全访问与运维诊断</p></div><button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="tabs" role="tablist">${tabs.map(([k, l]) => `<button class="tab" role="tab" aria-selected="${G.tab === k}" data-act="settings-tab" data-t="${k}">${l}</button>`).join('')}</div>
      <div class="dialog-body">${body}</div>
      <div class="dialog-foot">
        <button class="btn lead" data-act="dialog-close" ${G.saving ? 'disabled' : ''}>${G.dirty ? '取消' : '关闭'}</button>
        <span class="note">${settingsFootNote()}</span>
        ${settingsSaveButton('保存配置')}
      </div>
    </div>`, { scrollKey: 'cfg-' + G.tab, onClose: requestCloseSettings, onDispose: disposeSettings });
}

function configPayload() {
  const v = G.v;
  const int = (x, d) => { const n = parseInt(x, 10); return Number.isFinite(n) ? n : d; };
  const list = x => String(x || '').split(/[\n,]+/).map(s => s.trim()).filter(Boolean).join(',');
  return {
    logLevel: v.logLevel,
    logAccess: v.logAccess,
    logHistoryEntriesPerFile: int(v.logHistoryEntriesPerFile, 2000),
    logHistoryMaxFiles: int(v.logHistoryMaxFiles, 20),
    capyStripEmby: v.capyStripEmby ? '1' : '0',
    emosCompat: v.emosCompat,
    emosMatchHosts: list(v.emosMatchHosts),
    emosProxyId: String(v.emosProxyId).trim(),
    emosProxyName: String(v.emosProxyName).trim(),
    imageProxyLimitEnabled: v.imageProxyLimitEnabled,
    imageProxyMaxConcurrent: int(v.imageProxyMaxConcurrent, 4),
    imageProxyRequestIntervalMs: int(v.imageProxyRequestIntervalMs, 250),
    imageCacheEnabled: v.imageCacheEnabled,
    imageCacheTtlDays: int(v.imageCacheTtlDays, 30),
    imageCacheMaxMb: int(v.imageCacheMaxMb, 2048),
    corsAllowOrigin: list(v.corsAllowOrigin),
    externalAllowHosts: list(v.externalAllowHosts),
    externalAllowAny: v.externalAllowAny,
    trustProxy: v.trustProxy,
    trafficCaptureEnabled: v.trafficCaptureEnabled,
    trafficCaptureFile: String(v.trafficCaptureFile).trim(),
  };
}

async function saveConfig() {
  G.saving = true;
  renderSettings();
  const form = G;
  const r = await api('config.set', { config: configPayload() });
  if (G !== form) return;
  G.saving = false;
  if (!r.ok) {
    if (r._stale) return;
    renderSettings();
    toast('bad', '保存失败', apiError(r));
    return;
  }
  G.dirty = false;
  G.savedAt = fmtTime(Date.now());
  renderSettings();
  toast('ok', '配置已保存', '已立即生效，可以继续修改。');
  refreshImageCacheStats(false);
  if (typeof refreshStatsImageCache === 'function') refreshStatsImageCache();
}

async function refreshImageCacheStats(showToast = true) {
  const r = await api('imageCache.stats');
  if (!G || G.kind !== 'config') return;
  if (!r.ok) {
    G.cache = null;
    if (showToast && !r._stale) toast('bad', '刷新图片缓存状态失败', apiError(r));
  } else {
    G.cache = r.cache || null;
    if (showToast) toast('ok', '图片缓存状态已刷新', '');
  }
  if (G.tab === 'image') renderSettings();
}

async function clearImageCache() {
  const ok = await confirmDialog({ title: '清理图片缓存？', text: '缓存的图片会全部删除，之后的图片请求会重新回源。', ok: '清理缓存', danger: true });
  if (!ok) return;
  const r = await api('imageCache.clear');
  if (!r.ok) { if (!r._stale) toast('bad', '清理图片缓存失败', apiError(r)); return; }
  if (G && G.kind === 'config') { G.cache = r.cache || null; renderSettings(); }
  toast('ok', '图片缓存已清理', '');
}

async function refreshTrafficCaptureStats(showToast = true) {
  const r = await api('trafficCapture.stats');
  if (!G || G.kind !== 'config') return;
  if (!r.ok) {
    G.capture = null;
    if (showToast && !r._stale) toast('bad', '刷新代理流量记录状态失败', apiError(r));
  } else {
    G.capture = r.capture || null;
    if (showToast) toast('ok', '代理流量记录状态已刷新', '');
  }
  if (G.tab === 'diag') renderSettings();
}

async function clearTrafficCapture(stage) {
  const label = stage === undefined ? '' : (String(stage || '').trim() || '未分类');
  const ok = await confirmDialog({
    title: stage === undefined ? '清空代理流量记录？' : `清空「${label}」分类的记录？`,
    text: '记录文件中的对应内容会被删除，无法恢复。',
    ok: '清空',
    danger: true,
  });
  if (!ok) return;
  const r = stage === undefined ? await api('trafficCapture.clear') : await api('trafficCapture.clearStage', { stage });
  if (!r.ok) { if (!r._stale) toast('bad', '清空失败', apiError(r)); return; }
  if (G && G.kind === 'config') { G.capture = r.capture || null; renderSettings(); }
  toast('ok', stage === undefined ? '代理流量记录已清空' : `「${label}」分类记录已清空`, '');
}

/* ============================================================
 * 通知与告警
 * ============================================================ */

async function openTgModal() {
  const r = await api('tg.get');
  if (!r.ok || !r.config) {
    if (r._status !== 401) toast('bad', '读取通知设置失败', r.error || '响应无效');
    return;
  }
  const c = r.config;
  G = {
    kind: 'notify', saving: false, dirty: false, savedAt: '', testing: '',
    v: {
      enabled: !!c.enabled,
      token: c.token || '',
      chat: c.chat || '',
      serverRemark: c.serverRemark || '',
      reportEnabled: c.reportEnabled === true,
      reportTime: c.reportTime || '08:00',
      alertNodes: c.alertNodes !== false,
      alertErrors: c.alertErrors !== false,
      alertFailThreshold: c.alertFailThreshold || 3,
    },
  };
  renderNotifyDialog();
}

function tgPayload() {
  const v = G.v;
  const threshold = parseInt(v.alertFailThreshold, 10);
  return {
    enabled: v.enabled,
    token: String(v.token).trim(),
    chat: String(v.chat).trim(),
    serverRemark: String(v.serverRemark).trim(),
    reportEnabled: v.reportEnabled,
    reportTime: v.reportTime,
    alertNodes: v.alertNodes,
    alertErrors: v.alertErrors,
    alertFailThreshold: Number.isFinite(threshold) ? threshold : 3,
  };
}

function renderNotifyDialog() {
  const v = G.v;
  const ready = v.enabled && String(v.token).trim() && String(v.chat).trim();
  const threshold = parseInt(v.alertFailThreshold, 10) || 3;
  renderDialog('notify', `<div class="dialog compact" role="dialog" aria-modal="true" aria-labelledby="tgTitle">
      <div class="dialog-head"><div><h2 id="tgTitle">通知</h2><p>出现故障时通过 Telegram 联系你，也可以推送每日日报</p></div><button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="dialog-body">
        ${!ready ? `<div class="callout warn" id="tgNotReady" style="padding:10px 12px">${ico('i-warn')}<div class="ct"><b>通知渠道还没配置好</b><span>节点故障或程序出错时无法提醒你。打开 Telegram 并填写 Bot Token 与 Chat ID。</span></div></div>` : ''}
        <div class="fgroup">
          <div class="fgroup-title"><span>渠道</span></div>
          <div>${switchRow('enabled', 'Telegram', '通过 Bot 推送故障告警与每日日报。')}</div>
          ${v.enabled ? `<div class="field"><label class="field-label" for="tg-token">Bot Token <span class="req">*</span></label><input class="input mono" id="tg-token" data-g="token" value="${attr(v.token)}" spellcheck="false" autocomplete="off" placeholder="123456789:AA…"><div class="field-help">在 Telegram 中通过 @BotFather 获取。</div></div>
          <div class="frow">
            <div class="field"><label class="field-label" for="tg-chat">Chat ID <span class="req">*</span></label><input class="input mono" id="tg-chat" data-g="chat" value="${attr(v.chat)}" spellcheck="false" autocomplete="off" placeholder="-1001234567890"><div class="field-help">群组 / 频道 ID，或 @用户名。</div></div>
            <div class="field"><label class="field-label" for="tg-serverRemark">服务器备注 <span class="opt">选填</span></label><input class="input" id="tg-serverRemark" maxlength="80" data-g="serverRemark" value="${attr(v.serverRemark)}"><div class="field-help">显示在每条消息开头，区分多台服务器。</div></div>
          </div>` : ''}
        </div>
        <div class="fgroup">
          <div class="fgroup-title"><span>故障告警</span></div>
          <div>
            ${switchRow('alertNodes', '节点故障与恢复', `节点所有线路连续 ${threshold} 次检测失败时推送一次，恢复后（连续 2 次成功）再推送一次。`)}
            ${v.alertNodes ? `<div class="field" style="padding:4px 0 12px;max-width:260px"><label class="field-label" for="g-alertFailThreshold">判定故障的连续失败次数</label><div class="input-group"><input class="input num" id="g-alertFailThreshold" type="number" min="1" max="10" step="1" data-g="alertFailThreshold" value="${attr(v.alertFailThreshold)}"><span class="btn" style="pointer-events:none">次</span></div><div class="field-help">1–10，默认 3。每分钟检测一次，3 次约等于 3 分钟。</div></div>` : ''}
            ${switchRow('alertErrors', '程序内部错误', '数据库写入失败、文件读写失败、定时任务崩溃等错误，汇总后推送，最多每 10 分钟一条。上游超时这类节点问题不在此列。')}
          </div>
        </div>
        <div class="fgroup">
          <div class="fgroup-title"><span>每日日报</span></div>
          <div>
            ${switchRow('reportEnabled', '每日日报', '前一天的播放次数、会话、流量和活跃节点。')}
            ${v.reportEnabled ? `<div class="field" style="padding:4px 0 12px;max-width:220px"><label class="field-label" for="tg-reportTime">日报推送时间</label><input class="input num" id="tg-reportTime" type="time" data-g="reportTime" value="${attr(v.reportTime)}"><div class="field-help">北京时间。</div></div>` : ''}
          </div>
        </div>
      </div>
      <div class="dialog-foot">
        <button class="btn lead" data-act="tg-test-menu" ${String(v.token).trim() && String(v.chat).trim() ? '' : 'disabled title="先填写 Bot Token 与 Chat ID"'} aria-haspopup="menu">${G.testing ? '<span class="spin"></span>发送中…' : `${ico('i-send')}发送测试`}</button>
        <span class="note">${settingsFootNote()}</span>
        <button class="btn" data-act="dialog-close" ${G.saving ? 'disabled' : ''}>${G.dirty ? '取消' : '关闭'}</button>
        ${settingsSaveButton('保存')}
      </div>
    </div>`, { onClose: requestCloseSettings, onDispose: disposeSettings });
}

async function saveTg() {
  G.saving = true;
  renderSettings();
  const form = G;
  const r = await api('tg.set', { config: tgPayload() });
  if (G !== form) return;
  G.saving = false;
  if (!r.ok) {
    if (r._stale) return;
    renderSettings();
    toast('bad', '保存失败', apiError(r));
    return;
  }
  G.dirty = false;
  G.savedAt = fmtTime(Date.now());
  renderSettings();
  updateNotifyBadge(tgPayload());
  toast('ok', '通知设置已保存', '已立即生效，可以继续修改。');
}

/* 用表单里当前的值发送测试，不需要先保存 */
async function testTg(kind) {
  if (!G || G.kind !== 'notify' || G.testing) return;
  G.testing = kind || 'channel';
  renderSettings();
  const r = await api('tg.test', kind === 'report' ? { tg: tgPayload(), kind: 'report' } : { tg: tgPayload() });
  if (!G || G.kind !== 'notify') return;
  G.testing = '';
  renderSettings();
  if (r._stale) return;
  if (r.ok) toast('ok', kind === 'report' ? '日报已发送' : '测试消息已发送', '请在 Telegram 中查看。');
  else toast('bad', '发送失败', apiError(r));
}

/* 通知渠道没配好时在导航上提示一个小圆点：故障时没人能收到提醒 */
function updateNotifyBadge(c) {
  const badge = document.getElementById('notifyBadge');
  if (!badge) return;
  const ready = c && c.enabled && c.token && c.chat && (c.alertNodes !== false || c.alertErrors !== false);
  badge.hidden = !!ready;
  badge.setAttribute('aria-label', '通知渠道未配置');
  badge.parentElement.title = ready ? '通知' : '通知 · 渠道未配置，故障时无法提醒你';
}

async function checkNotifyChannel() {
  const r = await api('tg.get');
  if (r.ok && r.config) updateNotifyBadge(r.config);
}

/* ============================================================
 * 管理员双重验证（绑定 / 关闭）
 * ============================================================ */

function renderTwoFactorDialog(step) {
  const s = AUTH_STATUS || {};
  const needsCurrentCode = !!s.configured && !s.emergencyDisabled;
  const disable = TWO_FACTOR_ACTION === 'disable';
  const title = disable ? '关闭双重验证' : s.configured ? '重新绑定双重验证' : '开启双重验证';
  const sub = disable ? '关闭后，管理入口将恢复为仅验证管理员 Token'
    : s.configured ? '先验证当前管理员身份，再扫描新的二维码替换原绑定' : '验证管理员身份后生成绑定二维码';
  renderDialog('twoFactor', `<div class="dialog compact" role="dialog" aria-modal="true" aria-labelledby="twoFactorModalTitle">
      <div class="dialog-head"><div><h2 id="twoFactorModalTitle">${esc(title)}</h2><p id="twoFactorModalSubtitle">${esc(sub)}</p></div><button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="dialog-body">
        <div class="fgroup" id="twoFactorReauthStep" ${step === 'enroll' ? 'hidden' : ''}>
          ${s.emergencyDisabled ? `<div class="warnline" id="twoFactorEmergencyNote">${ico('i-warn')}<span>当前处于 ADMIN_2FA_DISABLED 紧急停用状态，只需验证管理员 Token。</span></div>` : ''}
          <div class="field"><label class="field-label" for="twoFactorAdminToken">管理员 Token <span class="req">*</span></label><input class="input mono" id="twoFactorAdminToken" type="password" autocomplete="current-password" spellcheck="false" autofocus></div>
          <div class="field" id="twoFactorCurrentCodeWrap" ${needsCurrentCode ? '' : 'hidden'}><label class="field-label" for="twoFactorCurrentCode">当前动态验证码 <span class="req">*</span></label><input class="input code-input" id="twoFactorCurrentCode" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000"></div>
        </div>
        <div class="fgroup" id="twoFactorEnrollStep" ${step === 'enroll' ? '' : 'hidden'}>
          <div class="qr"><img id="twoFactorQRCode" alt="2FA 绑定二维码"><span class="faint" style="font-size:12px">无法扫码时手动输入密钥</span><span class="mono" id="twoFactorManualKey"></span></div>
          <div class="field"><label class="field-label" for="twoFactorNewCode">新验证器中的动态验证码 <span class="req">*</span></label><input class="input code-input" id="twoFactorNewCode" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="000000"></div>
        </div>
      </div>
      <div class="dialog-foot">
        <button class="btn lead" data-act="dialog-close">取消</button><span class="note"></span>
        ${step === 'enroll'
          ? '<button class="btn btn-primary" id="twoFactorConfirmBtn" data-act="tfa-confirm">确认绑定</button>'
          : `<button class="btn ${disable ? 'btn-danger' : 'btn-primary'}" id="twoFactorContinueBtn" data-act="tfa-continue">${disable ? '确认关闭' : '生成二维码'}</button>`}
      </div>
    </div>`, { onClose: closeTwoFactorModal, onDispose: () => { twoFactorFlowGeneration++; TWO_FACTOR_SETUP_ID = ''; } });
}

function openTwoFactorSetup() {
  if (!AUTH_STATUS) return;
  twoFactorFlowGeneration++;
  TWO_FACTOR_ACTION = 'setup';
  TWO_FACTOR_SETUP_ID = '';
  G = null;
  renderTwoFactorDialog('reauth');
}

function openTwoFactorDisable() {
  if (!AUTH_STATUS?.configured) return;
  twoFactorFlowGeneration++;
  TWO_FACTOR_ACTION = 'disable';
  TWO_FACTOR_SETUP_ID = '';
  G = null;
  renderTwoFactorDialog('reauth');
}

function closeTwoFactorModal() {
  twoFactorFlowGeneration++;
  TWO_FACTOR_SETUP_ID = '';
  closeDialog();
}

async function submitTwoFactorReauth() {
  const token = document.getElementById('twoFactorAdminToken').value.trim();
  const currentCode = document.getElementById('twoFactorCurrentCode').value.trim();
  const needsCurrentCode = !!AUTH_STATUS?.configured && !AUTH_STATUS?.emergencyDisabled;
  if (!token) {
    toast('bad', '请输入管理员 Token', '');
    return;
  }
  if (needsCurrentCode && !currentCode) {
    toast('bad', '请输入当前动态验证码', '');
    return;
  }
  const button = document.getElementById('twoFactorContinueBtn');
  button.disabled = true;
  const generation = authGeneration;
  const flowGeneration = twoFactorFlowGeneration;
  if (TWO_FACTOR_ACTION === 'disable') {
    const confirmed = await confirmDialog({ title: '确定关闭管理员双重验证？', text: '关闭后将只验证 ADMIN_TOKEN。', ok: '关闭 2FA', danger: true });
    if (!confirmed) {
      button.disabled = false;
      return;
    }
    const r = await authFetch('/admin/auth/2fa/disable', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token, currentCode }),
    });
    if (generation !== authGeneration) return;
    if (handleSessionAuthFailure(r, generation)) return;
    if (r.ok) {
      persistTokenForStatus(token, r.twoFactor);
      acceptRotatedSession(r.twoFactor);
    }
    if (flowGeneration !== twoFactorFlowGeneration) {
      if (r.ok) closeTwoFactorModal();
      return;
    }
    button.disabled = false;
    if (!r.ok) {
      toast('bad', '关闭 2FA 失败', authErrorMessage(r.error));
      return;
    }
    closeTwoFactorModal();
    toast('ok', '管理员双重验证已关闭', '');
    return;
  }
  const r = await authFetch('/admin/auth/2fa/setup', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token, currentCode }),
  });
  if (generation !== authGeneration) return;
  if (handleSessionAuthFailure(r, generation)) return;
  if (flowGeneration !== twoFactorFlowGeneration) return;
  button.disabled = false;
  if (!r.ok || !r.setup) {
    toast('bad', '生成绑定二维码失败', authErrorMessage(r.error));
    return;
  }
  TWO_FACTOR_SETUP_ID = r.setup.setupId || '';
  renderTwoFactorDialog('enroll');
  document.getElementById('twoFactorQRCode').src = r.setup.qrCodeDataUrl || '';
  document.getElementById('twoFactorManualKey').textContent = r.setup.manualKey || '';
  document.getElementById('twoFactorNewCode').focus();
}

async function confirmTwoFactorSetup() {
  const code = document.getElementById('twoFactorNewCode').value.trim();
  if (!TWO_FACTOR_SETUP_ID) {
    toast('bad', '绑定二维码已失效', '请重新生成。');
    return;
  }
  if (!code) {
    toast('bad', '请输入新验证器生成的动态验证码', '');
    return;
  }
  const button = document.getElementById('twoFactorConfirmBtn');
  button.disabled = true;
  const generation = authGeneration;
  const flowGeneration = twoFactorFlowGeneration;
  const r = await authFetch('/admin/auth/2fa/confirm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ setupId: TWO_FACTOR_SETUP_ID, code }),
  });
  if (generation !== authGeneration) return;
  if (handleSessionAuthFailure(r, generation)) return;
  if (r.ok) acceptRotatedSession(r.twoFactor);
  if (flowGeneration !== twoFactorFlowGeneration) {
    if (r.ok) closeTwoFactorModal();
    return;
  }
  button.disabled = false;
  if (!r.ok) {
    toast('bad', '确认绑定失败', authErrorMessage(r.error));
    if (r.error === 'SETUP_EXPIRED') { TWO_FACTOR_SETUP_ID = ''; renderTwoFactorDialog('reauth'); }
    return;
  }
  closeTwoFactorModal();
  toast('ok', AUTH_STATUS.emergencyDisabled ? '新绑定已保存' : '管理员双重验证已开启',
    AUTH_STATUS.emergencyDisabled ? '请移除 ADMIN_2FA_DISABLED 并重启服务。' : '下次登录需要输入动态验证码。');
}

/* ============================================================
 * 事件
 * ============================================================ */

registerActions({
  'open-config': el => {
    configInitialTab = ['play', 'image', 'log', 'security', 'diag'].includes(el.dataset.tab) ? el.dataset.tab : 'play';
    openConfigModal();
  },
  'open-notify': () => openTgModal(),
  'settings-tab': el => { G.tab = el.dataset.t; renderSettings(); },
  'settings-save': () => { if (G?.kind === 'notify') saveTg(); else if (G) saveConfig(); },
  'cfg-cache-refresh': () => refreshImageCacheStats(true),
  'cfg-cache-clear': () => clearImageCache(),
  'cfg-capture-refresh': () => refreshTrafficCaptureStats(true),
  'cfg-capture-clear': () => clearTrafficCapture(),
  'cfg-capture-clear-stage': el => clearTrafficCapture(el.dataset.stage || ''),
  'cfg-2fa-setup': async () => { if (G?.dirty && !await confirmDialog({ title: '放弃未保存的配置修改？', text: '进入 2FA 设置会关闭配置弹窗。', ok: '继续', danger: true })) return; openTwoFactorSetup(); },
  'cfg-2fa-disable': async () => { if (G?.dirty && !await confirmDialog({ title: '放弃未保存的配置修改？', text: '进入 2FA 设置会关闭配置弹窗。', ok: '继续', danger: true })) return; openTwoFactorDisable(); },
  'tfa-continue': () => submitTwoFactorReauth(),
  'tfa-confirm': () => confirmTwoFactorSetup(),
  'tg-test-menu': el => openMenu(el, [
    { act: 'tg-test-channel', icon: 'i-send', label: '发送测试消息' },
    { act: 'tg-test-report', icon: 'i-stats', label: '发送一份今日日报' },
  ]),
  'tg-test-channel': () => testTg('channel'),
  'tg-test-report': () => testTg('report'),
});

document.addEventListener('input', e => {
  const t = e.target;
  if (G && t.dataset.g && t.type !== 'checkbox' && t.tagName !== 'SELECT' && t.closest('#overlay .dialog')) onSettingsInput(t);
});

document.addEventListener('change', e => {
  const t = e.target;
  if (G && t.dataset.g && (t.type === 'checkbox' || t.tagName === 'SELECT') && t.closest('#overlay .dialog')) onSettingsInput(t);
});

document.addEventListener('keydown', e => {
  if (e.key !== 'Enter' || !dialogOpen('twoFactor')) return;
  if (e.target.id === 'twoFactorAdminToken' || e.target.id === 'twoFactorCurrentCode') { e.preventDefault(); submitTwoFactorReauth(); }
  if (e.target.id === 'twoFactorNewCode') { e.preventDefault(); confirmTwoFactorSetup(); }
});

/* 登录后检查一次通知渠道；退出登录时清掉弹窗状态 */
registerPage('_settings', {
  afterLogin() { checkNotifyChannel(); },
  reset() {
    G = null;
    twoFactorFlowGeneration++;
    TWO_FACTOR_SETUP_ID = '';
  },
});
