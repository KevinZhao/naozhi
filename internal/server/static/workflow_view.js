// @ts-check
// workflow_view.js — the workflow panel's module (docs/rfc/workflow-dashboard.md
// §7.1): the workflow_set / workflow_state handlers, which hand each frame to
// workflow_state.js's store as it came, the fetches that store asks for (for
// the session on screen only) and the hooks dashboard.js wires. It renders
// nothing yet; renderWorkflowPanel is the panel's entry point.
import { NZ_CONTRACT } from './contract.js';
import { wsm } from './ws_manager.js';
import { selection, sessionList } from './state.js';
import { authHeaders } from './platform.js';
import { fetchJSON } from './nz_util.js';
import { sid } from './session_ident.js';
import { applyFrame, applyHttp, applyHttpError, applySet, needsFetch, reconcileSummaries, releaseRows, startFetch } from './workflow_state.js';

// store: session sid → its workflows (workflow_state.js); kept across turns
// and session switches, rows and results only for the session on screen.
const store = new Map();
// clock.offset is server time minus local time (ms), from the last
// server_now; null until one arrived.
const clock = { offset: null };
// pump.timer wakes the fetch pump for a fetch that had to wait.
const pump = { timer: 0, at: 0 };

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
 * @param {import('./workflow_state.js').WorkflowEntry} e
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
  } catch (err) {
    applyHttpError(store, s, e.taskId, err?.status || 0, retryAfter, Date.now());
  }
  pumpFetches();
}

wsm.on(NZ_CONTRACT.WS.workflow_set, (msg) => {
  noteServerNow(msg.server_now);
  applySet(store, msg, Date.now());
  pumpFetches();
});
wsm.on(NZ_CONTRACT.WS.workflow_state, (msg) => {
  noteServerNow(msg.server_now);
  applyFrame(store, msg, Date.now());
  pumpFetches();
});

/** renderWorkflowPanel paints the panel of the session on screen (PR-12). */
export function renderWorkflowPanel() {}

/**
 * onSessionsRefreshed is the fallback when no frames flow: after an
 * /api/sessions refresh it holds the shown session's entries against the
 * Summaries the payload carries (workflow_state.js reconcileSummaries).
 */
export function onSessionsRefreshed() {
  const s = shownSid();
  const sessions = /** @type {SessionSnapshot[]} */ (sessionList.lastSidebarData?.sessions || []);
  if (!s) return;
  const snap = sessions.find((x) => sid(x.key, x.node) === s);
  if (!snap) return;
  reconcileSummaries(store, s, snap.workflows, Date.now());
  pumpFetches();
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

/**
 * dropWorkflowSession forgets a deleted session.
 * @param {string} s
 */
export function dropWorkflowSession(s) {
  store.delete(s);
}

// workflowActions are the panel's data-action handlers, merged into
// dashboard.js's registerActions table.
export const workflowActions = {};
