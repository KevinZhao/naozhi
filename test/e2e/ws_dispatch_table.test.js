// @ts-check
// The receive dispatch table (wsm.on / wsm.onMessage), driven through a real
// socket (S18, #3024): reconnect re-subscribes, an unclaimed frame is dropped
// without throwing, unsubscribe resets the event cursor, auth_fail runs the
// onAuthFail UI (dashboard.js) before ws_manager closes the socket, and state
// changes reach dashboard's pollers through onStateChange.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs, waitForWsWhere } = require('./shim_wait');

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
    await waitForWs(page);
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
    await waitForWs(page);
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
    // auth_modal's new-session paths call sessionStream.unsubscribe() and leave the
    // subscribe to sessions_update's auto-subscribe, which (unlike
    // selectSession) does not zero lastEventTimeWs itself.
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page);
    await page.evaluate((key) => {
      sessionStream.lastEventTimeWs = 12345;
      sessionStream.unsubscribe();
      sessionStream.subscribe(key, 'local');
    }, KEY_B);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => subscribes(conn, KEY_B).length).toBe(1);
    const sub = subscribes(conn, KEY_B)[0];
    expect(sub.after).toBeUndefined();
    expect(sub.limit).toBeGreaterThan(0);
    await ctx.close();
  });

  test('auth_fail reaches the onAuthFail UI before the socket closes: a rate limit arms the block, a bad token shows the error', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page);
    let conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'auth_fail', error: 'too many attempts', retry_after: 30 });
    await waitForWs(page, 'DISCONNECTED');
    const block = await page.evaluate(() => ({ left: wsm._authBlockUntil - Date.now(), timer: wsm.reconnectTimer !== null }));
    expect(block.left, 'retry_after arms the redial block').toBeGreaterThan(25000);
    expect(block.left, 'the block is retry_after long, not the 60s fallback').toBeLessThanOrEqual(30000);
    expect(block.timer).toBe(true);

    // Lift the block (as the countdown does at expiry): it redials at once.
    const before = mock.wsConnections.length;
    await page.evaluate(() => { wsm._authBlockUntil = Date.now(); });
    await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBe(before + 1);
    await waitForWs(page);
    conn = mock.wsConnections[before];
    conn.send({ type: 'auth_fail', error: 'invalid token' });
    await waitForWs(page, 'DISCONNECTED');
    await expect(page.locator('#toast')).toContainText('WebSocket 鉴权失败');
    await ctx.close();
  });

  test('wsm state changes reach dashboard: CONNECTED stops the REST session poll, a drop re-arms it and hands the WS cursor to the REST poll', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page);
    expect(await page.evaluate(() => sessionPollTimer), 'the boot-time session poll stops once the socket is up').toBeNull();
    await page.click(`.session-card[data-key="${KEY_A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => subscribes(conn, KEY_A).length).toBe(1);
    const T = Date.now() + 60000; // later than any fixture event
    conn.send({ type: 'event', key: KEY_A, event: { type: 'text', detail: 'c', summary: 'c', time: T, uuid: 'u-cursor' } });
    await page.waitForFunction((t) => sessionStream.lastEventTimeWs === t, T);
    conn.close();
    // Read in the tick the drop is seen: the reconnect re-enters CONNECTED.
    const dropped = await (await waitForWsWhere(page, () => wsm.state === WS_STATES.DISCONNECTED &&
      { poll: sessionPollTimer !== null, cursor: lastEventTime })).jsonValue();
    expect(dropped).toEqual({ poll: true, cursor: T });
    await ctx.close();
  });
});
