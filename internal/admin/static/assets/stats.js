/* ============================================================
 * stats.js — 统计页：播放次数、会话、流量与图片缓存
 * 数据来自 stats.get（按天 × 节点 × 客户端聚合的行），
 * 节点展示名来自 list，图片缓存来自 imageCache.stats。
 * ============================================================ */

let statsRows = [];
let statsSortKey = 'lastActivityAt';
let statsSortDirection = 'desc';
let currentStatsRange = 7;
let statsRowsRange = null;
/* 明细表分页：数据一次取回，只分批渲染，避免 30 天上千行一次塞进 DOM */
const STATS_PAGE_SIZES = [25, 50, 100];
let statsPageSize = 50;
let statsPage = 1; // statsRows 实际对应的天数；切换范围失败时据此回退
let statsNodeNames = {};      // name → displayName
let statsNodeCount = 0;
let statsImageCache = null;
let statsState = 'idle';      // idle | loading | ready | error
let statsLoadToken = 0;
let statsRendered = false;
let statsLoadError = '';

const STATS_SORT_DEFAULT_DIRECTIONS = {
  lastActivityAt: 'desc',
  node: 'asc',
  client: 'asc',
  plays: 'desc',
  sessions: 'desc',
  playbackMillis: 'desc',
  inboundBytes: 'desc',
  outboundBytes: 'desc',
};
const STATS_SORT_FALLBACKS = [
  ['day', 'desc'],
  ['lastActivityAt', 'desc'],
  ['node', 'asc'],
  ['client', 'asc'],
];
const statsTextCollator = new Intl.Collator('zh-CN', { numeric: true, sensitivity: 'base' });
const STATS_WEEKDAYS = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
const STATS_BJ_DAY = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' });

/* 明细表头写成字面量：排序状态由 updateStatsSortHeaders 在渲染后同步 */
const STATS_TABLE_HEAD = `<tr>
  <th data-stats-sort="lastActivityAt" aria-sort="descending"><button type="button" data-act="stats-sort" data-key="lastActivityAt">最近活动<svg aria-hidden="true"><use href="#i-sort-desc"/></svg></button></th>
  <th data-stats-sort="node" aria-sort="none"><button type="button" data-act="stats-sort" data-key="node">Emby 节点<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th data-stats-sort="client" aria-sort="none"><button type="button" data-act="stats-sort" data-key="client">客户端<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th class="r" data-stats-sort="plays" aria-sort="none"><button type="button" data-act="stats-sort" data-key="plays">播放次数<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th class="r" data-stats-sort="sessions" aria-sort="none"><button type="button" data-act="stats-sort" data-key="sessions">会话数<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th class="r" data-stats-sort="playbackMillis" aria-sort="none"><button type="button" data-act="stats-sort" data-key="playbackMillis">播放时长<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th class="r" data-stats-sort="inboundBytes" aria-sort="none"><button type="button" data-act="stats-sort" data-key="inboundBytes">入站流量<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
  <th class="r" data-stats-sort="outboundBytes" aria-sort="none"><button type="button" data-act="stats-sort" data-key="outboundBytes">出站流量<svg aria-hidden="true"><use href="#i-sort"/></svg></button></th>
</tr>`;

function statsRangeDays() {
  const value = Number(currentStatsRange);
  return [1, 7, 30].includes(value) ? value : 7;
}

function statsRangeLabel(days) {
  if (days === 1) return '今天';
  if (days === 30) return '近 30 天';
  return '近 7 天';
}

function trafficInboundBytes(stat) {
  return Number(stat?.inboundBytes || 0);
}

function trafficOutboundBytes(stat) {
  return Number(stat?.outboundBytes || 0);
}

function statsPlaybackMillis(stat) {
  const value = Number(stat?.playbackMillis || 0);
  return Number.isFinite(value) ? Math.max(0, value) : 0;
}

function statsNodeLabel(name) {
  const key = String(name || '').trim();
  if (!key) return '未知节点';
  return statsNodeNames[key] || key;
}

/* 播放时长在指标卡里只给一个数值：不足 1 小时按分钟，否则按小时保留一位 */
function statsDurationParts(milliseconds) {
  const minutes = Math.max(0, Number(milliseconds || 0)) / 60000;
  if (minutes < 60) return [String(Math.round(minutes)), '分钟'];
  const hours = minutes / 60;
  return [hours >= 100 ? hours.toFixed(0) : hours.toFixed(1), '小时'];
}

/* 北京时间下最近 days 天的日期串（YYYY-MM-DD），最后一个是今天 */
function statsDayList(days) {
  const out = [];
  const now = Date.now();
  for (let i = days - 1; i >= 0; i--) out.push(STATS_BJ_DAY.format(new Date(now - i * 86400000)));
  return out;
}

function statsDayParts(day) {
  const [y, m, d] = String(day).split('-').map(Number);
  return { y, m, d, weekday: STATS_WEEKDAYS[new Date(Date.UTC(y, m - 1, d)).getUTCDay()] };
}

function formatStatsActivity(stat) {
  const formatted = String(stat?.lastActivity || '').trim();
  if (formatted) return formatted;
  const timestamp = Number(stat?.lastActivityAt || 0);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '--';
  return new Date(timestamp).toLocaleString('zh-CN', { hour12: false, timeZone: 'Asia/Shanghai' });
}

/* ============================================================
 * 汇总
 * ============================================================ */

function summarizeStats(rows, days) {
  const total = { plays: 0, sessions: 0, playback: 0, inbound: 0, outbound: 0 };
  const nodes = new Set();
  const clients = new Set();
  const byDay = new Map(statsDayList(days).map(day => [day, { day, plays: 0, sessions: 0, playback: 0, inbound: 0, outbound: 0 }]));
  const byClient = new Map();
  const byNode = new Map();
  for (const row of rows) {
    const plays = Number(row?.plays || 0);
    const sessions = Number(row?.sessions || 0);
    const playback = statsPlaybackMillis(row);
    const inbound = trafficInboundBytes(row);
    const outbound = trafficOutboundBytes(row);
    total.plays += plays;
    total.sessions += sessions;
    total.playback += playback;
    total.inbound += inbound;
    total.outbound += outbound;
    if (row?.node) nodes.add(row.node);
    if (row?.client) clients.add(row.client);

    const bucket = byDay.get(String(row?.day || '').trim());
    if (bucket) {
      bucket.plays += plays;
      bucket.sessions += sessions;
      bucket.playback += playback;
      bucket.inbound += inbound;
      bucket.outbound += outbound;
    }

    const client = String(row?.client || '').trim() || '未知客户端';
    byClient.set(client, (byClient.get(client) || 0) + plays);

    const node = String(row?.node || '').trim();
    const nb = byNode.get(node) || { node, plays: 0, outbound: 0 };
    nb.plays += plays;
    nb.outbound += outbound;
    byNode.set(node, nb);
  }
  return {
    total,
    nodes: nodes.size,
    clients: clients.size,
    days: [...byDay.values()],
    clientList: [...byClient.entries()].map(([client, plays]) => ({ client, plays }))
      .sort((a, b) => (b.plays - a.plays) || statsTextCollator.compare(a.client, b.client)),
    nodeList: [...byNode.values()]
      .sort((a, b) => (b.outbound - a.outbound) || (b.plays - a.plays) || statsTextCollator.compare(statsNodeLabel(a.node), statsNodeLabel(b.node))),
  };
}

/* ============================================================
 * 渲染
 * ============================================================ */

function renderStatsShell() {
  const page = document.getElementById('pageStats');
  page.innerHTML = `
    <header class="page-head">
      <div class="page-title"><h1>统计</h1><p id="stSummary">播放次数、会话与流量 · 北京时间</p></div>
      <div class="gactions">
        <div class="seg" role="group" aria-label="时间范围" id="stRange">
          <button type="button" data-act="stats-range" data-days="1" aria-pressed="false">今天</button>
          <button type="button" data-act="stats-range" data-days="7" aria-pressed="true">近 7 天</button>
          <button type="button" data-act="stats-range" data-days="30" aria-pressed="false">近 30 天</button>
        </div>
        <button class="btn" type="button" data-act="stats-refresh" id="stRefresh" title="重新读取统计">${ico('i-probe')}<span class="btn-label">刷新</span></button>
      </div>
    </header>
    <div class="page-body st-body" id="stBody"></div>`;
  statsRendered = true;
}

function syncStatsRangeControls() {
  const days = statsRangeDays();
  $$('#stRange [data-days]').forEach(btn => btn.setAttribute('aria-pressed', String(Number(btn.dataset.days) === days)));
}

function renderStatsBody() {
  const body = document.getElementById('stBody');
  if (!body) return;
  const days = statsRangeDays();
  const label = statsRangeLabel(days);
  const refresh = document.getElementById('stRefresh');
  if (refresh) {
    refresh.disabled = statsState === 'loading';
    refresh.innerHTML = statsState === 'loading' ? '<span class="spin"></span><span class="btn-label">读取中</span>' : `${ico('i-probe')}<span class="btn-label">刷新</span>`;
  }

  if (statsState === 'loading' && !statsRows.length && !body.dataset.filled) {
    body.innerHTML = statsSkeleton();
    return;
  }
  if (statsState === 'error' && !body.dataset.filled) {
    body.innerHTML = `<div class="st-state"><div class="empty">
      <div class="empty-icon" style="color:var(--bad)">${ico('i-bad')}</div>
      <h3>统计读取失败</h3>
      <p></p>
      <button class="btn btn-sm" data-act="stats-refresh">${ico('i-probe')}重试</button></div></div>`;
    body.querySelector('.empty p').textContent = `${statsLoadError || '请求失败'}，稍后重试即可。`;
    const summaryEl = document.getElementById('stSummary');
    if (summaryEl) summaryEl.textContent = `${label} · 读取失败 · 北京时间`;
    return;
  }
  body.classList.toggle('st-refreshing', statsState === 'loading');
  if (statsState === 'loading') return; // 刷新中保留上一份渲染，降低透明度即可

  const sum = summarizeStats(statsRows, days);
  const summaryEl = document.getElementById('stSummary');
  if (summaryEl) summaryEl.textContent = `${label} · ${sum.clients} 种客户端 · ${sum.nodes} 个活跃节点 · 北京时间`;

  if (!statsRows.length) {
    delete body.dataset.filled;
    body.innerHTML = `<div class="st-state"><div class="empty">
      <div class="empty-icon">${ico('i-stats')}</div>
      <h3>${esc(label)}还没有播放记录</h3>
      <p>客户端通过代理播放后，这里会显示播放次数、会话与流量。</p>
      ${days < 30 ? `<button class="btn btn-sm" data-act="stats-range" data-days="30">查看近 30 天</button>` : ''}
    </div></div>
    ${statsImageCacheMarkup(statsImageCache)}`;
    return;
  }
  body.dataset.filled = '1';

  body.innerHTML = `
    ${statsKpisMarkup(sum, days)}
    <div class="st-grid">
      <div class="st-col">${days === 1 ? statsTodayMarkup(sum) : `
        <section class="panel">
          <div class="panel-head"><span class="panel-title">每日播放次数</span><span class="panel-sub">次 · ${esc(label)}</span></div>
          <div class="st-chart" id="stChartPlays" data-chart="plays"></div>
        </section>
        <section class="panel">
          <div class="panel-head"><span class="panel-title">每日出站流量</span><span class="panel-sub">代理发给客户端的数据量 · ${esc(label)}</span></div>
          <div class="st-chart" id="stChartOut" data-chart="outbound"></div>
        </section>`}
        ${statsNodesMarkup(sum)}
      </div>
      <div class="st-col">
        ${statsClientsMarkup(sum)}
      </div>
    </div>
    ${statsImageCacheMarkup(statsImageCache)}
    <section class="panel">
      <div class="panel-head"><span class="panel-title">明细</span><span class="panel-sub" id="stTableSub"></span></div>
      <div class="st-table-wrap"><table class="st-table" id="statsTable"><thead>${STATS_TABLE_HEAD}</thead><tbody id="stTableBody"></tbody></table></div>
      <div class="st-pager" id="stPager"></div>
    </section>`;
  renderStatsTable();
  drawStatsCharts(sum);
}

function statsSkeleton() {
  const tile = '<div class="kpi"><div class="skel" style="height:10px;width:50%"></div><div class="skel" style="height:24px;width:70%"></div><div class="skel" style="height:10px;width:60%"></div></div>';
  return `<div class="st-kpis">${tile.repeat(6)}</div>
    <div class="st-grid"><div class="st-col"><div class="skel" style="height:210px"></div><div class="skel" style="height:210px"></div></div>
    <div class="st-col"><div class="skel" style="height:200px"></div><div class="skel" style="height:220px"></div></div></div>`;
}

function statsKpisMarkup(sum, days) {
  const label = statsRangeLabel(days);
  const t = sum.total;
  const perDay = v => Math.round(v / days);
  const [inV, inU] = bytesParts(t.inbound);
  const [outV, outU] = bytesParts(t.outbound);
  const [durV, durU] = statsDurationParts(t.playback);
  const avgPlays = days > 1 ? `日均 ${formatCount(perDay(t.plays))} 次` : '00:00 起';
  const tiles = [
    ['播放次数', label, formatCount(t.plays), '次', avgPlays],
    ['播放会话', label, formatCount(t.sessions), '个', t.sessions ? `平均每会话 ${(t.plays / t.sessions).toFixed(1)} 次播放` : '—'],
    ['播放时长', label, durV, durU, t.playback ? formatPlaybackDuration(t.playback) : '—'],
    ['入站流量', label, inV, inU, days > 1 ? `日均 ${formatBytes(perDay(t.inbound))}` : '00:00 起'],
    ['出站流量', label, outV, outU, days > 1 ? `日均 ${formatBytes(perDay(t.outbound))}` : '00:00 起'],
    ['活跃节点', label, String(sum.nodes), '个', statsNodeCount ? `共 ${statsNodeCount} 个节点` : '有播放记录的节点'],
  ];
  return `<div class="st-kpis">${tiles.map(([name, period, value, unit, sub]) => `
    <div class="kpi">
      <div class="kpi-label">${esc(name)} <em>${esc(period)}</em></div>
      <div class="kpi-val">${esc(value)}<small>${esc(unit)}</small></div>
      <div class="kpi-sub">${esc(sub)}</div>
    </div>`).join('')}</div>`;
}

/* 「今天」只有一天：画一根柱子没有意义，改成结论式摘要 */
function statsTodayMarkup(sum) {
  const today = sum.days[sum.days.length - 1] || { plays: 0, sessions: 0, outbound: 0, inbound: 0 };
  return `<section class="panel">
    <div class="panel-head"><span class="panel-title">今天</span><span class="panel-sub">北京时间 00:00 起</span></div>
    <div class="st-today">
      <dl class="kv">
        <dt>播放次数</dt><dd class="num">${esc(formatCount(today.plays))} 次</dd>
        <dt>播放会话</dt><dd class="num">${esc(formatCount(today.sessions))} 个</dd>
        <dt>出站流量</dt><dd class="num">${esc(formatBytes(today.outbound))}</dd>
        <dt>入站流量</dt><dd class="num">${esc(formatBytes(today.inbound))}</dd>
      </dl>
      <div class="st-today-hint">${ico('i-info')}<span>趋势需要跨天对比。</span><button class="btn btn-sm btn-ghost" data-act="stats-range" data-days="7">查看近 7 天</button></div>
    </div>
  </section>`;
}

function statsClientsMarkup(sum) {
  const total = sum.total.plays;
  const top = sum.clientList.slice(0, 6);
  const rest = sum.clientList.slice(6);
  const restPlays = rest.reduce((a, b) => a + b.plays, 0);
  const row = (name, plays, mono = true) => {
    const share = total > 0 ? plays / total : 0;
    return `<div class="st-share">
      <div class="st-share-top"><span class="${mono ? 'mono' : ''}" title="${attr(name)}">${esc(name)}</span><span class="num">${esc(formatCount(plays))} 次 · ${Math.round(share * 100)}%</span></div>
      <div class="st-bar"><i style="width:${total > 0 ? Math.max(1.5, share * 100).toFixed(1) : 0}%"></i></div>
    </div>`;
  };
  return `<section class="panel">
    <div class="panel-head"><span class="panel-title">客户端分布</span><span class="panel-sub">按播放次数 · ${sum.clients} 种</span></div>
    <div class="st-list">${top.length ? top.map(c => row(c.client, c.plays)).join('') + (rest.length ? row(`其他 ${rest.length} 种客户端`, restPlays, false) : '') : '<p class="faint">暂无客户端数据</p>'}</div>
  </section>`;
}

function statsNodesMarkup(sum) {
  const items = sum.nodeList;
  return `<section class="panel">
    <div class="panel-head"><span class="panel-title">按节点</span><span class="panel-sub">按出站流量排序</span></div>
    <div class="st-list st-nodes">${items.length ? items.map(item => {
      const idle = item.plays <= 0 && item.outbound <= 0;
      const display = statsNodeLabel(item.node);
      const showPath = item.node && display !== item.node;
      return `<div class="st-node${idle ? ' idle' : ''}">
        <div class="st-node-name"><span title="${attr(display)}">${esc(display)}</span>${showPath ? `<span class="mono">/${esc(item.node)}</span>` : ''}</div>
        <span class="st-node-meta num">${esc(formatCount(item.plays))} 次 · ${esc(formatBytes(item.outbound))}</span>
      </div>`;
    }).join('') : '<p class="faint">暂无节点数据</p>'}</div>
  </section>`;
}

/* 图片缓存概览（只读），缓存未启用时整块不显示。
 * 命中率是进程内、自统计起点以来的口径，标签上要带时间范围。 */
/* 图片缓存：始终显示。未开启时给出入口；开启后展示命中率、空间、条目和节省的回源次数。
 * 命中率是进程内、自统计起点以来的口径，标签上必须带时间范围，否则容易被误读成历史总命中率。 */
function statsImageCacheMarkup(cache) {
  const head = sub => `<div class="panel-head"><span class="panel-title">图片缓存</span>${sub ? `<span class="panel-sub">${sub}</span>` : ''}
      <button class="btn btn-ghost btn-sm" style="margin-left:auto" data-act="open-config" data-tab="image" title="在系统配置中调整图片缓存">${ico('i-settings')}设置</button></div>`;
  if (!cache) {
    return `<section class="panel st-cache">${head('')}<div class="st-cache-off"><span class="faint">图片缓存状态读取失败。</span><button class="btn btn-sm" data-act="stats-refresh">${ico('i-probe')}重试</button></div></section>`;
  }
  if (cache.enabled !== true) {
    return `<section class="panel st-cache">${head('未开启')}
      <div class="st-cache-off">
        <div class="empty-icon">${ico('i-file')}</div>
        <div class="st-cache-off-text"><b>图片缓存未开启</b><span>开启后海报、封面和背景图会缓存在本机，重复请求不再回源，海报墙打开更快，也能减轻上游压力。</span></div>
        <button class="btn btn-sm" data-act="open-config" data-tab="image">去开启</button>
      </div></section>`;
  }
  const bytes = Math.max(0, Number(cache.bytes || 0));
  const maxBytes = Math.max(0, Number(cache.maxBytes || 0));
  const ratio = maxBytes > 0 ? Math.min(bytes / maxBytes, 1) : 0;
  const near = maxBytes > 0 && ratio > 0.9;
  const hits = Math.max(0, Number(cache.hits || 0));
  const misses = Math.max(0, Number(cache.misses || 0));
  const lookups = Math.max(0, Number(cache.lookups || 0));
  const since = Number(cache.statsSince || 0);
  // 没有样本时不带时间范围：刚启动或刚清空时会读成「近 0 秒」。
  const span = lookups > 0 && since > 0 ? formatSpan(Date.now() - since * 1000) : '';
  const rate = lookups > 0 ? Number(cache.hitRate || 0) : null;
  const [usedV, usedU] = bytesParts(bytes);
  const cell = (label, value, sub, extra = '') => `<div class="kpi st-cache-cell"><div class="kpi-label">${label}</div>${value}${sub ? `<div class="kpi-sub">${sub}</div>` : ''}${extra}</div>`;
  return `<section class="panel st-cache">${head(span ? `命中率统计自 ${esc(span)}前起` : '本次启动后还没有图片请求')}
    ${cache.indexReady === false ? `<div class="st-cache-note">${ico('i-loader', 'st-spin')}正在建立缓存索引，条目数和占用空间稍后更新。</div>` : ''}
    <div class="st-cache-grid">
      ${cell('命中率', rate === null ? '<div class="kpi-val idle">—</div>' : `<div class="kpi-val">${Math.round(rate * 100)}<small>%</small></div>`,
        rate === null ? '暂无请求' : `${esc(formatCount(hits))} / ${esc(formatCount(lookups))} 次命中`,
        rate === null ? '' : `<div class="st-bar"><i style="width:${(rate * 100).toFixed(1)}%"></i></div>`)}
      ${cell('已用空间', `<div class="kpi-val">${esc(usedV)}<small>${esc(usedU)}</small></div>`,
        maxBytes > 0 ? `<span class="${near ? 'st-warn' : ''}">${ratio > 0 && ratio < 0.01 ? '<1' : Math.round(ratio * 100)}% · 上限 ${esc(formatBytes(maxBytes))}</span>` : '不限容量',
        maxBytes > 0 ? `<div class="st-bar${near ? ' warn' : ''}"><i style="width:${(ratio * 100).toFixed(1)}%"></i></div>` : '')}
      ${cell('缓存条目', `<div class="kpi-val">${esc(formatCount(cache.entries))}<small>个</small></div>`, `${esc(formatCount(cache.files ?? cache.entries))} 个文件`)}
      ${cell('节省回源', `<div class="kpi-val">${esc(formatCount(hits))}<small>次</small></div>`, lookups > 0 ? `未命中 ${esc(formatCount(misses))} 次，已回源` : '暂无请求')}
    </div>
  </section>`;
}

/* ============================================================
 * 明细表
 * ============================================================ */

function setStatsSort(key) {
  const defaultDirection = STATS_SORT_DEFAULT_DIRECTIONS[key];
  if (!defaultDirection) return;
  if (statsSortKey === key) {
    statsSortDirection = statsSortDirection === 'asc' ? 'desc' : 'asc';
  } else {
    statsSortKey = key;
    statsSortDirection = defaultDirection;
  }
  statsPage = 1;
  renderStatsTable();
}

function renderStatsTable() {
  const tbody = document.getElementById('stTableBody');
  if (!tbody) return;
  const stats = sortedStatsRows();
  const pages = Math.max(1, Math.ceil(stats.length / statsPageSize));
  statsPage = Math.min(Math.max(1, statsPage), pages);
  const start = (statsPage - 1) * statsPageSize;
  const pageRows = stats.slice(start, start + statsPageSize);
  tbody.innerHTML = pageRows.length ? pageRows.map(s => {
    const display = statsNodeLabel(s.node);
    const showPath = s.node && display !== s.node;
    return `<tr>
      <td class="mono st-time">${esc(formatStatsActivity(s))}</td>
      <td><div class="st-cell-node"><span title="${attr(display)}">${esc(display)}</span>${showPath ? `<span class="mono">/${esc(s.node)}</span>` : ''}</div></td>
      <td class="mono st-client" title="${attr(s.client)}">${esc(s.client || '未知客户端')}</td>
      <td class="r">${esc(formatCount(s.plays))}</td>
      <td class="r">${esc(formatCount(s.sessions))}</td>
      <td class="r">${esc(formatPlaybackDuration(statsPlaybackMillis(s)))}</td>
      <td class="r">${esc(formatBytes(trafficInboundBytes(s)))}</td>
      <td class="r">${esc(formatBytes(trafficOutboundBytes(s)))}</td>
    </tr>`;
  }).join('') : '<tr><td class="st-table-empty" colspan="8">暂无数据</td></tr>';
  const sub = document.getElementById('stTableSub');
  if (sub) sub.textContent = `${stats.length} 行 · 每行是一天内某节点上的一种客户端`;
  renderStatsPager(stats.length, start, pageRows.length, pages);
  updateStatsSortHeaders();
}

function renderStatsPager(total, start, count, pages) {
  const pager = document.getElementById('stPager');
  if (!pager) return;
  if (!total) { pager.hidden = true; return; }
  pager.hidden = false;
  pager.innerHTML = `<span class="st-pager-range">第 ${formatCount(start + 1)}–${formatCount(start + count)} 行，共 ${formatCount(total)} 行</span>
    <span class="st-pager-ctl">
      <label class="st-pager-size">每页 <select class="select" id="stPageSize" aria-label="每页行数">${STATS_PAGE_SIZES.map(n => `<option value="${n}" ${n === statsPageSize ? 'selected' : ''}>${n}</option>`).join('')}</select></label>
      <button class="btn btn-sm" data-act="stats-page" data-dir="-1" ${statsPage <= 1 ? 'disabled' : ''} aria-label="上一页">${ico('i-left')}上一页</button>
      <span class="st-pager-num num">${statsPage} / ${pages}</span>
      <button class="btn btn-sm" data-act="stats-page" data-dir="1" ${statsPage >= pages ? 'disabled' : ''} aria-label="下一页">下一页${ico('i-right')}</button>
    </span>`;
}

/* 翻页后把表头拉回视野，避免停在表格底部看不到新一页的开头 */
function revealStatsTable() {
  const table = document.getElementById('statsTable');
  const body = table?.closest('.page-body');
  if (!table || !body) return;
  const top = table.getBoundingClientRect().top - body.getBoundingClientRect().top;
  if (top < 0) body.scrollTop += top - 12;
}

function sortedStatsRows() {
  return statsRows.map((row, index) => ({ row, index })).sort((left, right) => {
    const primary = compareStatsField(left.row, right.row, statsSortKey);
    if (primary !== 0) return applySortDirection(primary, statsSortDirection);
    for (const [key, direction] of STATS_SORT_FALLBACKS) {
      if (key === statsSortKey) continue;
      const compared = compareStatsField(left.row, right.row, key);
      if (compared !== 0) return applySortDirection(compared, direction);
    }
    return left.index - right.index;
  }).map(item => item.row);
}

function compareStatsField(left, right, key) {
  if (key === 'day' || key === 'client') {
    return statsTextCollator.compare(String(left?.[key] || ''), String(right?.[key] || ''));
  }
  if (key === 'node') return statsTextCollator.compare(statsNodeLabel(left?.node), statsNodeLabel(right?.node));
  if (key === 'playbackMillis') return compareStatsNumber(statsPlaybackMillis(left), statsPlaybackMillis(right));
  if (key === 'inboundBytes') return compareStatsNumber(trafficInboundBytes(left), trafficInboundBytes(right));
  if (key === 'outboundBytes') return compareStatsNumber(trafficOutboundBytes(left), trafficOutboundBytes(right));
  return compareStatsNumber(Number(left?.[key] || 0), Number(right?.[key] || 0));
}

function compareStatsNumber(left, right) {
  if (left === right) return 0;
  return left < right ? -1 : 1;
}

function applySortDirection(compared, direction) {
  return direction === 'asc' ? compared : -compared;
}

function updateStatsSortHeaders() {
  $$('#statsTable th[data-stats-sort]').forEach(th => {
    const active = th.dataset.statsSort === statsSortKey;
    th.setAttribute('aria-sort', active ? (statsSortDirection === 'asc' ? 'ascending' : 'descending') : 'none');
    const use = th.querySelector('use');
    if (use) use.setAttribute('href', active ? (statsSortDirection === 'asc' ? '#i-sort-asc' : '#i-sort-desc') : '#i-sort');
    th.querySelector('button')?.classList.toggle('on', active);
  });
}

/* ============================================================
 * 每日柱状图：两张独立的图，各自一条纵轴
 * ============================================================ */

let statsChartData = null;

function statsNiceStep(raw) {
  if (raw <= 0) return 1;
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / pow;
  const nice = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
  return nice * pow;
}

/* 纵轴刻度：字节类按最大值选定一个单位，在该单位下取整齐的步长 */
function statsAxis(max, kind) {
  if (kind === 'bytes') {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let u = 0;
    let scaled = max;
    while (scaled >= 1000 && u < units.length - 1) { scaled /= 1000; u++; }
    const step = statsNiceStep(Math.max(scaled, 1) / 4);
    const top = Math.max(step, Math.ceil(scaled / step) * step);
    const factor = Math.pow(1000, u);
    return { top: top * factor, ticks: [0, 1, 2, 3, 4].map(k => step * k).filter(v => v <= top + 1e-9).map(v => ({ v: v * factor, label: `${+v.toFixed(2)} ${units[u]}` })) };
  }
  const step = Math.max(1, statsNiceStep(Math.max(max, 1) / 4));
  const top = Math.max(step, Math.ceil(max / step) * step);
  const ticks = [];
  for (let v = 0; v <= top + 1e-9; v += step) ticks.push({ v, label: formatCount(Math.round(v)) });
  return { top, ticks };
}

function drawStatsCharts(sum) {
  statsChartData = sum || statsChartData;
  if (!statsChartData) return;
  ['stChartPlays', 'stChartOut'].forEach(id => {
    const el = document.getElementById(id);
    if (el) drawStatsBars(el, statsChartData.days, el.dataset.chart);
  });
}

function drawStatsBars(el, days, kind) {
  const isBytes = kind === 'outbound';
  const valueOf = d => isBytes ? d.outbound : d.plays;
  const W = Math.max(260, el.clientWidth - 16);
  const narrow = W < 520;
  const padL = isBytes ? 58 : 44, padR = 8, padT = 22, padB = 24, plotH = narrow ? 120 : 140;
  const H = padT + plotH + padB;
  const pw = W - padL - padR;
  const n = days.length;
  const band = pw / n;
  const bw = Math.max(3, Math.min(28, band * 0.62));
  const max = Math.max(0, ...days.map(valueOf));
  const axis = statsAxis(max, isBytes ? 'bytes' : 'count');
  const y = v => padT + plotH - (v / axis.top) * plotH;
  const fmt = v => isBytes ? formatBytes(v) : `${formatCount(v)} 次`;

  let g = '';
  axis.ticks.forEach(t => {
    const yy = y(t.v).toFixed(1);
    g += `<line x1="${padL}" x2="${W - padR}" y1="${yy}" y2="${yy}" stroke="var(--grid)"/>`;
    g += `<text x="${padL - 8}" y="${+yy + 4}" text-anchor="end" fill="var(--text-3)" font-size="11" font-family="var(--font-mono)">${esc(t.label)}</text>`;
  });
  g += `<line x1="${padL}" x2="${W - padR}" y1="${padT + plotH}" y2="${padT + plotH}" stroke="var(--line-strong)"/>`;

  // 日期刻度：总是标出今天，其余按间隔抽样，避免挤在一起
  const every = n <= 10 && !narrow ? 1 : Math.ceil(n / (narrow ? 4 : 8));
  let peak = -1;
  days.forEach((d, i) => { if (valueOf(d) > 0 && (peak < 0 || valueOf(d) > valueOf(days[peak]))) peak = i; });
  days.forEach((d, i) => {
    const v = valueOf(d);
    const cx = padL + (i + 0.5) * band;
    const x0 = cx - bw / 2;
    const isToday = i === n - 1;
    if (v > 0) {
      const top = Math.min(y(v), padT + plotH - 2);
      const h = padT + plotH - top;
      const r = Math.min(3, bw / 2, h);
      g += `<path class="st-bar-mark" data-i="${i}" d="M${x0.toFixed(1)} ${padT + plotH}V${(top + r).toFixed(1)}Q${x0.toFixed(1)} ${top.toFixed(1)} ${(x0 + r).toFixed(1)} ${top.toFixed(1)}H${(x0 + bw - r).toFixed(1)}Q${(x0 + bw).toFixed(1)} ${top.toFixed(1)} ${(x0 + bw).toFixed(1)} ${(top + r).toFixed(1)}V${padT + plotH}Z" fill="var(--accent)" opacity="${isToday ? 1 : 0.62}"/>`;
    }
    if ((n - 1 - i) % every === 0) {
      const p = statsDayParts(d.day);
      g += `<text x="${cx.toFixed(1)}" y="${padT + plotH + 16}" text-anchor="middle" fill="var(--text-3)" font-size="11" font-family="var(--font-mono)">${isToday ? '今天' : `${p.m}/${p.d}`}</text>`;
    }
  });
  if (peak >= 0) {
    const cx = padL + (peak + 0.5) * band;
    const anchor = cx > W - 70 ? 'end' : cx < padL + 50 ? 'start' : 'middle';
    g += `<text x="${cx.toFixed(1)}" y="${(y(valueOf(days[peak])) - 7).toFixed(1)}" text-anchor="${anchor}" fill="var(--text-2)" font-size="11">最高 ${esc(fmt(valueOf(days[peak])))}</text>`;
  }
  // 命中区：整列，比柱子宽，便于悬停与键盘聚焦
  days.forEach((d, i) => {
    g += `<rect class="st-hit" data-i="${i}" x="${(padL + i * band).toFixed(1)}" y="${padT}" width="${band.toFixed(1)}" height="${plotH + padB}" fill="transparent" tabindex="0" aria-label="${attr(`${d.day} ${fmt(valueOf(d))}`)}"/>`;
  });

  el.innerHTML = `<div class="chart-wrap st-chart-wrap">
    <svg viewBox="0 0 ${W} ${H}" height="${H}" role="img" aria-label="${isBytes ? '每日出站流量柱状图' : '每日播放次数柱状图'}">${g}</svg>
    <div class="ttip" hidden></div></div>`;

  const wrap = el.querySelector('.chart-wrap');
  const svg = wrap.querySelector('svg');
  const tip = wrap.querySelector('.ttip');
  const show = i => {
    const d = days[i];
    const p = statsDayParts(d.day);
    svg.querySelectorAll('.st-bar-mark').forEach(m => m.classList.toggle('on', Number(m.dataset.i) === i));
    const rows = isBytes
      ? [['出站流量', formatBytes(d.outbound), true], ['入站流量', formatBytes(d.inbound)], ['播放次数', `${formatCount(d.plays)} 次`]]
      : [['播放次数', `${formatCount(d.plays)} 次`, true], ['播放会话', `${formatCount(d.sessions)} 个`], ['播放时长', d.playback ? formatPlaybackDuration(d.playback) : '—']];
    tip.innerHTML = `<div class="ttip-time"></div>${rows.map(([k, v, key]) => `<div class="ttip-row"><i style="border-color:${key ? 'var(--accent)' : 'transparent'}"></i><span>${esc(k)}</span><b>${esc(v)}</b></div>`).join('')}`;
    tip.querySelector('.ttip-time').textContent = `${p.m}月${p.d}日 ${p.weekday}${i === n - 1 ? ' · 今天' : ''}`;
    tip.hidden = false;
    const rect = svg.getBoundingClientRect();
    const wrect = wrap.getBoundingClientRect();
    const scale = rect.width / W;
    const left = (padL + (i + 0.5) * band) * scale + (rect.left - wrect.left);
    const tw = tip.offsetWidth;
    tip.style.left = `${Math.max(4, left + 14 + tw > wrect.width ? left - tw - 14 : left + 14)}px`;
    tip.style.top = '8px';
  };
  const hide = () => { tip.hidden = true; svg.querySelectorAll('.st-bar-mark.on').forEach(m => m.classList.remove('on')); };
  svg.querySelectorAll('.st-hit').forEach(hit => {
    const i = Number(hit.dataset.i);
    hit.addEventListener('pointerenter', () => show(i));
    hit.addEventListener('focus', () => show(i));
    hit.addEventListener('blur', hide);
  });
  svg.addEventListener('pointerleave', hide);
}

/* ============================================================
 * 加载
 * ============================================================ */

async function loadStats() {
  const days = statsRangeDays();
  const token = ++statsLoadToken;
  statsState = 'loading';
  syncStatsRangeControls();
  renderStatsBody();
  // 图片缓存、节点名与播放统计并发拉；前两者失败只影响对应区块。
  const [statsRes, listRes, cacheRes] = await Promise.all([
    api('stats.get', { days }),
    api('list').catch(() => null),
    api('imageCache.stats').catch(() => null),
  ]);
  if (token !== statsLoadToken) return;
  if (statsRes?._stale) return;
  if (listRes?.ok && Array.isArray(listRes.nodes)) {
    statsNodeNames = {};
    listRes.nodes.forEach(n => { if (n?.name) statsNodeNames[n.name] = n.displayName || n.name; });
    statsNodeCount = listRes.nodes.length;
  }
  statsImageCache = cacheRes?.ok ? cacheRes.cache || null : null;
  if (!statsRes?.ok) {
    statsLoadError = apiError(statsRes, '读取统计失败');
    statsState = 'error';
    const body = document.getElementById('stBody');
    if (body?.dataset.filled) {
      // 新范围读取失败：页面上仍是旧范围的数据，范围按钮和文案一起回退，避免把 7 天的数据当成 30 天显示
      if (statsRowsRange !== null) currentStatsRange = statsRowsRange;
      syncStatsRangeControls();
      statsState = 'ready';
      renderStatsBody();
      toast('bad', '统计刷新失败', `${statsLoadError}，显示的是上一次读取的数据。`);
      return;
    }
    renderStatsBody();
    return;
  }
  statsRows = Array.isArray(statsRes.stats) ? statsRes.stats : [];
  if (statsRowsRange !== days) statsPage = 1; // 换了时间范围从第一页看起；同范围刷新保留当前页
  statsRowsRange = days;
  statsState = 'ready';
  renderStatsBody();
}

/* 系统配置里改了图片缓存后，统计页的缓存卡片跟着刷新 */
async function refreshStatsImageCache() {
  if (!statsRendered) return;
  const r = await api('imageCache.stats');
  if (r._stale) return;
  statsImageCache = r.ok ? r.cache || null : null;
  if (statsState === 'ready') renderStatsBody();
}

registerActions({
  'stats-range': el => {
    const days = Number(el.dataset.days);
    if (![1, 7, 30].includes(days)) return;
    if (days === statsRangeDays() && statsState === 'ready') return;
    currentStatsRange = days;
    loadStats();
  },
  'stats-refresh': () => loadStats(),
  'stats-sort': el => setStatsSort(el.dataset.key),
  'stats-page': el => { statsPage += Number(el.dataset.dir) || 0; renderStatsTable(); revealStatsTable(); },
});

document.addEventListener('change', e => {
  if (e.target.id !== 'stPageSize') return;
  const size = Number(e.target.value);
  if (!STATS_PAGE_SIZES.includes(size)) return;
  // 保持当前页第一行仍在视野里
  const firstRow = (statsPage - 1) * statsPageSize;
  statsPageSize = size;
  statsPage = Math.floor(firstRow / size) + 1;
  renderStatsTable();
});

registerPage('stats', {
  enter() {
    if (!statsRendered) renderStatsShell();
    syncStatsRangeControls();
    loadStats();
  },
  leave() {
    statsLoadToken++; // 丢弃仍在路上的响应
    if (statsState === 'loading') statsState = statsRows.length ? 'ready' : 'idle';
  },
  reset() {
    statsLoadToken++;
    statsRows = [];
    statsRowsRange = null;
    statsPage = 1;
    statsNodeNames = {};
    statsNodeCount = 0;
    statsImageCache = null;
    statsChartData = null;
    statsState = 'idle';
    statsRendered = false;
    const page = document.getElementById('pageStats');
    if (page) page.innerHTML = '';
  },
  resize() {
    drawStatsCharts();
  },
});
