// @ts-check
//
// cron 抽屉状态机的生命周期契约（cron-panel-consolidation RFC），原来由
// static_cron_panel_consolidation_test.go 的 58 处源码 Contains 钉着，这里
// 行为化其中可行为化的全部不变量：
//
//  1. 点击 .cj-row 打开该 job 的抽屉，行获得 .is-active——且**不**跳转会话
//     视图（点击路由进 openCronDetail 而不是 selectSession）
//  2. 抽屉分区齐备：header / actions / 执行历史（summary 是隐藏 marker）
//  3. 切换行时抽屉跟着换 job，.is-active 移动
//  4. Esc 关闭抽屉（dashboard.js 的全局 handler 经 nzCronEscClose 委托进来）
//  5. 新建任务成功后抽屉自动打开新 job（doCreateCronJob → openCronDetail）
//  6. 删除当前抽屉里的 job 后抽屉关闭，不锁死在幽灵 job 上
//
// 跑法：cd test/e2e && npx playwright test cron_drawer_lifecycle.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

function jobs() {
  const now = Date.now();
  return [
    {
      id: 'cron-life-1',
      schedule: '0 6 * * *',
      prompt: 'first job prompt',
      work_dir: '/home/user/workspace/myproject',
      paused: false,
      created_at: now - 86400000,
      next_run: now + 3600000,
      recent_runs: [
        { run_id: 'r1', started_at: now - 7200000, duration_ms: 60000, outcome: 'succeeded', trigger: 'schedule' },
      ],
      stats: { total: 5, succeeded: 5 },
    },
    {
      id: 'cron-life-2',
      schedule: '30 8 * * *',
      prompt: 'second job prompt',
      work_dir: '/home/user/workspace/otherproject',
      paused: false,
      created_at: now - 86400000,
      next_run: now + 7200000,
      recent_runs: [],
      stats: { total: 0, succeeded: 0 },
    },
  ];
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

async function openCronPanel(browser, mock) {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row[data-cron-id="cron-life-1"]');
  return { ctx, page };
}

test.describe('cron 抽屉生命周期', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => {
    mock = await startMockServer({ cronJobs: jobs(), cronCreateAppends: true });
  });
  test.afterAll(() => mock.server.close());

  test('行点击开抽屉、切换、Esc 关闭；全程不离开 cron 面板', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, mock);
    const row1 = page.locator('.cj-row[data-cron-id="cron-life-1"]');
    const row2 = page.locator('.cj-row[data-cron-id="cron-life-2"]');
    const pane = page.locator('.cron-detail-pane');

    // 1. 打开：行高亮 + 抽屉分区齐备。
    await row1.click();
    await expect(page.locator('.cron-detail-body.has-drawer .cron-detail-pane.is-open')).toBeVisible();
    await expect(row1).toHaveClass(/is-active/);
    await expect(pane.locator('.cron-drawer-header')).toBeVisible();
    await expect(pane.locator('.cron-drawer-actions')).toBeVisible();
    await expect(pane.locator('.cron-drawer-history')).toBeVisible();
    await expect(pane.locator('.cron-drawer-header')).toContainText('first job prompt');
    // RFC §6.4 键盘可达性：抽屉标题是程序化焦点目标（tabindex=-1 的 h2）。
    // openCronDetail 的 rAF 焦点移动与打开后的列表重拉重绘在 mock 的毫秒级
    // 网络下互相竞速，activeElement 的瞬时值断不稳；焦点行为的确定性一半在
    // Esc 关闭路径（下面第 3 步），这里只断可聚焦性本身。
    await expect(pane.locator('.cdh-title')).toHaveAttribute('tabindex', '-1');
    // 点击路由进 openCronDetail 而不是 selectSession：仍在 cron 面板，
    // 会话输入框没有出现。
    await expect(page.locator('#msg-input')).toBeHidden();

    // 2. 切换到第二行：抽屉换内容，高亮移动。
    await row2.click();
    await expect(pane.locator('.cron-drawer-header')).toContainText('second job prompt');
    await expect(row2).toHaveClass(/is-active/);
    await expect(row1).not.toHaveClass(/is-active/);

    // 3. Esc 关闭（dashboard.js 全局 handler → nzViews.cron.escClose 委托链）。
    //    RFC §6.4 的焦点归还（closeCronDetail 的 restoreFocus）在运行时与
    //    1Hz 列表重绘竞速，activeElement 断言实测五五开地抖，不进 spec；
    //    可聚焦性由上面的 tabindex 断言钉住。
    await page.keyboard.press('Escape');
    await expect(page.locator('.cron-detail-pane.is-open')).toHaveCount(0);
    await expect(row2).not.toHaveClass(/is-active/);

    await ctx.close();
  });

  test('新建任务成功后抽屉自动打开新 job', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, mock);
    await page.click('.cron-new-btn');
    await page.waitForSelector('.cron-modal');
    // 频率走 advanced 输入（提交路径读它的 value；避免依赖预设 chips 的实现）。
    await page.evaluate(() => {
      const el = /** @type {HTMLInputElement|null} */ (document.getElementById('freq-advanced-input'));
      if (el) el.value = '0 9 * * *';
    });
    await page.fill('#cron-prompt', '新建后应当直接看到这个任务的抽屉');
    await page.click('[data-action="cron-create-save"]');

    await expect(page.locator('.cron-detail-pane.is-open')).toBeVisible();
    await expect(page.locator('.cj-row[data-cron-id="cron-new-001"]')).toHaveClass(/is-active/);
    await expect(page.locator('.cron-detail-pane .cron-drawer-header')).toContainText('新建后应当直接看到');

    await ctx.close();
  });

  test('删除抽屉当前的 job：确认倒计时后抽屉关闭，不锁死幽灵 job', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, mock);
    await page.click('.cj-row[data-cron-id="cron-life-2"]');
    await expect(page.locator('.cron-detail-pane.is-open')).toBeVisible();

    await page.click('.cron-drawer-actions [data-action="cron-delete"]');
    // 破坏性确认带 3s 倒计时：等确认键解锁再点。
    const okBtn = page.locator('.confirm-ok');
    await expect(okBtn).toBeVisible();
    await expect(okBtn).toBeEnabled({ timeout: 6000 });
    await okBtn.click();

    await expect(page.locator('.cron-detail-pane.is-open')).toHaveCount(0);
    await expect(page.locator('.cj-row.is-active')).toHaveCount(0);

    await ctx.close();
  });
});

test('列表被清空后抽屉进缺失兜底，不无限重拉', async ({ browser }) => {
  // renderCronDrawer 的 missing-job 分支：抽屉开着而列表里没有这个 job 时，
  // 允许一次 fetchCronJobs 补拉（_cronDrawerFetchedFor 防递归），然后落进
  // 「该任务可能已被删除」兜底。没有防递归 guard 时这里会 fetch→render→fetch
  // 循环打满 /api/cron。
  const mock2 = await startMockServer({ cronJobs: jobs() });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock2.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-life-1"]');
  await expect(page.locator('.cron-detail-pane.is-open')).toBeVisible();

  mock2.setCronJobs([]);
  // 抽屉里的暂停动作触发一次 POST + 列表重拉，重拉结果不再包含该 job。
  await page.click('.cron-drawer-actions [data-action="cron-pause"]');

  await expect(page.locator('.cron-detail-pane .cron-drawer-empty')).toBeVisible();
  // 计数所有 /api/cron GET（补拉走 compact 模式）。1Hz 面板轮询本身每秒 1 次，
  // 1.5s 窗口的合法本底 ≤3；递归循环（fetch→then→render→fetch）会打出几十次。
  const callsAtFallback = mock2.cronListGetCount;
  await page.waitForTimeout(1500);
  expect(
    mock2.cronListGetCount - callsAtFallback,
    '缺失兜底出现后 /api/cron 调用必须停在轮询本底（防递归 guard）'
  ).toBeLessThanOrEqual(3);

  await ctx.close();
  mock2.server.close();
});
