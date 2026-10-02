// @ts-check
// sessionFrames — the history / event / send_ack / session_state handlers that
// left wsm in S18d1 (#3024) — still keep the session subscription's
// bookkeeping on wsm, driven through a real socket:
//  - a reconnect resumes after the last event a history or an event frame
//    delivered (the cursor both frames advance);
//  - a send_ack for another session subscribes it from an initial page (the
//    cursor is reset, not carried over);
//  - a running push for the session already subscribed does not resubscribe.
// A bookkeeping write that lands on sessionFrames instead of wsm leaves wsm's
// fields stale without throwing, so each case asserts a frame the mock saw.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);
const ev = (text, time) => ({ type: 'text', detail: text, summary: text, time, uuid: 'u-' + text });

// open selects KEY_A over a connected socket and acks its subscribe.
async function open(browser, mock) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.text().includes('ws parse error')) errors.push(m.text()); });
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  await page.click(`.session-card[data-key="${KEY_A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, KEY_A).length).toBe(1);
  conn.send({ type: 'subscribed', key: KEY_A });
  await page.waitForFunction((key) => wsm.subscribedKey === key, KEY_A);
  return { ctx, page, conn, errors };
}

// shown waits until the frame carrying uuid u-<text> has been painted.
const shown = (page, text) => page.waitForSelector(`#events-scroll .event[data-uuid="u-${text}"]`);

// barrier: frames the page sent while handling earlier frames are on the
// socket ahead of a ping sent after them.
async function barrier(page, conn) {
  const n = conn.messages.filter((m) => m.type === 'ping').length;
  await page.evaluate(() => wsm.send({ type: 'ping' }));
  await expect.poll(() => conn.messages.filter((m) => m.type === 'ping').length).toBe(n + 1);
}

test.describe('sessionFrames keep the bookkeeping on wsm', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('a reconnect resumes after the last event a history or an event frame delivered', async ({ browser }) => {
    const { ctx, page, conn: first, errors } = await open(browser, mock);
    let conn = first;
    const T = Date.now() + 60000; // newer than anything the REST page painted
    const reconnect = async () => {
      const before = mock.wsConnections.length;
      conn.close();
      await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBe(before + 1);
      conn = mock.wsConnections[before];
      await expect.poll(() => subs(conn, KEY_A).length, { message: 'reconnect must re-subscribe' }).toBe(1);
      conn.send({ type: 'subscribed', key: KEY_A });
      return subs(conn, KEY_A)[0];
    };

    conn.send({ type: 'history', key: KEY_A, events: [ev('h1', T)] });
    await shown(page, 'h1');
    expect((await reconnect()).after, 'a history frame advances the resume cursor').toBe(T);

    conn.send({ type: 'event', key: KEY_A, event: ev('e1', T + 1) });
    await shown(page, 'e1');
    expect((await reconnect()).after, 'an event frame advances the resume cursor').toBe(T + 1);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a send_ack for another session subscribes it from an initial page', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'event', key: KEY_A, event: ev('a1', Date.now() + 60000) });
    await shown(page, 'a1');
    conn.send({ type: 'send_ack', id: 'r1', status: 'accepted', key: KEY_B });
    await expect.poll(() => subs(conn, KEY_B).length).toBe(1);
    const sub = subs(conn, KEY_B)[0];
    expect(sub.after, 'the cursor of the session left behind is not carried over').toBeUndefined();
    expect(sub.limit).toBeGreaterThan(0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a running push for the subscribed session does not resubscribe it', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await page.waitForFunction((key) => sessionsData[sid(key, 'local')].state === 'running', KEY_A);
    await barrier(page, conn);
    expect(subs(conn, KEY_A).length, 'an already-live subscription is left alone').toBe(1);
    expect(errors).toEqual([]);
    await ctx.close();
  });
});
