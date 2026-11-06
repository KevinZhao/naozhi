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

// typingOutside reports whether a key event comes from an input, textarea
// or editable element outside panel: a non-modal panel's Esc leaves such a
// key to that element (the chat input's Esc arms an interrupt).
export function typingOutside(e, panel) {
  const t = e.target;
  if (!t || (panel && panel.contains(t))) return false;
  const tag = (t.tagName || '').toLowerCase();
  return tag === 'input' || tag === 'textarea' || !!t.isContentEditable;
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
// Stays under ~8 chars. The unit is picked after rounding: 999.6ms is 1.0s,
// 9.96s is 10s and 59.6s is 1m 00s, not 1000ms / 10.0s / 60s.
export function formatDurationShort(ms) {
  if (!ms || ms <= 0) return '—';
  if (Math.round(ms) < 1000) return Math.round(ms) + 'ms';
  const tenths = Math.round(ms / 100);
  if (tenths < 100) return (tenths / 10).toFixed(1) + 's';
  const t = Math.round(ms / 1000), m = Math.floor(t / 60); // round first: 119.6s is 2m 00s
  if (m === 0) return t + 's';
  const rs = t - m * 60;
  if (m < 60) return m + 'm ' + (rs < 10 ? '0' + rs : rs) + 's';
  const h = Math.floor(m / 60);
  const rm = m - h * 60;
  return h + 'h ' + (rm < 10 ? '0' + rm : rm) + 'm';
}
// formatBytes renders a byte count as a compact human size (KiB/MiB/GiB);
// 0 / missing returns ''. Integer-ish display — these are coarse signals.
// A count that rounds to 1024 of a unit moves up one: 1048575 is 1 MiB.
export function formatBytes(n) {
  if (!n || n < 0) return '';
  if (Math.round(n / (1 << 20)) >= 1024) return (n / (1 << 30)).toFixed(1) + ' GiB';
  if (Math.round(n / (1 << 10)) >= 1024) return Math.round(n / (1 << 20)) + ' MiB';
  if (n >= 1024) return Math.round(n / (1 << 10)) + ' KiB';
  return n + ' B';
}
// formatRunDuration —— 时间轴行 / 详情区"耗时"文案。>1000ms 用 "Xs"，否则 "Xms"。
// 0 / 缺省返回 ''——running 状态没 duration_ms，调用方应传 0 跳过渲染。
// 和 formatDurationShort 一样按取整后的值选单位：59960ms 是 "1m 0s" 而非 "60s"。
export function formatRunDuration(ms) {
  if (!ms || ms <= 0) return '';
  if (Math.round(ms) < 1000) return Math.round(ms) + 'ms';
  const tenths = Math.round(ms / 100);
  if (tenths < 600) return (tenths / 10).toFixed(1).replace(/\.0$/, '') + 's';
  const t = Math.round(ms / 1000), m = Math.floor(t / 60);
  const ss = t - m * 60;
  return m + 'm ' + ss + 's';
}
// isCronSessionKey — cron-scheduler session keys carry the cron: prefix;
// dashboard's session-dismiss safety check and cron routing both test it.
export function isCronSessionKey(key) {
  return typeof key === 'string' && key.indexOf('cron:') === 0;
}

// nz.bus: the cross-module notification channel. dashboard
// dispatches cron-view commands here (WS frames reach the cron modules
// through their own wsm.on registrations) — the reverse dashboard→view edge
// must not become an import (a dashboard→cron import would invert module
// execution order and break cron's load-time init). dispatchEvent is synchronous.
export const nzBus = new EventTarget();
nz.bus = nzBus;

// nz.views: view-module entry points. dashboard cannot import
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
  stopped: { crashed: false, text: '你停止了它，进程已关闭' },
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

// HEALTH_STATES maps a health state (monitor/health, wire-stable) to its
// label and colour class, in the server's order: abnormal first.
export const HEALTH_STATES = {
  failed: { label: '失败', cls: 'fail' },
  lost: { label: '失联', cls: 'warn' },
  stalled: { label: '停滞', cls: 'warn' },
  waiting: { label: '待你回复', cls: 'ask' },
  unknown: { label: '未知', cls: 'off' },
  active: { label: '活跃', cls: 'run' },
  idle: { label: '空闲', cls: 'idle' },
};
// HEALTH_ERROR_CLASSES labels health.Classify's error classes (§15.1);
// "other" names nothing and has no label.
export const HEALTH_ERROR_CLASSES = {
  auth: '凭证 / 权限拒绝',
  region: '地区限制',
  rate_limit: '限流',
  network: '网络断了',
  credential_expired: '凭证过期',
  disk_full: '磁盘满',
  refusal: '模型拒答',
};

// TITLE_WIDTH is how wide an automatic title may run, a CJK character
// counting 1 and any other 0.5: about 20 characters.
const TITLE_WIDTH = 20;

// autoTitle is a session's name made from its prompt (or the CLI's summary):
// the first sentence — up to the first 。 or line break — without code, cut
// to TITLE_WIDTH. A prompt that is all code keeps its first line.
export function autoTitle(text) {
  const raw = String(text || '').replace(/```[\s\S]*?(?:```|$)/g, ' ').replace(/`[^`\n]*`/g, ' ');
  const first = (s) => s.split(/[。\n]/).map((x) => x.replace(/\s+/g, ' ').trim()).find(Boolean) || '';
  const line = first(raw) || first(String(text || '').replace(/`/g, ''));
  let w = 0, out = '';
  for (const ch of line) {
    w += /[\u2E80-\u9FFF\uF900-\uFAFF\uFF00-\uFFEF]/.test(ch) ? 1 : 0.5;
    if (w > TITLE_WIDTH) return out.trimEnd() + '…';
    out += ch;
  }
  return out;
}

// sessionTitleText is a /api/sessions row's name: the one you gave it, else
// autoTitle of the CLI's summary or the latest prompt; '' for none.
export function sessionTitleText(s) {
  if (!s) return '';
  return s.user_label || autoTitle(s.summary || s.last_prompt || '');
}

// currentHealth is the health verdict on a /api/sessions row (s.health), or
// null when there is none or it did not take in the session's last activity:
// a send since then has moved past it while the next reconcile is due. A
// failure custody retried in vain keeps its since; seen is what it took in.
function currentHealth(s) {
  const h = s && s.health;
  if (!h || !HEALTH_STATES[h.state]) return null;
  return s.last_active > Math.max(h.since, h.seen || 0) ? null : h;
}

// sessionExit describes why a /api/sessions row s has no process, or null
// when it has one. A dead session reads its own death_reason (a timeout can
// leave death_reason on a live session); one that failed before a restart
// reads ready again, and only its health, which the health machine holds
// from the journal, still names the death. The title adds what the next
// send does (startup_failure) and death_detail.
export function sessionExit(s) {
  if (!s) return null;
  let reason = s.death_reason;
  if (s.state !== 'dead') {
    const h = s.state === 'ready' && currentHealth(s);
    if (!h || h.state !== 'failed' || !h.death_reason) return null;
    reason = h.death_reason;
  }
  const info = deathReasonInfo(reason);
  return { crashed: info.crashed, text: info.text, title: info.text + '，' + nextSendText(s.startup_failure) + (s.death_detail ? '\n' + s.death_detail : '') };
}

// DEATH_EVIDENCE prefixes a health failure's evidence when a process death
// caused it (health.DeathEvidence); the death reason follows.
export const DEATH_EVIDENCE = 'death_reason=';

// failureText is a failure's cause as the user reads it: its death reason
// in words, else its error class; '' when neither names one, which is left
// unwritten rather than written as "other".
export function failureText(reason, errClass) {
  const info = reason ? deathReasonInfo(reason) : null;
  if (info && !info.raw) return info.text;
  return Object.hasOwn(HEALTH_ERROR_CLASSES, errClass || '') ? HEALTH_ERROR_CLASSES[errClass] : '';
}

// evidenceCause is a health unit's or note's cause in words, from its
// evidence and error class; '' when neither names one.
export function evidenceCause(evidence, errClass) {
  const ev = evidence || '';
  return failureText(ev.startsWith(DEATH_EVIDENCE) ? ev.slice(DEATH_EVIDENCE.length) : '', errClass);
}

// evidenceText is a health unit's or note's evidence for a tooltip: a death
// in words alone, else the error class in words beside the raw evidence,
// which then is all that says what happened.
export function evidenceText(evidence, errClass) {
  const ev = evidence || '';
  const death = ev.startsWith(DEATH_EVIDENCE) && failureText(ev.slice(DEATH_EVIDENCE.length), '');
  return death || [failureText('', errClass), ev].filter(Boolean).join(' · ');
}

// sessionFailureText is why /api/sessions row s failed, in words; '' when
// nothing names it. A dead session's own death reason names it only when
// the death was abnormal: one reclaimed after a failed turn (idle_timeout)
// still failed for the reason its health verdict holds.
export function sessionFailureText(s) {
  const h = currentHealth(s);
  const own = s && s.state === 'dead' && deathReasonInfo(s.death_reason).crashed;
  return failureText(own ? s.death_reason : h && h.death_reason, h && h.error_class);
}

// sessionHealthState is the state a session's status dot shows: the live
// process first (a turn running, an abnormal death), then the health
// verdict; 'new' for a session not spawned yet, else idle. Without a turn
// running an "active" verdict is one the turn's end has outrun (the next
// reconcile is due), so it reads idle.
export function sessionHealthState(s) {
  if (!s || s.state === 'new') return 'new';
  if (s.state === 'running') return 'active';
  const x = sessionExit(s);
  if (x && x.crashed) return 'failed';
  const h = currentHealth(s);
  return h && h.state !== 'unknown' && h.state !== 'active' ? h.state : 'idle';
}

// HEALTH_DOTS are the status dot classes: failed red, active blue, the
// states that may need you amber, the rest grey.
const HEALTH_DOTS = { active: 'dot-running', failed: 'dot-failed', waiting: 'dot-warn', stalled: 'dot-warn', lost: 'dot-warn', new: 'dot-new' };

// sessionDot is a session's status dot: its class and the state in words
// (with a failure's cause) for the title.
export function sessionDot(s) {
  const st = sessionHealthState(s);
  const x = st === 'failed' ? sessionExit(s) : null;
  const cause = st === 'failed' ? sessionFailureText(s) || (x && x.crashed ? x.text : '') : '';
  const label = st === 'new' ? '新会话' : HEALTH_STATES[st].label;
  return { cls: HEALTH_DOTS[st] || 'dot-ready', title: label + (cause ? '：' + cause : '') };
}

// STARTUP_FAILURE_TEXT names the startup_failure classes an operator must fix.
const STARTUP_FAILURE_TEXT = {
  unknown: '',
  resume_not_found: '',
  auth: '后端认证失败',
  mcp_config: 'CLI 配置错误',
  missing_runtime: 'CLI 运行环境缺失',
};

// nextSendText is what the next send to a dead session does, by its startup_failure f.
function nextSendText(f) {
  if (f && f.retry_at > Date.now()) return 'CLI 连续启动失败（' + f.streak + ' 次），已暂停自动重试；' + new Date(f.retry_at).toTimeString().slice(0, 8) + ' 后可重试，或发送 /new 立即重试';
  if (f && f.new_session) return '下次发送将开启新会话（上次会话无法恢复）';
  const cause = f && Object.prototype.hasOwnProperty.call(STARTUP_FAILURE_TEXT, f.class) && STARTUP_FAILURE_TEXT[f.class];
  return cause ? cause + '，需管理员修复后重试' : '下次发送时自动恢复';
}

// deathReasonInfo is DEATH_REASONS' entry for reason, else the wording for a
// cli_exited reason that carries an exit code (-1: a signal killed the CLI)
// or a signal name, else an abnormal exit naming the raw value (raw set).
const { CODE: EXIT_CODE, SIGNAL: EXIT_SIGNAL } = NZ_CONTRACT.DEATH_REASON_PREFIX;
function deathReasonInfo(reason) {
  if (Object.prototype.hasOwnProperty.call(DEATH_REASONS, reason)) return DEATH_REASONS[reason];
  const r = String(reason || ''), code = r.startsWith(EXIT_CODE) ? r.slice(EXIT_CODE.length) : '';
  if (/^-?\d+$/.test(code)) return { crashed: true, text: code === '-1' ? 'CLI 进程被信号终止' : 'CLI 进程异常退出（退出码 ' + code + '）' };
  if (r.startsWith(EXIT_SIGNAL) && r.length > EXIT_SIGNAL.length) return { crashed: true, text: 'CLI 进程被信号 ' + r.slice(EXIT_SIGNAL.length) + ' 终止' };
  return { crashed: true, raw: true, text: reason ? '进程已退出（' + reason + '）' : '进程已退出' };
}

// sessionExitChipHtml is the chip sidebar cards and the session header show
// for a session whose process died abnormally; '' otherwise (a reclaimed
// process resumes on the next send and is not news).
export function sessionExitChipHtml(s) {
  const x = sessionExit(s);
  if (!x || !x.crashed) return '';
  return '<span class="sc-exit" title="' + escAttr(x.title) + '">⚠ 异常退出</span>';
}

// patchCardHealth brings a rendered session card's status dot and exit chip
// in line with its row s, for the paths that patch a card in place between
// renders; it produces the markup sessionCardHtml renders.
export function patchCardHealth(card, s) {
  const dot = card && card.querySelector('.sc-meta .sc-dot');
  if (!dot) return;
  const d = sessionDot(s);
  dot.className = 'sc-dot ' + d.cls;
  dot.title = d.title;
  dot.parentElement.querySelector('.sc-exit')?.remove();
  const html = sessionExitChipHtml(s);
  if (html) dot.insertAdjacentHTML('afterend', html);
}

// data-nz-bg: the only styling JS computes per element is a node /
// access-profile colour, which no class can express. The renderers put the value
// in a data attribute and this observer applies it; reconcileChildren applies it
// to incoming nodes too, so an unchanged card compares equal.
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

