// @ts-check
//
// The workflow panel (docs/rfc/workflow-dashboard.md §7.2-7.5, §7.7, §11.4a):
// one <details> per workflow above the transcript, fed by workflow_set /
// workflow_state frames and GET /api/sessions/workflow, rows only while open.
//  - counts, a done phase folded, rows paged by 60; the newest running
//    workflow opens by itself and fetches its rows once, without writing
//    sessionStorage; a later delta fetches nothing; body.kbd-open hides it;
//    a phone opens nothing by itself;
//  - an ended workflow opened later shows its result and logs; one whose
//    result is unavailable keeps its header and rows;
//  - an auto-opened workflow that ends makes exactly one result request;
//  - a remote node's session never calls the endpoint.
//
// Run: cd test/e2e && npx playwright test workflow_panel.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const REMOTE = 'dashboard:direct:2026-01-01-115900-r:general';
const EPOCH = '00000000000000a1';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

const counts = (o = {}) => ({ total: 0, queued: 0, running: 0, done: 0, failed: 0, skipped: 0, stopped: 0, ...o });
const row = (index, rev, state, over = {}) => ({
  index, phase_index: 1, label: 'agent ' + index, state, rev, ...(state !== 'queued' && { agent_id: 'a' + index }), ...over,
});
// view is a WireView whose counts follow its rows; phases group them by phase_index.
function view(id, version, agents, over = {}) {
  const c = counts();
  const per = {};
  for (const a of agents) {
    c.total++; c[a.state]++;
    per[a.phase_index] = per[a.phase_index] || counts();
    per[a.phase_index].total++; per[a.phase_index][a.state]++;
  }
  const phases = Object.keys(per).map(Number).sort((x, y) => x - y).map((i) => ({ index: i, title: 'Phase' + i, counts: per[i] }));
  return {
    task_id: id, name: 'wf ' + id, status: 'running', source: 'stream', version, started_at: Date.now() - 60000,
    counts: c, phases, agents, tokens: 1200, tool_calls: 3, ...over,
  };
}
const fixture = (w, extra = {}) => ({ epoch: EPOCH, version: w.version, workflow: w, ...extra });
const head = (w) => ({ ...w, agents: [] });
const set = (key, ids, epoch = EPOCH, node) => ({ type: 'workflow_set', key, ...(node && { node }), epoch, task_ids: ids, server_now: Date.now() });
const full = (key, w, epoch = EPOCH, node) => ({
  type: 'workflow_state', key, ...(node && { node }), task_id: w.task_id, epoch, version: w.version, full: true, server_now: Date.now(), workflow: head(w),
});
const delta = (key, w, base, extra = {}) => ({
  type: 'workflow_state', key, task_id: w.task_id, epoch: EPOCH, version: w.version, base_version: base, full: false,
  server_now: Date.now(), workflow: w, ...extra,
});
const callsFor = (mock, id) => mock.workflowCalls.filter((q) => q.task_id === id);
const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key).length;
const panel = (page) => page.locator('#workflow-panel');
const wf = (page, id) => page.locator(`#workflow-panel .wf[data-task-id="${id}"]`);
const MARKS = ['m1', 'm2', 'm3'];
// SLOW holds every workflow answer (workflowDelayMs) past the 1s per-task
// refetch gap, so a fetch a frame makes due after an answer lands goes out at once.
const SLOW = 1100;

// boot opens the dashboard over a mock socket, selects key and returns the
// connection once the dashboard has subscribed. The workflows fixtures get
// the markers mark uses; the object stays the one passed, for in-place edits.
async function boot(page, overrides, key = A) {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const workflows = overrides.workflows || {};
  for (const id of MARKS) workflows[id] = fixture(view(id, 3, [], { status: 'completed' }));
  const mock = await startMockServer({ ws: true, ...overrides, workflows });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click(`.session-card[data-key="${key}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, key)).toBe(1);
  return { mock, conn, errors };
}

// push sends a workflow_set naming the workflows and a full frame of each.
function push(conn, key, ws) {
  conn.send(set(key, ws.map((w) => w.task_id)));
  for (const w of ws) conn.send(full(key, w));
}

// mark fences "no other fetch went out": a frame for a fresh, ended task
// whose header fetch goes out at once. A fetch the frames before it made due
// went out no later, unless the refetch gap held it (see SLOW).
async function mark(conn, mock, id, key = A) {
  conn.send(delta(key, view(id, 3, [], { status: 'completed' }), 1));
  await expect.poll(() => callsFor(mock, id).length).toBe(1);
}

// refreshed bumps the sessions payload with a visible change and waits for
// the sidebar to show it: the onSessionsApplied hooks have run by then.
async function refreshed(page, conn, data, text) {
  data.sessions[0].last_prompt = text;
  data.stats.version++;
  conn.send({ type: 'sessions_update' });
  await expect(page.locator(`.session-card[data-key="${A}"]`)).toContainText(text);
}

test('counts, folded done phase, 60-row pages; the auto-opened workflow fetches once, unpersisted', async ({ page }) => {
  const rows = [row(0, 2, 'done'), row(1, 2, 'done')];
  for (let i = 2; i < 72; i++) rows.push(row(i, 3, i < 7 ? 'running' : 'queued', { phase_index: 2 }));
  const w1 = view('w1', 9, rows);
  const { mock, conn, errors } = await boot(page, { workflows: { w1: fixture(w1) }, workflowDelayMs: SLOW });
  try {
    push(conn, A, [w1]);
    await expect(wf(page, 'w1')).toHaveAttribute('open', '');
    await expect.poll(() => callsFor(mock, 'w1')).toEqual([{ key: A, task_id: 'w1' }]);
    await expect(panel(page)).toBeVisible();
    await expect(wf(page, 'w1').locator('.wf-counts')).toHaveText('✓2 ▶5 ⏳65 ✗0 /72');
    const phases = wf(page, 'w1').locator('.wf-phase');
    await expect(phases.nth(1).locator('.wf-row')).toHaveCount(60);
    await expect(phases.nth(0)).not.toHaveAttribute('open', '');
    await expect(phases.nth(0).locator('.wf-row')).toHaveCount(0);
    await expect(phases.nth(1).locator('progress')).toHaveAttribute('aria-label', 'Phase2 0/70');
    // Rows run failed, running, queued, then the rest: running ones first here.
    await expect(phases.nth(1).locator('.wf-row').first()).toHaveAttribute('data-index', '2');
    await phases.nth(1).locator('.wf-more').click();
    await expect(phases.nth(1).locator('.wf-row')).toHaveCount(70);
    await expect(phases.nth(1).locator('.wf-more')).toHaveCount(0);
    expect(await page.evaluate(() => sessionStorage.getItem('nz_wf_open'))).toBeNull();

    // A delta on top of the rows merges locally.
    const w1b = view('w1', 10, [row(2, 10, 'done', { phase_index: 2 })], { counts: counts({ total: 72, done: 3, running: 4, queued: 65 }), phases: w1.phases });
    conn.send(delta(A, w1b, 9));
    await expect(wf(page, 'w1').locator('.wf-row[data-index="2"] .sr-only')).toHaveText('已完成');
    await mark(conn, mock, 'm1');
    expect(callsFor(mock, 'w1').length).toBe(1);

    // Only the user's own toggles are kept.
    await wf(page, 'w1').locator('> summary').click();
    await expect.poll(() => page.evaluate(() => sessionStorage.getItem('nz_wf_open'))).toBe('{"w1":0}');
    await wf(page, 'w1').locator('> summary').click();
    await expect.poll(() => page.evaluate(() => sessionStorage.getItem('nz_wf_open'))).toBe('{"w1":1}');
    await mark(conn, mock, 'm2');
    expect(callsFor(mock, 'w1').length).toBe(1);

    await page.evaluate(() => document.body.classList.add('kbd-open'));
    await expect(panel(page)).toBeHidden();
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('on a phone the newest running workflow stays folded and fetches nothing', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const w1 = view('w1', 9, [row(0, 9, 'running')]);
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1) } });
  try {
    push(conn, A, [w1]);
    await expect(wf(page, 'w1')).toBeVisible();
    await mark(conn, mock, 'm1');
    await expect(wf(page, 'w1')).not.toHaveAttribute('open', '');
    expect(callsFor(mock, 'w1')).toEqual([]);
  } finally { mock.server.close(); }
});

test('an ended workflow opened later shows result and logs; an unavailable result keeps the entry', async ({ page }) => {
  const ended = (id) => view(id, 7, [row(0, 6, 'done'), row(1, 7, 'done')], { status: 'completed', source: 'result_file', duration_ms: 300000 });
  const w3 = ended('w3');
  const w5 = ended('w5');
  const workflows = {
    w3: fixture(w3, { result: { text: '{"r":["4","Paris"],"s":"ok"}', truncated: false }, logs: ['phase Ask', 'phase Sum'] }),
    w5: fixture(w5, { result_unavailable: true }),
  };
  const { mock, conn, errors } = await boot(page, { workflows });
  try {
    push(conn, A, [w3, w5]);
    await expect(wf(page, 'w3')).toBeVisible();
    await expect(wf(page, 'w3')).not.toHaveAttribute('open', '');
    await wf(page, 'w3').locator('> summary').click();
    await expect(wf(page, 'w3').locator('.wf-result-text')).toHaveText('{"r":["4","Paris"],"s":"ok"}');
    await expect(wf(page, 'w3').locator('.wf-logs > summary')).toHaveText('日志(2)');
    expect(callsFor(mock, 'w3')).toEqual([{ key: A, task_id: 'w3' }]);
    // The done phase opens on a click; its rows render then.
    await wf(page, 'w3').locator('.wf-phase > summary').click();
    await expect(wf(page, 'w3').locator('.wf-row')).toHaveCount(2);

    await wf(page, 'w5').locator('> summary').click();
    await expect.poll(() => callsFor(mock, 'w5').length).toBeGreaterThan(0);
    await wf(page, 'w5').locator('.wf-phase > summary').click();
    await expect(wf(page, 'w5').locator('.wf-row')).toHaveCount(2);
    await expect(wf(page, 'w5').locator('.wf-result')).toHaveCount(0);
    // The result is asked again after a backoff; the entry stays.
    await expect.poll(() => callsFor(mock, 'w5').length, { timeout: 8000 }).toBeGreaterThan(1);
    await expect(wf(page, 'w5')).toBeVisible();
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('an auto-opened running workflow that ends makes exactly one result request', async ({ page }) => {
  const w1 = view('w1', 9, [row(0, 5, 'done'), row(1, 9, 'running')]);
  const workflows = { w1: fixture(w1) };
  const { mock, conn, errors } = await boot(page, { workflows, workflowDelayMs: SLOW });
  try {
    push(conn, A, [w1]);
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(1);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(2);

    const ended = view('w1', 10, [row(1, 10, 'done')], {
      status: 'completed', counts: counts({ total: 2, done: 2 }), phases: [{ index: 1, title: 'Phase1', counts: counts({ total: 2, done: 2 }) }],
    });
    conn.send(delta(A, ended, 9));
    await expect(wf(page, 'w1').locator('> summary .sr-only')).toHaveText('已完成');
    await mark(conn, mock, 'm1');
    expect(callsFor(mock, 'w1').length).toBe(1);

    const filed = { ...ended, version: 11, source: 'result_file', agents: [] };
    workflows.w1 = fixture({ ...filed, agents: [row(0, 5, 'done'), row(1, 10, 'done')] }, { result: { text: 'OK', truncated: false }, logs: ['done'] });
    conn.send(delta(A, filed, 10));
    await expect(wf(page, 'w1').locator('.wf-result-text')).toHaveText('OK');
    await expect(wf(page, 'w1').locator('.wf-logs > summary')).toHaveText('日志(1)');
    expect(callsFor(mock, 'w1')).toEqual([{ key: A, task_id: 'w1' }, { key: A, task_id: 'w1', since: '11', epoch: EPOCH }]);
    await mark(conn, mock, 'm2');
    expect(callsFor(mock, 'w1').length).toBe(2);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('a remote node session never calls the workflow endpoint', async ({ page }) => {
  const data = defaultSessions();
  data.sessions.push({ ...data.sessions[2], key: REMOTE, node: 'remote1', session_id: 'sess-remote', last_prompt: 'on remote1' });
  data.nodes = { local: { display_name: 'Local', status: 'ok' }, remote1: { display_name: 'Remote 1', status: 'ok' } };
  const { mock, conn, errors } = await boot(page, { sessions: data, workflows: { w6: fixture(view('w6', 9, [])) } });
  try {
    await page.click(`.session-card[data-key="${REMOTE}"][data-node="remote1"]`);
    await expect.poll(() => subs(conn, REMOTE)).toBe(1);
    conn.send(set(REMOTE, ['w6'], EPOCH, 'remote1'));
    conn.send(full(REMOTE, view('w6', 5, []), EPOCH, 'remote1'));
    conn.send({ ...delta(REMOTE, view('w6', 9, [row(0, 9, 'running')]), 7), node: 'remote1' });
    data.sessions[3].workflows = [{ task_id: 'w6', status: 'running', epoch: EPOCH, version: 12, counts: counts() }];
    await refreshed(page, conn, data, 'remote refreshed');
    await expect(panel(page)).toBeHidden();
    // Back on a local session, its fetch is the fence.
    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => subs(conn, A)).toBe(2);
    await mark(conn, mock, 'm1');
    expect(mock.workflowCalls.filter((q) => q.key === REMOTE)).toEqual([]);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});
