// nz_util.js — shared zero-dependency utility layer.
//
// RFC docs/rfc/dashboard-cron-view-extraction.md (PR-0a). These helpers were
// previously top-level functions in dashboard.js; they are pure functions /
// pure-DOM helpers with no dependency on any other dashboard.js global, so
// they form the bottom layer that every view (chat / cron / agent / asset)
// can consume.
//
// Loaded BEFORE dashboard.js via <script defer> ordering (dashboard.html), so
// the namespace + legacy top-level aliases are ready before dashboard.js runs.
//
// Exports:
//   window.nz.util.{esc, escAttr, escJs, fetchJSON, showToast, trapFocus}
// Plus legacy top-level aliases (window.esc, window.escAttr, …) so existing
// call sites in dashboard.js / agent_view.js keep working unchanged. The
// aliases are the migration bridge; new code should prefer window.nz.util.*.
//
// SECURITY: esc / escAttr / escJs are the single source of truth for HTML /
// attribute / JS-string escaping. Do NOT copy these into any view module —
// duplicated escapers drift and reintroduce XSS. Always reuse this layer.
(function () {
  'use strict';

  // esc() escapes the three structural HTML characters only. We deliberately
  // do NOT escape quote characters here: escAttr (below) layers quote-escaping
  // on top, and many call sites chain a further quote-escape, so adding " / '
  // here would change observable behaviour at every esc() call site.
  const _escAmpRe = /&/g;
  const _escLtRe = /</g;
  const _escGtRe = />/g;
  function esc(s) {
    if (!s) return '';
    return String(s)
      .replace(_escAmpRe, '&amp;')
      .replace(_escLtRe, '&lt;')
      .replace(_escGtRe, '&gt;');
  }
  // Escape for HTML attribute context. We don't know whether the caller used
  // single- or double-quoted attributes, so we escape both to be safe.
  function escAttr(s) {
    return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  // Escape for embedding inside a JS string literal (e.g. inline onclick="f('…')").
  function escJs(s) {
    if (!s) return '';
    return String(s).replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '\\"').replace(/\n/g, '\\n').replace(/\r/g, '\\r').replace(/</g, '\\u003c').replace(/>/g, '\\u003e');
  }

  // fetchJSON wraps fetch() with a hard timeout (default 10s) so spinners and
  // error paths fire deterministically. Returns parsed JSON on 2xx, throws
  // with the response body (and .status) on non-2xx.
  async function fetchJSON(url, opts = {}) {
    const { timeoutMs = 10000, signal: parentSignal, ...rest } = opts;
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
      const text = await r.text();
      if (!r.ok) { const err = new Error('HTTP ' + r.status + ': ' + text.slice(0, 500)); err.status = r.status; throw err; }
      return text ? JSON.parse(text) : null;
    } catch (e) {
      clearTimeout(timer);
      if (e.name === 'AbortError') throw new Error('fetch timed out after ' + timeoutMs + 'ms: ' + url);
      throw e;
    }
  }

  function showToast(msg, type, duration) {
    const el = document.getElementById('toast');
    el.textContent = msg;
    el.className = 'toast show' + (type ? ' ' + type : '');
    clearTimeout(el._tid);
    el._tid = setTimeout(() => { el.className = 'toast'; }, duration || 3000);
  }

  /* Focus trap: confine Tab within an overlay, restore focus on dismissal.
     Called after an overlay is appended to the DOM. Returns nothing — the
     overlay's MutationObserver tears down listeners when it's removed. */
  function trapFocus(overlay) {
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

  // Legacy top-level aliases — migration bridge for existing dashboard.js /
  // agent_view.js call sites that reference the bare global names. Defined
  // here (before dashboard.js loads) so those sites keep working unchanged.
  window.esc = esc;
  window.escAttr = escAttr;
  window.escJs = escJs;
  window.fetchJSON = fetchJSON;
  window.showToast = showToast;
  window.trapFocus = trapFocus;
})();
