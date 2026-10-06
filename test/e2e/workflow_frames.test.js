// @ts-check
//
// The workflow frames reach their store (docs/rfc/workflow-dashboard.md §14
// PR-11): workflow_view.js registers the workflow_set / workflow_state
// handlers, and the store's fetches go out for the open session only.
//  - a delta whose base is past the version the store holds is a gap: the
//    open session's workflow is fetched, header only (rows=none) as no rows
//    are held, and the answer is taken without error;
//  - the same gap in a session not on screen fetches nothing;
//  - after a workflow_set that drops its tasks, a late frame starts a new
//    entry, fetched the same way.
//
// Run: cd test/e2e && npx playwright test workflow_frames.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const EPOCH = '00000000000000a1';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

function view(taskID, version, agents = []) {
  return {
    task_id: taskID, name: 'probe', status: 'running', source: 'stream', version, started_at: Date.now() - 60000,
    counts: { total: 2, queued: 1, running: 1, done: 0, failed: 0, skipped: 0, stopped: 0 },
    phases: [{ index: 1, title: 'Ask', counts: { total: 2, queued: 1, running: 1, done: 0, failed: 0, skipped: 0, stopped: 0 } }],
    agents,
  };
}
const row = (index, rev) => ({ index, label: 'agent ' + index, state: 'running', rev });
const set = (key, ids) => ({ type: 'workflow_set', key, epoch: EPOCH, task_ids: ids, server_now: Date.now() });
const full = (key, taskID, version) => ({ type: 'workflow_state', key, task_id: taskID, epoch: EPOCH, version, full: true, server_now: Date.now(), workflow: view(taskID, version) });
const delta = (key, taskID, version, base) => ({
  type: 'workflow_state', key, task_id: taskID, epoch: EPOCH, version, base_version: base, full: false, server_now: Date.now(),
  workflow: view(taskID, version, [row(1, version)]),
});

test('a gap in the open session fetches its header; one in another session fetches nothing', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({
    ws: true,
    workflows: { w1: { epoch: EPOCH, version: 9, workflow: view('w1', 9, [row(0, 3), row(1, 9)]) } },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === A)).toBe(true);

    // B's gap goes first: had it fetched, its request would come first.
    conn.send(set(B, ['w2']));
    conn.send(full(B, 'w2', 5));
    conn.send(delta(B, 'w2', 9, 7));
    conn.send(set(A, ['w1']));
    conn.send(full(A, 'w1', 5));
    conn.send(delta(A, 'w1', 6, 5));
    conn.send(delta(A, 'w1', 9, 7));
    await expect.poll(() => mock.workflowCalls.length).toBeGreaterThan(0);
    expect(mock.workflowCalls[0]).toEqual({ key: A, task_id: 'w1', rows: 'none' });

    // Dropped by the set, a task's late gap starts from nothing (a 404 here).
    conn.send(set(A, []));
    conn.send(delta(A, 'w9', 3, 1));
    await expect.poll(() => mock.workflowCalls.map((q) => q.task_id)).toEqual(['w1', 'w9']);
    expect(mock.workflowCalls[1]).toEqual({ key: A, task_id: 'w9', rows: 'none' });
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});
