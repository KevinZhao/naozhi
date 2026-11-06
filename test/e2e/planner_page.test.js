// E2E: the Planner page (planner_view.js, planner RFC §10 D77, §15.1 PR ⑥,
// §15.7): the rail entry and its badge (questions, decisions and errors),
// the left column's 通知 cards (closing one; decisions and questions first
// and labelled, each thing said once) and 任务 (refetched after a Planner
// turn), the read-only planner:main stream (no snapshot block, no 追问, the
// pushed "收到，正在看" by the input box, no idle refetch on the inclusive
// `after` replay), the morning briefs in it by time (D91), the header's "⋯"
// menu (push pause, 停用 Planner, browser notifications only while not yet
// asked) and its 推送暂停到 line, a new output only counting on the 工作台
// button (D91: the workbench never opens by itself nor folds the left
// column); the phone layout's left block, which unfolded takes the stream's
// place and folds for a session or #planner, and ← or the system back from a
// session opened there going back to the page; the planner.enabled=false
// explanation.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

function inbox() {
  return {
    open: [
      { id: 'in_err', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3 * 3600000), state: 'open', source: 'health:cron:nightly' },
      { id: 'in_q', kind: 'question', level: 'timely', unit: SESSION, title: 'iso 会话在等你回复', why: '有提问等你回答超过 10 分钟', created: iso(now - 12 * 60000), state: 'open', source: 'waiting:' + SESSION, auto_close: true },
      { id: 'in_d', kind: 'decision', level: 'timely', title: '周报用哪个模板？', why: 'Planner 问你', created: iso(now - 5 * 60000), state: 'open', source: 'planner:d1', decision: { handler: 'ask', actions: ['模板 A', '模板 B'] } },
    ],
    closed: [],
    counts: { open: 3, badge: 3 },
  };
}

function plannerEvents() {
  return [
    { type: 'user', detail: '<naozhi-snapshot at="2026-11-04 09:00 CST">\n等你（1）：\n- [提问] iso 会话在等你回复\n</naozhi-snapshot>\n\n现在都在干什么？', time: now - 60000, uuid: 'pl-u1' },
    { type: 'text', detail: '早报：' + '计划'.repeat(300), time: now - 58000, uuid: 'pl-t0' },
    { type: 'text', detail: '两个会话在跑，一个在等你回复。', time: now - 55000, uuid: 'pl-t1' },
  ];
}

const output = (p) => ({ key: PLANNER, time: iso(Date.now()), type: 'output', summary: p, path: p, dir: '/tmp/planner', root: 'session:' + PLANNER });

async function routePreview(page) {
  await page.route('**/api/projects/file?*', (route) => {
    const p = new URL(route.request().url()).searchParams.get('path');
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ content: 'content of ' + p, size: 20, mime: 'text/plain' }) });
  });
}

test('desktop: badge, 等你 cards, stream, controls, and the workbench counts outputs', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const outputs = [];
  const events = plannerEvents();
  const box = inbox();
  const tasks = { tasks: [{ id: 'pr_1', kind: 'promise', title: 'iso 两分钟后提醒你看 hello.md', due: iso(now + 120000), state: 'remind', at: iso(now - 60000), created: iso(now - 60000) }] };
  const mock = await startMockServer({
    ws: true, outputs, plannerEvents: events, inbox: box, tasks,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  await routePreview(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    // The badge is seeded from GET /api/inbox once the socket is up; the
    // existing rail stays as it was.
    await expect(page.locator('#abnav-planner-count')).toHaveText('3');
    for (const id of ['chat', 'assets', 'files', 'cron', 'system', 'settings']) {
      await expect(page.locator('#abnav-' + id)).toBeVisible();
    }
    await expect(page.locator('#abnav-chat')).toHaveClass(/active/);

    await page.click('#abnav-planner');
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(page.locator('#abnav-planner')).toHaveAttribute('aria-pressed', 'true');
    const cards = page.locator('#pl-wait .pl-card');
    await expect(cards).toHaveCount(3);
    // The column lists what waits on you to act, the badge's items (D94).
    await expect(page.locator('#pl-wait-h')).toHaveText('等你');
    // Decisions first, then questions, then errors.
    await expect(cards.nth(0).locator('.pl-kind')).toHaveText('待你决定');
    await expect(cards.nth(0).locator('.pl-card-acts button')).toHaveText(['模板 A', '模板 B']);
    const q = page.locator('.pl-card[data-id="in_q"]');
    await expect(q.locator('.pl-kind')).toHaveText('提问');
    await expect(q.locator('.pl-wait')).toHaveText('等了 12 分钟');
    await expect(q.locator('.pl-link')).toHaveText('打开');
    await expect(page.locator('#pl-task .tk-card')).toHaveText(/iso 两分钟后提醒你看 hello\.md/);
    // Running goes unsaid; the seldom used controls are folded in "⋯".
    await expect(page.locator('#pl-status')).toHaveText('');
    await expect(page.locator('#pl-tools')).toBeHidden();
    await expect(page.locator('#pl-paused')).toBeHidden();
    await expect(page.locator('#pl-stream .event')).toHaveCount(3);
    await expect(page.locator('#pl-stream')).toContainText('两个会话在跑');
    // The user bubble shows what was typed, not the gateway's snapshot; a
    // long reply has 复制 and a short one none (only a long passage does);
    // neither has 追问 (that asks about the selected session).
    await expect(page.locator('#pl-stream .event.user')).toHaveText(/现在都在干什么？/);
    await expect(page.locator('#pl-stream')).not.toContainText('naozhi-snapshot');
    await expect(page.locator('#pl-stream .event.text[data-uuid="pl-t0"] .event-copy-btn')).toHaveCount(1);
    await expect(page.locator('#pl-stream .event-copy-btn')).toHaveCount(1);
    await expect(page.locator('#pl-stream .event-ask-btn')).toHaveCount(0);
    await expect(page.locator('#pl-note')).toBeHidden();

    // Closing a card.
    await page.click('.pl-card[data-id="in_err"] .pl-close');
    await expect(cards).toHaveCount(2);
    expect(mock.inboxCalls[0]).toEqual({ path: '/api/inbox/close', method: 'POST', body: { id: 'in_err' } });
    await expect(page.locator('#abnav-planner-count')).toHaveText('2');
    // Another tab answered the decision: the count frame refetches the list.
    box.open = box.open.filter((it) => it.id !== 'in_d');
    box.counts = { open: 1, badge: 1 };
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'inbox_count', open: 1, badge: 1 });
    await expect(page.locator('#abnav-planner-count')).toHaveText('1');
    await expect(cards).toHaveCount(1);

    // Pause pushes for an hour from "⋯": the header says until when.
    await page.click('#pl-more > summary');
    await expect(page.locator('#pl-tools')).toBeVisible();
    await page.screenshot({ path: test.info().outputPath('planner-desktop-menu.png') });
    await page.click('[data-action="planner-pause"][data-for="1h"]');
    await expect(page.locator('#pl-paused')).toContainText('已暂停到');
    await expect(page.locator('#pl-tools')).toBeHidden();
    expect(mock.inboxCalls[1]).toMatchObject({ path: '/api/inbox/pause', method: 'PUT', body: { for: '1h' } });
    await page.locator('.pl-center > .pl-head').first().screenshot({ path: test.info().outputPath('planner-desktop-paused.png') });
    await page.click('#pl-paused [data-action="planner-pause-resume"]');
    await expect(page.locator('#pl-paused')).toBeHidden();
    // 自定义: the hours input shows in the still open menu.
    await page.click('#pl-more > summary');
    await page.click('[data-action="planner-pause-custom"]');
    await expect(page.locator('#pl-pause-hours')).toBeVisible();
    await page.fill('#pl-pause-hours', '2');
    await page.click('[data-action="planner-pause-custom-ok"]');
    await expect(page.locator('#pl-paused')).toContainText('已暂停到');
    expect(mock.inboxCalls[3]).toMatchObject({ path: '/api/inbox/pause', method: 'PUT', body: { for: '2h' } });
    await page.click('#pl-paused [data-action="planner-pause-resume"]');
    await expect(page.locator('#pl-paused')).toBeHidden();
    await page.click('#pl-more > summary');
    await expect(page.locator('[data-action="planner-pause"][data-for="today"]')).toBeVisible();
    // A click elsewhere folds the menu.
    await page.click('#pl-stream');
    await expect(page.locator('#pl-tools')).toBeHidden();

    // A new output of planner:main only counts on the 工作台 button.
    const wbBtn = page.locator('.pl-head [data-action="workbench-toggle"]').first();
    await expect.poll(() => mock.outputsCalls.filter((k) => k === PLANNER).length).toBeGreaterThan(0);
    await expect(wbBtn).toHaveText('工作台');
    outputs.unshift(output('handoff/2026-11-04.md'));
    tasks.tasks.push({ id: 'pr_2', kind: 'promise', title: 'iso 明早告诉你结果', state: 'doing', at: iso(now), created: iso(now) });
    events.push({ type: 'result', summary: 'done', time: Date.now(), uuid: 'pl-r1' });
    await expect(wbBtn).toHaveText('工作台产出 1', { timeout: 10000 });
    await expect(page.locator('#fv-drawer')).not.toHaveClass(/fv-open/);
    // The turn's end refetches 任务: a promise it registered shows.
    await expect(page.locator('#pl-task .tk-card')).toHaveCount(2);
    // Opened from the button, it leaves the left column as it is.
    await wbBtn.click();
    await expect(page.locator('#fv-drawer')).toHaveClass(/fv-open/);
    await expect(page.locator('#wb-outputs .files-row')).toHaveCount(1);
    await expect(page.locator('.pl-left')).toHaveCSS('width', '280px');
    await expect(page.locator('#pl-wait .pl-card').first()).toBeVisible();
    await page.screenshot({ path: test.info().outputPath('planner-desktop-workbench.png') });
    await page.click('#fv-btn-close');
    await expect(page.locator('#fv-drawer')).not.toHaveClass(/fv-open/);

    // Idle: every poll replays the trailing result (inclusive `after`), which
    // must not refetch the outputs again.
    const idle = mock.outputsCalls.length;
    await page.waitForTimeout(7000);
    expect(mock.outputsCalls.length).toBe(idle);

    // The subscription: subscribed after the last event, a pushed receipt
    // (never logged) shows by the input box, not in the stream (D91), and
    // polling stops once it is live.
    const sub = conn.messages.find((m) => m.type === 'subscribe' && m.key === PLANNER);
    expect(sub).toMatchObject({ after: now - 55000 });
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
    await expect(page.locator('#pl-target-note')).toHaveText('收到，正在看');
    await expect(page.locator('#pl-stream .event.system')).toHaveCount(0);

    await page.click('#pl-more > summary');
    await page.check('#pl-stop');
    await expect(page.locator('#pl-status')).toHaveText('已停用');
    await expect(page.locator('#pl-note')).toContainText('Planner 已停用');
    expect(mock.plannerStateCalls).toEqual([{ stopped: true }]);
    await page.screenshot({ path: test.info().outputPath('planner-desktop.png') });

    // Leaving the page and opening a session from a card.
    await page.click('.pl-card[data-id="in_q"] .pl-link');
    await page.waitForFunction((key) => window.nz.test.selectedKey === key, SESSION);
    await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
    await expect(page.locator('#planner-main')).toBeHidden();
    await expect.poll(() => conn.messages.some((m) => m.type === 'unsubscribe' && m.key === PLANNER)).toBe(true);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: the left column is a collapsible block above the stream', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  const tasks = { tasks: [{ id: SESSION, kind: 'session', title: 'iso 修登录页', state: 'doing', at: iso(now - 60000), created: iso(now - 60000) }] };
  const mock = await startMockServer({
    ws: true, plannerEvents: plannerEvents(), inbox: inbox(), tasks,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('#abnav-planner-count')).toHaveText('3');
    await page.click('#abnav-planner');
    const toggle = page.locator('#pl-left-toggle');
    await expect(toggle).toBeVisible();
    // Folded, it says what it holds (Q15): what waits on you, the tasks in progress.
    await expect(toggle).toHaveText('等你 3 · 任务 1');
    await expect(page.locator('#pl-wait')).toBeHidden();
    await expect(page.locator('#pl-stream')).toContainText('两个会话在跑');
    await page.screenshot({ path: test.info().outputPath('planner-mobile.png') });
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(3);
    await expect(page.locator('#pl-wait .pl-card').first()).toBeInViewport();
    // Unfolded, the cards take the stream's place rather than squeezing it.
    await expect(page.locator('#pl-center')).toBeHidden();
    await page.screenshot({ path: test.info().outputPath('planner-mobile-open.png') });
    // A 任务 title opens its session in the middle column: the block folds.
    await page.locator('#pl-task .tk-open').tap();
    await expect(page.locator('#pl-sess')).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await page.screenshot({ path: test.info().outputPath('planner-mobile-task-session.png') });
    await page.locator('.pl-crumb .pl-link').tap();
    await expect(page.locator('#pl-events')).toBeVisible();
    // A session opened from a card: ← and the system back both go back to
    // the Planner page, not to the session list.
    await toggle.click();
    for (const back of [() => page.locator('.main-header .btn-mobile-back').tap(), () => page.goBack()]) {
      await page.locator('.pl-card[data-id="in_q"] .pl-link').tap();
      await page.waitForFunction((key) => window.nz.test.selectedKey === key, SESSION);
      await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
      await back();
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      await expect(page.locator('#planner-main')).toBeVisible();
    }
    // The brief's #planner lands on the conversation, the block left unfolded.
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await page.evaluate(() => { location.hash = 'planner'; });
    await expect(page.locator('#pl-stream')).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    // Opened from the session list, ← is the list's again.
    await page.locator('#abnav-chat').click();
    await page.locator('.session-card[data-key="' + SESSION + '"]').tap();
    await page.locator('.main-header .btn-mobile-back').tap();
    await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
    await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// On a narrow phone a pause until tomorrow and a status chip still leave
// the "⋯" menu on screen, and the time whole.
test('phone: the 已暂停到 line leaves "⋯" on screen', async ({ browser }) => {
  for (const [width, planner] of [[360, { alive: false }], [375, { stopped: true }]]) {
    const ctx = await browser.newContext({ viewport: { width, height: 760 }, isMobile: true, hasTouch: true });
    const page = await ctx.newPage();
    const box = inbox();
    const until = new Date(now + 86400000);
    until.setHours(9, 0, 0, 0);
    box.paused_until = until.toISOString();
    const mock = await startMockServer({
      ws: true, plannerEvents: plannerEvents(), inbox: box,
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true, ...planner },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      await expect(page.locator('#pl-paused')).toContainText('已暂停到');
      await expect(page.locator('#pl-status')).toHaveText(planner.stopped ? '已停用' : '启动中');
      const head = page.locator('#planner-main .pl-center > .pl-head');
      const [sw, cw] = await head.evaluate((el) => [el.scrollWidth, el.clientWidth]);
      expect(sw).toBeLessThanOrEqual(cw);
      const more = await page.locator('#pl-more > summary').boundingBox();
      expect(more.x + more.width).toBeLessThanOrEqual(width);
      // The chip stays on one line.
      const lines = await page.locator('#pl-status').evaluate((el) => Math.round(el.getBoundingClientRect().height / parseFloat(getComputedStyle(el).lineHeight)));
      expect(lines).toBe(1);
      // The time is whole; 恢复 is in "⋯".
      const time = page.locator('#pl-paused > span:not([aria-hidden]):visible');
      await expect(time).toHaveText(/^已暂停到 \d+\/\d+ 09:00$/);
      expect(await time.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
      await page.click('#pl-more > summary');
      await expect(page.locator('#pl-tools [data-action="planner-pause-resume"]')).toBeVisible();
      await page.screenshot({ path: test.info().outputPath('planner-paused-' + width + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

// A remote node's session that failed in a workspace is an item whose unit
// is "<node>/<key>": its card names the session and opens it on that node.
test('a remote session item opens the session on its node', async ({ browser }) => {
  const REMOTE_KEY = 'dashboard:direct:2026-01-01-120009-9:app';
  const data = defaultSessions();
  data.sessions.push({ ...data.sessions[2], key: REMOTE_KEY, node: 'mini', session_id: 'sess-remote', user_label: 'iso 远程修 CI', project: 'app' });
  data.nodes = { local: { display_name: 'Local', status: 'ok' }, mini: { display_name: 'Mini', status: 'ok' } };
  const box = { open: [
    { id: 'in_rf', kind: 'error', level: 'timely', unit: 'mini/' + REMOTE_KEY, ws: 'ws_app00001', title: 'iso 远程修 CI 失败', created: iso(now - 60000), state: 'open', source: 'failed:mini/' + REMOTE_KEY },
  ], closed: [], counts: { open: 1, badge: 1 } };
  for (const [name, viewport, extra] of [['desktop', { width: 1280, height: 800 }, {}], ['mobile', { width: 390, height: 844 }, { isMobile: true, hasTouch: true }]]) {
    const ctx = await browser.newContext({ viewport, ...extra });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, sessions: data, plannerEvents: plannerEvents(), inbox: box, tasks: { tasks: [] },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      if (name === 'mobile') await page.click('#pl-left-toggle');
      const card = page.locator('.pl-card[data-id="in_rf"]');
      // The title names the session: the meta only its workspace.
      await expect(card.locator('.pl-card-meta')).toHaveText(/^app\s*打开$/);
      const open = card.locator('.pl-link');
      await expect(open).toHaveAttribute('data-node', 'mini');
      await expect(open).toHaveAttribute('data-key', REMOTE_KEY);
      await page.screenshot({ path: test.info().outputPath('planner-remote-item-' + name + '.png') });
      await open.click();
      await page.waitForFunction((key) => window.nz.test.selectedKey === key, REMOTE_KEY);
      await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

test('journaled notes: the ones a restart dropped come back in time order, once', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const events = plannerEvents();
  // The history still holds this one (no restart since it was written); the
  // ring stamps every line it holds with a uuid.
  events.push({ type: 'system', summary: '已派出：iso 周报（task:abc）', time: now - 50000, uuid: 'pl-s1' });
  const mock = await startMockServer({
    ws: true, plannerEvents: events, inbox: inbox(),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerNotes: [
      // Older than the page, which is the whole history: it shows first.
      { time: iso(now - 70000), text: '驳回：iso 更早的一条' },
      { time: iso(now - 57000), text: '到点提醒（承诺，到期 09:01）：iso 交周报' },
      { time: iso(now - 50000 + 40), text: '已派出：iso 周报（task:abc）' },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const system = page.locator('#pl-stream .event.system');
    await expect(system).toHaveText([/驳回：iso 更早的一条/, /到点提醒.*iso 交周报/, /已派出：iso 周报/]);
    // The reminder sits between the replies it was written between.
    const order = await page.locator('#pl-stream .event').evaluateAll((els) => els.map((e) => e.textContent));
    const at = (re) => order.findIndex((t) => re.test(t));
    expect(at(/到点提醒/)).toBeGreaterThan(at(/早报/));
    expect(at(/到点提醒/)).toBeLessThan(at(/两个会话在跑/));
    await page.screenshot({ path: test.info().outputPath('planner-notes.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('planner.enabled off: the page explains it and the stop switch is off', async ({ page }) => {
  const mock = await startMockServer({ plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, planner: { enabled: false, stopped: false, key: PLANNER, alive: false } });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-note')).toContainText('Planner 没有开启');
    await expect(page.locator('#pl-status')).toHaveText('未开启');
    await expect(page.locator('#pl-stop')).toBeDisabled();
    await expect(page.locator('#pl-wait')).toContainText('没有等你的事');
    await expect(page.locator('#abnav-planner-count')).toBeHidden();
  } finally {
    mock.server.close();
  }
});

// The morning briefs of GET /api/planner/briefs are messages in the
// conversation, by the time they were made (D91): the day's open, an older
// one folded, a fold the user chose kept; the brief's push link (#planner&item=brief:<day>) opens,
// scrolls to and highlights it.
test('briefs sit in the conversation by time, and a push link scrolls to one', async ({ browser }) => {
  const day = (ms) => { const d = new Date(ms); return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); };
  const md = (n) => '## 早报 · iso\n\n完成 ' + n + '\n\n### 完成的（' + n + '）\n- iso 周报会话 · 11-04 18:00\n';
  const yesterday = now - 86400000;
  // A long conversation after today's brief, so the link has to scroll.
  const events = plannerEvents();
  for (let i = 0; i < 30; i++) events.push({ type: 'text', detail: 'iso 后来的消息 ' + i + '\n\n' + '一段话。'.repeat(20), time: now - 50000 + i * 100, uuid: 'pl-l' + i });
  const briefs = [
    { day: day(now - 57000), at: iso(now - 57000), sent: true, counts: { done: 2 }, markdown: md(2) },
    { day: day(yesterday), at: iso(yesterday), sent: true, counts: { done: 1 }, markdown: md(1) },
  ];
  for (const [name, viewport, extra] of [['desktop', { width: 1440, height: 900 }, {}], ['mobile', { width: 390, height: 844 }, { isMobile: true, hasTouch: true }]]) {
    const ctx = await browser.newContext({ viewport, ...extra });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, plannerEvents: events, inbox: inbox(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerBriefs: briefs,
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const all = page.locator('#pl-stream > .pl-brief');
      await expect(all).toHaveCount(2);
      // Yesterday's first, folded; today's between the replies it was made
      // between, open.
      const order = await page.locator('#pl-stream > .event, #pl-stream > .pl-brief').evaluateAll((els) => els.map((e) => e.classList.contains('pl-brief') ? 'brief:' + e.dataset.day : e.textContent.slice(0, 12)));
      expect(order[0]).toBe('brief:' + day(yesterday));
      const at = order.indexOf('brief:' + day(now - 57000));
      expect(order[at - 1]).toMatch(/早报：计划/);
      expect(order[at + 1]).toMatch(/两个会话在跑/);
      const old = all.nth(0);
      const today = all.nth(1);
      await expect(old).not.toHaveAttribute('open', '');
      await expect(today).toHaveAttribute('open', '');
      await expect(today.locator('summary')).toHaveText(/^早报 · \d{1,2}\/\d{1,2} \d\d:\d\d$/);
      await expect(today.locator('.md .md-h3')).toHaveText('完成的（2）');
      // Opened at the end of the conversation, today's brief is far above.
      await expect(today).not.toBeInViewport();
      await page.screenshot({ path: test.info().outputPath('planner-brief-end-' + name + '.png') });

      // Folded by the user, today's brief stays folded when the Planner page
      // is opened again and its stream repainted.
      await today.locator('summary').evaluate((el) => el.click());
      await expect(today).not.toHaveAttribute('open', '');
      await page.locator('#abnav-chat').click();
      await page.locator('#abnav-planner').click();
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      await expect(all).toHaveCount(2);
      await expect(today).not.toHaveAttribute('open', '');

      // The brief's push link: back on the chat view, then the link.
      await page.locator('#abnav-chat').click();
      await old.evaluate((el) => { el.open = false; });
      await page.evaluate((d) => { location.hash = 'planner&item=brief:' + d; }, day(yesterday));
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      await expect(old).toHaveAttribute('open', '');
      await expect(old.locator('summary')).toBeInViewport();
      await expect(old).toHaveClass(/inbox-lit/);
      await page.screenshot({ path: test.info().outputPath('planner-brief-link-' + name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

// Requests queued while the Planner was busy reach it as one coalesced
// message: yours join into one bubble, the gateway's receipts are not shown.
test('a coalesced turn: snapshots and receipts are not your bubble', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const head = '[以下是用户在你处理上一条消息期间追加发送的内容]\n';
  const events = [
    { type: 'user', time: now - 60000, uuid: 'pl-c1', detail:
      head + '\n[10:01] <naozhi-snapshot at="x">\n## 等你\n- iso 某条通知\n</naozhi-snapshot>\n\niso 第一条\n' +
      '\n[10:01] <naozhi-receipt>\niso 已派出 task:abc\n</naozhi-receipt>\n' +
      '\n[10:02] <naozhi-snapshot at="y">\n</naozhi-snapshot>\n\niso 第二条\n' },
  ];
  const mock = await startMockServer({
    ws: true, plannerEvents: events, inbox: inbox(),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const user = page.locator('#pl-stream .event.user');
    await expect(user).toHaveCount(1);
    await expect(user).toHaveText(/iso 第一条[\s\S]*iso 第二条/);
    await expect(page.locator('#pl-stream .event.system')).toHaveCount(0);
    await expect(page.locator('#pl-stream')).not.toContainText('task:abc');
    await expect(page.locator('#pl-stream')).not.toContainText('naozhi-snapshot');
    await expect(page.locator('#pl-stream')).not.toContainText('iso 某条通知');
    await expect(page.locator('#pl-stream')).not.toContainText('以下是用户');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// 通知 cards say each thing once (§15.7 Q10): a notice has neither a kind
// label nor "等了", the Planner's own item no source, a session's error whose
// title names it only its workspace; a long reason shows two lines, the
// whole of it in the tooltip. The "⋯" menu offers 允许浏览器通知 only while
// the browser has not asked yet (Q15).
test('等你 cards and the "⋯" menu say only what is needed', async ({ browser }) => {
  const why = 'iso 两个会议在同一时段，'.repeat(20);
  // A notice is the morning brief's (D5): only the question and the error list.
  const box = { open: [
    { id: 'in_n', kind: 'notice', level: 'timely', unit: PLANNER, title: 'iso 例行巡检', why: 'iso 一切正常', created: iso(now - 9 * 60000), state: 'open', source: 'planner:n0' },
    { id: 'in_q', kind: 'question', level: 'timely', unit: PLANNER, title: 'iso 日程冲突', why, link: 'https://example.com/x', created: iso(now - 7 * 60000), state: 'open', source: 'planner:q1' },
    { id: 'in_e', kind: 'error', level: 'timely', unit: SESSION, title: 'hello world 失败', created: iso(now - 3 * 60000), state: 'open', source: 'failed:' + SESSION },
  ], closed: [], counts: { open: 3, badge: 2 } };
  for (const [name, viewport, extra, permission] of [
    ['desktop', { width: 1440, height: 900 }, {}, 'default'],
    ['mobile', { width: 390, height: 844 }, { isMobile: true, hasTouch: true }, 'granted'],
  ]) {
    const ctx = await browser.newContext({ viewport, ...extra });
    await ctx.addInitScript((p) => { if (window.Notification) Object.defineProperty(Notification, 'permission', { get: () => p }); }, permission);
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, plannerEvents: plannerEvents(), inbox: box, tasks: { tasks: [] },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      if (name === 'mobile') await page.click('#pl-left-toggle');
      await expect(page.locator('#pl-wait .pl-card')).toHaveCount(2);
      await expect(page.locator('.pl-card[data-id="in_n"]')).toHaveCount(0);
      const ask = page.locator('.pl-card[data-id="in_q"]');
      await expect(ask.locator('.pl-kind')).toHaveText('提问');
      await expect(ask.locator('.pl-wait')).toHaveText(/^等了 [78] 分钟$/);
      await expect(ask.locator('.pl-card-meta')).toHaveText('打开原文');
      const reason = ask.locator('.pl-card-why');
      await expect(reason).toHaveAttribute('title', why);
      const lines = await reason.evaluate((el) => Math.round(el.clientHeight / parseFloat(getComputedStyle(el).lineHeight)));
      expect(lines).toBe(2);
      const err = page.locator('.pl-card[data-id="in_e"]');
      await expect(err.locator('.pl-kind')).toHaveText('错误');
      await expect(err.locator('.pl-wait')).toHaveText(/^[34] 分钟前$/);
      await expect(err.locator('.pl-card-why')).toHaveCount(0);
      await expect(err.locator('.pl-card-meta')).toHaveText(/^myproject\s*打开$/);
      // Nothing else lives under the column's lists.
      await expect(page.locator('#pl-left .pl-tool')).toHaveCount(0);
      await page.screenshot({ path: test.info().outputPath('planner-cards-' + name + '.png') });
      // The phone's unfolded left block takes the header's place.
      if (name === 'mobile') await page.click('#pl-left-toggle');
      await page.click('#pl-more > summary');
      await expect(page.locator('#pl-tools')).toBeVisible();
      if (permission === 'default') {
        await expect(page.locator('#pl-tools [data-action="planner-notify-perm"]')).toHaveText('允许浏览器通知');
      } else {
        await expect(page.locator('#pl-tools')).not.toContainText('浏览器通知');
      }
      await page.screenshot({ path: test.info().outputPath('planner-menu-' + name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
