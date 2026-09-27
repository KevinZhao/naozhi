// @ts-check
// dismissSession() removes the card from the DOM optimistically and, when
// DELETE /api/sessions fails, re-fetches the list so the card comes back:
// renderSidebar reconciles against the DOM as it stands, so a card removed in
// place is re-inserted by the next render that still lists it.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.describe('dismiss failure restores the sidebar card', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ deleteStatus: 500 }); });
  test.afterAll(() => mock.server.close());

  test('card reappears after DELETE /api/sessions returns 500', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${SESSION_KEY}"]`);

    // Drive the same entry point the × button uses. dismissSession is a
    // top-level function declaration so it is reachable on window.
    await page.evaluate((key) => {
      // @ts-ignore
      window.dismissSession(key, 'local');
    }, SESSION_KEY);

    // Optimistic removal is synchronous.
    await expect(page.locator(`.session-card[data-key="${SESSION_KEY}"]`)).toHaveCount(0);

    // The failed DELETE's .finally() re-fetches /api/sessions (debounced);
    // the server still lists the session, so the card must be re-mounted.
    await expect(page.locator(`.session-card[data-key="${SESSION_KEY}"]`)).toHaveCount(1, { timeout: 10000 });

    await ctx.close();
  });
});
