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

    // lastVersion is shared with the fallback pollers (fetchSessions writes the
    // server's version back on every response), so a sentinel value races them
    // (#3251). Observe scanDiscovered's own side effects instead: its call to
    // the shell.debouncedFetchSessions slot and its lastVersion=0 write.
    const result = await page.evaluate(async () => {
      const { shell } = await import('/static/shell.js');
      const { sessionList } = await import('/static/state.js');
      const { scanDiscovered } = await import('/static/discovery.js');
      const counts = { fetches: 0, zeroes: 0 };
      const origFetch = shell.debouncedFetchSessions;
      let version = sessionList.lastVersion;
      shell.debouncedFetchSessions = () => { counts.fetches++; return origFetch(); };
      Object.defineProperty(sessionList, 'lastVersion', {
        configurable: true,
        enumerable: true,
        get: () => version,
        set: (v) => { if (v === 0) counts.zeroes++; version = v; },
      });
      const scan = async () => {
        const before = { ...counts };
        await scanDiscovered();
        return { fetches: counts.fetches - before.fetches, zeroes: counts.zeroes - before.zeroes };
      };
      try {
        // Prime: the hash is recorded by this scan or the boot scan.
        await scan();
        const unchanged = await scan();
        // Positive control: a forgotten hash must force exactly one re-render.
        sessionList.lastDiscoveredJSON = '';
        const changed = await scan();
        return { unchanged, changed };
      } finally {
        shell.debouncedFetchSessions = origFetch;
        delete sessionList.lastVersion;
        sessionList.lastVersion = version;
      }
    });

    expect(result.unchanged).toEqual({ fetches: 0, zeroes: 0 });
    expect(result.changed).toEqual({ fetches: 1, zeroes: 1 });
    await ctx.close();
  });

  test('WS ping pauses when tab hidden and the cleanup path clears the timer', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    const result = await page.evaluate(() => {
      const w = window.wsm || null;
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
