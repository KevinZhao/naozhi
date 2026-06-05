// @ts-check
// Regression for #1768: the WS real-time push path (wsm.onEvent) and the
// incremental WS history path (wsm.onHistory) must bound the live DOM via
// trimEventsScroll, exactly like the HTTP-poll fallback (appendEvents) already
// did. Before the fix, MAX_LIVE_DOM_EVENTS (600) only capped the poll path, so
// a long streaming session over WS grew #events-scroll without limit and could
// OOM the tab (#398 was effectively a no-op while WS was live).
//
// The mock server rejects /ws to force HTTP fallback, so we can't exercise a
// real socket here. Instead we drive the exported global `wsm.onEvent`
// directly in-page after selecting a session — that is the exact function the
// live socket invokes per frame, so it is a faithful regression of the bug.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.describe('#1768 WS event append bounds the live DOM', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('wsm.onEvent caps #events-scroll at MAX_LIVE_DOM_EVENTS', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
    await page.waitForSelector('#events-scroll');

    // Push 1000 streaming "text" events through the exact WS callback the live
    // socket uses. Each non-internal event appends one .event bubble.
    const result = await page.evaluate(async (key) => {
      // selectedKey/selectedNode and wsm are top-level lexical globals in
      // dashboard.js; reach them via the same scope eval the page runs in.
      // eslint-disable-next-line no-eval
      const sk = eval('typeof selectedKey !== "undefined" ? selectedKey : null');
      const sn = eval('typeof selectedNode !== "undefined" ? selectedNode : "local"');
      const w = eval('typeof wsm !== "undefined" ? wsm : null');
      const cap = eval('typeof MAX_LIVE_DOM_EVENTS !== "undefined" ? MAX_LIVE_DOM_EVENTS : null');
      if (!w || !sk || cap == null) return { err: 'globals missing', sk, hasW: !!w, cap };
      const base = Date.now();
      for (let i = 0; i < 1000; i++) {
        w.onEvent({
          key: sk,
          node: sn,
          event: { type: 'text', detail: 'streamed chunk ' + i, time: base + i },
        });
      }
      const el = document.getElementById('events-scroll');
      const bubbles = el.querySelectorAll(':scope > .event').length;
      return { cap, bubbles };
    }, SESSION_KEY);

    expect(result.err).toBeUndefined();
    // DOM must be bounded at the cap (not 1000+). Allow exactly the cap.
    expect(result.bubbles).toBeLessThanOrEqual(result.cap);
    // Sanity: we actually filled past the cap, so trimming really happened.
    expect(result.bubbles).toBeGreaterThan(result.cap - 50);

    await ctx.close();
  });
});
