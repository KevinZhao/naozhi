// @ts-check
//
// #2435 三件遗留（item 3 / 4 / 7）的行为化，取代 static_cron_test.go：
// 那份测试把 humanizeCron 一族函数从 cron_view.js 源码里"剪"出来丢给 node
// 跑——行为是真的，但提取器耦合函数在哪个文件、闭括号长什么样，cron_view
// 拆分会把它整个弄断。这里让真浏览器渲染真卡片：
//
//  3. humanizeCron 的表驱动用例 → 每个 shape 一个 job，断卡片 chip 文本
//  4. ↑/↓ run 导航的守卫 → 离开 cron 视图 / 模态打开时按键必须惰性
//     （select/input 聚焦的守卫在实践中只在模态内可达，被模态守卫先挡，
//     无法独立行为化——该分支保持由 handler 内相邻两守卫的变异间接覆盖）
//  7. rail 红点只数 failed/missed，纯 paused 不点亮
//
// 跑法：cd test/e2e && npx playwright test cron_view_2435.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

// item 3 的表——与被替换测试的用例一一对应（含必须回落裸表达式的三条）。
const HUMANIZE_CASES = [
  { expr: '20 * * * *', want: '每小时 :20' },
  { expr: '15 * * * *', want: '每小时 :15' },
  { expr: '5 * * * *', want: '每小时 :05' },
  { expr: '0 * * * *', want: '每小时' },
  { expr: '13 6 * * *', want: '每天 06:13' },
  { expr: '*/15 * * * *', want: '每 15 分钟' },
  { expr: '0 */6 * * *', want: '每 6 小时' },
  { expr: '@every 30m', want: '每 30 分钟' },
  { expr: '0 9 * * 1-5', want: '工作日 09:00' },
  { expr: '0 9 * * 0,6', want: '周末 09:00' },
  { expr: '30 8 1 * *', want: '每月 1 日 08:30' },
  { expr: '60 * * * *', want: '60 * * * *' },
  { expr: '20 * 1 * *', want: '20 * 1 * *' },
  { expr: '20 * * * 1', want: '20 * * * 1' },
];

function humanizeJobs() {
  const now = Date.now();
  return HUMANIZE_CASES.map((c, i) => ({
    id: `cron-h-${i}`,
    schedule: c.expr,
    prompt: `humanize case ${i}`,
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  }));
}

function navJobs() {
  const now = Date.now();
  return [{
    id: 'cron-nav-1',
    schedule: '0 6 * * *',
    prompt: 'arrow guard job',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - 86400000,
    next_run: now + 3600000,
    recent_runs: [0, 1, 2].map((i) => ({
      run_id: `n-run-${i}`,
      started_at: now - (i + 1) * 3600000,
      duration_ms: 60000,
      outcome: 'succeeded',
      trigger: 'schedule',
    })),
    stats: { total: 3, succeeded: 3 },
  }];
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

async function openCron(browser, mockOpts) {
  const mock = await startMockServer(mockOpts);
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row');
  const cleanup = async () => { await ctx.close(); mock.server.close(); };
  return { page, cleanup };
}

test('humanizeCron 表驱动：卡片 chip 渲染人话，未识别 shape 回落裸表达式', async ({ browser }) => {
  const { page, cleanup } = await openCron(browser, { cronJobs: humanizeJobs() });
  for (let i = 0; i < HUMANIZE_CASES.length; i++) {
    const c = HUMANIZE_CASES[i];
    const chip = page.locator(`.cj-row[data-cron-id="cron-h-${i}"] .cj-schedule`);
    // chip 文本 = humanizeCron(expr) + 可选时区后缀，锚定前缀即锚定翻译本身。
    await expect(chip, `humanizeCron(${c.expr})`).toHaveText(
      new RegExp('^' + c.want.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    );
  }
  await cleanup();
});

test('↑/↓ 守卫：离开 cron 视图或模态打开时按键惰性，回到干净的 cron 视图才生效', async ({ browser }) => {
  const { page, cleanup } = await openCron(browser, { cronJobs: navJobs() });
  await page.click('.cj-row[data-cron-id="cron-nav-1"]');
  await page.waitForSelector('#cron-timeline-panel .ctr');
  await page.click('.ctr[data-run-id="n-run-0"]');
  await expect(page.locator('.ctr[data-run-id="n-run-0"] .ctr-detail')).toBeVisible();

  // 守卫 1：切到会话视图后 ↓ 不得被 handler 劫持（preventDefault）。
  // 离开视图后 cron 面板 DOM 已卸载，"看不见的 run 被切走"无从渲染——
  // 守卫在这里的可观察效果就是把按键还给页面默认行为。先 blur 输入框，
  // 否则 form-control 守卫先挡下按键，视图守卫就不是唯一防线。
  await page.click('#abnav-chat');
  await page.evaluate(() => { const a = document.activeElement; if (a && a !== document.body) a.blur(); });
  const hijacked = await page.evaluate(() => {
    const ev = new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true });
    document.body.dispatchEvent(ev);
    return ev.defaultPrevented;
  });
  expect(hijacked, '离开 cron 视图后 ↓ 不得被 preventDefault 劫持').toBe(false);
  await page.click('#abnav-cron');
  await expect(page.locator('.ctr[data-run-id="n-run-0"] .ctr-detail')).toBeVisible();
  await expect(page.locator('.ctr-detail')).toHaveCount(1);

  // 守卫 2：模态打开时 ↓ 惰性。（drawer 布局下列表头的新建钮不可见，
  // 用 drawer 里的编辑钮开模态——守卫只看 .modal-overlay 是否存在。）
  await page.click('.cron-detail-pane [data-action="cron-edit"]');
  await page.waitForSelector('.modal-overlay');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('.ctr[data-run-id="n-run-0"] .ctr-detail')).toHaveCount(1);
  // 关模态：Esc 监听挂在 overlay 元素上（焦点在 body 时不触发），走取消钮。
  await page.click('[data-action="cron-modal-dismiss"]');
  await expect(page.locator('.modal-overlay')).toHaveCount(0);

  // 干净的 cron 视图：↓ 正常移动展开。
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('.ctr[data-run-id="n-run-1"] .ctr-detail')).toBeVisible();

  await cleanup();
});

test('rail 红点：纯 paused 不点亮，failed/missed 点亮', async ({ browser }) => {
  const now = Date.now();
  const base = { schedule: '0 6 * * *', prompt: 'p', work_dir: '/home/user/workspace/myproject', created_at: now - 86400000, next_run: now + 3600000, recent_runs: [], stats: { total: 0, succeeded: 0 } };

  // 纯 paused：badge 保持隐藏——暂停是操作员的主动状态，不是告警。
  {
    const { page, cleanup } = await openCron(browser, {
      cronJobs: [{ ...base, id: 'cron-p-1', paused: true }],
    });
    await expect(page.locator('#abnav-cron-badge')).toBeHidden();
    await cleanup();
  }

  // 有 last_error：badge 点亮。
  {
    const { page, cleanup } = await openCron(browser, {
      cronJobs: [{ ...base, id: 'cron-p-2', paused: false, last_error: 'boom' }],
    });
    await expect(page.locator('#abnav-cron-badge')).toBeVisible();
    await cleanup();
  }

  // missed：badge 点亮。
  {
    const { page, cleanup } = await openCron(browser, {
      cronJobs: [{ ...base, id: 'cron-p-3', paused: false, missed: true }],
    });
    await expect(page.locator('#abnav-cron-badge')).toBeVisible();
    await cleanup();
  }
});
