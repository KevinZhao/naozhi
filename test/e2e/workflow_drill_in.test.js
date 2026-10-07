// @ts-check
//
// Workflow agent drill-in (docs/rfc/workflow-dashboard.md §8.4, §11.4b): a
// row of the workflow panel opens its agent's transcript in the agent view.
//  - one click, one switchTo carrying the label and a "<workflow> · <phase>"
//    crumb, one agent_events request; the row is marked while drilled in;
//    a tailer's replay of what the page showed is not shown twice; Esc
//    goes back and moves the mark off;
//  - an agent whose transcript is not on disk yet (202 pending) opens by
//    itself once it is;
//  - a WS subscribe rejected as pending falls back to the 3s HTTP poll;
//  - the attempt badge unfolds the earlier attempts, each drilling in.
//
// Run: cd test/e2e && npx playwright test workflow_drill_in.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const EPOCH = '00000000000000a1';
const T0 = 1791170018848;

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

const counts = (o = {}) => ({ total: 0, queued: 0, running: 0, done: 0, failed: 0, skipped: 0, stopped: 0, ...o });
const row = (index, state, over = {}) => ({
  index, phase_index: 1, label: 'agent ' + index, state, rev: 2, ...(state !== 'queued' && { agent_id: 'a' + index }), ...over,
});
function view(id, agents) {
  const c = counts();
  for (const a of agents) { c.total++; c[a.state]++; }
  return {
    task_id: id, name: 'wf ' + id, status: 'running', source: 'stream', version: 3, started_at: Date.now() - 60000,
    counts: c, phases: [{ index: 1, title: 'Ask', counts: c }], agents, tokens: 1200, tool_calls: 3,
  };
}
const text = (t, s) => ({ time: t, type: 'text', summary: s, detail: s });

// open boots the dashboard on session A with workflow w1 (rows as given)
// pushed and open, and records every switchTo the panel makes.
async function open(page, agents, overrides = {}) {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const w1 = view('w1', agents);
  const mock = await startMockServer({ ws: true, workflows: { w1: { epoch: EPOCH, version: 3, workflow: w1 } }, ...overrides });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click(`.session-card[data-key="${A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
  conn.send({ type: 'workflow_set', key: A, epoch: EPOCH, task_ids: ['w1'], server_now: Date.now() });
  conn.send({ type: 'workflow_state', key: A, task_id: 'w1', epoch: EPOCH, version: 3, full: true, server_now: Date.now(), workflow: { ...w1, agents: [] } });
  await expect(page.locator('#workflow-panel .wf-row')).toHaveCount(agents.length);
  await page.evaluate(() => {
    const w = /** @type {any} */ (window);
    const av = w.nz.views.agent;
    const orig = av.switchTo;
    w.switchCalls = [];
    av.switchTo = (/** @type {any[]} */ ...args) => { w.switchCalls.push(args); return orig(...args); };
  });
  return { mock, conn, errors };
}

const rowOf = (page, i) => page.locator(`#workflow-panel .wf-row[data-index="${i}"]`);
const switchCalls = (page) => page.evaluate(() => /** @type {any} */ (window).switchCalls);
const agentCalls = (mock, id) => mock.agentEventsCalls.filter((q) => q.task_id === id);
const subscribes = (conn, id) => conn.messages.filter((m) => m.type === 'agent_subscribe' && m.task_id === id);
const bubbles = (page) => page.locator('#events-scroll > .event');

test('a row drills in once with a crumb; the replay is not shown twice; Esc goes back', async ({ page }) => {
  const { mock, conn, errors } = await open(page, [row(1, 'running'), row(2, 'queued')], {
    agentEvents: { a1: [text(T0, 'Reply with just the number 2+2')] },
  });
  try {
    await rowOf(page, 1).locator('.wf-row-btn').click();
    await expect(bubbles(page).filter({ hasText: 'Reply with just the number 2+2' })).toHaveCount(1);
    expect(await switchCalls(page)).toEqual([['a1', { label: 'agent 1', crumb: 'wf w1 · Ask' }]]);
    expect(agentCalls(mock, 'a1').length).toBe(1);
    await expect(page.locator('#bc-agent-name')).toHaveText('agent 1');
    await expect(page.locator('#bc-agent-team')).toHaveText('wf w1 · Ask');
    await expect(rowOf(page, 1)).toHaveClass(/wf-sel/);
    await expect(rowOf(page, 1).locator('.wf-row-btn')).toHaveAttribute('aria-current', 'true');
    await expect.poll(() => subscribes(conn, 'a1').length).toBe(1);

    // The tailer starts at the transcript's top: its first frame repeats the page.
    for (const ev of [text(T0, 'Reply with just the number 2+2'), text(T0 + 2000, '4')]) {
      conn.send({ type: 'agent_event', key: A, task_id: 'a1', event: ev });
    }
    await expect(bubbles(page).filter({ hasText: /^4$/ })).toHaveCount(1);
    await expect(bubbles(page).filter({ hasText: 'Reply with just the number 2+2' })).toHaveCount(1);
    conn.send({ type: 'agent_done', key: A, task_id: 'a1', status: 'completed' });
    await expect(page.locator('#bc-agent-stat')).toHaveText('completed');

    // A queued row is no button: nothing to click.
    await expect(rowOf(page, 2).locator('button')).toHaveCount(0);

    await page.keyboard.press('Escape');
    await expect(page.locator('#agent-breadcrumb')).toBeHidden();
    await expect(rowOf(page, 1)).not.toHaveClass(/wf-sel/);
    await expect(rowOf(page, 1).locator('.wf-row-btn')).not.toHaveAttribute('aria-current', 'true');
    expect((await switchCalls(page)).length).toBe(1);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('an agent whose transcript is not written yet opens by itself after 202s', async ({ page }) => {
  const { mock, conn, errors } = await open(page, [row(1, 'running')], {
    agentEvents: { a1: [text(T0, 'the task')] },
    agentEventsPending: { a1: 3 },
  });
  try {
    await rowOf(page, 1).locator('.wf-row-btn').click();
    await expect(bubbles(page).filter({ hasText: 'the task' })).toHaveCount(1);
    expect(agentCalls(mock, 'a1').length).toBe(4);
    expect((await switchCalls(page)).length).toBe(1);
    await expect.poll(() => subscribes(conn, 'a1').length).toBe(1);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('a subscribe rejected as pending falls back to the 3s HTTP poll', async ({ page }) => {
  const { mock, conn, errors } = await open(page, [row(1, 'running')], {
    agentEvents: { a1: [text(T0, 'the task')] },
  });
  try {
    await rowOf(page, 1).locator('.wf-row-btn').click();
    await expect.poll(() => subscribes(conn, 'a1').length).toBe(1);
    conn.send({ type: 'agent_subscribe_rejected', key: A, task_id: 'a1', reason: 'pending' });
    await expect.poll(() => agentCalls(mock, 'a1').map((q) => q.after), { timeout: 6000 }).toEqual([undefined, String(T0)]);
    await expect(page.locator('#agent-breadcrumb')).toBeVisible();
    await expect(bubbles(page).filter({ hasText: 'the task' })).toHaveCount(1);
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});

test('the attempt badge lists the earlier attempts, each drilling in', async ({ page }) => {
  const retried = row(1, 'running', { attempt: 3, prev_agent_ids: ['a9', 'a8'] });
  const { mock, conn, errors } = await open(page, [retried, row(2, 'running')], {
    agentEvents: { a9: [text(T0, 'first try')], a1: [] },
  });
  try {
    const badge = rowOf(page, 1).locator('.wf-attempt-btn');
    await expect(badge).toHaveText('×3');
    await expect(badge).toHaveAttribute('aria-expanded', 'false');
    await expect(rowOf(page, 1).locator('.wf-attempts')).toBeHidden();
    await expect(rowOf(page, 2).locator('.wf-attempt-btn')).toHaveCount(0);
    await badge.click();
    await expect(badge).toHaveAttribute('aria-expanded', 'true');
    const items = rowOf(page, 1).locator('.wf-attempt-item');
    await expect(items).toHaveText(['第 1 次', '第 2 次']);
    await items.first().click();
    await expect(bubbles(page).filter({ hasText: 'first try' })).toHaveCount(1);
    expect(await switchCalls(page)).toEqual([['a9', { label: 'agent 1（第 1 次）', crumb: 'wf w1 · Ask' }]]);
    await expect(items.first()).toHaveAttribute('aria-current', 'true');
    await expect(rowOf(page, 1).locator('.wf-row-btn')).not.toHaveAttribute('aria-current', 'true');
    await expect(rowOf(page, 1)).toHaveClass(/wf-sel/);
    expect(agentCalls(mock, 'a9').length).toBe(1);

    // The list stays unfolded when the row is rebuilt for a new state.
    const ended = view('w1', [{ ...retried, state: 'done', rev: 4 }]);
    conn.send({ type: 'workflow_state', key: A, task_id: 'w1', epoch: EPOCH, version: 4, base_version: 3, full: false, server_now: Date.now(), workflow: { ...ended, version: 4 } });
    await expect(rowOf(page, 1).locator('.sr-only').first()).toHaveText('已完成');
    await expect(rowOf(page, 1).locator('.wf-attempts')).toBeVisible();
    await expect(badge).toHaveAttribute('aria-expanded', 'true');
    await expect(items.first()).toHaveAttribute('aria-current', 'true');
    await badge.click();
    await expect(rowOf(page, 1).locator('.wf-attempts')).toBeHidden();
    expect(errors).toEqual([]);
  } finally { mock.server.close(); }
});
