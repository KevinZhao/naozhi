// @ts-check
// workflow_state.js — the dashboard's store of Workflow tool runs and the
// client half of their wire protocol (docs/rfc/workflow-dashboard.md §6.1,
// §7.1), a leaf (caps.leaves): it imports only session_ident.js. Every
// function is pure over the store the caller hands in; none fetches. A
// function that finds a fetch is due records it on the entry (want), and
// needsFetch / startFetch turn that into one request at a time per task.
//
// Versions: an entry's version is how far its header and phases have come,
// rowsAt how far its rows are complete (meaningful only while rowsLoaded,
// never above version). Within one epoch the board never deletes a row, so
// a full set of rows at H plus every later delta is the current set.
import { sid as sidOf } from './session_ident.js';

// The display of each wire status. Keys are exactly NZ_CONTRACT.ENUMS
// WORKFLOW_STATUS / WORKFLOW_AGENT_STATE (scripts/check-enum-literals.mjs);
// code reads them here rather than spelling the values.
export const WORKFLOW_STATUS_DISPLAY = {
  running: { glyph: '●', text: '运行中', tone: 'run', settled: false },
  paused: { glyph: '⏸', text: '已暂停', tone: 'mute', settled: false },
  completed: { glyph: '✓', text: '已完成', tone: 'ok', settled: true },
  failed: { glyph: '✗', text: '失败', tone: 'err', settled: true },
  killed: { glyph: '■', text: '已终止', tone: 'mute', settled: true },
  interrupted: { glyph: '⚠', text: '已中断', tone: 'mute', settled: true },
  unknown: { glyph: '?', text: '状态未知', tone: 'mute', settled: false },
};
// rank orders rows failed, running, queued, the other settled states, done.
export const WORKFLOW_AGENT_DISPLAY = {
  queued: { glyph: '⏳', text: '排队中', tone: 'dim', rank: 2 },
  running: { glyph: '▶', text: '运行中', tone: 'run', rank: 1 },
  done: { glyph: '✓', text: '已完成', tone: 'ok', rank: 4 },
  failed: { glyph: '✗', text: '失败', tone: 'err', rank: 0 },
  skipped: { glyph: '⤼', text: '已跳过', tone: 'mute', rank: 3 },
  stopped: { glyph: '■', text: '已停止', tone: 'mute', rank: 3 },
  unknown: { glyph: '?', text: '状态未知', tone: 'mute', rank: 3 },
};

// A workflow whose header names this source has its result file read.
const SOURCE_RESULT_FILE = 'result_file';
// Entries kept per session beyond the ones its last list names.
const SID_ENTRY_CAP = 37;
const RESULT_TRIES = 5;
const REFETCH_GAP_MS = 1000;
const BACKOFF_MIN_MS = 1000;
const BACKOFF_MAX_MS = 30000;
// A workflow_set or frame younger than this means pushes are flowing.
export const PUSH_FRESH_MS = 5000;
const TERMINAL_SHOWN = 3;
const MODE_RANK = { none: 0, delta: 1, full: 2 };

/** @typedef {'full' | 'delta' | 'none'} FetchMode */
/**
 * @typedef {object} WorkflowEntry
 * @property {string} key
 * @property {string} node
 * @property {string} taskId
 * @property {string} epoch '' until a frame or response names one
 * @property {number} version
 * @property {WireView | null} workflow header and phases; its agents are not kept
 * @property {boolean} rowsLoaded
 * @property {number} rowsAt
 * @property {Map<number, Agent>} rows by index
 * @property {WorkflowResult | null} result
 * @property {string[] | null} logs
 * @property {boolean} logsTruncated
 * @property {boolean} resultLoaded
 * @property {number} resultTries
 * @property {boolean} fetchInFlight
 * @property {FetchMode | null} inflight the mode of the request in flight
 * @property {FetchMode | null} want a fetch found due and not started
 * @property {number} retryAt no fetch before this (ms), after a failure
 * @property {number} backoffMs
 * @property {WsFrames['workflow_state'][]} buffered deltas held while a fetch is in flight
 * @property {number} frameAt when a workflow_state frame last arrived (ms)
 * @property {string} frameEpoch the epoch that frame named
 * @property {number} lastFetchAt
 * @property {boolean} open the panel shows it expanded
 * @property {boolean} sawUnsettled a header of this page showed it not settled
 * @property {boolean} staleRetried a response for another epoch was refetched once
 * @property {number} used last touched (ms), for the entry cap
 */
/**
 * @typedef {object} WorkflowBucket
 * @property {Map<string, WorkflowEntry>} tasks
 * @property {Set<string> | null} listed the last workflow_set's or Summary's tasks
 * @property {string | null} epoch the last workflow_set's epoch
 * @property {number} setAt when the last workflow_set arrived (ms)
 */

/** @param {string} st */
export function isSettled(st) {
  return WORKFLOW_STATUS_DISPLAY[st]?.settled === true;
}

/** @param {Map<string, WorkflowBucket>} store @param {string} s */
function bucket(store, s) {
  let b = store.get(s);
  if (!b) {
    b = { tasks: new Map(), listed: null, epoch: null, setAt: 0 };
    store.set(s, b);
  }
  return b;
}

/** @param {string} key @param {string} node @param {string} taskId @returns {WorkflowEntry} */
function newEntry(key, node, taskId) {
  return {
    key, node, taskId, epoch: '', version: 0, workflow: null,
    rowsLoaded: false, rowsAt: 0, rows: new Map(),
    result: null, logs: null, logsTruncated: false, resultLoaded: false, resultTries: 0,
    fetchInFlight: false, inflight: null, want: null, retryAt: 0, backoffMs: 0, buffered: [],
    frameAt: 0, frameEpoch: '', lastFetchAt: 0, open: false, sawUnsettled: false, staleRetried: false, used: 0,
  };
}

/** @param {WorkflowBucket} b @param {string} key @param {string} node @param {string} taskId @param {number} now */
function entryFor(b, key, node, taskId, now) {
  let e = b.tasks.get(taskId);
  if (!e) {
    e = newEntry(key, node, taskId);
    b.tasks.set(taskId, e);
    capEntries(b, e);
  }
  e.used = now;
  return e;
}

// capEntries drops the least recently used entries past SID_ENTRY_CAP that
// the session's last list does not name, never a listed one nor keep.
/** @param {WorkflowBucket} b @param {WorkflowEntry} keep */
function capEntries(b, keep) {
  if (b.tasks.size <= SID_ENTRY_CAP) return;
  const spare = [...b.tasks.values()].filter((e) => e !== keep && !b.listed?.has(e.taskId)).sort((x, y) => x.used - y.used);
  for (const e of spare.slice(0, b.tasks.size - SID_ENTRY_CAP)) b.tasks.delete(e.taskId);
}

/** @param {WorkflowEntry} e @param {FetchMode} mode */
function want(e, mode) {
  if (!e.want || MODE_RANK[mode] > MODE_RANK[e.want]) e.want = mode;
}

/** @param {WorkflowEntry} e @param {WireView} w */
function setHeader(e, w) {
  e.workflow = { ...w, agents: [] };
  if (!isSettled(w.status)) e.sawUnsettled = true;
}

/** @param {WorkflowEntry} e */
function dropRows(e) {
  e.rows = new Map();
  e.rowsLoaded = false;
  e.rowsAt = 0;
}

/** @param {WorkflowEntry} e */
function dropResult(e) {
  e.result = null;
  e.logs = null;
  e.logsTruncated = false;
  e.resultLoaded = false;
  e.resultTries = 0;
}

/** @param {WorkflowEntry} e @param {Agent[]} rows */
function mergeRows(e, rows) {
  for (const a of rows) e.rows.set(a.index, a);
}

// resultDue: an expanded, ended workflow whose result file was read and
// whose result this page has not got (RFC §6.1 结果拉取).
/** @param {WorkflowEntry} e */
function resultDue(e) {
  const w = e.workflow;
  return e.open && !!w && isSettled(w.status) && w.source === SOURCE_RESULT_FILE && !e.resultLoaded && e.resultTries < RESULT_TRIES;
}

/** @param {WorkflowEntry} e */
function settle(e) {
  if (e.rowsLoaded && e.rowsAt < e.version) want(e, 'delta');
  if (resultDue(e)) want(e, e.rowsLoaded ? 'delta' : 'none');
}

/**
 * applySet takes a workflow_set: the session's entries it does not name, or
 * that belong to another epoch, go.
 * @param {Map<string, WorkflowBucket>} store
 * @param {WsFrames['workflow_set']} set
 * @param {number} now
 */
export function applySet(store, set, now) {
  const b = bucket(store, sidOf(set.key, set.node));
  const ids = new Set(set.task_ids);
  for (const [id, e] of b.tasks) {
    if (!ids.has(id) || (e.epoch !== '' && e.epoch !== set.epoch)) b.tasks.delete(id);
  }
  b.listed = ids;
  b.epoch = set.epoch;
  b.setAt = now;
}

/**
 * applyFrame takes a workflow_state frame.
 * @param {Map<string, WorkflowBucket>} store
 * @param {WsFrames['workflow_state']} frame
 * @param {number} now
 */
export function applyFrame(store, frame, now) {
  const b = bucket(store, sidOf(frame.key, frame.node));
  const e = entryFor(b, frame.key, frame.node || '', frame.task_id, now);
  e.frameAt = now;
  e.frameEpoch = frame.epoch;
  e.staleRetried = false;
  if (frame.full) applyFull(e, frame);
  else applyDelta(e, frame);
  due(e);
}

/** @param {WorkflowEntry} e @param {WsFrames['workflow_state']} frame */
function applyFull(e, frame) {
  if (frame.epoch !== e.epoch) {
    e.epoch = frame.epoch;
    e.version = frame.version;
    setHeader(e, frame.workflow);
    dropRows(e);
    dropResult(e);
    e.buffered = [];
    return;
  }
  if (frame.version > e.version) {
    e.version = frame.version;
    setHeader(e, frame.workflow);
  }
}

/** @param {WorkflowEntry} e @param {WsFrames['workflow_state']} frame */
function applyDelta(e, frame) {
  if (frame.epoch !== e.epoch) {
    want(e, e.open ? 'full' : 'none');
    return;
  }
  if (e.fetchInFlight) {
    e.buffered.push(frame);
    return;
  }
  const base = frame.base_version || 0;
  if (frame.version > e.version) {
    if (base > e.version) {
      want(e, e.rowsLoaded ? 'delta' : 'none');
    } else {
      setHeader(e, frame.workflow);
      e.version = frame.version;
    }
  }
  if (!e.rowsLoaded) return;
  if (base > e.rowsAt) {
    want(e, 'delta');
  } else if (e.rowsAt < frame.version) {
    mergeRows(e, frame.workflow.agents);
    // Rows the frame left out changed after rowsAt too: rowsAt stays, so
    // the delta fetch based on it brings them.
    if (frame.rows_omitted) want(e, 'delta');
    else e.rowsAt = frame.version;
  }
}

/**
 * applyHttp takes a GET /api/sessions/workflow response for session s. The
 * header never goes back to an older version; full rows replace the held
 * ones only when newer. What the landing leaves due is worked out afresh,
 * so a frame that asked for the same fetch while it was in flight costs no
 * second one.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 * @param {RestResponses['sessions_workflow']} resp
 * @param {number} now
 */
export function applyHttp(store, s, resp, now) {
  const e = store.get(s)?.tasks.get(resp.workflow.task_id);
  if (!e) return;
  const { backoffMs, inflight } = e;
  e.fetchInFlight = false;
  e.inflight = null;
  e.want = null;
  e.backoffMs = 0;
  e.retryAt = 0;
  e.used = now;
  if (resp.epoch !== e.frameEpoch && now - e.frameAt < PUSH_FRESH_MS) {
    // Frames that are flowing name another epoch: this answer is stale.
    // Ask again once; an entry still without that epoch's header (a delta
    // of it came first) asks for what it asked before.
    const again = !e.staleRetried;
    e.staleRetried = true;
    replay(e);
    if (again) {
      if (e.epoch !== e.frameEpoch) want(e, inflight || 'none');
      due(e);
    }
    return;
  }
  e.staleRetried = false;
  if (resp.epoch !== e.epoch) {
    e.epoch = resp.epoch;
    e.version = 0;
    dropRows(e);
    dropResult(e);
  }
  const h = resp.version;
  if (h >= e.version) {
    setHeader(e, resp.workflow);
    e.version = h;
  }
  if (resp.rows_mode === 'full' && (!e.rowsLoaded || h > e.rowsAt)) {
    e.rows = new Map();
    mergeRows(e, resp.workflow.agents);
    e.rowsAt = h;
    e.rowsLoaded = true;
  } else if (resp.rows_mode === 'delta' && e.rowsLoaded && h >= e.rowsAt) {
    mergeRows(e, resp.workflow.agents);
    e.rowsAt = h;
  }
  if (resp.result || resp.logs) {
    e.result = resp.result || null;
    e.logs = resp.logs || [];
    e.logsTruncated = !!resp.logs_truncated;
    e.resultLoaded = true;
  } else if (resultDue(e)) {
    e.resultTries++;
    e.backoffMs = backoffMs;
    e.retryAt = now + backoff(e, 0);
  }
  replay(e);
  due(e);
}

// due records what e is owed now: rows when expanded without them, plus
// what settle finds.
/** @param {WorkflowEntry} e */
function due(e) {
  if (e.open && !e.rowsLoaded) want(e, 'full');
  settle(e);
}

/** @param {WorkflowEntry} e */
function replay(e) {
  const held = e.buffered;
  e.buffered = [];
  for (const f of held) applyDelta(e, f);
}

// backoff doubles the entry's wait from BACKOFF_MIN_MS up to BACKOFF_MAX_MS,
// or takes retryAfterMs when that is longer.
/** @param {WorkflowEntry} e @param {number} retryAfterMs */
function backoff(e, retryAfterMs) {
  e.backoffMs = Math.min(BACKOFF_MAX_MS, e.backoffMs ? e.backoffMs * 2 : BACKOFF_MIN_MS);
  return Math.max(e.backoffMs, retryAfterMs);
}

/**
 * applyHttpError takes a failed request: 404 (no such session, task or
 * node) drops the entry; anything else (429, 5xx, network error: status 0)
 * keeps the state and retries the same fetch after a backoff, at least
 * retryAfter seconds.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 * @param {string} taskId
 * @param {number} status
 * @param {number} retryAfter
 * @param {number} now
 */
export function applyHttpError(store, s, taskId, status, retryAfter, now) {
  const b = store.get(s);
  const e = b?.tasks.get(taskId);
  if (!b || !e) return;
  if (status === 404) {
    b.tasks.delete(taskId);
    return;
  }
  e.fetchInFlight = false;
  if (e.inflight) want(e, e.inflight);
  e.inflight = null;
  e.retryAt = now + backoff(e, (retryAfter || 0) * 1000);
}

/**
 * needsFetch is the mode of the fetch e is due, or null: none is due, one
 * is in flight (its landing looks again), or it must wait (backoff, or the
 * per-task refetch gap).
 * @param {WorkflowEntry} e
 * @param {number} now
 * @returns {FetchMode | null}
 */
export function needsFetch(e, now) {
  if (!e.want || e.fetchInFlight || now < e.retryAt || now - e.lastFetchAt < REFETCH_GAP_MS) return null;
  return e.want === 'delta' && !e.rowsLoaded ? 'none' : e.want;
}

/**
 * startFetch marks a fetch of mode in flight and returns its query string.
 * @param {WorkflowEntry} e
 * @param {FetchMode} mode
 * @param {number} now
 */
export function startFetch(e, mode, now) {
  e.fetchInFlight = true;
  e.inflight = mode;
  e.want = null;
  e.lastFetchAt = now;
  const q = new URLSearchParams({ key: e.key, task_id: e.taskId });
  if (e.node && e.node !== 'local') q.set('node', e.node);
  if (mode === 'none') q.set('rows', 'none');
  if (mode === 'delta') {
    q.set('since', String(e.rowsAt));
    q.set('epoch', e.epoch);
  }
  return q.toString();
}

/**
 * expand records whether the panel shows e expanded; an expanded entry asks
 * for its rows, and an ended one for its result, in one fetch.
 * @param {WorkflowEntry} e
 * @param {boolean} open
 */
export function expand(e, open) {
  e.open = open;
  if (open) due(e);
}

/** @param {string} s */
function splitSid(s) {
  const i = s.lastIndexOf('\t');
  return { key: s.slice(0, i), node: s.slice(i + 1) };
}

/**
 * reconcileSummaries is the fallback when no frames flow (RFC §6.1): after
 * an /api/sessions refresh, a workflow whose Summary is ahead of the entry
 * and that had no frame for PUSH_FRESH_MS is fetched (header only unless
 * rows are held); with neither a workflow_set nor a frame for the session
 * in that time, the Summary list trims its entries. A remote node's
 * sessions are skipped.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 * @param {Summary[] | undefined} summaries
 * @param {number} now
 */
export function reconcileSummaries(store, s, summaries, now) {
  const { key, node } = splitSid(s);
  if (node !== 'local') return;
  const b = bucket(store, s);
  const list = summaries || [];
  if (now - pushedAt(b) >= PUSH_FRESH_MS) {
    const ids = new Set(list.map((x) => x.task_id));
    for (const id of [...b.tasks.keys()]) if (!ids.has(id)) b.tasks.delete(id);
    b.listed = ids;
  }
  for (const sum of list) {
    const e = entryFor(b, key, '', sum.task_id, now);
    if (!e.workflow) e.workflow = headerOf(sum);
    if (now - e.frameAt < PUSH_FRESH_MS) continue;
    if (sum.epoch !== e.epoch || sum.version > e.version) want(e, e.rowsLoaded ? 'delta' : 'none');
  }
}

// pushedAt is when session b last had a workflow_set or frame (ms).
/** @param {WorkflowBucket} b */
function pushedAt(b) {
  let at = b.setAt;
  for (const e of b.tasks.values()) at = Math.max(at, e.frameAt);
  return at;
}

// headerOf stands a Summary in for a header until a frame or response
// brings one; version 0 lets either replace it.
/** @param {Summary} sum @returns {WireView} */
function headerOf(sum) {
  return {
    task_id: sum.task_id, name: sum.name, status: sum.status, counts: sum.counts, tokens: sum.tokens,
    started_at: sum.started_at, ended_at: sum.ended_at,
    phases: [], agents: [], source: '', version: 0,
  };
}

/**
 * releaseRows drops the rows and results of a session the panel stopped
 * showing; headers stay.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 */
export function releaseRows(store, s) {
  for (const e of store.get(s)?.tasks.values() || []) {
    dropRows(e);
    dropResult(e);
    e.buffered = [];
    e.open = false;
  }
}

/**
 * visibleWorkflows is what the panel shows of session s: every workflow not
 * settled (unknown included), newest started first, then the latest
 * TERMINAL_SHOWN settled ones, newest ended first.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 */
export function visibleWorkflows(store, s) {
  const all = [...(store.get(s)?.tasks.values() || [])].filter((e) => e.workflow);
  const open = all.filter((e) => !isSettled(e.workflow.status))
    .sort((x, y) => (y.workflow.started_at || 0) - (x.workflow.started_at || 0));
  const done = all.filter((e) => isSettled(e.workflow.status))
    .sort((x, y) => (y.workflow.ended_at || 0) - (x.workflow.ended_at || 0));
  return [...open, ...done.slice(0, TERMINAL_SHOWN)];
}

/**
 * rowsOf is e's rows in index order.
 * @param {WorkflowEntry} e
 */
export function rowsOf(e) {
  return [...e.rows.values()].sort((x, y) => x.index - y.index);
}

/**
 * announceable lists the workflows of session s that just ended: each
 * (task, settled status) once per page, recorded in announced. Only the
 * selected session's are returned; one first seen settled, or ended in a
 * session not shown, is recorded silently.
 * @param {Map<string, WorkflowBucket>} store
 * @param {string} s
 * @param {string} selectedSid
 * @param {Set<string>} announced
 */
export function announceable(store, s, selectedSid, announced) {
  const out = [];
  for (const e of store.get(s)?.tasks.values() || []) {
    const st = e.workflow?.status;
    if (!st || !isSettled(st)) continue;
    const mark = e.taskId + '\t' + st;
    if (announced.has(mark)) continue;
    announced.add(mark);
    if (e.sawUnsettled && s === selectedSid) out.push(e);
  }
  return out;
}
