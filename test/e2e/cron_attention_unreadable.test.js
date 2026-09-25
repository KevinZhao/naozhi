// @ts-check
//
// An attention record the backend cannot read still blocks replay of its run
// (it fails closed), so the queue lists it as `unreadable` instead of hiding
// it (#2808). It names no job, and replay needs one, so its card offers the
// confirm action alone; confirming is how the operator clears it from the UI.
// A readable card beside it keeps both actions, which is the control that the
// withheld replay button is a property of the unreadable item and not of the
// queue.
//
// 跑法：cd test/e2e && npx playwright test cron_attention_unreadable.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function job() {
  const now = Date.now();
  return {
    id: 'cron-att-1',
    schedule: '0 6 * * *',
    prompt: 'sandbox job with side effects',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  };
}

test('unreadable attention record: listed, confirm-only, cleared by confirm', async ({ browser }) => {
  const now = Date.now();
  const mock = await startMockServer({
    cronJobs: [job()],
    cronAttention: [
      { job_id: 'cron-att-1', run_id: 'aaaa1111bbbb2222', reason: 'transport', job_label: 'nightly', created_at_ms: now - 60000 },
      { job_id: '', run_id: 'dead0000beef1111', reason: 'unreadable', unreadable: true, created_at_ms: now - 120000 },
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
  await page.locator('.cj-row[data-cron-id="cron-att-1"]').click();

  const bad = page.locator('.ctr-queue-card[data-run-id="dead0000beef1111"]');
  const good = page.locator('.ctr-queue-card[data-run-id="aaaa1111bbbb2222"]');
  await expect(bad, 'an unreadable record must reach the queue').toBeVisible({ timeout: 8000 });
  await expect(page.locator('.ctr-queue-title')).toContainText('2');
  await expect(bad.locator('.ctr-queue-reason')).toContainText('记录损坏');

  await expect(bad.locator('[data-action="cron-att-replay"]'), 'no replay for a record that names no job').toHaveCount(0);
  await expect(bad.locator('[data-action="cron-att-confirm"]')).toHaveCount(1);
  await expect(good.locator('[data-action="cron-att-replay"]'), 'control: a readable card keeps replay').toHaveCount(1);

  await bad.locator('[data-action="cron-att-confirm"]').click();
  await expect.poll(() => mock.cronConfirmCalls, { timeout: 5000 }).toEqual(['dead0000beef1111']);
  await expect(bad).toHaveCount(0);
  await expect(good).toBeVisible();

  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});
