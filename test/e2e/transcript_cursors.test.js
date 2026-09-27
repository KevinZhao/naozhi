// @ts-check
//
// The transcript's render cursor (the newest event time on screen) and its
// dedup rules, driven through the frames the server actually sends:
//  - a user event already on screen is a replay even when its timestamp moved,
//    and skipping it still advances the cursor;
//  - the opening (initial) frame is the one the server flags, not whichever
//    history frame lands first after a subscribe;
//  - an empty opening frame clears the cursor along with the pane;
//  - the cron live stream admits same-ms siblings and drops same-ms replays.
//
// Run: cd test/e2e && npx playwright test transcript_cursors.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

/**
 * openSubscribed opens the session over a connected socket and returns the
 * connection. Clicking before the socket authenticates takes the HTTP page
 * path instead, whose late render would reset the cursor under the frames.
 * @param {import('@playwright/test').Page} page
 * @param {any} mock
 */
async function openSubscribed(page, mock) {
  await page.waitForSelector('.session-card');
  // @ts-ignore — wsm / WS_STATES are mirrored onto window by the e2e shim.
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  await page.click(`.session-card[data-key="${KEY}"]`);
  return subscribed(page, mock, KEY);
}

/**
 * @param {import('@playwright/test').Page} page
 * @param {any} mock
 * @param {string} key
 */
async function subscribed(page, mock, key) {
  await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.some((/** @type {any} */ m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  return conn;
}

/** @param {import('@playwright/test').Page} page */
function bubbleTexts(page) {
  return page.locator('#events-scroll .event').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));
}

const T = Date.now() + 60_000;
/** @param {string} id @param {number} time @param {string} [type] */
const ev = (id, time, type = 'text') => ({ type, summary: id, detail: id, time, uuid: id });

test('a user event already on screen is a replay even when its timestamp moved', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#events-scroll .event');

  const result = await page.evaluate((t) => {
    const w = /** @type {any} */ (window);
    const el = document.getElementById('events-scroll');
    const count = (/** @type {string} */ txt) => [...el.querySelectorAll('.event')].filter((n) => (n.textContent || '').includes(txt)).length;
    w.appendEvents([{ type: 'user', detail: 'moved-stamp question', time: t, uuid: 'usr-moved' }]);
    // The same user event, re-stamped later (a later page echoes it).
    w.appendEvents([{ type: 'user', detail: 'moved-stamp question', time: t + 50, uuid: 'usr-moved' }]);
    const afterReplay = count('moved-stamp question');
    // The replay moved the cursor to t+50, so an event older than it is a
    // stale page overlap and stays out.
    w.appendEvents([{ type: 'text', detail: 'stale overlap', time: t + 20, uuid: 'txt-stale' }]);
    return { afterReplay, stale: count('stale overlap') };
  }, T);

  expect(result.afterReplay).toBe(1);
  expect(result.stale).toBe(0);
  await ctx.close();
  mock.server.close();
});

test('a backfill frame that lands before the flagged opening frame does not become the page', async ({ browser }) => {
  const mock = await startMockServer({ ws: true });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  const conn = await openSubscribed(page, mock);

  // A superseded subscription's backfill: newest event only, no initial flag.
  conn.send({ type: 'history', key: KEY, events: [ev('evt-c', T + 30)] });
  // The real opening frame carries the whole page.
  conn.send({ type: 'history', key: KEY, initial: true, events: [ev('evt-a', T + 10), ev('evt-b', T + 20), ev('evt-c', T + 30)] });

  await expect.poll(() => bubbleTexts(page)).toEqual(['evt-a', 'evt-b', 'evt-c']);
  await ctx.close();
  mock.server.close();
});

test('an empty opening frame clears the cursor with the pane', async ({ browser }) => {
  const mock = await startMockServer({ ws: true });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  const conn = await openSubscribed(page, mock);

  // A stale backfill pushes the cursor to the tip …
  conn.send({ type: 'history', key: KEY, events: [ev('evt-a', T + 10), ev('evt-b', T + 20)] });
  // … then the opening frame for a just-started run is empty …
  conn.send({ type: 'history', key: KEY, initial: true, events: [] });
  await expect(page.locator('#events-scroll .event')).toHaveCount(0);
  // … and the new push loop sends the same batch again: all of it renders.
  conn.send({ type: 'history', key: KEY, events: [ev('evt-a', T + 10), ev('evt-b', T + 20)] });

  await expect.poll(() => bubbleTexts(page)).toEqual(['evt-a', 'evt-b']);
  await ctx.close();
  mock.server.close();
});

test('the cron live stream admits same-ms siblings and drops same-ms replays', async ({ browser }) => {
  const jobs = [{
    id: 'cron-001',
    schedule: '@every 1h',
    prompt: 'check server status',
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: Date.now() - 86400000,
    next_run: Date.now() + 3600000,
    current_run: { run_id: 'run-live-1', started_at: Date.now() - 5000, state: 'running' },
    stats: { total: 1, succeeded: 0, failed: 0, skipped: 0 },
  }];
  const mock = await startMockServer({ ws: true, cronJobs: jobs });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  const conn = await subscribed(page, mock, 'cron:cron-001');
  const live = () => page.locator('#cron-live-events .event').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));

  conn.send({ type: 'history', key: 'cron:cron-001', events: [ev('cl-1', T), ev('cl-2', T)] });
  await expect.poll(live).toEqual(['cl-1', 'cl-2']);
  // A second history frame: cl-2 replays, cl-3 is a new same-ms sibling.
  conn.send({ type: 'history', key: 'cron:cron-001', events: [ev('cl-2', T), ev('cl-3', T)] });
  await expect.poll(live).toEqual(['cl-1', 'cl-2', 'cl-3']);
  // The event stream follows the same rule.
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('cl-4', T) });
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('cl-4', T) });
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('cl-5', T + 1) });
  await expect.poll(live).toEqual(['cl-1', 'cl-2', 'cl-3', 'cl-4', 'cl-5']);

  await ctx.close();
  mock.server.close();
});
