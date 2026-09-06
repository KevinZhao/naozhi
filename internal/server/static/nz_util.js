// nz_util.js — shared zero-dependency utility layer.
//
// RFC docs/rfc/dashboard-cron-view-extraction.md (PR-0a). These helpers were
// previously top-level functions in dashboard.js; they are pure functions /
// pure-DOM helpers with no dependency on any other dashboard.js global, so
// they form the bottom layer that every view (chat / cron / agent / asset)
// can consume.
//
// ES module (RFC docs/rfc/dashboard-es-modules.md, D3 PR-A). Loaded via
// <script type="module"> — parser-inserted modules share the deferred
// execution queue with the classic <script defer> files and run in tag order,
// so this still executes before dashboard.js (dashboard.html order is frozen
// during the migration; see the comment there).
//
// Exports: real ES exports for migrated modules, plus
//   window.nz.util.{esc, escAttr, escJs, fetchJSON, showToast, trapFocus}
// and legacy top-level aliases (window.esc, window.escAttr, …) so existing
// call sites in the not-yet-migrated classic files keep working unchanged.
// The aliases are the migration bridge; they go away in D3 PR-E.
//
// SECURITY: esc / escAttr / escJs are the single source of truth for HTML /
// attribute / JS-string escaping. Do NOT copy these into any view module —
// duplicated escapers drift and reintroduce XSS. Always reuse this layer.

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
// Escape for embedding inside a JS string literal (e.g. an inline click
// attribute of the form f('…')). Prose here deliberately avoids the literal
// `onclick` + `=` token: the CSP ratchet in dashboard_csp_test.go counts that
// token by text, so a mention in a comment would inflate this file's count
// and keep it from ever reaching 0.
export function escJs(s) {
  if (!s) return '';
  // R202606j-SEC-9 (#2344): besides the obvious string-breakers, escape
  // every C0/C1 control char (U+0000-001F, U+007F-009F) plus Unicode
  // zero-width / bidi format chars (ZWSP..RLM, LRE..RLO/PDF, WJ, isolates),
  // the line/paragraph separators U+2028/U+2029 (which are raw JS line
  // terminators and can break the literal), and the BOM U+FEFF. Such runes
  // can survive a filesystem path name and ride into the inline onclick
  // attribute; mapping each to its \\uXXXX escape keeps the emitted literal
  // inert and prevents visual path spoofing. Printables (CJK, emoji,
  // accented latin) pass through unchanged.
  let out = String(s)
    .replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '\\"')
    .replace(/\n/g, '\\n').replace(/\r/g, '\\r')
    .replace(/</g, '\\u003c').replace(/>/g, '\\u003e');
  return out.replace(/[\s\S]/g, ch => {
    const c = ch.charCodeAt(0);
    const dangerous =
      (c <= 0x1f) ||
      (c >= 0x7f && c <= 0x9f) ||
      (c >= 0x200b && c <= 0x200f) ||
      (c >= 0x202a && c <= 0x202e) ||
      c === 0x2060 ||
      (c >= 0x2066 && c <= 0x2069) ||
      c === 0x2028 || c === 0x2029 ||
      c === 0xfeff;
    return dangerous ? '\\u' + c.toString(16).padStart(4, '0') : ch;
  });
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
nz.util = { esc, escAttr, escJs, fetchJSON, showToast, trapFocus };

// Cross-module utility formatters/predicates (moved verbatim from cron_view,
// #2557 PR-E1 — both dashboard and cron consume them, and hosting them here
// keeps the module graph acyclic: dashboard must never import a view).

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

// nz.bus (#2557 PR-E1): the cross-module notification channel. dashboard's
// WS core dispatches cron-view commands here instead of calling cron
// functions through the window bridge — the reverse dashboard→view edge
// must not become an import (a dashboard→cron import would invert module
// execution order and break cron's load-time init). dispatchEvent is
// synchronous, so ordering matches the old direct calls.
export const nzBus = new EventTarget();
nz.bus = nzBus;

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

// Cross-file mutable state accessors (D3 RFC §3): dashboard.js — still a
// classic script — registers getters onto this object for its reassignable
// top-level bindings (a classic script's let/const never lands on window,
// and a copied value would go stale on reassignment). Migrated modules
// import { nzState } and read nzState.<name> live at the use site. (Named
// nzState, not state, so it never collides with the view modules' local
// `state` objects.)
export const nzState = {};
nz.state = nzState;

// Legacy top-level aliases — migration bridge for the classic-script call
// sites (dashboard.js / cron_view.js / agent_view.js) that reference the bare
// global names. This module runs before them (tag order), so the aliases
// exist by the time they execute. Removed in D3 PR-E.
Object.assign(window, { esc, escAttr, escJs, fetchJSON, showToast, trapFocus });
