// @ts-check
//
// The sidebar's workflow badge (docs/rfc/workflow-dashboard.md §14 PR-15):
//  - a local card whose session runs workflows shows "⚙ done/total" summed
//    over the running and paused ones, its title one line per workflow;
//    ended workflows show none;
//  - a remote node's card shows none, whatever its snapshot carries (NG3);
//  - the open session's card hides it;
//  - over a live socket a sessions_update refreshes it once stats.version
//    has moved: the server's board bumps the version for a count change at
//    most 30 s later and for a terminal status at once
//    (TestWorkflowSnapshotRefresh), and the terminal status removes the badge.
//
// Run: cd test/e2e && npx playwright test sidebar_workflow_badge.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120002-3:myproject';
const RUNNING = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const REMOTE = 'dashboard:direct:2026-01-01-115900-r:general';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

// wf is a workflow Summary as /api/sessions carries it.
function wf(taskID, name, status, done, total) {
  return {
    task_id: taskID, name, status, epoch: '00000000000000a1', version: 1, started_at: Date.now() - 60000,
    counts: { total, queued: total - done, running: 0, done, failed: 0, skipped: 0, stopped: 0 },
  };
}

function fixture() {
  const data = defaultSessions();
  const [a, running, b] = data.sessions;
  a.workflows = [wf('w1', 'probe', 'running', 2, 8), wf('w0', 'earlier', 'completed', 3, 3)];
  running.workflows = [wf('w2', 'fan-out', 'running', 1, 2), wf('w3', 'held', 'paused', 0, 3)];
  b.workflows = [wf('w4', 'finished', 'completed', 4, 4), wf('w5', 'lost', 'unknown', 0, 1)];
  data.sessions.push({
    ...b, key: REMOTE, node: 'remote1', session_id: 'sess-remote', last_prompt: 'on remote1',
    workflows: [wf('w6', 'remote run', 'running', 1, 4)],
  });
  data.nodes = { local: { display_name: 'Local', status: 'ok' }, remote1: { display_name: 'Remote 1', status: 'ok' } };
  return data;
}

const badge = (page, key, node = 'local') => page.locator(`.session-card[data-key="${key}"][data-node="${node}"] .sc-wf`);

test('running workflows badge local cards; ended ones and remote cards get none', async ({ page }) => {
  const mock = await startMockServer({ sessions: fixture() });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${REMOTE}"]`);
    await expect(badge(page, A)).toHaveText('⚙ 2/8');
    await expect(badge(page, A)).toHaveAttribute('title', 'probe · 2/8');
    await expect(badge(page, RUNNING)).toHaveText('⚙ 1/5');
    await expect(badge(page, RUNNING)).toHaveAttribute('title', 'fan-out · 1/2\nheld · 0/3');
    await expect(badge(page, B)).toHaveCount(0);
    await expect(badge(page, REMOTE, 'remote1')).toHaveCount(0);

    await page.click(`.session-card[data-key="${A}"]`);
    await expect(page.locator(`.session-card.active[data-key="${A}"]`)).toHaveCount(1);
    await expect(badge(page, A)).toBeHidden();
    await expect(badge(page, RUNNING)).toBeVisible();
  } finally { mock.server.close(); }
});

test('over a live socket the badge follows stats.version; a terminal status removes it', async ({ page }) => {
  const mock = await startMockServer({ ws: true, sessions: fixture() });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(badge(page, A)).toHaveText('⚙ 2/8');
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    // The connect poll lands 300 ms after the socket opens; let it go first.
    await expect.poll(() => mock.sessionsGetCalls).toBeGreaterThanOrEqual(2);

    // Same version: the poll short-circuits and the card keeps its counts.
    mock.setSessionWorkflows(A, [wf('w1', 'probe', 'running', 5, 8)], false);
    expect(await page.evaluate(() => fetchSessions()), 'an unchanged version was applied').toBeUndefined();
    expect(await badge(page, A).textContent()).toBe('⚙ 2/8');

    // The count emission's version bump repaints it.
    mock.setSessionWorkflows(A, [wf('w1', 'probe', 'running', 6, 8)]);
    conn.send({ type: 'sessions_update' });
    await expect(badge(page, A)).toHaveText('⚙ 6/8');

    const polls = mock.sessionsGetCalls;
    mock.setSessionWorkflows(A, [wf('w1', 'probe', 'completed', 8, 8)]);
    conn.send({ type: 'sessions_update' });
    await expect.poll(() => mock.sessionsGetCalls).toBe(polls + 1);
    await expect(badge(page, A)).toHaveCount(0);
    await expect(badge(page, RUNNING)).toHaveText('⚙ 1/5');
  } finally { mock.server.close(); }
});
