// @ts-check
// workflow_view.js — the workflow panel (docs/rfc/workflow-dashboard.md
// §7.1-7.5, §7.7): the workflow_set / workflow_state handlers, which hand each
// frame to workflow_state.js's store as it came, the fetches that store asks
// for (for the session on screen only), the #workflow-panel above the
// transcript and the hooks dashboard.js wires. Built with DOM APIs and
// textContent only; repaints are batched to one per animation frame, at
// most one per PAINT_GAP_MS.
import { NZ_CONTRACT } from './contract.js';
import { wsm } from './ws_manager.js';
import { selection, sessionList } from './state.js';
import { authHeaders } from './platform.js';
import { fetchJSON, nzBus, nzViews } from './nz_util.js';
import { sid } from './session_ident.js';
import { announce, isMobile } from './utilities.js';
import {
  WORKFLOW_AGENT_DISPLAY, WORKFLOW_STATUS_DISPLAY, announceable, applyFrame, applyHttp, applyHttpError, applySet, expand, isSettled,
  needsFetch, reconcileSummaries, releaseRows, rowsOf, startFetch, visibleWorkflows,
} from './workflow_state.js';

/** @typedef {import('./workflow_state.js').WorkflowEntry} WorkflowEntry */
/**
 * @typedef {object} RowView
 * @property {HTMLElement} el
 * @property {string} state
 * @property {string} agentId
 * @property {number} rev
 */
/**
 * @typedef {object} WorkflowView a workflow's <details> and what it renders
 * @property {HTMLDetailsElement} el
 * @property {HTMLElement} sum
 * @property {HTMLElement} body
 * @property {Map<number, HTMLDetailsElement>} phases by phase index
 * @property {Map<number, boolean>} phaseOpen the user's phase toggles
 * @property {Map<number, number>} limit rows shown per phase
 * @property {Map<number, RowView>} rows by agent index
 * @property {Set<number>} attempts rows whose earlier attempts are listed
 * @property {HTMLElement} loading
 * @property {HTMLElement | null} result the result block, for resultOf
 * @property {object | null} resultOf
 * @property {boolean} openNow set open once it is in the panel
 * @property {WorkflowEntry} entry the store entry it renders
 * @property {string} epoch that entry's epoch when the caches were filled
 */

// store: session sid → its workflows (workflow_state.js); kept across turns
// and session switches, rows and results only for the session on screen.
const store = new Map();
// clock.offset is server time minus local time (ms), from the last
// server_now; null until one arrived.
const clock = { offset: null };
// pump.timer wakes the fetch pump for a fetch that had to wait.
const pump = { timer: 0, at: 0 };
// view is what the panel shows: its element, the session and a view per task.
const view = { panel: null, sid: '', wfs: /** @type {Map<string, WorkflowView>} */ (new Map()), selected: '', following: false };
const paint = { pending: false, last: 0 };
const tick = { timer: 0 };
// announced holds the (task, settled status) pairs this page has passed.
const announced = new Set();

const PAINT_GAP_MS = 250;
const PHASE_PAGE = 60;
const OPEN_KEY = 'nz_wf_open';
const OPEN_KEEP = 100;
const UNCLAIMED = 'unclaimed';
const DEGRADED_TEXT = {
  snapshot_stale: '连接中断，进度可能过时',
  no_snapshot: '暂无明细',
  decode_error: '部分字段无法解析',
  snapshot_dropped: '明细过大，已停止更新',
  too_many: '仅显示概要',
  phases_capped: 'phase 过多，部分未显示',
};

/** @param {number} serverNow */
function noteServerNow(serverNow) {
  if (serverNow) clock.offset = serverNow - Date.now();
}

/** serverNow is the server's clock (ms), or null before it is known. */
export function serverNow() {
  return clock.offset === null ? null : Date.now() + clock.offset;
}

// shownSid is the session on screen, null for none or a remote node's (the
// endpoint answers only for local sessions).
function shownSid() {
  if (!selection.key || (selection.node && selection.node !== 'local')) return null;
  return sid(selection.key, selection.node);
}

// pumpFetches starts every fetch the shown session's entries are due and
// arms one timer for the earliest that must wait.
function pumpFetches() {
  const s = shownSid();
  const tasks = s ? store.get(s)?.tasks : null;
  if (!s || !tasks) return;
  const now = Date.now();
  let next = Infinity;
  for (const e of tasks.values()) {
    const mode = needsFetch(e, now);
    if (mode) fetchEntry(s, e, mode, now);
    else if (e.want && !e.fetchInFlight) next = Math.min(next, Math.max(e.retryAt, e.lastFetchAt + 1000));
  }
  if (next === Infinity || (pump.timer && pump.at <= next)) return;
  clearTimeout(pump.timer);
  pump.at = next;
  pump.timer = setTimeout(() => { pump.timer = 0; pumpFetches(); }, Math.max(0, next - now));
}

/**
 * @param {string} s
 * @param {WorkflowEntry} e
 * @param {import('./workflow_state.js').FetchMode} mode
 * @param {number} now
 */
async function fetchEntry(s, e, mode, now) {
  const qs = startFetch(e, mode, now);
  let retryAfter = 0;
  try {
    const resp = /** @type {RestResponses['sessions_workflow']} */ (await fetchJSON(NZ_CONTRACT.API.sessions_workflow + '?' + qs, {
      headers: authHeaders(),
      onResponse: (r) => { retryAfter = Number(r.headers.get('Retry-After')) || 0; },
    }));
    noteServerNow(resp.server_now);
    applyHttp(store, s, resp, Date.now());
    if (s !== shownSid()) releaseRows(store, s);
  } catch (err) {
    applyHttpError(store, s, e.taskId, err?.status || 0, retryAfter, Date.now());
  }
  landed(s);
}

// landed follows a change to session s's entries: due fetches go out, ended
// workflows are announced and the panel repaints if s is on screen.
/** @param {string} s */
function landed(s) {
  pumpFetches();
  const selected = selection.key ? sid(selection.key, selection.node) : '';
  for (const e of announceable(store, s, selected, announced)) {
    const w = e.workflow;
    announce('Workflow ' + nameOf(w) + ' ' + WORKFLOW_STATUS_DISPLAY[w.status].text + '（' + w.counts.done + '/' + w.counts.total + '）');
  }
  if (s === shownSid()) schedulePaint();
}

wsm.on(NZ_CONTRACT.WS.workflow_set, (msg) => {
  noteServerNow(msg.server_now);
  applySet(store, msg, Date.now());
  landed(sid(msg.key, msg.node));
});
wsm.on(NZ_CONTRACT.WS.workflow_state, (msg) => {
  noteServerNow(msg.server_now);
  applyFrame(store, msg, Date.now());
  landed(sid(msg.key, msg.node));
});

function schedulePaint() {
  if (paint.pending) return;
  paint.pending = true;
  const wait = Math.max(0, paint.last + PAINT_GAP_MS - Date.now());
  setTimeout(() => requestAnimationFrame(renderWorkflowPanel), wait);
}

/** @param {WireView} w */
function nameOf(w) {
  return w.name || w.description || w.task_id;
}

/** @param {WireView} w */
function isRunning(w) {
  return WORKFLOW_STATUS_DISPLAY[w.status] === WORKFLOW_STATUS_DISPLAY.running;
}

/** @param {WireView} w */
function statusText(w) {
  const d = WORKFLOW_STATUS_DISPLAY[w.status] || WORKFLOW_STATUS_DISPLAY.unknown;
  return d === WORKFLOW_STATUS_DISPLAY.unknown && w.raw_status === UNCLAIMED ? d.text + '（进程未认领）' : d.text;
}

/** @param {number} n */
function fmtTokens(n) {
  if (n < 1000) return String(n);
  return n < 1e6 ? (n / 1000).toFixed(1) + 'k' : (n / 1e6).toFixed(1) + 'M';
}

/** @param {number} ms */
function fmtElapsed(ms) {
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm' + String(Math.floor(s % 60)).padStart(2, '0') + 's';
  return Math.floor(m / 60) + 'h' + String(m % 60).padStart(2, '0') + 'm';
}

// elapsedOf is the workflow's run time on the server's clock: its duration
// once ended, '' while the clock or the start is unknown.
/** @param {WireView} w */
function elapsedOf(w) {
  if (isSettled(w.status)) return w.duration_ms ? fmtElapsed(w.duration_ms) : '';
  const now = serverNow();
  if (now === null || !w.started_at) return '';
  return fmtElapsed(Math.max(0, now - w.started_at));
}

/**
 * el builds an element with a class and text.
 * @param {string} tag
 * @param {string} cls
 * @param {string} [text]
 */
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text) n.textContent = text;
  return n;
}

// glyph is a state's glyph, hidden from screen readers, and its text, hidden
// from view.
/** @param {{glyph: string, text: string, tone: string}} d @param {string} text */
function glyph(d, text) {
  const f = document.createDocumentFragment();
  const g = el('span', 'wf-glyph wf-t-' + d.tone, d.glyph);
  g.setAttribute('aria-hidden', 'true');
  f.append(g, el('span', 'sr-only', text));
  return f;
}

/** @param {string} cls @param {string} text */
function quiet(cls, text) {
  const n = el('span', cls, text);
  n.setAttribute('aria-hidden', 'true');
  return n;
}

// openPrefs is the user's expand / collapse choices, by task id.
function openPrefs() {
  try { return JSON.parse(sessionStorage.getItem(OPEN_KEY) || '{}') || {}; } catch (_) { return {}; }
}

/** @param {string} taskId @param {boolean} open */
function saveOpenPref(taskId, open) {
  const prefs = openPrefs();
  delete prefs[taskId];
  prefs[taskId] = open ? 1 : 0;
  const ids = Object.keys(prefs);
  for (const id of ids.slice(0, Math.max(0, ids.length - OPEN_KEEP))) delete prefs[id];
  try { sessionStorage.setItem(OPEN_KEY, JSON.stringify(prefs)); } catch (_) { /* storage full or denied */ }
}

/**
 * ensureRows follows a workflow's <details> opening or closing: an open one
 * gets its rows, and an ended one its result, in one fetch.
 * @param {string} s
 * @param {string} taskId
 * @param {boolean} open
 */
function ensureRows(s, taskId, open) {
  const e = store.get(s)?.tasks.get(taskId);
  if (!e) return;
  expand(e, open);
  pumpFetches();
  schedulePaint();
}

/**
 * newWorkflowView builds a workflow's <details>. It opens once in the panel
 * when the user left it open, or, on a desktop without a choice, when it is
 * the newest running workflow; either way through the toggle listener, which
 * persists only the user's own toggles.
 * @param {string} s
 * @param {WorkflowEntry} e
 * @param {boolean} newest
 * @returns {WorkflowView}
 */
function newWorkflowView(s, e, newest) {
  const d = /** @type {HTMLDetailsElement} */ (el('details', 'wf'));
  d.dataset.taskId = e.taskId;
  const sum = el('summary', 'wf-sum');
  const body = el('div', 'wf-body');
  d.append(sum, body);
  d.addEventListener('toggle', () => {
    if (d.dataset.autoOpen) delete d.dataset.autoOpen;
    else saveOpenPref(e.taskId, d.open);
    ensureRows(s, e.taskId, d.open);
  });
  const pref = openPrefs()[e.taskId];
  const openNow = pref !== undefined ? pref === 1 : (e.open || (newest && !isMobile()));
  const loading = el('div', 'wf-empty', '加载中…');
  return {
    el: d, sum, body, phases: new Map(), phaseOpen: new Map(), limit: new Map(), rows: new Map(), attempts: new Set(), loading, result: null, resultOf: null, openNow,
    entry: e, epoch: e.epoch,
  };
}

// letGoOfHidden drops the rows and results of the session the panel last
// showed once it is off screen, whichever way the dashboard left it.
function letGoOfHidden() {
  if (!view.sid || view.sid === shownSid()) return;
  releaseRows(store, view.sid);
  view.sid = '';
}

/**
 * renderWorkflowPanel paints #workflow-panel for the session on screen:
 * every workflow not settled and the latest three settled ones, each a
 * <details> whose rows render only while it is open.
 */
export function renderWorkflowPanel() {
  paint.pending = false;
  paint.last = Date.now();
  const panel = document.getElementById('workflow-panel');
  const s = shownSid() || '';
  letGoOfHidden();
  if (panel !== view.panel || s !== view.sid) {
    if (panel) panel.textContent = '';
    view.panel = panel;
    view.sid = s;
    view.wfs.clear();
  }
  const list = panel && s ? visibleWorkflows(store, s) : [];
  if (panel) panel.classList.toggle('nz-hidden', !list.length);
  const keep = new Set(list.map((e) => e.taskId));
  for (const [id, v] of view.wfs) {
    if (!keep.has(id)) { v.el.remove(); view.wfs.delete(id); }
  }
  const newest = list.find((e) => isRunning(e.workflow));
  list.forEach((e, i) => {
    let v = view.wfs.get(e.taskId);
    if (!v) { v = newWorkflowView(s, e, e === newest); view.wfs.set(e.taskId, v); }
    if (panel.children[i] !== v.el) panel.insertBefore(v.el, panel.children[i] || null);
    if (v.openNow) {
      v.openNow = false;
      v.el.dataset.autoOpen = '1';
      v.el.open = true;
    }
    paintWorkflow(v, e);
  });
  syncTicker(list);
}

/** @param {WorkflowView} v @param {WorkflowEntry} e */
function paintWorkflow(v, e) {
  rebind(v, e);
  paintSummary(v, e.workflow);
  if (!v.el.open) {
    v.body.textContent = '';
    v.phases.clear();
    v.rows.clear();
    return;
  }
  paintBody(v, e);
}

/**
 * rebind keeps view v in step with entry e. A new entry or epoch (a restart
 * builds a new board, whose revs start over) empties the row, phase and
 * result caches; a <details> left open makes an entry that is not (a
 * replaced or released one) ask for its rows.
 * @param {WorkflowView} v
 * @param {WorkflowEntry} e
 */
function rebind(v, e) {
  if (v.entry !== e || v.epoch !== e.epoch) {
    v.entry = e;
    v.epoch = e.epoch;
    v.body.textContent = '';
    v.phases.clear();
    v.rows.clear();
    v.result = null;
    v.resultOf = null;
  }
  if (v.el.open && !e.open) {
    expand(e, true);
    pumpFetches();
  }
}

/** @param {WorkflowView} v @param {WireView} w */
function paintSummary(v, w) {
  const d = WORKFLOW_STATUS_DISPLAY[w.status] || WORKFLOW_STATUS_DISPLAY.unknown;
  const c = w.counts;
  const text = statusText(w);
  v.sum.setAttribute('aria-label', 'Workflow ' + nameOf(w) + ' · ' + text + ' · ' + c.done + '/' + c.total);
  const sig = [w.status, w.raw_status, nameOf(w), w.description, w.degraded, w.agents_capped, currentPhase(w)].join('\n');
  if (v.sum.dataset.sig !== sig) {
    v.sum.dataset.sig = sig;
    v.sum.textContent = '';
    v.sum.append(glyph(d, text), el('span', 'wf-name', nameOf(w)));
    if (w.description && w.description !== nameOf(w)) v.sum.append(el('span', 'wf-desc', w.description));
    const cur = currentPhase(w);
    if (cur) v.sum.append(quiet('wf-cur', cur));
    v.sum.append(quiet('wf-counts', ''), quiet('wf-stat', ''));
    if (d === WORKFLOW_STATUS_DISPLAY.unknown) v.sum.append(quiet('wf-chip', text));
    if (w.degraded) v.sum.append(quiet('wf-chip', DEGRADED_TEXT[w.degraded] || w.degraded));
    if (w.agents_capped) v.sum.append(quiet('wf-chip', 'agent 过多，部分未显示'));
  }
  v.sum.querySelector('.wf-counts').textContent = '✓' + c.done + ' ▶' + c.running + ' ⏳' + c.queued + ' ✗' + c.failed + ' /' + c.total;
  const stat = fmtTokens(w.tokens || 0) + ' tok · ' + (w.tool_calls || 0) + ' tools';
  const el2 = elapsedOf(w);
  v.sum.querySelector('.wf-stat').textContent = el2 ? stat + ' · ' + el2 : stat;
}

// currentPhase is the first phase with agents still to finish (else the
// last), as "<title> done/total".
/** @param {WireView} w */
function currentPhase(w) {
  const ps = w.phases || [];
  const p = ps.find((x) => x.counts.queued + x.counts.running > 0) || ps[ps.length - 1];
  return p ? (p.title || '#' + p.index) + ' ' + p.counts.done + '/' + p.counts.total : '';
}

/** @param {WorkflowView} v @param {WorkflowEntry} e */
function paintBody(v, e) {
  const w = e.workflow;
  const groups = new Map((w.phases || []).map((p) => [p.index, /** @type {Agent[]} */ ([])]));
  const loose = [];
  for (const a of rowsOf(e)) (groups.get(a.phase_index || 0) || loose).push(a);
  const want = /** @type {Node[]} */ ([]);
  const seen = new Set();
  for (const p of w.phases || []) want.push(paintPhase(v, e.taskId, p, groups.get(p.index), seen));
  if (loose.length) want.push(paintPhase(v, e.taskId, null, loose, seen));
  for (const [i, pd] of v.phases) if (!want.includes(pd)) v.phases.delete(i);
  for (const i of [...v.rows.keys()]) if (!seen.has(i)) v.rows.delete(i);
  if (!e.rowsLoaded) want.push(v.loading);
  if (e.resultLoaded) {
    // Rebuilt only for a new result, so an opened log stays open.
    if (v.resultOf !== e.logs) { v.result = resultBlock(e); v.resultOf = e.logs; }
    want.push(v.result);
  }
  placeChildren(v.body, want);
}

/**
 * paintPhase renders one phase: a <details>, open unless every agent is
 * done or the user closed it, whose rows (at most limit) render only while
 * it is open. phase null groups the rows of no listed phase.
 * @param {WorkflowView} v
 * @param {string} taskId
 * @param {Phase | null} p
 * @param {Agent[]} rows
 * @param {Set<number>} seen
 */
function paintPhase(v, taskId, p, rows, seen) {
  const key = p ? p.index : -1;
  let pd = v.phases.get(key);
  if (!pd) {
    pd = /** @type {HTMLDetailsElement} */ (el('details', 'wf-phase'));
    const done = !!p && p.counts.total > 0 && p.counts.done === p.counts.total;
    pd.open = v.phaseOpen.get(key) ?? !done;
    const bar = /** @type {HTMLProgressElement} */ (el('progress', 'wf-bar'));
    pd.append(el('summary', 'wf-phase-sum'), el('div', 'wf-rows'));
    pd.firstElementChild.append(el('span', 'wf-phase-title'), bar, quiet('wf-phase-n', ''));
    pd.addEventListener('toggle', () => { v.phaseOpen.set(key, pd.open); schedulePaint(); });
    v.phases.set(key, pd);
  }
  const title = p ? p.title || '#' + p.index : '其他';
  const done = p ? p.counts.done : rows.filter((a) => WORKFLOW_AGENT_DISPLAY[a.state] === WORKFLOW_AGENT_DISPLAY.done).length;
  const total = p ? p.counts.total : rows.length;
  pd.querySelector('.wf-phase-title').textContent = title;
  const bar = /** @type {HTMLProgressElement} */ (pd.querySelector('progress'));
  bar.max = Math.max(1, total);
  bar.value = done;
  bar.setAttribute('aria-label', title + ' ' + done + '/' + total);
  pd.querySelector('.wf-phase-n').textContent = done + '/' + total;
  const box = pd.querySelector('.wf-rows');
  if (!pd.open) {
    box.textContent = '';
    return pd;
  }
  const rank = (a) => (WORKFLOW_AGENT_DISPLAY[a.state] || WORKFLOW_AGENT_DISPLAY.unknown).rank;
  const sorted = rows.slice().sort((x, y) => rank(x) - rank(y) || x.index - y.index);
  const limit = v.limit.get(key) || PHASE_PAGE;
  const kids = sorted.slice(0, limit).map((a) => { seen.add(a.index); return rowEl(v, taskId, a); });
  if (sorted.length > limit) {
    const more = el('button', 'wf-more', '显示其余 ' + (sorted.length - limit) + ' 个');
    more.setAttribute('type', 'button');
    more.dataset.action = 'wf-more';
    more.dataset.taskId = taskId;
    more.dataset.phase = String(key);
    kids.push(more);
  }
  placeChildren(box, kids);
  return pd;
}

/** @param {Agent} a */
function agentIdOf(a) {
  return a.agent_id || (a.prev_agent_ids || []).slice(-1)[0] || '';
}

/** @param {Agent} a */
function earlierIdsOf(a) {
  return (a.prev_agent_ids || []).filter((id) => id !== a.agent_id);
}

/**
 * openButton is a button drilling into agentId's transcript.
 * @param {string} cls
 * @param {string} taskId
 * @param {string} agentId
 */
function openButton(cls, taskId, agentId) {
  const b = el('button', cls);
  b.setAttribute('type', 'button');
  b.dataset.action = 'wf-open-agent';
  b.dataset.agentId = agentId;
  b.dataset.taskId = taskId;
  return b;
}

/**
 * rowEl is agent a's row element, rebuilt when its state or agent id
 * changed (a started agent's row becomes a button), refilled when only its
 * content did. A row with earlier attempts gets their badge, which lists
 * them, each a button too.
 * @param {WorkflowView} v
 * @param {string} taskId
 * @param {Agent} a
 */
function rowEl(v, taskId, a) {
  const r = v.rows.get(a.index);
  const id = agentIdOf(a);
  if (r && r.rev === a.rev) return r.el;
  if (r && r.state === a.state && r.agentId === id) {
    fillRow(r.el, a);
    r.rev = a.rev;
    return r.el;
  }
  const row = el('div', 'wf-row');
  row.dataset.index = String(a.index);
  const inner = id ? openButton('wf-row-btn', taskId, id) : el('div', 'wf-row-btn');
  const d = WORKFLOW_AGENT_DISPLAY[a.state] || WORKFLOW_AGENT_DISPLAY.unknown;
  row.classList.add('wf-s-' + d.tone);
  const earlier = earlierIdsOf(a);
  inner.append(glyph(d, d.text), el('span', 'wf-name'), el('span', 'wf-model'), quiet('wf-stat', ''), ...(earlier.length ? [] : [el('span', 'wf-attempt')]), el('span', 'wf-detail'));
  row.append(inner);
  if (earlier.length) row.append(...attemptList(v, taskId, a, earlier));
  fillRow(row, a);
  markSelected(row);
  if (r) r.el.replaceWith(row);
  v.rows.set(a.index, { el: row, state: a.state, agentId: id, rev: a.rev });
  return row;
}

/**
 * attemptList is a's badge of earlier attempts and the list it unfolds.
 * @param {WorkflowView} v
 * @param {string} taskId
 * @param {Agent} a
 * @param {string[]} earlier
 */
function attemptList(v, taskId, a, earlier) {
  const open = v.attempts.has(a.index);
  const badge = el('button', 'wf-attempt-btn', '×' + Math.max(a.attempt || 0, earlier.length + 1));
  badge.setAttribute('type', 'button');
  badge.setAttribute('aria-expanded', String(open));
  badge.setAttribute('aria-label', '较早的 ' + earlier.length + ' 次尝试');
  badge.dataset.action = 'wf-attempts';
  badge.dataset.taskId = taskId;
  const list = el('div', 'wf-attempts' + (open ? '' : ' nz-hidden'));
  earlier.forEach((prev, i) => {
    const b = openButton('wf-attempt-item', taskId, prev);
    b.dataset.attempt = String(i + 1);
    b.textContent = '第 ' + (i + 1) + ' 次';
    list.append(b);
  });
  return [badge, list];
}

/** @param {HTMLElement} row @param {Agent} a */
function fillRow(row, a) {
  const q = (/** @type {string} */ c) => row.querySelector(c);
  q('.wf-name').textContent = a.label || '#' + a.index;
  q('.wf-model').textContent = a.model || '';
  const stat = [];
  if (a.tokens) stat.push(fmtTokens(a.tokens) + ' tok');
  if (a.tool_calls !== undefined || a.tokens) stat.push((a.tool_calls || 0) + ' tools');
  if (a.duration_ms) stat.push(fmtElapsed(a.duration_ms));
  q('.wf-stat').textContent = stat.join(' · ');
  const at = q('.wf-attempt');
  if (at) at.textContent = a.attempt > 1 ? '×' + a.attempt : '';
  const unknown = !WORKFLOW_AGENT_DISPLAY[a.state] || WORKFLOW_AGENT_DISPLAY[a.state] === WORKFLOW_AGENT_DISPLAY.unknown;
  const tool = a.last_tool ? [a.last_tool, a.last_tool_summary].filter(Boolean).join(' · ') : '';
  q('.wf-detail').textContent = a.error || (unknown ? a.raw_state || '' : '') || tool;
}

// markSelected marks the row, and its button, of the agent drilled into.
/** @param {HTMLElement} row */
function markSelected(row) {
  let on = false;
  for (const btn of row.querySelectorAll('[data-agent-id]')) {
    const hit = !!view.selected && /** @type {HTMLElement} */ (btn).dataset.agentId === view.selected;
    on = on || hit;
    if (hit) btn.setAttribute('aria-current', 'true');
    else btn.removeAttribute('aria-current');
  }
  row.classList.toggle('wf-sel', on);
}

// onAgentView follows the agent view from the panel's first drill-in on:
// a row click, Esc or a session switch moves the mark.
/** @param {Event} ev */
function onAgentView(ev) {
  view.selected = /** @type {CustomEvent} */ (ev).detail?.taskID || '';
  for (const v of view.wfs.values()) for (const r of v.rows.values()) markSelected(r.el);
}

/** @param {WorkflowEntry} e */
function resultBlock(e) {
  const box = el('div', 'wf-result');
  if (e.result) {
    box.append(el('div', 'wf-result-label', e.result.truncated ? '结果（已截断）' : '结果'), el('pre', 'wf-result-text', e.result.text));
  }
  if (e.logs?.length) {
    const logs = el('details', 'wf-logs');
    logs.append(el('summary', '', '日志(' + e.logs.length + ')' + (e.logsTruncated ? ' · 较早的已省略' : '')), el('pre', 'wf-log-text', e.logs.join('\n')));
    box.append(logs);
  }
  return box;
}

// placeChildren makes parent's children exactly kids, in order, moving
// nodes it already has rather than rebuilding them.
/** @param {Element} parent @param {Node[]} kids */
function placeChildren(parent, kids) {
  kids.forEach((k, i) => {
    if (parent.childNodes[i] !== k) parent.insertBefore(k, parent.childNodes[i] || null);
  });
  while (parent.childNodes.length > kids.length) parent.lastChild.remove();
}

// syncTicker runs one 1s timer, while a running workflow is on screen, to
// move the elapsed times on.
/** @param {WorkflowEntry[]} list */
function syncTicker(list) {
  const want = list.some((e) => isRunning(e.workflow) && e.workflow.started_at);
  if (want && !tick.timer) {
    tick.timer = setInterval(() => {
      if (!view.panel?.isConnected) { clearInterval(tick.timer); tick.timer = 0; return; }
      for (const [id, v] of view.wfs) {
        const e = store.get(view.sid)?.tasks.get(id);
        if (e && isRunning(e.workflow)) paintSummary(v, e.workflow);
      }
    }, 1000);
  } else if (!want && tick.timer) {
    clearInterval(tick.timer);
    tick.timer = 0;
  }
}

/**
 * onSessionsRefreshed is the fallback when no frames flow: after an
 * /api/sessions refresh it forgets sessions the payload no longer lists and
 * holds the shown session's entries against the Summaries it carries
 * (workflow_state.js reconcileSummaries).
 */
export function onSessionsRefreshed() {
  const sessions = /** @type {SessionSnapshot[]} */ (sessionList.lastSidebarData?.sessions || []);
  const listed = new Set(sessions.map((x) => sid(x.key, x.node)));
  for (const s of [...store.keys()]) if (!listed.has(s)) store.delete(s);
  letGoOfHidden();
  const s = shownSid();
  const snap = s && sessions.find((x) => sid(x.key, x.node) === s);
  if (!snap) return;
  reconcileSummaries(store, s, snap.workflows, Date.now());
  landed(s);
}

/**
 * onWorkflowSessionSwitched lets go of the rows and results of the session
 * switched away from.
 * @param {string} prevSid
 */
export function onWorkflowSessionSwitched(prevSid) {
  if (prevSid) releaseRows(store, prevSid);
  pumpFetches();
}

// workflowActions are the panel's data-action handlers, merged into
// dashboard.js's registerActions table. wf-open-agent drills into a row's
// agent (or an earlier attempt), wf-attempts unfolds a row's attempts.
export const workflowActions = {
  'wf-more': (/** @type {HTMLElement} */ btn) => {
    const v = view.wfs.get(btn.dataset.taskId);
    if (!v) return;
    const key = Number(btn.dataset.phase);
    v.limit.set(key, (v.limit.get(key) || PHASE_PAGE) + PHASE_PAGE);
    renderWorkflowPanel();
  },
  'wf-open-agent': (/** @type {HTMLElement} */ btn) => {
    const id = btn.dataset.agentId;
    const v = view.wfs.get(btn.dataset.taskId);
    const row = btn.closest('.wf-row');
    if (!id || !v || !row) return;
    let label = row.querySelector('.wf-name')?.textContent || id;
    if (btn.dataset.attempt) label += '（第 ' + btn.dataset.attempt + ' 次）';
    const phase = btn.closest('.wf-phase')?.querySelector('.wf-phase-title')?.textContent;
    if (!view.following) { view.following = true; nzBus.addEventListener('agent:view', onAgentView); }
    const opts = { label, crumb: nameOf(v.entry.workflow) + (phase ? ' · ' + phase : '') };
    // A settled row's duration_ms is the agent's run time; an earlier attempt's is not on the row.
    const a = v.entry.rows.get(Number(/** @type {HTMLElement} */ (row).dataset.index));
    if (!btn.dataset.attempt && a?.duration_ms && WORKFLOW_AGENT_DISPLAY[a.state]?.settled) opts.durationMs = a.duration_ms;
    nzViews.agent?.switchTo(id, opts);
  },
  'wf-attempts': (/** @type {HTMLElement} */ btn) => {
    const v = view.wfs.get(btn.dataset.taskId);
    const row = btn.closest('.wf-row');
    if (!v || !row) return;
    const index = Number(/** @type {HTMLElement} */ (row).dataset.index);
    const open = !v.attempts.has(index);
    if (open) v.attempts.add(index);
    else v.attempts.delete(index);
    btn.setAttribute('aria-expanded', String(open));
    row.querySelector('.wf-attempts')?.classList.toggle('nz-hidden', !open);
  },
};
