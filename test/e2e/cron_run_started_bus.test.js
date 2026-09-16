// @ts-check
//
// cronApplyRunStarted / cronApplyRunEnded 是 cron 面板的乐观更新路径：一个
// cron:run-started 总线事件应当让那一行立刻显示「运行中」，不等列表重新拉取。
// 这两个 handler 此前没有任何 e2e 覆盖 —— 它们只由 nz.bus 驱动，而没有 spec
// 派发过那两个事件，所以 handler 里抛异常也不会有测试变红。
//
// 本 spec 补上这条路径，同时钉住 handler 只依赖已导入的绑定：wsm 是从
// dashboard.js import 进来的对象，handler 直接读 wsm.cronLive。若它在运行时
// 不可达，handler 会抛，badge 永不出现，本测试就红 —— 这正是删掉那几处
// `typeof wsm !== 'undefined'` 死守卫之后需要有人看着的行为。
//
// 跑法：cd test/e2e && npx playwright test cron_run_started_bus.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

function jobs() {
  return [{
    id: 'cron-bus-1',
    schedule: '0 6 * * *',
    prompt: 'bus driven run',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: Date.now() - 86400000,
    next_run: Date.now() + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  }];
}

test.describe('cron 面板的总线乐观更新', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('cron:run-started 让该行立刻进入运行中，run-ended 撤回', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();

    // 任何未捕获异常都算失败：handler 抛出会让 badge 不出现，但先把真正的
    // 报错捞出来，否则只能看到一个"元素没出现"的超时。
    /** @type {string[]} */
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));

    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-bus-1"]');
    await row.waitFor();
    await expect(row, '初始状态不应是运行中').not.toHaveClass(/is-running/);

    await page.evaluate(() => {
      window.nz.bus.dispatchEvent(new CustomEvent('cron:run-started', {
        detail: {
          job_id: 'cron-bus-1',
          run_id: 'run-bus-1',
          started_at: Date.now(),
          trigger: 'cron',
          session_id: 'sess-bus-1',
        },
      }));
    });

    await expect(row, 'run-started 后该行应显示运行中').toHaveClass(/is-running/);
    expect(pageErrors, 'cronApplyRunStarted 不应抛异常').toEqual([]);

    await page.evaluate(() => {
      window.nz.bus.dispatchEvent(new CustomEvent('cron:run-ended', {
        detail: {
          job_id: 'cron-bus-1',
          run_id: 'run-bus-1',
          state: 'success',
          ended_at: Date.now(),
          duration_ms: 1200,
          trigger: 'cron',
        },
      }));
    });

    await expect(row, 'run-ended 后运行中标记应撤回').not.toHaveClass(/is-running/);
    expect(pageErrors, 'cronApplyRunEnded 不应抛异常').toEqual([]);

    await ctx.close();
  });
});
