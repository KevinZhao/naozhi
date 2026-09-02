// @ts-check
//
// cron 面板展示 bug 回归（fix/cron-view-display）：
//   #1 执行历史：后端每 job 只嵌 5 条 recent_runs，前端不得因 `< 10` 误判
//      "已到结尾"；首次「加载更多」必须真的请求 /api/cron/runs。
//   #2 行内详情：点 .ctr-detail 内部不得把行折叠。
//   #3 时区：浏览器时区 ≠ 服务端时区时 schedule chip 带 (CST) 标注；相同则不带。
//   #6 需关注 chip：rail 红点有 N，面板内必须有可点的「需关注 N」chip。
//
// 跑法：cd test/e2e && npx playwright test cron_view_display.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

// 恰好 5 条 recent_runs（== 后端 recentRunsPerJob），模拟线上 94 次运行的任务。
function fiveRuns() {
  const now = Date.now();
  const runs = [];
  for (let i = 1; i <= 5; i++) {
    runs.push({
      run_id: 'run-five' + i,
      state: 'succeeded',
      started_at: now - i * 60 * 60 * 1000,
      ended_at: now - i * 60 * 60 * 1000 + 10000,
      duration_ms: 10000,
      trigger: 'cron',
      session_id: 'sess-five' + i,
    });
  }
  return runs;
}

function jobs() {
  return [
    {
      id: 'cron-001',
      schedule: '13 6 * * *',
      prompt: 'daily digest',
      work_dir: '/home/user/workspace/myproject',
      paused: false,
      created_at: Date.now() - 86400000,
      next_run: Date.now() + 3600000,
      last_run_at: Date.now() - 3600000,
      recent_runs: fiveRuns(),
      stats: { total: 94, succeeded: 94 },
    },
    {
      id: 'cron-002',
      schedule: '0 9 * * 1-5',
      prompt: 'daily report',
      work_dir: '/home/user/workspace/otherproject',
      paused: true,
      created_at: Date.now() - 172800000,
    },
  ];
}

const SHANGHAI_META = {
  timezone: 'Asia/Shanghai',
  timezone_abbr: 'CST',
  timezone_label: 'Asia/Shanghai (UTC+08:00)',
};

async function openCronDrawer(page, url) {
  await page.goto(url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-timeline-panel .ctr');
}

test.describe('cron 面板展示回归', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ cronJobs: jobs(), cronListMeta: SHANGHAI_META }); });
  test.afterAll(() => mock.server.close());

  test('#1 5 条 recent_runs 不显示「已到结尾」，加载更多会请求 /api/cron/runs', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await openCronDrawer(page, mock.url);

    const rows = await page.$$('#cron-timeline-panel .ctr');
    expect(rows.length).toBe(5);
    const more = page.locator('#cron-timeline-panel .ct-more-btn');
    await expect(more).toHaveCount(1);
    await expect(more).not.toHaveText('已到结尾');
    await expect(more).toHaveText('加载更多');
    await expect(more).toBeEnabled();

    const [req] = await Promise.all([
      page.waitForRequest(r => /\/api\/cron\/runs\?job_id=cron-001/.test(r.url()) && r.method() === 'GET'),
      more.click(),
    ]);
    expect(req.url()).toMatch(/before=\d+/);
    // mock 返回 next_before:0 → 这才是真正的结尾。
    await expect(page.locator('#cron-timeline-panel .ct-more-btn')).toHaveText('已到结尾');
    await ctx.close();
  });

  test('#2 点击行内详情不折叠该行', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await openCronDrawer(page, mock.url);

    await page.click('#cron-timeline-panel .ctr[data-run-id="run-five1"] .ctr-main');
    const detail = page.locator('#cron-timeline-panel .ctr[data-run-id="run-five1"] .ctr-detail');
    await expect(detail).toHaveCount(1);
    // 点详情容器本身（对应线上点 <details> 输入快照 / 拖选文字）
    await detail.click({ position: { x: 20, y: 10 } });
    await page.waitForTimeout(150);
    await expect(page.locator('#cron-timeline-panel .ctr[data-run-id="run-five1"]')).toHaveClass(/is-expanded/);
    await expect(detail).toHaveCount(1);
    // 点行头才折叠（原行为保留）
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-five1"] .ctr-main');
    await expect(page.locator('#cron-timeline-panel .ctr[data-run-id="run-five1"] .ctr-detail')).toHaveCount(0);
    await ctx.close();
  });

  test('#6 有需关注任务时面板出现「需关注 N」chip 且可筛选', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    expect(await page.$$eval('.cj-row', els => els.length)).toBe(2);

    const chip = page.locator('.cron-status-chip[data-status="attention"]');
    await expect(chip).toHaveCount(1);
    await expect(chip).toBeVisible();
    await expect(chip).toHaveText('需关注 1');
    await chip.click();
    await expect(chip).toHaveClass(/active/);
    await expect(page.locator('.cj-row')).toHaveCount(1);
    await expect(page.locator('.cj-row[data-cron-id="cron-002"]')).toHaveCount(1);
    // 回到全部
    await page.click('.cron-status-chip[data-status="all"]');
    await expect(page.locator('.cj-row')).toHaveCount(2);
    await ctx.close();
  });

  test('#3 浏览器时区 ≠ 服务端时区：schedule chip 带 (CST) 标注', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 }, timezoneId: 'UTC' });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    const chipText = await page.$eval('.cj-row[data-cron-id="cron-001"] .cj-schedule', el => el.textContent);
    expect(chipText).toContain('06:13');
    expect(chipText).toContain('(CST)');
    // drawer 什么时候 同样标注
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForSelector('.css-when-schedule');
    expect(await page.$eval('.css-when-schedule', el => el.textContent)).toContain('(CST)');
    await ctx.close();
  });

  test('#3 浏览器时区 = 服务端时区：不加标注', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 }, timezoneId: 'Asia/Shanghai' });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    const chipText = await page.$eval('.cj-row[data-cron-id="cron-001"] .cj-schedule', el => el.textContent);
    expect(chipText).toContain('06:13');
    expect(chipText).not.toContain('(CST)');
    await ctx.close();
  });
});
