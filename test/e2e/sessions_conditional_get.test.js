// @ts-check
// /api/sessions conditional GET (#3014): the dashboard sends the last body's
// ETag as If-None-Match and treats a 304 as "nothing new".
//  - over a live socket, a sessions_update refetch revalidates, gets a 304 and
//    leaves the sidebar and the header as they were;
//  - a session_state frame (which zeroes lastVersion to force a repaint) and a
//    selection change (the header chips were painted for another session)
//    each make the next fetch unconditional;
//  - under WS-fallback polling a 304 is "unchanged", not a failed poll, and a
//    changed body still repaints.
// Both projects run it: a page-set If-None-Match must reach script as a 304
// in WebKit as well as Chromium.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

const cardDot = (page, key) => page.locator(`.session-card[data-key="${key}"] .sc-dot`);

// pageErrors collects uncaught errors and the console errors a mishandled 304
// would log (fetchSessions' catch, or the browser reporting the response).
// \b304\b, so a mock port such as 33049 in a WS reconnect error is not one.
function pageErrors(page) {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error' && /fetchSessions|\b304\b/.test(m.text())) errors.push(m.text()); });
  return errors;
}

// validatorsSince returns the If-None-Match of each GET /api/sessions after
// the n-th ('' for an unconditional one).
const validatorsSince = (mock, n) => mock.sessionsValidators.slice(n);

test.describe('over a live socket', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true, sessionsETag: true }); });
  test.afterAll(() => mock.server.close());

  // open selects KEY_A over a connected socket and settles the connect-time
  // refetches, so the next fetch has a validator to send.
  async function open(browser) {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const errors = pageErrors(page);
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.click(`.session-card[data-key="${KEY_A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === KEY_A)).toBe(true);
    conn.send({ type: 'subscribed', key: KEY_A });
    await page.evaluate(() => debouncedFetchSessions());
    await page.evaluate(() => fetchSessions());
    return { ctx, page, conn, errors };
  }

  test('a sessions_update refetch revalidates and a 304 leaves the page as it was', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser);
    const header = await page.locator('#main .main-header h2').textContent();
    const cards = await page.locator('.session-card').count();
    const n = mock.sessionsValidators.length;
    const notModified = mock.sessionsNotModified;

    conn.send({ type: 'sessions_update' });
    await expect.poll(() => mock.sessionsNotModified).toBe(notModified + 1);
    const sent = validatorsSince(mock, n);
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatch(/^W\/"b[0-9a-f]{32}"$/);

    await expect(page.locator('.session-card')).toHaveCount(cards);
    await expect(page.locator(`.session-card[data-key="${KEY_A}"]`)).toHaveClass(/active/);
    await expect(page.locator('#main .main-header h2')).toHaveText(header || '');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a session_state frame makes the next fetch unconditional', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser);
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'ready' });
    await page.waitForFunction(() => lastVersion === 0);
    const n = mock.sessionsValidators.length;
    expect(await page.evaluate(() => fetchSessions())).toBe(true);
    await page.evaluate(() => fetchSessions());
    const sent = validatorsSince(mock, n);
    expect(sent[0], 'the forced repaint fetches a body').toBe('');
    expect(sent[1], 'the one after revalidates again').not.toBe('');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('selecting another session makes the next fetch unconditional', async ({ browser }) => {
    const { ctx, page, errors } = await open(browser);
    const n = mock.sessionsValidators.length;
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await page.evaluate(() => fetchSessions());
    expect(validatorsSince(mock, n)[0], 'the chips were painted for the other session').toBe('');
    expect(errors).toEqual([]);
    await ctx.close();
  });
});

test.describe('under WS-fallback polling', () => {
  // The mock rejects /ws, so the 5 s poll is armed for real.
  async function openFallback(browser, mock) {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const errors = pageErrors(page);
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    // Settle the boot churn (the first scanDiscovered zeroes lastVersion).
    await page.evaluate(async () => {
      await scanDiscovered();
      await fetchSessions();
      await fetchSessions();
    });
    return { ctx, page, errors };
  }

  test('a 304 is an unchanged poll, not a failed one', async ({ browser }) => {
    const mock = await startMockServer({ sessionsETag: true });
    const { ctx, page, errors } = await openFallback(browser, mock);
    try {
      expect(await page.evaluate(() => wsm.state)).not.toBe('connected');
      expect(await page.evaluate(() => sessionPollTimer)).toBeTruthy();
      const n = mock.sessionsValidators.length;
      const notModified = mock.sessionsNotModified;
      expect(await page.evaluate(async () => (await fetchSessions()) === undefined)).toBe(true);
      // The 5 s interval revalidates the same way: of the next polls at most
      // one is a boot-time debounced fetch.
      await expect.poll(() => mock.sessionsValidators.length, { timeout: 12000 }).toBeGreaterThanOrEqual(n + 3);
      const sent = validatorsSince(mock, n);
      expect(sent.every((v) => v !== ''), 'every poll revalidated').toBe(true);
      expect(mock.sessionsNotModified - notModified, 'every poll got a 304').toBe(sent.length);
      await expect(page.locator('.modal-overlay')).toHaveCount(0);
      await expect(cardDot(page, KEY_A)).toHaveClass(/dot-ready/);
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('a changed body at the same version still repaints', async ({ browser }) => {
    const mock = await startMockServer({ sessionsETag: true });
    const { ctx, page, errors } = await openFallback(browser, mock);
    try {
      const n = mock.sessionsValidators.length;
      mock.setSessionStateWithoutVersionBump(KEY_A, 'running');
      expect(await page.evaluate(() => fetchSessions()), 'the poll applied a body').toBe(true);
      expect(validatorsSince(mock, n)[0], 'the stale validator was sent').not.toBe('');
      await expect(cardDot(page, KEY_A)).toHaveClass(/dot-running/);
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});
