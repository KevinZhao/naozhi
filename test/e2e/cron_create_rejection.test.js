// @ts-check
//
// A create the server refuses with a cause of its own shows that cause, not
// the status-class advice: a full job table is a 409, and "状态冲突，请刷新后
// 重试" would send the operator to a refresh that changes nothing. The two
// messages are the ones internal/dashboard/cron writeAddUpdateRejection sends.
//
// 跑法：cd test/e2e && npx playwright test cron_create_rejection.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => {
  const now = Date.now();
  mock = await startMockServer({
    cronJobs: [{
      id: 'cron-rej-1', schedule: '0 6 * * *', prompt: 'existing job', paused: false,
      created_at: now - 86400000, next_run: now + 3600000, recent_runs: [], stats: { total: 0, succeeded: 0 },
    }],
  });
});
test.afterAll(() => mock.server.close());

for (const c of [
  { status: 409, error: 'cron job quota reached', want: '创建定时任务失败：定时任务数已达上限，请先删除不用的任务' },
  { status: 400, error: 'schedule interval below the 5m minimum', want: '创建定时任务失败：执行间隔不能短于 5 分钟' },
]) {
  test(`create refused with HTTP ${c.status} "${c.error}" names the cause`, async ({ page }) => {
    await page.route('**/api/cron', (route) => {
      if (route.request().method() !== 'POST') return route.fallback();
      return route.fulfill({ status: c.status, contentType: 'application/json', body: JSON.stringify({ error: c.error }) });
    });
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row[data-cron-id="cron-rej-1"]');
    await page.click('.cron-new-btn');
    await page.waitForSelector('.cron-modal');
    await page.evaluate(() => {
      const el = /** @type {HTMLInputElement|null} */ (document.getElementById('freq-advanced-input'));
      if (el) el.value = '0 9 * * *';
    });
    await page.fill('#cron-prompt', 'refused job');
    await page.click('[data-action="cron-create-save"]');

    const toast = page.locator('.toast', { hasText: '创建定时任务失败' });
    await expect(toast).toContainText(c.want);
    await expect(toast).toContainText(c.error);
    await expect(toast).not.toContainText('状态冲突');
    // A refused create leaves the form open for the operator to fix.
    await expect(page.locator('.cron-modal')).toBeVisible();
  });
}
