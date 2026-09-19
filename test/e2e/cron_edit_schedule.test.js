// @ts-check
//
// cron 编辑模态的三条契约，原来由 static_cron_schedule_test.go 的 14 处
// Contains 钉着（_cronScheduleTouched 检查、seed 语句、humanize 函数存在……），
// 这里断言那些内部状态要保证的外部行为：
//
//  1. 不 round-trip 的 legacy schedule 在卡片与编辑模态里显示人话中文标签
//     （多选 weekly、@every N[mh]），不是裸 cron 表达式
//  2. 编辑老任务时只改 prompt 就保存：PATCH body 里**没有** schedule 和
//     work_dir 键（diff-only 契约——原 bug 是 picker round-trip 把 schedule
//     改写、work_dir 被 seed 成空串后被后端清空）
//  3. 用户真动了频率控件时 schedule 才进 PATCH body
//
// 跑法：cd test/e2e && npx playwright test cron_edit_schedule.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

function jobs() {
  const now = Date.now();
  return [
    {
      id: 'cron-es-1',
      schedule: '0 9 * * 1,3,5',
      prompt: 'edit me please',
      work_dir: '/home/user/workspace/myproject',
      paused: false,
      created_at: now - 86400000,
      next_run: now + 3600000,
      recent_runs: [],
      stats: { total: 0, succeeded: 0 },
    },
    {
      id: 'cron-es-2',
      schedule: '@every 30m',
      prompt: 'legacy every job',
      work_dir: '/home/user/workspace/otherproject',
      paused: false,
      created_at: now - 86400000,
      next_run: now + 1800000,
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

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {object} mock
 */
async function openCron(browser, mock) {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row[data-cron-id="cron-es-1"]');
  return { ctx, page };
}

test.describe('cron 编辑与 legacy schedule 显示', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  // 项目 skip 的用例会在 mock 创建前中止 beforeEach 链（文件级 skip 钩子
  // 先跑），afterEach 仍执行——不带守卫时在 mobile-safari 下关一个
  // undefined 的 mock 直接把 skipped 变 failed（#2786 里的 3 个假失败）。
  test.beforeEach(async () => { mock = await startMockServer({ cronJobs: jobs() }); });
  test.afterEach(() => { if (mock) { mock.server.close(); mock = undefined; } });

  test('legacy schedule 在卡片与编辑模态显示中文标签而非裸表达式', async ({ browser }) => {
    const { ctx, page } = await openCron(browser, mock);

    // 多选 weekly：卡片 chip 是人话（周一/周三），不是 "0 9 * * 1,3,5"。
    const chip1 = page.locator('.cj-row[data-cron-id="cron-es-1"] .cj-schedule');
    await expect(chip1).toContainText('周一');
    await expect(chip1).toContainText('周三');
    await expect(chip1).not.toContainText('1,3,5');

    // @every 30m → 每 30 分钟。
    const chip2 = page.locator('.cj-row[data-cron-id="cron-es-2"] .cj-schedule');
    await expect(chip2).toContainText('每 30 分钟');
    await expect(chip2).not.toContainText('@every');

    // 编辑模态里的 legacy hint 同样给人话，并声明这是老格式。
    await chip1.click();
    const hint = page.locator('.freq-legacy-hint');
    await expect(hint).toBeVisible();
    await expect(hint).toContainText('周一');
    await expect(hint).toContainText('老格式');

    await ctx.close();
  });

  test('只改 prompt 保存：PATCH 不带 schedule / work_dir', async ({ browser }) => {
    const { ctx, page } = await openCron(browser, mock);
    await page.click('.cj-row[data-cron-id="cron-es-1"] .cj-schedule');
    await page.waitForSelector('[data-action="cron-edit-save"]');

    await page.fill('#edit-cron-prompt', '只改了这个 prompt');
    await page.click('[data-action="cron-edit-save"]');

    await expect.poll(() => mock.cronPatchCalls.length).toBe(1);
    const call = mock.cronPatchCalls[0];
    expect(call.id).toBe('cron-es-1');
    const body = JSON.parse(call.body);
    expect(body.prompt).toBe('只改了这个 prompt');
    expect(
      'schedule' in body,
      'schedule 未被触碰时不得进 PATCH body（picker round-trip 会悄悄改写 multi-dow）'
    ).toBe(false);
    expect(
      'work_dir' in body,
      'work_dir 未被触碰时不得进 PATCH body（曾被 seed 成空串导致后端清空）'
    ).toBe(false);

    await ctx.close();
  });

  test('动了频率控件后保存：schedule 进 PATCH body', async ({ browser }) => {
    const { ctx, page } = await openCron(browser, mock);
    await page.click('.cj-row[data-cron-id="cron-es-1"] .cj-schedule');
    await page.waitForSelector('#freq-mode-select');

    await page.selectOption('#freq-mode-select', 'daily');
    await page.click('[data-action="cron-edit-save"]');

    await expect.poll(() => mock.cronPatchCalls.length).toBe(1);
    const body = JSON.parse(mock.cronPatchCalls[0].body);
    expect(typeof body.schedule).toBe('string');
    expect(body.schedule).not.toBe('0 9 * * 1,3,5');

    await ctx.close();
  });
});
