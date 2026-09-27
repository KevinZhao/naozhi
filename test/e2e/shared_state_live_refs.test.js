// @ts-check
//
// State shared between dashboard modules must be the live value, not a copy
// taken at startup or a property nobody reads:
//  - a message the user sends joins the message-nav count straight away;
//  - the Home panel lists the sessions the sidebar has loaded;
//  - voice input mode survives the composer being rebuilt;
//  - a new turn clears the running banner's tool tally;
//  - with both right-hand drawers open, only the last close restores the
//    sidebar.
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
    const { nzTest } = await import('/static/nz_util.js');
    try {
      nzTest.navPopoverOpen = true;
      return { ok: true, value: nzTest.navPopoverOpen };
    } catch (e) {
      return { ok: false, err: String(e) };
    } finally {
      nzTest.navPopoverOpen = false;
    }
  });
  expect(result).toEqual({ ok: true, value: true });
  expect(pageErrors).toEqual([]);

  await ctx.close();
  mock.server.close();
});

test('voice input mode survives the composer being rebuilt', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.click('#btn-mic');
  await expect(page.locator('#input-area')).toHaveClass(/voice-mode/);

  // Switching sessions renders a fresh composer; it must read the mode voice.js
  // toggled, not a default.
  const other = page.locator(`.session-card:not([data-key="${KEY}"])`).first();
  const otherKey = await other.getAttribute('data-key');
  await other.click();
  await expect(page.locator('.main-header')).toContainText(/./);
  await expect.poll(() => page.evaluate(() => {
    const card = document.querySelector('.session-card.active');
    return card && card.getAttribute('data-key');
  })).toBe(otherKey);
  await expect(page.locator('#input-area')).toHaveClass(/voice-mode/);
  await expect(page.locator('#btn-hold-talk')).toBeVisible();

  await ctx.close();
  mock.server.close();
});

test('a new turn clears the running banner tool tally', async ({ browser }) => {
  const mock = await startMockServer({ ws: true });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === KEY)).toBe(true);
  conn.send({ type: 'history', key: KEY, initial: true, events: defaultEvents() });
  conn.send({ type: 'session_state', key: KEY, state: 'running' });

  const now = Date.now();
  const push = (ev) => conn.send({ type: 'event', key: KEY, event: ev });
  push({ type: 'tool_use', tool: 'Read', summary: 'Read', detail: 'a.go', time: now + 1 });
  const stats = page.locator('#rb-stats');
  await expect(stats).toHaveText('Read \u00d71');

  // A user message typed on another surface starts the next turn.
  push({ type: 'user', summary: 'next', detail: 'next', time: now + 2, uuid: 'u-next' });
  push({ type: 'tool_use', tool: 'Bash', summary: 'Bash', detail: 'ls', time: now + 3 });
  await expect(stats).toHaveText('Bash \u00d71');

  await ctx.close();
  mock.server.close();
});

test('with both drawers open, only the last close restores the sidebar', async ({ browser }) => {
  const now = Date.now();
  // The 追问 button renders on text events over 500 characters; the file ref
  // in the same reply offers the preview.
  const reply = 'wrote `docs/page.html`. ' + '足够长的回复内容，凑到追问按钮的长度闸以上。'.repeat(30);
  const mock = await startMockServer({
    eventsByKey: {
      [KEY]: [
        { time: now - 2000, type: 'user', summary: 'make it', detail: 'make it' },
        { time: now - 1000, type: 'text', summary: reply, detail: reply, uuid: 'ev-txt-long' },
      ],
    },
    projectFiles: {
      myproject: { exists: { 'docs/page.html': { exists: true, size: 10, mime: 'text/html' } } },
    },
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  const body = page.locator('body');
  await expect(body).not.toHaveClass(/sidebar-collapsed/);

  const askBtn = page.locator('.event-ask-btn').first();
  await askBtn.hover({ force: true });
  await askBtn.click({ force: true });
  await expect(page.locator('#aside-drawer')).toBeVisible();
  await expect(body).toHaveClass(/sidebar-collapsed/);

  const previewBtn = page.locator('.fr-slot.fr-verified[data-path="docs/page.html"] .fr-btn-preview');
  await expect(previewBtn).toHaveCount(1, { timeout: 8000 });
  await previewBtn.click();
  await expect(page.locator('#fv-drawer')).toHaveClass(/fv-open/);

  // 追问 is still docked, so closing the preview (on top) keeps the sidebar
  // collapsed.
  await page.click('#fv-btn-close');
  await expect(page.locator('#fv-drawer')).not.toHaveClass(/fv-open/);
  await expect(body).toHaveClass(/sidebar-collapsed/);

  await page.click('#ad-close');
  await expect(body).not.toHaveClass(/sidebar-collapsed/);

  await ctx.close();
  mock.server.close();
});
