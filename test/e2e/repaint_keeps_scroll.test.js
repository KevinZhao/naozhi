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

// open selects KEY_A over a connected socket and paints its opening frame
// (`events`, else page30).
async function open(browser, mock, events = page30(Date.now() + 60000)) {
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

const scrollTop = (page) => page.evaluate(() => /** @type {HTMLElement} */ (document.getElementById('events-scroll')).scrollTop);
const atBottom = (page) => page.evaluate(() => {
  const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
  return el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
});

// resubscribeFromScratch drives the reaped-process chain: subscription_timeout,
// then a sessions_update whose refetch resubscribes without a cursor. It
// resolves once the opening frame `events` (plus `extra`) is painted; every
// frame here holds a29.
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

  test('AI output that arrived meanwhile does not shift a scrolled-up reader', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    const T = events[events.length - 1].time + 1000;
    await resubscribeFromScratch(page, conn, events, [
      { type: 'text', detail: 'later answer '.repeat(80), time: T, uuid: 'a30' },
      { type: 'text', detail: 'more '.repeat(80), time: T + 1, uuid: 'a31' },
    ]);
    await page.waitForSelector('#events-scroll .event[data-uuid="a31"]');
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

  test('a teammate\'s message the repaint brings pins the pane', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    await scrollUp(page);
    const T = events[events.length - 1].time + 1000;
    await resubscribeFromScratch(page, conn, events, [
      { type: 'user', detail: 'from IM', summary: 'from IM', time: T, uuid: 'im-1' },
    ]);
    expect(await atBottom(page), 'a teammate\'s message must be visible').toBe(true);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a reader in history the repaint no longer holds lands at its top, not the bottom', async ({ browser }) => {
    // 60 pairs on screen (as after "load earlier"); the repaint holds the last 30.
    const T = Date.now() + 60000;
    const older = page30(T - 1000).map((e) => ({ ...e, uuid: 'old-' + e.uuid }));
    const { ctx, page, conn, errors, events } = await open(browser, mock, older.concat(page30(T)));
    await page.evaluate(() => { /** @type {HTMLElement} */ (document.getElementById('events-scroll')).scrollTop = 300; });
    await resubscribeFromScratch(page, conn, events.slice(older.length));
    expect(await scrollTop(page), 'the reader sits above everything repainted').toBe(0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('an anchor among same-uuid bubbles survives the page cutting that group at the top', async ({ browser }) => {
    // One entry rendered as three bubbles sharing a uuid, ahead of the pairs.
    const T = Date.now() + 60000;
    const group = [0, 1, 2].map((i) => ({ type: 'text', detail: ('part ' + i + ' ').repeat(200), time: T - 100 + i, uuid: 'grp' }));
    const { ctx, page, conn, errors, events } = await open(browser, mock, group.concat(page30(T)));
    // Park the viewport top inside the third member.
    const offset = await page.evaluate(() => {
      const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
      const third = el.querySelectorAll(':scope > .event[data-uuid="grp"]')[2];
      el.scrollTop += third.getBoundingClientRect().top - el.getBoundingClientRect().top + 20;
      return Math.round(third.getBoundingClientRect().top - el.getBoundingClientRect().top);
    });
    // The new page starts at the second member.
    await resubscribeFromScratch(page, conn, events.slice(1));
    const now = await page.evaluate(() => {
      const el = /** @type {HTMLElement} */ (document.getElementById('events-scroll'));
      const members = el.querySelectorAll(':scope > .event[data-uuid="grp"]');
      return { n: members.length, top: Math.round(members[members.length - 1].getBoundingClientRect().top - el.getBoundingClientRect().top) };
    });
    expect(now.n).toBe(2);
    expect(now.top, 'the same (last) member stays where the reader had it').toBe(offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });
});

// An agent drill-in owns the pane: the session's snapshot is taken on the way
// in and nothing the session's stream paints may land in the agent's pane.
test.describe('an agent drill-in keeps the session\'s pane and position apart', () => {
  let mock;
  const agentPage = page30(Date.now()).map((e) => ({ ...e, uuid: 'ag-' + e.uuid }));
  test.beforeAll(async () => { mock = await startMockServer({ ws: true, agentEvents: { t1: agentPage } }); });
  test.afterAll(() => mock.server.close());

  // drillIn opens agent t1 and parks the reader up in its pane.
  async function drillIn(page) {
    await page.evaluate(() => /** @type {any} */ (window).nz.views.agent.switchTo('t1'));
    await page.waitForSelector('#events-scroll .event[data-uuid="ag-a29"]');
    await page.evaluate(() => { /** @type {HTMLElement} */ (document.getElementById('events-scroll')).scrollTop = 300; });
  }

  test('a session left while drilled in comes back where its reader was (#3697)', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    const anchor = await scrollUp(page);
    await drillIn(page);
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await expect.poll(() => subs(conn, KEY_B).length).toBe(1);
    await page.click(`.session-card[data-key="${KEY_A}"]`);
    await expect.poll(() => subs(conn, KEY_A).length).toBe(2);
    conn.send({ type: 'subscribed', key: KEY_A });
    conn.send({ type: 'history', key: KEY_A, initial: true, events });
    await page.waitForSelector('#events-scroll .event[data-uuid="a29"]');
    await frames(page);
    expect(await scrollTop(page), 'not the top an agent anchor missing from the page sends it to').toBeGreaterThan(0);
    expect(await offsetOf(page, anchor.uuid)).toBe(anchor.offset);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a resubscribe while drilled in does not paint the session over the agent (#3698)', async ({ browser }) => {
    const { ctx, page, conn, errors, events } = await open(browser, mock);
    await drillIn(page);
    const before = subs(conn, KEY_A).length;
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'ready', reason: 'subscription_timeout' });
    await page.waitForFunction(() => sessionStream.subscribedKey === null);
    conn.send({ type: 'sessions_update' });
    await expect.poll(() => subs(conn, KEY_A).length).toBe(before + 1);
    conn.send({ type: 'subscribed', key: KEY_A });
    conn.send({ type: 'history', key: KEY_A, initial: true, events });
    await page.waitForFunction(() => !sessionStream._initialSubscribe);
    const T = events[events.length - 1].time + 1000;
    conn.send({ type: 'history', key: KEY_A, events: [{ type: 'text', detail: 'backfill', time: T, uuid: 'a30' }] });
    conn.send({ type: 'agent_event', key: KEY_A, task_id: 't1', event: { type: 'text', detail: 'agent live', time: T + 1, uuid: 'ag-live' } });
    await page.waitForSelector('#events-scroll .event[data-uuid="ag-live"]');
    await frames(page);
    expect(await page.locator('#events-scroll .event[data-uuid^="a"]:not([data-uuid^="ag-"])').count(), 'no session bubble in the agent pane').toBe(0);
    await expect(page.locator('#events-scroll > .event[data-uuid="ag-a0"]')).toHaveCount(1);
    await expect(page.locator('#agent-breadcrumb')).toBeVisible();
    expect(errors).toEqual([]);
    await ctx.close();
  });
});
