// E2E: 单独聊 (planner RFC §15.14 ⑥, D117). The Planner page's "发给
// [Planner ▾]" set to 新对话 sends that message to a new ordinary session
// (the quick session's key on session.cwd and the default agent, never to
// the Planner), labels it "单独聊：<first sentence>", leaves its line in the
// Planner conversation and opens it in the middle column; the switch goes
// back to Planner. The line is a 动态 that opens it again; the breadcrumb's
// Planner goes back; its header renames it, hands it to the Planner (its
// card and an added line, then back to the conversation) and ends it (put
// away to the history, not the sidebar's 移除). The label is what keeps it a
// 单独聊 after a reload: a new session not listed yet is waited for, a title
// is cut to a label's 128 bytes, and a label that did not go says so. A ↗
// 追问 on a Planner reply runs an ordinary aside and
// 转成单独聊 makes it one; the Alt+P drawer, under which the 追问 drawer
// would open, offers none, and switching the page to another node's Planner
// closes it. A phone shows it full screen, the breadcrumb its way back.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };
const now = Date.now();
const LONG = 'iso 修复批次还在跑，' + '其中三个 PR 在等 CI，两个在等评审。'.repeat(30);

const plannerEvents = (reply) => [
  { type: 'user', detail: '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\niso 现在都在干什么？', time: now - 120000, uuid: 'as-u1' },
  { type: 'text', detail: reply || LONG, time: now - 110000, uuid: 'as-t1' },
  { type: 'result', time: now - 109000, uuid: 'as-r1' },
];

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.evaluate(() => document.getElementById('abnav-planner').click());
  await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
  await expect(page.locator('#pl-stream .event.text')).toHaveCount(1);
}

const sent = (mock) => mock.sendCalls.map((b) => JSON.parse(b));

test('desktop: 新对话 opens a 单独聊 in the middle column, its line reopens it, and it closes', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true });
  try {
    await openPlanner(page, mock);
    const to = page.locator('#pl-to');
    await expect(to).toBeVisible();
    await expect(page.locator('#pl-to-wrap')).toContainText('发给');
    await expect(to).toHaveValue('planner');
    await expect(page.locator('#pl-input')).toHaveAttribute('placeholder', '交代、追问…');

    await to.selectOption('new');
    await expect(page.locator('#pl-target-note')).toHaveText('开一个单独聊：普通会话，不进 Planner 的上下文');
    await expect(page.locator('#pl-input')).toHaveAttribute('placeholder', '问点别的…');
    await page.fill('#pl-input', 'iso BTC 现在多少钱？顺便看看 ETH');
    await page.press('#pl-input', 'Enter');

    // One send, to a new ordinary session on session.cwd, none to the Planner.
    await expect.poll(() => sent(mock).length).toBe(1);
    const body = sent(mock)[0];
    expect(body.key).toMatch(/^dashboard:direct:\d{4}-\d\d-\d\d-\d{6}-\d+-workspace:general$/);
    expect(body.text).toBe('iso BTC 现在多少钱？顺便看看 ETH');
    expect(body.workspace).toBe('/home/user/workspace');
    expect(mock.plannerSendCalls).toHaveLength(0);
    const key = body.key;
    await expect.poll(() => mock.plannerAsideCalls).toEqual([{ key, title: 'iso BTC 现在多少钱？' }]);

    // Opened in the middle column under its title; the switch is back on Planner.
    const pane = page.locator('#pl-sess');
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso BTC 现在多少钱？');
    await expect(page.locator('#pl-target-to')).toHaveText('→ 单独聊：iso BTC 现在多少钱？');
    await expect(page.locator('#pl-sess-stream .event.user')).toContainText('iso BTC 现在多少钱？顺便看看 ETH');
    await expect(page.locator('#pl-to-wrap')).toBeHidden();
    await expect(page.locator('#pl-sess-hand')).toBeVisible();
    await expect(page.locator('#pl-sess-end')).toBeVisible();
    await expect(page.locator('#pl-sess-rename')).toBeVisible();
    await expect(page.locator('#pl-sess-stop')).toBeHidden();

    // The title is renamed from the breadcrumb, 单独聊： kept; the dialog
    // takes no more than fits a label's 128 bytes with it.
    await page.click('#pl-sess-rename');
    await page.fill('.prompt-input', '长'.repeat(40));
    await page.click('.prompt-ok');
    const fitted = '单独聊：' + '长'.repeat(30);
    await expect(page.locator('#pl-sess-name')).toHaveText(fitted);
    expect(mock.labelCalls.at(-1)).toEqual({ key, label: fitted, status: 200 });
    await page.click('#pl-sess-rename');
    await page.fill('.prompt-input', '比特币行情');
    await page.click('.prompt-ok');
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：比特币行情');

    // Back to the conversation: the line it left folds into a 动态 row.
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await expect(pane).toBeHidden();
    await expect(to).toBeVisible();
    await expect(to).toHaveValue('planner');
    const row = page.locator('#pl-stream .pl-dyn');
    await expect(row).toHaveCount(1, { timeout: 15000 });
    await expect(row.locator('.pl-dyn-h')).toContainText('1 条动态（单独聊）');
    await row.locator('.pl-dyn-h').click();
    const line = row.locator('.pl-dyn-line');
    await expect(line.locator('.pl-dyn-src')).toHaveText('单独聊');
    await expect(line.locator('.pl-dyn-one')).toHaveText('iso BTC 现在多少钱？');
    await line.click();
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：比特币行情');

    // 全部 shows the line itself, which opens it too.
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await page.click('.pl-head > .pl-layer [data-layer="all"]');
    const note = page.locator('#pl-stream .pl-aside');
    await expect(note).toHaveText('💬 你开了单独聊：iso BTC 现在多少钱？ ▸');
    await note.click();
    await expect(pane).toBeVisible();
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await page.click('.pl-head > .pl-layer [data-layer="talk"]');
    await page.locator('#pl-stream .pl-dyn-line').click();
    await expect(pane).toBeVisible();

    // 交给 Planner: its card with the line added, a turn of yours; back to the conversation.
    await page.click('#pl-sess-hand');
    await page.fill('.prompt-input', 'iso 记下结论');
    await page.click('.prompt-ok');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 记下结论', ref: key }]);
    await expect(pane).toBeHidden();

    // 结束: put away to the history, its log kept, not the sidebar's 移除.
    await page.locator('#pl-stream .pl-dyn-line').click();
    await expect(pane).toBeVisible();
    await page.click('#pl-sess-end');
    await expect.poll(() => mock.plannerAsideEndCalls).toEqual([{ key }]);
    await expect(pane).toBeHidden();
    await expect(page.locator('#toast')).toHaveText('单独聊已结束，移到了历史');
    await expect(page.locator(`.session-card[data-key="${key}"]`)).toHaveCount(0);
    expect(mock.deleteCalls).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('desktop: a new session listed only after a while still gets its label; one that never does says so', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  // The spawn puts the key in the list after the first 4 tries.
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true, labelMissing: 4 });
  try {
    await openPlanner(page, mock);
    await page.locator('#pl-to').selectOption('new');
    await page.fill('#pl-input', 'iso 慢慢起来的会话');
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => sent(mock).length).toBe(1);
    const key = sent(mock)[0].key;
    await expect.poll(() => mock.labelCalls.map((c) => c.status), { timeout: 20000 }).toEqual([404, 404, 404, 404, 200]);
    expect(mock.labelCalls.at(-1)).toEqual({ key, label: '单独聊：iso 慢慢起来的会话', status: 200 });
    // After a reload it is still a 单独聊: the label says so.
    await page.reload();
    await waitForWs(page, 'CONNECTED');
    await page.evaluate(() => document.getElementById('abnav-planner').click());
    await page.locator('#pl-stream .pl-dyn-h').click();
    await page.locator('#pl-stream .pl-dyn-line').click();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 慢慢起来的会话');
    await expect(page.locator('#pl-sess-hand')).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }

  const ctx2 = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page2 = await ctx2.newPage();
  const mock2 = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true, labelStatus: 502 });
  try {
    await openPlanner(page2, mock2);
    await page2.locator('#pl-to').selectOption('new');
    await page2.fill('#pl-input', 'iso 标不上的会话');
    await page2.press('#pl-input', 'Enter');
    await expect(page2.locator('#toast')).toHaveText('单独聊已开，但没能标上标题，刷新后它会显示成普通会话');
    expect(mock2.labelCalls.map((c) => c.status)).toEqual([502]);
  } finally {
    await ctx2.close();
    mock2.server.close();
  }
});

test('desktop: the Alt+P drawer, under which the 追问 drawer would open, offers no ↗ 追问', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true });
  try {
    await openPlanner(page, mock);
    const reply = page.locator('#pl-stream .event.text').first();
    await reply.hover();
    await expect(reply.locator('.event-ask-btn')).toBeVisible();
    await page.evaluate(() => document.getElementById('abnav-chat').click());
    await page.keyboard.press('Alt+KeyP');
    await expect(page.locator('#planner-drawer')).toBeVisible();
    const inDrawer = page.locator('#planner-drawer .event.text').first();
    await expect(inDrawer).toBeVisible();
    await inDrawer.hover();
    await expect(inDrawer.locator('.event-ask-btn')).toBeHidden();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('desktop: 新对话 sent from the Alt+P drawer opens the 单独聊 on the Planner page', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.keyboard.press('Alt+KeyP');
    const drawer = page.locator('#planner-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('#pl-stream .event.text')).toHaveCount(1);
    await drawer.locator('#pl-to').selectOption('new');
    await drawer.locator('#pl-input').fill('iso 抽屉里问点别的');
    await drawer.locator('#pl-input').press('Enter');
    await expect.poll(() => sent(mock).length).toBe(1);
    // The drawer has no middle column: the page takes it, the drawer goes,
    // and the input box says it sends to the 单独聊.
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(drawer).toBeHidden();
    await expect(page.locator('#planner-main #pl-sess')).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 抽屉里问点别的');
    await expect(page.locator('#pl-events')).toBeHidden();
    expect(mock.plannerSendCalls).toHaveLength(0);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('desktop: switching the page to another node\'s Planner closes the 追问 on this one\'s reply', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const NODE = 'pc';
  const s = defaultSessions();
  s.nodes = { local: { display_name: 'Mac', status: 'ok' }, [NODE]: { display_name: 'EC2', status: 'ok', caps: ['files', 'planner', 'subscribe-history'] } };
  const remote = { planner, tasks: { tasks: [] }, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    custody: { paused: false, sessions: [] }, events: { [PLANNER]: [{ type: 'text', detail: 'iso 那边的 Planner。', time: now - 5000, uuid: 'rn-t1' }] } };
  const mock = await startMockServer({ ws: true, sessions: s, plannerEvents: plannerEvents(), planner, remote: { [NODE]: remote } });
  try {
    await openPlanner(page, mock);
    const reply = page.locator('#pl-stream .event.text').first();
    await reply.hover();
    await reply.locator('.event-ask-btn').click();
    const drawer = page.locator('#aside-drawer');
    await expect(drawer).toHaveClass(/visible/);
    await page.locator('#pl-host select').selectOption(NODE);
    await expect(drawer).not.toHaveClass(/visible/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('desktop: a ↗ 追问 on a Planner reply is an ordinary aside, and 转成单独聊 makes it one', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const scratch = 'dashboard:scratch:2026-01-01-130000-9:myproject';
  // The scratch's log as the drawer polls it, after the bubble it shows of
  // what is sent.
  const keyEvents = { [scratch]: [
    { type: 'user', detail: 'iso 为什么这么慢？还要多久', time: now + 600000, uuid: 'sx-u1' },
    { type: 'text', detail: 'iso 在等 CI。', time: now + 601000, uuid: 'sx-t1' },
  ] };
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true, keyEvents });
  try {
    await openPlanner(page, mock);
    const reply = page.locator('#pl-stream .event.text').first();
    await reply.hover();
    const ask = reply.locator('.event-ask-btn');
    await expect(ask).toHaveAttribute('data-key', PLANNER);
    await ask.click();
    await expect.poll(() => mock.scratchOpenCalls.map((b) => b.source_key)).toEqual([PLANNER]);
    const drawer = page.locator('#aside-drawer');
    await expect(drawer).toHaveClass(/visible/);
    const toAside = page.locator('#ad-aside');
    await expect(toAside).toBeVisible();
    await expect(toAside).toBeDisabled();

    await page.fill('#ad-input', 'iso 为什么这么慢？还要多久');
    await page.press('#ad-input', 'Enter');
    await expect(toAside).toBeEnabled();
    // On a Planner reply 转成单独聊 stands in for 保存为正式会话.
    await expect(page.locator('#ad-save')).toBeHidden();

    await toAside.click();
    const key = 'dashboard:direct:aside-0123456789abcdef:general';
    await expect.poll(() => mock.scratchPromoteCalls).toEqual(['scr-0001']);
    await expect.poll(() => mock.plannerAsideCalls).toEqual([{ key, title: 'iso 为什么这么慢？' }]);
    await expect(drawer).not.toHaveClass(/visible/);
    await expect(page.locator('#pl-sess')).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 为什么这么慢？');
    await expect(page.locator('#pl-sess-hand')).toBeVisible();

    // The drawer opened on the Planner page goes when the page does.
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await reply.hover();
    await reply.locator('.event-ask-btn').click();
    await expect(drawer).toHaveClass(/visible/);
    await page.evaluate(() => document.getElementById('abnav-chat').click());
    await expect(drawer).not.toHaveClass(/visible/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: a 单独聊 takes the screen, the breadcrumb at its top left goes back', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true });
  try {
    await openPlanner(page, mock);
    await page.locator('#pl-to').selectOption('new');
    await page.fill('#pl-input', 'iso Mac mini 该买哪款？');
    await page.locator('#pl-send').click();
    const pane = page.locator('#pl-sess');
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso Mac mini 该买哪款？');
    // Full screen: the conversation and the left block are out of view,
    // the way back at the top left.
    await expect(page.locator('#pl-events')).toBeHidden();
    await expect(page.locator('.pl-left')).toBeHidden();
    const box = await pane.boundingBox();
    expect(box.width).toBeGreaterThan(380);
    expect(box.y).toBeLessThan(10);
    const back = page.locator('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    const b = await back.boundingBox();
    expect(b.x).toBeLessThan(40);
    expect(b.y).toBeLessThan(40);
    expect(await page.locator('#pl-sess-hand').evaluate((el) => el.innerText.trim())).toBe('交给');
    await back.click();
    await expect(pane).toBeHidden();
    await expect(page.locator('#pl-events')).toBeVisible();
    await expect(page.locator('.pl-left')).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('结束 takes the 单独聊 out of 你开的 at once, not at the next read of 任务', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const key = 'dashboard:direct:2026-01-01-120000-1-workspace:general';
  const s = defaultSessions();
  s.sessions.push({ key, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude', user_label: '单独聊：iso 今天几号？',
    workspace: '/home/user/workspace', node: 'local', last_active: now - 60000, created_at: now - 90000 });
  const at = new Date(now - 60000).toISOString();
  const tasks = { tasks: [{ id: key, kind: 'session', group: 'yours', title: '单独聊：iso 今天几号？', state: 'doing', at, health: 'idle', health_since: at,
    live: { running: false, reply: '11 月 16 日', last_active: at } }] };
  const mock = await startMockServer({ ws: true, sessions: s, tasks, plannerEvents: plannerEvents(), planner, custody: { paused: false, sessions: [] } });
  try {
    await openPlanner(page, mock);
    const card = page.locator(`#pl-task .tk-card[data-id="${key}"]`);
    await expect(card).toHaveCount(1);
    await card.locator('.tk-title').click();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 今天几号？');
    await page.click('#pl-sess-end');
    await expect.poll(() => mock.plannerAsideEndCalls).toEqual([{ key }]);
    await expect(page.locator('#toast')).toHaveText('单独聊已结束，移到了历史');
    // Well inside the 10 s the group refetches in only while a turn runs.
    await expect(card).toHaveCount(0, { timeout: 3000 });
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
