// @ts-check
// The receive dispatch table (wsm.on / wsm.onMessage), driven through a real
// socket (S18, #3024): reconnect re-subscribes, an unclaimed frame is dropped
// without throwing, and unsubscribe resets the event cursor.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const subscribes = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);

test.describe('WS dispatch table', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('auth_ok on a reconnect re-subscribes the selected session', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click(`.session-card[data-key="${KEY_A}"]`);
    const first = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => subscribes(first, KEY_A).length).toBe(1);

    const before = mock.wsConnections.length;
    first.close();
    await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBe(before + 1);
    const second = mock.wsConnections[before];
    await expect.poll(() => second.messages.map((m) => m.type).slice(0, 1)).toEqual(['auth']);
    await expect.poll(() => subscribes(second, KEY_A).length, { message: 'reconnect must re-subscribe' }).toBe(1);
    await ctx.close();
  });

  test('a frame no handler claims is dropped without throwing', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const parseErrors = [];
    page.on('console', (m) => { if (m.text().includes('ws parse error')) parseErrors.push(m.text()); });
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    // Called synchronously: onmessage's try/catch would swallow a throw.
    const r = await page.evaluate(() => {
      wsm.onMessage({ type: 'no_such_frame' });
      wsm.onMessage({ type: 'run_started', subsystem: 'no_such_subsystem', owner_id: 'x' });
      return 'ok';
    });
    expect(r).toBe('ok');
    // And through the socket: an unknown frame, then one we can observe.
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'no_such_frame' });
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await page.waitForFunction((key) => sessionsData[sid(key, 'local')].state === 'running', KEY_A);
    expect(parseErrors).toEqual([]);
    await ctx.close();
  });

  test('unsubscribe resets the event cursor, so the next subscribe asks for an initial page', async ({ browser }) => {
    // auth_modal's new-session paths call wsm.unsubscribe() and leave the
    // subscribe to sessions_update's auto-subscribe, which (unlike
    // selectSession) does not zero lastEventTimeWs itself.
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.evaluate((key) => {
      wsm.lastEventTimeWs = 12345;
      wsm.unsubscribe();
      wsm.subscribe(key, 'local');
    }, KEY_B);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => subscribes(conn, KEY_B).length).toBe(1);
    const sub = subscribes(conn, KEY_B)[0];
    expect(sub.after).toBeUndefined();
    expect(sub.limit).toBeGreaterThan(0);
    await ctx.close();
  });
});
