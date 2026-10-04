// node --test scripts/cron-state.test.mjs
// cron_state.js is a leaf (caps.leaves): it loads in node with window, a
// MutationObserver and a bare document stubbed before the import. fetch is replaced per test by
// one that holds the GET /api/cron response until the test releases it, and
// Date.now by a clock the test advances, so "patched after the fetch started"
// is a fact of the test rather than of the scheduler.
import { test, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';

globalThis.window = globalThis;
globalThis.MutationObserver = class { observe() {} disconnect() {} };
globalThis.document = { documentElement: {}, addEventListener() {}, getElementById: () => null };

const { cronStore, cronRunClearedAtLocal, fetchCronJobs, noteCronRunCleared, reconcileCurrentRun } = await import('../internal/server/static/cron_state.js');

const realNow = Date.now;
const realFetch = globalThis.fetch;
let clock;
let held;

beforeEach(() => {
  clock = 1000;
  Date.now = () => clock;
  held = [];
  globalThis.fetch = () => new Promise(resolve => held.push(resolve));
  cronStore.jobs = [];
  cronRunClearedAtLocal.clear();
});
afterEach(() => {
  Date.now = realNow;
  globalThis.fetch = realFetch;
});

// startFetch issues fetchCronJobs at the current clock and returns a function
// that delivers `jobs` as its response and waits for the merge.
function startFetch() {
  const done = fetchCronJobs();
  const resolve = held.shift();
  assert.ok(resolve, 'fetchCronJobs issued no request');
  return (jobs) => {
    const body = JSON.stringify({ jobs });
    resolve({ ok: true, status: 200, text: async () => body });
    return done;
  };
}

// The writes cron_view.js's cronApplyRunStarted / cronApplyRunEnded make.
function patchStarted(job, runId, startedAt) {
  job.current_run = { run_id: runId, started_at: startedAt, phase: 'queued', trigger: 'manual', session_id: '', applied_at_local: Date.now() };
}
function patchEnded(job, runId) {
  job.current_run = null;
  noteCronRunCleared(job.id, runId);
}
const row = () => cronStore.jobs.find(j => j.id === 'j1');

test('a response built before run_ended(old) and run_started(new) keeps the new run', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }];
  const deliver = startFetch();
  clock += 5;
  patchEnded(row(), 'old');
  clock += 5;
  patchStarted(row(), 'new', 1009);
  await deliver([{ id: 'j1', current_run: { run_id: 'old', started_at: 1, phase: 'running' } }]);
  assert.equal(row().current_run.run_id, 'new');
  assert.equal(row().current_run.started_at, 1009);
});

test('a response without current_run keeps a run patched after the fetch started', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: null }];
  const deliver = startFetch();
  clock += 5;
  patchStarted(row(), 'new', 1005);
  await deliver([{ id: 'j1' }]);
  assert.equal(row().current_run.run_id, 'new');
});

test('a response naming the patched run takes its fields and keeps the patch time', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: null }];
  const older = startFetch();
  clock += 2;
  const newer = startFetch();
  clock += 3;
  patchStarted(row(), 'new', 1005);
  const appliedAt = row().current_run.applied_at_local;
  await newer([{ id: 'j1', current_run: { run_id: 'new', started_at: 1005, phase: 'running', session_id: 's-1' } }]);
  assert.deepEqual(row().current_run, { run_id: 'new', started_at: 1005, phase: 'running', session_id: 's-1', applied_at_local: appliedAt });
  // The first fetch, generated before run_started, lands last.
  await older([{ id: 'j1' }]);
  assert.equal(row().current_run.run_id, 'new');
  assert.equal(row().current_run.session_id, 's-1');
});

test('a response still reporting the run cleared after the fetch started does not bring it back', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }];
  const deliver = startFetch();
  clock += 5;
  patchEnded(row(), 'old');
  await deliver([{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }]);
  assert.equal(row().current_run, null);
});

test('two runs ending during one fetch both stay hidden from its response', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }];
  const deliver = startFetch();
  clock += 5;
  patchEnded(row(), 'old');
  clock += 1;
  patchStarted(row(), 'skip', 1006);
  patchEnded(row(), 'skip');
  await deliver([{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }]);
  assert.equal(row().current_run, null);
});

test('clears older than the keep window are dropped on the next clear', () => {
  noteCronRunCleared('j1', 'a');
  clock += 30000;
  noteCronRunCleared('j1', 'b');
  clock += 30001;
  noteCronRunCleared('j1', 'c');
  assert.deepEqual(cronRunClearedAtLocal.get('j1'), [{ at: 31000, runId: 'b' }, { at: 61001, runId: 'c' }]);
  noteCronRunCleared('j2', undefined);
  assert.deepEqual(cronRunClearedAtLocal.get('j2'), [{ at: 61001, runId: '' }]);
});

test('a response reporting a different run than the cleared one shows it', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: { run_id: 'old', started_at: 1 } }];
  const deliver = startFetch();
  clock += 5;
  patchEnded(row(), 'old');
  await deliver([{ id: 'j1', current_run: { run_id: 'next', started_at: 1003 } }]);
  assert.equal(row().current_run.run_id, 'next');
});

test('a fetch started after the local patch lets the server win either way', async () => {
  cronStore.jobs = [{ id: 'j1', current_run: null }];
  patchStarted(row(), 'new', 1000);
  clock += 5;
  await startFetch()([{ id: 'j1' }]);
  assert.equal(row().current_run, undefined);

  patchEnded(row(), 'new');
  clock += 5;
  await startFetch()([{ id: 'j1', current_run: { run_id: 'new', started_at: 1000 } }]);
  assert.equal(row().current_run.run_id, 'new');
});

test('reconcileCurrentRun branches', () => {
  const t0 = 100;
  const patched = { run_id: 'a', started_at: 1, applied_at_local: 101 };
  const stalePatch = { run_id: 'a', started_at: 1, applied_at_local: 99 };
  const fromServer = (id) => ({ run_id: id, started_at: 2, phase: 'running' });

  assert.equal(reconcileCurrentRun(patched, undefined, t0, undefined), patched);
  assert.equal(reconcileCurrentRun(patched, null, t0, undefined), patched);
  assert.equal(reconcileCurrentRun(patched, fromServer('b'), t0, undefined), patched);
  assert.deepEqual(reconcileCurrentRun(patched, fromServer('a'), t0, undefined), { run_id: 'a', started_at: 2, phase: 'running', applied_at_local: 101 });
  // A patch no newer than the fetch: the server is authoritative.
  assert.equal(reconcileCurrentRun(stalePatch, undefined, t0, undefined), undefined);
  const b = fromServer('b');
  assert.equal(reconcileCurrentRun(stalePatch, b, t0, undefined), b);
  assert.equal(reconcileCurrentRun({ ...patched, applied_at_local: t0 }, undefined, t0, undefined), undefined);

  assert.equal(reconcileCurrentRun(null, fromServer('a'), t0, [{ at: 101, runId: 'a' }]), null);
  assert.equal(reconcileCurrentRun(null, fromServer('a'), t0, [{ at: 101, runId: '' }]), null);
  assert.equal(reconcileCurrentRun(null, b, t0, [{ at: 101, runId: 'a' }]), b);
  assert.equal(reconcileCurrentRun(null, b, t0, [{ at: 100, runId: 'b' }]), b);
  // Any clear inside the window counts, not only the latest.
  assert.equal(reconcileCurrentRun(null, b, t0, [{ at: 101, runId: 'b' }, { at: 102, runId: 'c' }]), null);
  assert.equal(reconcileCurrentRun(null, b, t0, [{ at: 90, runId: 'b' }, { at: 102, runId: 'c' }]), b);
  assert.equal(reconcileCurrentRun(null, b, t0, undefined), b);
  assert.equal(reconcileCurrentRun(null, undefined, t0, [{ at: 101, runId: 'a' }]), undefined);
});
