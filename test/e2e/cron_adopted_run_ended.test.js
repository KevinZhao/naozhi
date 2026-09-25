// @ts-check
//
// A run adopted across a restart (#2712) reaches the dashboard in an order no
// local run produces: the list API already shows its current_run (the
// reconciler populated the gate slot at boot), but this page never received a
// run_started for it — that frame went out, if at all, from the previous
// process. When the adopted turn settles, the backend now ends it through
// finishRun (#2799), which sends a run_ended frame. The row must leave its
// running state on that frame alone, with no matching run_started ever seen.
//
// "Alone" is enforced, not hoped for: run_ended also triggers a list refetch,
// and a refetch returning the settled job would clear the row even if frame
// handling were broken. Every list response is held 2.5s, and the assertion
// window is 1.5s, so only the frame can have cleared it.
//
// 跑法：cd test/e2e && npx playwright test cron_adopted_run_ended.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function adoptedJob() {
  const now = Date.now();
  return {
    id: 'cron-adopt-1',
    schedule: '0 6 * * *',
    prompt: 'long job adopted across a restart',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
    // What the list API returns right after a boot that adopted this run.
    current_run: { run_id: 'run-adopt-1', started_at: now - 90000, phase: 'sending', trigger: 'cron' },
  };
}

test('adopted run: run_ended with no prior run_started clears the running row', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [adoptedJob()], compactCronListDelayMs: 2500 });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));

  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  const row = page.locator('.cj-row[data-cron-id="cron-adopt-1"]');
  await expect(row, 'the list payload alone must show the adopted run as running').toHaveClass(/is-running/, { timeout: 8000 });

  await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBeGreaterThan(0);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  // The backend state after the settle: no current_run any more.
  const settled = adoptedJob();
  delete settled.current_run;
  mock.setCronJobs([settled]);
  conn.send({
    type: 'run_ended', subsystem: 'cron', owner_id: 'cron-adopt-1',
    run_id: 'run-adopt-1', state: 'succeeded', started_at: Date.now() - 90000,
    ended_at: Date.now(), duration_ms: 90000, trigger: 'cron',
  });

  await expect(row, 'run_ended must clear the row even though its run_started was never seen')
    .not.toHaveClass(/is-running/, { timeout: 1500 });
  expect(pageErrors, 'an unpaired run_ended must not throw').toEqual([]);

  await ctx.close();
  mock.server.close();
});
