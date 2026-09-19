// @ts-check
//
// cron 执行历史的行内展开（cron-history-redesign §16，取代 v3 的 sheet 浮层）。
// 原来由 static_cron_history_redesign_test.go 的 18 处 Contains 钉着（模块状态
// 存在、函数存在、onclick 入口、ESC 优先级、↑↓ 快捷键、sheet 符号已删……），
// 这里断言那些源码形状要保证的实际行为：
//
//  1. 点行就地展开 `.ctr-detail`（是该行的子节点，不是 sheet 浮层——页面上
//     根本不存在 #cron-run-sheet）；同一时刻至多一行展开；二次点击收起
//  2. Esc 的优先级：先收行内展开（drawer 不动），再收 drawer
//  3. ↑↓ 把展开移到相邻行
//  4. 切换 job 后展开不残留（上下文隔离）
//  5. `.ctr-detail` 有 max-height 保护（长 result 不把 drawer 撑爆）
//
// 跑法：cd test/e2e && npx playwright test cron_inline_expand.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

function runsFor(prefix) {
  const now = Date.now();
  return [0, 1, 2].map((i) => ({
    run_id: `${prefix}-run-${i}`,
    started_at: now - (i + 1) * 3600000,
    duration_ms: 60000 + i,
    outcome: i === 1 ? 'failed' : 'succeeded',
    trigger: 'schedule',
    result: `run ${i} 的结果摘要`,
  }));
}

function jobs() {
  const now = Date.now();
  return [
    { id: 'cron-ie-1', schedule: '0 6 * * *', prompt: 'inline expand job one', work_dir: '/home/user/workspace/myproject', paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: runsFor('a'), stats: { total: 3, succeeded: 2, failed: 1 } },
    { id: 'cron-ie-2', schedule: '30 8 * * *', prompt: 'inline expand job two', work_dir: '/home/user/workspace/otherproject', paused: false, created_at: now - 86400000, next_run: now + 7200000, recent_runs: runsFor('b'), stats: { total: 3, succeeded: 3 } },
  ];
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test.describe('cron 执行历史行内展开', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterAll(() => mock.server.close());

  /** @param {import('@playwright/test').Browser} browser */
  async function openDrawer(browser, jobId) {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.click(`.cj-row[data-cron-id="${jobId}"]`);
    await page.waitForSelector('#cron-timeline-panel .ctr');
    return { ctx, page };
  }

  test('就地展开、单行独占、二次点击收起；无 sheet 浮层', async ({ browser }) => {
    const { ctx, page } = await openDrawer(browser, 'cron-ie-1');
    const row0 = page.locator('.ctr[data-run-id="a-run-0"]');
    const row1 = page.locator('.ctr[data-run-id="a-run-1"]');

    await row0.click();
    // 详情是被点击那一行的子节点——「行内」的字面定义。
    await expect(row0.locator('.ctr-detail')).toBeVisible();
    await expect(page.locator('#cron-run-sheet')).toHaveCount(0);

    // max-height 保护：长 result 不能把 drawer 撑到不可滚动。
    const maxH = await row0.locator('.ctr-detail').evaluate((el) => getComputedStyle(el).maxHeight);
    expect(maxH, '.ctr-detail 必须有 max-height 上限').not.toBe('none');

    // 换行展开：同一时刻至多一个 .ctr-detail。
    await row1.locator('.ctr-main').click();
    await expect(row1.locator('.ctr-detail')).toBeVisible();
    await expect(page.locator('.ctr-detail')).toHaveCount(1);

    // 二次点击同一行 = 收起。展开后行的几何中心落在 .ctr-detail 里
    //（detail 内的点击不做 toggle——里面有自己的按钮），所以点行头。
    await row1.locator('.ctr-main').click();
    await expect(page.locator('.ctr-detail')).toHaveCount(0);

    await ctx.close();
  });

  test('Esc 先收行内展开再收 drawer；↑↓ 移动展开行', async ({ browser }) => {
    const { ctx, page } = await openDrawer(browser, 'cron-ie-1');
    await page.click('.ctr[data-run-id="a-run-0"]');
    await expect(page.locator('.ctr[data-run-id="a-run-0"] .ctr-detail')).toBeVisible();

    // ↓ 把展开移到下一行。
    await page.keyboard.press('ArrowDown');
    await expect(page.locator('.ctr[data-run-id="a-run-1"] .ctr-detail')).toBeVisible();
    await expect(page.locator('.ctr-detail')).toHaveCount(1);
    // ↑ 移回来。
    await page.keyboard.press('ArrowUp');
    await expect(page.locator('.ctr[data-run-id="a-run-0"] .ctr-detail')).toBeVisible();

    // 第一次 Esc：只收行内展开，drawer 原地不动。
    await page.keyboard.press('Escape');
    await expect(page.locator('.ctr-detail')).toHaveCount(0);
    await expect(page.locator('.cron-detail-pane.is-open')).toBeVisible();

    // 第二次 Esc：收 drawer。
    await page.keyboard.press('Escape');
    await expect(page.locator('.cron-detail-pane.is-open')).toHaveCount(0);

    await ctx.close();
  });

  test('切换 job 后展开不残留', async ({ browser }) => {
    const { ctx, page } = await openDrawer(browser, 'cron-ie-1');
    await page.click('.ctr[data-run-id="a-run-0"]');
    await expect(page.locator('.ctr-detail')).toHaveCount(1);

    await page.click('.cj-row[data-cron-id="cron-ie-2"]');
    await page.waitForSelector('.ctr[data-run-id="b-run-0"]');
    await expect(page.locator('.ctr-detail')).toHaveCount(0);

    // 切回原 job：展开不得复活。isExpanded 判 jobId+runId 同时匹配，所以
    // 只在别的 job 页面上断言看不出状态是否真被清——复活是残留状态的真实危害。
    await page.click('.cj-row[data-cron-id="cron-ie-1"]');
    await page.waitForSelector('.ctr[data-run-id="a-run-0"]');
    await expect(page.locator('.ctr-detail')).toHaveCount(0);

    await ctx.close();
  });
});
