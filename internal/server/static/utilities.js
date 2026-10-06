// utilities.js — extracted from dashboard.js (#2558 D4).
//
// A leaf (caps.leaves): it imports only contract.js, state.js, nz_util.js and
// the other leaves, so any module can import it without forming a cycle.
// Shared state is read from the state.js objects.
import { NZ_CONTRACT } from './contract.js';
import { composer, perSession, selection, serverInfo, sessionList, timers, transcript, ui } from './state.js';
import { esc, escAttr, showToast, trapFocus, sessionExitChipHtml } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { authHeaders, getToken, lsSet } from './platform.js';
import { sid } from './session_ident.js';
import { featureForBackend, pendingBackendID } from './features.js';

// --- Utilities ---

// mainEmptyHtml returns the inner HTML for `#main` when no session is
// selected. Called after dismiss/remove flows that nuke the active
// session. Kept in sync with the cold-start markup in dashboard.html —
// both render a `>_` mark, a Chinese lead line "问点什么？", and the
// quick-ask textarea that fires submitQuickAsk() on Enter. Consolidating
// the copies into a helper means a future tweak touches one place and
// prevents cold-start / dismiss-path divergence.
//
// After rendering, callers SHOULD invoke wireQuickAskInput() to bind the
// keydown / auto-grow handlers on the freshly-painted textarea (the cold
// start HTML gets wired on DOMContentLoaded).
function mainEmptyHtml() {
  return '<div class="empty-state empty-cta empty-quick nz-stack">' +
    '<span class="nz-empty-glyph" aria-hidden="true">&gt;_</span>' +
    '<div class="nz-empty-title">问点什么？</div>' +
    '<form class="quick-ask-form" id="quick-ask-form">' +
      '<textarea id="quick-ask-input" class="quick-ask-input" rows="1" ' +
        'placeholder="Enter 发送 · Shift+Enter 换行" autocomplete="off" spellcheck="false" ' +
        'aria-label="快速提问输入框"></textarea>' +
      '<button type="submit" class="quick-ask-send" aria-label="发送">' +
        '<svg viewBox="0 0 24 24" aria-hidden="true">' +
          '<line x1="22" y1="2" x2="11" y2="13"/>' +
          '<polygon points="22 2 15 22 11 13 2 9 22 2"/>' +
        '</svg></button>' +
    '</form>' +
    '<div class="nz-hint-dim">默认目录 · general agent · 随时 <code>/cd</code> 切换目录，或用上方 <b>+</b> 开项目会话</div>' +
    // R110-P1 空闲态 Home 仪表 MVP 占位：renderRecentSessionsPanel()
    // 按需注入"最近会话"缩略列表；零 session 时渲染为空字符串，保留冷启动
    // 简洁空态不退化。Helper 外部调用，不嵌在本 HTML 里以保持 pure 可读。
    '<div id="recent-sessions-panel" class="recent-panel-wrap"></div>' +
  '</div>';
}

// computeHomeStats aggregates sessionList.allSessionsCache into the two stats surfaced
// on the idle Home panel. Pure function so a contract test can exercise the
// "today" boundary and cost summation without driving the DOM.
//
// Scope is deliberately conservative: the TODO lists 4 metrics (today active
// / prompts processed / tokens / cost), but prompts and tokens require an
// event-log scan or a backend aggregator that doesn't exist yet. The two
// metrics here (today active count, total cost) are already shipped in
// /api/sessions per-session fields.
//
//   todayActive — sessions whose last_active >= local-midnight today. Uses
//                 the JS Date constructor so the user's browser timezone
//                 matches what they'd consider "today" in the sidebar.
//   totalCost   — sum of s.total_cost across all cached sessions (not gated
//                 by today, because a cron-heavy workspace accumulates cost
//                 overnight and wiping at midnight would hide it).
//   totalPrompts — sum of s.message_count across all cached sessions. The
//                 SessionSnapshot already ships message_count (the cumulative
//                 "user" turn count observed by the live process event log),
//                 so the "已处理 prompt 数" card (R110-P1 #445) is derivable
//                 client-side without the deferred /api/stats/aggregate
//                 backend scan. Cumulative tokens still need that backend
//                 endpoint (no per-session token field exists yet), so the
//                 token card stays out of scope here.
//
// Input shape tolerant: missing last_active / total_cost / message_count on a
// session contributes zero / is skipped rather than NaN-poisoning the totals.
function computeHomeStats(items, nowMs) {
  const arr = Array.isArray(items) ? items : [];
  const now = typeof nowMs === 'number' ? nowMs : Date.now();
  const d = new Date(now);
  const dayStart = new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, 0, 0, 0).getTime();
  let todayActive = 0;
  let totalCost = 0;
  let totalPrompts = 0;
  for (const s of arr) {
    if (!s) continue;
    if (typeof s.last_active === 'number' && s.last_active >= dayStart) todayActive++;
    if (typeof s.total_cost === 'number' && isFinite(s.total_cost)) totalCost += s.total_cost;
    if (typeof s.message_count === 'number' && isFinite(s.message_count) && s.message_count > 0) totalPrompts += s.message_count;
  }
  return { todayActive: todayActive, totalCost: totalCost, totalPrompts: totalPrompts };
}

// formatHomeCost keeps the $/precision format close to the session card's
// header cost chip (.high-cost / .has-cost): two decimals once cost is
// measurable, four decimals for sub-cent fractions (so "$0.0023" still
// shows signal instead of collapsing to $0.00).
function formatHomeCost(cost) {
  const c = typeof cost === 'number' && isFinite(cost) ? cost : 0;
  if (c >= 0.01) return '$' + c.toFixed(2);
  if (c > 0) return '$' + c.toFixed(4);
  return '$0.00';
}

// buildHomeHealthLines turns a stats snapshot into up to 2 Chinese lines for
// the bottom health strip of the Home panel. Pure function so a contract
// test can exercise each data-path without driving the DOM. Returns [] when
// the snapshot is missing entirely — caller suppresses the strip.
//
// Line shape:
//   Line 1: running/ready/total counts + uptime (always when stats present)
//   Line 2: CLI name + version (when defaultCLIName is set)
//   Line 3 (gated): watchdog kills — only when > 0; signals prod trouble
//
// Scope: leans entirely on fields ALREADY in /api/sessions `stats`. The TODO
// lists claude 子进程数 / shim 连通 / cron 队列长度 / 状态文件大小 as future
// additions — those need backend extensions, so omit here rather than
// inventing empty placeholders that would never fill.
function buildHomeHealthLines(stats) {
  if (!stats || typeof stats !== 'object') return [];
  const lines = [];
  // Line 1: session breakdown + uptime.
  const running = typeof stats.running === 'number' ? stats.running : 0;
  const ready = typeof stats.ready === 'number' ? stats.ready : 0;
  const total = typeof stats.total === 'number' ? stats.total : 0;
  let line1 = '运行中 ' + running + ' · 就绪 ' + ready + ' · 总 ' + total;
  if (stats.uptime) line1 += ' · 已运行 ' + stats.uptime;
  lines.push({ text: line1, kind: 'info' });
  // claude 子进程容量 (R110-P1 #445 "claude 子进程数"): max_procs ships in the
  // /api/sessions stats static block already, so surface live-vs-capacity
  // without the deferred /api/stats backend scan. Only when max_procs > 0
  // (a 0/missing cap means "uncapped" — no ratio to show). Warn when the
  // pool is saturated so operators notice spawn back-pressure.
  const maxProcs = typeof stats.max_procs === 'number' ? stats.max_procs : 0;
  if (maxProcs > 0) {
    lines.push({
      text: 'claude 子进程 ' + running + '/' + maxProcs,
      kind: running >= maxProcs ? 'warn' : 'info',
    });
  }
  // Line 2: CLI identity. Helpful when operators have multiple naozhi
  // deployments on different CLI versions.
  if (stats.cli_name) {
    let cli = stats.cli_name;
    if (stats.cli_version) cli += ' ' + stats.cli_version;
    lines.push({ text: cli, kind: 'info' });
  }
  // naozhi build tag (R110-P1 #445 service-health): version_tag already ships
  // in the /api/sessions stats block (omitempty when the -X ldflag is unset)
  // and the backend struct doc promises a "naozhi v1.2.3-dirty" footer that
  // was never wired client-side. Surface it so operators can confirm the
  // running build straight from the Home health strip.
  if (stats.version_tag) {
    lines.push({ text: 'naozhi ' + stats.version_tag, kind: 'info' });
  }
  // Multi-Backend RFC §8.3 D22: when ≥2 backends are configured, show a
  // one-liner summarizing per-backend availability + version. The rich
  // per-feature table lives in the doctor status panel (built by
  // renderBackendsDoctorPanel below) — this line is just the at-a-glance
  // health roll-up.
  if (serverInfo.cliBackends && Array.isArray(serverInfo.cliBackends.backends) && serverInfo.cliBackends.backends.length > 1) {
    const okCount = serverInfo.cliBackends.backends.filter(b => b && b.available).length;
    const totalCount = serverInfo.cliBackends.backends.length;
    const ids = serverInfo.cliBackends.backends.map(b => (b && b.id) || '?').join(' · ');
    lines.push({
      text: 'Backends: ' + okCount + '/' + totalCount + ' (' + ids + ')',
      kind: okCount === totalCount ? 'info' : 'warn',
    });
  }
  // Line 3 (gated): watchdog kills > 0 is a prod signal operators should see.
  const wd = stats.watchdog || {};
  const totalKills = typeof wd.total_kills === 'number' ? wd.total_kills : 0;
  if (totalKills > 0) {
    const noOutput = typeof wd.no_output_kills === 'number' ? wd.no_output_kills : 0;
    lines.push({
      text: 'Watchdog 已介入 ' + totalKills + ' 次（无输出 ' + noOutput + '）',
      kind: 'warn',
    });
  }
  return lines;
}

// renderRecentSessionsPanel populates the Home-panel slot inside the main
// empty-state body. Reads sessionList.allSessionsCache (written by renderSidebar after
// each fetchSessions → so reflects the same authoritative snapshot the
// sidebar shows), picks the 5 most recently active sessions, and renders a
// compact clickable list. When there are zero sessions, returns an empty
// innerHTML so the cold-start minimal CTA stays unchanged. Callers must
// guard by selection.key == null (active-session main shell wins).
//
// Pure-rendering: writes to the DOM by id rather than returning HTML, because
// the cold-start HTML already carries the placeholder div and we don't want
// to fight the order of initial paint.
function renderRecentSessionsPanel() {
  const host = document.getElementById('recent-sessions-panel');
  if (!host) return;
  if (selection.key) return; // active session rendered by renderMainShell
  const items = Array.isArray(sessionList.allSessionsCache) ? sessionList.allSessionsCache : [];
  if (items.length === 0) { host.innerHTML = ''; return; }
  // Sort by last_active desc; sessions without last_active sink to the
  // bottom so a brand-new "new" card doesn't squat on position 1 forever.
  const top = items.slice().sort((a, b) => (b.last_active || 0) - (a.last_active || 0)).slice(0, 5);
  const rows = top.map(s => {
    const sNode = s.node || 'local';
    const label = s.user_label || s.summary || s.last_prompt || '未命名';
    const state = s.state === 'dead' ? 'ready' : (s.state || 'ready');
    const dotCls = state === 'running' ? 'dot-running' : (state === 'ready' ? 'dot-ready' : 'dot-new');
    const ago = s.last_active ? timeAgo(s.last_active) : '';
    return '<button type="button" class="recent-row" ' +
      'data-key="' + escAttr(s.key) + '" data-node="' + escAttr(sNode) + '" ' +
      'data-action="session-select">' +
      '<span class="recent-dot ' + dotCls + '" aria-hidden="true"></span>' +
      '<span class="recent-label" title="' + escAttr(label) + '">' + esc(label) + '</span>' +
      sessionExitChipHtml(s.state, s.death_reason, s.death_detail, s.startup_failure) +
      (ago ? '<span class="recent-time">' + esc(ago) + '</span>' : '') +
      '</button>';
  }).join('');
  host.innerHTML =
    '<div class="recent-panel">' +
      '<div class="recent-panel-title">最近会话</div>' +
      '<div class="recent-panel-list" role="list">' + rows + '</div>' +
    '</div>';
}

// summarizeCostBuckets folds a /api/cost/summary (group_by=unit) payload into
// the card model. Pure so a contract test can drive it without the DOM.
function summarizeCostBuckets(data) {
  const out = { usd: 0, credits: 0, tokens: 0, entries: 0, unknown: 0, dropped: 0, partial: 0, loadedAt: Date.now() };
  const buckets = data && Array.isArray(data.buckets) ? data.buckets : [];
  for (const b of buckets) {
    if (!b || typeof b.amount !== 'number' || !isFinite(b.amount)) continue;
    if (b.unit === 'USD') out.usd += b.amount;
    else if (b.unit === 'credits') out.credits += b.amount;
    else if (b.unit === 'tokens') out.tokens += b.amount;
    out.entries += (b.entries | 0);
  }
  if (data && data.basis && typeof data.basis.unknown === 'number') out.unknown = data.basis.unknown;
  if (data && typeof data.dropped === 'number') out.dropped = data.dropped;
  if (data && data.kinds && typeof data.kinds.partial === 'number') out.partial = data.kinds.partial;
  return out;
}

// buildCostHealthLines adds a warn line when the ledger reports dropped
// entries or unknown-priced models, so the figure on the card is read with
// the right amount of trust. [] when the ledger is healthy or not loaded.
function buildCostHealthLines(c) {
  if (!c) return [];
  const lines = [];
  if (c.dropped > 0) {
    lines.push({ text: '成本账本丢弃 ' + c.dropped + ' 条（写入队列满），金额偏低', kind: 'warn' });
  }
  if (c.unknown > 0) {
    lines.push({ text: '成本账本含 ' + c.unknown + ' 条未知定价（模型不在 CLI 价表）', kind: 'warn' });
  }
  if (c.partial > 0) {
    lines.push({ text: '成本账本含 ' + c.partial + ' 个进程中断的轮次（按 CLI 实测单价估算）', kind: 'info' });
  }
  return lines;
}

// costCardTitle explains what the ledger figure is (and is not): a CLI
// estimate at list/contract price, never an invoice. A narrower `scope` marks
// the dropped count ledger-wide: a lost entry has no owner to filter on.
function costCardTitle(c, scope) {
  let t = '近 30 天账本合计（' + (scope || '会话 + cron + 云沙箱') + '），CLI 估算口径，非账单';
  if (c.unknown > 0) t += '；含 ' + c.unknown + ' 条未知定价（模型不在 CLI 价表，按默认模型估算）';
  if (c.dropped > 0) t += (scope ? '；整个账本曾丢弃 ' + c.dropped + ' 条（无法归到来源），此项可能偏低' : '；账本曾丢弃 ' + c.dropped + ' 条，金额可能偏低');
  return t;
}

// refreshCostSummary pulls the ledger's last-30-day totals, for the whole
// ledger or one session key (a daemon books under sys:<name>), into
// costSummaries. Each key is asked at most once per 30 s (attempts, not
// successes, so a failing endpoint is not hammered on every repaint), one
// fetch in flight at a time; resolves true when the key took a new snapshot,
// so the caller can repaint. Failures keep the previous snapshot.
const COST_SUMMARY_TTL_MS = 30 * 1000;
const costSummaries = new Map();
async function refreshCostSummary(sessionKey = '') {
  let e = costSummaries.get(sessionKey);
  if (!e) costSummaries.set(sessionKey, e = { at: 0, inFlight: false, c: null });
  const now = Date.now();
  if (e.inFlight || (now - e.at) < COST_SUMMARY_TTL_MS) return false;
  e.at = now;
  e.inFlight = true;
  try {
    const data = await fetchCostSummary('group_by=unit' + (sessionKey ? '&session_key=' + encodeURIComponent(sessionKey) : ''));
    if (!data) return false;
    e.c = summarizeCostBuckets(data);
    return true;
  } catch (_) {
    return false;
  } finally {
    e.inFlight = false;
  }
}

// cachedCostSummary is refreshCostSummary(sessionKey)'s last snapshot, or null.
const cachedCostSummary = (sessionKey = '') => (costSummaries.get(sessionKey) || { c: null }).c;

// fetchCostSummary GETs /api/cost/summary for the last 30 days with `query`
// (already encoded) and resolves the body, or null on a non-2xx answer.
async function fetchCostSummary(query) {
  const to = new Date();
  const from = new Date(to.getTime() - 30 * 24 * 3600 * 1000);
  const resp = await fetch(NZ_CONTRACT.API.cost_summary + '?' + query + '&from=' + encodeURIComponent(from.toISOString()) +
    '&to=' + encodeURIComponent(to.toISOString()), { headers: authHeaders() });
  return resp.ok ? resp.json() : null;
}

// fetchCostBudget resolves /api/cost/budget for `query` (an encoded
// session_key= or job_id=), or null when the request fails.
const fetchCostBudget = (query) => fetch(NZ_CONTRACT.API.cost_budget + '?' + query, { headers: authHeaders() })
  .then((r) => (r.ok ? r.json() : null)).catch(() => null);

// costBudgetChipHtml renders a /api/cost/budget answer as "今日 $x / $y",
// ⚠-flagged from warn_ratio on, or '' when no cost.budget cap applies.
const COST_BUDGET_SCOPES = { chat: '本聊天', project: '本项目（绑定的群共享）', job: '本任务', global: '整机' };
function costBudgetChipHtml(b, cls) {
  if (!b || !(b.limit > 0)) return '';
  const usage = '$' + Number(b.spent || 0).toFixed(2) + ' / $' + b.limit.toFixed(2);
  const title = (COST_BUDGET_SCOPES[b.scope] || '') + '今日费用预算（cost.budget）已用 ' + usage + '，' + formatAbsTime(b.reset_at) +
    ' 重置' + (b.blocked ? '；已用尽，新消息和 cron 运行会被拒绝' : b.over ? '；已超出，仅提醒' : '');
  return '<span class="' + cls + (b.over ? ' bad' : '') + '" title="' + escAttr(title) + '">' + (b.warn ? '⚠ ' : '') + '今日 ' + esc(usage) + '</span>';
}

// costStatHtml renders the 花费 card: ledger figure when loaded (with a
// credits sub-line for kiro sessions and an honest hover explanation),
// otherwise the legacy live-session sum labelled as such.
function costStatHtml(stats) {
  const c = cachedCostSummary();
  if (!c) {
    return '<div class="svc-stat" title="当前会话列表 total_cost 之和（不含已删会话 / cron）">' +
        '<div class="svc-stat-value">' + esc(formatHomeCost(stats.totalCost)) + '</div>' +
        '<div class="svc-stat-label">累计花费</div>' +
      '</div>';
  }
  const sub = c.credits > 0
    ? '<div class="svc-stat-sub">' + esc(c.credits.toFixed(2)) + ' credits</div>'
    : '';
  const flag = (c.unknown > 0 || c.dropped > 0)
    ? ' <span class="svc-stat-flag" role="img" aria-label="金额存疑">⚠</span>'
    : '';
  return '<div class="svc-stat" title="' + escAttr(costCardTitle(c)) + '">' +
      '<div class="svc-stat-value">' + esc(formatHomeCost(c.usd)) + flag + '</div>' +
      sub +
      '<div class="svc-stat-label">近 30 天花费</div>' +
    '</div>';
}

// renderServiceOverviewHtml builds the 系统 view's 服务概览: today's stats, the
// health strip from the /api/sessions snapshot and the backend doctor panel.
function renderServiceOverviewHtml() {
  const items = Array.isArray(sessionList.allSessionsCache) ? sessionList.allSessionsCache : [];
  const stats = computeHomeStats(items, Date.now());
  const statsHtml =
    '<div class="svc-stats" role="group" aria-label="今日概览">' +
      '<div class="svc-stat">' +
        '<div class="svc-stat-value">' + stats.todayActive + '</div>' +
        '<div class="svc-stat-label">今日活跃会话</div>' +
      '</div>' +
      '<div class="svc-stat">' +
        '<div class="svc-stat-value">' + stats.totalPrompts + '</div>' +
        '<div class="svc-stat-label">已处理 prompt</div>' +
      '</div>' +
      costStatHtml(stats) +
    '</div>';
  const healthLines = buildHomeHealthLines(serverInfo.lastStatsSnapshot);
  for (const l of buildCostHealthLines(cachedCostSummary())) healthLines.push(l);
  const healthHtml = healthLines.length === 0
    ? ''
    : '<div class="svc-health" role="status" aria-label="服务健康">' +
        healthLines.map(l =>
          '<div class="svc-health-line ' + esc(l.kind || 'info') + '">' + esc(l.text) + '</div>'
        ).join('') +
      '</div>';
  // Multi-Backend RFC §8.3 D22: doctor status panel — clickable details
  // block listing each enabled backend's caps + features. Single-backend
  // mode skips it (renderBackendsDoctorPanel returns '').
  const doctorHtml = renderBackendsDoctorPanel();
  return '<section class="svc-overview" aria-label="服务概览">' +
      '<h2 class="svc-overview-title">服务概览</h2>' +
      statsHtml + healthHtml + doctorHtml +
    '</section>';
}

// renderBackendsDoctorPanel builds a foldable <details> panel listing each
// enabled backend with its protocol caps + user-feature flags. Multi-Backend
// RFC §8.3 D22. Returns '' for single-backend deployments / when serverInfo.cliBackends
// is unavailable so the cold-start home page doesn't show a half-empty
// section. The output includes a small "▼" affordance and a screen-reader
// label so keyboard users know the section is expandable.
function renderBackendsDoctorPanel() {
  if (!serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return '';
  if (serverInfo.cliBackends.backends.length <= 1) return '';
  const rows = serverInfo.cliBackends.backends.map(b => {
    if (!b) return '';
    const id = esc(b.id || '?');
    const name = esc(b.display_name || b.id || '?');
    const ver = b.version ? ' v' + esc(b.version) : '';
    const proto = b.protocol ? esc(b.protocol) : '';
    const status = b.available
      ? '<span class="doctor-status doctor-status-ok">●</span>'
      : '<span class="doctor-status doctor-status-bad" title="binary missing or --version probe failed">○</span>';
    // !Array.isArray gate: typeof [] is 'object', so without this an array-typed
    // features field would render numeric-keyed pills like "0", "1". Review
    // (PR #121) catch — server contract says object{flag:bool}, but be defensive.
    const features = b.features && typeof b.features === 'object' && !Array.isArray(b.features)
      ? b.features
      : {};
    // Render features as compact pills — green for supported, struck for missing.
    const featPills = Object.keys(features).sort().map(k => {
      const on = features[k] === true;
      return '<span class="doctor-feat ' + (on ? 'doctor-feat-on' : 'doctor-feat-off') +
        '" title="' + escAttr(k) + (on ? '' : ' (not supported)') + '">' + esc(k) + '</span>';
    }).join('');
    return '<div class="doctor-row">' +
      '<div class="doctor-row-head">' + status +
        '<span class="doctor-row-name">[' + id + '] ' + name + ver + '</span>' +
        (proto ? '<span class="doctor-row-proto">' + proto + '</span>' : '') +
      '</div>' +
      (featPills ? '<div class="doctor-row-feats">' + featPills + '</div>' : '') +
    '</div>';
  }).join('');
  const defaultID = esc(serverInfo.cliBackends.default || '');
  // Arrow is supplied by the .doctor-summary::before CSS so it can flip
  // 90° on [open]. Don't bake it into the text.
  return '<details class="doctor-panel" aria-label="后端状态详情">' +
    '<summary class="doctor-summary">Backends 状态 (default: ' + defaultID + ')</summary>' +
    '<div class="doctor-body">' + rows + '</div>' +
  '</details>';
}

// showToast moved to nz_util.js (PR-0a). Available as window.nz.util.showToast
// and the top-level alias window.showToast, loaded before this file.

// RNEW-UX-010 — polite announcement into #sr-announce for screen readers.
// Used for signals that don't surface as a toast (WS connect/disconnect,
// new-session arrival, cron completion). We clear the textContent after a
// short tick so an identical follow-up message still re-triggers the AT
// announcement (some readers skip unchanged text). Silent no-op if the
// element isn't mounted yet (e.g. during very early boot).
function announce(msg) {
  const el = document.getElementById('sr-announce');
  if (!el || !msg) return;
  el.textContent = '';
  setTimeout(() => { el.textContent = String(msg); }, 50);
  clearTimeout(el._clearTid);
  el._clearTid = setTimeout(() => { el.textContent = ''; }, 3000);
}

// API_ERROR_HEADS: substrings of server error messages → the Chinese head
// localizeAPIError uses instead of the status-class one. Labels mirror
// internal/server classifyWorkspaceErr (work_dir), internal/dashboard/cron
// writeAddUpdateRejection and internal/dashboard/discovery writeTakeoverRefusal.
const API_ERROR_HEADS = [
  ['work_dir outside allowed root', '工作目录不在允许范围内'],
  ['work_dir does not exist', '工作目录不存在'],
  ['work_dir is not a directory', '路径不是目录'],
  ['work_dir is not a valid path', '工作目录路径不合法'],
  ['work_dir must be an absolute path', '工作目录必须是绝对路径'],
  ['cron job quota reached', '定时任务数已达上限，请先删除不用的任务'],
  ['schedule interval below the 5m minimum', '执行间隔不能短于 5 分钟'],
  ['takeover refused: max concurrent processes reached', '进程数已满，外部进程未被终止；请先关闭一个空闲会话后重试'],
  ['takeover already in progress', '该会话正在被接管，请稍候'],
  ['takeover refused: router is shutting down', '服务正在重启，外部进程未被终止，请稍后重试'],
];

// localizeAPIError turns an HTTP status code + raw server message into a
// user-facing Chinese string. Classifies by status class so operators get
// a consistent mental model — 4xx = "你这边要改", 5xx = "服务端问题，请
// 稍后重试". The raw tail is appended (truncated to 120 chars) so diagnostic
// signal isn't lost, but the Chinese prefix is always there for screen-readers
// and non-technical operators. Single locale (zh-CN), so no i18n layer.
function localizeAPIError(status, raw) {
  const tail = (raw || '').toString().trim().slice(0, 120);
  const withTail = tail ? '（' + tail + '）' : '';
  if (status === 0 || status === undefined || status === null) {
    return '网络错误' + withTail;
  }
  if (status === 401) {
    return '鉴权失败，请重新登录' + withTail;
  }
  // 服务端自带原因的标签换成精确中文，免得状态码类的通用文案误导操作员
  // （如配额满的 409 被说成"刷新后重试"）。表见 API_ERROR_HEADS。
  const r = String(raw || '');
  const known = API_ERROR_HEADS.find(([needle]) => r.indexOf(needle) !== -1);
  if (known) return known[1] + withTail;
  if (status === 403) {
    return '无权限或参数越界' + withTail;
  }
  if (status === 404) {
    return '资源不存在' + withTail;
  }
  if (status === 409) {
    return '状态冲突，请刷新后重试' + withTail;
  }
  if (status === 413) {
    return '内容过大' + withTail;
  }
  if (status === 429) {
    return '请求过于频繁，请稍后重试' + withTail;
  }
  if (status >= 400 && status < 500) {
    return '请求失败（HTTP ' + status + '）' + withTail;
  }
  if (status === 502 || status === 503 || status === 504) {
    return '服务暂时不可用，请稍后重试' + withTail;
  }
  if (status >= 500) {
    return '服务器错误（HTTP ' + status + '）' + withTail;
  }
  return '操作失败（HTTP ' + status + '）' + withTail;
}

// showAPIError renders a HTTP-failed fetch as a Chinese toast. `action`
// is a short user-facing verb (e.g. '删除会话', '保存任务') — it prefixes
// the localized status reason, so the full toast reads like
// "删除会话失败：鉴权失败，请重新登录（...）". Pass the raw server message
// as `raw` (from `await r.text()`) for diagnostic context; truncated at the
// localize layer.
function showAPIError(action, status, raw, duration) {
  const msg = (action ? action + '失败：' : '') + localizeAPIError(status, raw);
  showToast(msg, 'error', duration);
}

// showNetworkError handles the catch-branch of fetch/awaited calls. A thrown
// Error typically means the request never reached the server (DNS / offline
// / CORS / abort). Keep the Chinese verbiage identical to localizeAPIError's
// status=0 arm so the user's mental model stays unified.
function showNetworkError(action, err, duration) {
  const detail = (err && err.message) ? err.message.slice(0, 120) : '';
  const tail = detail ? '（' + detail + '）' : '';
  const msg = (action ? action + '失败：' : '') + '网络错误' + tail;
  showToast(msg, 'error', duration);
}

// reconnectNow cancels any pending reconnect timer, resets the exponential
// backoff so the next failure window starts tight again, and kicks an
// immediate connect. Triggered by the sidebar-status "reconnect" button
// that surfaces after backoff has grown past 8s (see updateStatusBar).
// Idempotent: double-click only results in one connect attempt because
// wsm.connect short-circuits when the socket is already OPEN/CONNECTING.
function reconnectNow() {
  if (wsm.reconnectTimer) {
    clearTimeout(wsm.reconnectTimer);
    wsm.reconnectTimer = null;
  }
  wsm.backoff = 1000;
  // No toast: the sidebar status row already flips to "connecting..." when
  // wsm.connect() sets CONNECTING, and the outage/reconnect button update
  // through updateStatusBar. A toast here was redundant with that signal.
  wsm.connect();
}

function fallbackCopy(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.cssText = 'position:fixed;left:-9999px';
  document.body.appendChild(ta);
  ta.select();
  document.execCommand('copy');
  document.body.removeChild(ta);
}

// Flash a button to "copied!" state for ~1.5s then revert.
function flashCopyButton(btn) {
  btn.textContent = 'copied!';
  btn.classList.add('copied');
  setTimeout(() => { btn.textContent = 'copy'; btn.classList.remove('copied'); }, 1500);
}

// Shared clipboard helper for in-line buttons — uses navigator.clipboard with
// an execCommand fallback for non-HTTPS / older browsers.
function copyWithFeedback(btn, text) {
  const done = () => flashCopyButton(btn);
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).then(done).catch(() => { fallbackCopy(text); done(); });
  } else {
    fallbackCopy(text);
    done();
  }
}

function copyCodeBlock(btn) {
  // DOM may be re-rendered between render and click (event list ticks every
  // ~1s). Fall back silently instead of throwing when the wrap is gone.
  const { code } = _codeBlockInfo(btn);
  if (!code) return;
  copyWithFeedback(btn, code);
}

function _codeBlockInfo(btn) {
  const wrap = btn.closest('.md-code-wrap');
  if (!wrap) return { code: '', lang: '' };
  // Path-list blocks (.md-pathlist) render one <code> per line instead of a
  // single <pre><code>, so copy must join every row's text — querySelector
  // alone would copy just the first path. file-ref button injection may also
  // nest extra <code> inside .fr-slot, so scope to the row's first <code>.
  if (wrap.classList.contains('md-pathlist')) {
    const code = Array.from(wrap.querySelectorAll('.md-pathline'))
      .map(row => { const c = row.querySelector('code'); return c ? c.textContent : ''; })
      .join('\n');
    return { code, lang: '' };
  }
  const codeEl = wrap.querySelector('code');
  const code = codeEl ? codeEl.textContent : '';
  const lang = (codeEl && codeEl.getAttribute('data-lang') || '').toLowerCase();
  return { code, lang };
}

// Snippet payload for preview drawer. Storing in a module variable instead of
// drawer.dataset avoids the multi-MB attribute truncation and DOM-serialize cost.

function copyEventContent(btn) {
  const text = btn.dataset.raw || btn.closest('.event').querySelector('.event-content').textContent;
  copyWithFeedback(btn, text);
}

function shortPath(p) {
  // Collapse the OS home prefix to ~. Previously only Linux (/home/<user>/)
  // matched, so on macOS every palette row repeated the full
  // /Users/<user>/… prefix and the project name drowned in boilerplate
  // (ui-polish-light-theme D6).
  for (const home of ['/home/', '/Users/']) {
    const i = p.indexOf(home);
    if (i >= 0) {
      const rest = p.substring(i + home.length);
      const slash = rest.indexOf('/');
      if (slash >= 0) return '~' + rest.substring(slash);
    }
  }
  return p.length > 40 ? '...' + p.substring(p.length - 37) : p;
}

// historyDayLabel formats a Date as the history drawer's day-group
// label. Today and yesterday collapse to \u4e2d\u6587 "\u4eca\u5929" / "\u6628\u5929" so the
// most common buckets read instantly without parsing a date. Older
// entries defer to the browser locale so CJK users see "4\u670829\u65e5 \u5468\u4e09"
// and EN users see "Wed, Apr 29" \u2014 both read naturally now that the
// .hp-day-header uppercase was dropped in Round 129.
//
// Exposed at module scope (not inside renderHistoryPopover) so the
// Round 129 contract test can assert its existence and both branches
// are easy to eyeball in the source.
function historyDayLabel(d) {
  if (!d || isNaN(d.getTime())) return '';
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const target = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const diffDays = Math.round((today.getTime() - target.getTime()) / 86400000);
  if (diffDays === 0) return '\u4eca\u5929';
  if (diffDays === 1) return '\u6628\u5929';
  const opts = { month: 'short', day: 'numeric', weekday: 'short' };
  if (d.getFullYear() !== today.getFullYear()) opts.year = 'numeric';
  return d.toLocaleDateString(undefined, opts);
}

// Sidebar relative-time ticker. While WS is connected renderSidebar only runs
// on sessions_update (the 5s /api/sessions poll short-circuits on an unchanged
// version), so every minute the .sc-time text is recomputed from the data-ts
// stamp renderSessionCard emits instead of rebuilding the sidebar. Paused while
// the tab is hidden via the visibilitychange gate below (same as the other
// pollers) — stale text on a hidden tab costs nothing.
const SIDEBAR_TIME_TICK_MS = 60000;
let sidebarTimeTimer = null;
function refreshSidebarTimes() {
  const list = document.getElementById('session-list');
  if (!list) return;
  list.querySelectorAll('.sc-time[data-ts]').forEach(el => {
    const ts = Number(el.dataset.ts);
    if (!ts) return;
    const txt = timeAgo(ts);
    if (el.textContent !== txt) el.textContent = txt;
  });
}
function startSidebarTimeTick() {
  if (sidebarTimeTimer) return;
  refreshSidebarTimes();
  sidebarTimeTimer = setInterval(refreshSidebarTimes, SIDEBAR_TIME_TICK_MS);
}
function stopSidebarTimeTick() {
  if (sidebarTimeTimer) { clearInterval(sidebarTimeTimer); sidebarTimeTimer = null; }
}

function timeAgo(ms, future) {
  if (!ms) return '\u2014';
  const d = future ? ms - Date.now() : Date.now() - ms;
  if (d < 0) return future ? 'now' : 'just now';
  const suffix = future ? '' : ' ago';
  if (d < 5000) return future ? 'now' : 'just now';
  if (d < 60000) return Math.floor(d/1000) + 's' + suffix;
  if (d < 3600000) return Math.floor(d/60000) + 'm' + suffix;
  if (d < 86400000) return Math.floor(d/3600000) + 'h' + suffix;
  return Math.floor(d/86400000) + 'd' + suffix;
}

// formatAbsTime renders an epoch-ms timestamp in local time as
// "YYYY-MM-DD HH:MM:SS (TZ)" for use inside title attributes on the various
// "3m ago" / "next 2h" relative labels. The goal is R110-P3: keep the
// compact relative form in the UI, but let hover reveal the exact instant
// so operators can reason about long-running jobs / stale sessions without
// doing mental arithmetic. Falls back to '' on falsy input so callers can
// safely gate the title attribute with a truthy check.
function formatAbsTime(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  const pad = n => (n < 10 ? '0' + n : '' + n);
  const tz = (() => {
    const off = -d.getTimezoneOffset();
    const sign = off >= 0 ? '+' : '-';
    const abs = Math.abs(off);
    return 'UTC' + sign + pad(Math.floor(abs / 60)) + ':' + pad(abs % 60);
  })();
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
    ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) +
    ' (' + tz + ')';
}

// trapFocus moved to nz_util.js (PR-0a). Available as window.nz.util.trapFocus
// and the top-level alias window.trapFocus, loaded before this file.

// confirmDialog renders a styled confirm prompt matching the rest of the
// dashboard (reuses .modal-overlay / .modal / .modal-btns). Returns a Promise
// that resolves to `true` on confirm or `false` on cancel / Esc / backdrop
// click. Native window.confirm() is blocking + looks out of place next to our
// custom dark-theme modals; this helper fixes both.
//
// Call shape:
//   const ok = await confirmDialog({
//     title: '删除定时任务？',
//     message: '任务将被永久删除，下次不再触发。',
//     detail: 'cron-id-12345',      // optional mono-spaced tail
//     confirmText: '删除',
//     variant: 'danger',            // 'danger' | 'primary' (default danger)
//   });
//
// Semantics:
//   - Default focus lands on the CANCEL button (safer than focusing the
//     destructive primary). Enter in a destructive dialog still requires
//     the user to Tab over first, matching macOS confirm dialogs.
//   - Esc and backdrop click both resolve to false — identical to the
//     cancel button. Consistent with every other modal in the dashboard.
//   - XSS-safe: all caller-supplied text is routed through esc() before
//     insertion; callers may pass untrusted content (session key, cron id).
//   - If a dialog is already open, this call resolves immediately to false
//     to avoid stacking multiple confirms on the same decision.
function confirmDialog(opts) {
  return new Promise((resolve) => {
    if (document.querySelector('.modal-overlay.confirm-overlay')) {
      resolve(false);
      return;
    }
    const title = (opts && opts.title) || '确认操作';
    const message = (opts && opts.message) || '';
    const detail = (opts && opts.detail) || '';
    const confirmText = (opts && opts.confirmText) || '确认';
    const cancelText = (opts && opts.cancelText) || '取消';
    const variant = (opts && opts.variant) || 'danger';
    const confirmClass = variant === 'danger' ? 'danger' : 'primary';
    // Round 2 R-12: optional countdown (seconds) before the confirm
    // button activates. Used by destructive flows to insert a "速度带"
    // — short enough to not annoy, long enough to catch fat-fingered
    // double-Enter. Default 0 = no countdown (legacy behaviour).
    const countdownSecs = (opts && typeof opts.countdownSecs === 'number') ? Math.max(0, Math.floor(opts.countdownSecs)) : 0;

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay confirm-overlay';
    // message may include line breaks now (RFC §7.5 multi-paragraph
    // copy). Render via <pre>-style white-space:pre-wrap on .confirm-msg
    // so existing toast-style single-line callers still look right.
    overlay.innerHTML =
      '<div class="modal confirm-dialog" role="alertdialog" aria-modal="true" aria-labelledby="confirm-title">' +
        '<h3 id="confirm-title">' + esc(title) + '</h3>' +
        (message ? '<p class="confirm-msg">' + esc(message) + '</p>' : '') +
        (detail ? '<p class="confirm-detail"><code>' + esc(detail) + '</code></p>' : '') +
        '<div class="modal-btns">' +
          '<button type="button" class="confirm-cancel">' + esc(cancelText) + '</button>' +
          '<button type="button" class="' + confirmClass + ' confirm-ok"' + (countdownSecs > 0 ? ' disabled' : '') + '>' +
            esc(confirmText) + (countdownSecs > 0 ? ' (' + countdownSecs + ')' : '') +
          '</button>' +
        '</div>' +
      '</div>';

    let settled = false;
    let tickTimer = null;
    const finish = (ok) => {
      if (settled) return;
      settled = true;
      if (tickTimer) { clearInterval(tickTimer); tickTimer = null; }
      overlay.remove();
      resolve(!!ok);
    };

    const okBtn = overlay.querySelector('.confirm-ok');
    overlay.querySelector('.confirm-cancel').addEventListener('click', () => finish(false));
    okBtn.addEventListener('click', () => {
      // Defensive: while disabled the click won't fire on a real button,
      // but if a custom CSS rule ever overrides pointer-events we still
      // refuse early activation by checking the disabled attribute.
      if (okBtn.hasAttribute('disabled')) return;
      finish(true);
    });
    // Backdrop click cancels. Guard against inner clicks bubbling through
    // by checking that the click's target is the overlay itself.
    overlay.addEventListener('click', (e) => { if (e.target === overlay) finish(false); });
    // trapFocus handles Esc (removes overlay); mirror that by observing the
    // removal and resolving false if the consumer used Esc / other removal.
    const obs = new MutationObserver(() => {
      if (!document.body.contains(overlay)) { obs.disconnect(); finish(false); }
    });
    obs.observe(document.body, { childList: true, subtree: false });

    document.body.appendChild(overlay);
    trapFocus(overlay);
    // Focus cancel first — protects against a stray Enter auto-firing the
    // destructive primary. User must explicitly Tab or click to confirm.
    setTimeout(() => overlay.querySelector('.confirm-cancel').focus(), 50);

    if (countdownSecs > 0) {
      let remaining = countdownSecs;
      tickTimer = setInterval(() => {
        remaining -= 1;
        if (remaining <= 0) {
          clearInterval(tickTimer);
          tickTimer = null;
          okBtn.removeAttribute('disabled');
          okBtn.textContent = confirmText;
          // Live region for SR — announce activation politely so a
          // keyboard user knows they can now Tab + Enter.
          okBtn.setAttribute('aria-live', 'polite');
        } else {
          okBtn.textContent = confirmText + ' (' + remaining + ')';
        }
      }, 1000);
    }
  });
}

// RNEW-UX-013: promptDialog is the themed replacement for native window.prompt().
// Matches confirmDialog shape (overlay + .modal-btns) so the two share styling,
// trapFocus, Esc/backdrop-cancel semantics, and XSS-safe rendering. Returns a
// Promise that resolves to the trimmed input string on confirm, or null on
// cancel / Esc / backdrop click (mirroring window.prompt's null-for-cancel
// convention so existing call sites translate without special-casing).
//
// Call shape:
//   const next = await promptDialog({
//     title: '重命名会话',
//     message: '留空恢复默认标题，最多 128 字节',
//     defaultValue: current,
//     placeholder: '输入新标题',
//     confirmText: '保存',
//     maxLength: 128,
//   });
//   if (next === null) return;  // user cancelled
//
// Semantics:
//   - Enter inside the input submits (matching window.prompt expectations —
//     the information-entry dialog defaults to primary, not cancel, because
//     the action isn't destructive).
//   - Esc and backdrop cancel (resolve null). Consistent with confirmDialog.
//   - If a prompt dialog is already open, this call resolves immediately to
//     null to avoid stacking.
//   - XSS-safe: every caller-supplied string routes through esc() before
//     insertion. defaultValue is set via .value (DOM property) not innerHTML.
function promptDialog(opts) {
  return new Promise((resolve) => {
    if (document.querySelector('.modal-overlay.prompt-overlay')) {
      resolve(null);
      return;
    }
    const title = (opts && opts.title) || '输入内容';
    const message = (opts && opts.message) || '';
    const defaultValue = (opts && opts.defaultValue) != null ? String(opts.defaultValue) : '';
    const placeholder = (opts && opts.placeholder) || '';
    const confirmText = (opts && opts.confirmText) || '确认';
    const cancelText = (opts && opts.cancelText) || '取消';
    const maxLength = (opts && opts.maxLength) || 0;

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay prompt-overlay';
    const maxAttr = maxLength > 0 ? ' maxlength="' + maxLength + '"' : '';
    overlay.innerHTML =
      '<div class="modal prompt-dialog" role="dialog" aria-modal="true" aria-labelledby="prompt-title">' +
        '<h3 id="prompt-title">' + esc(title) + '</h3>' +
        (message ? '<p class="prompt-message">' + esc(message) + '</p>' : '') +
        '<input type="text" class="prompt-input" placeholder="' + escAttr(placeholder) + '"' + maxAttr + '>' +
        '<div class="modal-btns">' +
          '<button type="button" class="prompt-cancel">' + esc(cancelText) + '</button>' +
          '<button type="button" class="primary prompt-ok">' + esc(confirmText) + '</button>' +
        '</div>' +
      '</div>';

    const input = overlay.querySelector('.prompt-input');
    // Use the DOM .value property (not innerHTML interpolation) to seed the
    // default — avoids having to escape attribute quotes and preserves any
    // literal whitespace the caller relied on.
    input.value = defaultValue;

    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      overlay.remove();
      resolve(value);
    };

    overlay.querySelector('.prompt-cancel').addEventListener('click', () => finish(null));
    overlay.querySelector('.prompt-ok').addEventListener('click', () => finish(input.value));
    overlay.addEventListener('click', (e) => { if (e.target === overlay) finish(null); });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); finish(input.value); }
    });
    // Mirror confirmDialog: if the overlay is removed externally (trapFocus
    // Esc handling, or a caller manipulating DOM), settle as cancelled.
    const obs = new MutationObserver(() => {
      if (!document.body.contains(overlay)) { obs.disconnect(); finish(null); }
    });
    obs.observe(document.body, { childList: true, subtree: false });

    document.body.appendChild(overlay);
    trapFocus(overlay);
    // Focus the input so the user can type immediately. Select all so the
    // default value is replaced on first keystroke — matches window.prompt
    // behaviour in Chrome/Firefox.
    setTimeout(() => { input.focus(); input.select(); }, 50);
  });
}

// Auth-prompt de-dupe + debounce. Two guards keep the token modal from
// machine-gunning back open: (1) only ever one overlay at a time, and
// (2) after the operator explicitly dismisses the prompt, suppress
// *background* re-prompts (the 5s /api/sessions poll, WS reconnect) for a
// cooldown window. User-initiated actions (send / upload) pass {auto:false}
// and bypass the cooldown so a click still gets immediate feedback. A
// successful login clears the cooldown.
const authModalCooldown = { until: 0 };
const AUTH_MODAL_COOLDOWN_MS = 60000;

function showAuthModal(opts) {
  opts = opts || {};
  // De-dupe: never stack a second auth prompt over an existing modal.
  if (document.querySelector('.modal-overlay')) return;
  // Debounce: a freshly-dismissed prompt should not be reopened by the
  // next background poll. User actions (auto !== true) always prompt.
  if (opts.auto && Date.now() < authModalCooldown.until) return;
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="Dashboard API token">' +
      // R110-P3 brand lockup: `>_` mark + `naozhi` wordmark anchors the
      // login screen so operators recognize they're on the right service.
      // Mirrors the `>_` glyph used in the empty state; pure text (no image
      // asset) keeps the static bundle tiny.
      '<div class="auth-brand">' +
        '<div class="ab-mark" aria-hidden="true">&gt;_</div>' +
        '<div class="ab-wordmark">' +
          '<span class="ab-name">naozhi</span>' +
          '<span class="ab-tag">Claude Code on IM</span>' +
        '</div>' +
      '</div>' +
      '<h3>Dashboard API Token</h3>' +
      // R110-P3 brand/onboarding hint: first-time operators often don't know
      // where the token comes from. Points them at the one configuration
      // surface (dashboard_token in config.yaml). Kept concise; full docs live
      // in README.md and docs/ops/ so the modal stays task-focused.
      '<div class="auth-hint">token 配置于 <code>config.yaml</code> 的 <code>dashboard_token</code> 字段</div>' +
      '<input id="token-input" type="password" placeholder="请输入 dashboard token…" data-action-keydown="token-input-key">' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="auth-dismiss">取消</button>' +
        '<button type="button" class="primary" data-action="token-save">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);
  // Guarded: a quick-ask session unmounts every overlay, so the input can be
  // gone by the time this fires — bare .focus() throws from the timer.
  setTimeout(() => { const i = document.getElementById('token-input'); if (i) i.focus(); }, 100);
}

// dismissAuthModal closes the auth prompt and starts the background-reprompt
// cooldown so the next /api/sessions poll (or WS reconnect) doesn't pop it
// straight back open. The operator can still trigger it immediately via an
// explicit send/upload.
function dismissAuthModal() {
  authModalCooldown.until = Date.now() + AUTH_MODAL_COOLDOWN_MS;
  const overlay = document.querySelector('.modal-overlay');
  if (overlay) overlay.remove();
}

// Time-divider threshold: insert a visual gap label when the interval between
// adjacent rendered events exceeds this many ms. 5 minutes matches iMessage-ish
// chat grouping — tight enough to separate turns, loose enough to not spam.
const EVENT_DIVIDER_GAP_MS = 5 * 60 * 1000;

// Avatar-grouping threshold (WeChat-style): when a bubble follows another from
// the SAME sender within this gap, its repeated avatar is hidden via the
// .nz-grouped class (see regroupAvatars). Deliberately MUCH tighter than
// EVENT_DIVIDER_GAP_MS — grouping reacts to rapid-fire turns (sub-minute),
// while the time divider marks coarse conversation breaks. The two are
// independent: a 30s gap groups avatars but emits no divider.
const AVATAR_GROUP_GAP_MS = 30 * 1000;

const EARLIER_PAGE_LIMIT = 100;

// UX3 (#398): the live-push path (appendEvents) does insertAdjacentHTML('beforeend')
// once per event with no upper bound, so a long-running session that streams
// thousands of events grows #events-scroll without limit and eventually OOMs the
// tab. The historical-load path is already paginated (INITIAL_HISTORY_LIMIT +
// "load earlier"); this cap is the live half of the same budget. We keep a
// generous tail (matching a few "load earlier" pages) and trim the oldest DOM
// nodes from the top once exceeded. Trimming only drops rendered DOM — the
// server still holds full history, and "load earlier" re-fetches if the operator
// scrolls back up. Mirrors the existing CRON_LIVE_MAX_EVENTS top-trim precedent.
const MAX_LIVE_DOM_EVENTS = 600;

// cron-live RFC §5: cron live 容器内最多保留 200 条事件。后端
// EventEntriesSince(after) 不受 50 条上限约束（After>0 时 Limit 被忽略），
// 一次 history 帧可能返回数百条事件 —— 前端必须自己截尾。超出部分计入
// truncatedCount 并用容器顶部的提示告知操作员。
const CRON_LIVE_MAX_EVENTS = 200;

// 当 cron live 收到的事件全被 INTERNAL_EVENT_TYPES 过滤光（parallel agent
// team 整段是 agent / task_* / tool_use），渲这条占位而非留空 innerHTML，
// 否则 CSS .cdl-events:empty::before 会误报"暂无事件"。.cdl-agent-only 类名
// 供 appendEventsToContainer 在追加真实事件前识别并清除占位。
const CRON_LIVE_AGENT_ONLY_HTML =
  '<div class="empty-state cdl-agent-only">本轮仅有 agent / 工具活动，正文消息请在任务结束后查看历史详情</div>';

// formatTimeShort returns a chat-style label for a divider: today -> HH:MM,
// yesterday -> "昨天 HH:MM", within a week -> "周三 HH:MM", older -> "M-D HH:MM",
// different year -> "YYYY-M-D HH:MM".
function formatTimeShort(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const now = new Date();
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const hm = hh + ':' + mm;
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  if (sameDay) return hm;
  const yesterday = new Date(now); yesterday.setDate(now.getDate() - 1);
  const isYesterday = d.getFullYear() === yesterday.getFullYear() && d.getMonth() === yesterday.getMonth() && d.getDate() === yesterday.getDate();
  if (isYesterday) return '昨天 ' + hm;
  // Local calendar-day difference (not floor(ms/24h)): an event 6d23.5h ago
  // is the same weekday as today and must not get a weekday label (#2429).
  const dayStart = x => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diffDays = Math.round((dayStart(now) - dayStart(d)) / 86400000);
  if (diffDays < 7 && diffDays >= 0) {
    const wk = ['周日','周一','周二','周三','周四','周五','周六'][d.getDay()];
    return wk + ' ' + hm;
  }
  const md = (d.getMonth() + 1) + '-' + d.getDate();
  if (d.getFullYear() !== now.getFullYear()) return d.getFullYear() + '-' + md + ' ' + hm;
  return md + ' ' + hm;
}

// formatTimeFull is a locale-ish absolute timestamp used in the event tooltip.
function formatTimeFull(ms) {
  if (!ms) return '';
  const d = new Date(ms);
  const pad = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
    pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function timeDividerHtml(ms) {
  return '<div class="event-time-divider" data-time="' + (ms || 0) + '">' + esc(formatTimeShort(ms)) + '</div>';
}

// esc / escAttr / escJs moved to nz_util.js (PR-0a, RFC
// dashboard-cron-view-extraction). They are exposed as window.nz.util.* and
// as top-level aliases (window.esc, window.escAttr, window.escJs) loaded
// before this file, so the bare call sites below keep working unchanged.
// SECURITY: the single source of truth for HTML/attr/JS escaping lives there
// — never re-define a local copy here or in any view module.

// URL schemes that are safe to embed in <a href>.
// RNEW-SEC-007: Only https?: and fragment-only URLs (#...) are accepted.
// Previously the allowlist also matched mailto:, absolute paths (/...),
// and query-only URLs (?...). Those introduced defence-in-depth gaps:
//   - mailto: can trigger unexpected behaviour in Electron/extension hosts
//     and is never present in LLM-rendered markdown anchor targets today.
//   - A single leading "/" lets any string starting with a slash pass the
//     check; if a caller ever forgot to esc() the capture first, a payload
//     like "/"+"><script>..." would reach href and bypass the scheme
//     gate. The stricter regex fails closed in that scenario.
// Internal links should be constructed against absolute /api/... paths in
// code, not routed through safeUrl.
// Anything else (javascript:, data:, vbscript:, file:, about:) -> '#'.
function safeUrl(u) {
  if (!u) return '#';
  const trimmed = String(u).trim();
  if (/^(https?:|#)/i.test(trimmed)) return trimmed;
  return '#';
}

// Reverse exactly the three entities esc() emits (&amp; &lt; &gt;) in a
// single pass. inlineMd runs esc(s) before its link passes, so a URL capture
// arrives with `&` already encoded; feeding that straight into escAttr()
// double-encodes the href. Single-pass means a literal `&amp;lt;` in the
// source decodes to `&lt;` (as authored), never to `<`.
function decodeEscEntities(s) {
  return s.replace(/&(amp|lt|gt);/g, function(_, name) {
    return name === 'amp' ? '&' : name === 'lt' ? '<' : '>';
  });
}

// formatFileSize renders a byte count as a short human label (e.g. "1.2 MB").
// Promotion checks the *rounded* value so 1048575 B renders "1.0 MB" rather
// than "1024.0 KB".
export function formatFileSize(bytes) {
  if (!bytes || bytes <= 0) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = bytes, i = 0;
  while (i < units.length - 1 && (i === 0 ? v >= 1024 : Number(v.toFixed(1)) >= 1024)) {
    v /= 1024;
    i++;
  }
  return i === 0 ? v + ' B' : v.toFixed(1) + ' ' + units[i];
}

// setActiveSessionCard flips the .active class on at most one session card,
// kept in a cached reference (activeCard.el). key===null drops selection
// altogether (used by openCronPanel / previewDiscovered clear paths). Node
// defaults to 'local' to match data-node attribute emission. A subsequent
// card with the same key but a different node counts as "different" — the
// data-key + data-node pair is the identity.
const activeCard = { el: null };
export function setActiveSessionCard(key, node) {
  const n = node || 'local';
  // Drop stale cached ref if the previous card was detached by a sidebar
  // rebuild (renderSidebar replaces list.innerHTML wholesale).
  if (activeCard.el && !activeCard.el.isConnected) activeCard.el = null;
  if (activeCard.el) activeCard.el.classList.remove('active');
  activeCard.el = null;
  if (key === null || key === undefined) return null;
  const next = document.querySelector(
    '.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]'
    + '[data-node="' + (window.CSS && CSS.escape ? CSS.escape(n) : n) + '"]'
  );
  if (next) {
    next.classList.add('active');
    activeCard.el = next;
  }
  return next;
}

export function getMsgValue(el) { return (el ? el.innerText : '').trim(); }
export function setMsgValue(el, v) { if (el) el.innerText = v; }

// stickEventsBottom forces the events pane to the last bubble and keeps it there
// across the async layout tail — lazy-loaded images, mermaid diagrams, katex
// formulas, and the "load earlier" button that inserts at the top after the
// initial scrollTop assignment all change scrollHeight after the first paint.
// Used by session-open flows where losing the bottom anchor would hide the
// newest messages (the whole point of opening the session).
export function stickEventsBottom() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  el.scrollTop = el.scrollHeight;
  requestAnimationFrame(() => {
    el.scrollTop = el.scrollHeight;
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  });
  // Re-stick after each lazy-loaded image, but only while the user hasn't
  // scrolled away from the bottom. Without this guard, a session opened
  // seconds ago whose images are still loading will yank the viewport back
  // to the bottom the moment any image finishes — even if the user has
  // since scrolled up to read history (common on mobile/slow networks).
  el.querySelectorAll('img').forEach(img => {
    if (img.complete) return;
    const restick = () => {
      if (el.scrollTop + el.clientHeight >= el.scrollHeight - 30) {
        el.scrollTop = el.scrollHeight;
      }
    };
    img.addEventListener('load', restick, { once: true });
    img.addEventListener('error', restick, { once: true });
  });
}

// Read the data-time of the last event-time-divider in the scroll container so
// incremental appenders can decide whether a new divider is needed.
export function lastDividerTime(el) {
  if (!el) return 0;
  // Walk the last few children back to find the most recent divider or bubble.
  const kids = el.children;
  for (let i = kids.length - 1; i >= 0; i--) {
    const c = kids[i];
    if (c.classList && (c.classList.contains('event') || c.classList.contains('event-time-divider'))) {
      const t = Number(c.getAttribute('data-time') || 0);
      if (t) return t;
    }
  }
  return 0;
}

export function closeHistoryPopover() {
  if (ui.activePopoverBackdrop) { ui.activePopoverBackdrop.remove(); ui.activePopoverBackdrop = null; }
  if (ui.activePopover) { ui.activePopover.remove(); ui.activePopover = null; }
}

export function stopPreviewPolling() {
  if (timers.preview) { clearInterval(timers.preview); timers.preview = null; }
  transcript.previewEventCount = 0;
  // Invalidate any in-flight previewDiscovered(): every caller of this
  // function (selectSession, the createSession paths, a newer preview) is
  // moving the operator off the discovered panel, so a preview fetch that
  // resolves afterwards must neither paint into the now-managed
  // #events-scroll nor re-arm previewTimer.
  transcript.previewGen++;
}

// mobileQuery is the phone breakpoint, created on first use: a module-scope
// matchMedia call would run at import time.
const mobileMQ = { list: null };
export function mobileQuery() { return mobileMQ.list || (mobileMQ.list = window.matchMedia('(max-width:768px)')); }
export function isMobile() { return mobileQuery().matches; }

export function mobileEnterChat() {
  if (!isMobile()) return;
  // #2431: switching sessions while already in chat view must not stack
  // another entry — replace ours so a single back press leaves chat.
  if (history.state && history.state.view === 'chat') history.replaceState({ view: 'chat' }, '');
  else history.pushState({ view: 'chat' }, '');
  document.body.classList.remove('mobile-list-view');
  document.body.classList.add('mobile-chat-view');
}

// removeSidebarCard drops a session card from the DOM without waiting for
// the next renderSidebar. The next render reconciles against the DOM, so a
// card whose removal the server refused (a failed DELETE re-fetches the list)
// comes back.
export function removeSidebarCard(key) {
  // Escape like setActiveSessionCard: discovered keys embed the node name, so
  // a `"` or `\` would otherwise make querySelector throw mid-takeover/dismiss.
  const card = document.querySelector('.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]');
  if (card) card.remove();
}

// Pending-session persistence (#cwd-fallback fix). The three pending maps
// (sessionWorkspaces/sessionNodes/sessionBackends) used to live ONLY in JS
// memory, so a page reload before the first send dropped the chosen workspace.
// The next send then carried no `workspace`, the backend never wrote a
// per-chat override (send.go gates SetWorkspace on a non-empty workspace), and
// the spawn fell through to defaultCWD = workspace root — the session landed in
// the wrong directory. We mirror the maps into localStorage so a reload (or
// even a never-sent session) rehydrates the workspace and the first send still
// carries it. This only re-hydrates state the user authored in THIS browser —
// no fuzzy cross-session guessing (the semantics #1567 deliberately removed).
export const PENDING_LS_KEY = 'pending_sessions';
const PENDING_LS_MAX = 64; // bound localStorage size — far above realistic un-sent backlog

// persistPending snapshots the in-memory pending maps to localStorage. Called
// after every mutation of the three maps. lsSet swallows quota/disabled errors.
export function persistPending() {
  const keys = Object.keys(perSession.workspaces).slice(0, PENDING_LS_MAX);
  const obj = {};
  for (const k of keys) {
    const entry = { ws: perSession.workspaces[k] };
    if (perSession.nodes[k] && perSession.nodes[k] !== 'local') entry.node = perSession.nodes[k];
    if (perSession.backends[k]) entry.backend = perSession.backends[k];
    if (perSession.accessProfiles[k]) entry.access_profile = perSession.accessProfiles[k];
    obj[k] = entry;
  }
  lsSet(PENDING_LS_KEY, obj);
}

// eagerBindWorkspace tells the backend the chosen workspace the moment a
// session is created, instead of waiting for the first send to carry it. This
// writes the per-chat override eagerly (server-side validateWorkspace +
// SetWorkspace), so even a session opened in another browser/device — or one
// reloaded before its first send — spawns into the right directory. Local
// nodes only: remote sessions resolve their workspace on their own node.
// Fire-and-forget — never blocks or fails the creation flow: localStorage /
// network errors are swallowed rather than aborting session creation.
export function eagerBindWorkspace(key, workspace, node) {
  if (!key || !workspace) return;
  const nd = node || 'local';
  if (nd !== 'local') return;
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    fetch(NZ_CONTRACT.API.sessions_bind, {
      method: 'POST', headers,
      body: JSON.stringify({ key: key, node: nd, workspace: workspace }),
    }).catch(() => {});
  } catch (_) { /* never break creation over a bind */ }
}

export function removePendingSession(key) {
  delete perSession.workspaces[key];
  delete perSession.nodes[key];
  delete perSession.backends[key];
  delete perSession.accessProfiles[key];
  persistPending();
}

// applyFeatureGates updates the input-area controls to reflect the
// active session's backend features. Called after every renderMainShell
// / selectSession / cliBackends fetch — cheap, just toggles aria + class.
// Multi-Backend RFC §8.3 D9 / D11-D15.
//
// Important: NEVER silently disable. Per RFC §8.7: "all gated controls
// must have a hover/aria tooltip explaining why" — the title attribute
// carries the operator-readable reason.
export function applyFeatureGates() {
  if (!serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return;
  if (serverInfo.cliBackends.backends.length <= 1) return; // single-backend mode

  const sess = sessionList.sessionsData[sid(selection.key, selection.node)] || {};
  const backendID = sess.backend || pendingBackendID(selection.key, selection.node) || serverInfo.cliBackends.default || '';
  const backendName = (() => {
    const e = serverInfo.cliBackends.backends.find(b => b && b.id === backendID);
    return (e && (e.display_name || e.id)) || backendID || 'this backend';
  })();

  // D14 image_input: file picker accepts both images + PDF; if image is
  // disabled but PDF still works, leave the button enabled — most kiro
  // deployments support image so this branch rarely hits in practice.
  // Audio is governed separately (D15) by the voice button.
  const imageOK = featureForBackend(backendID, 'image_input');
  const filePickBtn = document.querySelector('button[data-action="file-picker"]');
  if (filePickBtn) {
    if (!imageOK) {
      filePickBtn.classList.add('feat-disabled');
      filePickBtn.title = '当前后端 (' + backendName + ') 不支持图片上传';
      filePickBtn.setAttribute('aria-disabled', 'true');
      // Review #118 HIGH-1: rely on the native disabled property as the
      // hard gate, not just CSS — `cursor:not-allowed` is cosmetic and
      // a keyboard activation (Enter/Space on focus) would still fire
      // onclick. Browsers skip click events on disabled buttons entirely,
      // and `applyFeatureGates` is the single re-entry point so the
      // pair stays in sync.
      filePickBtn.disabled = true;
    } else {
      filePickBtn.classList.remove('feat-disabled');
      filePickBtn.title = '上传图片或 PDF';
      filePickBtn.removeAttribute('aria-disabled');
      filePickBtn.disabled = false;
    }
  }

  // D15 audio_input: kiro acp 申报 audio:false 但 naozhi 后端会先转写
  // 再喂 prompt — 所以这里**不真正 disable**，只把 tooltip 改成提示性
  // 文案，让用户知道音频会经过转写阶段。
  const audioOK = featureForBackend(backendID, 'audio_input');
  const micBtn = document.getElementById('btn-mic');
  const holdBtn = document.getElementById('btn-hold-talk');
  // Review #118 HIGH-2: when audio is supported again (e.g. user switches
  // from kiro back to claude in the same browser session), we MUST reset
  // titles to their template defaults — otherwise the kiro-era hint
  // ("会先转写为文字") sticks forever. Default titles mirror
  // renderMainShell template (line ~2152 / ~2154).
  const micDefaultTitle = composer.voiceInputMode ? '切换键盘' : '切换语音';
  const holdDefaultTitle = '按住说话改录音';
  if (!audioOK) {
    const audioHint = '当前后端 (' + backendName + ') 不直接接收音频，naozhi 会先转写为文字再发送';
    if (micBtn) {
      micBtn.classList.add('feat-degraded');
      micBtn.title = audioHint;
    }
    if (holdBtn) {
      holdBtn.classList.add('feat-degraded');
      holdBtn.title = audioHint;
    }
  } else {
    if (micBtn) {
      micBtn.classList.remove('feat-degraded');
      micBtn.title = micDefaultTitle;
    }
    if (holdBtn) {
      holdBtn.classList.remove('feat-degraded');
      holdBtn.title = holdDefaultTitle;
    }
  }
}

export {
  AVATAR_GROUP_GAP_MS,
  CRON_LIVE_AGENT_ONLY_HTML,
  CRON_LIVE_MAX_EVENTS,
  EARLIER_PAGE_LIMIT,
  EVENT_DIVIDER_GAP_MS,
  MAX_LIVE_DOM_EVENTS,
  announce,
  authModalCooldown,
  cachedCostSummary,
  confirmDialog,
  copyCodeBlock,
  copyEventContent,
  costBudgetChipHtml,
  costCardTitle,
  decodeEscEntities,
  dismissAuthModal,
  fetchCostBudget,
  fetchCostSummary,
  formatAbsTime,
  formatHomeCost,
  formatTimeFull,
  historyDayLabel,
  mainEmptyHtml,
  promptDialog,
  reconnectNow,
  refreshCostSummary,
  refreshSidebarTimes,
  renderRecentSessionsPanel,
  renderServiceOverviewHtml,
  safeUrl,
  shortPath,
  showAPIError,
  showAuthModal,
  showNetworkError,
  startSidebarTimeTick,
  stopSidebarTimeTick,
  timeAgo,
  timeDividerHtml,
};
