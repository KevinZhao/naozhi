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
// serverJobs. While state.holdGets is set, each list GET is held too: it is
// pushed on state.heldGets and answered, on release(), with the list as it
// was when the request arrived (what a slow server would have read).
async function setup(browser, jobList = jobs()) {
  const mock = await startMockServer({ ws: true, cronJobs: jobList });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const state = { serverJobs: JSON.parse(JSON.stringify(jobList)), held: {}, holdGets: false, heldGets: [] };
  await page.route(/\/api\/cron(\?|$)/, async (route) => {
    const req = route.request();
    if (req.method() === 'GET') {
      const body = JSON.stringify({ jobs: state.serverJobs, recent_runs_cap: 5 });
      const answer = () => route.fulfill({ status: 200, contentType: 'application/json', body });
      if (state.holdGets) return new Promise((resolve) => state.heldGets.push({ release: () => answer().then(resolve) }));
      return answer();
    }
    if (req.method() === 'DELETE') return holdRoute(state, 'delete', route);
    return route.fallback();
  });
  await page.route(/\/api\/cron\/(pause|resume)$/, (route) =>
    holdRoute(state, /pause$/.test(route.request().url()) ? 'pause' : 'resume', route));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await expect(page.locator('.cj-row')).toHaveCount(jobList.length);
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

// nextHeldGet waits for the next list GET held under state.holdGets.
async function nextHeldGet(state) {
  await expect.poll(() => state.heldGets.length).toBeGreaterThan(0);
  return state.heldGets.shift();
}

// runEnded sends a cron run_ended frame for another job, which makes the page
// refetch the list.
function runEnded(mock) {
  for (const c of mock.wsConnections) {
    c.send({ type: 'run_ended', subsystem: 'cron', owner_id: 'cron-002', run_id: 'run-x', state: 'skipped', ended_at: Date.now() });
  }
}

// staleRefetch makes the page refetch the list while the request is held: a
// cron run_ended frame for another job does fetchCronJobs. Resolves once the
// (pre-change) list response has been handled.
async function staleRefetch(page, mock) {
  const resp = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
  runEnded(mock);
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

    // The refetch after the failure is held: what shows until it answers is
    // the local rollback, not the server's list.
    state.holdGets = true;
    await state.held.pause(() => ({ status: 500 }));
    const refetch = await nextHeldGet(state);
    await expect(page.locator('#toast')).toContainText('暂停定时任务失败');
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText(whenBefore);
    await expect(row(page, 'cron-001')).not.toHaveClass(/paused/);
    await expect(pane.locator('[data-action="cron-pause"]')).toBeVisible();
    await expect(attentionChip(page)).toHaveText('需关注 2');
    state.holdGets = false;
    await refetch.release();
    await expect(row(page, 'cron-001')).not.toHaveClass(/paused/);
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

    state.holdGets = true;
    await state.held.resume(() => ({ abort: true }));
    const refetch = await nextHeldGet(state);
    await expect(page.locator('#toast')).toContainText('恢复定时任务失败：网络错误');
    await expect(row(page, 'cron-002').locator('.cj-when')).toHaveText('已暂停');
    await expect(attentionChip(page)).toHaveText('需关注 2');
    state.holdGets = false;
    await refetch.release();
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
    // The rows' order comes from the sort, so the position is read from the
    // cache itself; cron-002 sits in the middle of it, where a re-append
    // would show.
    const cacheOrder = () => page.evaluate(async () =>
      (await import('/static/cron_state.js')).cronStore.jobs.map(j => j.id));
    const before = await cacheOrder();
    expect(before).toEqual(['cron-001', 'cron-002', 'cron-003']);
    await row(page, 'cron-002').locator('.cj-main, .cj-title').first().click();
    const pane = page.locator('#cron-detail-pane');
    await pane.locator('[data-action="cron-delete"]').click();
    await confirmDelete(page);
    await waitHeld(state, 'delete');
    await expect(row(page, 'cron-002')).toHaveCount(0);
    await expect(pane).not.toHaveClass(/is-open/);

    // The refetch after the failure is held: the job, its place and its drawer
    // come back from the local copy.
    state.holdGets = true;
    await state.held.delete(() => ({ status: 500 }));
    const refetch = await nextHeldGet(state);
    await expect(page.locator('#toast')).toContainText('删除定时任务失败');
    await expect(row(page, 'cron-002')).toHaveCount(1);
    expect(await cacheOrder()).toEqual(before);
    await expect(pane).toHaveClass(/is-open/);
    await expect(pane.locator('[data-action="cron-delete"]')).toBeVisible();
    await expect(attentionChip(page)).toHaveText('需关注 2');
    state.holdGets = false;
    await refetch.release();
    await expect(row(page, 'cron-002')).toHaveCount(1);
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

test('暂停：比它早出发、却比确认后的补拉晚落地的旧列表也不翻回', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser);
  try {
    // A list fetch leaves before the pause and is slow to answer.
    state.holdGets = true;
    runEnded(mock);
    const early = await nextHeldGet(state);
    state.holdGets = false;

    await row(page, 'cron-001').locator('.cj-menu-btn').click();
    await page.click('.cj-menu-item[data-menu-action="pause"]');
    await waitHeld(state, 'pause');
    await expect(row(page, 'cron-001')).toHaveClass(/paused/);

    // The pause is applied and the refetch after it lands first.
    const after = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
    await state.held.pause(() => {
      state.serverJobs.find(j => j.id === 'cron-001').paused = true;
      return { status: 200 };
    });
    await after;
    await expect(row(page, 'cron-001')).toHaveClass(/paused/);

    // Then the early fetch lands, carrying the list from before the pause.
    const late = page.waitForResponse(r => /\/api\/cron\?compact=1/.test(r.url()));
    await early.release();
    await late;
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r())));
    await expect(row(page, 'cron-001')).toHaveClass(/paused/);
    await expect(row(page, 'cron-001').locator('.cj-when')).toHaveText('已暂停');
    await expect(attentionChip(page)).toHaveText('需关注 3');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

function sixJobs() {
  const now = Date.now();
  const base = { work_dir: '/home/user/workspace/myproject', recent_runs: [], paused: false, next_run: now + 3600000 };
  const titles = ['备份 数据库', '备份 日志', '日报', '周报', '清理', '巡检'];
  return titles.map((title, i) => ({ ...base, id: 'cron-10' + i, schedule: '0 ' + (i + 1) + ' * * *',
    title, prompt: title + ' job', created_at: now - (i + 1) * 1000 }));
}

test('搜索中删除使任务降到 5 个：搜索框带着关键词留下，可以清空', async ({ browser }) => {
  const { mock, ctx, page, state } = await setup(browser, sixJobs());
  try {
    const input = page.locator('#cron-search-input');
    await input.fill('备份');
    await expect(page.locator('.cj-row')).toHaveCount(2);

    await row(page, 'cron-100').locator('.cj-menu-btn').click();
    await page.click('.cj-menu-item[data-menu-action="delete"]');
    await confirmDelete(page);
    await waitHeld(state, 'delete');
    await expect(row(page, 'cron-100')).toHaveCount(0);
    // Five jobs left and the query still filters them: the box stays with it.
    await expect(page.locator('.cj-row')).toHaveCount(1);
    await expect(input).toBeVisible();
    await expect(input).toHaveValue('备份');

    await state.held.delete(() => {
      state.serverJobs = state.serverJobs.filter(j => j.id !== 'cron-100');
      return { status: 200 };
    });
    await expect(input).toHaveValue('备份');
    await page.click('[data-action="cron-search-clear"]');
    await expect(page.locator('.cj-row')).toHaveCount(5);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('正在输入搜索词时列表重绘不换掉搜索框（输入法组字不被打断）', async ({ browser }) => {
  const { mock, ctx, page } = await setup(browser, sixJobs());
  try {
    const input = page.locator('#cron-search-input');
    await input.click();
    await input.pressSequentially('备');
    await page.evaluate(() => { document.getElementById('cron-search-input')._nzMark = 1; });
    await staleRefetch(page, mock);
    expect(await page.evaluate(() => {
      const el = document.getElementById('cron-search-input');
      return { same: el._nzMark === 1, focused: document.activeElement === el, value: el.value };
    })).toEqual({ same: true, focused: true, value: '备' });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
