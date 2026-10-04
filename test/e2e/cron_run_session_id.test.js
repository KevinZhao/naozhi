// @ts-check
//
// cron 执行历史展开详情底部的「会话 ID」行（#3256）。删除确认框告诉用户
// 「session_id 在执行历史的「详情」里能找到」，这里钉住那句话成立：
//
//  1. 有 session_id 的 run（失败的也算）展开后显示完整 id，点一下整段选中
//  2. 没有 session_id 的 run 不渲染这一行（不出现空的「会话 ID」）
//  3. 行本身（未展开）不显示 session_id
//
// 跑法：cd test/e2e && npx playwright test cron_run_session_id.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const FAILED_SID = '7f3c2a10-5b4e-4d6a-9c1e-2f8b0a7d6e51';
const OK_SID = '0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';

function jobs() {
  const now = Date.now();
  return [{
    id: 'cron-sid-1', schedule: '0 6 * * *', prompt: 'session id job', work_dir: '/home/user/workspace/myproject',
    paused: false, created_at: now - 86400000, next_run: now + 3600000,
    stats: { total: 3, succeeded: 1, failed: 2 },
    recent_runs: [
      { run_id: 'sid-run-fail', state: 'failed', started_at: now - 3600000, ended_at: now - 3590000, duration_ms: 10000, trigger: 'cron', session_id: FAILED_SID, error_class: 'turn_failed' },
      { run_id: 'sid-run-ok', state: 'succeeded', started_at: now - 7200000, ended_at: now - 7190000, duration_ms: 10000, trigger: 'cron', session_id: OK_SID },
      { run_id: 'sid-run-none', state: 'failed', started_at: now - 10800000, ended_at: now - 10790000, duration_ms: 10000, trigger: 'cron', error_class: 'network' },
    ],
  }];
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test.describe('cron run 详情的会话 ID', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('有 id 的 run 展示全长 id，无 id 的不渲染', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.click('.cj-row[data-cron-id="cron-sid-1"]');
    await page.waitForSelector('#cron-timeline-panel .ctr');

    // 折叠态的行不带 session_id。
    await expect(page.locator('#cron-timeline-panel')).not.toContainText(FAILED_SID);

    const failRow = page.locator('.ctr[data-run-id="sid-run-fail"]');
    await failRow.click();
    await expect(failRow.locator('.ctr-final.err')).toBeVisible();
    const code = failRow.locator('.ctr-detail .ctr-session-row code');
    await expect(code).toHaveText(FAILED_SID);
    const sel = await code.evaluate((el) => getComputedStyle(el).userSelect);
    expect(sel, '点一下要整段选中，方便复制').toBe('all');

    const okRow = page.locator('.ctr[data-run-id="sid-run-ok"]');
    await okRow.locator('.ctr-main').click();
    await expect(okRow.locator('.ctr-detail .ctr-session-row code')).toHaveText(OK_SID);

    const noneRow = page.locator('.ctr[data-run-id="sid-run-none"]');
    await noneRow.locator('.ctr-main').click();
    await expect(noneRow.locator('.ctr-final.err')).toBeVisible();
    await expect(noneRow.locator('.ctr-session-row')).toHaveCount(0);

    await ctx.close();
  });
});
