// @ts-check
// A full repaint of the session on screen keeps a reader scrolled up where they
// stand. The opening frame of a resubscribe repaints the whole pane: after the
// server's subscription_timeout (the idle process was reaped, 60 s later) the
// next sessions_update resubscribes from scratch, as do a revive and a WS
// reconnect without a cursor; the REST fallback's full fetch repaints too.
// None of them is the reader opening the session, so none may pin the bottom.
// A reader who was at the bottom stays pinned; a session switch still restores
// the switch-away snapshot.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);

// page30 is a transcript tall enough to scroll: 30 question / answer pairs.
function page30(T) {
  const events = [];
  for (let i = 0; i < 30; i++) {
    events.push({ type: 'user', detail: 'q' + i, summary: 'q' + i, time: T + i * 10, uuid: 'q' + i });
    events.push({ type: 'text', detail: ('answer ' + i + ' ').repeat(60), time: T + i * 10 + 1, uuid: 'a' + i });
  }
  return events;
}

// open selects KEY_A over a connected socket and paints its opening frame.
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
  const events = page30(Date.now() + 60000);
  conn.send({ type: 'history', key: KEY_A, initial: true, events });
  await page.waitForSelector('#events-scroll .event[data-uuid="a29"]');
  await frames(page);
  return { ctx, page, conn, errors, events };
}

// frames lets the two-rAF tails of stickEventsBottom / the restores run.
const frames = (page) => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));

// scrollUp parks the reader mid-transcript and returns the bubble at the top of
// the viewport with its offset there.
async function scrollUp(page) {
  return page.evaluate(() => {
    const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
    el.scrollTop = 1500;
    const top = el.getBoundingClientRect().top;
    const b = /** @type {HTMLElement} */ ([...el.querySelectorAll(':scope > .event[data-uuid]')].find((n) => n.getBoundingClientRect().bottom > top));
    return { uuid: b.dataset.uuid, offset: Math.round(b.getBoundingClientRect().top - top) };
  });
}

// offsetOf is where the bubble `uuid` sits relative to the viewport top.
const offsetOf = (page, uuid) => page.evaluate((u) => {
  const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
  const b = [...el.querySelectorAll(':scope > .event[data-uuid]')].find((n) => /** @type {HTMLElement} */ (n).dataset.uuid === u);
  return b ? Math.round(b.getBoundingClientRect().top - el.getBoundingClientRect().top) : null;
}, uuid);

const atBottom = (page) => page.evaluate(() => {
  const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
  return el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
});

// resubscribeFromScratch drives the reaped-process chain: subscription_timeout,
// then a sessions_update whose refetch resubscribes without a cursor. It
// resolves once the opening frame `events` (plus `extra`) is painted.
async function resubscribeFromScratch(page, conn, events, extra) {
  const before = subs(conn, KEY_A).length;
  conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'ready', reason: 'subscription_timeout' });
  await page.waitForFunction(() => sessionStream.subscribedKey === null);
  conn.send({ type: 'sessions_update' });
  await expect.poll(() => subs(conn, KEY_A).length).toBe(before + 1);
  const sub = subs(conn, KEY_A)[before];
  expect(sub.after, 'a timed-out subscription restarts from an opening frame').toBeUndefined();
  conn.send({ type: 'subscribed', key: KEY_A });
  // Mark the bubble on screen, so the wait below sees the repaint replace it.
  await page.evaluate(() => document.querySelector('#events-scroll .event[data-uuid="a29"]')?.setAttribute('data-old', '1'));
  conn.send({ type: 'history', key: KEY_A, initial: true, events: events.concat(extra || []) });
  await page.waitForSelector('#events-scroll .event[data-uuid="a29"]:not([data-old])');
  await frames(page);
}

test.describe('a full repaint of the session on screen keeps the reader\'s position', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('a resubscribe after subscription_timeout does not pin a scrolled-up reader', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    await resubscribeFromScratch(page, conn, events);
    expect(await atBottom(page), 'the repaint must not pin the pane').toBe(false);
    expect(await offsetOf(page, anchor.uuid), 'the bubble the reader was on stays put').toBe(anchor.offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('events that arrived meanwhile do not shift a scrolled-up reader', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    const T = events[events.length - 1].time + 1000;
    await resubscribeFromScratch(page, conn, events, [
      { type: 'user', detail: 'later', summary: 'later', time: T, uuid: 'q30' },
      { type: 'text', detail: 'later answer '.repeat(80), time: T + 1, uuid: 'a30' },
    ]);
    await page.waitForSelector('#events-scroll .event[data-uuid="a30"]');
    expect(await offsetOf(page, anchor.uuid)).toBe(anchor.offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a reader at the bottom stays pinned through the repaint', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    expect(await atBottom(page)).toBe(true);
    const T = events[events.length - 1].time + 1000;
    await resubscribeFromScratch(page, conn, events, [
      { type: 'text', detail: 'later answer '.repeat(80), time: T, uuid: 'a30' },
    ]);
    expect(await atBottom(page), 'the newest bubble must be visible').toBe(true);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('the REST full fetch does not pin a scrolled-up reader', async ({ browser }) => {
    const { ctx, page, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    await page.evaluate((evs) => renderEvents(evs, true), events);
    await frames(page);
    expect(await atBottom(page)).toBe(false);
    expect(await offsetOf(page, anchor.uuid)).toBe(anchor.offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('switching away and back restores the reader to the bubble they were on', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await expect.poll(() => subs(conn, KEY_B).length).toBe(1);
    await page.click(`.session-card[data-key="${KEY_A}"]`);
    await expect.poll(() => subs(conn, KEY_A).length).toBe(2);
    conn.send({ type: 'subscribed', key: KEY_A });
    conn.send({ type: 'history', key: KEY_A, initial: true, events });
    await page.waitForSelector('#events-scroll .event[data-uuid="a29"]');
    await frames(page);
    expect(await offsetOf(page, anchor.uuid)).toBe(anchor.offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });
});
