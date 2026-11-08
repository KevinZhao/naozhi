// E2E: the Planner page's left column and its task cards (tasks.js,
// planner_resize.js). The column's width is dragged on its separator (or set
// with the arrow keys), kept across reloads and reset by a double click; a
// phone lays it out full width with no separator. Every task card is the
// column's width whatever its title, and its status line reads the task's
// live state: 在跑 with the latest tool call and how long the turn has run,
// 等你 with the reply that asked, 停滞 with how long it said nothing, 失败
// with why, 空闲 with the last reply — the session's own text escaped, never
// rendered. A session_state frame of a task: session refetches the list, so
// the line follows the session as it works.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const M = 60000;
const RUN = 'task:run000000001';
const ASK = 'task:ask000000001';
const STALL = 'task:stl000000001';
const FAIL = 'task:fai000000001';
const IDLE = 'task:idl000000001';

function tasks() {
  return {
    tasks: [
      { id: ASK, kind: 'session', title: 'iso 周报', state: 'waiting', by: 'health', at: iso(now - 8 * M), created: iso(now - 60 * M), health: 'waiting', health_since: iso(now - 8 * M),
        live: { running: false, reply: '用新版模板还是旧版？', last_active: iso(now - 8 * M) } },
      { id: FAIL, kind: 'session', title: 'iso checkout 重试', state: 'failed', by: 'health', why: '限流（rate_limit）', at: iso(now - 19 * M), created: iso(now - 90 * M), health: 'failed',
        live: { running: false, last_active: iso(now - 19 * M) } },
      { id: RUN, kind: 'session', title: 'iso ' + '很长的标题'.repeat(30) + 'Averyveryveryverylongunbrokenidentifierthatmustnotwidenthecard_' + 'x'.repeat(80), state: 'doing', at: iso(now - 60 * M), created: iso(now - 60 * M), health: 'active', health_since: iso(now - 12 * M),
        live: { running: true, activity: '<img src=x onerror="window.__tkXSS=1">Bash: go test ./...', reply: '先看代码', last_active: iso(now - 10000) } },
      { id: STALL, kind: 'session', title: 'iso 调研 ASR', state: 'doing', at: iso(now - 40 * M), created: iso(now - 40 * M), health: 'stalled', health_since: iso(now - 20 * M),
        live: { running: false, activity: 'WebFetch https://example.com', last_active: iso(now - 22 * M) } },
      { id: IDLE, kind: 'session', title: 'iso 写 hello', state: 'doing', at: iso(now - 30 * M), created: iso(now - 30 * M), health: 'idle',
        live: { running: false, reply: '已改完，<b>PR #3911</b>。', last_active: iso(now - 5 * M) } },
      { id: 'pr_1', kind: 'promise', title: 'iso CI 绿了告诉你', state: 'doing', at: iso(now - 7200000), created: iso(now - 7200000), close_when: 'master CI 绿' },
    ],
  };
}

function server(data) {
  return startMockServer({
    ws: true, tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
}

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  await expect(page.locator('#pl-task .tk-card').first()).toBeVisible();
}

const leftWidth = (page) => page.locator('#pl-left').evaluate((el) => Math.round(el.getBoundingClientRect().width));

test('the left column is dragged wider, kept across a reload, and reset', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await server(tasks());
  try {
    await openPlanner(page, mock);
    expect(await leftWidth(page)).toBe(340);
    const sep = page.locator('#pl-resizer');
    await expect(sep).toHaveAttribute('role', 'separator');
    await expect(sep).toHaveAttribute('aria-valuenow', '340');
    const box = await sep.boundingBox();
    const x = box.x + box.width / 2;
    await page.mouse.move(x, 400);
    await page.mouse.down();
    await page.mouse.move(x + 100, 400, { steps: 5 });
    await page.mouse.up();
    await expect.poll(() => leftWidth(page)).toBe(440);
    await expect(sep).toHaveAttribute('aria-valuenow', '440');
    // A drag past the bound stops at it: half the window, at most 640.
    await page.mouse.move(x + 100, 400);
    await page.mouse.down();
    await page.mouse.move(x + 900, 400, { steps: 5 });
    await page.mouse.up();
    await expect.poll(() => leftWidth(page)).toBe(640);
    // The keyboard moves it too.
    await sep.focus();
    await page.keyboard.press('ArrowLeft');
    await expect.poll(() => leftWidth(page)).toBe(624);
    await page.keyboard.press('Home');
    await expect.poll(() => leftWidth(page)).toBe(260);
    await page.keyboard.press('ArrowRight');
    await expect.poll(() => leftWidth(page)).toBe(276);

    await page.reload();
    await openPlanner(page, mock);
    expect(await leftWidth(page)).toBe(276);
    // A double click goes back to the default and forgets the choice.
    await page.locator('#pl-resizer').dblclick();
    await expect.poll(() => leftWidth(page)).toBe(340);
    await page.reload();
    await openPlanner(page, mock);
    expect(await leftWidth(page)).toBe(340);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('on a phone the column is full width with no separator, whatever was saved', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  await page.addInitScript(() => localStorage.setItem('nz:planner_left_w', '600'));
  const mock = await server(tasks());
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await page.click('#pl-left-toggle');
    await expect(page.locator('#pl-task .tk-card').first()).toBeVisible();
    await expect(page.locator('#pl-resizer')).toBeHidden();
    expect(await leftWidth(page)).toBe(390);
    const widths = await page.locator('#pl-task .tk-sec-doing .tk-card').evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().width)));
    expect(new Set(widths).size).toBe(1);
    expect(widths[0]).toBeLessThanOrEqual(390);
    await page.screenshot({ path: test.info().outputPath('left-phone.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('task cards are one width, their status lines say what each session does, escaped', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await server(tasks());
  try {
    await openPlanner(page, mock);
    const cards = page.locator('#pl-task .tk-sec-doing .tk-card');
    await expect(cards).toHaveCount(6);
    // One width, the column's, however long or unbroken a title is; the
    // title stops at two lines and keeps the whole in its tooltip.
    const rects = await cards.evaluateAll((els) => els.map((e) => { const r = e.getBoundingClientRect(); return { w: Math.round(r.width), h: Math.round(r.height) }; }));
    expect(new Set(rects.map((r) => r.w)).size).toBe(1);
    const inner = await page.locator('#pl-left-body').evaluate((el) => el.clientWidth);
    expect(rects[0].w).toBeLessThanOrEqual(inner);
    const run = page.locator('.tk-card[data-id="' + RUN + '"]');
    const title = run.locator('.tk-title-t');
    const lines = await title.evaluate((el) => Math.round(el.getBoundingClientRect().height / parseFloat(getComputedStyle(el).lineHeight)));
    expect(lines).toBeLessThanOrEqual(2);
    await expect(title).toHaveAttribute('title', /^iso 很长的标题/);
    // Even the long one stays compact.
    expect(Math.max(...rects.map((r) => r.h))).toBeLessThan(100);

    const line = (key) => page.locator('.tk-card[data-id="' + key + '"] .tk-status');
    await expect(line(RUN).locator('.tk-badge')).toHaveText('在跑');
    await expect(line(RUN).locator('.tk-now')).toHaveText('<img src=x onerror="window.__tkXSS=1">Bash: go test ./...');
    await expect(line(RUN).locator('.tk-time')).toHaveText('已跑 12 分钟');
    await expect(line(ASK).locator('.tk-badge')).toHaveText('等你');
    await expect(line(ASK).locator('.tk-now')).toHaveText('用新版模板还是旧版？');
    await expect(line(ASK).locator('.tk-time')).toHaveText('等了 8 分钟');
    await expect(line(STALL).locator('.tk-badge')).toHaveText('停滞');
    await expect(line(STALL).locator('.tk-time')).toHaveText('22 分钟没有输出');
    await expect(line(FAIL).locator('.tk-badge')).toHaveText('失败');
    await expect(line(FAIL).locator('.tk-now')).toHaveText('限流（rate_limit）');
    await expect(line(IDLE).locator('.tk-badge')).toHaveText('空闲');
    await expect(line(IDLE).locator('.tk-now')).toHaveText('最后回复：已改完，<b>PR #3911</b>。');
    await expect(line(IDLE).locator('.tk-time')).toHaveText('5 分钟前');
    // The session's text is text: no element came from it, nothing ran.
    await expect(page.locator('#pl-task img, #pl-task b')).toHaveCount(0);
    expect(await page.evaluate(() => window.__tkXSS)).toBeUndefined();
    // The badge's colour is the sidebar dot's for the same state.
    const color = (key) => line(key).locator('.tk-badge').evaluate((el) => getComputedStyle(el).color);
    const dot = (v) => page.evaluate((name) => { const d = document.createElement('span'); d.style.color = 'var(' + name + ')'; document.body.append(d); const c = getComputedStyle(d).color; d.remove(); return c; }, v);
    expect(await color(RUN)).toBe(await dot('--nz-status-running'));
    expect(await color(ASK)).toBe(await dot('--nz-status-warn'));
    expect(await color(STALL)).toBe(await dot('--nz-status-warn'));
    expect(await color(FAIL)).toBe(await dot('--nz-status-failed'));
    await page.locator('#pl-left').screenshot({ path: test.info().outputPath('status-lines.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the status line follows the session as it works', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const data = tasks();
  const mock = await server(data);
  try {
    await openPlanner(page, mock);
    const line = page.locator('.tk-card[data-id="' + IDLE + '"] .tk-status');
    await expect(line.locator('.tk-badge')).toHaveText('空闲');
    // The session starts a turn: its session_state frame refetches the list.
    const t = data.tasks.find((x) => x.id === IDLE);
    Object.assign(t, { health: 'active', health_since: iso(Date.now() - 3 * M), live: { running: true, activity: 'Edit hello.md', last_active: iso(Date.now()) } });
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: IDLE, state: 'running' });
    await expect(line.locator('.tk-badge')).toHaveText('在跑');
    await expect(line.locator('.tk-now')).toHaveText('Edit hello.md');
    await expect(line.locator('.tk-time')).toHaveText('已跑 3 分钟');
    // While it runs the list is refetched on its own, frame or not.
    t.live = { running: true, activity: 'Bash: go test ./...', last_active: iso(Date.now()) };
    await expect(line.locator('.tk-now')).toHaveText('Bash: go test ./...', { timeout: 15000 });
    // An open "⋯" survives the refreshes.
    const card = page.locator('.tk-card[data-id="' + IDLE + '"]');
    await card.locator('.tk-more').click();
    await expect(card.locator('.tk-stop')).toBeVisible();
    t.live = { running: false, reply: '写好了', last_active: iso(Date.now()) };
    t.health = 'idle';
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: IDLE, state: 'ready' });
    await expect(line.locator('.tk-now')).toHaveText('最后回复：写好了');
    await expect(card.locator('.tk-stop')).toBeVisible();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
