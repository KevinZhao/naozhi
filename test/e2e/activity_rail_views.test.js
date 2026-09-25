// @ts-check
//
// The activity rail and the full-screen views it switches between (会话 /
// 资产 / 文件 / 自动化 / 系统 / 设置), driven by clicking the rail:
//
//   - exactly one view shows at a time: its container is visible and not
//     [hidden], the chat panels are hidden, and only its button is
//     aria-pressed;
//   - the rail buttons carry Chinese labels (the R149 localization contract);
//   - clicking the active view again is a no-op: no refetch, no poll restart;
//   - a cron repaint that arrives while chat is showing stays out of the
//     chat DOM. Cron rendering is keyed on the active view and paints into
//     #cron-main, never #main;
//   - the 自动化 rail dot lights for errored or missed jobs, not for paused;
//   - the 系统 badge is primed at load, and the view polls only while shown.
//
// 跑法：cd test/e2e && npx playwright test activity_rail_views.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const VIEWS = [
  { view: 'cron', btn: '#abnav-cron', main: '#cron-main' },
  { view: 'system', btn: '#abnav-system', main: '#system-main' },
  { view: 'settings', btn: '#abnav-settings', main: '#settings-main' },
  { view: 'files', btn: '#abnav-files', main: '.files-main' },
];
const BUTTONS = ['#abnav-chat', '#abnav-assets', '#abnav-files', '#abnav-cron', '#abnav-system', '#abnav-settings'];

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop rail; desktop-chrome only');
  }
});

/** @param {Partial<ReturnType<typeof cronJob>>} over */
function cronJob(over = {}) {
  const now = Date.now();
  return {
    id: 'cron-rail-1', schedule: '0 6 * * *', prompt: 'nightly', work_dir: '/home/user/workspace/myproject',
    paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: [],
    stats: { total: 1, succeeded: 1 }, ...over,
  };
}

/** @param {import('@playwright/test').Browser} browser @param {object} overrides */
async function open(browser, overrides) {
  const mock = await startMockServer(overrides);
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  return { mock, ctx, page, pageErrors };
}

/** @param {import('@playwright/test').Page} page @param {string} pressed */
async function expectOnlyPressed(page, pressed) {
  for (const b of BUTTONS) {
    await expect(page.locator(b)).toHaveAttribute('aria-pressed', String(b === pressed));
  }
}

test('one view at a time: container shown, chat hidden, one button pressed', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await open(browser, { systemDaemons: [], projectFiles: { myproject: { dirs: { '': [] } } } });
  try {
    for (const v of VIEWS) await expect(page.locator(v.main)).toBeHidden();
    await expectOnlyPressed(page, '#abnav-chat');

    for (const v of VIEWS) {
      await page.click(v.btn);
      await expect(page.locator('body')).toHaveClass(new RegExp('nz-view-' + v.view));
      await expect(page.locator(v.main)).toBeVisible();
      if (v.main.startsWith('#')) {
        expect(await page.locator(v.main).getAttribute('hidden'), v.main + ' must drop [hidden] in its view').toBeNull();
      }
      await expect(page.locator('.main')).toBeHidden();
      await expectOnlyPressed(page, v.btn);
      for (const other of VIEWS) {
        if (other !== v) await expect(page.locator(other.main)).toBeHidden();
      }
    }
    await page.click('#abnav-chat');
    await expect(page.locator('.main')).toBeVisible();
    for (const v of VIEWS) await expect(page.locator(v.main)).toBeHidden();
    await expectOnlyPressed(page, '#abnav-chat');
    for (const id of ['#cron-main', '#system-main', '#settings-main']) {
      expect(await page.locator(id).getAttribute('hidden'), id + ' must carry [hidden] outside its view').not.toBeNull();
    }
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('rail buttons carry localized labels', async ({ browser }) => {
  const { mock, ctx, page } = await open(browser, {});
  try {
    const want = {
      '#abnav-cron': { label: '自动化视图', title: '定时任务' },
      '#abnav-system': { label: '系统任务视图', title: '系统任务（内置后台守护）' },
      '#abnav-settings': { label: '设置视图', title: '设置' },
    };
    for (const [sel, w] of Object.entries(want)) {
      await expect(page.locator(sel)).toHaveAttribute('aria-label', w.label);
      await expect(page.locator(sel)).toHaveAttribute('title', w.title);
    }
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('clicking the active view again does not refetch', async ({ browser }) => {
  const { mock, ctx, page } = await open(browser, { cronJobs: [cronJob()] });
  try {
    await page.click('#abnav-cron');
    await expect(page.locator('.cj-row[data-cron-id="cron-rail-1"]')).toBeVisible();
    await expect.poll(() => mock.cronListGetCount).toBeGreaterThan(0);
    // Let the entry fetch settle, then count.
    await page.waitForTimeout(300);
    const before = mock.cronListGetCount;
    await page.click('#abnav-cron');
    await page.click('#abnav-cron');
    await page.waitForTimeout(500);
    expect(mock.cronListGetCount, 'a re-click on the active view must not re-enter it').toBe(before);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a cron repaint while chat is showing stays out of the chat DOM', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await open(browser, { ws: true, cronJobs: [cronJob()] });
  try {
    await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
    await page.waitForSelector('#events-scroll');
    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
    // A cron frame drives renderCronPanel; with chat showing it must not paint.
    mock.wsConnections[mock.wsConnections.length - 1].send({
      type: 'run_started', subsystem: 'cron', owner_id: 'cron-rail-1', run_id: 'run-rail-1',
      started_at: Date.now(), trigger: 'cron',
    });
    await page.waitForTimeout(400);
    await expect(page.locator('#events-scroll')).toBeVisible();
    await expect(page.locator('.main .cj-row')).toHaveCount(0);
    await expect(page.locator('#cron-main')).toBeHidden();
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const [name, jobs, lit] of /** @type {const} */ ([
  ['an errored job lights the 自动化 dot', [cronJob({ last_error: 'boom' })], true],
  ['a missed job lights the 自动化 dot', [cronJob({ missed: true })], true],
  ['a paused job alone does not light it', [cronJob({ paused: true })], false],
])) {
  test(name, async ({ browser }) => {
    const { mock, ctx, page } = await open(browser, { cronJobs: jobs });
    try {
      await expect.poll(() => mock.cronListGetCount).toBeGreaterThan(0);
      const badge = page.locator('#abnav-cron-badge');
      if (lit) await expect(badge).toBeVisible();
      else {
        await page.waitForTimeout(300);
        await expect(badge).toBeHidden();
      }
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('系统: badge primed at load, view polls only while shown', async ({ browser }) => {
  const mock = await startMockServer({
    systemDaemons: [{ name: 'auto-titler', enabled: true, last_run: { state: 'failed' } }],
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    // Primed before the view is ever opened.
    await expect(page.locator('#abnav-system-badge')).toBeVisible();

    await page.click('#abnav-system');
    await expect(page.locator('#system-main')).toBeVisible();
    await page.clock.pauseAt(Date.now() + 1000);
    const entered = mock.systemDaemonsGetCount;
    await page.clock.runFor(5200);
    await expect.poll(() => mock.systemDaemonsGetCount, { message: 'the view polls while shown' }).toBeGreaterThan(entered);

    await page.click('#abnav-chat');
    await page.waitForTimeout(200);
    const left = mock.systemDaemonsGetCount;
    await page.clock.runFor(15500);
    await page.waitForTimeout(300);
    expect(mock.systemDaemonsGetCount, 'leaving the view stops the poll').toBe(left);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('layout: 设置 sits at the rail foot on desktop; on a phone the rail is one bottom tab row', async ({ browser }) => {
  const mock = await startMockServer({});
  try {
    const desk = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    const dp = await desk.newPage();
    await dp.goto(mock.url + '/dashboard');
    await dp.waitForSelector('.session-card');
    const settings = await dp.locator('#abnav-settings').boundingBox();
    const cron = await dp.locator('#abnav-cron').boundingBox();
    expect(settings && cron).toBeTruthy();
    // The bottom group is pushed to the foot, well below the top group.
    expect(/** @type {any} */ (settings).y - /** @type {any} */ (cron).y).toBeGreaterThan(300);
    await desk.close();

    const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const pp = await phone.newPage();
    await pp.goto(mock.url + '/dashboard');
    await pp.waitForSelector('.session-card');
    const boxes = [];
    for (const b of BUTTONS) {
      const loc = pp.locator(b);
      if (await loc.isVisible()) boxes.push(await loc.boundingBox());
    }
    expect(boxes.length, 'the phone tab bar shows the rail buttons').toBeGreaterThanOrEqual(5);
    const ys = boxes.map(b => Math.round(/** @type {any} */ (b).y));
    expect(new Set(ys).size, 'every tab sits on the same row').toBe(1);
    expect(ys[0]).toBeGreaterThan(844 / 2);
    await phone.close();
  } finally {
    mock.server.close();
  }
});
