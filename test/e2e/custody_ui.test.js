// E2E: custody in the dashboard (custody.js, planner RFC §5.4, D93, D94).
// The tab tells the gateway which session you are at (viewing frames: the
// session open in a visible, focused tab; '' once you switch away, leave the
// tab or open another view; again after a reconnect). The session header's
// chip is the one place custody shows as a switch: it says 开 while custody
// holds back on a session you look at (the hold is in its title), and a
// click switches it as yours with a toast saying what it did. Custody's
// messages name custody as their sender in a faint line (托管 · 原因 · 第 n
// 次: from the event's custody_reason / custody_n, or an older text's
// prefix), and the latest answer given by its recommended option is taken
// back with 撤回 (POST /api/custody/undo: the gateway finds it and tells the
// session in the user's name). A task card marks custody only when it is off
// or stopped, the "⋯" menu has one pause for pushes and custody, and the
// system view's health table has no 托管 column.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const CHOICE = '方案: 先补测试 (Recommended)';
const EARLIER = '分支: 新开分支 (Recommended)';
// The choices as shown: without the recommended marker.
const CHOICE_SHOWN = '方案: 先补测试';
const EARLIER_SHOWN = '分支: 新开分支';

const EVENTS = [
  { type: 'user', detail: 'iso 修一下登录页', time: now - 600000, uuid: 'cu-u1' },
  { type: 'text', detail: '接口超时，先停下了', time: now - 590000, uuid: 'cu-t1' },
  { type: 'user', source: 'custody', detail: '⚙ 托管 · 按建议执行 · 第 1/8 次\n' + EARLIER + '.', time: now - 550000, uuid: 'cu-c0' },
  { type: 'user', source: 'custody', detail: '⚙ 托管 · 出错重试 · 第 1/8 次\n继续', time: now - 500000, uuid: 'cu-c1' },
  { type: 'text', detail: '要先补测试还是先改代码？', time: now - 400000, uuid: 'cu-t2' },
  { type: 'user', source: 'custody', detail: '⚙ 托管 · 按建议执行 · 第 2/8 次\n' + CHOICE + '.', time: now - 300000, uuid: 'cu-c2' },
  { type: 'text', detail: '好，先补测试', time: now - 290000, uuid: 'cu-t3' },
  // Since D94 the text is the body alone; the event says the reason and the count.
  { type: 'user', source: 'custody', custody_reason: '出错重试', custody_n: 2, detail: '继续', time: now - 280000, uuid: 'cu-c3' },
];
const NOTES = [
  { id: 'cn1', key: TASK, time: iso(now - 520000), kind: 'custody', event: 'retry', evidence: '网络', n: 1 },
  { id: 'cn2', key: TASK, time: iso(now - 200000), kind: 'custody', event: 'deferred', evidence: '你在看' },
];

function sessions() {
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 修登录页',
    purpose_title: 'iso 修登录页', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  return s;
}

function fixture() {
  return {
    ws: true,
    sessions: sessions(),
    notes: { [TASK]: NOTES },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [],
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    tasks: { tasks: [{ id: TASK, kind: 'session', title: 'iso 修登录页', state: 'doing', by: 'planner', at: iso(now - 60000), created: iso(now - 3600000) }] },
    custody: { paused: false, sessions: [{ key: TASK, on: true, state: 'on', sent: 2, max: 8 }], undo: CHOICE + '.' },
    systemHealth: {
      at: iso(now - 20000), unowned: 0,
      units: [
        { id: TASK, kind: 'session', source: 'router', coverage: 'full', state: 'idle', since: iso(now - 60000), last_active: iso(now - 60000) },
        { id: A, kind: 'session', source: 'router', coverage: 'full', state: 'idle', since: iso(now - 60000), last_active: iso(now - 60000) },
        { id: PLANNER, kind: 'session', source: 'router', coverage: 'full', state: 'idle', since: iso(now - 60000) },
      ],
    },
  };
}

const lastConn = (mock) => mock.wsConnections[mock.wsConnections.length - 1];
// viewed is the session the tab last said you are at; undefined before any.
const viewed = (mock) => {
  const v = lastConn(mock).messages.filter((m) => m.type === 'viewing');
  return v.length ? v[v.length - 1].key : undefined;
};

async function openSession(page, mock, key, events) {
  await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), key);
  const conn = lastConn(mock);
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  conn.send({ type: 'subscribed', key, state: 'ready' });
  conn.send({ type: 'history', key, initial: true, events });
}

for (const [name, vp] of [['desktop', { viewport: { width: 1440, height: 900 } }], ['phone', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }]]) {
  test(`custody in the session view, the Planner page and the system view (${name})`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext(vp);
    const page = await ctx.newPage();
    const mock = await startMockServer(fixture());
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');

      // At the session: the gateway is told, and the chip says 开 though
      // custody holds back while you look (Q2): the hold is in its title.
      await openSession(page, mock, TASK, EVENTS);
      await expect.poll(() => viewed(mock)).toBe(TASK);
      const chip = page.locator('#header-custody .custody-chip');
      await expect(chip).toHaveClass(/custody-on/);
      await expect(chip).toHaveText('托管开');
      await expect(chip).toHaveAttribute('title', /你在看或刚给它发过消息时，先不替你回话/);
      await expect(chip).not.toHaveAttribute('title', /\/8/);
      await expect(chip).toHaveAttribute('aria-pressed', 'true');

      // Custody's messages: custody is the sender, a faint line with the
      // reason and the count, no cap and no second ⚙ (Q5).
      const pane = page.locator('#events-scroll');
      const all = pane.locator('.event.user.custody-msg');
      await expect(all).toHaveCount(4);
      const earlier = all.nth(0);
      const msgs = pane.locator('.event.user.custody-msg:not([data-uuid="cu-c0"])');
      await expect(msgs.nth(0).locator('.custody-source')).toHaveText('托管 · 出错重试 · 第 1 次');
      // An older text's prefix once, as the source line; the body after it.
      await expect(msgs.nth(0).locator('.event-content')).toHaveText('托管 · 出错重试 · 第 1 次继续');
      await expect(msgs.nth(0).locator('.event-icon')).toHaveText('⚙');
      // A message from since D94: the event's fields, the text the body alone.
      const fielded = pane.locator('.event.user.custody-msg[data-uuid="cu-c3"]');
      await expect(fielded.locator('.custody-source')).toHaveText('托管 · 出错重试 · 第 2 次');
      await expect(fielded.locator('.event-content')).toHaveText('托管 · 出错重试 · 第 2 次继续');
      await expect(msgs.nth(1).locator('.custody-suggested')).toHaveText(/^已按建议执行：方案: 先补测试·撤回$/);
      await expect(msgs.nth(1).locator('.custody-suggested')).not.toContainText('Recommended');
      // 撤回 only on the latest: the gateway takes back that one.
      await expect(earlier.locator('.custody-suggested')).toContainText('已按建议执行：' + EARLIER_SHOWN);
      await expect(earlier.locator('[data-action="custody-undo"]')).toBeHidden();
      await expect(msgs.nth(1).locator('[data-action="custody-undo"]')).toBeVisible();
      await expect(pane.locator('.event.user:not(.custody-msg)')).toHaveCount(1);
      // What custody did is said among the bubbles; its messages are not notes.
      await expect(pane.locator('.gw-note[data-note-id="cn1"]')).toContainText('托管：出错（网络），稍后自动重试 · 第 1 次');
      await expect(pane.locator('.gw-note[data-note-id="cn2"]')).toContainText('托管：已暂缓（你在看）');
      await page.locator('#main .main-header').screenshot({ path: test.info().outputPath('custody-header-' + name + '.png') });
      await msgs.nth(1).scrollIntoViewIfNeeded();
      await page.screenshot({ path: test.info().outputPath('custody-session-' + name + '.png') });

      // 撤回 asks the gateway to take the answer back; nothing goes out as a
      // plain send.
      const sends = mock.sendCalls.length;
      await msgs.nth(1).locator('[data-action="custody-undo"]').click();
      await expect.poll(() => mock.custodyCalls.at(-1)).toEqual({ undo: TASK });
      await expect(msgs.nth(1).locator('.custody-undone')).toHaveText('已发撤回');
      expect(mock.sendCalls.length).toBe(sends);

      // One click switches it as yours, and another back on; a toast says
      // what each click did (Q3: a phone has no title to hover).
      await chip.click();
      await expect.poll(() => mock.custodyCalls.at(-1)).toEqual({ key: TASK, on: false });
      await expect(chip).toHaveClass(/custody-off/);
      await expect(chip).toHaveAttribute('aria-pressed', 'false');
      await expect(page.locator('#toast')).toHaveText(/^托管已关/);
      await chip.click();
      await expect.poll(() => mock.custodyCalls.at(-1)).toEqual({ key: TASK, on: true });
      await expect(chip).toHaveClass(/custody-on/);
      await expect(page.locator('#toast')).toHaveText(/^托管已打开：.*再点一下关掉$/);

      // Leaving the tab ends it; coming back says it again.
      await page.evaluate(() => {
        document.hasFocus = () => false;
        window.dispatchEvent(new Event('blur'));
      });
      await expect.poll(() => viewed(mock)).toBe('');
      await expect(chip).toHaveClass(/custody-on/);
      await page.evaluate(() => {
        delete document.hasFocus;
        window.dispatchEvent(new Event('focus'));
      });
      await expect.poll(() => viewed(mock)).toBe(TASK);

      // A dropped socket forgets it on the gateway: the new one says it again.
      lastConn(mock).close();
      await expect.poll(() => mock.wsConnections.length).toBe(2);
      await waitForWs(page, 'CONNECTED');
      await expect.poll(() => viewed(mock)).toBe(TASK);

      // Another session: that one is viewed, and its custody is off.
      await openSession(page, mock, A, [{ type: 'user', detail: 'iso 你好', time: now - 1000, uuid: 'cu-a1' }]);
      await expect.poll(() => viewed(mock)).toBe(A);
      await expect(chip).toHaveClass(/custody-off/);
      await expect(chip).toContainText('关');

      // The Planner page shows no session of its own: none is viewed.
      if (name === 'phone') await page.click('[data-action="mobile-back"]');
      await page.click('#abnav-planner');
      await expect.poll(() => viewed(mock)).toBe('');
      if (name === 'phone') await page.click('#pl-left-toggle');
      // A dispatched session in custody, as it runs: its card says nothing of it (D2).
      await expect(page.locator('#pl-task .tk-card[data-id="' + TASK + '"]')).toBeVisible();
      await expect(page.locator('#pl-task .custody-chip')).toHaveCount(0);
      await page.locator('#pl-task').screenshot({ path: test.info().outputPath('custody-tasks-' + name + '.png') });

      // One pause for pushes and custody (D4), said in the header; then resume.
      if (name === 'phone') await page.click('#pl-left-toggle');
      await page.click('#pl-more > summary');
      const tools = page.locator('#pl-tools');
      await expect(tools.locator('.pl-tool-l')).toHaveText(['暂停推送与托管']);
      await tools.screenshot({ path: test.info().outputPath('custody-menu-' + name + '.png') });
      await tools.locator('[data-action="planner-pause"][data-for="1h"]').click();
      await expect.poll(() => mock.inboxCalls.at(-1)).toMatchObject({ method: 'PUT', body: { for: '1h' } });
      await expect(page.locator('#pl-paused')).toBeVisible();
      await expect(page.locator('#pl-paused')).toContainText(/已暂停到 \d\d:\d\d/);
      await expect.poll(() => page.evaluate(() => window.nz.test.custody.paused)).toBe(true);
      await page.locator('#pl-center > .pl-head').screenshot({ path: test.info().outputPath('custody-paused-' + name + '.png') });
      // A phone's header says until when; 恢复 is in "⋯" there.
      if (name === 'phone') {
        await page.click('#pl-more > summary');
        await tools.locator('[data-action="planner-pause-resume"]').click();
      } else {
        await page.locator('#pl-paused [data-action="planner-pause-resume"]').click();
      }
      await expect.poll(() => mock.inboxCalls.at(-1)).toMatchObject({ method: 'DELETE' });
      await expect(page.locator('#pl-paused')).toBeHidden();
      await expect.poll(() => page.evaluate(() => window.nz.test.custody.paused)).toBe(false);

      // The health table has no 托管 column (the system view is desktop only).
      if (name === 'desktop') {
        await page.click('#abnav-system');
        const table = page.locator('.hl-table');
        await expect(table.locator('tr[data-unit="' + TASK + '"]')).toBeVisible();
        await expect(table.locator('thead th')).toHaveText(['会话', '工作区', '健康状态', '最后活动']);
        await expect(table).not.toContainText('托管');
        await page.locator('.sys-health').screenshot({ path: test.info().outputPath('custody-health-' + name + '.png') });
      }
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

// Without the Planner custody does not run: no chip, no column entries.
test('no custody chip with the Planner off', async ({ page }) => {
  const mock = await startMockServer({ ws: true, sessions: sessions() });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSession(page, mock, TASK, EVENTS);
    await expect(page.locator('#events-scroll .event.user.custody-msg')).toHaveCount(4);
    await expect(page.locator('#header-custody')).toBeEmpty();
  } finally {
    mock.server.close();
  }
});
