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
//  - rows_omitted and a version gap each fetch the rows since rowsAt;
//  - a queued row is no button; started, it becomes one with data-agent-id,
//    activated by Space; state labels are visually hidden;
//  - an unclaimed unknown workflow is listed and says so;
//  - switching away and back refetches each open workflow exactly once, also
//    when the first fetch landed after the switch; an unchanged WS reconnect
//    refetches nothing; /new's workflow_set empties it;
//  - a suspended subscription re-subscribes once the snapshot names a
//    protocol; a sessions_update over a live socket runs the fallback;
//  - polling (no socket) fetches a folded workflow header only;
//  - a remote node's session never calls the endpoint; a session gone from
//    the list takes its workflows with it; elapsed times tick between frames;
//  - the panel is no live region; an ending is announced once, and only for
//    the session on screen, also after switching to the other one;
//  - three open workflows leave the transcript in view; 400 agents render and
//    update without long tasks.
//
// Run: cd test/e2e && npx playwright test workflow_panel.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const REMOTE = 'dashboard:direct:2026-01-01-115900-r:general';
const EPOCH = '00000000000000a1';
const EPOCH2 = '00000000000000b2';

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

test('rows_omitted fetches the rows since rowsAt once; so does a version gap', async ({ page }) => {
  const w1 = view('w1', 9, [row(0, 9, 'running'), row(1, 9, 'running')]);
  const workflows = { w1: fixture(w1) };
  const { mock, conn } = await boot(page, { workflows });
  try {
    push(conn, A, [w1]);
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(1);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(2);

    workflows.w1 = fixture(view('w1', 10, [row(0, 10, 'running', { tokens: 500 }), row(1, 10, 'running', { tokens: 700 })]));
    conn.send(delta(A, view('w1', 10, [row(0, 10, 'running', { tokens: 500 })]), 9, { rows_omitted: 1 }));
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(2);
    await expect(wf(page, 'w1').locator('.wf-row[data-index="1"] .wf-stat')).toHaveText('700 tok · 0 tools');

    workflows.w1 = fixture(view('w1', 20, [row(0, 20, 'running'), row(1, 10, 'running')]));
    conn.send(delta(A, view('w1', 20, [row(0, 20, 'running')]), 15));
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(3);
    // Exactly these: no other fetch slipped in between.
    expect(callsFor(mock, 'w1').slice(1)).toEqual([
      { key: A, task_id: 'w1', since: '9', epoch: EPOCH }, { key: A, task_id: 'w1', since: '10', epoch: EPOCH },
    ]);
  } finally { mock.server.close(); }
});

test('a queued row is no button; started it becomes a Space-activated button; labels are hidden', async ({ page }) => {
  const w1 = view('w1', 9, [row(0, 9, 'queued'), row(1, 9, 'running')]);
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1) } });
  try {
    push(conn, A, [w1]);
    const r0 = wf(page, 'w1').locator('.wf-row[data-index="0"]');
    await expect(r0.locator('.wf-row-btn')).toHaveCount(1);
    expect(await r0.locator('.wf-row-btn').evaluate((n) => n.tagName)).toBe('DIV');
    await expect(r0.locator('[data-action]')).toHaveCount(0);
    const box = await r0.locator('.sr-only').boundingBox();
    expect(box && box.width <= 1 && box.height <= 1).toBe(true);
    await expect(r0.locator('.wf-glyph')).toHaveAttribute('aria-hidden', 'true');

    conn.send(delta(A, view('w1', 10, [row(0, 10, 'running', { agent_id: 'a0' })]), 9));
    const btn = r0.locator('button.wf-row-btn[data-agent-id="a0"]');
    await expect(btn).toHaveCount(1);
    await expect(btn).toHaveAttribute('data-action', 'wf-open-agent');
    await expect(r0.locator('.sr-only')).toHaveText('运行中');
    await btn.focus();
    await expect(btn).toBeFocused();
    await page.keyboard.press('Space');
    await expect(r0).toHaveClass(/wf-sel/);
    await expect(btn).toHaveAttribute('aria-current', 'true');
  } finally { mock.server.close(); }
});

test('an unclaimed unknown workflow is listed and says so', async ({ page }) => {
  const done = view('w0', 3, [row(0, 3, 'done')], { status: 'completed' });
  const lost = view('w9', 2, [], { status: 'unknown', raw_status: 'unclaimed', started_at: 0, counts: counts({ total: 1, queued: 1 }), phases: [] });
  const { mock, conn } = await boot(page, {});
  try {
    push(conn, A, [done, lost]);
    await expect(wf(page, 'w9')).toBeVisible();
    await expect(wf(page, 'w9').locator('.wf-chip')).toHaveText('状态未知（进程未认领）');
    await expect(wf(page, 'w9').locator('> summary')).toHaveAttribute('aria-label', 'Workflow wf w9 · 状态未知（进程未认领） · 0/1');
    // Not settled, so it lists before the ended one.
    await expect(page.locator('#workflow-panel > .wf').first()).toHaveAttribute('data-task-id', 'w9');
  } finally { mock.server.close(); }
});

test('back from another session: one refetch per open workflow; an unchanged reconnect, none; /new empties', async ({ page }) => {
  const w1 = view('w1', 9, [row(0, 9, 'running')]);
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1) }, workflowDelayMs: SLOW });
  try {
    push(conn, A, [w1]);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(1);
    await page.click(`.session-card[data-key="${B}"]`);
    await expect.poll(() => subs(conn, B)).toBe(1);
    await expect(panel(page)).toBeHidden();

    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => subs(conn, A)).toBe(2);
    push(conn, A, [w1]);
    await expect(wf(page, 'w1')).toHaveAttribute('open', '');
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(1);
    await mark(conn, mock, 'm1');
    expect(callsFor(mock, 'w1').length).toBe(2);

    conn.close();
    await expect.poll(() => mock.wsConnections.length, { timeout: 15000 }).toBe(2);
    const conn2 = mock.wsConnections[1];
    await expect.poll(() => subs(conn2, A), { timeout: 10000 }).toBe(1);
    push(conn2, A, [w1]);
    await mark(conn2, mock, 'm2');
    expect(callsFor(mock, 'w1').length).toBe(2);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(1);

    conn2.send(set(A, [], EPOCH2));
    await expect(panel(page)).toBeHidden();
    await expect(page.locator('#workflow-panel .wf')).toHaveCount(0);
  } finally { mock.server.close(); }
});

test('rows of a fetch that lands after a switch away are let go', async ({ page }) => {
  const w1 = view('w1', 9, [row(0, 9, 'running')]);
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1) }, workflowDelayMs: 800 });
  try {
    await page.evaluate(() => performance.clearResourceTimings());
    push(conn, A, [w1]);
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(1);
    await page.click(`.session-card[data-key="${B}"]`);
    // The answer has reached the page (its resource entry is there) after the switch.
    await expect.poll(() => page.evaluate(() => performance.getEntriesByType('resource')
      .filter((e) => e.name.includes('/api/sessions/workflow') && e.name.includes('task_id=w1')).length)).toBe(1);
    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => callsFor(mock, 'w1').length).toBe(2);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(1);
  } finally { mock.server.close(); }
});

test('a running workflow\'s elapsed time moves on between frames', async ({ page }) => {
  const w1 = view('w1', 9, [], { started_at: Date.now() - 65000 });
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1) } });
  try {
    push(conn, A, [w1]);
    const stat = wf(page, 'w1').locator('> summary .wf-stat');
    await expect(stat).toHaveText(/^1\.2k tok · 3 tools · 1m0[5-9]s$/);
    const before = await stat.textContent();
    await expect.poll(() => stat.textContent(), { timeout: 4000 }).not.toBe(before);
  } finally { mock.server.close(); }
});

test('a session gone from the list takes its workflows with it', async ({ page }) => {
  const data = defaultSessions();
  const { mock, conn } = await boot(page, { sessions: data });
  try {
    push(conn, B, [view('w2', 3, [], { status: 'completed' })]);
    const [gone] = data.sessions.splice(1, 1);
    data.stats.version++;
    conn.send({ type: 'sessions_update' });
    await expect(page.locator(`.session-card[data-key="${B}"]`)).toHaveCount(0);
    data.sessions.push(gone);
    data.stats.version++;
    conn.send({ type: 'sessions_update' });
    await page.click(`.session-card[data-key="${B}"]`);
    await expect.poll(() => subs(conn, B)).toBe(1);
    await expect(page.locator('#workflow-panel .wf')).toHaveCount(0);
  } finally { mock.server.close(); }
});

test('a suspended subscription re-subscribes once the snapshot names a protocol', async ({ page }) => {
  const data = defaultSessions();
  const w1 = view('w1', 3, []);
  const { mock, conn } = await boot(page, { sessions: data, workflows: { w1: fixture(w1) } });
  try {
    conn.send({ type: 'subscribed', key: A, state: 'ready', reason: 'suspended' });
    await refreshed(page, conn, data, 'no process yet');
    expect(subs(conn, A)).toBe(1);
    data.sessions[0].protocol = 'stream-json';
    await refreshed(page, conn, data, 'process back');
    await expect.poll(() => subs(conn, A)).toBe(2);
    // Another refresh while that subscribe is unanswered sends no third.
    await refreshed(page, conn, data, 'still pending');
    conn.send({ type: 'subscribed', key: A, state: 'ready' });
    push(conn, A, [w1]);
    await expect(wf(page, 'w1')).toBeVisible();
    // Upgraded, a later refresh does not subscribe again.
    await refreshed(page, conn, data, 'upgraded');
    // Switching to B is the fence: its subscribe follows any for A.
    await page.click(`.session-card[data-key="${B}"]`);
    await expect.poll(() => subs(conn, B)).toBe(1);
    expect(subs(conn, A)).toBe(2);
  } finally { mock.server.close(); }
});

test('over a live socket a sessions_update runs the fallback fetch', async ({ page }) => {
  const w3 = view('w3', 4, [row(0, 4, 'done')], { status: 'completed' });
  const { mock, conn } = await boot(page, { workflows: { w3: fixture(w3) } });
  try {
    mock.setSessionWorkflows(A, [{ task_id: 'w3', name: 'wf w3', status: 'completed', epoch: EPOCH, version: 4, counts: w3.counts }]);
    conn.send({ type: 'sessions_update' });
    await expect.poll(() => callsFor(mock, 'w3')).toEqual([{ key: A, task_id: 'w3', rows: 'none' }]);
    await expect(wf(page, 'w3')).toBeVisible();
    await expect(wf(page, 'w3')).not.toHaveAttribute('open', '');
  } finally { mock.server.close(); }
});

test('polling without a socket fetches a folded workflow header only', async ({ page }) => {
  test.setTimeout(45000);
  const data = defaultSessions();
  const sum = (version) => ({ task_id: 'w3', name: 'wf w3', status: 'completed', epoch: EPOCH, version, counts: counts({ total: 1, done: 1 }) });
  data.sessions[0].workflows = [sum(4)];
  const w3 = view('w3', 4, [row(0, 4, 'done')], { status: 'completed' });
  const workflows = { w3: fixture(w3) };
  const mock = await startMockServer({ sessions: data, workflows });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => callsFor(mock, 'w3').length, { timeout: 12000 }).toBe(1);
    workflows.w3 = fixture(view('w3', 6, [row(0, 6, 'done')], { status: 'completed' }));
    mock.setSessionWorkflows(A, [sum(6)]);
    await expect.poll(() => callsFor(mock, 'w3').length, { timeout: 12000 }).toBe(2);
    expect(callsFor(mock, 'w3')).toEqual([{ key: A, task_id: 'w3', rows: 'none' }, { key: A, task_id: 'w3', rows: 'none' }]);
    await expect(wf(page, 'w3')).not.toHaveAttribute('open', '');
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

test('no live region; an ending is announced once, only for the session on screen', async ({ page }) => {
  const w1 = view('w1', 3, [row(0, 3, 'running')], { name: 'probe' });
  const { mock, conn } = await boot(page, { workflows: { w1: fixture(w1), w7: fixture(view('w7', 3, [])) } });
  try {
    await page.evaluate(() => {
      const el = document.getElementById('sr-announce');
      /** @type {any} */ (window).__wfSaid = [];
      new MutationObserver(() => {
        if (el.textContent.startsWith('Workflow')) /** @type {any} */ (window).__wfSaid.push(el.textContent);
      }).observe(el, { childList: true, characterData: true, subtree: true });
    });
    const said = () => page.evaluate(() => /** @type {any} */ (window).__wfSaid);
    push(conn, A, [w1]);
    await expect(wf(page, 'w1')).toBeVisible();
    await expect(panel(page)).not.toHaveAttribute('aria-live', /.*/);
    await expect(wf(page, 'w1').locator('> summary .wf-stat')).toHaveAttribute('aria-hidden', 'true');
    await expect(wf(page, 'w1').locator('> summary .wf-counts')).toHaveAttribute('aria-hidden', 'true');
    const ended = (key, id, v, name = 'probe') => delta(key, view(id, v, [], { name, status: 'completed', counts: counts({ total: 1, done: 1 }) }), v - 1);
    conn.send(ended(A, 'w1', 4));
    await expect.poll(said).toEqual(['Workflow probe 已完成（1/1）']);
    conn.send(ended(A, 'w1', 5));

    // B is subscribed too (cron live does that) but not on screen.
    push(conn, B, [view('w2', 3, [row(0, 3, 'running')], { name: 'probe' })]);
    conn.send(ended(B, 'w2', 4));
    await page.click(`.session-card[data-key="${B}"]`);
    await expect.poll(() => subs(conn, B)).toBe(1);
    push(conn, B, [view('w2', 4, [], { name: 'probe', status: 'completed', counts: counts({ total: 1, done: 1 }) }), view('w7', 3, [], { name: 'last' })]);
    await expect(wf(page, 'w2')).toBeVisible();
    // w7 ending on screen is the fence: announcements go out in order.
    conn.send(ended(B, 'w7', 4, 'last'));
    await expect.poll(said).toEqual(['Workflow probe 已完成（1/1）', 'Workflow last 已完成（1/1）']);
  } finally { mock.server.close(); }
});

test('three open running workflows leave the transcript in view', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  const ws = ['w1', 'w2', 'w3'].map((id) => view(id, 9, Array.from({ length: 30 }, (_, i) => row(i, 9, 'running'))));
  const workflows = Object.fromEntries(ws.map((w) => [w.task_id, fixture(w)]));
  await page.addInitScript(() => sessionStorage.setItem('nz_wf_open', JSON.stringify({ w1: 1, w2: 1, w3: 1 })));
  const { mock, conn } = await boot(page, { workflows });
  try {
    push(conn, A, ws);
    await expect(page.locator('#workflow-panel .wf[open]')).toHaveCount(3);
    await expect(page.locator('#workflow-panel .wf-row')).toHaveCount(90);
    const p = await panel(page).boundingBox();
    const ev = await page.locator('#events-scroll').boundingBox();
    expect(p.height).toBeLessThanOrEqual(800 * 0.42 + 1);
    expect(ev.height).toBeGreaterThan(200);
    expect(ev.y).toBeGreaterThanOrEqual(p.y + p.height - 1);
  } finally { mock.server.close(); }
});

test('400 agents render and take updates without long tasks', async ({ page }) => {
  const rows = [];
  for (let i = 0; i < 400; i++) {
    rows.push(row(i, 9, i % 7 === 0 ? 'queued' : 'running', {
      phase_index: 1 + (i % 4), model: 'sonnet-5-5', tokens: 1000 + i, tool_calls: i % 9, last_tool: 'Grep', last_tool_summary: 'pattern in internal/' + 'x'.repeat(i % 40),
    }));
  }
  const w1 = view('w1', 9, rows);
  // Every animation-frame callback is timed: the panel paints in one.
  await page.addInitScript(() => {
    const w = /** @type {any} */ (window);
    const raf = w.requestAnimationFrame.bind(w);
    w.__raf = [];
    w.requestAnimationFrame = (cb) => raf((t) => { const t0 = performance.now(); cb(t); w.__raf.push(performance.now() - t0); });
  });
  const { mock, conn, errors } = await boot(page, { workflows: { w1: fixture(w1) } });
  try {
    await page.evaluate(() => {
      const w = /** @type {any} */ (window);
      w.__long = [];
      new PerformanceObserver((l) => { for (const e of l.getEntries()) w.__long.push(e.duration); }).observe({ type: 'longtask', buffered: false });
    });
    push(conn, A, [w1]);
    await expect(wf(page, 'w1').locator('.wf-row')).toHaveCount(240);
    // One delta touching every row (> 64 KiB: the mock's 64-bit length), then a stream of smaller ones.
    const all = rows.map((a) => ({ ...a, rev: 10, tokens: a.tokens + 1, ...(a.state === 'queued' && { state: 'running', agent_id: 'a' + a.index }) }));
    const big = delta(A, { ...w1, version: 10, agents: all }, 9);
    expect(JSON.stringify(big).length).toBeGreaterThan(65536);
    conn.send(big);
    await expect(wf(page, 'w1').locator('.wf-row[data-index="0"] button.wf-row-btn')).toHaveCount(1);
    for (let v = 11; v < 41; v++) {
      const some = all.filter((a) => a.index % 30 === v % 30).map((a) => ({ ...a, rev: v, tokens: 100000 + v }));
      conn.send(delta(A, { ...w1, version: v, agents: some }, v - 1));
      await page.evaluate(() => 0);
    }
    // Row 10 changed last at v40 (10 % 30 === 40 % 30).
    await expect(wf(page, 'w1').locator('.wf-row[data-index="10"] .wf-stat')).toHaveText(/^100\.0k tok/);
    await page.locator('#workflow-panel').evaluate((p) => { p.scrollTop = p.scrollHeight; });
    const long = await page.evaluate(() => /** @type {any} */ (window).__long);
    const raf = (await page.evaluate(() => /** @type {any} */ (window).__raf)).sort((x, y) => x - y);
    console.log('workflow 400-agent: long tasks (ms)', JSON.stringify(long), '; rAF callbacks', raf.length, 'p50/p95/max (ms)',
      [raf[raf.length >> 1], raf[Math.floor(raf.length * 0.95)], raf[raf.length - 1]].map((x) => x.toFixed(1)).join('/'));
    expect(long.filter((d) => d > 200)).toEqual([]);
    expect(callsFor(mock, 'w1').length).toBe(1);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});
