// @ts-check
//
// cronApplyRunStarted / cronApplyRunEnded 是 cron 面板的乐观更新路径：一个
// subsystem 为 cron 的 run_started 帧应当让那一行立刻显示「运行中」，不等列表
// 重新拉取。帧经 wsm.onMessage 进分发表，由 cron_view 自己注册的认领接住
// （owner_id 在那里投影成 job_id）；run_frames_unified.test.js 走真实的 WS
// 连接推同样的帧，本 spec 不连 WS，直接同步调用分发入口。
//
// 同步调用的好处是 handler 抛出的异常会让 page.evaluate 直接失败，而不是被
// onmessage 的 try/catch 吞成一条 console.error；pageerror 另外兜住异步路径。
//
// 另外两例守认领本身：别的 subsystem 的帧即使 owner_id 撞上 cron job id 也
// 不归 cron（认领判据只认 subsystem），以及 run_ended 在抽屉开着时刷新该 job
// 的 timeline 头（limit=10 的 runs 请求只有这条路径会发）。
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

test.describe('cron 面板的 run 帧乐观更新', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('run_started 让该行立刻进入运行中，run_ended 撤回', async ({ browser }) => {
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
      wsm.onMessage({
        type: 'run_started',
        subsystem: 'cron',
        owner_id: 'cron-bus-1',
        run_id: 'run-bus-1',
        started_at: Date.now(),
        trigger: 'cron',
        session_id: 'sess-bus-1',
      });
    });

    await expect(row, 'run_started 后该行应显示运行中').toHaveClass(/is-running/);
    expect(pageErrors, 'cronApplyRunStarted 不应抛异常').toEqual([]);

    await page.evaluate(() => {
      wsm.onMessage({
        type: 'run_ended',
        subsystem: 'cron',
        owner_id: 'cron-bus-1',
        run_id: 'run-bus-1',
        state: 'succeeded',
        ended_at: Date.now(),
        duration_ms: 1200,
        trigger: 'cron',
      });
    });

    await expect(row, 'run_ended 后运行中标记应撤回').not.toHaveClass(/is-running/);
    expect(pageErrors, 'cronApplyRunEnded 不应抛异常').toEqual([]);

    await ctx.close();
  });

  test('别的 subsystem 的 run 帧不碰 cron 行', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.locator('.cj-row[data-cron-id="cron-bus-1"]').waitFor();

    // 同一次 evaluate 里推帧并读 class：cronApplyRunStarted 同步重绘，
    // 认领一旦放进这帧，返回值就是 true，不存在"还没画出来"的空过。
    const lit = await page.evaluate(() => {
      wsm.onMessage({
        type: 'run_started', subsystem: 'scratch', owner_id: 'cron-bus-1',
        run_id: 'run-foreign-1', started_at: Date.now(), trigger: 'cron',
      });
      const el = document.querySelector('.cj-row[data-cron-id="cron-bus-1"]');
      return !!el && el.classList.contains('is-running');
    });
    expect(lit, 'subsystem 不是 cron 的帧不应被 cron 认领').toBe(false);

    await ctx.close();
  });

  test('run_ended 刷新已打开抽屉的 timeline 头', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.locator('.cj-row[data-cron-id="cron-bus-1"]').click();
    await expect(page.locator('.cron-detail-pane.is-open')).toBeVisible();

    const head = page.waitForRequest((r) => r.url().includes('/api/cron/runs?job_id=cron-bus-1&limit=10'));
    await page.evaluate(() => {
      wsm.onMessage({
        type: 'run_ended', subsystem: 'cron', owner_id: 'cron-bus-1', run_id: 'run-bus-2',
        state: 'failed', ended_at: Date.now(), duration_ms: 800, trigger: 'cron',
      });
    });
    await head;

    await ctx.close();
  });
});
