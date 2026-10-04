// @ts-check
//
// 统一 run 帧（#2540）：wire 上只有 run_started / run_ended 两种 run 帧，
// subsystem 字段区分来源。本 spec 从 mock 的 WS 端真的推帧进来，覆盖两条
// 分发路径 —— cron（cron_view 的认领把 owner_id 投影成 job_id，驱动列表行
// 的运行中标记）与 sysession（触发系统面板的 daemons 拉取，点亮后台失败
// 徽章的唯一路径）。cron_run_started_bus.test.js 不连 WS、同步调用
// wsm.onMessage；本 spec 覆盖的是从 socket 到分发表的整段。
//
// 跑法：cd test/e2e && npx playwright test run_frames_unified.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

// 模块级引用：mock 的 GET /api/cron 持续读这同一个数组，测试在推帧的同时
// 更新它 —— 真实后端在 run_started 之后的 list 响应必然带 current_run，
// 不这么做的话，一个在飞的 fetchCronJobs 会用"无 current_run"的旧数据把
// 乐观补丁冲掉（本 spec 第一版正是这样间歇性假红的，产品行为没有错）。
const JOBS = jobs();

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

test.describe('乐观补丁 vs 迟到的旧 list 响应', () => {
  // 决定性复现 #2540 PR2 修的竞态：list 响应生成于 run_started 帧之前、
  // 落地于其后（compactCronListDelayMs 拉开窗口），旧世界观整体替换
  // cronJobs，运行中徽章闪没。修复后：比 fetch 发起更新的本地补丁在
  // merge 时被保留。
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  const JOBS2 = jobs();
  test.beforeAll(async () => {
    mock = await startMockServer({ ws: true, cronJobs: JOBS2, compactCronListDelayMs: 400 });
  });
  test.afterAll(() => mock.server.close());

  test('迟到 400ms 的旧响应不得冲掉更新的 run_started 补丁', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-wire-1"]');
    await row.waitFor();

    // 帧在 click 触发的 list 响应仍在飞（延迟 400ms）时到达。
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({
      type: 'run_started', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-stale-1', started_at: Date.now(), trigger: 'cron',
    });
    await expect(row).toHaveClass(/is-running/);
    // 关键断言：等过延迟窗口，旧响应落地之后徽章仍在。
    await page.waitForTimeout(700);
    await expect(row).toHaveClass(/is-running/);
    await ctx.close();
  });
});

test.describe('迟到的旧响应不得把新 run 换回刚结束的那次', () => {
  // #3116：list 响应生成于 run_ended(old) / run_started(new) 之前、落地于其后，
  // 带着 old 的 current_run。两边都有 current_run，只比"有无"的保护会放行，
  // 抽屉的 run id、运行时长和 live 订阅的 after= 都回到 old。mock 的 list
  // 故意停在 old 不更新，模拟一个慢 list 事务。
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  const JOBS3 = jobs();
  JOBS3[0].current_run = { run_id: 'run-old', started_at: Date.now() - 3600000, phase: 'running', trigger: 'cron' };
  test.beforeAll(async () => {
    mock = await startMockServer({ ws: true, cronJobs: JOBS3, compactCronListDelayMs: 400 });
  });
  test.afterAll(() => mock.server.close());

  test('run_ended(old) 后的重拉带回 old 时保留 new', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-wire-1"]');
    await expect(row).toHaveClass(/is-running/);

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    const listsBefore = mock.cronListGetCount;
    // 只有此后发出的 list 请求带这个 prompt：页面上看到它就是旧响应已合并。
    JOBS3[0].prompt = 'stale list marker';
    conn.send({
      type: 'run_ended', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-old', state: 'succeeded', started_at: JOBS3[0].current_run.started_at,
      ended_at: Date.now(), duration_ms: 3600000, trigger: 'cron',
    });
    // run_ended 的重拉已发出（body 在请求时定格为 old），再推 new。
    await expect.poll(() => mock.cronListGetCount).toBeGreaterThan(listsBefore);
    const newStartedAt = Date.now();
    conn.send({
      type: 'run_started', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-new', started_at: newStartedAt, trigger: 'cron', session_id: 'sess-new',
    });
    const state = () => page.evaluate(async () => {
      const m = await import('/static/cron_state.js');
      const j = m.cronStore.jobs.find(x => x.id === 'cron-wire-1');
      return { prompt: j && j.prompt, run: j && j.current_run, cleared: m.cronRunClearedAtLocal.get('cron-wire-1') };
    });
    await expect.poll(async () => (await state()).prompt, { message: 'run_ended 的重拉应已落地' }).toBe('stale list marker');
    const merged = await state();
    expect(merged.run && merged.run.run_id, '旧响应把 new 换回了 old').toBe('run-new');
    expect(merged.run.started_at).toBe(newStartedAt);
    expect(merged.cleared && merged.cleared.map(c => c.runId), 'run_ended 应记下被清掉的 run').toEqual(['run-old']);
    await expect(row).toHaveClass(/is-running/);
    await ctx.close();
  });
});

test.describe('统一 run 帧的 WS 分发', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true, cronJobs: JOBS }); });
  test.afterAll(() => mock.server.close());

  test('cron 来源：run_started/run_ended 驱动列表行的运行中标记', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    // 真实 socket 路径上 handler 抛的异常被 onmessage 的 try/catch 吞成
    // console.error('ws parse error:', …)，不会变成 pageerror，所以两处都要收。
    page.on('console', (m) => { if (m.type() === 'error' && m.text().startsWith('ws parse error')) pageErrors.push(m.text()); });

    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-wire-1"]');
    await row.waitFor();
    await expect(row).not.toHaveClass(/is-running/);

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    const startedAt = Date.now();
    // 帧与 list 数据同步更新，如同真实后端。
    JOBS[0].current_run = { run_id: 'run-wire-1', started_at: startedAt, phase: 'sending', trigger: 'cron' };
    conn.send({
      type: 'run_started', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-wire-1', started_at: startedAt, trigger: 'cron', session_id: 'sess-w1',
    });
    await page.waitForTimeout(600);
    // 投影层（owner_id → job_id）坏掉时这里不会变 running —— 这正是本断言的靶子。
    await expect(row).toHaveClass(/is-running/);

    delete JOBS[0].current_run;
    const listsBefore = mock.cronListGetCount;
    conn.send({
      type: 'run_ended', subsystem: 'cron', owner_id: 'cron-wire-1',
      run_id: 'run-wire-1', state: 'succeeded', started_at: startedAt,
      ended_at: Date.now(), duration_ms: 1200, trigger: 'cron',
    });
    await expect(row).not.toHaveClass(/is-running/);
    await expect(page.locator('#sr-announce'), 'succeeded 应播报').toHaveText('定时任务已完成');
    // run_ended 还要重拉列表，计数与 last_error_class 才会从后端回填。
    await expect.poll(() => mock.cronListGetCount, { message: 'run_ended 应触发一次列表重拉' }).toBeGreaterThan(listsBefore);
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

    const startedFetch = page.waitForRequest((req) => req.url().includes('/api/system/daemons'), { timeout: 5000 });
    conn.send({ type: 'run_started', subsystem: 'sysession', owner_id: 'autotitler', run_id: 'run-daemon-2', trigger: 'tick' });
    await startedFetch;

    await ctx.close();
  });
});
