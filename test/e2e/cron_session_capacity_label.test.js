// @ts-check
//
// session_capacity：GetOrCreate 撞上 router 的会话上限（cron 命名空间 12 个），
// 这次 run 记成 skipped —— 是争用，不是任务本身的错（#3009）。dashboard 的
// cronErrorClassLabel 要给它中文标签，否则 default 分支把原始枚举串直接吐到
// 执行历史里。
//
// 跑法：cd test/e2e && npx playwright test cron_session_capacity_label.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function jobs() {
  const now = Date.now();
  return [{
    id: 'cron-001',
    schedule: '13 6 * * *',
    prompt: 'daily digest',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    last_run_at: now - 3600000,
    recent_runs: [{
      run_id: 'run-capacity01',
      state: 'skipped',
      started_at: now - 60 * 60 * 1000,
      ended_at: now - 60 * 60 * 1000 + 200,
      duration_ms: 200,
      trigger: 'cron',
      error_class: 'session_capacity',
    }],
    stats: { total: 1, succeeded: 0 },
  }];
}

test.describe('cron session_capacity 标签', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('并发上限跳过显示中文标签', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForSelector('#cron-timeline-panel .ctr');

    const errCls = page.locator('#cron-timeline-panel .ctr-errcls');
    await expect(errCls).toHaveText('并发上限跳过');
    const text = await page.locator('#cron-timeline-panel').innerText();
    expect(text, '原始枚举串不应出现在界面上').not.toContain('session_capacity');

    await ctx.close();
  });
});
