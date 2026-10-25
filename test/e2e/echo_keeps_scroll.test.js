// @ts-check
// The echo of this tab's own send does not drag a reader back to the bottom.
// A message sent while a turn runs is queued; its real `user` event lands only
// after that turn's result, by which time the reader may have scrolled up to
// read the reply. The send already pinned the pane (renderOptimisticUserMsg),
// so on each path that replaces the optimistic bubble — a WS `event` frame
// (appendLiveEvent), a WS `history` frame (appendHistoryBackfill) and the
// REST poll (appendEvents) — the echo keeps the reader where they are. A user
// event with no optimistic bubble behind it (a teammate posting through IM)
// still pins the pane to the bottom.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);

// open selects KEY_A over a connected socket, acks its subscribe and paints an
// opening frame tall enough to scroll.
async function open(browser, mock) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  await page.click(`.session-card[data-key="${KEY_A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, KEY_A).length).toBe(1);
  conn.send({ type: 'subscribed', key: KEY_A });
  await page.waitForFunction((key) => sessionStream.subscribedKey === key, KEY_A);
  await page.evaluate(() => debouncedFetchSessions());
  const T = Date.now() + 60000;
  const events = [];
  for (let i = 0; i < 30; i++) {
    events.push({ type: 'user', detail: 'q' + i, summary: 'q' + i, time: T + i * 10, uuid: 'q' + i });
    events.push({ type: 'text', detail: ('answer ' + i + ' ').repeat(60), time: T + i * 10 + 1, uuid: 'a' + i });
  }
  conn.send({ type: 'history', key: KEY_A, initial: true, events });
  await page.waitForSelector('#events-scroll .event[data-uuid="a29"]');
  return { ctx, page, conn, errors, next: T + 1000 };
}

// frames lets the stickEventsBottom tail (two rAFs) run.
const frames = (page) => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));

// sendThenScrollUp paints the optimistic bubble a send renders (pinning the
// pane), then scrolls the reader up to read history.
async function sendThenScrollUp(page, text) {
  await page.evaluate((t) => renderOptimisticUserMsg(t), text);
  await frames(page);
  return page.evaluate(() => {
    const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
    el.scrollTop = 200;
    return el.scrollTop;
  });
}

const scrollTop = (page) => page.evaluate(() => /** @type {HTMLElement} */ (document.getElementById('events-scroll')).scrollTop);
const atBottom = (page) => page.evaluate(() => {
  const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
  return el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
});

// echoed waits until the real user event replaced the optimistic bubble.
async function echoed(page, uuid) {
  await page.waitForSelector(`#events-scroll .event[data-uuid="${uuid}"]`);
  await expect(page.locator('#events-scroll .optimistic-msg')).toHaveCount(0);
  await frames(page);
}

test.describe('the echo of this tab\'s send keeps the reader\'s scroll position', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('on a WS event frame', async ({ browser }) => {
    const { ctx, page, conn, errors, next } = await open(browser, mock);
    const top = await sendThenScrollUp(page, 'queued-1');
    conn.send({ type: 'event', key: KEY_A, event: { type: 'user', detail: 'queued-1', summary: 'queued-1', time: next, uuid: 'echo-1' } });
    await echoed(page, 'echo-1');
    expect(await scrollTop(page), 'the echo must not pin the pane').toBe(top);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('on a WS history frame', async ({ browser }) => {
    const { ctx, page, conn, errors, next } = await open(browser, mock);
    const top = await sendThenScrollUp(page, 'queued-2');
    // The queued message's echo rides behind the running turn's result.
    conn.send({ type: 'history', key: KEY_A, events: [
      { type: 'result', detail: '', summary: 'done', time: next, uuid: 'r-2' },
      { type: 'user', detail: 'queued-2', summary: 'queued-2', time: next + 1, uuid: 'echo-2' },
    ] });
    await echoed(page, 'echo-2');
    expect(await scrollTop(page), 'the echo must not pin the pane').toBe(top);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('on the REST poll', async ({ browser }) => {
    const { ctx, page, errors, next } = await open(browser, mock);
    const top = await sendThenScrollUp(page, 'queued-3');
    await page.evaluate((t) => appendEvents([{ type: 'user', detail: 'queued-3', summary: 'queued-3', time: t, uuid: 'echo-3' }]), next);
    await echoed(page, 'echo-3');
    expect(await scrollTop(page), 'the echo must not pin the pane').toBe(top);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a user event this tab did not send still pins the pane', async ({ browser }) => {
    const { ctx, page, conn, errors, next } = await open(browser, mock);
    await page.evaluate(() => { /** @type {HTMLElement} */ (document.getElementById('events-scroll')).scrollTop = 200; });
    conn.send({ type: 'history', key: KEY_A, events: [
      { type: 'user', detail: 'from IM', summary: 'from IM', time: next, uuid: 'im-1' },
    ] });
    await page.waitForSelector('#events-scroll .event[data-uuid="im-1"]');
    await frames(page);
    expect(await atBottom(page), 'a teammate\'s message must be visible').toBe(true);
    expect(errors).toEqual([]);
    await ctx.close();
  });
});
