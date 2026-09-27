// @ts-check
//
// State shared between dashboard modules must be the live value, not a copy
// taken at startup or a property nobody reads (#2550 L0):
//  - a message the user sends joins the message-nav count straight away;
//  - the Home panel lists the sessions the sidebar has loaded.
//
// Run: cd test/e2e && npx playwright test shared_state_live_refs.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultEvents } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

test('a sent message joins the message-nav count', async ({ browser }) => {
  // The optimistic bubble is drawn on the WS send path.
  const mock = await startMockServer({ ws: true });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  // Answer the subscription with the session's history, as the server does.
  await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === KEY)).toBe(true);
  conn.send({ type: 'history', key: KEY, initial: true, events: defaultEvents() });
  await expect(page.locator('#events-scroll .event.user')).not.toHaveCount(0);
  const before = await page.locator('#events-scroll .event.user').count();

  await page.fill('#msg-input', 'one more question');
  await page.click('#btn-send');
  await expect(page.locator('#events-scroll .event.user')).toHaveCount(before + 1);

  // The pill shows once there are two user messages, counting all of them.
  await expect(page.locator('#nav-pill')).toHaveClass(/visible/);
  await expect(page.locator('#nav-counter')).toHaveText(String(before + 1));

  await ctx.close();
  mock.server.close();
});

test('the Home panel lists the loaded sessions', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');

  const cards = await page.locator('.session-card').count();
  await expect(page.locator('#recent-sessions-panel .recent-row')).toHaveCount(Math.min(cards, 5));

  await ctx.close();
  mock.server.close();
});

test('the test surface writes the message-nav popover flag its owner reads', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');

  // A setter that assigns an imported binding throws in module (strict)
  // code; the flag's owner, msg_nav, is the only one that can write it.
  const result = await page.evaluate(async () => {
    const { nzTest, nzState } = await import('/static/nz_util.js');
    try {
      nzTest.navPopoverOpen = true;
      return { ok: true, test: nzTest.navPopoverOpen, state: nzState.navPopoverOpen };
    } catch (e) {
      return { ok: false, err: String(e) };
    } finally {
      nzTest.navPopoverOpen = false;
    }
  });
  expect(result).toEqual({ ok: true, test: true, state: true });
  expect(pageErrors).toEqual([]);

  await ctx.close();
  mock.server.close();
});
