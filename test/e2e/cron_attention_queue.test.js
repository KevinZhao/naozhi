// @ts-check
//
// The §7.4 confirmation queue sits in every job's drawer, whichever job its
// cards name. cron_attention.js only fetches the queue (S20j, #3026); the
// callers repaint the timeline that shows it. This pins the replay caller:
// replaying a card that names another job than the open drawer's must clear
// the card, although the drawer's own run history has nothing to refresh.
// The drawer's open-time list refetch is held, so its repaint cannot stand
// in for the open caller's.
//
// 跑法：cd test/e2e && npx playwright test cron_attention_queue.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function job(id) {
  const now = Date.now();
  return {
    id,
    schedule: '0 6 * * *',
    prompt: 'sandbox job ' + id,
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  };
}

test("replaying another job's queued run clears its card from the open drawer", async ({ browser }) => {
  const mock = await startMockServer({
    cronJobs: [job('cron-open'), job('cron-other')],
    cronAttention: [
      { job_id: 'cron-other', run_id: 'bbbb2222cccc3333', reason: 'transport', job_label: 'other', created_at_ms: Date.now() - 60000 },
    ],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));

  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  const row = page.locator('.cj-row[data-cron-id="cron-open"]');
  await row.waitFor();
  // Hold every later list refetch past its 8 s timeout.
  await page.route((u) => u.pathname === '/api/cron' && u.searchParams.has('compact'), () => {});
  await row.click();

  const card = page.locator('.ctr-queue-card[data-run-id="bbbb2222cccc3333"]');
  await expect(card, 'the queue fetched on open reaches the drawer').toBeVisible({ timeout: 5000 });
  await card.locator('[data-action="cron-att-replay"]').click();
  await expect.poll(() => mock.cronReplayCalls, { timeout: 5000 }).toEqual([{ run_id: 'bbbb2222cccc3333', job_id: 'cron-other' }]);
  await expect(card).toHaveCount(0);
  await expect(page.locator('.ctr-queue')).toHaveCount(0);

  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});
