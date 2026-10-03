// @ts-check
//
// 连续失败自动暂停（#3009）：job 的 paused_reason=auto_failures 时，列表行显示
// "已自动暂停"，抽屉写明连续失败次数（编辑后计数清零则不写次数）；手动暂停的
// job 仍是"已暂停"。
//
// 跑法：cd test/e2e && npx playwright test cron_auto_pause_label.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function jobs() {
  const now = Date.now();
  const base = { schedule: '13 6 * * *', work_dir: '/home/user/workspace/myproject', created_at: now - 86400000, paused: true };
  return [
    { ...base, id: 'cron-auto', prompt: 'broken digest', paused_reason: 'auto_failures', consecutive_failures: 5 },
    { ...base, id: 'cron-edited', prompt: 'edited digest', paused_reason: 'auto_failures' },
    { ...base, id: 'cron-manual', prompt: 'held digest' },
  ];
}

test.describe('cron 自动暂停标签', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('列表与抽屉区分自动暂停和手动暂停', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');

    await expect(page.locator('.cj-row[data-cron-id="cron-auto"] .cj-when')).toHaveText('已自动暂停');
    await expect(page.locator('.cj-row[data-cron-id="cron-manual"] .cj-when')).toHaveText('已暂停');

    await page.click('.cj-row[data-cron-id="cron-auto"]');
    await expect(page.locator('.css-when-paused')).toHaveText('连续失败 5 次，已自动暂停 · 恢复后排期');

    await page.click('.cj-row[data-cron-id="cron-edited"]');
    await expect(page.locator('.css-when-paused')).toHaveText('已自动暂停 · 恢复后排期');
    await ctx.close();
  });
});
