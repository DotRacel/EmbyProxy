/* ============================================================
 * nodes.js — 节点页：列表 / 详情 / 延迟曲线 / 表格 / 编辑弹窗 / 导入
 * ============================================================ */

const PROBE_POLL_MS = 30000;
const RELATIVE_TICK_MS = 1000;
const SERIES_RANGES = {
  '1h': { hours: 1, buckets: 60, step: '1 分钟', tick: 10 },
  '6h': { hours: 6, buckets: 180, step: '2 分钟', tick: 60 },
  '24h': { hours: 24, buckets: 288, step: '5 分钟', tick: 180 },
};
const SERIES_COLORS = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)'];

const N = {
  loaded: false,
  loading: false,
  loadError: '',
  nodes: [],
  probes: {},
  probeMeta: { intervalMs: 60000, retentionHours: 24, enabled: true },
  todayOutbound: {},
  todaySessions: {},
  todaySessionTotal: 0,
  selected: '',
  q: '',
  status: 'all',
  tag: 'all',
  view: 'split',
  pane: 'list',
  rows: new Set(),
  sort: { key: 'name', dir: 1 },
  checking: new Set(),
  checkAll: null,
  range: '6h',
  chartMode: 'lines',
  hidden: new Set(),
  series: null,        // { name, range, mode, points, targets }
  seriesState: 'idle', // idle | loading | ready | error
  seriesToken: 0,
};
let probeTimer = null;
let relativeTimer = null;
let chartCtx = null;

try {
  const v = localStorage.getItem('ep_nodes_view');
  if (v === 'split' || v === 'table') N.view = v;
  const r = localStorage.getItem('ep_series_range');
  if (SERIES_RANGES[r]) N.range = r;
} catch (_) { /* 存储不可用时用默认值 */ }

/* ============================================================
 * 数据
 * ============================================================ */

function splitTargets(target) {
  return String(target || '').split(/[\n;,，；|]+/g).map(t => t.trim()).filter(Boolean);
}

function normalizeImpersonateProfile(profile) {
  const value = String(profile || '').toLowerCase();
  return IMPERSONATE_LABELS[value] ? value : 'yamby';
}

function nodeTitle(n) { return n.displayName || n.name; }

function nodeProxyUrl(n, withSecret = true) {
  const base = `${location.origin}/${n.name}`;
  return n.secret && withSecret ? `${base}/${n.secret}` : base;
}

function nodeProbe(name) { return N.probes[name] || null; }

/* 节点状态：未检测 / 故障（所有线路失败）/ 降级（节点可用但有线路失败）/ 正常。
 * 降级按节点当前配置的线路判断：探测数据可能还带着刚删掉的线路，不能直接看 targets。 */
function nodeStatus(name) {
  const p = nodeProbe(name);
  if (!p || !p.probed) return 'idle';
  if (!p.ok) return 'bad';
  const n = N.nodes.find(x => x.name === name);
  if (n ? nodeLines(n).some(l => l.status === 'bad') : (p.targets || []).some(t => t.at && !t.ok)) return 'warn';
  return 'ok';
}

function effStatus(name) { return N.checking.has(name) ? 'checking' : nodeStatus(name); }

/* 线路列表：以节点配置为准，合并最新探测结果 */
function nodeLines(n) {
  const p = nodeProbe(n.name);
  const stats = new Map((p?.targets || []).map(t => [t.target, t]));
  return splitTargets(n.target).map((url, i) => {
    const t = stats.get(url) || null;
    const st = !t || !t.at ? 'idle' : t.ok ? 'ok' : 'bad';
    // 代理只在这条线路最近一次检测成功时才算「承载中」，故障节点不标
    return { url, host: hostOf(url), index: i, stat: t, status: st, serving: st === 'ok' && !!p?.activeTarget && p.activeTarget === url };
  });
}

function roleName(i) { return i === 0 ? '主线' : `备用 ${i}`; }

function visibleNodes() {
  const q = N.q.trim().toLowerCase();
  return N.nodes.filter(n => {
    if (N.status !== 'all' && nodeStatus(n.name) !== N.status) return false;
    if (N.tag !== 'all' && (n.tag || '') !== N.tag) return false;
    if (q && ![n.name, n.displayName, n.tag].some(v => String(v || '').toLowerCase().includes(q))) return false;
    return true;
  }).sort((a, b) => (Number(!!b.fav) - Number(!!a.fav)) || a.name.localeCompare(b.name));
}

function currentNode() { return N.nodes.find(n => n.name === N.selected) || null; }

/* 批量操作只作用于「已选且在当前筛选结果里」的节点，被筛选隐藏的选择不受影响 */
function selectedVisible() {
  return visibleNodes().filter(n => N.rows.has(n.name)).map(n => n.name);
}

async function loadNodes() {
  N.loading = !N.loaded;
  if (N.loading) renderNodesPage();
  const r = await api('list');
  N.loading = false;
  if (!r.ok) {
    if (r._stale) return;
    N.loadError = apiError(r, '读取节点失败');
    renderNodesPage();
    return;
  }
  N.loadError = '';
  setAppVersion(r.build);
  N.nodes = Array.isArray(r.nodes) ? r.nodes : [];
  N.loaded = true;
  syncNodeState();
  renderNodesPage();
  loadProbes();
  loadTodayTraffic();
}

function syncNodeState() {
  const names = new Set(N.nodes.map(n => n.name));
  N.rows.forEach(name => { if (!names.has(name)) N.rows.delete(name); });
  N.checking.forEach(name => { if (!names.has(name)) N.checking.delete(name); });
  if (N.selected && !names.has(N.selected)) N.selected = '';
  if (!N.selected) N.selected = visibleNodes()[0]?.name || N.nodes[0]?.name || '';
  if (N.series && N.series.name !== N.selected) { N.series = null; N.seriesState = 'idle'; N.hidden.clear(); }
  const count = document.getElementById('navCount');
  if (count) count.textContent = N.nodes.length ? String(N.nodes.length) : '';
}

async function loadProbes() {
  const r = await api('probe.summary');
  if (!r.ok) return;
  const map = {};
  for (const p of (Array.isArray(r.probes) ? r.probes : [])) if (p && p.name) map[p.name] = p;
  N.probes = map;
  N.probeMeta = {
    intervalMs: Number(r.intervalMs || 60000),
    retentionHours: Number(r.retentionHours || 24),
    enabled: r.enabled !== false,
  };
  renderNodesPage({ keepChart: true });
  // 曲线跟着探测一起刷新，否则详情页会一直停在打开那一刻的快照。
  if (N.selected) loadSeries(N.selected, { quiet: true });
}

async function loadTodayTraffic() {
  const r = await api('stats.get', { days: 1 });
  if (!r.ok) return;
  const outbound = {};
  const sessions = {};
  let total = 0;
  for (const s of (Array.isArray(r.stats) ? r.stats : [])) {
    const node = String(s.node || '');
    outbound[node] = (outbound[node] || 0) + Number(s.outboundBytes || 0);
    sessions[node] = (sessions[node] || 0) + Number(s.sessions || 0);
    total += Number(s.sessions || 0);
  }
  N.todayOutbound = outbound;
  N.todaySessions = sessions;
  N.todaySessionTotal = total;
  renderNodesPage({ keepChart: true });
}

async function loadSeries(name, { quiet = false } = {}) {
  const range = N.range;
  const mode = N.chartMode;
  const cfg = SERIES_RANGES[range];
  const token = ++N.seriesToken;
  if (!quiet || !N.series || N.series.name !== name || N.series.range !== range) {
    N.seriesState = N.series && N.series.name === name ? 'refresh' : 'loading';
    drawChart();
  }
  const r = await api('probe.series', { name, hours: cfg.hours, buckets: cfg.buckets, perTarget: mode === 'lines' });
  if (token !== N.seriesToken) return;
  if (!r.ok) {
    if (r._stale) return;
    N.seriesState = 'error';
    N.seriesError = apiError(r, '读取曲线失败');
    drawChart();
    return;
  }
  N.series = { name, range, mode, points: Array.isArray(r.points) ? r.points : [], targets: Array.isArray(r.targets) ? r.targets : [] };
  N.seriesState = 'ready';
  drawChart();
  updateCallout();
}

function startNodeTimers() {
  stopNodeTimers();
  probeTimer = setInterval(() => {
    if (currentTab !== 'nodes') { stopNodeTimers(); return; }
    loadProbes();
  }, PROBE_POLL_MS);
  relativeTimer = setInterval(tickRelative, RELATIVE_TICK_MS);
}

function stopNodeTimers() {
  if (probeTimer) { clearInterval(probeTimer); probeTimer = null; }
  if (relativeTimer) { clearInterval(relativeTimer); relativeTimer = null; }
}

function tickRelative() {
  $$('[data-ago]').forEach(el => {
    const at = Number(el.dataset.ago);
    if (!at) return;
    const [v, u] = formatSpan(Date.now() - at).split(' ');
    if (el.dataset.agoMode === 'kpi') el.innerHTML = `${esc(v)}<small>${esc(u)}</small>`;
    else el.textContent = `${v} ${u}前`;
  });
}

/* ============================================================
 * 渲染：页面骨架
 * ============================================================ */

function renderNodesPage({ keepChart = false } = {}) {
  const page = document.getElementById('pageNodes');
  if (!page) return;
  if (!page.dataset.ready) {
    page.innerHTML = `<header class="page-head">
        <div class="page-title"><h1>节点</h1></div>
        <div class="gstats" id="gstats"></div>
        <div class="gactions" id="gactions"></div>
      </header>
      <div class="toolbar" id="nodesToolbar"></div>
      <div class="page-region" id="nodesRegion" style="display:grid;grid-template-rows:auto minmax(0,1fr)"></div>`;
    page.dataset.ready = '1';
  }
  renderHead();
  renderToolbar();
  renderRegion(keepChart);
}

function renderHead() {
  const okMs = N.nodes.map(n => nodeProbe(n.name)).filter(p => p && p.probed && p.ok && p.lastMs >= 0).map(p => p.lastMs);
  const avg = okMs.length ? Math.round(okMs.reduce((a, b) => a + b, 0) / okMs.length) : null;
  $('#gstats').innerHTML = N.nodes.length ? `
    <div class="gstat"><b class="num">${formatCount(N.todaySessionTotal)}<small>个</small></b><span>今日会话</span></div>
    <div class="gstat"><b class="num">${avg ?? '—'}<small>ms</small></b><span>平均首字节</span></div>` : '';
  const ca = N.checkAll;
  $('#gactions').innerHTML = `
    ${ca ? `<button class="btn" data-act="nodes-cancel-all" title="停止检测剩余节点"><span class="spin"></span><span class="btn-label">检测中 ${ca.done}/${ca.total}</span></button>`
      : `<button class="btn" data-act="nodes-check-all" ${N.nodes.length ? '' : 'disabled'} title="检测当前筛选结果中的全部节点">${ico('i-probe')}<span class="btn-label">全部检测</span></button>`}
    <button class="btn ${N.nodes.length || !N.loaded ? 'btn-primary' : ''}" data-act="nodes-new">${ico('i-plus')}<span class="btn-label">新增节点</span></button>
    <button class="btn btn-icon" data-act="nodes-global-menu" aria-label="更多：导入、导出" aria-haspopup="menu">${ico('i-more')}</button>`;
}

function renderToolbar() {
  const bar = $('#nodesToolbar');
  if (!N.nodes.length) { bar.hidden = true; return; }
  bar.hidden = false;
  const cnt = st => st === 'all' ? N.nodes.length : N.nodes.filter(n => nodeStatus(n.name) === st).length;
  const seg = (st, label, color) => `<button aria-pressed="${N.status === st}" data-act="nodes-status" data-status="${st}">${color ? `<i class="sdot" style="background:var(--${color})"></i>` : ''}${label} <span class="n">${cnt(st)}</span></button>`;
  const tags = [...new Set(N.nodes.map(n => n.tag).filter(Boolean))].sort((a, b) => a.localeCompare(b, 'zh-CN'));
  if (N.tag !== 'all' && !tags.includes(N.tag)) N.tag = 'all';
  const active = document.activeElement;
  const searchFocused = active && active.id === 'nodeSearch';
  const pos = searchFocused ? active.selectionStart : null;
  bar.innerHTML = `
    <label class="input-affix search">${ico('i-search')}<input class="input" id="nodeSearch" data-search type="search" placeholder="搜索名称、路径或标签" value="${attr(N.q)}" aria-label="搜索节点"></label>
    <div class="seg statusseg" role="group" aria-label="按状态筛选">${seg('all', '全部')}${seg('ok', '正常', 'ok')}${seg('warn', '降级', 'warn')}${seg('bad', '故障', 'bad')}${seg('idle', '未检测', 'idle')}</div>
    <select class="select tagsel" id="nodeTagFilter" aria-label="按标签筛选"><option value="all">全部标签</option>${tags.map(t => `<option value="${attr(t)}" ${N.tag === t ? 'selected' : ''}>${esc(t)}</option>`).join('')}</select>
    <div class="grow"></div>
    <div class="seg viewtoggle" role="group" aria-label="视图">
      <button aria-pressed="${N.view === 'split'}" data-act="nodes-view" data-view="split" title="列表与详情分栏，适合逐个查看">${ico('i-split')}分栏</button>
      <button aria-pressed="${N.view === 'table'}" data-act="nodes-view" data-view="table" title="表格，适合排序、比较与批量操作">${ico('i-table')}表格</button>
    </div>`;
  if (searchFocused) { const el = $('#nodeSearch'); el.focus(); try { el.setSelectionRange(pos, pos); } catch (_) { /* search 输入框在部分浏览器不支持选区 */ } }
}

function renderRegion(keepChart) {
  const reg = $('#nodesRegion');
  const ca = N.checkAll;
  const prog = ca ? `<div class="progress"><i style="width:${(ca.done / ca.total) * 100}%"></i></div>` : '<div></div>';
  if (N.loading) { reg.innerHTML = prog + skeletonView(); return; }
  if (N.loadError && !N.loaded) {
    reg.innerHTML = prog + `<div class="full-empty"><div class="empty"><div class="empty-icon" style="color:var(--bad)">${ico('i-bad')}</div><h3>节点读取失败</h3><p></p><button class="btn btn-sm" data-act="nodes-reload">${ico('i-probe')}重试</button></div></div>`;
    reg.querySelector('.empty p').textContent = N.loadError;
    return;
  }
  if (!N.nodes.length) { reg.innerHTML = prog + fullEmptyView(); return; }
  const view = isNarrow() ? 'split' : N.view;
  // 窄屏只显示一栏：当前节点被筛选隐藏时回到列表，避免停在空详情页
  if (isNarrow() && N.pane === 'detail' && !visibleNodes().some(n => n.name === N.selected)) N.pane = 'list';
  if (view === 'table') {
    const old = $('#tview');
    const st = old ? old.scrollTop : 0;
    reg.innerHTML = prog + `<div class="tview" id="tview">${tableView()}</div>`;
    $('#tview').scrollTop = st;
    return;
  }
  // 列表、详情各自保留滚动位置；只有图表区在 keepChart 时原样保留，避免轮询刷新时闪烁
  const listScroll = $('.list-scroll', reg)?.scrollTop || 0;
  const detailScroll = $('#nodeDetail', reg)?.scrollTop || 0;
  const oldChart = keepChart ? $('#chartPanel') : null;
  const sameNode = oldChart && oldChart.dataset.node === N.selected;
  reg.innerHTML = prog + `<div class="content" data-pane="${N.pane}"><section class="list" aria-label="节点列表">${listView()}</section><section class="detail" id="nodeDetail" aria-label="节点详情">${detailView()}</section></div>`;
  $('.list-scroll', reg).scrollTop = listScroll;
  $('#nodeDetail', reg).scrollTop = detailScroll;
  const panel = $('#chartPanel');
  if (panel && sameNode) panel.replaceWith(oldChart);
  else drawChart();
  tickRelative();
}

function skeletonView() {
  const rows = Array.from({ length: 7 }, () => `<div style="display:grid;gap:7px;padding:12px 10px"><div class="skel" style="height:12px;width:60%"></div><div class="skel" style="height:10px;width:40%"></div></div>`).join('');
  return `<div class="content"><section class="list"><div class="list-head"><span>正在读取节点…</span></div><div class="list-scroll">${rows}</div></section>
    <section class="detail"><div class="detail-inner"><div class="skel" style="height:26px;width:40%"></div><div class="skel" style="height:86px"></div><div class="skel" style="height:280px"></div></div></section></div>`;
}

function fullEmptyView() {
  return `<div class="full-empty"><div class="empty">
    <div class="empty-icon">${ico('i-nodes')}</div>
    <h3>还没有 Emby 节点</h3>
    <p>添加第一个节点后，客户端就能通过代理地址访问对应的 Emby 服务器，这里会显示它的延迟和可用率。</p>
    <div class="steps">
      <div><i>1</i>填写节点路径，例如 <span class="mono">hk-01</span>，它决定代理地址</div>
      <div><i>2</i>添加一条或多条上游线路，按顺序故障转移</div>
      <div><i>3</i>保存后自动开始每分钟检测</div>
    </div>
    <div style="display:flex;gap:8px;margin-top:8px;flex-wrap:wrap;justify-content:center">
      <button class="btn btn-primary" data-act="nodes-new">${ico('i-plus')}新增节点</button>
      <button class="btn" data-act="nodes-import">${ico('i-upload')}导入 JSON</button>
    </div></div></div>`;
}

/* ============================================================
 * 列表
 * ============================================================ */

function sparkSvg(name) {
  const p = nodeProbe(name);
  const pts = (p?.spark || []).map(Number);
  const vals = pts.filter(v => v >= 0);
  if (vals.length < 2) return '<svg class="spark" aria-hidden="true"></svg>';
  const mx = Math.max(...vals) * 1.1;
  const mn = Math.min(...vals) * 0.85;
  const span = (mx - mn) || 1;
  const last = pts.length - 1;
  const col = nodeStatus(name) === 'bad' ? 'var(--idle)' : 'var(--accent)';
  let d = '';
  let pen = false;
  pts.forEach((v, i) => {
    if (v < 0) { pen = false; return; }
    const x = i / last * 62 + 1;
    const y = 17 - (v - mn) / span * 15;
    d += (pen ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
    pen = true;
  });
  let li = last;
  while (li >= 0 && pts[li] < 0) li--;
  const end = li >= 0 ? `<circle cx="${(li / last * 62 + 1).toFixed(1)}" cy="${(17 - (pts[li] - mn) / span * 15).toFixed(1)}" r="2" fill="${col}"/>` : '';
  return `<svg class="spark" viewBox="0 0 64 18" aria-hidden="true"><path d="${d}" fill="none" stroke="${col}" stroke-width="1.4" stroke-linejoin="round"/>${end}</svg>`;
}

function rowMetric(n) {
  const st = effStatus(n.name);
  const p = nodeProbe(n.name);
  if (st === 'checking') return `<span class="err checking"><span class="spin" style="width:11px;height:11px;border-width:1.6px"></span>检测中</span>`;
  if (st === 'idle') return `<span class="err idle">${N.probeMeta.enabled ? '未检测' : '检测已关闭'}</span>`;
  if (st === 'bad') return `<span class="err bad">${esc(p.lastError || '不可用')}</span>${sparkSvg(n.name)}`;
  return `<b>${esc(p.lastMs)}<small>ms</small></b>${sparkSvg(n.name)}`;
}

function filterDescription() {
  const parts = [];
  if (N.q) parts.push(`名称、路径或标签包含「${N.q}」`);
  if (N.status !== 'all') parts.push(`状态为「${STATUS[N.status].label}」`);
  if (N.tag !== 'all') parts.push(`标签为「${N.tag}」`);
  return parts.join('，');
}

function listView() {
  const list = visibleNodes();
  const filtered = N.q || N.status !== 'all' || N.tag !== 'all';
  let body;
  if (!list.length) {
    body = `<div class="empty" style="padding-top:48px"><div class="empty-icon">${ico('i-search')}</div><h3>没有匹配的节点</h3><p>没有${esc(filterDescription())}的节点。</p><button class="btn btn-sm" data-act="nodes-clear-filter">清除筛选</button></div>`;
  } else {
    body = list.map(n => {
      const lines = splitTargets(n.target).length;
      return `<button class="nrow" role="option" aria-selected="${n.name === N.selected}" data-act="nodes-select" data-node="${attr(n.name)}" title="${attr(nodeTitle(n))}">
        ${statusDot(effStatus(n.name))}
        <div class="nrow-name">${n.fav ? ico('i-pin') : ''}<span>${esc(nodeTitle(n))}</span></div>
        <div class="nrow-metric">${rowMetric(n)}</div>
        <div class="nrow-meta"><span class="mono">/${esc(n.name)}</span><span>·</span><span>${lines} 线路</span>${n.tag ? `<span class="chip">${esc(n.tag)}</span>` : ''}</div>
      </button>`;
    }).join('');
  }
  return `<div class="list-head"><span>${filtered ? `筛选出 ${list.length} / ${N.nodes.length} 个` : `共 ${N.nodes.length} 个节点`}</span><span>置顶优先 · 按路径排序</span></div>
    <div class="list-scroll" role="listbox" aria-label="节点">${body}</div>`;
}

/* ============================================================
 * 详情
 * ============================================================ */

function detailView() {
  const n = currentNode();
  const vis = visibleNodes();
  if (!n || !vis.includes(n)) {
    return `<div class="detail-inner"><button class="btn btn-ghost btn-sm back" data-act="nodes-back">${ico('i-left')}节点</button></div>
      <div class="detail-empty"><div class="empty"><div class="empty-icon">${ico('i-nodes')}</div><h3>选择一个节点</h3><p>从节点列表选择一个节点，查看它的延迟、可用率和上游线路。</p></div></div>`;
  }
  const st = effStatus(n.name);
  const p = nodeProbe(n.name);
  const status = nodeStatus(n.name);
  const out = bytesParts(N.todayOutbound[n.name] || 0);
  const sessions = N.todaySessions[n.name] || 0;
  const interval = Math.max(1, Math.round((N.probeMeta.intervalMs || 60000) / 60000));

  // 指标卡只显示名称和数值；统计口径放在悬停提示里
  let k1;
  let k1Tip;
  if (status === 'idle') { k1 = '<div class="kpi-val idle">—</div>'; k1Tip = N.probeMeta.enabled ? '尚未检测' : '后台检测已关闭'; }
  else if (status === 'bad') { k1 = `<div class="kpi-val bad">${esc(p.lastError || '不可用')}</div>`; k1Tip = p.lastStatus ? `最新一次检测失败，状态码 ${p.lastStatus}` : '最新一次检测失败，所有线路均不可用'; }
  else { k1 = `<div class="kpi-val">${esc(p.lastMs)}<small>ms</small></div>`; k1Tip = `最新一次检测 · 24 小时均值 ${p.avgMs} ms`; }
  const okCount = p && p.samples ? Math.round(p.availability * p.samples) : 0;

  return `<div class="detail-inner">
    <button class="btn btn-ghost btn-sm back" data-act="nodes-back">${ico('i-left')}节点</button>
    <div class="dhead">
      <div class="dhead-main">
        <div class="dhead-title">${n.fav ? `<svg class="pin" aria-label="已置顶"><use href="#i-pin"/></svg>` : ''}<h2>${esc(nodeTitle(n))}</h2>${statusPill(st)}</div>
        <div class="dhead-meta">
          <span class="addr"><span class="mono">${esc(nodeProxyUrl(n, false))}${n.secret ? '/<span class="secret">••••••</span>' : ''}</span></span>
          ${n.tag ? `<span class="chip">${esc(n.tag)}</span>` : ''}
        </div>
      </div>
      <div class="dhead-actions">
        <button class="btn" data-act="nodes-copy" data-node="${attr(n.name)}" title="复制代理地址${n.secret ? '（含密钥）' : ''}">${ico('i-copy')}<span class="btn-label">复制地址</span></button>
        ${N.checking.has(n.name) ? `<button class="btn" disabled><span class="spin"></span><span class="btn-label">检测中…</span></button>`
          : `<button class="btn" data-act="nodes-check" data-node="${attr(n.name)}" title="经本机代理检测整条链路，并逐条检测上游线路">${ico('i-probe')}<span class="btn-label">检测</span></button>`}
        <button class="btn" data-act="nodes-edit" data-node="${attr(n.name)}">${ico('i-edit')}<span class="btn-label">编辑</span></button>
        <button class="btn btn-icon" data-act="nodes-node-menu" data-node="${attr(n.name)}" aria-label="更多操作" aria-haspopup="menu">${ico('i-more')}</button>
      </div>
    </div>
    <div id="nodeCallout">${calloutView(n)}</div>
    <div class="kpis">
      <div class="kpi" title="${attr(k1Tip)}"><div class="kpi-label">首字节延迟</div>${k1}</div>
      <div class="kpi" title="${attr(!p || !p.samples ? '暂无检测记录' : `近 ${N.probeMeta.retentionHours || 24} 小时 · ${okCount} / ${p.samples} 次检测成功`)}"><div class="kpi-label">可用率</div>
        ${!p || !p.samples ? '<div class="kpi-val idle">—</div>' : `<div class="kpi-val">${fmtPct(p.availability)}<small>%</small></div>`}</div>
      <div class="kpi" title="${attr(!p || !p.lastAt ? '等待首次检测' : `每 ${interval} 分钟自动检测 · 上次 ${fmtTime(p.lastAt)}（北京时间）`)}"><div class="kpi-label">距上次检测</div>
        ${!p || !p.lastAt ? '<div class="kpi-val idle">—</div>' : `<div class="kpi-val" data-ago="${p.lastAt}" data-ago-mode="kpi"></div>`}</div>
      <div class="kpi" title="${attr(`今日 00:00 起（北京时间）· ${sessions} 个会话`)}"><div class="kpi-label">出站流量</div><div class="kpi-val">${esc(out[0])}<small>${esc(out[1])}</small></div></div>
    </div>
    ${chartPanelView(n)}
    ${linesPanelView(n)}
    ${factsView(n)}
  </div>`;
}

/* 异常提示：故障说明影响，降级说明哪条线路失败、由谁承载 */
function calloutView(n) {
  const status = nodeStatus(n.name);
  const p = nodeProbe(n.name);
  if (status === 'bad') {
    const since = lastOkInSeries(n.name);
    const dur = since ? `，最后一次成功在 ${fmtTime(since)}（${formatAgo(since)}）` : N.series && N.series.name === n.name ? `，近 ${SERIES_RANGES[N.series.range].hours} 小时内没有成功记录` : '';
    return `<div class="callout bad" role="alert">${ico('i-bad')}<div class="ct"><b>所有线路均不可用</b><span>最近一次错误：${esc(p.lastError || '未知错误')}${esc(dur)}。客户端此时访问 /${esc(n.name)} 会失败。</span></div>
      <div class="ca"><button class="btn btn-sm" data-act="nodes-check" data-node="${attr(n.name)}">${ico('i-probe')}重新检测</button><button class="btn btn-sm btn-ghost" data-act="nodes-logs" data-node="${attr(n.name)}">查看日志</button></div></div>`;
  }
  if (status === 'warn') {
    const lines = nodeLines(n);
    const failed = lines.filter(l => l.status === 'bad');
    const f = failed[0];
    if (!f) return '';
    const serving = lines.find(l => l.serving) || lines.find(l => l.status === 'ok');
    const head = failed.length > 1 ? `${failed.length} 条线路失败，节点仍可用` : `${roleName(f.index)} ${f.host} ${f.stat.err || '失败'}，节点仍可用`;
    const tail = serving ? `当前由${roleName(serving.index)} ${serving.host} 承载。` : '';
    return `<div class="callout warn">${ico('i-warn')}<div class="ct"><b>${esc(head)}</b><span>最近一次检测在 ${esc(fmtTime(f.stat.at))}。${esc(tail)}失败线路每次请求失败后暂停 1 分钟再重试。</span></div>
      <div class="ca"><button class="btn btn-sm" data-act="nodes-check" data-node="${attr(n.name)}">${ico('i-probe')}重新检测</button><button class="btn btn-sm btn-ghost" data-act="nodes-logs" data-node="${attr(n.name)}">查看日志</button></div></div>`;
  }
  return '';
}

function updateCallout() {
  const box = $('#nodeCallout');
  const n = currentNode();
  if (box && n) box.innerHTML = calloutView(n);
}

function lastOkInSeries(name) {
  if (!N.series || N.series.name !== name) return 0;
  const pts = N.series.points;
  for (let i = pts.length - 1; i >= 0; i--) if (pts[i].n > 0 && pts[i].ok > 0) return pts[i].at;
  return 0;
}

function linesPanelView(n) {
  const lines = nodeLines(n);
  const statusCell = l => l.status === 'bad' ? `<span class="status bad">${ico('i-bad')}${esc(l.stat.err || '失败')}</span>` : statusPill(l.status);
  const avail = l => l.stat && l.stat.samples ? fmtPct(l.stat.availability) + '%' : '—';
  const rows = lines.map(l => `<tr>
      <td class="ord">${l.index + 1}</td>
      <td><div class="host"><b>${esc(l.host)}${l.serving ? '<span class="serve" title="代理当前转发到这条线路">承载中</span>' : ''}</b><span class="mono">${esc(l.url)}</span></div></td>
      <td><span class="role ${l.index === 0 ? 'primary' : 'backup'}">${roleName(l.index)}</span></td>
      <td>${statusCell(l)}</td>
      <td class="r ms">${l.status === 'ok' ? `${esc(l.stat.ms)} ms` : '—'}</td>
      <td class="r">${avail(l)}</td>
      <td class="acts">
        ${l.index > 0 ? `<button class="btn btn-ghost btn-sm" data-act="nodes-promote" data-node="${attr(n.name)}" data-line="${l.index}" title="移到第一位，作为主线">设为主线</button>` : ''}
        <button class="btn btn-ghost btn-sm btn-icon" data-act="nodes-copy-line" data-url="${attr(l.url)}" aria-label="复制线路地址" title="复制线路地址">${ico('i-copy')}</button>
      </td></tr>`).join('');
  const cards = lines.map(l => `<div class="lcard"><div class="lcard-top"><span class="role ${l.index === 0 ? 'primary' : 'backup'}">${roleName(l.index)}</span><b>${esc(l.host)}</b>${statusCell(l)}</div>
      <span class="mono">${esc(l.url)}</span>
      <div class="lcard-bot"><span class="faint" style="font-size:12px">${l.status === 'ok' ? `<span class="mono" style="color:var(--text)">${esc(l.stat.ms)} ms</span> · ` : ''}24 小时 ${avail(l)}${l.serving ? ' · <span style="color:var(--accent-strong)">承载中</span>' : ''}</span>
      <span>${l.index > 0 ? `<button class="btn btn-ghost btn-sm" data-act="nodes-promote" data-node="${attr(n.name)}" data-line="${l.index}">设为主线</button>` : ''}</span></div></div>`).join('');
  return `<section class="panel">
    <div class="panel-head"><span class="panel-title">上游线路</span>
      <button class="btn btn-sm" style="margin-left:auto" data-act="nodes-edit" data-node="${attr(n.name)}" data-tab="lines">${ico('i-edit')}管理线路</button></div>
    ${lines.length ? `<div style="overflow-x:auto"><table class="ltable">
      <thead><tr><th>顺序</th><th>线路</th><th>角色</th><th>状态</th><th class="r">最新延迟</th><th class="r">24 小时可用率</th><th></th></tr></thead>
      <tbody>${rows}</tbody></table></div>
    <div class="lcards">${cards}</div>` : `<div class="empty" style="padding:24px"><p>还没有配置上游线路。</p></div>`}
  </section>`;
}

/* 敏感值默认模糊显示，点一下直接复制，就地显示「已复制」。
 * 值本身不放进 data-* 属性，点击时再按节点名取。 */
function secretChip(n, kind) {
  const isPath = kind === 'path';
  const value = isPath ? `/${n.name}${n.secret ? '/' + n.secret : ''}` : String(n.secret || '');
  const blurred = !isPath || !!n.secret;
  const label = isPath ? '点击复制完整代理地址' : '点击复制访问密钥';
  return `<button type="button" class="reveal ${blurred ? 'is-blurred' : ''}" data-act="nodes-copy-secret" data-node="${attr(n.name)}" data-kind="${kind}" title="${label}" aria-label="${label}${blurred ? '（内容已隐藏）' : ''}">
    <span class="reveal-val mono" aria-hidden="${blurred}">${esc(value)}</span>
    <span class="reveal-hint">${ico('i-copy')}<span>复制</span></span>
  </button>`;
}

function factsView(n) {
  return `<div class="facts" style="grid-template-columns:repeat(2,minmax(0,1fr))">
    <section class="panel fact"><div class="panel-head">${ico(n.secret ? 'i-key' : 'i-globe')}<span class="panel-title">访问</span></div>
      <dl class="kv">
        <dt>访问方式</dt><dd>${n.secret ? '密钥保护' : '公开'}</dd>
        <dt>代理路径</dt><dd>${secretChip(n, 'path')}</dd>
        <dt>访问密钥</dt><dd class="${n.secret ? '' : 'off'}">${n.secret ? secretChip(n, 'secret') : '未设置'}</dd>
      </dl></section>
    <section class="panel fact"><div class="panel-head">${ico('i-id')}<span class="panel-title">客户端身份</span></div>
      <dl class="kv">
        <dt>身份</dt><dd>${n.impersonate !== false ? `伪装为 ${esc(IMPERSONATE_LABELS[normalizeImpersonateProfile(n.impersonateProfile)])}` : '透传真实客户端'}</dd>
        <dt>302 直连</dt><dd class="${n.directExternal ? '' : 'off'}">${n.directExternal ? '开启 · 客户端直连视频流' : '关闭'}</dd>
      </dl></section>
  </div>`;
}

/* ============================================================
 * 延迟曲线：分桶、失败 / 无采样区分、多线路对比、悬浮提示
 * ============================================================ */

function chartPanelView(n) {
  const ranges = [['1h', '1 小时'], ['6h', '6 小时'], ['24h', '24 小时']];
  return `<section class="panel" id="chartPanel" data-node="${attr(n.name)}">
    <div class="chart-head">
      <div class="panel-title">首字节延迟</div>
      <div class="seg" role="group" aria-label="曲线模式">
        <button aria-pressed="${N.chartMode === 'lines'}" data-act="nodes-chart-mode" data-mode="lines" title="每条上游线路一条曲线">按线路</button>
        <button aria-pressed="${N.chartMode === 'node'}" data-act="nodes-chart-mode" data-mode="node" title="节点整体：每次取第一条成功线路">节点整体</button>
      </div>
      <div class="seg" role="group" aria-label="时间范围">${ranges.map(([k, l]) => `<button aria-pressed="${N.range === k}" data-act="nodes-range" data-range="${k}">${l}</button>`).join('')}</div>
    </div>
    <div id="chartBody"></div>
  </section>`;
}

function bucketState(p) {
  if (!p || !p.n) return 'miss';
  if (p.ok <= 0) return 'fail';
  if (p.ok < 1) return 'part';
  return 'ok';
}

function niceMax(v) {
  const steps = [5, 10, 20, 25, 50, 100, 200, 250, 500, 1000, 2000, 2500];
  return (steps.find(s => s * 4 >= v) || Math.ceil(v / 4000) * 1000) * 4;
}

function chartState(body, html) { body.innerHTML = `<div class="chart-state">${html}</div>`; }

function drawChart() {
  const body = $('#chartBody');
  if (!body) return;
  const n = currentNode();
  if (!n) return;
  const p = nodeProbe(n.name);
  const s = N.series && N.series.name === n.name && N.series.range === N.range && N.series.mode === N.chartMode ? N.series : null;
  if (!s && N.seriesState !== 'error') {
    chartState(body, `<div style="width:100%;display:grid;gap:10px"><div class="skel" style="height:150px"></div><div class="skel" style="height:12px;width:50%"></div></div>`);
    // 缓存的是别的节点或别的范围（例如当前节点刚被删除、选中项自动切换），补一次请求
    if (N.seriesState === 'ready' || N.seriesState === 'idle') loadSeries(n.name);
    return;
  }
  if (!s) {
    chartState(body, `<div class="empty"><div class="empty-icon" style="color:var(--bad)">${ico('i-bad')}</div><h3>曲线加载失败</h3><p></p><button class="btn btn-sm" data-act="nodes-chart-retry">${ico('i-probe')}重试</button></div>`);
    body.querySelector('.empty p').textContent = `${N.seriesError}。指标和线路数据不受影响。`;
    return;
  }
  if (!p || !p.samples) {
    chartState(body, `<div class="empty"><div class="empty-icon">${ico('i-stats')}</div><h3>还没有检测记录</h3><p>${N.probeMeta.enabled ? '节点保存后每分钟检测一次，第一个采样点出现后这里开始绘制曲线。' : '后台检测已关闭，可以手动检测一次。'}</p><button class="btn btn-sm" data-act="nodes-check" data-node="${attr(n.name)}">${ico('i-probe')}立即检测</button></div>`);
    return;
  }
  const lines = splitTargets(n.target);
  const series = N.chartMode === 'node' || !s.targets.length
    ? [{ key: 'node', name: '节点整体', sub: '首个可用线路', color: 'var(--accent)', dash: false, data: s.points, last: p.ok ? `${p.lastMs} ms` : (p.lastError || '失败') }]
    : s.targets.map((t, i) => {
      const idx = lines.indexOf(t.target);
      const role = idx >= 0 ? idx : i;
      const stat = (p.targets || []).find(x => x.target === t.target);
      return { key: 't:' + t.target, name: roleName(role), sub: hostOf(t.target), color: SERIES_COLORS[role] || 'var(--text-3)', dash: role > 0, data: t.points || [], last: stat && stat.at ? (stat.ok ? `${stat.ms} ms` : (stat.err || '失败')) : '—' };
    });
  const withData = series.filter(x => x.data.some(b => b.n > 0));
  if (!withData.length || series[0].data.filter(b => b.n > 0).length < 2 && series.every(x => x.data.filter(b => b.n > 0).length < 2)) {
    const have = Math.max(...series.map(x => x.data.filter(b => b.n > 0).length));
    chartState(body, `<div class="empty"><div class="empty-icon">${ico('i-clock')}</div><h3>采样中</h3><p>这个时间范围内已有 ${have} 个采样点，再过几分钟出现曲线。</p></div>`);
    return;
  }
  const shown = series.filter(x => !N.hidden.has(x.key));
  const W = Math.max(280, body.clientWidth - 16);
  const narrow = W < 520;
  const padL = 44;
  const padR = 14;
  const padT = 14;
  const plotH = narrow ? 150 : 190;
  const stripH = 8;
  const stripGap = 4;
  const stripTop = padT + plotH + 26;
  const H = stripTop + Math.max(shown.length, 1) * (stripH + stripGap) + 4;
  const pw = W - padL - padR;
  const nb = series[0].data.length;
  const bw = pw / Math.max(nb, 1);
  const x = i => padL + (i + 0.5) * bw;
  const allMs = shown.flatMap(x2 => x2.data.map(d => d.ms).filter(v => v >= 0));
  const ymax = niceMax((allMs.length ? Math.max(...allMs) : 100) * 1.08);
  const y = v => padT + plotH - (v / ymax) * plotH;
  let g = '';
  for (let k = 0; k <= 4; k++) {
    const v = ymax / 4 * k;
    const yy = y(v);
    g += `<line x1="${padL}" x2="${W - padR}" y1="${yy}" y2="${yy}" stroke="var(--grid)" stroke-width="1"/><text x="${padL - 8}" y="${yy + 4}" text-anchor="end" fill="var(--text-3)" font-size="11" font-family="var(--font-mono)">${v}</text>`;
  }
  // 第一个有样本的桶之前是节点还没开始检测的时间，留空而不是标成「无采样」
  const firstIdx = Math.min(...series.map(x2 => { const k = x2.data.findIndex(b => b.n > 0); return k < 0 ? nb : k; }));
  for (let i = firstIdx; i < nb; i++) {
    const states = shown.map(x2 => bucketState(x2.data[i]));
    if (!shown.length) break;
    if (states.every(st => st === 'miss')) g += `<rect x="${padL + i * bw}" y="${padT}" width="${bw + 0.5}" height="${plotH}" fill="url(#hatch)"/>`;
    else if (states.every(st => st === 'fail' || st === 'miss')) g += `<rect x="${padL + i * bw}" y="${padT}" width="${bw + 0.5}" height="${plotH}" fill="var(--bad-wash)"/>`;
  }
  // 时间刻度落在整点 / 整 10 分钟
  const cfg = SERIES_RANGES[s.range];
  const tickMins = narrow ? cfg.tick * 2 : cfg.tick;
  const tStart = series[0].data[0]?.at || Date.now() - cfg.hours * 3600000;
  const tSpan = cfg.hours * 3600000;
  const xt = t => padL + (t - tStart) / tSpan * pw;
  const firstMin = Math.ceil(tStart / 60000);
  for (let m = firstMin; m * 60000 <= tStart + tSpan; m++) {
    if (((m + 480) % 1440) % tickMins) continue;
    const t = m * 60000;
    const xx = xt(t);
    if (xx < padL + 14 || xx > W - padR - 14) continue;
    g += `<line x1="${xx}" x2="${xx}" y1="${padT + plotH}" y2="${padT + plotH + 4}" stroke="var(--line-strong)"/><text x="${xx}" y="${padT + plotH + 17}" text-anchor="middle" fill="var(--text-3)" font-size="11" font-family="var(--font-mono)">${fmtTime(t)}</text>`;
  }
  g += `<line x1="${padL}" x2="${W - padR}" y1="${padT + plotH}" y2="${padT + plotH}" stroke="var(--line-strong)"/>`;
  shown.forEach((sr, si) => {
    let d = '';
    let pen = false;
    sr.data.forEach((b, i) => { if (!(b.ms >= 0)) { pen = false; return; } d += (pen ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(b.ms).toFixed(1); pen = true; });
    if (shown.length === 1) {
      let a = '';
      let seg = [];
      const flush = () => { if (seg.length > 1) a += `M${seg[0][0]} ${padT + plotH}` + seg.map(pt => `L${pt[0]} ${pt[1]}`).join('') + `L${seg[seg.length - 1][0]} ${padT + plotH}Z`; seg = []; };
      sr.data.forEach((b, i) => { if (!(b.ms >= 0)) flush(); else seg.push([x(i).toFixed(1), y(b.ms).toFixed(1)]); });
      flush();
      g += `<path d="${a}" fill="url(#area)"/>`;
    }
    g += `<path d="${d}" fill="none" stroke="${sr.color}" stroke-width="${si === 0 ? 1.8 : 1.5}" stroke-linejoin="round" ${sr.dash ? 'stroke-dasharray="5 3"' : ''}/>`;
  });
  if (shown.length) {
    let best = null;
    shown.forEach(sr => sr.data.forEach((b, i) => { if (b.ms >= 0 && (!best || b.ms > best.ms)) best = { ms: b.ms, i }; }));
    const vals = shown[0].data.map(b => b.ms).filter(v => v >= 0).sort((a, b) => a - b);
    const med = vals[vals.length >> 1] || 0;
    if (best && med && best.ms > med * 2) {
      const bx = x(best.i);
      const by = y(best.ms);
      const anchor = bx > W - 90 ? 'end' : bx < padL + 60 ? 'start' : 'middle';
      g += `<circle cx="${bx}" cy="${by}" r="3.5" fill="var(--panel)" stroke="var(--warn)" stroke-width="1.8"/><text x="${bx}" y="${Math.max(by - 8, 11)}" text-anchor="${anchor}" fill="var(--text-2)" font-size="11">峰值 ${best.ms} ms</text>`;
    }
  }
  shown.forEach((sr, si) => {
    const yy = stripTop + si * (stripH + stripGap);
    g += `<text x="${padL - 8}" y="${yy + 7.5}" text-anchor="end" fill="var(--text-3)" font-size="10">${esc(sr.name)}</text>`;
    const gap = bw >= 3 ? 1 : 0;
    sr.data.forEach((b, i) => {
      if (i < firstIdx) return;
      const st = bucketState(b);
      const fill = st === 'ok' ? 'var(--ok)' : st === 'part' ? 'var(--warn)' : st === 'fail' ? 'var(--bad)' : 'url(#hatch)';
      g += `<rect x="${(padL + i * bw).toFixed(2)}" y="${yy}" width="${Math.max(bw - gap, 0.6).toFixed(2)}" height="${stripH}" rx="${bw > 4 ? 1.5 : 0}" fill="${fill}" opacity="${st === 'ok' ? 0.55 : 1}"/>`;
    });
  });
  if (!shown.length) g += `<text x="${padL + pw / 2}" y="${padT + plotH / 2}" text-anchor="middle" fill="var(--text-3)" font-size="13">所有线路均已隐藏，点击上方图例重新显示</text>`;
  else if (firstIdx > nb * 0.15 && firstIdx < nb) {
    const fx = padL + firstIdx * bw;
    g += `<line x1="${fx}" x2="${fx}" y1="${padT}" y2="${padT + plotH}" stroke="var(--line-strong)" stroke-dasharray="2 3"/><text x="${fx - 8}" y="${padT + plotH / 2}" text-anchor="end" fill="var(--text-3)" font-size="11">${fmtTime(series[0].data[firstIdx].at)} 开始检测</text>`;
  }

  const legend = series.map(sr => `<button class="lg" aria-pressed="${!N.hidden.has(sr.key)}" data-act="nodes-series" data-key="${attr(sr.key)}" title="${N.hidden.has(sr.key) ? '显示' : '隐藏'} ${attr(sr.name)} · ${attr(sr.sub)}"><i class="${sr.dash ? 'dash' : ''}" style="border-color:${sr.color}"></i><span class="lg-name">${esc(sr.name)} · ${esc(sr.sub)}</span><span class="lg-val">${esc(sr.last)}</span></button>`).join('');
  body.innerHTML = `<div class="legend">${legend}</div>
    <div class="chart-wrap" id="chartWrap" style="${N.seriesState === 'refresh' ? 'opacity:.6' : ''}">
      <svg viewBox="0 0 ${W} ${H}" height="${H}" role="img" aria-label="首字节延迟曲线，近 ${cfg.hours} 小时">
        <defs>
          <pattern id="hatch" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="5" height="5" fill="transparent"/><line x1="0" y1="0" x2="0" y2="5" stroke="var(--line-strong)" stroke-width="2"/></pattern>
          <linearGradient id="area" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="var(--accent)" stop-opacity=".28"/><stop offset="1" stop-color="var(--accent)" stop-opacity="0"/></linearGradient>
        </defs>
        ${g}
        <g id="chartHover"></g>
        <rect id="chartHit" x="${padL}" y="${padT}" width="${pw}" height="${H - padT}" fill="transparent" style="cursor:crosshair"/>
      </svg>
      <div class="ttip" id="chartTip" hidden></div>
    </div>
    <div class="chart-foot">
      <span>检测结果：</span>
      <span class="k"><i style="background:var(--ok);opacity:.55"></i>全部成功</span>
      <span class="k"><i style="background:var(--warn)"></i>部分失败</span>
      <span class="k"><i style="background:var(--bad)"></i>失败（曲线断开）</span>
      <span class="k"><i class="hatch"></i>无采样</span>
    </div>`;
  chartCtx = { series, shown, x, y, bw, padL, padT, plotH, W, nb, firstIdx, step: cfg.hours * 60 / cfg.buckets };
  const hit = $('#chartHit');
  hit.addEventListener('pointermove', onChartHover);
  hit.addEventListener('pointerdown', onChartHover);
  hit.addEventListener('pointerleave', () => { $('#chartHover').innerHTML = ''; $('#chartTip').hidden = true; });
}

function onChartHover(e) {
  const c = chartCtx;
  if (!c) return;
  const svg = e.currentTarget.ownerSVGElement;
  const rect = svg.getBoundingClientRect();
  const px = (e.clientX - rect.left) * (c.W / rect.width);
  const i = Math.max(0, Math.min(c.nb - 1, Math.floor((px - c.padL) / c.bw)));
  const xx = c.x(i);
  let h = `<line x1="${xx}" x2="${xx}" y1="${c.padT}" y2="${c.padT + c.plotH}" stroke="var(--text-3)" stroke-width="1" stroke-dasharray="2 2"/>`;
  c.shown.forEach(sr => { const b = sr.data[i]; if (b && b.ms >= 0) h += `<circle cx="${xx}" cy="${c.y(b.ms)}" r="4" fill="${sr.color}" stroke="var(--panel)" stroke-width="2"/>`; });
  $('#chartHover').innerHTML = h;
  const b0 = c.series[0].data[i];
  const tip = $('#chartTip');
  const rows = c.shown.map(sr => {
    const b = sr.data[i];
    const st = bucketState(b);
    let v;
    if (i < c.firstIdx) v = '<b class="miss">尚未开始检测</b>';
    else if (st === 'miss') v = '<b class="miss">无采样</b>';
    else if (st === 'fail') v = `<b class="bad">${esc(b.err || '失败')}</b>`;
    else v = `<b>${esc(b.ms)} ms</b>`;
    const failed = st === 'part' ? Math.max(1, Math.round(b.n * (1 - b.ok))) : 0;
    const extra = st === 'part' ? `<span style="grid-column:2/-1;color:var(--warn);font-size:11px">${failed} / ${b.n} 次失败${b.err ? ' · ' + esc(b.err) : ''}</span>` : '';
    return `<div class="ttip-row"><i style="border-color:${sr.color};${sr.dash ? 'border-top-style:dashed' : ''}"></i><span>${esc(sr.name)} · ${esc(sr.sub)}</span>${v}${extra}</div>`;
  }).join('');
  const t0 = b0?.at || 0;
  tip.innerHTML = `<div class="ttip-time">${c.step > 1 ? `${fmtTime(t0)} – ${fmtTime(t0 + c.step * 60000)}` : fmtTime(t0)} · ${esc(fmtDateTime(t0).split(' ')[0])}</div>${rows || '<span class="faint">无可见线路</span>'}`;
  tip.hidden = false;
  const wrap = $('#chartWrap').getBoundingClientRect();
  const left = (xx / c.W) * rect.width + (rect.left - wrap.left);
  const tw = tip.offsetWidth;
  tip.style.left = (left + 14 + tw > wrap.width ? Math.max(0, left - tw - 14) : left + 14) + 'px';
  tip.style.top = '30px';
}

/* ============================================================
 * 表格视图
 * ============================================================ */

function tableView() {
  const list = visibleNodes();
  if (!list.length) return listView().replace('class="list-scroll"', 'class="list-scroll" style="padding:24px"');
  const { key, dir } = N.sort;
  const order = { bad: 0, warn: 1, idle: 2, ok: 3 };
  const val = n => {
    const p = nodeProbe(n.name);
    switch (key) {
      case 'status': return order[nodeStatus(n.name)];
      case 'ms': return p && p.probed && p.ok ? p.lastMs : 1e9;
      case 'avail': return p && p.samples ? p.availability : -1;
      case 'out': return N.todayOutbound[n.name] || 0;
      case 'check': return p && p.lastAt ? -p.lastAt : 1e15;
      default: return n.name;
    }
  };
  list.sort((a, b) => { const va = val(a); const vb = val(b); return (va > vb ? 1 : va < vb ? -1 : 0) * dir || a.name.localeCompare(b.name); });
  const th = (k, label, right) => `<th class="${right ? 'r' : ''}"><button data-act="nodes-sort" data-key="${k}" ${key === k ? `aria-sort="${dir > 0 ? 'ascending' : 'descending'}"` : ''}>${label}${ico(key === k ? (dir > 0 ? 'i-sort-asc' : 'i-sort-desc') : 'i-sort')}</button></th>`;
  const sel = selectedVisible();
  const allSel = sel.length === list.length;
  const rows = list.map(n => {
    const st = effStatus(n.name);
    const p = nodeProbe(n.name);
    const lines = nodeLines(n);
    const nOk = lines.filter(l => l.status === 'ok').length;
    const out = N.todayOutbound[n.name] || 0;
    return `<tr aria-selected="${N.rows.has(n.name)}">
      <td class="cb"><input type="checkbox" class="cbx" data-row="${attr(n.name)}" ${N.rows.has(n.name) ? 'checked' : ''} aria-label="选择 ${attr(nodeTitle(n))}"></td>
      <td><div class="nm"><button data-act="nodes-open" data-node="${attr(n.name)}" title="${attr(nodeTitle(n))}">${n.fav ? ico('i-pin', 'pin-s') : ''}${esc(nodeTitle(n))}</button><span class="mono">/${esc(n.name)}</span></div></td>
      <td>${st === 'bad' ? `<span class="status bad">${ico('i-bad')}${esc(p.lastError || '故障')}</span>` : statusPill(st)}</td>
      <td class="r ms">${(st === 'ok' || st === 'warn') ? `${esc(p.lastMs)} ms` : '—'}</td>
      <td class="r">${p && p.samples ? fmtPct(p.availability) + '%' : '—'}</td>
      <td><span class="lines" title="${nOk} / ${lines.length} 条正常">${lines.map(l => `<i class="${l.status === 'ok' ? '' : l.status}"></i>`).join('')}<span>${nOk}/${lines.length}</span></span></td>
      <td>${n.tag ? `<span class="chip">${esc(n.tag)}</span>` : '<span class="faint">—</span>'}</td>
      <td>${n.secret ? `<span class="attr">${ico('i-key')}密钥</span>` : `<span class="attr">${ico('i-globe')}公开</span>`}</td>
      <td class="r">${out ? esc(formatBytes(out)) : '—'}</td>
      <td class="r faint">${p && p.lastAt ? `<span data-ago="${p.lastAt}"></span>` : '—'}</td>
      <td class="r" style="padding-right:24px"><button class="btn btn-ghost btn-sm" data-act="nodes-check" data-node="${attr(n.name)}" ${N.checking.has(n.name) ? 'disabled' : ''}>${N.checking.has(n.name) ? '<span class="spin"></span>' : '检测'}</button><button class="btn btn-ghost btn-sm" data-act="nodes-edit" data-node="${attr(n.name)}">编辑</button></td>
    </tr>`;
  }).join('');
  return `${sel.length ? `<div class="batchbar"><b>已选 ${sel.length} 个节点</b>
      <button class="btn btn-sm" data-act="nodes-batch-check">${ico('i-probe')}检测选中</button>
      <button class="btn btn-sm" data-act="nodes-batch-tag">${ico('i-tag')}设置标签</button>
      <button class="btn btn-sm" data-act="nodes-batch-export">${ico('i-download')}导出</button>
      <button class="btn btn-sm btn-danger" data-act="nodes-batch-delete">${ico('i-trash')}删除</button>
      <button class="btn btn-sm btn-ghost" data-act="nodes-batch-clear" style="margin-left:auto">取消选择</button></div>` : ''}
    <table class="ntable"><thead><tr>
      <th class="cb"><input type="checkbox" class="cbx" data-row="__all" ${allSel ? 'checked' : ''} aria-label="全选"></th>
      ${th('name', '节点')}${th('status', '状态')}${th('ms', '首字节延迟', 1)}${th('avail', '24 小时可用率', 1)}<th>线路</th><th>标签</th><th>访问</th>${th('out', '今日出站', 1)}${th('check', '上次检测', 1)}<th></th>
    </tr></thead><tbody>${rows}</tbody></table>`;
}

/* ============================================================
 * 检测
 * ============================================================ */

function formatCheckResult(result) {
  if (!result) return { ok: false, text: '检测失败' };
  const msValue = Number(result.ms);
  const ms = Number.isFinite(msValue) ? `${Math.round(msValue)} ms` : '';
  if (result.error) {
    const error = String(result.error);
    const lower = error.toLowerCase();
    if (lower.includes('timeout') || lower.includes('deadline')) return { ok: false, text: '超时' };
    return { ok: false, text: error };
  }
  if (Number.isFinite(msValue) && msValue > 9999) return { ok: false, text: '超时' };
  const status = Number(result.status || 0);
  if (status >= 200 && status < 400) return { ok: true, text: ms || '正常' };
  return { ok: false, text: `状态码 ${status || '异常'}${ms ? ' · ' + ms : ''}` };
}

function applyProbeUpdates(probes) {
  for (const p of (Array.isArray(probes) ? probes : [])) if (p && p.name) N.probes[p.name] = p;
}

async function checkNodes(names, { silent = false } = {}) {
  names.forEach(nm => N.checking.add(nm));
  renderNodesPage({ keepChart: true });
  const r = await api('checkStatus', names.length === 1 ? { name: names[0] } : { names });
  names.forEach(nm => N.checking.delete(nm));
  if (!r.ok) {
    if (!r._stale) {
      renderNodesPage({ keepChart: true });
      if (!silent) toast('bad', '检测失败', apiError(r));
    }
    return [];
  }
  applyProbeUpdates(r.probes);
  renderNodesPage({ keepChart: true });
  if (names.includes(N.selected)) loadSeries(N.selected, { quiet: true });
  const results = Array.isArray(r.results) ? r.results : [];
  if (!silent && names.length === 1) {
    const n = N.nodes.find(x => x.name === names[0]);
    const res = formatCheckResult(results[0]);
    const st = nodeStatus(names[0]);
    const lines = n ? nodeLines(n) : [];
    if (!res.ok) toast('bad', `${n ? nodeTitle(n) : names[0]} 检测失败`, `${res.text}：经代理访问 /${names[0]} 没有成功。`);
    else if (st === 'warn') toast('warn', `${nodeTitle(n)} 可用，但有线路失败`, `经代理链路 ${res.text}；${lines.filter(l => l.status === 'bad').length} 条线路失败。`);
    else toast('ok', `${n ? nodeTitle(n) : names[0]} 检测成功`, `经代理链路 ${res.text}${lines.length ? `，${lines.length} 条线路均正常` : ''}。`);
  }
  return results;
}

async function checkAllNodes(names) {
  const list = names || visibleNodes().map(n => n.name);
  if (!list.length) { toast('warn', '没有可检测的节点', '当前筛选结果为空。'); return; }
  if (N.checkAll) return;
  N.checkAll = { done: 0, total: list.length, cancelled: false };
  renderNodesPage({ keepChart: true });
  const queue = [...list];
  let ok = 0;
  let fail = 0;
  const worker = async () => {
    while (queue.length && N.checkAll && !N.checkAll.cancelled) {
      const nm = queue.shift();
      const [res] = await checkNodes([nm], { silent: true });
      if (formatCheckResult(res).ok) ok++; else fail++;
      if (N.checkAll) { N.checkAll.done++; renderHead(); const bar = $('.progress i'); if (bar) bar.style.width = (N.checkAll.done / N.checkAll.total * 100) + '%'; }
    }
  };
  await Promise.all([worker(), worker(), worker(), worker()]);
  if (!N.checkAll) return;
  const { cancelled, done } = N.checkAll;
  N.checkAll = null;
  renderNodesPage({ keepChart: true });
  if (cancelled) toast('warn', '已停止检测', `已完成 ${done} / ${list.length} 个节点。`);
  else toast(fail ? 'warn' : 'ok', '检测完成', `${ok} 个可用，${fail} 个失败。${fail ? '失败的节点已在列表中标出。' : ''}`);
}

/* ============================================================
 * 节点操作
 * ============================================================ */

function nodeByEl(el) { return N.nodes.find(n => n.name === el.dataset.node) || currentNode(); }

async function saveNodeObject(node, oldName) {
  const payload = {
    name: node.name, displayName: node.displayName || '', target: node.target || '', secret: node.secret || '', tag: node.tag || '',
    impersonate: node.impersonate !== false, impersonateProfile: normalizeImpersonateProfile(node.impersonateProfile),
    directExternal: !!node.directExternal, fav: !!node.fav,
  };
  if (oldName) payload.oldName = oldName;
  return api('save', { node: payload });
}

async function promoteLine(n, index) {
  const targets = splitTargets(n.target);
  const url = targets[index];
  const next = [url, ...targets.filter((_, i) => i !== index)];
  const ok = await confirmDialog({
    title: `将 ${hostOf(url)} 设为主线？`,
    text: `线路顺序会调整为：${next.map((u, i) => `${i + 1}. ${hostOf(u)}`).join('，')}。保存后新的请求立即优先走这条线路。`,
    ok: '设为主线',
  });
  if (!ok) return;
  const r = await saveNodeObject({ ...n, target: next.join('\n') }, n.name);
  if (!r.ok) { if (!r._stale) toast('bad', '更换主线失败', apiError(r)); return; }
  toast('ok', '主线已更换', `${hostOf(url)} 现在是主线。`);
  await loadNodes();
}

async function deleteNodes(names) {
  const single = names.length === 1 ? N.nodes.find(n => n.name === names[0]) : null;
  const ok = await confirmDialog({
    title: single ? `删除节点「${nodeTitle(single)}」？` : `删除 ${names.length} 个节点？`,
    text: single
      ? `代理地址 /${single.name} 会立即失效，正在播放的客户端会中断。检测记录一并删除，此操作无法撤销。`
      : `${names.map(x => '/' + x).join('、')} 的代理地址会立即失效，此操作无法撤销。`,
    ok: single ? '删除节点' : `删除 ${names.length} 个节点`,
    danger: true,
  });
  if (!ok) return;
  const r = single ? await api('delete', { name: names[0] }) : await api('batchDelete', { names });
  if (!r.ok) { if (!r._stale) toast('bad', '删除失败', apiError(r)); return; }
  names.forEach(nm => N.rows.delete(nm));
  if (names.includes(N.selected)) { N.selected = ''; N.pane = 'list'; }
  toast('ok', single ? '节点已删除' : `已删除 ${names.length} 个节点`, single ? `/${single.name}` : '');
  await loadNodes();
}

async function exportNodes(names) {
  const r = await api('export', names && names.length ? { names } : {});
  if (!r.ok) { if (!r._stale) toast('bad', '导出失败', apiError(r)); return; }
  const nodes = Array.isArray(r.nodes) ? r.nodes : [];
  if (!nodes.length) { toast('warn', '没有可导出的节点', ''); return; }
  const scope = names && names.length ? 'selected' : 'all';
  const file = `emby-proxy-nodes-${scope}-${formatStamp(new Date())}.json`;
  downloadJson(file, nodes);
  toast('ok', `已导出 ${nodes.length} 个节点`, file);
}

function openBatchTag() {
  const names = selectedVisible();
  if (!names.length) return;
  const tags = [...new Set(N.nodes.map(n => n.tag).filter(Boolean))];
  renderDialog('batchTag', `<div class="dialog compact" role="dialog" aria-modal="true" aria-labelledby="btTitle">
      <div class="dialog-head"><div><h2 id="btTitle">设置标签</h2><p>为 ${names.length} 个节点设置同一个标签，留空则清除标签</p></div><button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="dialog-body"><div class="field"><label class="field-label" for="batchTagInput">标签</label><input class="input" id="batchTagInput" list="batchTagList" placeholder="如：公益服" maxlength="64" autofocus><datalist id="batchTagList">${tags.map(t => `<option value="${attr(t)}">`).join('')}</datalist><div class="field-help">每个节点只能有一个标签，原有标签会被替换。</div></div></div>
      <div class="dialog-foot"><button class="btn lead" data-act="dialog-close">取消</button><span class="note"></span><button class="btn btn-primary" data-act="nodes-batch-tag-save">保存</button></div>
    </div>`);
}

/* ============================================================
 * 编辑弹窗
 * ============================================================ */

let F = null;

function openNodeEditor(name, { tab = 'basic' } = {}) {
  const n = name ? N.nodes.find(x => x.name === name) : null;
  F = {
    isNew: !n, oldName: n?.name || '', tab, saving: false, serverError: '', errors: {}, dirty: false, fav: !!n?.fav,
    v: n ? {
      name: n.name, displayName: n.displayName || '', tag: n.tag || '',
      lines: splitTargets(n.target).map(url => ({ url })),
      access: n.secret ? 'secret' : 'public', secret: n.secret || '',
      impersonate: n.impersonate !== false, profile: normalizeImpersonateProfile(n.impersonateProfile), direct: !!n.directExternal,
    } : { name: '', displayName: '', tag: '', lines: [{ url: '' }], access: 'public', secret: '', impersonate: true, profile: 'yamby', direct: false },
  };
  if (!F.v.lines.length) F.v.lines = [{ url: '' }];
  renderNodeEditor();
}

function validateNodeForm() {
  const e = {};
  const v = F.v;
  const name = v.name.trim();
  if (!name) e.name = '请填写节点路径';
  else if (!/^[a-z0-9_-]{1,32}$/.test(name)) {
    const why = /[A-Z]/.test(name) ? '包含大写字母' : /\s/.test(name) ? '包含空格' : name.length > 32 ? '超过 32 位' : '包含不支持的字符';
    e.name = `只能使用小写字母、数字、_ 和 -，长度 1–32 位。当前${why}。`;
  } else if (N.nodes.some(n => n.name === name && n.name !== F.oldName)) e.name = `路径 /${name} 已被其他节点使用`;
  if (byteLen(v.displayName.trim()) > 32) e.displayName = '展示名称超出 32 字节（约 10 个汉字），请缩短';
  if (byteLen(v.tag.trim()) > 64) e.tag = '标签最长 64 字节';
  const lineErr = {};
  const filled = v.lines.filter(l => l.url.trim());
  if (!filled.length) e.linesAll = '至少需要一条上游线路';
  if (filled.length > 20) e.linesAll = '最多 20 条线路';
  v.lines.forEach((l, i) => {
    const u = l.url.trim();
    if (!u) { if (v.lines.length > 1) lineErr[i] = '地址为空，填写或删除这一行'; return; }
    if (!/^https?:\/\/[^\s/]+/i.test(u)) lineErr[i] = '只支持 http:// 或 https:// 开头的地址';
  });
  if (Object.keys(lineErr).length) e.lines = lineErr;
  if (v.access === 'secret') {
    if (!v.secret) e.secret = '选择了密钥保护，请填写或生成访问密钥';
    else if (!/^[^/?#\s]{1,128}$/.test(v.secret)) e.secret = '密钥不能包含 / ? # 或空白，最长 128 位';
  }
  F.errors = e;
  return e;
}

function tabErrorCounts(e) {
  return {
    basic: ['name', 'displayName', 'tag'].filter(k => e[k]).length,
    lines: (e.linesAll ? 1 : 0) + Object.keys(e.lines || {}).length,
    access: e.secret ? 1 : 0,
  };
}

function fieldError(k) {
  const m = F.errors[k];
  return m ? `<div class="field-error" id="err-${k}">${ico('i-bad')}<span>${esc(m)}</span></div>` : '';
}

function renderNodeEditor() {
  if (!F) return;
  const v = F.v;
  const e = F.errors;
  const te = tabErrorCounts(e);
  const tabs = [['basic', '基本信息'], ['lines', '上游线路'], ['access', '访问与身份']];
  const existing = F.oldName ? nodeProbe(F.oldName) : null;
  const lineStat = url => (existing?.targets || []).find(t => t.target === url.trim());
  let body = '';
  if (F.serverError) body += `<div class="formerr" role="alert">${ico('i-bad')}<div><b>保存失败，已填写的内容都保留着</b><span>${esc(F.serverError)}</span></div></div>`;
  if (F.tab === 'basic') {
    body += `<div class="fgroup">
      <div class="field"><label class="field-label" for="f-name">节点路径 <span class="req">*</span></label>
        <div class="prefix"><span>/</span><input class="input mono" id="f-name" data-f="name" value="${attr(v.name)}" placeholder="hk-01" autocomplete="off" spellcheck="false" maxlength="40" ${e.name ? 'aria-invalid="true" aria-describedby="err-name"' : ''}></div>
        ${fieldError('name') || `<div class="field-help">小写字母、数字、_ 和 -，1–32 位。它决定代理地址${F.isNew ? '' : '，修改后旧地址立即失效'}。</div>`}</div>
      <div class="field"><label class="field-label" for="f-dn">展示名称 <span class="opt">选填</span><span class="counter ${byteLen(v.displayName) > 32 ? 'over' : ''}" id="dnCounter">${byteLen(v.displayName)} / 32 字节</span></label>
        <input class="input" id="f-dn" data-f="displayName" value="${attr(v.displayName)}" placeholder="香港主力站" ${e.displayName ? 'aria-invalid="true"' : ''}>
        ${fieldError('displayName') || '<div class="field-help">列表和详情中显示的名字，可用中文，约 10 个汉字。留空则显示路径。</div>'}</div>
      <div class="field"><label class="field-label" for="f-tag">标签 <span class="opt">选填 · 一个</span></label>
        <input class="input" id="f-tag" data-f="tag" value="${attr(v.tag)}" placeholder="如：公益服" list="tagOptions" ${e.tag ? 'aria-invalid="true"' : ''}><datalist id="tagOptions">${[...new Set(N.nodes.map(n => n.tag).filter(Boolean))].map(t => `<option value="${attr(t)}">`).join('')}</datalist>
        ${fieldError('tag') || '<div class="field-help">用于筛选和批量操作。</div>'}</div>
      <div class="field"><span class="field-label">代理地址预览</span>
        <div class="preview-addr">${ico('i-globe', 'faint')}<span class="mono" id="addrPreview">${esc(location.origin)}/${esc(v.name.trim() || '…')}${v.access === 'secret' ? '/' + (v.secret ? '••••••' : '…') : ''}</span><span class="faint" style="margin-left:auto;font-size:12px">${v.access === 'secret' ? '密钥保护' : '公开'}</span></div></div>
    </div>`;
  }
  if (F.tab === 'lines') {
    body += `<div class="fgroup">
      <div class="fgroup-title"><span>上游线路 · 按顺序故障转移</span><span>${v.lines.length} / 20</span></div>
      ${e.linesAll ? fieldError('linesAll') : ''}
      <div class="uplist">${v.lines.map((l, i) => {
        const le = (e.lines || {})[i];
        const t = lineStat(l.url);
        const res = !t || !t.at ? '<span class="upres idle">未检测</span>' : t.ok ? `<span class="upres ok">${esc(t.ms)} ms</span>` : `<span class="upres bad">${esc(t.err || '失败')}</span>`;
        return `<div class="uprow"><span class="ord">${i + 1}</span><span class="role ${i === 0 ? 'primary' : 'backup'}">${roleName(i)}</span>
          <input class="input mono" id="f-line-${i}" data-line-in="${i}" value="${attr(l.url)}" placeholder="https://emby.example.com" aria-label="第 ${i + 1} 条线路地址" ${le ? 'aria-invalid="true"' : ''} spellcheck="false" autocomplete="off">
          <span class="upact">${res}
            <button class="btn btn-ghost btn-sm btn-icon" data-act="editor-line-up" data-i="${i}" ${i === 0 ? 'disabled' : ''} aria-label="上移" title="上移">${ico('i-up')}</button>
            <button class="btn btn-ghost btn-sm btn-icon" data-act="editor-line-down" data-i="${i}" ${i === v.lines.length - 1 ? 'disabled' : ''} aria-label="下移" title="下移">${ico('i-down')}</button>
            <button class="btn btn-ghost btn-sm btn-icon" data-act="editor-line-remove" data-i="${i}" ${v.lines.length === 1 ? 'disabled' : ''} aria-label="删除这条线路" title="删除">${ico('i-trash')}</button></span>
          ${le ? `<div class="uprow-err field-error">${ico('i-bad')}<span>${esc(le)}</span></div>` : ''}</div>`;
      }).join('')}</div>
      <div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn btn-sm" data-act="editor-line-add" ${v.lines.length >= 20 ? 'disabled' : ''}>${ico('i-plus')}添加线路</button></div>
      <div class="summary-box">第 1 条为主线。主线连接失败或返回 5xx / 403 / 404 时依次尝试下一条，失败线路暂停 1 分钟。只支持 http / https，末尾的 / 会自动去掉。可以一次粘贴多行地址。${F.isNew ? '' : '右侧显示的是该线路最近一次的检测结果。'}</div>
    </div>`;
  }
  if (F.tab === 'access') {
    body += `<div class="fgroup">
      <div class="fgroup-title"><span>访问方式</span></div>
      <div class="choice" role="radiogroup" aria-label="访问方式">
        <label><input type="radio" name="f-access" value="public" data-f="access" ${v.access === 'public' ? 'checked' : ''}><b>公开</b><span>知道路径即可访问：/${esc(v.name.trim() || 'name')}</span></label>
        <label><input type="radio" name="f-access" value="secret" data-f="access" ${v.access === 'secret' ? 'checked' : ''}><b>密钥保护</b><span>路径需带密钥：/${esc(v.name.trim() || 'name')}/密钥</span></label>
      </div>
      ${v.access === 'secret' ? `<div class="field"><label class="field-label" for="f-secret">访问密钥 <span class="req">*</span></label>
        <div class="input-group"><input class="input mono" id="f-secret" data-f="secret" value="${attr(v.secret)}" placeholder="不含 / ? # 和空白，最长 128 位" spellcheck="false" autocomplete="off" ${e.secret ? 'aria-invalid="true"' : ''}><button class="btn" data-act="editor-gen-secret" title="生成 24 位随机密钥">${ico('i-dice')}生成</button></div>
        ${fieldError('secret') || '<div class="field-help">生成的密钥去掉了易混淆字符 0 O 1 l I。</div>'}</div>` : ''}
    </div>
    <div class="fgroup">
      <div class="fgroup-title"><span>客户端身份</span></div>
      <div>
        <div class="toggle-row"><div class="tx"><b>伪装客户端身份</b><span>用所选客户端身份访问上游；关闭则透传真实客户端身份。</span></div><label class="switch"><input type="checkbox" data-f="impersonate" ${v.impersonate ? 'checked' : ''} aria-label="伪装客户端身份"><span></span></label></div>
        <div class="field" style="padding:4px 0 12px"><label class="field-label" for="f-prof">身份模板</label>
          <select class="select" id="f-prof" data-f="profile" ${v.impersonate ? '' : 'disabled'}>${Object.entries(IMPERSONATE_LABELS).map(([k, l]) => `<option value="${k}" ${v.profile === k ? 'selected' : ''}>${l}</option>`).join('')}</select>
          <div class="field-help">${{ yamby: 'Yamby 2.0.4.6 · Android 设备', hills_android: 'Hills 1.7.2 · Android 设备', hills_windows: 'Hills Windows 1.3.1 · Windows 设备' }[v.profile]}</div></div>
      </div>
    </div>
    <details class="adv" ${v.direct ? 'open' : ''}><summary>${ico('i-down')}高级选项</summary>
      <div class="toggle-row"><div class="tx"><b>302 直连外链</b><span>播放时上游返回 302，让客户端直接连接视频流，不经过代理转发。</span></div><label class="switch"><input type="checkbox" data-f="direct" ${v.direct ? 'checked' : ''} aria-label="302 直连外链"><span></span></label></div>
    </details>`;
  }
  const errCount = Object.values(te).reduce((a, b) => a + b, 0);
  renderDialog('nodeEditor', `<div class="dialog" role="dialog" aria-modal="true" aria-labelledby="nodeEditorTitle">
      <div class="dialog-head"><div><h2 id="nodeEditorTitle"></h2><p>${F.isNew ? '填写路径和至少一条上游线路即可保存，其余可稍后设置' : `/${esc(F.oldName)} · 保存后立即生效，无需重启`}</p></div>
        <button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="tabs" role="tablist">${tabs.map(([k, l]) => `<button class="tab" role="tab" aria-selected="${F.tab === k}" data-act="editor-tab" data-t="${k}">${l}${te[k] ? `<span class="terr" aria-label="${te[k]} 处错误">${te[k]}</span>` : ''}</button>`).join('')}</div>
      <div class="dialog-body">${body}</div>
      <div class="dialog-foot">
        <button class="btn lead" data-act="dialog-close" ${F.saving ? 'disabled' : ''}>取消</button>
        <span class="note" id="editorNote">${editorNote(errCount)}</span>
        <button class="btn btn-primary" data-act="editor-save" ${F.saving ? 'disabled' : ''}>${F.saving ? '<span class="spin"></span>保存中…' : F.isNew ? '保存节点' : '保存修改'}</button>
      </div>
    </div>`, { scrollKey: 'editor-' + F.tab, onClose: closeNodeEditor, onDispose: () => { F = null; } });
  $('#nodeEditorTitle').textContent = F.isNew ? '新增 Emby 节点' : `编辑「${F.v.displayName.trim() || F.oldName}」`;
}

function editorNote(errCount) {
  if (errCount) return `<span style="color:var(--bad)">还有 ${errCount} 处需要修改</span>`;
  if (F.dirty) return '<span style="color:var(--warn)">有未保存的修改</span>';
  return F.isNew ? '带 * 为必填' : '';
}

async function closeNodeEditor() {
  if (!F || F.saving) return;
  if (F.dirty) {
    const ok = await confirmDialog({ title: '放弃未保存的修改？', text: '关闭后本次填写的内容不会保存。', ok: '放弃修改', danger: true });
    if (!ok) return;
  }
  F = null;
  closeDialog();
}

/* 输入时只更新必要的局部，不重绘整个弹窗，避免打断输入 */
function onEditorField(el) {
  const k = el.dataset.f;
  const v = F.v;
  F.dirty = true;
  if (el.type === 'checkbox') v[k] = el.checked;
  else if (el.type === 'radio') v[k] = el.value;
  else v[k] = el.value;
  const hadErrors = Object.keys(F.errors).length > 0;
  if (hadErrors) validateNodeForm();
  if (el.type === 'checkbox' || el.type === 'radio' || el.tagName === 'SELECT' || hadErrors) { renderNodeEditor(); return; }
  if (k === 'displayName') {
    const c = $('#dnCounter');
    if (c) { c.textContent = `${byteLen(v.displayName)} / 32 字节`; c.classList.toggle('over', byteLen(v.displayName) > 32); }
  }
  const pv = $('#addrPreview');
  if (pv) pv.textContent = `${location.origin}/${v.name.trim() || '…'}${v.access === 'secret' ? '/' + (v.secret ? '••••••' : '…') : ''}`;
  const note = $('#editorNote');
  if (note) note.innerHTML = editorNote(0);
}

function onEditorLine(el) {
  const i = Number(el.dataset.lineIn);
  const v = F.v;
  F.dirty = true;
  // 粘贴多行地址时拆成多条线路
  const parts = el.value.split(/[\n;,，；|]+/).map(s => s.trim()).filter(Boolean);
  if (parts.length > 1) {
    v.lines.splice(i, 1, ...parts.slice(0, 20 - (v.lines.length - 1)).map(url => ({ url })));
    if (Object.keys(F.errors).length) validateNodeForm();
    renderNodeEditor();
    return;
  }
  v.lines[i].url = el.value;
  if (Object.keys(F.errors).length) { validateNodeForm(); renderNodeEditor(); return; }
  const note = $('#editorNote');
  if (note) note.innerHTML = editorNote(0);
}

/* 密钥会出现在代理 URL 的路径里（/节点名/密钥/），后端限制是不含 / ? # 与空白、最长 128。
 * 这里只用字母数字，并去掉易混淆的 0 O 1 l I；字符集 57 个，用拒绝采样避开取模偏置。 */
function generateNodeSecret() {
  if (!window.crypto || typeof crypto.getRandomValues !== 'function') {
    toast('bad', '无法生成密钥', '当前环境不支持安全随机数，请手动填写。');
    return '';
  }
  const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
  const length = 24;
  const limit = 256 - (256 % alphabet.length); // 大于等于此值的字节直接丢弃，保证每个字符等概率
  const chars = [];
  const buf = new Uint8Array(length);
  while (chars.length < length) {
    crypto.getRandomValues(buf);
    for (const byte of buf) {
      if (byte >= limit) continue;
      chars.push(alphabet[byte % alphabet.length]);
      if (chars.length === length) break;
    }
  }
  return chars.join('');
}

/* 服务端校验错误归到对应标签页 */
function serverErrorTab(msg) {
  const m = String(msg || '').toLowerCase();
  if (m.includes('target') || m.includes('上游')) return 'lines';
  if (m.includes('secret') || m.includes('密钥')) return 'access';
  return 'basic';
}

async function saveNodeEditor() {
  if (!F || F.saving) return;
  const e = validateNodeForm();
  if (Object.keys(e).length) {
    const te = tabErrorCounts(e);
    if (!te[F.tab]) F.tab = Object.keys(te).find(k => te[k]) || F.tab;
    F.serverError = '';
    renderNodeEditor();
    toast('bad', '还有字段需要修改', '已切换到第一个有问题的分组，错误项已标红。');
    return;
  }
  const v = F.v;
  const node = {
    name: v.name.trim(), displayName: v.displayName.trim(), tag: v.tag.trim(),
    target: v.lines.map(l => l.url.trim().replace(/\/+$/, '')).filter(Boolean).join('\n'),
    secret: v.access === 'secret' ? v.secret : '',
    impersonate: v.impersonate, impersonateProfile: v.profile, directExternal: v.direct, fav: F.fav,
  };
  F.saving = true;
  F.serverError = '';
  renderNodeEditor();
  const form = F;
  const r = await saveNodeObject(node, F.isNew ? '' : F.oldName);
  if (F !== form) return;
  F.saving = false;
  if (!r.ok) {
    if (r._stale) return;
    F.serverError = apiError(r, '保存失败');
    F.tab = serverErrorTab(r.error);
    renderNodeEditor();
    toast('bad', '保存失败', '内容没有丢失，修改后可以直接重试。');
    return;
  }
  const wasNew = F.isNew;
  F = null;
  closeDialog();
  N.selected = node.name;
  N.pane = 'detail';
  N.view = N.view === 'table' && wasNew ? 'split' : N.view;
  toast('ok', wasNew ? '节点已添加' : '修改已保存', wasNew ? `代理地址 ${location.origin}/${node.name} 已可用，首次检测会在 1 分钟内完成。` : '改动已立即生效。');
  await loadNodes();
}

/* ============================================================
 * 导入
 * ============================================================ */

let IMP = null;

function openImport() {
  IMP = { text: '', results: null, busy: false, error: '' };
  renderImport();
}

function renderImport() {
  if (!IMP) return;
  const res = IMP.results;
  const okN = res ? res.filter(x => x.ok).length : 0;
  const failed = res ? res.filter(x => !x.ok) : [];
  renderDialog('import', `<div class="dialog compact" role="dialog" aria-modal="true" aria-labelledby="impTitle">
      <div class="dialog-head"><div><h2 id="impTitle">导入节点</h2><p>粘贴或选择导出的 JSON 文件；同名节点会被覆盖</p></div><button class="btn btn-ghost btn-icon" data-act="dialog-close" aria-label="关闭">${ico('i-x')}</button></div>
      <div class="dialog-body">
        ${res ? `<div class="formerr" style="${failed.length ? '' : 'background:var(--ok-wash);border-color:rgba(62,207,142,.35)'}">${ico(failed.length ? 'i-warn' : 'i-ok')}<div><b>导入完成：${okN} 个成功${failed.length ? `，${failed.length} 个失败` : ''}</b><span>${failed.length ? '失败的节点没有写入，修正后可以单独再导入。' : '节点已写入并开始检测。'}</span></div></div>
          ${failed.length ? `<div class="import-results">${failed.map(f => `<div><span class="bad mono">${esc(f.name || '（无名称）')}</span><span>${esc(f.error || '未知错误')}</span></div>`).join('')}</div>` : ''}` : ''}
        ${IMP.error ? `<div class="field-error">${ico('i-bad')}<span>${esc(IMP.error)}</span></div>` : ''}
        <label class="dropzone" id="importDrop">${ico('i-file')}<span>拖入 JSON 文件，或点击选择</span><span class="faint" style="font-size:12px">最大 4 MB</span><input type="file" id="importFile" accept=".json,application/json" hidden></label>
        <div class="field"><label class="field-label" for="importJson">JSON 内容</label><textarea class="input mono" id="importJson" rows="8" spellcheck="false" placeholder='[{"name":"my-emby","target":"https://emby.example.com","secret":"","tag":""}]'>${esc(IMP.text)}</textarea></div>
      </div>
      <div class="dialog-foot"><button class="btn lead" data-act="dialog-close">${res ? '关闭' : '取消'}</button><span class="note"></span><button class="btn btn-primary" data-act="nodes-import-run" ${IMP.busy ? 'disabled' : ''}>${IMP.busy ? '<span class="spin"></span>导入中…' : '导入'}</button></div>
    </div>`, { onDispose: () => { IMP = null; } });
  const drop = $('#importDrop');
  const file = $('#importFile');
  file.addEventListener('change', () => readImportFile(file.files[0]));
  drop.addEventListener('dragover', ev => { ev.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('over'));
  drop.addEventListener('drop', ev => { ev.preventDefault(); drop.classList.remove('over'); readImportFile(ev.dataTransfer.files[0]); });
}

async function readImportFile(file) {
  if (!file || !IMP) return;
  if (file.size > 4 * 1024 * 1024) { IMP.error = '文件超过 4 MB，导入接口最多接受 4 MB'; renderImport(); return; }
  try {
    IMP.text = (await file.text()).trim();
    IMP.error = '';
    renderImport();
    toast('ok', '已读取文件', file.name);
  } catch (err) {
    IMP.error = '读取文件失败：' + (err?.message || '未知错误');
    renderImport();
  }
}

async function runImport() {
  if (!IMP || IMP.busy) return;
  IMP.text = $('#importJson').value.trim();
  let nodes;
  try { nodes = JSON.parse(IMP.text); } catch (_) { IMP.error = 'JSON 格式错误，请检查括号和引号'; renderImport(); return; }
  if (!Array.isArray(nodes)) nodes = [nodes];
  IMP.busy = true;
  IMP.error = '';
  renderImport();
  const form = IMP;
  const r = await api('import', { nodes });
  if (r.ok) loadNodes(); // 弹窗中途被关掉也要刷新列表，否则导入的节点要等切页后才出现
  if (IMP !== form) {
    if (r.ok) {
      const results = Array.isArray(r.results) ? r.results : [];
      const failed = results.filter(x => !x.ok).length;
      toast(failed ? 'warn' : 'ok', `导入完成：${results.length - failed} 个成功${failed ? `，${failed} 个失败` : ''}`, '');
    }
    return;
  }
  IMP.busy = false;
  if (!r.ok) { if (r._stale) return; IMP.error = '导入失败：' + apiError(r); renderImport(); return; }
  IMP.results = Array.isArray(r.results) ? r.results : [];
  renderImport();
}

/* ============================================================
 * 事件
 * ============================================================ */

registerActions({
  'nodes-new': () => openNodeEditor(null),
  'nodes-edit': el => openNodeEditor(el.dataset.node || N.selected, { tab: el.dataset.tab || 'basic' }),
  'nodes-reload': () => loadNodes(),
  'nodes-select': el => {
    if (N.selected !== el.dataset.node) { N.hidden.clear(); N.series = null; N.seriesState = 'loading'; }
    N.selected = el.dataset.node;
    N.pane = 'detail';
    renderNodesPage();
    loadSeries(N.selected);
  },
  'nodes-open': el => { N.selected = el.dataset.node; N.view = 'split'; N.pane = 'detail'; N.series = null; N.hidden.clear(); saveView(); renderNodesPage(); loadSeries(N.selected); },
  'nodes-back': () => { N.pane = 'list'; renderNodesPage({ keepChart: true }); },
  'nodes-status': el => { N.status = el.dataset.status; renderNodesPage({ keepChart: true }); },
  'nodes-view': el => { N.view = el.dataset.view; saveView(); renderNodesPage(); },
  'nodes-clear-filter': () => { N.q = ''; N.status = 'all'; N.tag = 'all'; renderNodesPage(); },
  'nodes-check': el => checkNodes([el.dataset.node]),
  'nodes-check-all': () => checkAllNodes(),
  'nodes-cancel-all': () => { if (N.checkAll) N.checkAll.cancelled = true; },
  'nodes-copy': el => { const n = nodeByEl(el); copyText(nodeProxyUrl(n), '已复制代理地址', n.secret ? '地址包含访问密钥，请只分享给可信的人。' : nodeProxyUrl(n)); },
  'nodes-copy-line': el => copyText(el.dataset.url, '已复制线路地址', el.dataset.url),
  'nodes-copy-secret': el => {
    const n = N.nodes.find(x => x.name === el.dataset.node);
    if (!n) return;
    const isPath = el.dataset.kind === 'path';
    const text = isPath ? nodeProxyUrl(n) : String(n.secret || '');
    copyText(text, isPath ? '已复制代理地址' : '已复制访问密钥', '', {
      quiet: true,
      onCopied: () => {
        const hint = el.querySelector('.reveal-hint span');
        const icon = el.querySelector('.reveal-hint use');
        el.classList.add('is-copied');
        if (hint) hint.textContent = '已复制';
        icon?.setAttribute('href', '#i-ok');
        clearTimeout(el._copiedTimer);
        el._copiedTimer = setTimeout(() => {
          el.classList.remove('is-copied');
          if (hint) hint.textContent = '复制';
          icon?.setAttribute('href', '#i-copy');
        }, 1600);
      },
    });
  },
  'nodes-promote': el => promoteLine(nodeByEl(el), Number(el.dataset.line)),
  'nodes-logs': el => {
    if (typeof openLogsForNode === 'function') openLogsForNode(el.dataset.node || N.selected);
    else switchTab('logs');
  },
  'nodes-node-menu': el => {
    const n = nodeByEl(el);
    openMenu(el, [
      { act: 'nodes-fav', icon: n.fav ? 'i-pin' : 'i-pin-o', label: n.fav ? '取消置顶' : '置顶', data: { node: n.name } },
      { act: 'nodes-logs', icon: 'i-logs', label: '查看该节点日志', data: { node: n.name } },
      { act: 'nodes-export-one', icon: 'i-download', label: '导出此节点', data: { node: n.name } },
      '-',
      { act: 'nodes-delete', icon: 'i-trash', label: '删除节点…', danger: true, data: { node: n.name } },
    ]);
  },
  'nodes-global-menu': el => openMenu(el, [
    { act: 'nodes-import', icon: 'i-upload', label: '导入节点…', hint: 'JSON' },
    { act: 'nodes-export-all', icon: 'i-download', label: '导出全部节点' },
  ]),
  'nodes-fav': async el => {
    const r = await api('toggleFav', { name: el.dataset.node });
    if (!r.ok) { if (!r._stale) toast('bad', '操作失败', apiError(r)); return; }
    toast('ok', r.fav ? '已置顶' : '已取消置顶', '');
    loadNodes();
  },
  'nodes-delete': el => deleteNodes([el.dataset.node]),
  'nodes-export-one': el => exportNodes([el.dataset.node]),
  'nodes-export-all': () => exportNodes(null),
  'nodes-import': () => openImport(),
  'nodes-import-run': () => runImport(),
  'nodes-range': el => { N.range = el.dataset.range; try { localStorage.setItem('ep_series_range', N.range); } catch (_) { /* 忽略 */ } refreshChartPanel(); },
  'nodes-chart-mode': el => { N.chartMode = el.dataset.mode; N.hidden.clear(); refreshChartPanel(); },
  'nodes-chart-retry': () => loadSeries(N.selected),
  'nodes-series': el => { const k = el.dataset.key; if (N.hidden.has(k)) N.hidden.delete(k); else N.hidden.add(k); drawChart(); },
  'nodes-sort': el => {
    const k = el.dataset.key;
    N.sort = { key: k, dir: N.sort.key === k ? -N.sort.dir : (k === 'name' || k === 'status' || k === 'check' ? 1 : -1) };
    renderNodesPage();
  },
  'nodes-batch-check': () => { const names = selectedVisible(); if (names.length) checkAllNodes(names); },
  'nodes-batch-clear': () => { N.rows.clear(); renderNodesPage(); },
  'nodes-batch-export': () => { const names = selectedVisible(); if (names.length) exportNodes(names); },
  'nodes-batch-delete': () => { const names = selectedVisible(); if (names.length) deleteNodes(names); },
  'nodes-batch-tag': () => openBatchTag(),
  'nodes-batch-tag-save': async el => {
    if (el.disabled) return;
    const tag = $('#batchTagInput').value.trim();
    const names = selectedVisible();
    if (!names.length) return;
    el.disabled = true;
    el.innerHTML = '<span class="spin"></span>保存中…';
    const r = await api('batchTag', { names, tag });
    const stillOpen = dialogOpen('batchTag');
    if (!r.ok) {
      if (stillOpen) { el.disabled = false; el.textContent = '保存'; }
      if (!r._stale) toast('bad', '设置标签失败', apiError(r));
      return;
    }
    if (stillOpen) closeDialog();
    toast('ok', tag ? `已为 ${names.length} 个节点设置标签` : `已清除 ${names.length} 个节点的标签`, tag);
    loadNodes();
  },
  'editor-tab': el => { F.tab = el.dataset.t; renderNodeEditor(); },
  'editor-save': () => saveNodeEditor(),
  'editor-line-add': () => { F.v.lines.push({ url: '' }); F.dirty = true; renderNodeEditor(); $(`#f-line-${F.v.lines.length - 1}`)?.focus(); },
  'editor-line-up': el => { const i = Number(el.dataset.i); const L = F.v.lines; [L[i - 1], L[i]] = [L[i], L[i - 1]]; F.dirty = true; if (Object.keys(F.errors).length) validateNodeForm(); renderNodeEditor(); },
  'editor-line-down': el => { const i = Number(el.dataset.i); const L = F.v.lines; [L[i + 1], L[i]] = [L[i], L[i + 1]]; F.dirty = true; if (Object.keys(F.errors).length) validateNodeForm(); renderNodeEditor(); },
  'editor-line-remove': el => { F.v.lines.splice(Number(el.dataset.i), 1); F.dirty = true; if (Object.keys(F.errors).length) validateNodeForm(); renderNodeEditor(); },
  'editor-gen-secret': () => {
    const secret = generateNodeSecret();
    if (!secret) return;
    F.v.secret = secret;
    F.dirty = true;
    if (Object.keys(F.errors).length) validateNodeForm();
    renderNodeEditor();
  },
});

function saveView() { try { localStorage.setItem('ep_nodes_view', N.view); } catch (_) { /* 忽略 */ } }

function refreshChartPanel() {
  const panel = $('#chartPanel');
  const n = currentNode();
  if (!panel || !n) return;
  panel.outerHTML = chartPanelView(n);
  drawChart();
  loadSeries(n.name);
}

function onNodesTextInput(t) {
  if (t.id === 'nodeSearch') { N.q = t.value; renderRegion(false); return; }
  if (F && t.closest('#overlay .dialog')) {
    if (t.dataset.lineIn !== undefined) onEditorLine(t);
    else if (t.dataset.f && t.type !== 'checkbox' && t.type !== 'radio' && t.tagName !== 'SELECT') onEditorField(t);
  }
}

/* 输入法组字期间不处理（重绘会打断中文输入），组字结束后补一次 */
document.addEventListener('input', e => { if (!e.isComposing) onNodesTextInput(e.target); });
document.addEventListener('compositionend', e => onNodesTextInput(e.target));

/* <input> 会吞掉粘贴内容里的换行，所以在 paste 事件里读原文，多行地址直接拆成多条线路 */
document.addEventListener('paste', e => {
  const t = e.target;
  if (!F || t.dataset?.lineIn === undefined) return;
  const text = e.clipboardData?.getData('text') || '';
  const parts = text.split(/[\r\n;,，；|]+/).map(x => x.trim()).filter(Boolean);
  if (parts.length < 2) return;
  e.preventDefault();
  const i = Number(t.dataset.lineIn);
  const before = t.value.slice(0, t.selectionStart ?? t.value.length).trim();
  const after = t.value.slice(t.selectionEnd ?? t.value.length).trim();
  parts[0] = before + parts[0];
  parts[parts.length - 1] += after;
  // 当前这一行会被替换，其余已有线路保留；总数最多 20 条，多出来的粘贴内容丢弃
  const room = 20 - (F.v.lines.length - 1);
  const kept = parts.slice(0, room);
  F.v.lines.splice(i, 1, ...kept.map(url => ({ url })));
  F.dirty = true;
  if (Object.keys(F.errors).length) validateNodeForm();
  renderNodeEditor();
  if (kept.length < parts.length) toast('warn', `已拆分为 ${kept.length} 条线路`, `最多 20 条线路，已忽略 ${parts.length - kept.length} 条。`);
  else toast('ok', `已拆分为 ${kept.length} 条线路`, '');
});

document.addEventListener('change', e => {
  const t = e.target;
  if (t.id === 'nodeTagFilter') { N.tag = t.value; renderNodesPage(); return; }
  if (t.dataset.row !== undefined) {
    const list = visibleNodes();
    if (t.dataset.row === '__all') list.forEach(n => { if (t.checked) N.rows.add(n.name); else N.rows.delete(n.name); });
    else if (t.checked) N.rows.add(t.dataset.row);
    else N.rows.delete(t.dataset.row);
    renderNodesPage();
    return;
  }
  if (F && t.closest('#overlay .dialog') && t.dataset.f && (t.type === 'checkbox' || t.type === 'radio' || t.tagName === 'SELECT')) onEditorField(t);
});

registerPage('nodes', {
  enter() {
    renderNodesPage();
    loadNodes();
    startNodeTimers();
  },
  leave() {
    stopNodeTimers();
    if (N.checkAll) N.checkAll.cancelled = true;
  },
  reset() {
    stopNodeTimers();
    F = null;
    IMP = null;
    N.loaded = false;
    N.nodes = [];
    N.probes = {};
    N.rows.clear();
    N.checking.clear();
    N.checkAll = null;
    N.series = null;
    N.selected = '';
    const page = document.getElementById('pageNodes');
    if (page) { page.innerHTML = ''; delete page.dataset.ready; }
  },
  resize(bpChanged) {
    if (bpChanged) renderNodesPage();
    else drawChart();
  },
  keydown(e) {
    if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && e.target.classList?.contains('nrow')) {
      e.preventDefault();
      const list = visibleNodes();
      const i = list.findIndex(n => n.name === N.selected);
      const nx = list[Math.max(0, Math.min(list.length - 1, i + (e.key === 'ArrowDown' ? 1 : -1)))];
      if (!nx || nx.name === N.selected) return;
      N.selected = nx.name;
      N.hidden.clear();
      N.series = null;
      renderNodesPage();
      loadSeries(nx.name);
      document.querySelector(`.nrow[data-node="${CSS.escape(nx.name)}"]`)?.focus();
    }
  },
});
