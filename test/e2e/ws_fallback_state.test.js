// @ts-check
// Regressions for #2431 (WS/polling state handling in dashboard.js):
//  1. Under WS-fallback polling the stats.version short-circuit must NOT skip
//     the sidebar repaint — process state flips never bump storeGen, so a
//     frozen dot/last_response was the symptom.
//  2. Hidden-tab gate: wsm.setState must not re-arm pollers while the tab is
//     hidden, and startPollers must not arm the 5 s sessions poll over a live
//     WS.
//  3. The optimistic 'running' copy must land in the payload renderSidebar and
//     _lastSidebarData see, so a cached re-render can't paint the card idle.
// The mock server rejects /ws, so the page runs in the fallback-polling state
// (wsm.state === 'disconnected') for real; the WS-connected cases fake
// wsm.state via page.evaluate — the same globals the runtime uses.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

function cardDot(page, key) {
  return page.locator(`.session-card[data-key="${key}"] .sc-dot`);
}

test.describe('#2431 WS fallback state', () => {
  test('fallback poll repaints a state flip that does not bump stats.version', async ({ browser }) => {
    const mock = await startMockServer();
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      // Sanity: the mock rejects /ws, so we are genuinely in fallback mode.
      // eslint-disable-next-line no-eval
      expect(await page.evaluate(() => eval('wsm.state'))).not.toBe('connected');
      await expect(cardDot(page, KEY)).toHaveClass(/dot-ready/);

      // Settle the boot churn: the first scanDiscovered zeroes lastVersion once
      // (hash primed from ''), and the WS-reject path fires a debounced fetch.
      // Prime both so the version gate is genuinely armed (lastVersion === 1)
      // before the flip — otherwise the old code passes by accident.
      const primed = await page.evaluate(async () => {
        /* eslint-disable no-eval */
        await eval('scanDiscovered')();
        await eval('fetchSessions')();
        await eval('fetchSessions')();
        return eval('lastVersion');
        /* eslint-enable no-eval */
      });
      expect(primed).toBe(1);

      // Server-side turn start: state flips, stats.version stays put (exactly
      // what the real server does — storeGen only moves on session mutations).
      mock.setSessionStateWithoutVersionBump(KEY, 'running');

      // Drive the very function the 5 s fallback interval calls.
      // eslint-disable-next-line no-eval
      await page.evaluate(async () => { await eval('fetchSessions')(); });
      await expect(cardDot(page, KEY)).toHaveClass(/dot-running/);

      // ...and the interval itself keeps it fresh: flip back, wait for the poll.
      mock.setSessionStateWithoutVersionBump(KEY, 'ready');
      await expect(cardDot(page, KEY)).toHaveClass(/dot-ready/, { timeout: 12000 });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('startPollers does not arm the sessions poll over a live WS; setState respects hidden tab', async ({ browser }) => {
    const mock = await startMockServer();
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');

      const result = await page.evaluate(() => {
        // Make document.hidden scriptable so we can drive the visibility gate.
        let hidden = false;
        Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
        const flip = (h) => { hidden = h; document.dispatchEvent(new Event('visibilitychange')); };
        /* eslint-disable no-eval */
        const getTimer = () => eval('sessionPollTimer');
        const getDiscTimer = () => eval('discoveredPollTimer');

        // Case A: tab returns to foreground while WS is (faked) connected.
        eval('wsm.state = WS_STATES.CONNECTED');
        flip(true);  // stopPollers → clears everything
        const clearedWhileHidden = getTimer() == null;
        flip(false); // startPollers
        const armedOverLiveWs = getTimer() != null;

        // Case B: WS drops while the tab is hidden — nothing may be armed.
        flip(true);
        eval('wsm.setState(WS_STATES.DISCONNECTED)');
        const armedByHiddenDisconnect = getTimer() != null || getDiscTimer() != null;

        // Case C: WS comes back while hidden — no discovered scan either.
        eval('wsm.setState(WS_STATES.CONNECTED)');
        const discArmedByHiddenConnect = getDiscTimer() != null;

        // Case D: returning to the foreground with WS down re-arms fallback.
        eval('wsm.state = WS_STATES.DISCONNECTED');
        flip(false);
        const rearmedOnVisibleWithWsDown = getTimer() != null;
        /* eslint-enable no-eval */
        return { clearedWhileHidden, armedOverLiveWs, armedByHiddenDisconnect, discArmedByHiddenConnect, rearmedOnVisibleWithWsDown };
      });

      expect(result.clearedWhileHidden).toBe(true);
      expect(result.armedOverLiveWs).toBe(false);
      expect(result.armedByHiddenDisconnect).toBe(false);
      expect(result.discArmedByHiddenConnect).toBe(false);
      expect(result.rearmedOnVisibleWithWsDown).toBe(true);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('optimistic running survives a fetchSessions repaint and the cached sidebar payload', async ({ browser }) => {
    const mock = await startMockServer();
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await expect(cardDot(page, KEY)).toHaveClass(/dot-ready/);

      const result = await page.evaluate(async (key) => {
        /* eslint-disable no-eval */
        eval('markSessionOptimisticRunning')(key, 'local');
        // REST still says 'ready' (mock never changed) — the poll must keep the
        // optimistic flip in BOTH sessionsData and the payload it renders from.
        await eval('fetchSessions')();
        const cached = eval('_lastSidebarData');
        const inPayload = (cached.sessions || []).find(s => s.key === key);
        // Cached re-render path used by toggleProjectCollapsed / sidebar search.
        eval('renderSidebar')(cached);
        /* eslint-enable no-eval */
        return { payloadState: inPayload && inPayload.state };
      }, KEY);

      expect(result.payloadState).toBe('running');
      await expect(cardDot(page, KEY)).toHaveClass(/dot-running/);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});
