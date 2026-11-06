// E2E: v0.5.1's simpler dashboard (D94). A failed row says why in words and
// whether custody gave up on it, a background task that runs a while says
// so, a title is its prompt's first sentence, and on a phone a group of one
// session shows no header (Q7, Q19, D6, A1). A turn no session_state ends
// (the CLI woke on its own) still counts unread, refreshes its row and, in a
// background tab, notifies once (Q14). An answered question folds to one
// line, custody's answer by the recommended option on it with 撤回 (Q9). The
// header keeps the session's name, a dispatched session's first message
// drops the gateway's note (Q10, Q11), the latest answer's 复制 shows and
// copies formatted text (Q16). The idle Home leaves 最近会话 to the sidebar,
// Esc closes the history popover from its search box (Q17, Q18). The Planner
// writes names and cards for the session keys it names, and its session
// cards no tool names (Q18, Q12); its left column and every count hold only
// what waits on you (D5). A phone's session view keeps the bottom bar (Q15),
// and custody given up shows on the health table and the task card (D2).
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const C = 'dashboard:direct:2026-01-01-120002-3:myproject';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

const lastConn = (mock) => mock.wsConnections[mock.wsConnections.length - 1];

async function openSession(page, mock, key, events) {
  await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), key);
  const conn = lastConn(mock);
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  conn.send({ type: 'subscribed', key, state: 'ready' });
  conn.send({ type: 'history', key, initial: true, events });
}

// push sends the open session's live events, as the hub's event push does.
const push = (mock, key, events) => lastConn(mock).send({ type: 'history', key, events });

function stubNotification() {
  window.__notes = [];
  window.__hidden = false;
  class FakeNotification {
    constructor(title, opts) { this.title = title; this.opts = opts || {}; window.__notes.push({ title, body: this.opts.body, tag: this.opts.tag }); }
    close() {}
  }
  FakeNotification.permission = 'granted';
  FakeNotification.requestPermission = async () => 'granted';
  window.Notification = FakeNotification;
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.__hidden });
}

for (const [name, vp] of [['desktop', DESKTOP], ['phone', PHONE]]) {
  test(`session rows say a failure, a long background task and the first sentence (${name})`, async ({ browser }) => {
    const ctx = await browser.newContext(vp);
    const page = await ctx.newPage();
    const s = defaultSessions();
    const [a, b, c] = s.sessions;
    a.last_response = 'API Error: 500 {"type":"error","error":{"type":"api_error"}}';
    a.health = { state: 'failed', since: now - 30000, error_class: 'network' };
    a.last_active = now - 60000;
    b.state = 'ready';
    b.health = { state: 'active', since: now - 45 * 60000 - 5000, background_task: true };
    b.last_active = now - 50 * 60000;
    c.last_prompt = '修一下登录页的样式，跑 `go test ./...`。然后再看看手机上的布局';
    const mock = await startMockServer({
      ws: true, sessions: s,
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      custody: { paused: false, sessions: [] },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      const row = (k) => page.locator('.session-card[data-key="' + k + '"]');
      // The failure in words, the raw reply in the title (Q7).
      await expect(row(A).locator('.sc-response')).toHaveText('失败 · 网络断了');
      await expect(row(A).locator('.sc-response')).toHaveAttribute('title', /API Error: 500/);
      await expect(row(A)).not.toContainText('"type"');
      // A background task past 30 minutes (A1).
      await expect(row(B).locator('.sc-response')).toHaveText('后台任务 · 已跑 45 分钟');
      // The prompt's first sentence, without code (Q19).
      await expect(row(C).locator('.sc-prompt')).toHaveText('修一下登录页的样式，跑');
      // Custody gave up on the failed one: the row says so.
      s.sessions[0].health.since = now - 20000;
      await page.evaluate(async () => {
        const c = window.nz.test.custody;
        c.byKey.set('dashboard:direct:2026-01-01-120000-1:myproject', { key: 'dashboard:direct:2026-01-01-120000-1:myproject', on: true, state: 'stopped', why: '服务端错误', sent: 3, max: 8 });
        window.nz.bus.dispatchEvent(new CustomEvent('custody:changed'));
      });
      await expect(row(A).locator('.sc-response')).toHaveText('失败 · 网络断了 · 托管已停');
      // A group of one session: a phone shows its card alone, the group on it (D6).
      const single = page.locator('.section-header.sh-single');
      await expect(single).toHaveCount(1);
      if (name === 'phone') {
        await expect(single).toBeHidden();
        await expect(row(B).locator('.sc-group')).toHaveText('otherproject');
      } else {
        await expect(single).toBeVisible();
        await expect(row(B).locator('.sc-group')).toBeHidden();
      }
      await page.screenshot({ path: test.info().outputPath('rows-' + name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('a turn no session_state ends counts unread, refreshes its row and notifies once in the background', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  await ctx.addInitScript(stubNotification);
  const page = await ctx.newPage();
  const s = defaultSessions();
  s.sessions[0].last_response = 'iso 已在后台跑起来了';
  s.sessions[2].last_response = 'iso 好的';
  const mock = await startMockServer({ ws: true, sessions: s });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSession(page, mock, C, [{ type: 'user', detail: 'iso 跑一下', time: now - 60000, uuid: 'bg-u1' }]);
    const row = page.locator('.session-card[data-key="' + A + '"]');
    await expect(row.locator('.sc-response')).toHaveText('iso 已在后台跑起来了');

    // Another session's reply moves with no turn pushed: the tab is in the
    // background, so one notification, and the row's count and preview move.
    await page.evaluate(() => { window.__hidden = true; });
    s.sessions[0].last_response = 'iso 后台任务跑完了：结果 42';
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(row.locator('.sc-unread')).toHaveText('1');
    await expect(row.locator('.sc-response')).toHaveText('iso 后台任务跑完了：结果 42');
    await expect.poll(() => page.evaluate(() => window.__notes)).toEqual([{ title: 'hello world 跑完了', body: 'iso 后台任务跑完了：结果 42', tag: 'turn-end:local:' + A }]);
    // The same poll again is no news.
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(row.locator('.sc-unread')).toHaveText('1');

    // The open session: a result with no turn running ends a turn the CLI
    // started; one ending the turn a send started is not news.
    push(mock, C, [{ type: 'result', detail: 'iso 后台的 sleep 结束了', time: Date.now(), uuid: 'bg-r1' }]);
    await expect.poll(() => page.evaluate(() => window.__notes.length)).toBe(2);
    expect((await page.evaluate(() => window.__notes))[1]).toMatchObject({ title: 'fix the bug 跑完了', body: 'iso 后台的 sleep 结束了' });
    await page.evaluate((k) => { const d = window.nz.test.sessionsData; for (const v of Object.values(d)) if (v.key === k) v.state = 'running'; }, C);
    push(mock, C, [{ type: 'result', detail: 'iso 回合结束', time: Date.now() + 1, uuid: 'bg-r2' }]);
    await page.waitForTimeout(500);
    expect(await page.evaluate(() => window.__notes.length)).toBe(2);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('an answered question folds to one line; custody\'s answer is that line, with 撤回', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const ask = (id, t, question, labels) => ({ type: 'ask_question', time: t, uuid: 'aq-' + id,
    ask_question: { tool_use_id: id, items: [{ header: '格式', question, options: labels.map((label) => ({ label })) }] } });
  const events = [
    { type: 'user', detail: 'iso 导出一下', time: now - 600000, uuid: 'q-u1' },
    ask('t1', now - 500000, '导出成什么格式？', ['CSV (Recommended)', 'JSON']),
    { type: 'user', source: 'custody', custody_reason: '按建议执行', custody_n: 1, detail: '格式: CSV (Recommended).', time: now - 400000, uuid: 'q-c1' },
    { type: 'text', detail: '好，导出 CSV', time: now - 390000, uuid: 'q-t1' },
    ask('t2', now - 300000, '要不要压缩？', ['压缩 (Recommended)', '不压缩']),
  ];
  const mock = await startMockServer({
    ws: true, planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    custody: { paused: false, sessions: [{ key: A, on: true, state: 'on', sent: 1, max: 8 }], undo: '格式: CSV.' },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSession(page, mock, A, events);
    const pane = page.locator('#events-scroll');
    const first = pane.locator('.event.ask_question[data-tool-use-id="t1"]');
    await expect(first).toHaveClass(/ask-done/);
    await expect(first.locator('.ask-title')).toHaveText('提问');
    await expect(first.locator('.ask-status')).toHaveText('已按建议选：CSV·撤回');
    await expect(first.locator('.ask-opt').first()).toBeHidden();
    await expect(first).toHaveAttribute('title', /导出成什么格式？/);
    // Custody's answer has no bubble of its own: the card says it.
    await expect(pane.locator('.event.user.custody-msg')).toHaveCount(0);
    await expect(pane).not.toContainText('Recommended');

    // The open one: 提问, one question's 提交, the labels without the marker.
    const second = pane.locator('.event.ask_question[data-tool-use-id="t2"]');
    await expect(second.locator('.ask-title')).toHaveText('提问');
    await expect(second.locator('.ask-opt-label')).toHaveText(['压缩', '不压缩']);
    await expect(second.locator('.ask-submit')).toHaveText('提交');
    await page.screenshot({ path: test.info().outputPath('ask-desktop.png') });
    // Custody answers it live: the card folds, its 撤回 the latest's.
    push(mock, A, [{ type: 'user', source: 'custody', custody_reason: '按建议执行', custody_n: 2, detail: '格式: 压缩 (Recommended).', time: Date.now(), uuid: 'q-c2' }]);
    await expect(second).toHaveClass(/ask-done/);
    await expect(second.locator('.ask-status')).toHaveText('已按建议选：压缩·撤回');
    await expect(pane.locator('.event.user.custody-msg')).toHaveCount(0);
    await expect(first.locator('[data-action="custody-undo"]')).toBeHidden();
    await second.locator('[data-action="custody-undo"]').click();
    await expect.poll(() => mock.custodyCalls.at(-1)).toEqual({ undo: A });
    await expect(second.locator('.custody-undone')).toHaveText('已发撤回');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the header keeps the name; the gateway\'s note and Markdown stay out of what you read and copy', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  await ctx.addInitScript(() => {
    window.__copied = null;
    const write = async (items) => {
      const out = {};
      for (const it of items) for (const t of it.types) out[t] = await (await it.getType(t)).text();
      window.__copied = out;
    };
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { write, writeText: async (t) => { window.__copied = { raw: t }; } } });
  });
  const page = await ctx.newPage();
  const s = defaultSessions();
  s.sessions[0].user_label = 'iso 命名会话';
  const mock = await startMockServer({ ws: true, sessions: s });
  const note = '\n\n（网关附注：做完、卡住或判断做不了时，用 SendMessage 发给 planner-abc123 汇报。）';
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSession(page, mock, A, [
      { type: 'user', detail: 'iso 把周报整理一下' + note, time: now - 600000, uuid: 'h-u1' },
      { type: 'text', detail: '第一版', time: now - 590000, uuid: 'h-t1' },
      { type: 'text', detail: '整理好了：**三件事** 和 `make test`', time: now - 580000, uuid: 'h-t2' },
    ]);
    const pane = page.locator('#events-scroll');
    await expect(pane.locator('.event.user .event-content')).toHaveText('iso 把周报整理一下');
    await expect(pane).not.toContainText('网关附注');
    // A message you send leaves the name you gave the session.
    // The heading's text: its rename and export buttons follow it.
    const h2 = page.locator('.main-header h2');
    const title = () => h2.evaluate((el) => el.firstChild.data);
    await expect.poll(title).toBe('iso 命名会话');
    push(mock, A, [{ type: 'user', detail: '继续', time: Date.now(), uuid: 'h-u2' }]);
    await expect(pane.locator('.event.user[data-uuid="h-u2"]')).toBeVisible();
    await expect.poll(title).toBe('iso 命名会话');
    await expect(h2.locator('.btn-rename').first()).toBeAttached();

    // The latest answer's 复制 shows without a hover; it copies the answer
    // formatted, and as text without Markdown marks.
    const copy = pane.locator('.event.text[data-uuid="h-t2"] .event-copy-btn');
    await expect(copy).toHaveCSS('opacity', '1');
    await expect(pane.locator('.event.text[data-uuid="h-t1"] .event-copy-btn')).toHaveCSS('opacity', '0');
    await copy.click();
    await expect.poll(() => page.evaluate(() => window.__copied)).not.toBeNull();
    const copied = await page.evaluate(() => window.__copied);
    expect(copied['text/plain']).toBe('整理好了：三件事 和 make test');
    expect(copied['text/html']).toContain('<strong>三件事</strong>');
    expect(copied['text/html']).not.toContain('复制');

    // A session with no name follows its prompt's first sentence.
    await openSession(page, mock, C, [{ type: 'user', detail: 'fix the bug', time: now - 60000, uuid: 'h-c1' }]);
    await expect.poll(title).toBe('fix the bug');
    push(mock, C, [{ type: 'user', detail: 'iso 再看看样式。顺便跑测试', time: Date.now(), uuid: 'h-c2' }]);
    await expect.poll(title).toBe('iso 再看看样式');
    await expect(h2.locator('.btn-rename').first()).toBeAttached();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the idle Home leaves 最近会话 to the sidebar; Esc closes the history popover from its search box', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('#recent-sessions-panel .recent-row').first()).toBeAttached();
    await expect(page.locator('#recent-sessions-panel')).toBeHidden();
    await page.evaluate(() => document.body.classList.add('sidebar-collapsed'));
    await expect(page.locator('#recent-sessions-panel')).toBeVisible();
    await page.evaluate(() => document.body.classList.remove('sidebar-collapsed'));

    await page.click('#btn-history');
    const search = page.locator('#hp-search');
    await search.focus();
    await page.keyboard.press('Escape');
    await expect(page.locator('.history-popover')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
});

for (const [name, vp] of [['desktop', DESKTOP], ['phone', PHONE]]) {
  test(`the Planner names the sessions it writes keys for; every count holds what waits on you (${name})`, async ({ browser }) => {
    const ctx = await browser.newContext(vp);
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true,
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: [
        { type: 'user', detail: 'iso 昨天 g6e 的结论在哪？', time: now - 60000, uuid: 'pn-u1' },
        { type: 'text', detail: '在 ' + A + ' 里，另一个 ' + B + ' 还在跑。', time: now - 50000, uuid: 'pn-t1' },
      ],
      inbox: { open: [
        { id: 'in_q', kind: 'question', level: 'timely', unit: A, title: 'iso 会话在等你回复', created: iso(now - 120000), state: 'open', source: 'waiting:' + A },
        { id: 'in_n', kind: 'notice', level: 'timely', unit: PLANNER, title: 'iso 巡检：一切正常', created: iso(now - 60000), state: 'open', source: 'planner:n1' },
      ], closed: [], counts: { open: 2, badge: 1 } },
      tasks: { tasks: [] },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await expect(page.locator('#abnav-planner-count')).toHaveText('1');
      if (name === 'desktop') await expect(page.locator('#sw-n')).toHaveText('1');
      await page.click('#abnav-planner');
      const reply = page.locator('#pl-stream .event.text[data-uuid="pn-t1"]');
      await expect(reply.locator('.pl-scard')).toHaveCount(2);
      await expect(reply.locator('.event-content')).toContainText('在 hello world 里，另一个 review this code 还在跑。');
      await expect(reply).not.toContainText(A);
      // A session card says its state, not the tool it is in (Q12).
      await expect(reply.locator('.pl-scard[data-key="' + B + '"]')).not.toContainText('reviewing code');
      // One count: the left column lists the question, not the notice (D5).
      if (name === 'phone') {
        await expect(page.locator('#pl-left-toggle')).toHaveText('等你 1 · 任务 0');
        await page.click('#pl-left-toggle');
      }
      await expect(page.locator('#pl-wait .pl-card')).toHaveCount(1);
      await expect(page.locator('#pl-wait')).not.toContainText('巡检');
      await page.screenshot({ path: test.info().outputPath('planner-' + name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('a phone\'s session view keeps the bottom bar and drops the ▲▼ pill and the branch chip', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.locator('.session-card[data-key="' + A + '"]').tap();
    await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
    await expect(page.locator('.activity-bar')).toBeVisible();
    await expect(page.locator('#abnav-planner')).toBeInViewport();
    const [bar, input] = await Promise.all([page.locator('.activity-bar').boundingBox(), page.locator('#msg-input').boundingBox()]);
    expect(input.y + input.height).toBeLessThanOrEqual(bar.y + 1);
    await expect(page.locator('.nav-pill')).toBeHidden();
    await expect(page.locator('.main-header .detail-git')).toBeHidden();
    await page.screenshot({ path: test.info().outputPath('session-phone.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('custody given up shows on the health table and the task card; custody on goes unsaid', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 修登录页',
    purpose_title: 'iso 修登录页', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  const task2 = 'task:ffeeddccbbaa';
  s.sessions.push({ key: task2, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 整理周报',
    purpose_title: 'iso 整理周报', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  const mock = await startMockServer({
    ws: true, sessions: s,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    custody: { paused: false, sessions: [{ key: TASK, on: true, state: 'stopped', why: '托管重试了 3 次还是出错', sent: 3, max: 8 }, { key: task2, on: true, state: 'on', sent: 0, max: 8 }] },
    tasks: { tasks: [
      { id: TASK, kind: 'session', title: 'iso 修登录页', state: 'failed', by: 'health', at: iso(now - 60000), created: iso(now - 3600000) },
      { id: task2, kind: 'session', title: 'iso 整理周报', state: 'doing', by: 'planner', at: iso(now - 60000), created: iso(now - 3600000) },
    ] },
    systemHealth: { at: iso(now - 20000), unowned: 1, units: [
      { id: TASK, kind: 'session', source: 'router', coverage: 'full', state: 'failed', since: iso(now - 60000), last_active: iso(now - 60000) },
      { id: task2, kind: 'session', source: 'router', coverage: 'full', state: 'idle', since: iso(now - 60000), last_active: iso(now - 60000) },
    ] },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-task .tk-card[data-id="' + TASK + '"] .custody-chip')).toHaveText('托管已停托管重试了 3 次还是出错');
    await expect(page.locator('#pl-task .tk-card[data-id="' + task2 + '"]')).toBeVisible();
    await expect(page.locator('#pl-task .tk-card[data-id="' + task2 + '"] .custody-chip')).toHaveCount(0);
    await page.click('#abnav-system');
    await expect(page.locator('tr[data-unit="' + TASK + '"] .hl-state')).toHaveText('失败 · 托管已停');
    await expect(page.locator('tr[data-unit="' + task2 + '"] .hl-state')).toHaveText('空闲');
    await page.screenshot({ path: test.info().outputPath('system-desktop.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
