// @ts-check
// Regressions for #1770 (polling / WS keep-alive efficiency on mobile):
//  1. scanDiscovered must NOT force a full sidebar re-render (lastVersion=0)
//     when the discovered set is unchanged.
//  2. The WS keep-alive ping must pause while the tab is hidden (stopPollers)
//     and re-arm on resume only when the socket is live.
// The mock server rejects /ws (HTTP fallback), so we drive the relevant
// globals directly via page.evaluate — the same functions the runtime uses.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

test.describe('#1770 polling / keep-alive', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('scanDiscovered skips forced re-render when discovered set is unchanged', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    const result = await page.evaluate(async () => {
      // eslint-disable-next-line no-eval
      const run = () => eval('scanDiscovered()');
      // Prime: first scan records the hash and (because the hash changed from
      // the initial '') sets lastVersion=0 once.
      await run();
      // Now set a sentinel lastVersion and scan again with the SAME data
      // (mock returns [] every time). The unchanged-hash guard must leave
      // lastVersion untouched.
      // eslint-disable-next-line no-eval
      eval('lastVersion = 12345');
      await run();
      // eslint-disable-next-line no-eval
      const after = eval('lastVersion');
      return { after };
    });

    // Unchanged discovered set → lastVersion preserved (NOT reset to 0).
    expect(result.after).toBe(12345);
    await ctx.close();
  });

  test('WS ping pauses when tab hidden and the cleanup path clears the timer', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    const result = await page.evaluate(() => {
      // eslint-disable-next-line no-eval
      const w = eval('typeof wsm !== "undefined" ? wsm : null');
      if (!w) return { err: 'wsm missing' };
      // Simulate a live ping timer, then run the cleanup() that stopPollers
      // calls on visibilitychange→hidden.
      w.startPing();
      const armed = w.pingTimer != null;
      w.cleanup();
      const clearedAfterHidden = w.pingTimer == null;
      return { armed, clearedAfterHidden };
    });

    expect(result.err).toBeUndefined();
    expect(result.armed).toBe(true);
    expect(result.clearedAfterHidden).toBe(true);
    await ctx.close();
  });
});
