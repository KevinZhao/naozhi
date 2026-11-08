// E2E: talking on the Planner page and handing work to it (planner_chat.js,
// planner_cards.js; planner RFC §10 D77, §10.1, §15.1 PR ⑬): the input box
// sends to the Planner, the gateway's "收到，正在看" shows by the box until
// the reply to that message comes (D91), not another turn's; a handed-over session, a named
// session and the action blocks render as cards (a dispatch runs at once,
// D88: its card has no answer to give; a task with no name yet shows no
// key); a session card opens in the middle column
// under the breadcrumb (§15.7 Q7: no 返回 Planner, its status line unnamed,
// no 交给 Planner), shows its milestones and then its raw record, answers
// its questions and sends to that session, and goes back; an unlisted
// session's card is read-only; the chat view's 交给 Planner sends the session's reference with
// the typed text, and is hidden while planner.enabled is off.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const fence = (lang, body) => '```' + lang + '\n' + body + '\n```';

const DISPATCH = { type: 'dispatch', workspace: 'naozhi', title: 'iso 整理周报', model: 'sonnet', message: '把本周合并的 PR 整理成周报' };

const TASK = 'task:iso123';
const REF = '<naozhi-session-ref>{"key":"' + SESSION + '","name":"iso-hello","title":"hello world"}</naozhi-session-ref>\n';
// BIG_SNAPSHOT is a snapshot near its 2000-rune budget: the logged bubble
// (EventDetailMaxRunes) is cut inside it.
const BIG_SNAPSHOT = '<naozhi-snapshot at="2026-11-04 09:30 CST">\n' + '- iso 会话 · 空闲 · naozhi\n'.repeat(90) + '</naozhi-snapshot>\n\n';

function plannerEvents() {
  return [
    {
      type: 'user', time: now - 120000, uuid: 'pc-u1',
      detail: REF + '<naozhi-snapshot at="2026-11-04 09:00 CST">\n会话（在跑 0，最近 24 小时结束 0）：\n</naozhi-snapshot>\n\n帮我盯着这个',
    },
    {
      type: 'text', time: now - 110000, uuid: 'pc-t1',
      detail: 'hello world（`' + SESSION + '`）在等你回复，iso 周报 `' + TASK + '` 还没开始，hello world（名字 hw，`' + SESSION + '`）先等等，两个都在 myproject（`' + SESSION + '` 和 `' + TASK + '`）。我派一个会话整理周报：\n\n' + fence('naozhi-action', JSON.stringify(DISPATCH)) +
        '\n\n' + fence('naozhi-action', '{"type":"dispatch","title":') + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'promise', text: 'iso 周五前提醒你看周报', due: '', close_when: '你看过周报' })) + '\n\n' +
        fence('naozhi-action', JSON.stringify(DISPATCH)) + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'status', task: TASK, state: 'done', why: 'iso 它汇报周报写好了' })),
    },
    {
      type: 'user', time: now - 105000, uuid: 'pc-u3',
      detail: (REF + BIG_SNAPSHOT + '再看看这个').slice(0, 2000),
    },
  ];
}

// inbox is empty: a dispatch opens no card in 通知.
function inbox() {
  return { open: [], closed: [], counts: { open: 0, badge: 0 } };
}

const health = () => ({ at: iso(now), unowned: 0, units: [{ id: SESSION, kind: 'session', source: 'dashboard', coverage: 'covered', state: 'waiting', since: iso(now - 600000), last_active: iso(now - 300000) }] });

const ask = (tuid, q, time) => ({
  type: 'ask_question', time, uuid: 'se-' + tuid, summary: q,
  ask_question: { tool_use_id: tuid, items: [{ header: '发版', question: q, options: [{ label: '发' }, { label: '先别发' }] }] },
});

const sessionEvents = () => [
  ask('aq-old', '先跑测试吗？', now - 95000),
  { type: 'user', detail: '修一下登录页', time: now - 90000, uuid: 'se-u1' },
  { type: 'tool_use', tool: 'Edit', summary: 'Edit login.html', time: now - 85000, uuid: 'se-tu1' },
  { type: 'text', detail: '登录页改好了，等你确认要不要发版。', time: now - 80000, uuid: 'se-t1' },
  ask('aq-new', '要不要发版？', now - 79000),
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
    await expect(page.locator('#pl-stream .event')).toHaveCount(3);

    // The handed-over session: a card in place of the reference block, also
    // when the logged bubble was cut inside the snapshot after it.
    const user = page.locator('#pl-stream .event.user').first();
    await expect(user.locator('.pl-handoff .pl-scard')).toHaveAttribute('data-key', SESSION);
    await expect(user).toContainText('帮我盯着这个');
    await expect(user).not.toContainText('naozhi-session-ref');
    const cutUser = page.locator('#pl-stream .event.user').nth(1);
    await expect(cutUser.locator('.pl-handoff .pl-scard')).toHaveAttribute('data-key', SESSION);
    await expect(cutUser).toContainText('消息过长');
    await expect(cutUser).not.toContainText('naozhi-snapshot');

    // The named session's card carries its health; the dispatch card says
    // what was dispatched, with nothing to approve.
    const reply = page.locator('#pl-stream .event.text').first();
    const card = reply.locator('.pl-scards .pl-scard[data-key="' + SESSION + '"]');
    await expect(card.locator('.pl-scard-name')).toHaveText('hello world');
    await expect(card.locator('.hl-state')).toHaveText('待你回复');
    await expect(card.locator('.pl-scard-time')).toContainText('分钟前');
    // The keys after the names fold into the cards (§6.4: names, not keys).
    await expect(reply.locator('.pl-scards .pl-scard')).toHaveCount(2);
    await expect(reply).toContainText('hello world在等你回复，iso 周报 还没开始，hello world（名字 hw）先等等，两个都在 myproject。');
    await expect(reply).not.toContainText(SESSION);
    const dispatch = reply.locator('.pl-act-dispatch').first();
    await expect(dispatch.locator('.pl-act-h')).toHaveText('派出会话 · iso 整理周报');
    await expect(dispatch).toContainText('把本周合并的 PR 整理成周报');
    await expect(dispatch).not.toContainText('可在"任务"里停止');
    await expect(reply.locator('.pl-act-dispatch button')).toHaveCount(0);
    await expect(reply.locator('.pl-act-bad .pl-act-h')).toHaveText('看不懂的动作块');
    await expect(reply.locator('.pl-act-promise')).toContainText('iso 周五前提醒你看周报');
    // The Planner's judgment of a task (§15.4 status).
    await expect(reply.locator('.pl-act-status .pl-act-h')).toHaveText('Planner 判断 · 已完成');
    await expect(reply.locator('.pl-act-status')).toContainText('iso 它汇报周报写好了');
    // Its task is neither listed nor in 任务 yet: no 任务 row, never the key;
    // its session card names its kind (D92).
    await expect(reply.locator('.pl-act-status dt')).toHaveText(['依据']);
    await expect(reply.locator('.pl-scard[data-key="' + TASK + '"] .pl-scard-name')).toHaveText('派出的任务');
    await expect(reply.locator('code[data-lang="naozhi-action"]')).toHaveCount(0);
    await expect(page.locator('#pl-cards')).toBeHidden();

    // Send to the Planner: a pending bubble, then the receipt and the reply.
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    await expect(page.locator('#pl-target-to')).toHaveText('');
    await page.fill('#pl-input', 'iso 现在都在干什么？');
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 现在都在干什么？' }]);
    await expect(page.locator('#pl-stream .pl-pending')).toHaveText(/iso 现在都在干什么？/);
    await expect(page.locator('#pl-input')).toHaveValue('');
    await expect(page.locator('#pl-target-note')).toHaveText('收到，正在看');
    conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
    await expect(page.locator('#pl-stream .event.system')).toHaveCount(0);
    // A turn already running answers something else: its reply leaves the
    // ack, and so does the page's first page read again on coming back.
    conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail: 'iso 上一件事的进展', time: Date.now(), uuid: 'pc-t-other' } });
    await expect(page.locator('#pl-stream .event.text').last()).toHaveText(/iso 上一件事的进展/);
    await expect(page.locator('#pl-target-note')).toHaveText('收到，正在看');
    await page.click('#abnav-chat');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-stream .event')).toHaveCount(3);
    await expect(page.locator('#pl-target-note')).toHaveText('收到，正在看');
    const t = Date.now();
    conn.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\niso 现在都在干什么？', time: t, uuid: 'pc-u2' } });
    conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail: '`' + SESSION + '` 在等你。', time: t + 1, uuid: 'pc-t2' } });
    await expect(page.locator('#pl-stream .pl-pending')).toHaveCount(0);
    await expect(page.locator('#pl-stream .event.user').last()).toHaveText(/iso 现在都在干什么？/);
    await expect(page.locator('#pl-target-note')).toHaveText('');
    await expect(page.locator('#pl-stream .event.text').last().locator('.pl-scard')).toHaveCount(1);
    // A key with nothing before it reads as the session's name.
    await expect(page.locator('#pl-stream .event.text').last()).toContainText('hello world 在等你。');
    await page.screenshot({ path: test.info().outputPath('planner-chat-desktop.png') });

    // Open the session card in the middle column.
    await card.click();
    await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-events')).toBeHidden();
    await expect(page.locator('.pl-crumb')).toHaveText(/Planner›hello world/);
    await expect(page.locator('#pl-sess-line .hl-state')).toHaveText('待你回复');
    await expect(page.locator('#pl-sess-line .pl-scard-name')).toHaveCount(0);
    await expect(page.locator('#pl-sess .pl-btn', { hasText: '返回 Planner' })).toHaveCount(0);
    // Not a dispatched session: no 停止.
    await expect(page.locator('#pl-sess-stop')).toBeHidden();
    const sess = page.locator('#pl-sess-stream');
    await expect(sess).toContainText('登录页改好了');
    await expect(sess.locator('.event.tool_use')).toHaveCount(0);
    await page.click('#pl-sess-raw');
    await expect(sess.locator('.event.tool_use')).toHaveCount(1);
    await expect(page.locator('#pl-sess-raw')).toHaveText('只看里程碑');
    await expect(page.locator('#pl-target-to')).toHaveText('→ hello world');
    await expect(page.locator('#pl-composer')).not.toContainText('交给 Planner');
    await page.screenshot({ path: test.info().outputPath('planner-chat-session-desktop.png') });

    // Its AskUserQuestion cards: the one a user event followed is locked and
    // folded to its answer, the open one answers this session, not the chat
    // view's selection.
    const oldAsk = sess.locator('.event.ask_question[data-tool-use-id="aq-old"]');
    await expect(oldAsk.locator('.ask-status')).toHaveText('已回答：修一下登录页');
    await expect(oldAsk.locator('.ask-opt').first()).toBeDisabled();
    await expect(oldAsk.locator('.ask-opt').first()).toBeHidden();
    const newAsk = sess.locator('.event.ask_question[data-tool-use-id="aq-new"]');
    await newAsk.locator('.ask-opt', { hasText: '先别发' }).click();
    await newAsk.locator('.ask-submit').click();
    await expect.poll(() => mock.sendCalls.map((b) => JSON.parse(b))).toContainEqual({ key: SESSION, text: '发版: 先别发.' });

    await page.fill('#pl-input', 'iso 先别发版');
    await page.click('#pl-send');
    await expect.poll(() => mock.sendCalls.map((b) => JSON.parse(b))).toContainEqual({ key: SESSION, text: 'iso 先别发版' });
    expect(mock.plannerSendCalls).toHaveLength(1);

    // Back to the conversation.
    await page.click('.pl-crumb [data-action="planner-close-session"]');
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(page.locator('#pl-stream')).toContainText('hello world 在等你。');
    await expect(page.locator('#pl-target-to')).toHaveText('');

    // A card naming a session that is not listed opens read-only: a send
    // would create it as an ordinary session.
    await reply.locator('.pl-scard[data-key="' + TASK + '"]').click();
    await expect(page.locator('#pl-input')).toBeDisabled();
    await expect(page.locator('#pl-send')).toBeDisabled();
    await expect(page.locator('#pl-target-note')).toHaveText('不在会话列表里，不能发消息');
    await page.click('.pl-crumb [data-action="planner-close-session"]');
    await expect(page.locator('#pl-input')).toBeEnabled();
    expect(mock.sendCalls.map((b) => JSON.parse(b).key)).not.toContain(TASK);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// A list of keys, or one after a colon, stands for the names: folding them
// away left "派出了 、、。" (§15.12 ①). A key after its name, and a bracket
// holding only keys, still go.
const FOLD_ROWS = [
  ['派出了 A、B、C。', '派出了 iso 周报、iso 评审、iso 部署。'],
  ['派出了 A，B；C。', '派出了 iso 周报，iso 评审；iso 部署。'],
  ['A, B 都在跑。', 'iso 周报, iso 评审 都在跑。'],
  ['派出了 A 和 B。', '派出了 iso 周报 和 iso 评审。'],
  ['名单：A。', '名单：iso 周报。'],
  ['iso 周报：A 已完成。', 'iso 周报： 已完成。'],
  ['iso 周报 A 先等等。', 'iso 周报 先等等。'],
  ['iso 周报 和 iso 评审（A、B）都在跑。', 'iso 周报 和 iso 评审都在跑。'],
  ['三个都在 naozhi（A，B，C）。', '三个都在 naozhi。'],
];

test('keys in a reply fold to names or away, never to bare separators', async ({ page }) => {
  const tasks = [['task:isoa1', 'iso 周报'], ['task:isob2', 'iso 评审'], ['task:isoc3', 'iso 部署']];
  const s = defaultSessions();
  for (const [key, label] of tasks) {
    s.sessions.push({ key, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: label,
      workspace: '/home/user/workspace/naozhi', last_active: now - 20000, node: 'local' });
  }
  const tick = (key) => '`' + key + '`';
  const reply = (line) => line.replace('A', tick(tasks[0][0])).replace('B', tick(tasks[1][0])).replace('C', tick(tasks[2][0]));
  const mock = await startMockServer({
    ws: true, sessions: s, inbox: inbox(), systemHealth: health(),
    plannerEvents: FOLD_ROWS.map(([line], i) => ({ type: 'text', time: now - 60000 + i, uuid: 'pc-fold' + i, detail: reply(line) })),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const replies = page.locator('#pl-stream .event.text');
    await expect(replies).toHaveCount(FOLD_ROWS.length);
    // Each reply's own text: its cards and copy button read the names too.
    const texts = () => replies.evaluateAll((els) => els.map((el) => {
      const c = el.querySelector('.event-content').cloneNode(true);
      c.querySelectorAll('.pl-scards, button').forEach((n) => n.remove());
      return c.textContent.trim();
    }));
    await expect.poll(texts).toEqual(FOLD_ROWS.map(([, want]) => want));
    await expect(page.locator('#pl-stream')).not.toContainText('task:');
  } finally {
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
    await expect(page.locator('#msg-input')).toHaveAttribute('data-placeholder', '发消息…');
    // Nothing typed: nothing to hand over (it once sent an empty card).
    await expect(btn).toBeDisabled();
    await page.click('#msg-input');
    await page.keyboard.type('   ');
    await expect(btn).toBeDisabled();
    await page.keyboard.type('iso 这个交给你');
    await expect(btn).toBeEnabled();
    await btn.click();
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 这个交给你', ref: SESSION }]);
    await expect(page.locator('#msg-input')).toHaveText('');
    // Cleared by the hand-off, not by typing: disabled again.
    await expect(btn).toBeDisabled();
    await expect(page.locator('#handoff-note')).toContainText('已交给 Planner');
    expect(mock.sendCalls).toHaveLength(0);
    await page.click('#handoff-note [data-action="planner-goto"]');
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    // An empty conversation says how to start one.
    await expect(page.locator('#pl-stream > .pl-empty')).toContainText('直接说要做的事，或在会话里点"交给 Planner"');
  } finally {
    mock.server.close();
  }
});

// A session card's name gives way before its state: a long name ends in an
// ellipsis and the state stays on the card.
for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
  test(`${name}: a long session name leaves the card its state`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport });
    const page = await ctx.newPage();
    const sessions = defaultSessions();
    sessions.sessions[0].last_prompt = '把登录页的样式改成和设置页一致，顺便检查一下手机上的输入框和按钮会不会被挤出屏幕';
    const mock = await startMockServer({
      ws: true, sessions, inbox: inbox(), systemHealth: health(),
      plannerEvents: [{ type: 'text', detail: '它在等你：`' + SESSION + '`', time: now - 60000, uuid: 'pc-long' }],
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.locator('#abnav-planner').click();
      const card = page.locator('#pl-stream .pl-scard[data-key="' + SESSION + '"]');
      await expect(card.locator('.hl-state')).toHaveText('待你回复');
      const line = await card.locator('.pl-scard-line').boundingBox();
      const state = await card.locator('.hl-state').boundingBox();
      expect(state.x + state.width).toBeLessThanOrEqual(line.x + line.width + 0.5);
      await page.screenshot({ path: test.info().outputPath(`planner-long-name-${name}.png`) });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

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
    await expect(page.locator('#pl-stream .pl-act-dispatch').first()).toBeVisible();
    await page.screenshot({ path: test.info().outputPath('planner-chat-mobile.png') });
    await page.locator('#pl-stream .event.text .pl-scards .pl-scard[data-key="' + SESSION + '"]').tap();
    await expect(page.locator('.pl-crumb b')).toHaveText('hello world');
    await expect(page.locator('#pl-sess-stream')).toContainText('登录页改好了');
    await expect(page.locator('#pl-input')).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-chat-session-mobile.png') });
    await page.locator('.pl-crumb .pl-link').tap();
    await expect(page.locator('#pl-events')).toBeVisible();

    await page.locator('#abnav-chat').click();
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('.sidebar')).not.toBeInViewport(); // slid away
    // Empty, the row keeps its four controls and the placeholder one line;
    // typing brings 交给 Planner in. The header leaves the backend name out.
    await expect(page.locator('#btn-handoff')).toBeHidden();
    await expect(page.locator('#header-cli')).toBeHidden();
    const input = page.locator('#msg-input');
    expect((await input.boundingBox()).height).toBeLessThan(60);
    await expect(page.locator('#btn-send')).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-chat-empty-mobile.png') });
    await input.click();
    await page.keyboard.type('iso 交给你');
    await expect(page.locator('#btn-handoff')).toBeInViewport();
    await expect(page.locator('#btn-send')).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-chat-handoff-mobile.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// A permission the Planner's own tool was refused is nothing for you to do:
// the Planner conversation leaves it out (Q12); a session's view keeps its own.
for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
  test(`${name}: the Planner's own permission denials are not in its conversation`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport });
    const page = await ctx.newPage();
    const denied = { type: 'permission_denied', tool: 'Read', detail: 'Permission to use Read has been denied because Claude Code is running in don\'t ask mode. '.repeat(8), time: now - 60000, uuid: 'pc-d1' };
    const answer = { type: 'text', detail: 'iso 看过了', time: now - 50000, uuid: 'pc-t1' };
    const mock = await startMockServer({
      ws: true, plannerEvents: [denied, answer], tasks: { tasks: [] }, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, systemHealth: health(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      await expect(page.locator('#pl-stream .event.text')).toContainText('iso 看过了');
      await expect(page.locator('#pl-stream .event.permission_denied')).toHaveCount(0);
      await expect(page.locator('#pl-stream')).not.toContainText('权限拒绝');
      await page.screenshot({ path: test.info().outputPath(`planner-denied-${name}.png`) });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

// A conversation of several turns, as the Planner page shows it on a desktop
// and a phone: a plain answer, a reply with dispatch / notify / promise / ask
// blocks, the dispatch's receipt (not shown) and the reply after it, a turn
// the scout started, a long reply with a table, code and session cards, then
// a turn streamed in over the socket. The layout holds: nothing [hidden]
// shows, nothing runs past the column, no block shows as its raw text, and
// the stream follows the bottom while you are there and leaves you where you
// scrolled to otherwise.
const MT_TASK = 'task:mt0001';
const MT_DISPATCH = { type: 'dispatch', workspace: 'naozhi', title: 'iso 整理周报', model: 'sonnet', message: '把本周合并的 PR 整理成周报，按模块分组' };
const MT_SCOUT = '<naozhi-scout findings="2" run="20261106T100000-ab12">\n巡检发现了需要看一眼的事。\n' +
  '{"source":"slack","kind":"reply_needed","id":"D1/1.1","title":"iso 问进度"}\n</naozhi-scout>\n\n' +
  '<naozhi-snapshot at="2026-11-06 10:00 CST">\n等你（0）\n</naozhi-snapshot>';
const snap = (s) => '<naozhi-snapshot at="2026-11-06 09:00 CST">\n会话（在跑 1）：\n</naozhi-snapshot>\n\n' + s;
const LONG_REPLY = '这周的情况整理如下：\n\n' +
  '## 进度\n\n' +
  '| 模块 | 负责会话 | 状态 | 备注 |\n|---|---|---|---|\n' +
  '| 登录页 | hello world | 待你回复 | 等你确认要不要发版 |\n' +
  '| 周报 | iso 整理周报 | 进行中 | 预计今天下午写完，写完后我会把链接发给你 |\n' +
  '| 托管 | — | 已完成 | 连续三次没有进展会停 |\n\n' +
  '登录页的改动在这里：\n\n' +
  '```js\nfunction login(user, pass) {\n  if (!user || !pass) throw new Error("missing credentials: both the user name and the password are required here");\n  return api.post("/login", { user, pass });\n}\n```\n\n' +
  '接下来：\n\n1. 你先回 hello world（`' + 'dashboard:direct:2026-01-01-120000-1:myproject' + '`）的发版问题；\n2. 周报写完我再提醒你；\n   - 写到一半卡住也会告诉你\n3. 其余的我盯着。\n\n' +
  '> 备注：巡检每两小时一次。\n\n还有 iso 整理周报（`' + MT_TASK + '`）在跑。';

function multiTurnEvents() {
  const t0 = now - 600000;
  return [
    // ① a plain answer
    { type: 'user', detail: snap('iso 早上好，今天有什么要我处理的？'), time: t0, uuid: 'mt-u1' },
    { type: 'text', detail: '早上好。今天有两件事要你看：\n\n- 登录页的发版问题\n- 周报的范围\n\n别的我在盯着。', time: t0 + 5000, uuid: 'mt-t1' },
    // ② the action blocks
    { type: 'user', detail: snap('iso 周报派出去做，周五前提醒我看；顺便问问我要不要发版'), time: t0 + 60000, uuid: 'mt-u2' },
    {
      type: 'text', time: t0 + 70000, uuid: 'mt-t2',
      detail: '好，我这样安排：\n\n' + fence('naozhi-action', JSON.stringify(MT_DISPATCH)) + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'promise', text: 'iso 周五前提醒你看周报', due: '2026-11-07T10:00:00+08:00', close_when: '你看过周报' })) + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'notify', title: 'iso 登录页等你确认发版', why: '它停在问题上半小时了', level: 'push' })) + '\n\n' +
        fence('naozhi-action', JSON.stringify({ type: 'ask', question: 'iso 登录页要发版吗？', options: ['发', '先别发'] })),
    },
    // ③ the dispatch's receipt (written for the Planner, not shown) and the reply after it
    { type: 'user', detail: '<naozhi-receipt>\n已派出：iso 整理周报（' + MT_TASK + '），工作区 naozhi。\n</naozhi-receipt>', time: t0 + 80000, uuid: 'mt-r1' },
    { type: 'text', detail: '派出了 iso 整理周报（`' + MT_TASK + '`），做完我告诉你。', time: t0 + 85000, uuid: 'mt-t3' },
    // ④ a turn the scout started
    { type: 'user', detail: MT_SCOUT, summary: MT_SCOUT.slice(0, 120), time: t0 + 200000, uuid: 'mt-u4' },
    { type: 'text', detail: 'Slack 上有人在问进度，我先记下了，等你空了再回。', time: t0 + 205000, uuid: 'mt-t4' },
    // ⑤ a long reply with a table, code and session cards
    { type: 'user', detail: snap('iso 整体进度怎么样？'), time: t0 + 300000, uuid: 'mt-u5' },
    { type: 'text', detail: LONG_REPLY, time: t0 + 310000, uuid: 'mt-t5' },
  ];
}

function multiTurnSessions() {
  const s = defaultSessions();
  s.sessions.push({
    key: MT_TASK, state: 'running', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task',
    user_label: 'iso 整理周报', workspace: '/home/user/workspace/naozhi', last_active: now - 20000, last_prompt: '把本周合并的 PR 整理成周报', node: 'local',
  });
  return s;
}

// layoutProblems lists what is wrong with the Planner page's layout: an
// element marked hidden that still shows, a bubble or block running past the
// stream, the stream scrolling sideways.
async function layoutProblems(page) {
  return page.evaluate(() => {
    const out = [];
    const main = document.getElementById('planner-main');
    for (const el of main.querySelectorAll('[hidden]')) {
      if (getComputedStyle(el).display !== 'none') out.push('hidden but shown: ' + (el.id || el.className));
    }
    const scroller = document.getElementById('pl-events');
    if (scroller.scrollWidth > scroller.clientWidth + 1) out.push('stream scrolls sideways: ' + scroller.scrollWidth + ' > ' + scroller.clientWidth);
    const right = scroller.getBoundingClientRect().right;
    for (const el of scroller.querySelectorAll('.event, .pl-act, .pl-scard, .md-table-wrap, table, .md-code-wrap')) {
      // A wide table or code block scrolls inside its wrap.
      if (el.parentElement.closest('.md-table-wrap, .md-code-wrap')) continue;
      const r = el.getBoundingClientRect();
      if (r.width && r.right > right + 1) out.push('past the column: ' + el.className + ' ' + Math.round(r.right) + ' > ' + Math.round(right));
    }
    return out;
  });
}

const atBottom = (page) => page.evaluate(() => {
  const s = document.getElementById('pl-events');
  return s.scrollTop + s.clientHeight >= s.scrollHeight - 2;
});

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true }]) {
  test(`${vp.name}: a conversation of several turns reads cleanly`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    const mock = await startMockServer({
      ws: true, sessions: multiTurnSessions(), plannerEvents: multiTurnEvents(), inbox: inbox(), systemHealth: health(),
      tasks: { tasks: [{ id: MT_TASK, kind: 'session', title: 'iso 整理周报', state: 'doing', at: iso(now - 20000), created: iso(now - 500000) }] },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.locator('#abnav-planner').click();
      const stream = page.locator('#pl-stream');
      // Your three messages are bubbles; the receipt and the scout's data are not.
      await expect(stream.locator('.event.user')).toHaveCount(3);
      await expect(stream.locator('.event.text')).toHaveCount(5);
      await expect(stream).toContainText('来自巡检：2 条新发现');
      for (const raw of ['naozhi-action', 'naozhi-receipt', 'naozhi-scout', 'naozhi-snapshot', '"type":', '```', MT_TASK, SESSION]) {
        await expect(stream).not.toContainText(raw);
      }
      await expect(stream.locator('code[data-lang="naozhi-action"]')).toHaveCount(0);
      // ② every block is its card.
      const acts = stream.locator('.event.text').nth(1);
      await expect(acts.locator('.pl-act')).toHaveCount(4);
      await expect(acts.locator('.pl-act-dispatch .pl-act-h')).toHaveText('派出会话 · iso 整理周报');
      await expect(acts.locator('.pl-act-promise')).toContainText('iso 周五前提醒你看周报');
      await expect(acts.locator('.pl-act-notify')).toContainText('iso 登录页等你确认发版');
      await expect(acts.locator('.pl-act-ask .pl-act-opt')).toHaveText(['发', '先别发']);
      // ③ the dispatched session's card, under the reply that names it.
      await expect(stream.locator('.event.text').nth(2).locator('.pl-scard[data-key="' + MT_TASK + '"] .pl-scard-name')).toHaveText('iso 整理周报');
      // ⑤ the long reply: table, code, lists and the cards of both sessions.
      const long = stream.locator('.event.text').nth(4);
      await expect(long.locator('table')).toHaveCount(1);
      await expect(long.locator('pre code')).toHaveCount(1);
      await expect(long.locator('ol > li')).toHaveCount(3);
      await expect(long.locator('.pl-scard')).toHaveCount(2);
      await expect.poll(() => atBottom(page)).toBe(true);
      expect(await layoutProblems(page)).toEqual([]);
      await expect(page.locator('#pl-input')).toBeInViewport();
      await page.screenshot({ path: test.info().outputPath(`planner-turns-${vp.name}.png`) });
      // The whole conversation, top to bottom, for review.
      const scrollShots = await page.evaluate(() => { const s = document.getElementById('pl-events'); return Math.ceil(s.scrollHeight / s.clientHeight); });
      for (let i = 0; i < Math.min(scrollShots, 6); i++) {
        await page.evaluate((n) => { const s = document.getElementById('pl-events'); s.scrollTop = n * s.clientHeight; }, i);
        await page.screenshot({ path: test.info().outputPath(`planner-turns-${vp.name}-p${i + 1}.png`) });
      }

      // ⑥ a turn streamed in: your message, the Planner's tool calls (not
      // shown) and its text, one event at a time, followed at the bottom.
      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
      conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
      await page.evaluate(() => { const s = document.getElementById('pl-events'); s.scrollTop = s.scrollHeight; });
      await page.fill('#pl-input', 'iso 周报写到哪了？');
      await page.press('#pl-input', 'Enter');
      await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 周报写到哪了？' }]);
      await expect(stream.locator('.pl-pending')).toHaveCount(1);
      conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
      await expect(page.locator('#pl-target-note')).toHaveText('收到，正在看');
      let t = Date.now();
      conn.send({ type: 'session_state', key: PLANNER, state: 'running' });
      conn.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: snap('iso 周报写到哪了？'), time: t, uuid: 'mt-u6' } });
      conn.send({ type: 'event', key: PLANNER, event: { type: 'tool_use', tool: 'Read', summary: 'Read report.md', time: t + 1, uuid: 'mt-tu6' } });
      await expect(stream.locator('.pl-pending')).toHaveCount(0);
      await expect(stream.locator('.event.user')).toHaveCount(4);
      await expect(stream.locator('.event.tool_use')).toHaveCount(0);
      for (let i = 0; i < 4; i++) {
        t += 1000;
        const detail = 'iso 第 ' + (i + 1) + ' 段：周报的「' + ['合并的 PR', '回滚', '性能', '下周计划'][i] + '」部分写好了，' + '按模块列出改动和负责的会话，'.repeat(3) + '还差最后的汇总。';
        conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail, time: t, uuid: 'mt-s' + i } });
        await expect(stream.locator('.event.text').last()).toContainText('iso 第 ' + (i + 1) + ' 段');
        await expect.poll(() => atBottom(page)).toBe(true);
      }
      await expect(page.locator('#pl-target-note')).toHaveText('');
      // Scrolled up, a new event leaves you where you were.
      await page.evaluate(() => { document.getElementById('pl-events').scrollTop = 0; });
      conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail: 'iso 写完了。', time: t + 5000, uuid: 'mt-s9' } });
      await expect(stream.locator('.event.text').last()).toContainText('iso 写完了。');
      expect(await page.evaluate(() => document.getElementById('pl-events').scrollTop)).toBe(0);
      expect(await layoutProblems(page)).toEqual([]);
      await page.evaluate(() => { const s = document.getElementById('pl-events'); s.scrollTop = s.scrollHeight; });
      await page.screenshot({ path: test.info().outputPath(`planner-turns-streamed-${vp.name}.png`) });
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
