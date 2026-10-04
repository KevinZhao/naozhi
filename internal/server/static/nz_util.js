// nz_util.js — the shared utility layer every view (chat / cron / agent /
// asset) consumes: pure functions and pure-DOM helpers. Its only import is
// contract.js, so it stays a leaf of the module graph.
//
// Exports: real ES exports for the view modules, plus the single
// window.nz namespace (util / state / actions / bus / views / test).
//
// SECURITY: esc / escAttr are the single source of truth for HTML /
// attribute escaping. Do NOT copy these into any view module — duplicated
// escapers drift and reintroduce XSS. Always reuse this layer. (escJs was
// deleted with the last JS-string-literal sink, #1980 — inline handlers are
// gone; do not resurrect it without resurrecting the review that guarded it.)

import { NZ_CONTRACT } from './contract.js';

// esc() escapes the three structural HTML characters only. We deliberately
// do NOT escape quote characters here: escAttr (below) layers quote-escaping
// on top, and many call sites chain a further quote-escape, so adding " / '
// here would change observable behaviour at every esc() call site.
const _escAmpRe = /&/g;
const _escLtRe = /</g;
const _escGtRe = />/g;
export function esc(s) {
  if (!s) return '';
  return String(s)
    .replace(_escAmpRe, '&amp;')
    .replace(_escLtRe, '&lt;')
    .replace(_escGtRe, '&gt;');
}
// Escape for HTML attribute context. We don't know whether the caller used
// single- or double-quoted attributes, so we escape both to be safe.
export function escAttr(s) {
  return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// fetchJSON wraps fetch() with a hard timeout (default 10s) so spinners and
// error paths fire deterministically. Returns parsed JSON on 2xx, throws
// with the response body (and .status) on non-2xx.
export async function fetchJSON(url, opts = {}) {
  const { timeoutMs = 10000, signal: parentSignal, onResponse, ...rest } = opts;
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(new Error('timeout')), timeoutMs);
  // Chain caller-provided signal so e.g. component-unmount can abort too.
  if (parentSignal) {
    if (parentSignal.aborted) { clearTimeout(timer); ctrl.abort(parentSignal.reason); }
    else parentSignal.addEventListener('abort', () => ctrl.abort(parentSignal.reason), { once: true });
  }
  try {
    const r = await fetch(url, { ...rest, signal: ctrl.signal });
    clearTimeout(timer);
    // onResponse lets callers inspect response headers/status before the
    // body is parsed (the parsed JSON discards them). Invoked only on a
    // successful fetch; guarded so a caller bug can't mask the response.
    if (typeof onResponse === 'function') {
      try { onResponse(r); } catch (_) { /* caller hook must not break the fetch */ }
    }
    const text = await r.text();
    if (!r.ok) { const err = new Error('HTTP ' + r.status + ': ' + text.slice(0, 500)); err.status = r.status; throw err; }
    return text ? JSON.parse(text) : null;
  } catch (e) {
    clearTimeout(timer);
    if (e.name === 'AbortError') throw new Error('fetch timed out after ' + timeoutMs + 'ms: ' + url);
    throw e;
  }
}

export function showToast(msg, type, duration) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast show' + (type ? ' ' + type : '');
  clearTimeout(el._tid);
  el._tid = setTimeout(() => { el.className = 'toast'; }, duration || 3000);
}

/* Focus trap: confine Tab within an overlay, restore focus on dismissal.
   Called after an overlay is appended to the DOM. Returns nothing — the
   overlay's MutationObserver tears down listeners when it's removed. */
export function trapFocus(overlay) {
  if (!overlay || overlay._trapped) return;
  overlay._trapped = true;
  const prevActive = document.activeElement;
  const FOCUSABLE = 'button, [href], input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"]), [contenteditable="true"]';
  const onKey = (e) => {
    if (e.key === 'Escape') {
      // Let inner handlers pre-empt; otherwise dismiss the overlay.
      if (!e.defaultPrevented) { overlay.remove(); }
      return;
    }
    if (e.key !== 'Tab') return;
    const nodes = [...overlay.querySelectorAll(FOCUSABLE)].filter(el => !el.disabled && el.offsetParent !== null);
    if (nodes.length === 0) { e.preventDefault(); return; }
    const first = nodes[0], last = nodes[nodes.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  overlay.addEventListener('keydown', onKey);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(overlay)) {
      overlay.removeEventListener('keydown', onKey);
      obs.disconnect();
      if (prevActive && prevActive.focus) { try { prevActive.focus(); } catch (_) {} }
    }
  });
  obs.observe(document.body, { childList: true, subtree: false });
}

// Single root namespace (RFC §2.5.4): window.nz.{util,render,core,views}.
const nz = (window.nz = window.nz || {});
nz.util = { esc, escAttr, fetchJSON, showToast, trapFocus };

// Formatters / predicates dashboard and cron both use; hosting them here
// keeps the module graph acyclic (dashboard must never import a view).

// formatCostUSD renders a per-run cost. Sub-cent runs show 4 decimals so a
// $0.0044 run is not rounded to $0.00; larger runs show cents.
export function formatCostUSD(usd) {
  if (!usd || usd <= 0) return '';
  if (usd < 0.01) return '$' + usd.toFixed(4);
  return '$' + usd.toFixed(2);
}
// formatDurationShort renders a millisecond duration as a compact human
// string suitable for KPI tiles: "850ms" / "12s" / "3m 15s" / "1h 02m".
// Stays under ~8 chars so the .ck-value column doesn't overflow at narrow
// drawer widths.
export function formatDurationShort(ms) {
  if (!ms || ms <= 0) return '—';
  const s = ms / 1000;
  if (s < 1) return Math.round(ms) + 'ms';
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's';
  const m = Math.floor(s / 60);
  const rs = Math.round(s - m * 60);
  if (m < 60) return m + 'm ' + (rs < 10 ? '0' + rs : rs) + 's';
  const h = Math.floor(m / 60);
  const rm = m - h * 60;
  return h + 'h ' + (rm < 10 ? '0' + rm : rm) + 'm';
}
// formatBytes renders a byte count as a compact human size (KiB/MiB/GiB);
// 0 / missing returns ''. Integer-ish display — these are coarse signals.
export function formatBytes(n) {
  if (!n || n < 0) return '';
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + ' GiB';
  if (n >= 1 << 20) return Math.round(n / (1 << 20)) + ' MiB';
  if (n >= 1 << 10) return Math.round(n / (1 << 10)) + ' KiB';
  return n + ' B';
}
// formatRunDuration —— 时间轴行 / 详情区"耗时"文案。>1000ms 用 "Xs"，否则 "Xms"。
// 0 / 缺省返回 ''——running 状态没 duration_ms，调用方应传 0 跳过渲染。
export function formatRunDuration(ms) {
  if (!ms || ms <= 0) return '';
  if (ms < 1000) return ms + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1).replace(/\.0$/, '') + 's';
  const m = Math.floor(s / 60);
  const ss = Math.round(s - m * 60);
  return m + 'm ' + ss + 's';
}
// isCronSessionKey — cron-scheduler session keys carry the cron: prefix;
// dashboard's session-dismiss safety check and cron routing both test it.
export function isCronSessionKey(key) {
  return typeof key === 'string' && key.indexOf('cron:') === 0;
}

// nz.bus (#2557 PR-E1): the cross-module notification channel. dashboard
// dispatches cron-view commands here (WS frames reach the cron modules
// through their own wsm.on registrations) — the reverse dashboard→view edge
// must not become an import (a dashboard→cron import would invert module
// execution order and break cron's load-time init). dispatchEvent is
// synchronous, so ordering matches the old direct calls.
export const nzBus = new EventTarget();
nz.bus = nzBus;

// nz.views (#2557 PR-E3): view-module entry points. dashboard cannot import
// the views (they import dashboard — an import back would create a cycle and
// invert execution order), so each view registers its public surface here at
// module init and dashboard reaches it at call time.
export const nzViews = {};
nz.views = nzViews;

// runStateDot / runStateLabel —— 统一 run 词表（runtelemetry.RunState）的
// 单一状态色 / 中文文案表（#2540）。cron 时间轴、session 运行记录面板共用：
// 三种 run 历史在 wire 上说同一种 state 之后，渲染侧若各自维护词表，新增
// 一个 state 就要改多处且必然漂移。RFC §8.2 配色：
//   succeeded 绿 / failed 红 / skipped 灰 / timed_out 橙 / canceled 紫 / running 蓝脉动
export function runStateDot(state) {
  switch (state) {
    case 'succeeded': return 'ok';
    case 'failed': return 'err';
    case 'skipped': return 'skip';
    case 'timed_out': return 'warn';
    case 'canceled': return 'cancel';
    case 'running': return 'run';
    default: return 'unk';
  }
}
export function runStateLabel(state) {
  switch (state) {
    case 'succeeded': return '成功';
    case 'failed': return '失败';
    case 'skipped': return '跳过';
    case 'timed_out': return '超时';
    case 'canceled': return '已取消';
    case 'running': return '运行中';
    default: return state || '未知';
  }
}

// DEATH_REASONS translates death_reason (cli DeathReason*, session's reclaim
// reasons); crashed: the process ended on its own, not reclaimed on purpose.
const DEATH_REASONS = {
  idle_timeout: { crashed: false, text: '空闲超时，进程已回收' },
  evicted: { crashed: false, text: '为腾出容量，进程已回收' },
  released: { crashed: false, text: '本次执行结束，进程已释放' },
  cli_exited: { crashed: true, text: 'CLI 进程退出' },
  shim_eof: { crashed: true, text: '与 CLI 的连接断开' },
  shim_read_error: { crashed: true, text: '读取 CLI 输出出错' },
  shim_oversize_then_eof: { crashed: true, text: 'CLI 输出超长后连接断开' },
  shim_oversize_then_read_error: { crashed: true, text: 'CLI 输出超长后读取出错' },
  readloop_panic: { crashed: true, text: '读取循环崩溃' },
  killed: { crashed: true, text: '进程被终止' },
  no_output_timeout: { crashed: true, text: '长时间无输出，进程已终止' },
  total_timeout: { crashed: true, text: '超过单轮时长上限，进程已终止' },
};

// sessionExit describes why a session has no process, or null when it is not
// dead. Only state==='dead' counts: a timeout can leave death_reason on a
// session whose process is still alive. detail (death_detail) ends the title.
export function sessionExit(state, reason, detail) {
  if (state !== 'dead') return null;
  const info = deathReasonInfo(reason);
  return { crashed: info.crashed, text: info.text, title: info.text + '，下次发送时自动恢复' + (detail ? '\n' + detail : '') };
}

// deathReasonInfo is DEATH_REASONS' entry for reason, else the wording for a
// cli_exited reason that carries an exit code (-1: a signal killed the CLI)
// or a signal name, else an abnormal exit naming the raw value.
const { CODE: EXIT_CODE, SIGNAL: EXIT_SIGNAL } = NZ_CONTRACT.DEATH_REASON_PREFIX;
function deathReasonInfo(reason) {
  if (Object.prototype.hasOwnProperty.call(DEATH_REASONS, reason)) return DEATH_REASONS[reason];
  const r = String(reason || ''), code = r.startsWith(EXIT_CODE) ? r.slice(EXIT_CODE.length) : '';
  if (/^-?\d+$/.test(code)) return { crashed: true, text: code === '-1' ? 'CLI 进程被信号终止' : 'CLI 进程异常退出（退出码 ' + code + '）' };
  if (r.startsWith(EXIT_SIGNAL) && r.length > EXIT_SIGNAL.length) return { crashed: true, text: 'CLI 进程被信号 ' + r.slice(EXIT_SIGNAL.length) + ' 终止' };
  return { crashed: true, text: reason ? '进程已退出（' + reason + '）' : '进程已退出' };
}

// sessionExitChipHtml is the chip sidebar cards and the session header show
// for a dead session: a warning for an abnormal exit, a muted note for a
// reclaim. '' when the session is not dead.
export function sessionExitChipHtml(state, reason, detail) {
  const x = sessionExit(state, reason, detail);
  if (!x) return '';
  const cls = x.crashed ? 'sc-exit sc-exit-crashed' : 'sc-exit sc-exit-reclaimed';
  const label = x.crashed ? '⚠ 异常退出' : '已回收';
  return '<span class="' + cls + '" title="' + escAttr(x.title) + '">' + label + '</span>';
}

// patchCardExitChip brings a rendered session card's exit chip in line with
// state / reason, for the paths that patch a card in place between renders;
// it produces the markup sessionCardHtml renders, right after the state text.
export function patchCardExitChip(card, state, reason, detail) {
  const meta = card && card.querySelector('.sc-meta');
  if (!meta) return;
  const old = meta.querySelector('.sc-exit');
  if (old) old.remove();
  const html = sessionExitChipHtml(state, reason, detail);
  if (!html) return;
  const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
  if (stateSpan) stateSpan.insertAdjacentHTML('afterend', html);
}

// data-nz-bg: the only styling JS computes per element is a node /
// access-profile colour, which no class can express. The renderers put the value
// in a data attribute and this observer applies it via CSSOM — a property
// assignment, not an inline attribute — so style-src needs no 'unsafe-inline'.
// Emitting style="" here would put it back.
function applyDataBg(root) {
  const els = root.querySelectorAll ? root.querySelectorAll('[data-nz-bg]') : [];
  for (const el of els) {
    const v = el.dataset.nzBg;
    if (v && el.style.background !== v) el.style.background = v;
  }
}
// reconcileChildren makes parent's element children match the markup in html,
// keyed by keyOf(el) (null = unkeyed, matched by position only). A child whose
// key and content match an existing child keeps the existing node, so focus,
// hover state and cached references survive; a changed child is replaced and
// stale children are removed. The comparison is against the DOM as it stands:
// a node patched in place compares by its current state, so there is no cache
// to drift out of step. Returns whether anything changed.
export function reconcileChildren(parent, html, keyOf) {
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  // The live nodes carry the CSSOM background data-nz-bg resolves to; give the
  // incoming ones the same so an unchanged card compares equal.
  applyDataBg(tpl.content);
  const byKey = new Map();
  for (const c of parent.children) {
    const k = keyOf(c);
    if (k !== null && !byKey.has(k)) byKey.set(k, c);
  }
  let changed = false;
  let cursor = parent.firstElementChild;
  for (const n of [...tpl.content.children]) {
    const k = keyOf(n);
    let old = k !== null ? byKey.get(k) : (cursor && keyOf(cursor) === null ? cursor : null);
    if (k !== null) byKey.delete(k);
    if (old && !old.isEqualNode(n)) {
      if (old === cursor) cursor = cursor.nextElementSibling;
      old.remove();
      old = null;
    }
    const keep = old || n;
    if (!old) changed = true;
    if (keep === cursor) {
      cursor = cursor.nextElementSibling;
    } else {
      parent.insertBefore(keep, cursor);
      if (old) changed = true;
    }
  }
  while (cursor) {
    const next = cursor.nextElementSibling;
    cursor.remove();
    cursor = next;
    changed = true;
  }
  return changed;
}

new MutationObserver((records) => {
  for (const r of records) {
    for (const n of r.addedNodes) {
      if (n.nodeType !== 1) continue;
      if (n.hasAttribute('data-nz-bg')) {
        const v = n.dataset.nzBg;
        if (v) n.style.background = v;
      }
      applyDataBg(n);
    }
  }
}).observe(document.documentElement, { childList: true, subtree: true });
document.addEventListener('DOMContentLoaded', () => applyDataBg(document));

// data-action delegation (#1980, docs/rfc/csp-data-action.md): one registry,
// one document-level dispatcher per event type, so generated HTML carries
// `data-action="key"` attributes instead of inline on*="…" handlers (the
// blocker for dropping CSP script-src 'unsafe-inline').
//
// Registry has no prototype and the dispatcher type-checks the entry, so an
// attribute-injected "__proto__" / "constructor" can never reach a callable
// gadget. Keys are code literals by contract (never interpolate data into
// data-action; parameters ride sibling data-* attributes, escAttr'd and
// always double-quoted).
export const nzActions = Object.create(null); // key → fn(el, event)
nz.actions = nzActions;
export function registerActions(map) {
  for (const k of Object.keys(map)) {
    if (!/^[a-z][a-z0-9-]*$/.test(k)) throw new Error('bad action key: ' + k);
    if (k in nzActions) throw new Error('duplicate action key: ' + k);
    if (typeof map[k] !== 'function') throw new Error('action not a function: ' + k);
    nzActions[k] = map[k];
  }
}
// Bubble phase on purpose: a capture listener at document would run before
// every existing stopPropagation shield (e.g. the #session-list long-press
// click swallower) and double-dispatch. error/load are NOT delegated — the
// codebase only ever assigns them as element properties (CSP-legal).
const DELEGATED = ['click', 'keydown', 'change', 'input', 'compositionend',
  'dragstart', 'dragover', 'dragleave', 'drop', 'dragend'];
for (const type of DELEGATED) {
  document.addEventListener(type, (e) => {
    const attr = type === 'click' ? 'data-action' : 'data-action-' + type;
    const el = e.target instanceof Element ? e.target.closest('[' + attr + ']') : null;
    if (!el) return;
    // The registry has a null prototype and registerActions only accepts
    // functions, so any truthy lookup here is a registered handler — an
    // attribute-injected "__proto__"/"constructor" resolves to undefined.
    const fn = nzActions[el.getAttribute(attr)];
    if (fn) fn(el, e);
  });
}

