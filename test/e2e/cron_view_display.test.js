// @ts-check
//
// cron 面板展示 bug 回归（fix/cron-view-display）：
//   #1 执行历史：后端每 job 只嵌 5 条 recent_runs，前端不得因 `< 10` 误判
//      "已到结尾"；首次「加载更多」必须真的请求 /api/cron/runs。
//      反过来，少于 recent_runs_cap（列表响应下发）条即是全部历史：直接
//      "已到结尾"，不再翻页请求。
//   #2 行内详情：点 .ctr-detail 内部不得把行折叠。
//   #3 时区：浏览器时区 ≠ 服务端时区时 schedule chip 带 (CST) 标注；相同则不带。
//   #6 需关注 chip：rail 红点有 N，面板内必须有可点的「需关注 N」chip。
//   #7 新建弹窗的「完成后通知我」提示列表响应下发的默认通知目标。
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

// 行与面板外壳的各段：cronJobCardHtml 的 when 列（cronJobWhen）与子行
// （cronJobSubRowHtml），renderCronPanel 的错过横幅、隐藏的汇总 chip 与筛选栏。
test('行的 when 列与子行图标、面板的错过横幅 / 汇总 / 筛选栏按 job 状态渲染', async ({ browser }) => {
  const now = Date.now();
  const base = { work_dir: '/home/user/workspace/myproject', created_at: now - 86400000, recent_runs: [] };
  const mock = await startMockServer({
    cronJobs: [
      { ...base, id: 'cj-run', schedule: '0 6 * * *', prompt: 'running job', notify: false, fresh_context: true,
        next_run: now + 3600000, current_run: { run_id: 'run-xyz', phase: 'sending', started_at: now - 3000 } },
      { ...base, id: 'cj-paused', schedule: '0 7 * * *', prompt: 'paused job', paused: true, next_run: now + 3600000 },
      { ...base, id: 'cj-missed', schedule: '0 8 * * *', prompt: 'missed job', missed: true, missed_since: now - 7200000,
        next_run: now + 3600000, last_run_at: now - 7200000 },
      { ...base, id: 'cj-err', schedule: '0 9 * * *', prompt: 'failing job', last_error: 'boom happened', next_run: now + 3600000 },
    ],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    const row = (/** @type {string} */ id) => page.locator(`.cj-row[data-cron-id="${id}"]`);
    await expect(row('cj-err')).toHaveCount(1);

    // Running: the when column is the live clock, titled with the run, and the
    // sub-row carries the non-default icons plus the inline when label.
    const runWhen = row('cj-run').locator('.cj-when');
    await expect(runWhen).toHaveClass(/running/);
    await expect(runWhen).toHaveAttribute('title', 'run_id run-xyz — phase sending');
    const runSub = row('cj-run').locator('.cj-sub');
    await expect(runSub.locator('.cj-schedule')).toHaveAttribute('data-action', 'cron-edit');
    await expect(runSub.locator('.cj-icon.notify-off')).toHaveCount(1);
    await expect(runSub.locator('.cj-icon.fresh')).toHaveCount(1);
    await expect(runSub.locator('.cj-icon.missed')).toHaveCount(0);
    await expect(runSub.locator('.cj-when-inline')).toHaveCount(1);

    // Paused: 已暂停 in both when slots, no run button.
    await expect(row('cj-paused').locator('.cj-when')).toHaveText('已暂停');
    await expect(row('cj-paused').locator('.cj-when-inline.paused')).toHaveText('已暂停');
    await expect(row('cj-paused').locator('.cj-run')).toHaveCount(0);

    // Missed: the warning icon and the last-run chip; error: the strip.
    await expect(row('cj-missed').locator('.cj-sub .cj-icon.missed')).toHaveCount(1);
    await expect(row('cj-missed').locator('.cj-sub .cj-ago')).toContainText('上次');
    await expect(row('cj-missed').locator('.cj-when')).toHaveAttribute('title', /^next run: /);
    await expect(row('cj-err').locator('.cj-error .cj-err-text')).toHaveText('boom happened');

    // Panel chrome.
    await expect(page.locator('.cron-missed-banner .cmb-text')).toContainText('有 1 个任务曾错过调度');
    await expect(page.locator('.cj-summary')).toHaveText('· 运行中 1 · 需关注 3');
    await expect(page.locator('.cron-filter-bar .cron-status-chip[data-status="attention"]')).toHaveText('需关注 3');
    await expect(page.locator('.cron-filter-bar .cron-sort-select option')).toHaveCount(4);
    await expect(page.locator('#cron-search-input')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('#1 少于 recent_runs_cap 条 recent_runs 即全部历史：「已到结尾」，不请求 /api/cron/runs', async ({ browser }) => {
  // No stats.total, so only the list response's recent_runs_cap (5) can tell
  // that 3 embedded runs are the whole history.
  const job = Object.assign(jobs()[0], { recent_runs: fiveRuns().slice(0, 3), stats: undefined });
  const mock = await startMockServer({ cronJobs: [job] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pages = [];
  page.on('request', (r) => { if (/\/api\/cron\/runs\?job_id=/.test(r.url())) pages.push(r.url()); });
  try {
    await openCronDrawer(page, mock.url);
    await expect(page.locator('#cron-timeline-panel .ctr')).toHaveCount(3);
    await expect(page.locator('#cron-timeline-panel .ct-more-btn')).toHaveText('已到结尾');
    await expect(page.locator('#cron-timeline-panel .ct-more-btn')).toBeDisabled();
    expect(pages).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('#7 新建弹窗提示列表响应里的默认通知目标，未配置时提示去配置', async ({ browser }) => {
  for (const [meta, want] of [
    [{ notify_default: { platform: 'feishu', chat_id: 'oc_***abcd' } }, '→ feishu (oc_***abcd)'],
    [{}, /^未配置默认通知目标/],
  ]) {
    const mock = await startMockServer({ cronJobs: jobs(), cronListMeta: meta });
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#abnav-cron');
      await page.waitForSelector('.cj-row');
      await page.click('.cron-new-btn');
      await expect(page.locator('.cron-modal #cron-notify-default-hint')).toHaveText(want);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
