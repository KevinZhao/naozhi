// @ts-check
// #398: every incremental append path must bound the DOM it grows.
//
// Historical loads are paginated (INITIAL_HISTORY_LIMIT + "load earlier"), but
// the live paths only ever appended, so a long-running session grew
// #events-scroll — and a long cron run grew #cron-live-events — until the tab
// OOMed. Three append paths carry the budget:
//
//   - wsm.onEvent / wsm.onHistory (the WS socket)  → ws_dom_trim.test.js
//   - appendEvents (the HTTP-poll fallback)        → here
//   - the cron:live-event bus subscription         → here
//
// The two here replaced internal/server/static_event_dom_cap_test.go, which
// grepped dashboard.js for `const MAX_LIVE_DOM_EVENTS`, `function
// trimEventsScroll(`, `trimEventsScroll(el)` and cron_view.js for `bubbles >
// CRON_LIVE_MAX_EVENTS` (#2547). Those four substrings say the cap is declared
// and mentioned; they do not say the DOM is actually bounded — a trim that
// evicts the wrong nodes, stops one short, or runs before the append satisfies
// every one of them. Counting the bubbles that survive does say it.
//
// 跑法：cd test/e2e && npx playwright test live_dom_cap.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.describe('#398 live append paths bound the DOM', () => {
  test('appendEvents (poll fallback) caps #events-scroll at MAX_LIVE_DOM_EVENTS', async ({ browser }) => {
    const mock = await startMockServer();
    const ctx = await browser.newContext({ ...desktop });
    try {
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
      await page.waitForSelector('#events-scroll');

      // appendEvents is the function the poll response handler feeds. Push well
      // past the cap in one batch, then again in small batches: a trim that only
      // runs on the first call, or only outside the loop, shows up as an
      // unbounded second half.
      const result = await page.evaluate(() => {
        const w = /** @type {any} */ (window);
        // eslint-disable-next-line no-eval
        const cap = eval('typeof MAX_LIVE_DOM_EVENTS !== "undefined" ? MAX_LIVE_DOM_EVENTS : null');
        if (cap == null) return { err: 'MAX_LIVE_DOM_EVENTS missing' };
        const el = document.getElementById('events-scroll');
        const base = Date.now();
        /** @param {number} n @param {number} from */
        const batch = (n, from) => Array.from({ length: n }, (_, i) => ({
          type: 'text', detail: 'polled chunk ' + (from + i), time: base + from + i,
          uuid: 'poll-' + (from + i),
        }));
        w.appendEvents(batch(cap + 200, 0));
        const afterBig = el.querySelectorAll(':scope > .event').length;
        for (let i = 0; i < 50; i++) w.appendEvents(batch(4, cap + 200 + i * 4));
        const afterDrip = el.querySelectorAll(':scope > .event').length;
        return { cap, afterBig, afterDrip };
      });

      expect(result.err).toBeUndefined();
      expect(result.afterBig, 'one oversized batch must be trimmed to the cap')
        .toBeLessThanOrEqual(result.cap);
      expect(result.afterDrip, 'a long drip of small batches must stay at the cap')
        .toBeLessThanOrEqual(result.cap);
      // Sanity: the cap was actually reached, so the bound above is not
      // vacuously true on an empty container.
      expect(result.afterDrip).toBeGreaterThan(result.cap - 50);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('cron:live-event caps #cron-live-events at CRON_LIVE_MAX_EVENTS', async ({ browser }) => {
    // The container only exists while a run is in flight, so the fixture gives
    // cron-001 a current_run.
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
    const mock = await startMockServer({ cronJobs: jobs });
    const ctx = await browser.newContext({ ...desktop });
    try {
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#abnav-cron');
      await page.waitForSelector('.cj-row');
      await page.click('.cj-row[data-cron-id="cron-001"]');
      await page.waitForSelector('#cron-live-events');

      // Dispatch on nz.bus — the same channel dashboard.js's WS core uses to
      // hand each frame to the cron view. CRON_LIVE_MAX_EVENTS lives in
      // utilities.js module scope and is not on the page's instrumentation
      // surface, so the bound is established without naming it: push a large
      // round, count, push another equally large round, count again. A
      // container that trims lands on the same number twice; one that only
      // appends doubles.
      const ROUND = 350;
      const result = await page.evaluate((n) => {
        const w = /** @type {any} */ (window);
        if (!w.nz || !w.nz.bus) return { err: 'nz.bus missing' };
        const el = document.getElementById('cron-live-events');
        const base = Date.now();
        /** @param {number} from */
        const push = (from) => {
          for (let i = from; i < from + n; i++) {
            w.nz.bus.dispatchEvent(new CustomEvent('cron:live-event', {
              detail: { type: 'text', detail: 'cron chunk ' + i, time: base + i, uuid: 'cl-' + i },
            }));
          }
          return el.querySelectorAll(':scope > .event').length;
        };
        return { first: push(0), second: push(n) };
      }, ROUND);

      expect(result.err).toBeUndefined();
      expect(result.first, `${ROUND} events must be trimmed to a bound below that`)
        .toBeLessThan(ROUND);
      expect(result.second, 'a second round must not grow the container past the bound')
        .toBe(result.first);
      // Sanity: bubbles really were appended, so the bound is not vacuously
      // satisfied by a container the events never reached.
      expect(result.first).toBeGreaterThan(0);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});
