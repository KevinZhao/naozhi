// @ts-check
//
// 统一 run 帧（#2540）：wire 上只有 run_started / run_ended 两种 run 帧，
// subsystem 字段区分来源。本 spec 从 mock 的 WS 端真的推帧进来，覆盖两条
// 分发路径 —— cron（投影 owner_id → job_id 后走 nz.bus，驱动列表行的
// 运行中标记）与 sysession（触发系统面板的 daemons 拉取，点亮后台失败
// 徽章的唯一路径）。cron_run_started_bus.test.js 覆盖的是 bus 之后的
// 乐观更新；本 spec 覆盖的是 wire 到 bus 之间那一段，两者相接。
//
// 跑法：cd test/e2e && npx playwright test run_frames_unified.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

function jobs() {
  return [{
    id: 'cron-wire-1',
    schedule: '0 6 * * *',
    prompt: 'wire driven run',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: Date.now() - 86400000,
    next_run: Date.now() + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  }];
}

test.describe('统一 run 帧的 WS 分发', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true, cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  test('cron 来源：run_started/run_ended 驱动列表行的运行中标记', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));

    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-wire-1"]');
    await row.waitFor();
    await expect(row).not.toHaveClass(/is-running/);

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({
      type: 'run_started', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-wire-1', started_at: Date.now(), trigger: 'cron', session_id: 'sess-w1',
    });
    // 投影层（owner_id → job_id）坏掉时这里不会变 running —— 这正是本断言的靶子。
    await expect(row).toHaveClass(/is-running/);

    conn.send({
      type: 'run_ended', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-wire-1', state: 'succeeded', started_at: Date.now() - 1200,
      ended_at: Date.now(), duration_ms: 1200, trigger: 'cron',
    });
    await expect(row).not.toHaveClass(/is-running/);
    expect(pageErrors, '分发与投影不应抛异常').toEqual([]);

    await ctx.close();
  });

  test('sysession 来源：run_ended 触发系统面板的 daemons 拉取', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);

    // 后台 daemon 失败时，这个 fetch 是点亮系统徽章的唯一路径 —— 断言请求
    // 本身而不是渲染结果，这样 mock 不需要实现该端点（fetchJSON 会吞 404）。
    const daemonsFetch = page.waitForRequest(
      (req) => req.url().includes('/api/system/daemons'),
      { timeout: 5000 }
    );
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({
      type: 'run_ended', subsystem: 'sysession', owner_id: 'autotitler',
      run_id: 'run-daemon-1', state: 'failed', duration_ms: 800,
      error_class: 'spawn_failed', trigger: 'tick',
    });
    await daemonsFetch;

    await ctx.close();
  });
});
