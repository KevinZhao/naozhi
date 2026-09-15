// @ts-check
//
// interrupted 与 canceled 都落成 RunState=canceled；区别是谁中止的。canceled
// 是操作员点的，interrupted 是进程自己没了（优雅关闭超出 drain 预算，或被硬
// 杀），由启动时的 reconcileRunInflight 补记（Epic H #2546 Phase 0）。
//
// dashboard 的 cronErrorClassLabel 此前没有 interrupted 分支，default 分支把
// 原始枚举串直接吐出来 —— 操作员看到英文 "interrupted"，还容易读成自己点过
// 取消。#2711 步 1。
//
// 跑法：cd test/e2e && npx playwright test cron_interrupted_label.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

// 两条 canceled：一条是操作员取消，一条是进程中断。它们的 state 相同，只有
// error_class 不同 —— 正是本测试要求 UI 区分开的那一对。
function runs() {
  const now = Date.now();
  return [
    {
      run_id: 'run-interrupted1',
      state: 'canceled',
      started_at: now - 1 * 60 * 60 * 1000,
      ended_at: now - 1 * 60 * 60 * 1000 + 5000,
      duration_ms: 5000,
      trigger: 'cron',
      error_class: 'interrupted',
    },
    {
      run_id: 'run-canceled1',
      state: 'canceled',
      started_at: now - 2 * 60 * 60 * 1000,
      ended_at: now - 2 * 60 * 60 * 1000 + 3000,
      duration_ms: 3000,
      trigger: 'manual',
      error_class: 'canceled',
    },
  ];
}

function jobs() {
  return [{
    id: 'cron-001',
    schedule: '13 6 * * *',
    prompt: 'daily digest',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: Date.now() - 86400000,
    next_run: Date.now() + 3600000,
    last_run_at: Date.now() - 3600000,
    recent_runs: runs(),
    stats: { total: 2, succeeded: 0 },
  }];
}

test.describe('cron interrupted 标签', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('interrupted 显示中文且与「已取消」区分', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForSelector('#cron-timeline-panel .ctr');

    const panel = page.locator('#cron-timeline-panel');
    const text = await panel.innerText();

    // 原始枚举串泄漏到界面上是本测试要防的第一件事。
    expect(text, 'interrupted 的原始枚举串不应出现在界面上').not.toContain('interrupted');
    expect(text, 'interrupted 必须有中文标签').toContain('进程中断');
    // 第二件事：它不能和操作员取消混成同一句话。
    expect(text, '操作员取消仍应显示「已取消」').toContain('已取消');
    expect(await panel.innerText()).not.toContain('进程中断（未跑完）已取消');

    await ctx.close();
  });
});
