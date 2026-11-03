// E2E: talking on the Planner page and handing work to it (planner_chat.js,
// planner_cards.js; planner RFC §10 D77, §10.1, §15.1 PR ⑬): the input box
// sends to the Planner, the pushed receipt and the reply render and replace
// the pending bubble; a handed-over session, a named session and the action
// blocks render as cards (the dispatch card answers the left column's
// decision); a session card opens in the middle column under the breadcrumb,
// shows its milestones and then its raw record, sends to that session and
// goes back; the chat view's 交给 Planner sends the session's reference with
// the typed text, and is hidden while planner.enabled is off.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const fence = (lang, body) => '```' + lang + '\n' + body + '\n```';

const DISPATCH = { type: 'dispatch', workspace: 'naozhi', title: 'iso 整理周报', model: 'sonnet', message: '把本周合并的 PR 整理成周报' };

function plannerEvents() {
  return [
    {
      type: 'user', time: now - 120000, uuid: 'pc-u1',
      detail: '<naozhi-snapshot at="2026-11-04 09:00 CST">\n会话（在跑 0，最近 24 小时结束 0）：\n</naozhi-snapshot>\n\n' +
        '<naozhi-session-ref>{"key":"' + SESSION + '","name":"iso-hello","title":"hello world"}</naozhi-session-ref>\n帮我盯着这个',
    },
    {
      type: 'text', time: now - 110000, uuid: 'pc-t1',
      detail: '`' + SESSION + '` 在等你回复。我建议派一个会话整理周报：\n\n' + fence('naozhi-action', JSON.stringify(DISPATCH)) +
        '\n\n' + fence('naozhi-action', '{"type":"dispatch","title":') + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'promise', text: 'iso 周五前提醒你看周报', due: '', close_when: '你看过周报' })),
    },
  ];
}

function inbox() {
  return {
    open: [{
      id: 'in_d', kind: 'decision', level: 'timely', unit: PLANNER, title: '派出：iso 整理周报', why: 'Planner 提议',
      created: iso(now - 100000), state: 'open', source: 'dispatch:d1',
      decision: { handler: 'dispatch', payload: { title: DISPATCH.title, message: DISPATCH.message, workspace: 'naozhi' }, actions: ['approve', 'reject'] },
    }],
    closed: [],
    counts: { open: 1, badge: 1 },
  };
}

const health = () => ({ at: iso(now), unowned: 0, units: [{ id: SESSION, kind: 'session', source: 'dashboard', coverage: 'covered', state: 'waiting', since: iso(now - 600000), last_active: iso(now - 300000) }] });

const sessionEvents = () => [
  { type: 'user', detail: '修一下登录页', time: now - 90000, uuid: 'se-u1' },
  { type: 'tool_use', tool: 'Edit', summary: 'Edit login.html', time: now - 85000, uuid: 'se-tu1' },
  { type: 'text', detail: '登录页改好了，等你确认要不要发版。', time: now - 80000, uuid: 'se-t1' },
];

test('desktop: send to the Planner, cards, and a session opened in the middle column', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const events = plannerEvents();
  const box = inbox();
  const mock = await startMockServer({
    ws: true, plannerEvents: events, inbox: box, systemHealth: health(), keyEvents: { [SESSION]: sessionEvents() },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-stream .event')).toHaveCount(2);

    // The handed-over session: a card in place of the reference block.
    const user = page.locator('#pl-stream .event.user');
    await expect(user.locator('.pl-handoff .pl-scard')).toHaveAttribute('data-key', SESSION);
    await expect(user).toContainText('帮我盯着这个');
    await expect(user).not.toContainText('naozhi-session-ref');

    // The named session's card carries its health; the dispatch card answers
    // the same decision as the left column; the broken block says so.
    const reply = page.locator('#pl-stream .event.text').first();
    const card = reply.locator('.pl-scards .pl-scard[data-key="' + SESSION + '"]');
    await expect(card.locator('.pl-scard-name')).toHaveText('hello world');
    await expect(card.locator('.hl-state')).toHaveText('待你回复');
    await expect(card.locator('.pl-scard-time')).toContainText('分钟前');
    const dispatch = reply.locator('.pl-act-dispatch');
    await expect(dispatch.locator('.pl-act-h')).toHaveText('派活提议 · iso 整理周报');
    await expect(dispatch).toContainText('把本周合并的 PR 整理成周报');
    await expect(dispatch.locator('.pl-act-state button')).toHaveText(['批准', '驳回']);
    await expect(reply.locator('.pl-act-bad .pl-act-h')).toHaveText('格式不对 · 网关不会执行');
    await expect(reply.locator('.pl-act-promise')).toContainText('iso 周五前提醒你看周报');
    await expect(reply.locator('code[data-lang="naozhi-action"]')).toHaveCount(0);
    await dispatch.locator('.pl-act-state button', { hasText: '批准' }).click();
    await expect(dispatch.locator('.pl-act-done')).toHaveText('已批准');
    expect(mock.inboxCalls).toContainEqual({ path: '/api/inbox/decide', method: 'POST', body: { id: 'in_d', action: 'approve' } });
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(0);

    // Send to the Planner: a pending bubble, then the receipt and the reply.
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    await expect(page.locator('#pl-target-to')).toHaveText('→ Planner');
    await page.fill('#pl-input', 'iso 现在都在干什么？');
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 现在都在干什么？' }]);
    await expect(page.locator('#pl-stream .pl-pending')).toHaveText(/iso 现在都在干什么？/);
    await expect(page.locator('#pl-input')).toHaveValue('');
    conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
    await expect(page.locator('#pl-stream .event.system')).toHaveText(/收到，正在看/);
    const t = Date.now();
    conn.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\niso 现在都在干什么？', time: t, uuid: 'pc-u2' } });
    conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail: '只有 `' + SESSION + '` 在等你。', time: t + 1, uuid: 'pc-t2' } });
    await expect(page.locator('#pl-stream .pl-pending')).toHaveCount(0);
    await expect(page.locator('#pl-stream .event.user').last()).toHaveText(/iso 现在都在干什么？/);
    await expect(page.locator('#pl-stream .event.text').last().locator('.pl-scard')).toHaveCount(1);
    await page.screenshot({ path: test.info().outputPath('planner-chat-desktop.png') });

    // Open the session card in the middle column.
    await card.click();
    await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-events')).toBeHidden();
    await expect(page.locator('.pl-crumb')).toHaveText(/Planner›hello world/);
    await expect(page.locator('#pl-sess-line .hl-state')).toHaveText('待你回复');
    const sess = page.locator('#pl-sess-stream');
    await expect(sess).toContainText('登录页改好了');
    await expect(sess.locator('.event.tool_use')).toHaveCount(0);
    await page.click('#pl-sess-raw');
    await expect(sess.locator('.event.tool_use')).toHaveCount(1);
    await expect(page.locator('#pl-sess-raw')).toHaveText('只看里程碑');
    await expect(page.locator('#pl-target-to')).toHaveText('→ hello world');
    await expect(page.locator('#pl-sess-handoff')).toBeVisible();
    await page.screenshot({ path: test.info().outputPath('planner-chat-session-desktop.png') });
    await page.fill('#pl-input', 'iso 先别发版');
    await page.click('#pl-send');
    await expect.poll(() => mock.sendCalls.map((b) => JSON.parse(b))).toContainEqual({ key: SESSION, text: 'iso 先别发版' });
    expect(mock.plannerSendCalls).toHaveLength(1);

    // Back to the conversation.
    await page.click('.pl-sess .pl-btn[data-action="planner-close-session"]');
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(page.locator('#pl-stream')).toContainText('只有');
    await expect(page.locator('#pl-target-to')).toHaveText('→ Planner');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('chat view: 交给 Planner sends the session with the typed text', async ({ page }) => {
  const mock = await startMockServer({
    ws: true, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    const btn = page.locator('#btn-handoff');
    await expect(btn).toBeVisible();
    await expect(btn).toBeEnabled();
    await page.click('#msg-input');
    await page.keyboard.type('iso 这个交给你');
    await btn.click();
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 这个交给你', ref: SESSION }]);
    await expect(page.locator('#msg-input')).toHaveText('');
    await expect(page.locator('#handoff-note')).toContainText('已交给 Planner');
    expect(mock.sendCalls).toHaveLength(0);
    await page.click('#handoff-note [data-action="planner-goto"]');
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
  } finally {
    mock.server.close();
  }
});

test('chat view: no 交给 Planner while planner.enabled is off', async ({ page }) => {
  const mock = await startMockServer({ ws: true, planner: { enabled: false, stopped: false, key: PLANNER, alive: false } });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('#msg-input')).toBeVisible();
    await expect(page.locator('#btn-handoff')).toBeHidden();
  } finally {
    mock.server.close();
  }
});

test('phone: input box, an opened session card and 交给 Planner fit', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  const mock = await startMockServer({
    ws: true, plannerEvents: plannerEvents(), inbox: inbox(), systemHealth: health(), keyEvents: { [SESSION]: sessionEvents() },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.locator('#abnav-planner').click();
    await expect(page.locator('#pl-input')).toBeInViewport();
    await expect(page.locator('#pl-stream .pl-act-dispatch')).toBeVisible();
    await page.screenshot({ path: test.info().outputPath('planner-chat-mobile.png') });
    await page.locator('#pl-stream .event.text .pl-scards .pl-scard').tap();
    await expect(page.locator('.pl-crumb b')).toHaveText('hello world');
    await expect(page.locator('#pl-sess-stream')).toContainText('登录页改好了');
    await expect(page.locator('#pl-input')).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-chat-session-mobile.png') });
    await page.locator('.pl-crumb .pl-link').tap();
    await expect(page.locator('#pl-events')).toBeVisible();

    await page.locator('#abnav-chat').click();
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('.sidebar')).not.toBeInViewport(); // slid away
    await expect(page.locator('#btn-handoff')).toBeInViewport();
    await expect(page.locator('#btn-send')).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-chat-handoff-mobile.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
