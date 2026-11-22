// cron 暂停 / 恢复 / 删除是乐观的：点下去行、抽屉、需关注计数、rail 红点立刻变，
// 请求在后面确认；请求没回来之前一次带旧数据的列表补拉（run_ended 触发的
// fetchCronJobs）不能把行翻回去；失败则原样还原并弹 toast。
//
// 服务端用 page.route 接管：serverJobs 是"服务端此刻的真相"，暂停 / 删除请求被
// 扣住，直到测试放行时才改 serverJobs 并应答——所以扣住期间的 GET 一定是旧数据。
//
// 跑法：cd test/e2e && npx playwright test cron_optimistic.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, '仅 desktop-chrome project 跑');
});

function jobs() {
  const now = Date.now();
  const base = { work_dir: '/home/user/workspace/myproject', recent_runs: [] };
  return [
    { ...base, id: 'cron-001', schedule: '0 6 * * *', prompt: 'active job', paused: false,
      created_at: now - 1000, next_run: now + 3600000, stats: { total: 3 } },
    { ...base, id: 'cron-002', schedule: '0 7 * * *', prompt: 'paused job', paused: true,
      created_at: now - 2000 },
    { ...base, id: 'cron-003', schedule: '0 8 * * *', prompt: 'failing job', paused: false,
      created_at: now - 3000, next_run: now + 7200000, last_error: 'boom' },
  ];
}

// setup opens the cron view over a route-controlled /api/cron. hold(kind)
// returns a handle whose release(fn) answers the next held request of that
// kind ('pause' | 'resume' | 'delete') with fn's {status} after fn mutated
// serverJobs.
async function setup(browser) {
  const mock = await startMockServer({ ws: true, cronJobs: jobs() });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const state = { serverJobs: jobs(), held: {} };
  await page.route(/\/api\/cron(\?|$)/, async (route) => {
    const req = route.request();
    if (req.method() === 'GET') {
      return route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify({ jobs: state.serverJobs, recent_runs_cap: 5 }) });
    }
    if (req.method() === 'DELETE') return holdRoute(state, 'delete', route);
    return route.fallback();
  });
  await page.route(/\/api\/cron\/(pause|resume)$/, (route) =>
    holdRoute(state, /pause$/.test(route.request().url()) ? 'pause' : 'resume', route));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await expect(page.locator('.cj-row')).toHaveCount(3);
  return { mock, ctx, page, state };
}

function holdRoute(state, kind, route) {
  return new Promise((resolve) => {
    state.held[kind] = async (answer) => {
      delete state.held[kind];
      const { status, abort } = answer();
      if (abort) await route.abort('failed');
      else await route.fulfill({ status, contentType: 'application/json', body: status === 200 ? '{"ok":true}' : '{"error":"boom"}' });
      resolve();
    };
  });
}

async function waitHeld(state, kind) {
  await expect.poll(() => typeof state.held[kind]).toBe('function');
}

// staleRefetch makes the page refetch the list while the request is held: a
// cron run_ended frame for another job does fetchCronJobs. Resolves once the
// (pre-change) list response has been handled.
async function staleRefetch(page, mock) {
  const resp = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
  for (const c of mock.wsConnections) {
    c.send({ type: 'run_ended', subsystem: 'cron', owner_id: 'cron-002', run_id: 'run-x', state: 'skipped', ended_at: Date.now() });
  }
  await resp;
  // Let the .then(renderCronPanel) after the fetch run.
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
}

const row = (page, id) => page.locator(`.cj-row[data-cron-id="${id}"]`);
const attentionChip = (page) => page.locator('.cron-filter-bar .cron-status-chip[data-status="attention"]');

test('暂停：行、抽屉与需关注计数立刻变，扣住期间的旧列表不翻回，成功后以服务端为准', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    await row(page, 'cron-001').locator('.cj-main, .cj-title').first().click();
    const pane = page.locator('#cron-detail-pane');
    await expect(pane.locator('[data-action="cron-pause"]')).toBeVisible();
    await expect(attentionChip(page)).toHaveText('需关注 2');

    await pane.locator('[data-action="cron-pause"]').click();
    await waitHeld(state, 'pause');
    // Before the server answered.
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText('已暂停');
    await expect(row(page, 'cron-001')).toHaveClass(/paused/);
    await expect(pane.locator('[data-action="cron-resume"]')).toBeVisible();
    await expect(pane.locator('[data-action="cron-pause"]')).toHaveCount(0);
    await expect(attentionChip(page)).toHaveText('需关注 3');
    await expect(page.locator('.cj-summary')).toHaveText('· 需关注 3');

    // A list response that still says "not paused" lands while the POST is held.
    await staleRefetch(page, mock);
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText('已暂停');
    await expect(attentionChip(page)).toHaveText('需关注 3');

    const after = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
    await state.held.pause(() => {
      state.serverJobs.find(j => j.id === 'cron-001').paused = true;
      return { status: 200 };
    });
    await after;
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText('已暂停');
    await expect(page.locator('#toast.error')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('暂停失败：行、抽屉、计数原样回来，toast 说明原因', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    await row(page, 'cron-001').locator('.cj-main, .cj-title').first().click();
    const pane = page.locator('#cron-detail-pane');
    const whenBefore = await row(page, 'cron-001').locator('.cj-when').textContent();
    await pane.locator('[data-action="cron-pause"]').click();
    await waitHeld(state, 'pause');
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText('已暂停');

    await state.held.pause(() => ({ status: 500 }));
    await expect(page.locator('#toast')).toContainText('暂停定时任务失败');
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText(whenBefore);
    await expect(row(page, 'cron-001')).not.toHaveClass(/paused/);
    await expect(pane.locator('[data-action="cron-pause"]')).toBeVisible();
    await expect(attentionChip(page)).toHaveText('需关注 2');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('恢复：菜单点恢复立刻生效；网络失败回到已暂停并提示网络错误', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    await row(page, 'cron-002').locator('.cj-menu-btn').click();
    await page.click('.cj-menu-item[data-menu-action="resume"]');
    await waitHeld(state, 'resume');
    await expect(row(page, 'cron-002')).not.toHaveClass(/paused/);
    await expect(row(page, 'cron-002').locator('.cj-when')).not.toHaveText('已暂停');
    await expect(attentionChip(page)).toHaveText('需关注 1');

    await staleRefetch(page, mock);
    await expect(row(page, 'cron-002')).not.toHaveClass(/paused/);

    await state.held.resume(() => ({ abort: true }));
    await expect(page.locator('#toast')).toContainText('恢复定时任务失败：网络错误');
    await expect(row(page, 'cron-002').locator('.cj-when')).toHaveText('已暂停');
    await expect(attentionChip(page)).toHaveText('需关注 2');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

async function confirmDelete(page) {
  const ok = page.locator('.confirm-dialog .confirm-ok');
  await expect(ok).toBeEnabled({ timeout: 6000 });
  await ok.click();
}

test('删除：行与抽屉立刻消失、计数与 rail 红点同步，扣住期间的旧列表不复活，成功后不再出现', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    await expect(page.locator('#abnav-cron-badge')).toBeVisible();
    await row(page, 'cron-003').locator('.cj-main, .cj-title').first().click();
    const pane = page.locator('#cron-detail-pane');
    await expect(pane).toHaveClass(/is-open/);
    await pane.locator('[data-action="cron-delete"]').click();
    await confirmDelete(page);
    await waitHeld(state, 'delete');

    await expect(row(page, 'cron-003')).toHaveCount(0);
    await expect(page.locator('.cj-row')).toHaveCount(2);
    await expect(pane).not.toHaveClass(/is-open/);
    await expect(attentionChip(page)).toHaveText('需关注 1');
    // cron-003 was the only errored job: the rail's attention dot goes too.
    await expect(page.locator('#abnav-cron-badge')).toBeHidden();

    await staleRefetch(page, mock);
    await expect(row(page, 'cron-003')).toHaveCount(0);
    await expect(page.locator('#abnav-cron-badge')).toBeHidden();

    const after = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
    await state.held.delete(() => {
      state.serverJobs = state.serverJobs.filter(j => j.id !== 'cron-003');
      return { status: 200 };
    });
    await after;
    await expect(page.locator('.cj-row')).toHaveCount(2);
    await expect(page.locator('#toast.error')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('删除失败：任务回到原位置、抽屉重新打开、红点回来，toast 说明原因', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    const order = () => page.$$eval('.cj-row', els => els.map(e => e.getAttribute('data-cron-id')));
    // Sort by name so cron-001 sits in the middle and a re-append would show.
    await page.selectOption('.cron-sort-select', 'title_asc');
    const before = await order();
    await row(page, 'cron-001').locator('.cj-main, .cj-title').first().click();
    const pane = page.locator('#cron-detail-pane');
    await pane.locator('[data-action="cron-delete"]').click();
    await confirmDelete(page);
    await waitHeld(state, 'delete');
    await expect(row(page, 'cron-001')).toHaveCount(0);
    await expect(pane).not.toHaveClass(/is-open/);

    await state.held.delete(() => ({ status: 500 }));
    await expect(page.locator('#toast')).toContainText('删除定时任务失败');
    await expect(row(page, 'cron-001')).toHaveCount(1);
    expect(await order()).toEqual(before);
    await expect(pane).toHaveClass(/is-open/);
    await expect(pane.locator('[data-action="cron-delete"]')).toBeVisible();
    await expect(page.locator('#abnav-cron-badge')).toBeVisible();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('删除得到 404（已经不在了）按成功处理：不回滚、不报错', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    await row(page, 'cron-001').locator('.cj-menu-btn').click();
    await page.click('.cj-menu-item[data-menu-action="delete"]');
    await confirmDelete(page);
    await waitHeld(state, 'delete');
    await expect(row(page, 'cron-001')).toHaveCount(0);
    const after = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
    await state.held.delete(() => {
      state.serverJobs = state.serverJobs.filter(j => j.id !== 'cron-001');
      return { status: 404 };
    });
    await after;
    await expect(row(page, 'cron-001')).toHaveCount(0);
    await expect(page.locator('#toast.error')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
