// node --test scripts/workflow-state.test.mjs
// workflow_state.js is a leaf (caps.leaves): the client half of the workflow
// wire protocol (docs/rfc/workflow-dashboard.md §6.1), driven here frame by
// frame and response by response. A "request" is what the view's fetch pump
// would send: needsFetch says one is due, startFetch marks it in flight.
import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.window = globalThis;
globalThis.MutationObserver = class { observe() {} disconnect() {} };
globalThis.document = { documentElement: {}, addEventListener() {}, getElementById: () => null };

const ws = await import('../internal/server/static/workflow_state.js');
const { NZ_CONTRACT } = await import('../internal/server/static/contract.js');

const KEY = 'feishu:p2p:u1';
const SID = KEY + '\tlocal';
const E1 = 'aaaaaaaaaaaaaaaa';
const E2 = 'bbbbbbbbbbbbbbbb';
const T0 = 1_000_000;

function wv(task, version, over = {}) {
  return {
    task_id: task, status: 'running', counts: { total: 0, queued: 0, running: 0, done: 0, failed: 0, skipped: 0, stopped: 0 },
    phases: [], agents: [], source: 'stream', version, ...over,
  };
}
const row = (index, rev, state = 'running') => ({ index, label: 'a' + index, state, rev });
const full = (task, epoch, version, over) => ({ type: 'workflow_state', key: KEY, task_id: task, epoch, version, full: true, server_now: 1, workflow: wv(task, version, over) });
const delta = (task, epoch, version, base, agents, over = {}, extra = {}) => ({
  type: 'workflow_state', key: KEY, task_id: task, epoch, version, base_version: base, full: false, server_now: 1,
  workflow: wv(task, version, { ...over, agents }), ...extra,
});
const set = (ids, epoch) => ({ type: 'workflow_set', key: KEY, epoch, task_ids: ids, server_now: 1 });
const resp = (task, epoch, version, mode, agents = [], over = {}, extra = {}) => ({
  epoch, version, server_now: 1, rows_mode: mode, workflow: wv(task, version, { ...over, agents }), ...extra,
});
const entry = (store, task) => store.get(SID)?.tasks.get(task);

// requests starts every fetch due at now and returns [task, mode, query].
function requests(store, now) {
  const out = [];
  for (const b of store.values()) {
    for (const e of b.tasks.values()) {
      const mode = ws.needsFetch(e, now);
      if (mode) out.push([e.taskId, mode, ws.startFetch(e, mode, now)]);
    }
  }
  return out;
}
const modes = (reqs) => reqs.map(([t, m]) => t + ':' + m);

// loaded is a store holding task w1 in E1 at version 5, rows complete at 5,
// expanded, with no fetch due.
function loaded() {
  const store = new Map();
  ws.applySet(store, set(['w1'], E1), T0);
  ws.applyFrame(store, full('w1', E1, 5), T0);
  ws.expand(entry(store, 'w1'), true);
  assert.deepEqual(modes(requests(store, T0)), ['w1:full']);
  ws.applyHttp(store, SID, resp('w1', E1, 5, 'full', [row(0, 3), row(1, 5)]), T0);
  assert.deepEqual(requests(store, T0 + 5000), []);
  return store;
}

test('workflow_set drops the entries it does not name and those of another epoch', () => {
  const store = loaded();
  ws.applyFrame(store, full('w2', E1, 6), T0);
  ws.applyFrame(store, full('w3', E1, 6), T0);
  ws.applySet(store, set(['w1', 'w3'], E1), T0);
  assert.deepEqual([...store.get(SID).tasks.keys()].sort(), ['w1', 'w3']);
  ws.applySet(store, set(['w1'], E2), T0);
  assert.equal(store.get(SID).tasks.size, 0, 'an entry of the old epoch goes even when listed');
  ws.applyFrame(store, full('w1', E2, 1), T0);
  ws.applySet(store, set([], E2), T0);
  assert.equal(store.get(SID).tasks.size, 0, '/new: the empty set clears the session');
});

test('a full frame of a new epoch drops rows and result; of the same epoch it keeps them', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 5), T0 + 5000);
  assert.equal(e.rowsLoaded, true);
  assert.equal(e.rows.size, 2);
  assert.deepEqual(requests(store, T0 + 10000), [], 'a resubscribe with nothing new costs no request');

  ws.applyFrame(store, full('w1', E1, 8, { name: 'later' }), T0 + 10000);
  assert.equal(e.version, 8);
  assert.equal(e.workflow.name, 'later');
  assert.equal(e.rows.size, 2, 'rows kept');
  assert.deepEqual(requests(store, T0 + 20000).map(([, m, q]) => m + ' ' + q), [`delta key=${encodeURIComponent(KEY)}&task_id=w1&since=5&epoch=${E1}`]);

  const store2 = loaded();
  const e2 = entry(store2, 'w1');
  e2.result = { text: 'r', truncated: false };
  e2.resultLoaded = true;
  ws.applyFrame(store2, full('w1', E2, 2), T0 + 5000);
  assert.equal(e2.rowsLoaded, false);
  assert.equal(e2.rows.size, 0);
  assert.equal(e2.resultLoaded, false);
  assert.equal(e2.result, null);
  assert.deepEqual(modes(requests(store2, T0 + 10000)), ['w1:full'], 'expanded: rows of the new epoch');
});

test('a full frame arriving while a fetch is in flight sends no second request', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  ws.expand(entry(store, 'w1'), true);
  assert.deepEqual(modes(requests(store, T0)), ['w1:full']);
  ws.applyFrame(store, full('w1', E1, 5), T0 + 100);
  ws.applyFrame(store, full('w1', E1, 7), T0 + 200);
  assert.deepEqual(requests(store, T0 + 3000), [], 'in flight');
  ws.applyHttp(store, SID, resp('w1', E1, 7, 'full', [row(0, 7)]), T0 + 300);
  assert.deepEqual(requests(store, T0 + 3000), [], 'the landing covered what the frames asked for');
});

test('an HTTP response older than the frames never moves the header or version back', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  const e = entry(store, 'w1');
  ws.expand(e, true);
  requests(store, T0);
  ws.applyFrame(store, full('w1', E1, 9, { name: 'new' }), T0 + 10);
  ws.applyHttp(store, SID, resp('w1', E1, 6, 'full', [row(0, 6)], { name: 'old' }), T0 + 20);
  assert.equal(e.version, 9);
  assert.equal(e.workflow.name, 'new');
  assert.equal(e.rowsAt, 6);
  assert.deepEqual(modes(requests(store, T0 + 2000)), ['w1:delta'], 'rows behind the header: one delta');
});

test('a delta at or below the held version leaves the header', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, delta('w1', E1, 5, 3, [], { name: 'stale' }), T0 + 5000);
  assert.equal(e.workflow.name, undefined);
  assert.deepEqual(requests(store, T0 + 10000), []);
});

test('a delta whose base is past the held version is a gap: fetch (header only without rows)', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  ws.applyFrame(store, delta('w1', E1, 9, 7, [row(0, 9)], { name: 'skipped ahead' }), T0 + 10);
  const e = entry(store, 'w1');
  assert.equal(e.version, 5, 'a gap does not apply the header');
  assert.deepEqual(requests(store, T0 + 2000).map(([, m, q]) => m + ' ' + q), [`none key=${encodeURIComponent(KEY)}&task_id=w1&rows=none`]);

  const s2 = loaded();
  ws.applyFrame(s2, delta('w1', E1, 9, 7, [row(0, 9)]), T0 + 5000);
  assert.deepEqual(modes(requests(s2, T0 + 10000)), ['w1:delta']);
});

test('a delta based at or below rowsAt merges by index and moves rowsAt', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, delta('w1', E1, 7, 5, [row(1, 7, 'done'), row(2, 7)]), T0 + 5000);
  assert.equal(e.version, 7);
  assert.equal(e.rowsAt, 7);
  assert.deepEqual(ws.rowsOf(e).map((a) => a.index + a.state), ['0running', '1done', '2running']);
  ws.applyFrame(store, delta('w1', E1, 9, 6, [row(1, 9, 'failed')]), T0 + 5100);
  assert.equal(e.rowsAt, 9);
  assert.equal(e.rows.get(1).state, 'failed');
  assert.deepEqual(requests(store, T0 + 10000), [], 'a running workflow\'s deltas after the fetch cost no request');
});

test('a delta based past rowsAt asks for the rows since rowsAt', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  requests(store, T0 + 10000);
  ws.applyHttp(store, SID, resp('w1', E1, 7, 'delta', []), T0 + 10000);
  e.rowsAt = 5;
  ws.applyFrame(store, delta('w1', E1, 9, 7, [row(0, 9)]), T0 + 12000);
  assert.equal(e.version, 9, 'the header applies');
  assert.equal(e.rowsAt, 5, 'the rows do not');
  assert.deepEqual(requests(store, T0 + 20000).map(([, m, q]) => m + ' ' + q), [`delta key=${encodeURIComponent(KEY)}&task_id=w1&since=5&epoch=${E1}`]);
});

test('deltas arriving while a fetch is in flight are held and replayed after it lands', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 10000)), ['w1:delta']);
  ws.applyFrame(store, delta('w1', E1, 8, 7, [row(2, 8)]), T0 + 10010);
  ws.applyFrame(store, delta('w1', E1, 9, 8, [row(2, 9, 'done')]), T0 + 10020);
  assert.equal(e.buffered.length, 2);
  assert.equal(e.version, 7);
  ws.applyHttp(store, SID, resp('w1', E1, 7, 'delta', [row(1, 7, 'done')]), T0 + 10030);
  assert.equal(e.buffered.length, 0);
  assert.equal(e.version, 9);
  assert.equal(e.rowsAt, 9);
  assert.deepEqual(ws.rowsOf(e).map((a) => a.index + a.state), ['0running', '1done', '2done']);
  assert.deepEqual(requests(store, T0 + 20000), []);
});

test('expanding and the full frame of a resubscribe together cost one request', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  const e = entry(store, 'w1');
  ws.expand(e, true);
  ws.applyFrame(store, full('w1', E1, 5), T0 + 1);
  assert.deepEqual(modes(requests(store, T0 + 2)), ['w1:full']);
  ws.expand(e, true);
  ws.applyFrame(store, full('w1', E1, 5), T0 + 3);
  assert.deepEqual(requests(store, T0 + 2000), []);
});

test('the Summary fallback fetches the header alone without rows, and the rows since rowsAt with them', () => {
  const sum = (task, epoch, version, status = 'running') => ({ task_id: task, status, counts: wv(task, 0).counts, epoch, version });
  const store = new Map();
  ws.reconcileSummaries(store, SID, [sum('w1', E1, 4)], T0);
  assert.deepEqual(modes(requests(store, T0)), ['w1:none']);
  ws.applyHttp(store, SID, resp('w1', E1, 4, 'none'), T0 + 10);
  ws.reconcileSummaries(store, SID, [sum('w1', E1, 4)], T0 + 5000);
  assert.deepEqual(requests(store, T0 + 5000), [], 'nothing new');
  for (let i = 1; i <= 5; i++) {
    ws.reconcileSummaries(store, SID, [sum('w1', E1, 4 + i)], T0 + 5000 * (i + 1));
    const reqs = requests(store, T0 + 5000 * (i + 1));
    assert.deepEqual(modes(reqs), ['w1:none'], 'a collapsed workflow polled with the socket down never fetches rows');
    ws.applyHttp(store, SID, resp('w1', E1, 4 + i, 'none'), T0 + 5000 * (i + 1));
  }

  const rows = loaded();
  ws.reconcileSummaries(rows, SID, [sum('w1', E1, 8)], T0 + 4000);
  assert.deepEqual(requests(rows, T0 + 4000), [], 'a frame within 5s: pushes flow, the Summary is not acted on');
  ws.reconcileSummaries(rows, SID, [sum('w1', E1, 8)], T0 + 5000);
  assert.deepEqual(modes(requests(rows, T0 + 5000)), ['w1:delta']);
  const other = loaded();
  ws.reconcileSummaries(other, SID, [sum('w1', E2, 1)], T0 + 6000);
  assert.deepEqual(modes(requests(other, T0 + 6000)), ['w1:delta'], 'another epoch is fetched too');
  ws.applyHttp(other, SID, resp('w1', E2, 1, 'full', [row(0, 1)]), T0 + 6000);
  const e = entry(other, 'w1');
  assert.equal(e.epoch, E2, 'with no frames flowing the answer is the newer state');
  assert.equal(e.rowsAt, 1);
  assert.deepEqual(ws.rowsOf(e).map((a) => a.index), [0]);
});

test('the Summary fallback trims without a recent workflow_set or frame, keeps unknown, and skips remote nodes', () => {
  const sum = (task, status) => ({ task_id: task, status, counts: wv(task, 0).counts, epoch: E1, version: 1 });
  const store = new Map();
  ws.applySet(store, set(['w1', 'w2', 'w3'], E1), T0);
  for (const t of ['w1', 'w2', 'w3']) ws.applyFrame(store, full(t, E1, 1), T0);
  ws.reconcileSummaries(store, SID, [sum('w1', 'running')], T0 + 1000);
  assert.equal(store.get(SID).tasks.size, 3, 'a recent workflow_set is the list');
  ws.applyFrame(store, full('w2', E1, 2), T0 + 4000);
  ws.reconcileSummaries(store, SID, [sum('w1', 'running')], T0 + 8000);
  assert.equal(store.get(SID).tasks.size, 3, 'frames flowing: the last workflow_set still stands, however old');
  ws.reconcileSummaries(store, SID, [sum('w1', 'running'), sum('w3', 'unknown')], T0 + 9000);
  assert.deepEqual([...store.get(SID).tasks.keys()].sort(), ['w1', 'w3']);
  ws.applyFrame(store, full('w3', E1, 2, { status: 'unknown' }), T0 + 9000);
  assert.deepEqual(ws.visibleWorkflows(store, SID).map((e) => e.taskId).sort(), ['w1', 'w3'], 'unknown is shown');

  const remote = KEY + '\tnode-b';
  ws.reconcileSummaries(store, remote, [sum('w9', 'running')], T0);
  assert.equal(store.has(remote), false);
});

test('result fetch: an expanded workflow ending with its result file fetches once, rows and result together', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, delta('w1', E1, 7, 5, [row(1, 7, 'done')], { status: 'completed' }), T0 + 5000);
  assert.deepEqual(requests(store, T0 + 10000), [], 'not before the result file was read');
  ws.applyFrame(store, delta('w1', E1, 8, 7, [row(0, 8, 'done')], { status: 'completed', source: 'result_file' }), T0 + 10000);
  assert.deepEqual(modes(requests(store, T0 + 11000)), ['w1:delta']);
  ws.applyHttp(store, SID, resp('w1', E1, 8, 'delta', [], { status: 'completed', source: 'result_file' }, { result: { text: '{"r":1}', truncated: false }, logs: ['l1'] }), T0 + 11000);
  assert.equal(e.resultLoaded, true);
  assert.deepEqual(e.logs, ['l1']);
  ws.applyFrame(store, full('w1', E1, 8, { status: 'completed', source: 'result_file' }), T0 + 12000);
  assert.deepEqual(requests(store, T0 + 20000), [], 'once loaded, never again');

  // Expanded only once it had ended: the rows and the result in one fetch.
  const later = new Map();
  ws.applyFrame(later, full('w1', E1, 8, { status: 'completed', source: 'result_file' }), T0);
  assert.deepEqual(requests(later, T0), []);
  ws.expand(entry(later, 'w1'), true);
  assert.deepEqual(modes(requests(later, T0)), ['w1:full']);
  ws.applyHttp(later, SID, resp('w1', E1, 8, 'full', [row(0, 8, 'done')], { status: 'completed', source: 'result_file' }, { result: { text: 'r', truncated: false } }), T0);
  assert.equal(entry(later, 'w1').resultLoaded, true);
  assert.deepEqual(requests(later, T0 + 5000), []);
});

test('result fetch: not for a collapsed workflow, nor one that ended without a result file', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  ws.applyFrame(store, delta('w1', E1, 8, 5, [], { status: 'completed', source: 'result_file' }), T0 + 10);
  assert.deepEqual(requests(store, T0 + 2000), [], 'collapsed');
  const s2 = loaded();
  ws.applyFrame(s2, delta('w1', E1, 8, 5, [], { status: 'interrupted', source: 'stream' }), T0 + 5000);
  assert.deepEqual(requests(s2, T0 + 10000), [], 'interrupted: no result file to read');
});

test('result fetch: result_unavailable retries with a backoff, at most five times', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  const done = { status: 'completed', source: 'result_file' };
  ws.applyFrame(store, delta('w1', E1, 8, 5, [], done), T0 + 5000);
  let now = T0 + 5000;
  const gaps = [];
  for (let i = 0; i < 8; i++) {
    let reqs = requests(store, now);
    let waited = 0;
    while (reqs.length === 0 && waited < 120000) {
      now += 500;
      waited += 500;
      reqs = requests(store, now);
    }
    if (reqs.length === 0) break;
    gaps.push(waited);
    ws.applyHttp(store, SID, resp('w1', E1, 8, 'delta', [], done, { result_unavailable: true }), now);
  }
  assert.equal(gaps.length, 5, 'five tries, then only a new expansion retries');
  assert.ok(gaps[1] >= 1000 && gaps[2] >= 2000 && gaps[3] >= 4000 && gaps[4] >= 8000, `backoff gaps ${gaps}`);
  ws.releaseRows(store, SID);
  ws.applyFrame(store, full('w1', E1, 8, done), now);
  ws.expand(e, true);
  assert.deepEqual(modes(requests(store, now + 60000)), ['w1:full'], 'expanding again tries again');
});

test('agents_capped never fetches; rows_omitted fetches once, from the rows already complete', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, delta('w1', E1, 7, 5, [row(1, 7)], { agents_capped: true }), T0 + 5000);
  assert.deepEqual(requests(store, T0 + 10000), []);
  ws.applyFrame(store, delta('w1', E1, 9, 7, [row(2, 9)], {}, { rows_omitted: 40 }), T0 + 10000);
  assert.equal(e.rowsAt, 7, 'the omitted rows changed after 7');
  assert.equal(e.rows.get(2).rev, 9, 'the rows it carried merge');
  const reqs = requests(store, T0 + 11000);
  assert.deepEqual(reqs.map(([, m, q]) => m + ' ' + q), [`delta key=${encodeURIComponent(KEY)}&task_id=w1&since=7&epoch=${E1}`]);
  ws.applyHttp(store, SID, resp('w1', E1, 9, 'delta', [row(3, 9)]), T0 + 11000);
  assert.equal(e.rowsAt, 9);
  assert.deepEqual(requests(store, T0 + 20000), []);
});

test('a delta of another epoch, or a response of another epoch, refetches', () => {
  const store = loaded();
  ws.applyFrame(store, delta('w1', E2, 3, 1, [row(0, 3)]), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 10000)), ['w1:full']);
  ws.applyFrame(store, full('w1', E2, 3), T0 + 10001);
  ws.applyHttp(store, SID, resp('w1', E1, 5, 'full', [row(0, 5)]), T0 + 10002);
  const e = entry(store, 'w1');
  assert.equal(e.epoch, E2);
  assert.equal(e.rowsLoaded, false, 'an answer for the old epoch is dropped');
  assert.deepEqual(modes(requests(store, T0 + 12000)), ['w1:full'], 'and asked again');
  ws.applyHttp(store, SID, resp('w1', E1, 5, 'full', [row(0, 5)]), T0 + 12001);
  assert.deepEqual(requests(store, T0 + 14000), [], 'only once');
});

test('an answer naming the epoch of the flowing frames is taken, by an entry a delta started or one of an older epoch', () => {
  const sum = (task, status, ended) => ({ task_id: task, status, counts: wv(task, 0).counts, epoch: E1, version: 1, ended_at: ended });
  const store = new Map();
  ws.applySet(store, set(['w1', 'w2', 'w3', 'w4'], E1), T0);
  for (const [i, t] of ['w1', 'w2', 'w3', 'w4'].entries()) ws.applyFrame(store, full(t, E1, 1, { status: 'completed', ended_at: 100 - i }), T0);
  ws.reconcileSummaries(store, SID, ['w1', 'w2', 'w3'].map((t, i) => sum(t, 'completed', 100 - i)), T0 + 10000);
  assert.equal(entry(store, 'w4'), undefined, 'the Summary trims the fourth settled one');
  ws.applyFrame(store, delta('w4', E1, 2, 1, [], { status: 'completed', source: 'result_file' }), T0 + 11000);
  const e = entry(store, 'w4');
  assert.equal(e.epoch, '', 'a delta names no epoch for an entry without one');
  assert.deepEqual(modes(requests(store, T0 + 11000)), ['w4:none']);
  ws.applyHttp(store, SID, resp('w4', E1, 2, 'none', [], { status: 'completed', source: 'result_file' }), T0 + 11010);
  assert.equal(e.epoch, E1);
  assert.equal(e.workflow?.source, 'result_file');
  assert.equal(e.version, 2);
  ws.applyFrame(store, delta('w4', E1, 3, 2, [], { status: 'completed', source: 'result_file', tokens: 9 }), T0 + 12000);
  assert.equal(e.workflow?.tokens, 9, 'later deltas apply');
  assert.deepEqual(requests(store, T0 + 14000), [], 'no fetch per delta');

  const older = new Map();
  ws.applyFrame(older, full('w1', E1, 5), T0);
  ws.applyFrame(older, delta('w1', E2, 3, 1, [row(0, 3)], { name: 'rebuilt' }), T0 + 5000);
  assert.deepEqual(modes(requests(older, T0 + 5000)), ['w1:none']);
  ws.applyHttp(older, SID, resp('w1', E2, 3, 'none', [], { name: 'rebuilt' }), T0 + 5010);
  const o = entry(older, 'w1');
  assert.equal(o.epoch, E2);
  assert.equal(o.workflow?.name, 'rebuilt');
  assert.deepEqual(requests(older, T0 + 8000), []);
});

test('a stale answer for a collapsed entry that lacks the frames\' epoch refetches once', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 5), T0);
  ws.applyFrame(store, delta('w1', E2, 3, 1, []), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 5000)), ['w1:none']);
  ws.applyHttp(store, SID, resp('w1', E1, 6, 'none'), T0 + 5010);
  const e = entry(store, 'w1');
  assert.equal(e.epoch, E1);
  assert.equal(e.version, 5, 'the stale answer is dropped');
  assert.deepEqual(modes(requests(store, T0 + 6010)), ['w1:none'], 'and asked again');
  ws.applyHttp(store, SID, resp('w1', E1, 6, 'none'), T0 + 6020);
  assert.deepEqual(requests(store, T0 + 9000), [], 'only once');
});

test('a full frame of a new epoch drops the deltas held for the old one', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 10000)), ['w1:delta']);
  ws.applyFrame(store, delta('w1', E1, 8, 7, [row(2, 8)]), T0 + 10010);
  ws.applyFrame(store, full('w1', E2, 2), T0 + 10020);
  assert.equal(e.buffered.length, 0);
  ws.applyHttp(store, SID, resp('w1', E2, 2, 'full', [row(0, 2)]), T0 + 10030);
  assert.equal(e.rowsAt, 2);
  assert.deepEqual(ws.rowsOf(e).map((a) => a.index), [0]);
  assert.deepEqual(requests(store, T0 + 20000), [], 'no old-epoch delta replayed into a refetch');
});

test('after releaseRows a delta due is fetched header only, and a delta answer adds no rows', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  ws.releaseRows(store, SID);
  assert.deepEqual(requests(store, T0 + 10000).map(([, m, q]) => m + ' ' + q), [`none key=${encodeURIComponent(KEY)}&task_id=w1&rows=none`]);

  const s2 = loaded();
  const e2 = entry(s2, 'w1');
  ws.applyFrame(s2, full('w1', E1, 7), T0 + 5000);
  assert.deepEqual(modes(requests(s2, T0 + 10000)), ['w1:delta']);
  ws.releaseRows(s2, SID);
  ws.applyHttp(s2, SID, resp('w1', E1, 7, 'delta', [row(2, 7)]), T0 + 10010);
  assert.equal(e2.rowsLoaded, false);
  assert.equal(e2.rows.size, 0);
  assert.equal(e2.rowsAt, 0);
  assert.equal(e.rowsAt, 0);
});

test('429 and network errors keep the state and retry the same fetch after a backoff, Retry-After included', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 10000)), ['w1:delta']);
  ws.applyFrame(store, delta('w1', E1, 8, 7, [row(4, 8)]), T0 + 10001);
  ws.applyHttpError(store, SID, 'w1', 429, 5, T0 + 10002);
  assert.equal(e.rows.size, 2);
  assert.equal(e.buffered.length, 1, 'held deltas stay');
  assert.deepEqual(requests(store, T0 + 14000), [], 'Retry-After 5s wins over the 1s backoff');
  assert.deepEqual(modes(requests(store, T0 + 15003)), ['w1:delta']);
  ws.applyHttpError(store, SID, 'w1', 0, 0, T0 + 15004);
  assert.deepEqual(requests(store, T0 + 16500), [], 'doubled to 2s');
  assert.deepEqual(modes(requests(store, T0 + 17005)), ['w1:delta']);
});

test('a task is fetched at most once a second, the next fetch waiting out the gap', () => {
  const store = loaded();
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  assert.deepEqual(modes(requests(store, T0 + 5000)), ['w1:delta']);
  ws.applyHttp(store, SID, resp('w1', E1, 7, 'delta'), T0 + 5100);
  ws.applyFrame(store, delta('w1', E1, 9, 8, [row(0, 9)]), T0 + 5200);
  assert.deepEqual(requests(store, T0 + 5999), [], 'within a second of the last');
  assert.deepEqual(modes(requests(store, T0 + 6000)), ['w1:delta']);
});

test('404 drops the entry', () => {
  const store = loaded();
  ws.applyFrame(store, full('w1', E1, 7), T0 + 5000);
  requests(store, T0 + 10000);
  ws.applyHttpError(store, SID, 'w1', 404, 0, T0 + 10001);
  assert.equal(entry(store, 'w1'), undefined);
  ws.applyHttp(store, SID, resp('w1', E1, 7, 'delta'), T0 + 10002);
  assert.equal(entry(store, 'w1'), undefined, 'a late answer does not bring it back');
});

test('visibleWorkflows shows every unsettled one, unknown included, then the three latest settled', () => {
  const store = new Map();
  ws.applyFrame(store, full('w1', E1, 1, { status: 'running', started_at: 10 }), T0);
  ws.applyFrame(store, full('w2', E1, 1, { status: 'unknown', raw_status: 'unclaimed', started_at: 20 }), T0);
  ws.applyFrame(store, full('w3', E1, 1, { status: 'paused', started_at: 5 }), T0);
  for (let i = 0; i < 5; i++) ws.applyFrame(store, full('t' + i, E1, 1, { status: 'completed', ended_at: 100 + i }), T0);
  assert.deepEqual(ws.visibleWorkflows(store, SID).map((e) => e.taskId), ['w2', 'w1', 'w3', 't4', 't3', 't2']);
});

test('the entry cap never drops what the last workflow_set listed', () => {
  const store = new Map();
  const ids = Array.from({ length: 37 }, (_, i) => 'w' + i);
  ws.applySet(store, set(ids, E1), T0);
  ids.forEach((id, i) => ws.applyFrame(store, full(id, E1, 1), T0 + i));
  ws.applyFrame(store, full('x1', E1, 1), T0 + 100);
  ws.applyFrame(store, full('x2', E1, 1), T0 + 101);
  const kept = [...store.get(SID).tasks.keys()];
  for (const id of ids) assert.ok(kept.includes(id), id + ' was dropped');
  assert.equal(kept.length, 38, 'only the oldest unlisted entry goes');
  assert.ok(kept.includes('x2'));
});

test('releaseRows drops rows and results and keeps headers', () => {
  const store = loaded();
  const e = entry(store, 'w1');
  e.result = { text: 'r', truncated: false };
  e.resultLoaded = true;
  ws.releaseRows(store, SID);
  assert.equal(e.rowsLoaded, false);
  assert.equal(e.resultLoaded, false);
  assert.equal(e.rows.size, 0);
  assert.equal(e.result, null);
  assert.equal(e.open, false);
  assert.equal(e.version, 5);
  assert.ok(e.workflow);
});

test('announceable: a workflow seen running and ending in the shown session, once', () => {
  const store = new Map();
  const announced = new Set();
  const other = 'cron:j1\tlocal';
  ws.applyFrame(store, full('w1', E1, 1), T0);
  ws.applyFrame(store, full('w0', E1, 1, { status: 'completed' }), T0);
  assert.deepEqual(ws.announceable(store, SID, SID, announced), [], 'first seen settled: silent');
  ws.applyFrame(store, delta('w1', E1, 2, 1, [], { status: 'completed' }), T0);
  assert.deepEqual(ws.announceable(store, SID, SID, announced).map((e) => e.taskId), ['w1']);
  assert.deepEqual(ws.announceable(store, SID, SID, announced), [], 'once');
  ws.applyFrame(store, { ...full('j1', E1, 1), key: 'cron:j1' }, T0);
  ws.applyFrame(store, { ...delta('j1', E1, 2, 1, [], { status: 'failed' }), key: 'cron:j1' }, T0);
  assert.deepEqual(ws.announceable(store, other, SID, announced), [], 'another session ends silently');
  assert.deepEqual(ws.announceable(store, other, other, announced), [], 'and stays silent when shown');
});

test('the display tables hold exactly the contract enums', () => {
  assert.deepEqual(Object.keys(ws.WORKFLOW_STATUS_DISPLAY).sort(), [...NZ_CONTRACT.ENUMS.WORKFLOW_STATUS].sort());
  assert.deepEqual(Object.keys(ws.WORKFLOW_AGENT_DISPLAY).sort(), [...NZ_CONTRACT.ENUMS.WORKFLOW_AGENT_STATE].sort());
  for (const st of NZ_CONTRACT.ENUMS.WORKFLOW_STATUS) {
    assert.equal(ws.isSettled(st), ['completed', 'failed', 'killed', 'interrupted'].includes(st), st);
  }
});
