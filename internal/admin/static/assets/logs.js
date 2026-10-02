/* ============================================================
 * logs.js — 日志页
 * 分页、实时流（SSE）、断档补齐、清空同步的逻辑沿用旧版，只重做界面。
 * 第 1 页是最新一页；「上一页」翻向更新，「下一页」翻向更旧。
 * ============================================================ */

const LOG_PAGE_LIMIT = 2000;
const LOG_DOM_LIMIT = 4000;
const LOG_POLL_MS = 5000;

let logsRendered = false;
let logNodes = [];
let lastLogLines = [];
let lastLogEntries = [];
let lastLogMeta = {};
let currentLogPage = 1;
let currentLogTotalPages = 1;
let currentLogHasOlder = false;
let currentLogLevelFilter = 'all';
let currentLogQuery = '';
let currentLogNodeFilter = '';
let currentLogMode = 'req';
let logSearchTimer = null;
let logRefreshTimer = null;
let logStreamController = null;
let logStreamToken = 0;
let lastLogSeenId = 0;
let lastLogStreamId = 0;
let logViewEpoch = 0;
let logGapRefreshPending = false;
let pendingLogCount = 0;
let logsLoadedOnce = false;
let logsLoadFailed = false;
let traceRenderQueued = false;

function safeLogLevel(value) {
  const level = String(value || 'info').trim().toLowerCase();
  return ['silent', 'error', 'warn', 'info', 'debug'].includes(level) ? level : 'info';
}

/* ============================================================
 * 页面骨架
 * ============================================================ */

function renderLogsPage() {
  const page = document.getElementById('pageLogs');
  page.innerHTML = `
    <header class="page-head">
      <div class="page-title">
        <h1>日志</h1>
        <p id="logSummary">正在加载…</p>
      </div>
      <div class="lv-live">
        <label class="lv-live-toggle" for="logAutoRefresh" title="实时接收新日志">
          <span class="switch"><input type="checkbox" id="logAutoRefresh" checked><span></span></span>
          <span>实时</span>
        </label>
        <span id="logStreamStatus"></span>
        <button class="btn btn-sm" data-act="logs-reconnect" id="logReconnectBtn" hidden>${ico('i-probe')}重新连接</button>
      </div>
      <div class="gactions">
        <button class="btn" data-act="logs-copy" title="复制当前显示的日志">${ico('i-copy')}<span class="btn-label">复制</span></button>
        <button class="btn btn-danger" data-act="logs-clear" title="清空日志缓冲区与落盘历史">${ico('i-trash')}<span class="btn-label">清空</span></button>
      </div>
    </header>
    <div class="toolbar lv-toolbar">
      <label class="input-affix search lv-search">${ico('i-search')}<input class="input mono" id="logSearch" type="search" data-search placeholder="requestId / 路径 / IP" aria-label="搜索日志" spellcheck="false"></label>
      <select class="select lv-select" id="logLevelFilter" aria-label="日志等级">
        <option value="all">全部等级</option>
        <option value="error">ERROR</option>
        <option value="warn">WARN</option>
        <option value="info">INFO</option>
        <option value="debug">DEBUG</option>
      </select>
      <select class="select lv-select lv-node" id="logNodeFilter" aria-label="按节点筛选"><option value="">全部节点</option></select>
      <div class="seg" role="group" aria-label="日志视图" id="logModeSeg">
        <button data-act="logs-mode" data-mode="req" aria-pressed="true">按请求</button>
        <button data-act="logs-mode" data-mode="line" aria-pressed="false">按行</button>
      </div>
      <div class="grow"></div>
      <div class="lv-pager" role="group" aria-label="翻页">
        <button class="btn btn-sm btn-icon" data-act="logs-refresh" title="刷新" aria-label="刷新">${ico('i-probe')}</button>
        <button class="btn btn-sm" data-act="logs-newer" id="logNewerBtn" title="翻向更新的日志">上一页</button>
        <span class="lv-page">
          <input class="input num" id="logPageInput" type="number" min="1" value="1" aria-label="页码，回车跳转">
          <span class="faint num" id="logPageTotal">/ 1</span>
        </span>
        <button class="btn btn-sm" data-act="logs-older" id="logOlderBtn" title="翻向更早的日志">下一页</button>
        <button class="btn btn-sm" data-act="logs-latest" id="logLatestBtn" title="回到最新一页">最新</button>
      </div>
    </div>
    <div class="page-body lv-body" id="logBody">
      <div class="lv-view" id="logTraceView"><div class="lv-trace" id="traceList">${logsSkeleton()}</div></div>
      <div class="lv-view lv-lineview" id="logLineView" hidden>
        <div class="lv-console" id="logConsole" role="log" aria-live="off"></div>
        <button class="lv-newbadge" type="button" data-act="logs-scroll-bottom" id="logNewBadge" hidden></button>
      </div>
    </div>`;

  document.getElementById('logAutoRefresh').addEventListener('change', e => setLogAutoRefresh(e.target.checked));
  document.getElementById('logSearch').addEventListener('input', e => setLogQuery(e.target.value));
  document.getElementById('logLevelFilter').addEventListener('change', e => setLogLevelFilter(e.target.value));
  document.getElementById('logNodeFilter').addEventListener('change', e => setLogNodeFilter(e.target.value));
  document.getElementById('logPageInput').addEventListener('keydown', e => { if (e.key === 'Enter') jumpLogsPage(); });
  document.getElementById('logConsole').addEventListener('scroll', e => {
    if (isAtBottom(e.currentTarget)) resetPendingLogCount();
  });
  setLogStreamStatus('待机', 'paused');
  logsRendered = true;
}

function logsSkeleton() {
  return Array.from({ length: 6 }, () => `<div class="lv-skel"><div class="skel" style="height:12px;width:55%"></div><div class="skel" style="height:10px;width:35%"></div></div>`).join('');
}

/* ============================================================
 * 加载与渲染
 * ============================================================ */

async function loadLogs(showToast = false, page = currentLogPage, options = {}) {
  const requestedPage = Math.max(1, Math.floor(Number(page || 1)));
  const previousNewestId = Number(lastLogMeta?.newestId || 0);
  currentLogPage = requestedPage;
  const requestLevel = currentLogLevelFilter;
  const requestNode = currentLogNodeFilter;
  const requestSearch = currentLogQuery;
  const requestLogViewEpoch = logViewEpoch;
  const payload = { limit: LOG_PAGE_LIMIT, page: requestedPage, level: requestLevel, query: requestSearch, node: requestNode };
  const r = await api('logs.list', payload);
  if (r._stale) return false;
  if (!r.ok) {
    toast('bad', '加载日志失败', apiError(r));
    if (!logsLoadedOnce) { logsLoadFailed = true; renderLogs([], {}); }
    return false;
  }
  if (requestLevel !== currentLogLevelFilter || requestNode !== currentLogNodeFilter || requestSearch !== currentLogQuery) {
    return false;
  }
  if (requestLogViewEpoch !== logViewEpoch) {
    return false;
  }
  logsLoadedOnce = true;
  logsLoadFailed = false;
  const logs = Array.isArray(r.logs) ? r.logs : [];
  const currentIds = new Set(lastLogEntries.map(logEntryId).filter(id => id > 0));
  const nextNewestId = Number(r.newestId || 0);
  const nextStreamId = Number(r.streamId || nextNewestId || 0);
  const streamSequenceReset = lastLogStreamId > 0 && nextStreamId < lastLogStreamId;
  const snapshotNewestId = Math.max(nextNewestId, newestLogId(logs));
  const logSequenceReset = previousNewestId > 0 && snapshotNewestId < previousNewestId;
  const newLogCount = requestedPage === 1 && previousNewestId > 0 && nextNewestId > previousNewestId
    ? logs.filter(log => {
        const id = logEntryId(log);
        return id > previousNewestId && !currentIds.has(id);
      }).length
    : 0;
  const resolvedPage = Math.max(1, Number(r.page || requestedPage));
  const mergedLogs = resolvedPage === 1 ? mergeLogsWithStreamedEntries(logs, { skipExisting: logSequenceReset }) : logs;
  const mergedExtraCount = Math.max(0, mergedLogs.length - logs.length);
  const mergedNewestId = Math.max(snapshotNewestId, newestLogId(mergedLogs));
  const mergedMeta = {
    ...r,
    newestId: mergedNewestId,
    totalEntries: Math.max(0, Number(r.totalEntries ?? logs.length)) + mergedExtraCount,
  };
  lastLogEntries = mergedLogs;
  lastLogMeta = mergedMeta;
  currentLogPage = resolvedPage;
  currentLogTotalPages = Math.max(1, Math.ceil(Number(mergedMeta.totalEntries || 0) / payload.limit));
  currentLogHasOlder = currentLogTotalPages > currentLogPage || !!r.hasOlder;
  if (currentLogPage === 1) {
    lastLogSeenId = mergedNewestId;
    // 只在搜索框为空时同步流水号：搜索会关掉实时流（isLogStreamAllowed），
    // 节点筛选不会，所以这里看的是搜索词而不是下推给服务端的 query。
    if (requestSearch === '') {
      lastLogStreamId = streamSequenceReset ? nextStreamId : Math.max(lastLogStreamId, nextStreamId);
    }
  }
  renderLogs(mergedLogs, { ...mergedMeta, newLogCount });
  if (currentTab === 'logs' && !options.skipRealtimeSync) syncLogRealtime();
  if (showToast) toast('ok', '日志已刷新');
  return true;
}

async function clearLogs() {
  const yes = await confirmDialog({
    title: '清空运行日志？',
    text: '内存中的日志和落盘的日志历史都会被清空，无法恢复。',
    ok: '清空日志',
    danger: true,
  });
  if (!yes) return;
  const r = await api('logs.clear');
  if (r._stale) return;
  if (!r.ok) {
    toast('bad', '清空日志失败', apiError(r));
    return;
  }
  resetLogViewAfterClear(r);
  if (currentTab === 'logs') syncLogRealtime();
  toast('ok', '日志已清空');
}

function resetLogViewAfterClear(meta = {}) {
  stopLogStream();
  logViewEpoch++;
  const clearedMeta = {
    ...lastLogMeta,
    ...meta,
    newestId: 0,
    oldestId: 0,
    totalEntries: 0,
    totalPages: 1,
    page: 1,
    hasOlder: false,
  };
  lastLogEntries = [];
  lastLogMeta = clearedMeta;
  currentLogPage = 1;
  currentLogTotalPages = 1;
  currentLogHasOlder = false;
  lastLogSeenId = 0;
  lastLogStreamId = 0;
  logGapRefreshPending = false;
  renderLogs([], { ...clearedMeta, resetPending: true });
}

function renderLogs(logs, meta = {}) {
  const consoleEl = document.getElementById('logConsole');
  if (!consoleEl) return;
  const wasAtBottom = isAtBottom(consoleEl);
  lastLogLines = logs.map(x => x.line || '');
  updateLogSummary(logs, meta);
  updateLogPager();
  renderTrace(logs);
  if (!logs.length) {
    consoleEl.innerHTML = logsEmptyMarkup();
    resetPendingLogCount();
    return;
  }
  consoleEl.innerHTML = logs.map(log => logLineMarkup(log)).join('');
  if (wasAtBottom || meta.resetPending) {
    scrollLogsToBottom();
  } else if (Number(meta.newLogCount || 0) > 0) {
    addPendingLogCount(Number(meta.newLogCount || 0));
  }
}

function logsFiltered() {
  return currentLogLevelFilter !== 'all' || !!currentLogNodeFilter || !!currentLogQuery;
}

function logsEmptyMarkup() {
  if (logsLoadFailed) {
    return `<div class="empty lv-empty"><div class="empty-icon" style="color:var(--bad)">${ico('i-bad')}</div><h3>日志加载失败</h3><p>请检查网络后重试。</p><button class="btn btn-sm" data-act="logs-refresh">${ico('i-probe')}重试</button></div>`;
  }
  if (logsFiltered()) {
    return `<div class="empty lv-empty"><div class="empty-icon">${ico('i-search')}</div><h3>没有匹配的日志</h3><p>${esc(logFilterText())}下没有日志。</p><button class="btn btn-sm" data-act="logs-clear-filter">清除筛选</button></div>`;
  }
  return `<div class="empty lv-empty"><div class="empty-icon">${ico('i-logs')}</div><h3>暂无运行日志</h3><p>服务运行时产生的日志会出现在这里。</p></div>`;
}

function logFilterText() {
  const parts = [];
  if (currentLogLevelFilter !== 'all') parts.push(`等级 ${currentLogLevelFilter.toUpperCase()}`);
  if (currentLogNodeFilter) parts.push(`节点「${logNodeLabel(currentLogNodeFilter)}」`);
  if (currentLogQuery) parts.push(`搜索「${currentLogQuery}」`);
  return parts.join('、') || '当前条件';
}

function updateLogSummary(logs, meta = {}) {
  const el = document.getElementById('logSummary');
  if (!el) return;
  const totalEntries = Math.max(0, Number(meta.totalEntries ?? logs.length));
  const parts = [];
  if (logsFiltered()) parts.push(logFilterText());
  parts.push(totalEntries === logs.length ? `共 ${formatCount(totalEntries)} 条` : `本页 ${formatCount(logs.length)} / 共 ${formatCount(totalEntries)} 条`);
  parts.push(`第 ${currentLogPage}/${currentLogTotalPages} 页`);
  parts.push(`内存容量 ${formatCount(meta.capacity || 0)} 条${meta.history ? ' · 含落盘历史' : ''}`);
  el.textContent = parts.join(' · ');
}

/* 日志行形如：2026-07-30T22:08:45+08:00 [INFO] [200] [access] event=requestFinished id=… */
function splitLogLine(entry) {
  const raw = String(entry?.line || '');
  let rest = raw;
  let time = '';
  const head = rest.match(/^(\S+)\s+\[([A-Za-z]+)\]\s*/);
  if (head) {
    time = traceTimeText(head[1]);
    rest = rest.slice(head[0].length);
  }
  let status = 0;
  let scope = '';
  let tag;
  while ((tag = rest.match(/^\[([^\]]*)\]\s*/))) {
    const value = tag[1];
    if (/^\d{3}$/.test(value)) status = Number(value);
    else if (!scope) scope = value;
    rest = rest.slice(tag[0].length);
  }
  if (!head) rest = raw;
  return { time, status, scope: scope || entry?.scope || '', text: rest };
}

function logLevelPill(level) {
  const lv = safeLogLevel(level);
  return `<span class="lv-lvl ${lv}">${lv.toUpperCase()}</span>`;
}

function statusCodeChip(status) {
  const code = Number(status || 0);
  if (!code) return `<span class="lv-code idle">—</span>`;
  const cls = code >= 500 ? 'bad' : code >= 400 ? 'warn' : 'ok';
  return `<span class="lv-code ${cls}" title="HTTP ${code}">${code}</span>`;
}

function logLineMarkup(entry) {
  const p = splitLogLine(entry);
  const lv = safeLogLevel(entry.level);
  return `<div class="lv-line is-${lv}">
    <span class="lv-time">${esc(p.time || '—')}</span>${logLevelPill(lv)}
    <span class="lv-msg">${p.status ? statusCodeChip(p.status) + ' ' : ''}${p.scope ? `<span class="lv-scope">${esc(p.scope)}</span> ` : ''}${esc(p.text)}</span>
  </div>`;
}

function logEntryId(entry) {
  const id = Number(entry?.id || 0);
  return Number.isFinite(id) ? id : 0;
}

function newestLogId(logs) {
  return (logs || []).reduce((max, log) => Math.max(max, logEntryId(log)), 0);
}

function mergeLogsWithStreamedEntries(snapshotLogs, options = {}) {
  const snapshotNewestId = newestLogId(snapshotLogs);
  const seenIds = new Set(snapshotLogs.map(logEntryId).filter(id => id > 0));
  const existingLogs = options.skipExisting ? [] : lastLogEntries;
  const streamedLogs = existingLogs
    .filter(log => {
      const id = logEntryId(log);
      return id > snapshotNewestId && !seenIds.has(id) && logMatchesCurrentFilter(log);
    })
    .sort((a, b) => logEntryId(a) - logEntryId(b));
  return [...snapshotLogs, ...streamedLogs].slice(-LOG_DOM_LIMIT);
}

/* ============================================================
 * 筛选
 * ============================================================ */

async function loadLogNodes() {
  const r = await api('list');
  if (!r.ok || !Array.isArray(r.nodes)) return;
  logNodes = r.nodes;
  renderLogNodeFilter();
  queueTraceRender();
}

/* 节点筛选：值用 name，显示用 displayName。 */
function renderLogNodeFilter() {
  const sel = document.getElementById('logNodeFilter');
  if (!sel) return;
  const nodes = Array.isArray(logNodes) ? logNodes : [];
  if (currentLogNodeFilter && !nodes.some(n => n.name === currentLogNodeFilter)) {
    // 选中的节点被删掉了：静默回到「全部节点」，等下一次刷新自然生效。
    currentLogNodeFilter = '';
  }
  sel.innerHTML = '<option value="">全部节点</option>'
    + nodes.map(n => `<option value="${attr(n.name)}">${esc(n.displayName || n.name)}</option>`).join('');
  sel.value = currentLogNodeFilter;
}

function setLogNodeFilter(value) {
  const next = String(value || '').trim();
  const nodes = Array.isArray(logNodes) ? logNodes : [];
  currentLogNodeFilter = nodes.some(n => n.name === next) ? next : '';
  const sel = document.getElementById('logNodeFilter');
  if (sel) sel.value = currentLogNodeFilter;
  loadLogs(false, 1);
}

/* 节点名在日志里出现的形态不止一种：
 *   access requestStarted  → uri=/uhdnow/... 且 node=uhdnow
 *   proxy  upstreamReady   → node=uhdnow（没有 uri）
 *   access requestFinished → uri=/uhdnow/...（没有 node=）
 * 只有裸节点名能同时覆盖三类。后端 LogFilter 也是对整行做小写子串匹配，
 * 这里保持同一套判定，服务端分页结果和 SSE 增量才不会一个松一个紧。 */
function logEntryMatchesNode(entry, node) {
  if (!node) return true;
  return String(entry?.line || '').toLowerCase().includes(node.toLowerCase());
}

function logNodeLabel(name) {
  const node = (Array.isArray(logNodes) ? logNodes : []).find(n => n.name === name);
  return node ? (node.displayName || node.name) : name;
}

function setLogLevelFilter(value) {
  const level = String(value || 'all').toLowerCase();
  currentLogLevelFilter = ['all', 'error', 'warn', 'info', 'debug'].includes(level) ? level : 'all';
  const select = document.getElementById('logLevelFilter');
  if (select) select.value = currentLogLevelFilter;
  loadLogs(false, 1);
}

function setLogQuery(value) {
  currentLogQuery = String(value || '').trim();
  if (currentLogQuery && document.getElementById('logAutoRefresh')?.checked) {
    disableLogAutoRefresh('搜索中', 'paused');
  }
  if (logSearchTimer) clearTimeout(logSearchTimer);
  logSearchTimer = setTimeout(() => loadLogs(false, 1), 300);
}

function clearLogFilters() {
  currentLogLevelFilter = 'all';
  currentLogNodeFilter = '';
  currentLogQuery = '';
  const search = document.getElementById('logSearch');
  if (search) search.value = '';
  const level = document.getElementById('logLevelFilter');
  if (level) level.value = 'all';
  const node = document.getElementById('logNodeFilter');
  if (node) node.value = '';
  loadLogs(false, 1);
}

/* ============================================================
 * 翻页
 * ============================================================ */

function updateLogPager() {
  const latestBtn = document.getElementById('logLatestBtn');
  const newerBtn = document.getElementById('logNewerBtn');
  const olderBtn = document.getElementById('logOlderBtn');
  updateLogPageInput();
  if (latestBtn) latestBtn.disabled = currentLogPage <= 1;
  if (newerBtn) newerBtn.disabled = currentLogPage <= 1;
  if (olderBtn) olderBtn.disabled = !currentLogHasOlder || currentLogPage >= currentLogTotalPages;
}

function loadLatestLogs(showToast = false) {
  loadLogs(showToast, 1);
}

function loadOlderLogs() {
  if (!currentLogHasOlder || currentLogPage >= currentLogTotalPages) return;
  disableLogAutoRefresh();
  loadLogs(false, currentLogPage + 1);
}

function loadNewerLogs() {
  if (currentLogPage <= 1) return;
  loadLogs(false, currentLogPage - 1);
}

function jumpLogsPage() {
  const input = document.getElementById('logPageInput');
  const page = Math.floor(Number(input?.value || 1));
  if (!Number.isFinite(page) || page < 1) {
    toast('bad', '页码不合法', `请输入 1 到 ${currentLogTotalPages} 之间的页码。`);
    updateLogPageInput();
    return;
  }
  if (page > 1) disableLogAutoRefresh();
  loadLogs(false, page);
}

function updateLogPageInput() {
  const input = document.getElementById('logPageInput');
  const total = document.getElementById('logPageTotal');
  if (input) {
    input.value = currentLogPage;
    input.max = currentLogTotalPages;
  }
  if (total) total.textContent = `/ ${currentLogTotalPages}`;
}

/* ============================================================
 * 实时：SSE 流，失败时退回每 5 秒轮询
 * ============================================================ */

function setLogAutoRefresh(enabled) {
  if (!enabled) {
    stopLogRealtime();
    return;
  }
  if (currentLogPage > 1) {
    disableLogAutoRefresh();
    toast('warn', '历史页不启用实时日志', '请先回到最新一页。');
    return;
  }
  if (currentLogQuery) {
    disableLogAutoRefresh('搜索中', 'paused');
    toast('warn', '搜索时不启用实时日志', '清空搜索后再打开。');
    return;
  }
  if (currentTab === 'logs') startLogStream();
  else setLogStreamStatus('待机', 'paused');
}

function syncLogRealtime() {
  if (!isLogRealtimeEnabled()) {
    stopLogRealtime();
    return;
  }
  if (!isLogStreamAllowed()) {
    stopLogStream();
    stopLogAutoRefresh();
    setLogStreamStatus(currentLogQuery ? '搜索中' : '历史页', 'paused');
    return;
  }
  if (!logStreamController && !logRefreshTimer) startLogStream();
}

function isLogStreamAllowed() {
  return currentTab === 'logs' && currentLogPage === 1 && currentLogQuery === '';
}

function isLogRealtimeEnabled() {
  return !!document.getElementById('logAutoRefresh')?.checked;
}

async function startLogStream() {
  stopLogStream();
  stopLogAutoRefresh();
  if (!isLogStreamAllowed()) {
    setLogStreamStatus(currentLogQuery ? '搜索中' : '待机', 'paused');
    return;
  }
  const token = ++logStreamToken;
  const generation = authGeneration;
  const controller = new AbortController();
  logStreamController = controller;
  logGapRefreshPending = false;
  setLogStreamStatus('连接中', 'connecting');
  try {
    const response = await fetch('/admin/logs/stream', {
      credentials: 'same-origin',
      signal: controller.signal,
    });
    if (generation !== authGeneration) {
      controller.abort();
      return;
    }
    if (response.status === 401) {
      handleSessionAuthFailure({ _status: 401, error: 'UNAUTHORIZED' }, generation);
      return;
    }
    if (!response.ok || !response.body) throw new Error('日志流不可用');
    setLogStreamStatus('同步中', 'connecting');
    const streamEpoch = logViewEpoch;
    const hydrated = await loadLogs(false, 1, { skipRealtimeSync: true });
    if (!hydrated) throw new Error('日志补齐失败');
    if (token !== logStreamToken || streamEpoch !== logViewEpoch) return;
    setLogStreamStatus('实时', 'stream');
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      buffer = buffer.replace(/\r\n/g, '\n');
      buffer = consumeLogStreamBuffer(buffer, token, streamEpoch);
    }
    if (buffer.trim()) handleLogStreamFrame(buffer, token, streamEpoch);
    if (!controller.signal.aborted && token === logStreamToken) startLogAutoRefresh();
  } catch (e) {
    if (!controller.signal.aborted && token === logStreamToken) {
      controller.abort();
      startLogAutoRefresh();
    }
  } finally {
    if (logStreamController === controller) logStreamController = null;
  }
}

function consumeLogStreamBuffer(buffer, token, streamEpoch) {
  let index = buffer.indexOf('\n\n');
  while (index >= 0) {
    handleLogStreamFrame(buffer.slice(0, index), token, streamEpoch);
    buffer = buffer.slice(index + 2);
    index = buffer.indexOf('\n\n');
  }
  return buffer;
}

function handleLogStreamFrame(frame, token, streamEpoch) {
  if (token !== logStreamToken || streamEpoch !== logViewEpoch) return;
  const data = frame.split('\n')
    .filter(line => line.startsWith('data:'))
    .map(line => line.slice(5).trimStart())
    .join('\n');
  if (!data) return;
  try {
    handleLogStreamEntry(JSON.parse(data), token, streamEpoch);
  } catch (e) {
    return;
  }
}

function handleLogStreamEntry(entry, token, streamEpoch) {
  if (token !== logStreamToken || streamEpoch !== logViewEpoch) return;
  if (!entry || typeof entry !== 'object' || !isLogStreamAllowed()) return;
  if (entry.type === 'clear') {
    resetLogViewAfterClear();
    if (currentTab === 'logs') syncLogRealtime();
    return;
  }
  const id = logEntryId(entry);
  if (id > 0 && !noteLogStreamId(id)) return;
  if (!logMatchesCurrentFilter(entry)) return;
  if (id > 0 && id <= lastLogSeenId) return;
  appendLogLine(entry);
}

function noteLogStreamId(id) {
  if (id <= 0) return true;
  if (lastLogStreamId > 0 && id <= lastLogStreamId) return false;
  if (lastLogStreamId > 0 && id > lastLogStreamId + 1) {
    scheduleLogGapRefresh();
  }
  lastLogStreamId = id;
  return true;
}

/* 流里的序号跳号说明订阅缓冲溢出丢了条目，用一次快照补齐 */
function scheduleLogGapRefresh() {
  if (logGapRefreshPending) return;
  logGapRefreshPending = true;
  const expectedEpoch = logViewEpoch;
  const expectedToken = logStreamToken;
  setLogStreamStatus('同步中', 'connecting');
  Promise.resolve().then(async () => {
    try {
      await loadLogs(false, 1, { skipRealtimeSync: true });
    } catch (e) {
      return;
    } finally {
      logGapRefreshPending = false;
      if (expectedEpoch === logViewEpoch && expectedToken === logStreamToken && logStreamController && isLogStreamAllowed()) {
        setLogStreamStatus('实时', 'stream');
      }
    }
  });
}

function logMatchesCurrentFilter(entry) {
  if (currentLogLevelFilter !== 'all' && safeLogLevel(entry.level) !== currentLogLevelFilter) return false;
  if (!logEntryMatchesNode(entry, currentLogNodeFilter)) return false;
  const query = currentLogQuery.toLowerCase();
  if (query && !String(entry.line || '').toLowerCase().includes(query)) return false;
  return true;
}

function appendLogLine(entry) {
  const consoleEl = document.getElementById('logConsole');
  if (!consoleEl) return;
  const id = logEntryId(entry);
  if (id > 0 && id <= lastLogSeenId) return;
  const wasAtBottom = isAtBottom(consoleEl);
  if (consoleEl.querySelector('.lv-empty')) consoleEl.innerHTML = '';
  const holder = document.createElement('div');
  holder.innerHTML = logLineMarkup(entry);
  consoleEl.appendChild(holder.firstElementChild);
  while (consoleEl.children.length > LOG_DOM_LIMIT) {
    consoleEl.firstElementChild?.remove();
  }

  const previousTotal = Math.max(Number(lastLogMeta.totalEntries || 0), lastLogEntries.length);
  lastLogEntries = [...lastLogEntries, entry].slice(-LOG_DOM_LIMIT);
  lastLogLines = lastLogEntries.map(x => x.line || '');
  const totalEntries = previousTotal + 1;
  lastLogMeta = { ...lastLogMeta, newestId: entry.id || lastLogMeta.newestId, totalEntries };
  lastLogSeenId = Math.max(lastLogSeenId, id);
  currentLogTotalPages = Math.max(1, Math.ceil(totalEntries / LOG_PAGE_LIMIT));
  currentLogHasOlder = currentLogTotalPages > currentLogPage;
  updateLogSummary(lastLogEntries, lastLogMeta);
  updateLogPager();
  if (wasAtBottom) scrollLogsToBottom();
  else addPendingLogCount(1);
  queueTraceRender();
}

function startLogAutoRefresh() {
  stopLogAutoRefresh();
  if (!isLogStreamAllowed()) {
    setLogStreamStatus(currentLogQuery ? '搜索中' : '待机', 'paused');
    return;
  }
  setLogStreamStatus('已断开 · 每 5 秒刷新', 'polling');
  logRefreshTimer = setInterval(() => {
    if (isLogStreamAllowed()) loadLogs();
    else stopLogAutoRefresh();
  }, LOG_POLL_MS);
  loadLogs();
}

function stopLogAutoRefresh() {
  if (logRefreshTimer) {
    clearInterval(logRefreshTimer);
    logRefreshTimer = null;
  }
}

function stopLogStream() {
  logStreamToken++;
  if (logStreamController) {
    logStreamController.abort();
    logStreamController = null;
  }
}

function stopLogRealtime(status = '已关闭', mode = '') {
  stopLogStream();
  stopLogAutoRefresh();
  setLogStreamStatus(status, mode);
}

function disableLogAutoRefresh(status = '已关闭', mode = '') {
  const el = document.getElementById('logAutoRefresh');
  if (el) el.checked = false;
  stopLogRealtime(status, mode);
}

/* mode：stream 实时 / connecting 连接中 / polling 流断开改为轮询 / paused 待机 / '' 已关闭 */
function setLogStreamStatus(text, mode = '') {
  const el = document.getElementById('logStreamStatus');
  if (!el) return;
  const kind = mode === 'stream' ? 'ok' : mode === 'connecting' ? 'checking' : mode === 'polling' ? 'warn' : 'idle';
  const icon = mode === 'stream' ? 'i-ok' : mode === 'connecting' ? 'i-loader' : mode === 'polling' ? 'i-warn' : 'i-idle';
  el.className = `status ${kind}`;
  el.innerHTML = `${ico(icon)}<span></span>`;
  el.querySelector('span').textContent = text;
  el.title = mode === 'polling' ? '实时连接已断开，暂时改为每 5 秒刷新一次' : '';
  const reconnect = document.getElementById('logReconnectBtn');
  if (reconnect) reconnect.hidden = mode !== 'polling';
}

function isAtBottom(el) {
  if (!el) return true;
  return el.scrollHeight - el.scrollTop - el.clientHeight < 24;
}

function scrollLogsToBottom() {
  const consoleEl = document.getElementById('logConsole');
  if (consoleEl) consoleEl.scrollTop = consoleEl.scrollHeight;
  resetPendingLogCount();
}

function addPendingLogCount(count) {
  pendingLogCount += Math.max(0, Number(count || 0));
  updateLogNewBadge();
}

function resetPendingLogCount() {
  pendingLogCount = 0;
  updateLogNewBadge();
}

function updateLogNewBadge() {
  const el = document.getElementById('logNewBadge');
  if (!el) return;
  el.hidden = pendingLogCount <= 0 || currentLogMode !== 'line';
  el.textContent = `↓ ${pendingLogCount} 条新日志`;
}

function copyLogs() {
  if (!lastLogLines.length) {
    toast('warn', '没有可复制的日志');
    return;
  }
  copyText(lastLogLines.join('\n'), '已复制日志', `${formatCount(lastLogLines.length)} 行`);
}

/* ============================================================
 * 按请求视图：按 requestId 聚合，最新的请求在最上面
 * ============================================================ */

const TRACE_RENDER_LIMIT = 300;
const TRACE_OTHERS_LIMIT = 60;
const TRACE_OTHERS_KEY = '__trace_others__';
const TRACE_LEVEL_WEIGHT = { debug: 0, info: 1, warn: 2, error: 3 };
let traceGroupIndex = new Map();
let traceOpenState = new Map();

function setLogMode(mode) {
  currentLogMode = mode === 'line' ? 'line' : 'req';
  const lineView = document.getElementById('logLineView');
  const traceView = document.getElementById('logTraceView');
  const body = document.getElementById('logBody');
  if (lineView) lineView.hidden = currentLogMode !== 'line';
  if (traceView) traceView.hidden = currentLogMode !== 'req';
  if (body) body.classList.toggle('is-line', currentLogMode === 'line');
  document.querySelectorAll('#logModeSeg [data-mode]').forEach(btn => btn.setAttribute('aria-pressed', String(btn.dataset.mode === currentLogMode)));
  if (currentLogMode === 'req') renderTrace(lastLogEntries);
  else scrollLogsToBottom();
  updateLogNewBadge();
}

/* 实时流每来一行都要重新聚合，合并到下一帧只渲染一次 */
function queueTraceRender() {
  if (currentLogMode !== 'req' || traceRenderQueued) return;
  traceRenderQueued = true;
  requestAnimationFrame(() => {
    traceRenderQueued = false;
    renderTrace(lastLogEntries);
  });
}

/* 解析 `key=value` / `key="value with space"` 形式的日志字段 */
function parseLogMeta(text) {
  const meta = {};
  const re = /([A-Za-z][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*"|\S*)/g;
  let m;
  while ((m = re.exec(String(text || '')))) {
    let value = m[2];
    if (value.startsWith('"')) {
      try {
        value = JSON.parse(value);
      } catch (e) {
        value = value.slice(1, -1);
      }
    }
    if (meta[m[1]] === undefined) meta[m[1]] = value;
  }
  return meta;
}

function traceTimeText(value) {
  const text = String(value || '');
  const m = text.match(/T(\d{2}:\d{2}:\d{2})/);
  return m ? m[1] : text;
}

function traceNumber(value) {
  if (value === undefined || value === null || value === '') return -1;
  const n = Number(value);
  return Number.isFinite(n) && n >= 0 ? n : -1;
}

function parseLogRequest(entry) {
  const raw = String(entry?.line || '');
  const result = {
    entry,
    raw,
    level: safeLogLevel(entry?.level),
    time: '',
    timeText: '',
    status: 0,
    scope: '',
    event: '',
    id: '',
    method: '',
    uri: '',
    ip: '',
    node: '',
    bytes: -1,
    totalMs: -1,
    bodyMs: -1,
  };
  let rest = raw;
  const head = rest.match(/^(\S+)\s+\[([A-Za-z]+)\]\s*/);
  if (head) {
    result.time = head[1];
    result.timeText = traceTimeText(head[1]);
    rest = rest.slice(head[0].length);
  }
  let tag;
  while ((tag = rest.match(/^\[([^\]]*)\]\s*/))) {
    const value = tag[1];
    if (/^\d{3}$/.test(value)) result.status = Number(value);
    else if (!result.scope) result.scope = value;
    rest = rest.slice(tag[0].length);
  }
  const meta = parseLogMeta(rest);
  result.event = String(meta.event || '');
  result.id = String(meta.id || '');
  result.method = String(meta.method || '');
  result.uri = String(meta.uri || '');
  result.ip = String(meta.ip || '');
  result.node = String(meta.node || '');
  result.bytes = traceNumber(meta.bytes);
  result.totalMs = traceNumber(meta.totalMs);
  result.bodyMs = traceNumber(meta.bodyMs);
  if (result.status === 0) result.status = Math.max(0, traceNumber(meta.status));
  return result;
}

function formatTraceMs(ms) {
  const value = Number(ms);
  if (!Number.isFinite(value) || value < 0) return '—';
  if (value >= 1000) return `${(value / 1000).toFixed(1)} s`;
  return `${Math.round(value)} ms`;
}

function isTraceError(group) {
  return group.status >= 500 || group.level === 'error';
}

function traceCardOpen(group) {
  const explicit = traceOpenState.get(group.id);
  if (typeof explicit === 'boolean') return explicit;
  return isTraceError(group);
}

function toggleTraceCard(id) {
  const key = String(id || '');
  const group = traceGroupIndex.get(key);
  if (!group) return;
  if (traceOpenState.size > 2000) traceOpenState.clear();
  traceOpenState.set(key, !traceCardOpen(group));
  renderTrace(lastLogEntries);
}

function copyTraceRaw(id) {
  const group = traceGroupIndex.get(String(id || ''));
  if (!group || !group.items.length) {
    toast('warn', '没有可复制的原始日志');
    return;
  }
  copyText(group.items.map(item => item.raw).join('\n'), '已复制原始日志', `${group.items.length} 行`);
}

function traceBarRow(name, ms, maxMs, kind) {
  const value = Math.max(0, Number(ms || 0));
  const width = maxMs > 0 ? Math.max(2, Math.min(100, value / maxMs * 100)) : 2;
  return `<div class="lv-bar">
    <span class="lv-bar-name">${esc(name)}</span>
    <span class="lv-bar-track"><span class="lv-bar-fill ${kind}" style="width:${width.toFixed(1)}%"></span></span>
    <span class="lv-bar-val">${esc(formatTraceMs(value))}</span>
  </div>`;
}

function traceItemsMarkup(items) {
  return `<div class="lv-items">${items.map(item => {
    const p = splitLogLine(item.entry);
    return `<div class="lv-item"><span class="lv-time">${esc(p.time || '—')}</span>${logLevelPill(item.level)}<span class="lv-msg">${p.scope ? `<span class="lv-scope">${esc(p.scope)}</span> ` : ''}${esc(p.text)}</span></div>`;
  }).join('')}</div>`;
}

function renderTrace(entries) {
  const el = document.getElementById('traceList');
  if (!el) return;
  if (currentLogMode !== 'req') return;
  const list = Array.isArray(entries) ? entries : [];
  const groups = [];
  const index = new Map();
  const others = [];
  for (const entry of list) {
    const parsed = parseLogRequest(entry);
    if (!parsed.id) {
      others.push(parsed);
      continue;
    }
    let group = index.get(parsed.id);
    if (!group) {
      group = {
        id: parsed.id, items: [], status: 0, method: '', uri: '', ip: '', node: '',
        scope: '', timeText: '', level: 'debug', bytes: -1, totalMs: -1, bodyMs: -1,
      };
      index.set(parsed.id, group);
      groups.push(group);
    }
    group.items.push(parsed);
    if (parsed.status > 0) group.status = parsed.status;
    if (parsed.method) group.method = parsed.method;
    if (parsed.uri) group.uri = parsed.uri;
    if (parsed.ip) group.ip = parsed.ip;
    if (parsed.node) group.node = parsed.node;
    if (parsed.scope) group.scope = parsed.scope;
    if (parsed.timeText) group.timeText = parsed.timeText;
    if (parsed.bytes >= 0) group.bytes = parsed.bytes;
    if (parsed.totalMs >= 0) group.totalMs = parsed.totalMs;
    if (parsed.bodyMs >= 0) group.bodyMs = parsed.bodyMs;
    if ((TRACE_LEVEL_WEIGHT[parsed.level] || 0) > (TRACE_LEVEL_WEIGHT[group.level] || 0)) group.level = parsed.level;
  }

  // 完成行只带 uri 不带 node=，按路径第一段对上已知节点，卡片里才能显示节点名。
  const knownNodes = new Set((Array.isArray(logNodes) ? logNodes : []).map(n => n.name));
  for (const group of groups) {
    if (group.node || !group.uri) continue;
    const first = group.uri.split('?')[0].split('/').filter(Boolean)[0] || '';
    if (knownNodes.has(first)) group.node = first;
  }

  const visible = groups.slice(-TRACE_RENDER_LIMIT).reverse();
  const otherItems = others.slice(-TRACE_OTHERS_LIMIT);
  traceGroupIndex = new Map(visible.map(group => [group.id, group]));

  const total = Math.max(0, Number(lastLogMeta?.totalEntries ?? list.length));
  const foot = `<div class="lv-foot"><span>${esc(`共 ${formatCount(groups.length)} 个请求 · ${formatCount(total)} 行日志 · 内存 ${formatCount(lastLogMeta?.capacity || 0)} 条轮转`)}</span>${groups.length > visible.length ? `<span>仅显示最近 ${visible.length} 个请求</span>` : ''}</div>`;

  if (!visible.length && !otherItems.length) {
    if (!list.length) {
      el.innerHTML = logsLoadedOnce || logsLoadFailed ? logsEmptyMarkup() : logsSkeleton();
      return;
    }
    el.innerHTML = `<div class="empty lv-empty"><div class="empty-icon">${ico('i-logs')}</div><h3>暂无请求记录</h3><p>有客户端访问后，这里会按 requestId 聚合每个请求的耗时。</p></div>` + foot;
    return;
  }

  const maxMs = Math.max(1, ...visible.map(group => Math.max(0, group.totalMs)));
  const cards = visible.map(group => {
    const open = traceCardOpen(group);
    const error = isTraceError(group);
    const target = `${group.method ? group.method + ' ' : ''}${group.uri || '（未知路径）'}`;
    const nodeText = group.node ? logNodeLabel(group.node) : (group.scope || '—');
    const meta = [group.id, nodeText, group.ip || '—', group.timeText || '—'];
    const bars = [];
    if (group.bodyMs >= 0 && group.totalMs >= 0) {
      bars.push(traceBarRow('首字节', Math.max(0, group.totalMs - group.bodyMs), maxMs, 'start'));
      bars.push(traceBarRow('完成', group.totalMs, maxMs, error ? 'bad' : 'final'));
    } else if (group.totalMs >= 0) {
      bars.push(traceBarRow('完成', group.totalMs, maxMs, error ? 'bad' : 'final'));
    }
    const extra = [];
    if (group.bytes >= 0) extra.push(`响应 ${formatBytes(group.bytes)}`);
    extra.push(`${group.items.length} 条日志`);
    const body = open ? `<div class="lv-card-body">
      ${bars.length ? `<div class="lv-bars">${bars.join('')}</div>` : ''}
      <div class="lv-extra">${esc(extra.join(' · '))}</div>
      ${traceItemsMarkup(group.items)}
      <div class="lv-card-actions">
        <button class="btn btn-ghost btn-sm" type="button" data-act="logs-copy-id" data-id="${attr(group.id)}">${ico('i-copy')}复制 requestId</button>
        <button class="btn btn-ghost btn-sm" type="button" data-act="logs-copy-raw" data-id="${attr(group.id)}">${ico('i-file')}复制原始日志</button>
      </div>
    </div>` : '';
    return `<article class="lv-card${error ? ' is-error' : ''}${open ? ' is-open' : ''}" data-request-id="${attr(group.id)}">
      <button class="lv-card-top" type="button" data-act="logs-toggle" data-id="${attr(group.id)}" aria-expanded="${open}">
        ${statusCodeChip(group.status)}
        <span class="lv-target mono">${esc(target)}</span>
        <span class="lv-ms num">${esc(formatTraceMs(group.totalMs))}</span>
        ${ico('i-down', 'lv-chev')}
      </button>
      <div class="lv-card-meta"><span class="mono">${esc(meta[0])}</span><span>${esc(meta[1])}</span><span class="mono">${esc(meta[2])}</span><span class="num">${esc(meta[3])}</span></div>
      ${body}
    </article>`;
  }).join('');

  let othersCard = '';
  if (otherItems.length) {
    const othersOpen = traceOpenState.get(TRACE_OTHERS_KEY) === true;
    traceGroupIndex.set(TRACE_OTHERS_KEY, { id: TRACE_OTHERS_KEY, items: otherItems, status: 0, level: 'info' });
    othersCard = `<article class="lv-card lv-others${othersOpen ? ' is-open' : ''}" data-request-id="${attr(TRACE_OTHERS_KEY)}">
      <button class="lv-card-top" type="button" data-act="logs-toggle" data-id="${attr(TRACE_OTHERS_KEY)}" aria-expanded="${othersOpen}">
        <span class="lv-code idle">其他</span>
        <span class="lv-target">未关联 requestId 的日志</span>
        <span class="lv-ms num">${otherItems.length} 条</span>
        ${ico('i-down', 'lv-chev')}
      </button>
      ${othersOpen ? `<div class="lv-card-body">${traceItemsMarkup(otherItems.slice().reverse())}
        <div class="lv-card-actions"><button class="btn btn-ghost btn-sm" type="button" data-act="logs-copy-raw" data-id="${attr(TRACE_OTHERS_KEY)}">${ico('i-file')}复制原始日志</button></div></div>` : ''}
    </article>`;
  }

  el.innerHTML = cards + othersCard + foot;
}

/* ============================================================
 * 注册
 * ============================================================ */

registerActions({
  'logs-copy': () => copyLogs(),
  'logs-clear': () => clearLogs(),
  'logs-refresh': () => loadLogs(true),
  'logs-latest': () => loadLatestLogs(true),
  'logs-newer': () => loadNewerLogs(),
  'logs-older': () => loadOlderLogs(),
  'logs-reconnect': () => { if (isLogRealtimeEnabled()) startLogStream(); },
  'logs-scroll-bottom': () => scrollLogsToBottom(),
  'logs-clear-filter': () => clearLogFilters(),
  'logs-mode': el => setLogMode(el.dataset.mode),
  'logs-toggle': el => toggleTraceCard(el.dataset.id),
  'logs-copy-id': el => copyText(el.dataset.id, '已复制 requestId', el.dataset.id),
  'logs-copy-raw': el => copyTraceRaw(el.dataset.id),
});

/* 节点页「查看日志」：切到日志页并预选该节点 */
function openLogsForNode(name) {
  currentLogNodeFilter = String(name || '').trim();
  currentLogPage = 1;
  if (logsRendered) {
    const sel = document.getElementById('logNodeFilter');
    if (sel) {
      if (![...sel.options].some(o => o.value === currentLogNodeFilter)) {
        const opt = document.createElement('option');
        opt.value = currentLogNodeFilter;
        opt.textContent = currentLogNodeFilter;
        sel.appendChild(opt);
      }
      sel.value = currentLogNodeFilter;
    }
  }
  if (currentTab === 'logs') {
    loadLogs(false, 1);
    loadLogNodes();
  } else {
    switchTab('logs');
  }
}

registerPage('logs', {
  enter() {
    if (!logsRendered) renderLogsPage();
    const sel = document.getElementById('logNodeFilter');
    if (sel && currentLogNodeFilter && ![...sel.options].some(o => o.value === currentLogNodeFilter)) {
      const opt = document.createElement('option');
      opt.value = currentLogNodeFilter;
      opt.textContent = currentLogNodeFilter;
      sel.appendChild(opt);
      sel.value = currentLogNodeFilter;
    }
    loadLogNodes();
    loadLogs();
    if (isLogRealtimeEnabled()) syncLogRealtime();
  },
  leave() {
    if (logSearchTimer) { clearTimeout(logSearchTimer); logSearchTimer = null; }
    stopLogRealtime('待机', 'paused');
  },
  reset() {
    if (logSearchTimer) { clearTimeout(logSearchTimer); logSearchTimer = null; }
    stopLogStream();
    stopLogAutoRefresh();
    logViewEpoch++;
    lastLogLines = [];
    lastLogEntries = [];
    lastLogMeta = {};
    currentLogPage = 1;
    currentLogTotalPages = 1;
    currentLogHasOlder = false;
    currentLogLevelFilter = 'all';
    currentLogQuery = '';
    currentLogNodeFilter = '';
    currentLogMode = 'req';
    lastLogSeenId = 0;
    lastLogStreamId = 0;
    logGapRefreshPending = false;
    pendingLogCount = 0;
    logsLoadedOnce = false;
    logsLoadFailed = false;
    logNodes = [];
    traceOpenState = new Map();
    traceGroupIndex = new Map();
    const page = document.getElementById('pageLogs');
    if (page) page.innerHTML = '';
    logsRendered = false;
  },
});
