// E2E: the Planner page (planner_view.js, planner RFC §10 D77, §15.1 PR ⑥):
// the rail entry and its "等你" badge, the left column's 等你 cards (closing
// one) and 答应你的 empty state, the read-only planner:main stream (no
// snapshot block, no 追问, receipts over the subscription, no idle refetch
// on the inclusive `after` replay), the push pause and 停用 Planner
// controls, the workbench opening by itself on a new output and folding the
// left column to its strip; the phone layout's collapsible left block; the
// planner.enabled=false explanation.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
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
      { id: 'in_d', kind: 'decision', level: 'timely', title: '派出一个会话整理周报？', why: 'Planner 提议', created: iso(now - 5 * 60000), state: 'open', source: 'planner:d1', decision: { handler: 'dispatch', actions: ['approve', 'reject'] } },
    ],
    closed: [],
    counts: { open: 3, badge: 2 },
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

test('desktop: badge, 等你 cards, stream, controls, and the workbench folds the left column', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const outputs = [];
  const events = plannerEvents();
  const box = inbox();
  const mock = await startMockServer({
    ws: true, outputs, plannerEvents: events, inbox: box,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  await routePreview(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    // The badge is seeded from GET /api/inbox once the socket is up; the
    // existing rail stays as it was.
    await expect(page.locator('#abnav-planner-count')).toHaveText('2');
    for (const id of ['chat', 'assets', 'files', 'cron', 'system', 'settings']) {
      await expect(page.locator('#abnav-' + id)).toBeVisible();
    }
    await expect(page.locator('#abnav-chat')).toHaveClass(/active/);

    await page.click('#abnav-planner');
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(page.locator('#abnav-planner')).toHaveAttribute('aria-pressed', 'true');
    const cards = page.locator('#pl-wait .pl-card');
    await expect(cards).toHaveCount(3);
    // The column counts what the badge counts (decisions and questions).
    await expect(page.locator('#pl-wait-count')).toHaveText('2');
    // Decisions first, then questions, then errors.
    await expect(cards.nth(0).locator('.pl-kind')).toHaveText('决定');
    await expect(cards.nth(0).locator('.pl-card-acts button')).toHaveText(['批准', '驳回']);
    const q = page.locator('.pl-card[data-id="in_q"]');
    await expect(q.locator('.pl-kind')).toHaveText('提问');
    await expect(q.locator('.pl-wait')).toHaveText('等了 12 分钟');
    await expect(q.locator('.pl-link')).toHaveText('打开');
    await expect(page.locator('#pl-promise')).toContainText('还没有答应你的事');
    await expect(page.locator('#pl-status')).toHaveText('在线');
    await expect(page.locator('#pl-brief')).toContainText('早报');
    await expect(page.locator('#pl-stream .event')).toHaveCount(3);
    await expect(page.locator('#pl-stream')).toContainText('两个会话在跑');
    // The user bubble shows what was typed, not the gateway's snapshot; a
    // long reply has 复制 but no 追问 (that asks about the selected session).
    await expect(page.locator('#pl-stream .event.user')).toHaveText(/现在都在干什么？/);
    await expect(page.locator('#pl-stream')).not.toContainText('naozhi-snapshot');
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

    // Pause pushes for an hour, then the stop switch.
    await page.click('[data-action="planner-pause"][data-for="1h"]');
    await expect(page.locator('#pl-tools')).toContainText('已暂停到');
    expect(mock.inboxCalls[1]).toMatchObject({ path: '/api/inbox/pause', method: 'PUT', body: { for: '1h' } });
    await page.click('[data-action="planner-pause-resume"]');
    await expect(page.locator('[data-action="planner-pause"][data-for="today"]')).toBeVisible();

    // A new output of planner:main opens the workbench; the left column folds.
    await expect.poll(() => mock.outputsCalls.filter((k) => k === PLANNER).length).toBeGreaterThan(0);
    await expect(page.locator('.pl-left')).toHaveCSS('width', '280px');
    outputs.unshift(output('handoff/2026-11-04.md'));
    events.push({ type: 'result', summary: 'done', time: Date.now(), uuid: 'pl-r1' });
    await expect(page.locator('#fv-drawer')).toHaveClass(/fv-open/, { timeout: 10000 });
    await expect(page.locator('#fv-title')).toHaveText('handoff/2026-11-04.md');
    await expect(page.locator('#planner-main')).toHaveClass(/pl-wb-open/);
    await expect(page.locator('.pl-left')).toHaveCSS('width', '48px');
    await expect(page.locator('#pl-strip-wait')).toHaveText('1');
    await page.screenshot({ path: test.info().outputPath('planner-desktop-workbench.png') });
    await page.click('#fv-btn-close');
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-wb-open/);
    await expect(page.locator('.pl-left')).toHaveCSS('width', '280px');

    // Idle: every poll replays the trailing result (inclusive `after`), which
    // must not refetch the outputs again.
    const idle = mock.outputsCalls.length;
    await page.waitForTimeout(7000);
    expect(mock.outputsCalls.length).toBe(idle);

    // The subscription: subscribed after the last event, a pushed receipt
    // (never logged) shows up, and polling stops once it is live.
    const sub = conn.messages.find((m) => m.type === 'subscribe' && m.key === PLANNER);
    expect(sub).toMatchObject({ after: now - 55000 });
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
    await expect(page.locator('#pl-stream .event.system')).toHaveText(/收到，正在看/);

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
  const mock = await startMockServer({
    ws: true, plannerEvents: plannerEvents(), inbox: inbox(),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('#abnav-planner-count')).toHaveText('2');
    await page.click('#abnav-planner');
    const toggle = page.locator('#pl-left-toggle');
    await expect(toggle).toBeVisible();
    await expect(toggle).toContainText('等你 2');
    await expect(page.locator('#pl-wait')).toBeHidden();
    await expect(page.locator('#pl-stream')).toContainText('两个会话在跑');
    await page.screenshot({ path: test.info().outputPath('planner-mobile.png') });
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(3);
    await expect(page.locator('#pl-wait .pl-card').first()).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-mobile-open.png') });
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
