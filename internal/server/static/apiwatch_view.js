// apiwatch_view.js — the 系统 view's 接口监控 section: the model API retries,
// failed turns, watchdog timeouts and latency GET /api/system/apiwatch reports
// (internal/apiwatch). Read-only; the CLI does the retrying.
// Layering: never import dashboard.js or system_view.js back; the view
// repaints through the onChange callback it hands in.
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, formatDurationShort, registerActions } from './nz_util.js';
import { formatAbsTime, timeAgo } from './utilities.js';

// Class ids are internal/apiwatch's Class values.
const CLASS_LABELS = {
  region: '地区限制（400）',
  auth: '凭证 / 鉴权',
  rate_limit: '限流（429）',
  overloaded: '过载（529）',
  server: '服务端错误（5xx）',
  client: '请求被拒（4xx）',
  first_byte: '首字节超时',
  timeout: '请求超时',
  network: '连接错误',
  watchdog: '看门狗超时',
  other: '其他失败',
};
const CLASS_HINTS = {
  region: '出口 IP 被判在不支持的地区：多半是 VPN / 公司网络没接上',
  auth: '临时凭证过期或取不到：检查 Midway（mwinit）与 awsCredentialExport',
  rate_limit: '请求太密，被限流',
  overloaded: '模型端过载，CLI 会退避重试',
  first_byte: '请求发出后迟迟没有响应头，CLI 断开重发',
  network: '连不上接口：网络切换、睡眠唤醒后的旧连接、代理',
  watchdog: 'naozhi 看门狗：长时间无输出或总时长超限，进程被杀',
};
const KIND_LABELS = { retry: '重试', turn: '失败', timeout: '超时' };
const WINDOWS = [[1, '1 小时'], [6, '6 小时'], [24, '24 小时'], [72, '3 天'], [168, '7 天']];

let report = null;
// unavailable: the endpoint answered nothing usable (an older backend, a
// mock without it); the section then takes no room, like 全会话健康.
let unavailable = false;
let hours = 24;
// inflight is the window (hours) of the request in flight, 0 for none.
let inflight = 0;
let onChange = null;

// fetchedAt throttles the 系统 view's 5s poll: the report moves on the scale
// of turns, so every 15s is enough.
let fetchedAt = 0;
const MIN_INTERVAL_MS = 15000;

function fetchAPIWatch(repaint, force) {
  if (repaint) onChange = repaint;
  if (inflight === hours || (!force && report && Date.now() - fetchedAt < MIN_INTERVAL_MS)) return Promise.resolve();
  // A window switched mid-request: the answer for the old one is dropped.
  const h = hours;
  inflight = h;
  fetchedAt = Date.now();
  return fetchJSON(NZ_CONTRACT.API.system_apiwatch + '?hours=' + h, { timeoutMs: 8000 })
    .then((data) => { if (h === hours) { report = data && data.summary ? data : null; unavailable = !report; } })
    .catch(() => { if (h === hours && !report) unavailable = true; })
    .finally(() => { if (inflight === h) inflight = 0; if (h === hours && onChange) onChange(); });
}

function pct(n, d) {
  if (!d) return '—';
  const p = n / d * 100;
  return (p < 10 ? p.toFixed(1) : Math.round(p)) + '%';
}

function statHtml(label, value, sub, title) {
  return '<div class="aw-stat"' + (title ? ' title="' + escAttr(title) + '"' : '') + '>' +
    '<div class="aw-stat-label">' + esc(label) + '</div>' +
    '<div class="aw-stat-value">' + esc(value) + '</div>' +
    (sub ? '<div class="aw-stat-sub">' + esc(sub) + '</div>' : '') +
  '</div>';
}

function latText(l) {
  return l && l.n ? formatDurationShort(l.p50) + ' / ' + formatDurationShort(l.p95) : '—';
}

// bucketLabel names a bucket by its start: the time alone today, with the
// day before it otherwise.
function bucketLabel(t) {
  const d = new Date(t);
  const hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
  return d.toDateString() === new Date().toDateString() ? hm : (d.getMonth() + 1) + '/' + d.getDate() + ' ' + hm;
}

// barsHtml is one single-series bar chart over the buckets: value(b) gives
// the bar, tip(b) its hover text. Bars rise from the baseline; the top value
// is the only axis label.
function barsHtml(title, buckets, value, fmt, tip) {
  const max = buckets.reduce((m, b) => Math.max(m, value(b)), 0);
  const bars = buckets.map((b) => {
    const v = value(b);
    const h = max ? Math.max(v > 0 ? 3 : 0, Math.round(v / max * 100)) : 0;
    return '<div class="aw-bar" title="' + escAttr(tip(b)) + '"><span style="height:' + h + '%"></span></div>';
  }).join('');
  const first = buckets.length ? bucketLabel(buckets[0].start) : '';
  const last = buckets.length ? bucketLabel(buckets[buckets.length - 1].start) : '';
  return '<div class="aw-chart">' +
    '<div class="aw-chart-head"><span>' + esc(title) + '</span><span class="aw-chart-max">' + (max ? '峰值 ' + esc(fmt(max)) : '无数据') + '</span></div>' +
    '<div class="aw-bars" role="img" aria-label="' + escAttr(title) + '">' + bars + '</div>' +
    '<div class="aw-axis"><span>' + esc(first) + '</span><span>' + esc(last) + '</span></div>' +
  '</div>';
}

function bucketTip(b) {
  return bucketLabel(b.start) + '\n轮次 ' + b.turns + ' · 失败 ' + b.failed +
    ' · 重试 ' + b.retries + ' · 超时 ' + b.timeouts +
    (b.ttft_p95 ? '\n首字 p95 ' + formatDurationShort(b.ttft_p95) : '') +
    (b.api_p95 ? ' · 接口耗时 p95 ' + formatDurationShort(b.api_p95) : '');
}

function whenHtml(t) {
  const ms = Date.parse(t);
  return isNaN(ms) ? '—' : '<span title="' + escAttr(formatAbsTime(ms)) + '">' + esc(timeAgo(ms)) + '</span>';
}

function detailText(e) {
  const parts = [];
  if (e.kind === 'retry') {
    if (e.attempt) parts.push('第 ' + e.attempt + (e.max_retries ? '/' + e.max_retries : '') + ' 次');
    if (e.waited_ms) parts.push('等了 ' + formatDurationShort(e.waited_ms) + ' 无响应');
    if (e.retry_delay_ms) parts.push(formatDurationShort(e.retry_delay_ms) + ' 后重试');
  }
  if (e.kind === 'timeout') parts.push(e.reason === 'total_timeout' ? '总时长超限' : '长时间无输出');
  if (e.status) parts.push('HTTP ' + e.status);
  if (e.category && e.category !== 'unknown') parts.push(e.category);
  if (e.error) parts.push(e.error);
  return parts.join(' · ');
}

function recentRowHtml(e) {
  const detail = detailText(e);
  return '<tr>' +
    '<td class="hl-num">' + whenHtml(e.at) + '</td>' +
    '<td>' + esc(KIND_LABELS[e.kind] || e.kind) + '</td>' +
    '<td>' + esc(CLASS_LABELS[e.class] || e.class || '—') + '</td>' +
    '<td><span class="hl-name" title="' + escAttr(e.key) + '">' + esc(e.key) + '</span></td>' +
    '<td class="aw-detail" title="' + escAttr(detail) + '">' + esc(detail || '—') + '</td>' +
  '</tr>';
}

function renderAPIWatchHtml() {
  const tabs = WINDOWS.map(([h, label]) =>
    '<button type="button" class="aw-win' + (h === hours ? ' on' : '') + '" data-action="apiwatch-window" data-hours="' + h + '"' +
    (h === hours ? ' aria-pressed="true"' : ' aria-pressed="false"') + '>' + esc(label) + '</button>').join('');
  const head = '<div class="hl-head"><h2 class="svc-overview-title">接口监控</h2><div class="aw-wins" role="group" aria-label="时间范围">' + tabs + '</div></div>';
  if (!report && unavailable) return '';
  if (!report) {
    return '<section class="sys-apiwatch" aria-label="接口监控">' + head + '<div class="system-empty">加载中…</div></section>';
  }
  const s = report.summary;
  const problems = s.retries + s.failed_turns + s.timeouts;
  const stats = '<div class="aw-stats">' +
    statHtml('轮次', String(s.turns), s.retried_turns ? s.retried_turns + ' 轮发生过重试' : '') +
    statHtml('失败', String(s.failed_turns), pct(s.failed_turns, s.turns), '以错误结束的轮次（不含手动停止）') +
    statHtml('重试', String(s.retries), '', 'CLI 自动重试的接口请求次数') +
    statHtml('超时', String(s.timeouts), '', 'naozhi 看门狗杀掉的轮次') +
    statHtml('首字延迟 p50 / p95', latText(s.ttft), s.ttft.n ? '最长 ' + formatDurationShort(s.ttft.max) : '', '从这一轮开始到第一个 token，含发请求前的准备') +
    statHtml('接口耗时 p50 / p95', latText(s.api), s.api.n ? '最长 ' + formatDurationShort(s.api.max) : '', '一轮里所有模型接口调用的总时长') +
  '</div>';
  const charts = '<div class="aw-charts">' +
    barsHtml('问题数（重试 + 失败 + 超时）', report.buckets, (b) => b.retries + b.failed + b.timeouts, String, bucketTip) +
    barsHtml('首字延迟 p95', report.buckets, (b) => b.ttft_p95, formatDurationShort, bucketTip) +
  '</div>';
  let classes = '';
  if (report.classes.length) {
    classes = '<div class="aw-classes">' + report.classes.map((c) => {
      const hint = CLASS_HINTS[c.class] || '';
      const title = [hint, c.sample].filter(Boolean).join('\n');
      return '<div class="aw-class"' + (title ? ' title="' + escAttr(title) + '"' : '') + '>' +
        '<span class="aw-class-name">' + esc(CLASS_LABELS[c.class] || c.class) + '</span>' +
        '<b>' + c.count + '</b>' +
        '<span class="aw-class-last">最近 ' + whenHtml(c.last) + '</span>' +
        (hint ? '<div class="aw-class-hint">' + esc(hint) + '</div>' : '') +
      '</div>';
    }).join('') + '</div>';
  }
  const recent = report.recent.length
    ? '<div class="hl-scroll aw-recent"><table class="hl-table">' +
        '<thead><tr><th>时间</th><th>类型</th><th>原因</th><th>会话</th><th>详情</th></tr></thead>' +
        '<tbody>' + report.recent.map(recentRowHtml).join('') + '</tbody></table></div>'
    : '<div class="system-empty">' + (s.turns ? '这段时间接口调用没有出问题。' : '这段时间没有接口调用记录。') + '</div>';
  const slow = report.slowest.length
    ? '<details class="aw-slow"><summary>最慢的 ' + report.slowest.length + ' 轮</summary><div class="hl-scroll"><table class="hl-table">' +
        '<thead><tr><th>时间</th><th>会话</th><th>模型</th><th>接口耗时</th><th>首字</th><th>总时长</th></tr></thead><tbody>' +
        report.slowest.map((e) => '<tr><td class="hl-num">' + whenHtml(e.at) + '</td>' +
          '<td><span class="hl-name" title="' + escAttr(e.key) + '">' + esc(e.key) + '</span></td>' +
          '<td>' + esc(e.model || '—') + '</td>' +
          '<td class="hl-num">' + esc(formatDurationShort(e.api_ms)) + '</td>' +
          '<td class="hl-num">' + esc(formatDurationShort(e.ttft_ms)) + '</td>' +
          '<td class="hl-num">' + esc(formatDurationShort(e.duration_ms)) + '</td></tr>').join('') +
      '</tbody></table></div></details>'
    : '';
  const lead = problems
    ? '<div class="aw-lead">共 ' + problems + ' 个问题，按原因：</div>'
    : '';
  return '<section class="sys-apiwatch" aria-label="接口监控">' + head + stats + charts + lead + classes + recent + slow + '</section>';
}

registerActions({
  'apiwatch-window': (el) => {
    const h = parseInt(el.dataset.hours, 10);
    if (!h || h === hours) return;
    hours = h;
    report = null;
    if (onChange) onChange();
    fetchAPIWatch(null, true);
  },
});

export { fetchAPIWatch, renderAPIWatchHtml };
