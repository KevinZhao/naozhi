// utilities.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureUtilities(), called from dashboard's module body.
import { esc, escAttr, nzState, showToast, trapFocus } from './nz_util.js';

const deps = {
  allSessionsCache: null,
  cliBackends: null,
  getToken: null,
  lastStatsSnapshot: null,
  renderSystemView: null,
  wsm: null,
};
export function configureUtilities(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('utilities dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// Late-bound hooks: assigned by the code below, read by other modules at event
// time (never at load time) — the shape they had as dashboard module-scope
// lets before this extraction.
let costSummaryCache = null;

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

// computeHomeStats aggregates deps.allSessionsCache into the two stats surfaced
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
  if (deps.cliBackends && Array.isArray(deps.cliBackends.backends) && deps.cliBackends.backends.length > 1) {
    const okCount = deps.cliBackends.backends.filter(b => b && b.available).length;
    const totalCount = deps.cliBackends.backends.length;
    const ids = deps.cliBackends.backends.map(b => (b && b.id) || '?').join(' · ');
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
// empty-state body. Reads deps.allSessionsCache (written by renderSidebar after
// each fetchSessions → so reflects the same authoritative snapshot the
// sidebar shows), picks the 5 most recently active sessions, and renders a
// compact clickable list. When there are zero sessions, returns an empty
// innerHTML so the cold-start minimal CTA stays unchanged. Callers must
// guard by nzState.selectedKey == null (active-session main shell wins).
//
// ui-polish-light-theme D3: the R110-P1 stats strip / health strip / doctor
// panel used to render here too — version tags and subprocess counts are ops
// info, not "ask something" material, and they buried the one clickable
// affordance. They now live in the 系统 view (renderServiceOverviewHtml) and
// the settings 关于 section; Home keeps only the recent-session list.
//
// Pure-rendering: writes to the DOM by id rather than returning HTML, because
// the cold-start HTML already carries the placeholder div and we don't want
// to fight the order of initial paint.
function renderRecentSessionsPanel() {
  const host = document.getElementById('recent-sessions-panel');
  if (!host) return;
  if (nzState.selectedKey) return; // active session rendered by renderMainShell
  const items = Array.isArray(deps.allSessionsCache) ? deps.allSessionsCache : [];
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
    lines.push({ text: '成本账本含 ' + c.partial + ' 个进程中断的轮次（只记 token，未计价）', kind: 'info' });
  }
  return lines;
}

// costCardTitle explains what the ledger figure is (and is not): a CLI
// estimate at list/contract price, never an invoice.
function costCardTitle(c) {
  let t = '近 30 天账本合计（会话 + cron + 云沙箱），CLI 估算口径，非账单';
  if (c.unknown > 0) t += '；含 ' + c.unknown + ' 条未知定价（模型不在 CLI 价表，按默认模型估算）';
  if (c.dropped > 0) t += '；账本曾丢弃 ' + c.dropped + ' 条，金额可能偏低';
  return t;
}

// refreshCostSummary pulls the ledger's last-30-day totals at most once per
// 30 s (attempts, not successes, so a failing endpoint is not hammered on
// every repaint), one fetch in flight at a time, and repaints the system
// view when it is showing. Failures keep the previous snapshot (or the
// session-sum fallback) — never blank the card.
const COST_SUMMARY_TTL_MS = 30 * 1000;
let costSummaryLastAttempt = 0;
let costSummaryInFlight = false;
async function refreshCostSummary() {
  const now = Date.now();
  if (costSummaryInFlight || (now - costSummaryLastAttempt) < COST_SUMMARY_TTL_MS) return;
  costSummaryLastAttempt = now;
  costSummaryInFlight = true;
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const to = new Date();
    const from = new Date(to.getTime() - 30 * 24 * 3600 * 1000);
    const resp = await fetch(NZ_CONTRACT.API.cost_summary + '?group_by=unit&from=' + encodeURIComponent(from.toISOString()) +
      '&to=' + encodeURIComponent(to.toISOString()), { headers });
    if (!resp.ok) return;
    costSummaryCache = summarizeCostBuckets(await resp.json());
    if (nzState.activeView === 'system') deps.renderSystemView();
  } catch (_) {
  } finally {
    costSummaryInFlight = false;
  }
}

// renderServiceOverviewHtml builds the 服务概览 section for the 系统 view:
// the aggregate stats (today active / prompts / cost), the health strip
// derived from the /api/sessions stats snapshot, and the multi-backend
// doctor panel. Moved here from the Home panel (ui-polish-light-theme D3)
// — the pure helpers stayed put, only the call site and CSS classes
// (svc-*) changed. Version identity lines also render in the settings
// 关于 section (renderSettingsView) for discoverability.
// costStatHtml renders the 花费 card: ledger figure when loaded (with a
// credits sub-line for kiro sessions and an honest hover explanation),
// otherwise the legacy live-session sum labelled as such.
function costStatHtml(stats) {
  const c = costSummaryCache;
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

function renderServiceOverviewHtml() {
  const items = Array.isArray(deps.allSessionsCache) ? deps.allSessionsCache : [];
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
  const healthLines = buildHomeHealthLines(deps.lastStatsSnapshot);
  for (const l of buildCostHealthLines(costSummaryCache)) healthLines.push(l);
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
// RFC §8.3 D22. Returns '' for single-backend deployments / when deps.cliBackends
// is unavailable so the cold-start home page doesn't show a half-empty
// section. The output includes a small "▼" affordance and a screen-reader
// label so keyboard users know the section is expandable.
function renderBackendsDoctorPanel() {
  if (!deps.cliBackends || !Array.isArray(deps.cliBackends.backends)) return '';
  if (deps.cliBackends.backends.length <= 1) return '';
  const rows = deps.cliBackends.backends.map(b => {
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
  const defaultID = esc(deps.cliBackends.default || '');
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

// localizeAPIError turns an HTTP status code + raw server message into a
// user-facing Chinese string. Classifies by status class so operators get
// a consistent mental model — 4xx = "你这边要改", 5xx = "服务端问题，请
// 稍后重试". The raw tail is appended (truncated to 120 chars) so diagnostic
// signal isn't lost, but the Chinese prefix is always there for screen-readers
// and non-technical operators.
//
// Why not a full i18n dict: current project is single-locale (zh-CN); a
// full go-i18n pipeline was floated in UX review but rejected as overkill
// — UX1 target is "no raw English errors", not "pluggable locales".
function localizeAPIError(status, raw) {
  const tail = (raw || '').toString().trim().slice(0, 120);
  const withTail = tail ? '（' + tail + '）' : '';
  if (status === 0 || status === undefined || status === null) {
    return '网络错误' + withTail;
  }
  if (status === 401) {
    return '鉴权失败，请重新登录' + withTail;
  }
  // work_dir 专项：当后端返回 classifyWorkspaceErr 标签时把通用文案换成更
  // 精确的中文，避免 "无权限或参数越界" 把 "不存在 / 不是目录 / 越界"
  // 三种含义不同的失败合并成一句话，操作员看到无法自助修复。
  // 与 internal/server/server.go classifyWorkspaceErr 输出保持一致。
  if (raw) {
    const r = String(raw);
    if (r.indexOf('work_dir outside allowed root') !== -1) {
      return '工作目录不在允许范围内（' + r.slice(0, 120) + '）';
    }
    if (r.indexOf('work_dir does not exist') !== -1) {
      return '工作目录不存在' + withTail;
    }
    if (r.indexOf('work_dir is not a directory') !== -1) {
      return '路径不是目录' + withTail;
    }
    if (r.indexOf('work_dir is not a valid path') !== -1) {
      return '工作目录路径不合法' + withTail;
    }
    if (r.indexOf('work_dir must be an absolute path') !== -1) {
      return '工作目录必须是绝对路径' + withTail;
    }
  }
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
// deps.wsm.connect short-circuits when the socket is already OPEN/CONNECTING.
function reconnectNow() {
  if (deps.wsm.reconnectTimer) {
    clearTimeout(deps.wsm.reconnectTimer);
    deps.wsm.reconnectTimer = null;
  }
  deps.wsm.backoff = 1000;
  // No toast: the sidebar status row already flips to "connecting..." when
  // deps.wsm.connect() sets CONNECTING, and the outage/reconnect button update
  // through updateStatusBar. A toast here was redundant with that signal.
  deps.wsm.connect();
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

// Sidebar relative-time ticker. While WS is connected renderSidebar only
// runs on sessions_update (the 5s /api/sessions poll short-circuits on an
// unchanged version), so a card's "2m ago" label froze at whatever the last
// render produced. Rather than force a full sidebar rebuild every minute,
// recompute just the .sc-time text from the data-ts stamp renderSessionCard
// emits. Paused while the tab is hidden via the visibilitychange gate below
// (same as the other pollers) — stale text on a hidden tab costs nothing.
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

// INITIAL_HISTORY_LIMIT caps how many events the server sends on a fresh
// subscribe / first fetch. Keeps big sessions snappy on first paint; older
// pages load lazily via the "load earlier" button. Server caps at 500
// regardless (maxEventsPageLimit) so 100-500 is the effective window.
const INITIAL_HISTORY_LIMIT = 100;
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



export {
  AVATAR_GROUP_GAP_MS,
  CRON_LIVE_AGENT_ONLY_HTML,
  CRON_LIVE_MAX_EVENTS,
  EARLIER_PAGE_LIMIT,
  EVENT_DIVIDER_GAP_MS,
  INITIAL_HISTORY_LIMIT,
  MAX_LIVE_DOM_EVENTS,
  announce,
  confirmDialog,
  copyCodeBlock,
  copyEventContent,
  costSummaryCache,
  decodeEscEntities,
  formatAbsTime,
  formatTimeFull,
  historyDayLabel,
  mainEmptyHtml,
  promptDialog,
  reconnectNow,
  refreshCostSummary,
  renderRecentSessionsPanel,
  renderServiceOverviewHtml,
  safeUrl,
  shortPath,
  showAPIError,
  showNetworkError,
  startSidebarTimeTick,
  stopSidebarTimeTick,
  timeAgo,
  timeDividerHtml,
};
