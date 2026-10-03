// @ts-check
// The two global shortcuts, Esc and Alt+N.
//
// 1. Esc (dashboard.js) closes the voice overlay, the history
//    popover and the message-nav list with four independent ifs, not an
//    else-if chain, and agent_view's own Esc listener leaves the drill-in on
//    the same key press. One Esc with all four open must close all four: an
//    else-if, or a missing branch, leaves one behind. Esc while the composer
//    has focus belongs to the composer (its double-Esc interrupt), so the
//    global listener must leave everything open then.
// 2. Alt+N (auth_modal.js, next to createNewSession) opens the
//    new-session modal, but not while the user is typing in the composer.
//
// 跑法：cd test/e2e && npx playwright test esc_close_all.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TASK_ID = 'taskesc1';
const BASE = 1750000000000;

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

test.describe('global shortcuts', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>> | undefined} */
  let mock;
  test.beforeEach(async () => {
    mock = await startMockServer({
      // Two user messages, so the nav pill (and its list) is available.
      eventsByKey: {
        [KEY]: [
          { type: 'user', detail: 'first question', time: BASE, uuid: 'ev-esc-1' },
          { type: 'text', detail: 'first answer', time: BASE + 1000, uuid: 'ev-esc-2' },
          { type: 'user', detail: 'second question', time: BASE + 2000, uuid: 'ev-esc-3' },
          { type: 'text', detail: 'second answer', time: BASE + 3000, uuid: 'ev-esc-4' },
        ],
      },
      agentEvents: { [TASK_ID]: [] },
    });
  });
  // The file-level beforeEach skips before this describe's beforeEach runs.
  test.afterEach(() => { mock?.server.close(); mock = undefined; });

  async function openSession(browser) {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    /** @type {string[]} */
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${KEY}"]`);
    await expect(page.locator('#msg-input')).toBeVisible();
    await expect(page.locator('#nav-pill')).toHaveClass(/visible/);
    // Both Esc listeners stand down while an input has focus.
    await page.evaluate(() => /** @type {HTMLElement} */ (document.activeElement)?.blur());
    return { ctx, page, pageErrors };
  }

  test('one Esc closes the voice overlay, the history popover, the nav list and the agent drill-in', async ({ browser }) => {
    const { ctx, page, pageErrors } = await openSession(browser);
    try {
      const activeTask = () => page.evaluate(() => (/** @type {any} */ (window)).nz.views.agent.activeTaskID());
      // The nav list is opened by a real click. The others are opened without
      // a click, since an outside click closes the nav list (and the history
      // popover).
      await page.click('#nav-counter');
      await expect(page.locator('#nav-list-popover')).toHaveCount(1);
      await page.evaluate(() => (/** @type {any} */ (window)).nz.test.toggleHistory());
      await expect(page.locator('.history-popover')).toHaveCount(1);
      await page.evaluate(() => document.getElementById('voice-overlay')?.classList.add('show'));
      await page.evaluate((id) => (/** @type {any} */ (window)).nz.views.agent.switchTo(id), TASK_ID);
      await expect.poll(activeTask).toBe(TASK_ID);
      // All four are open at once before the key press.
      await expect(page.locator('#nav-list-popover')).toHaveCount(1);
      await expect(page.locator('.history-popover')).toHaveCount(1);
      await expect(page.locator('#voice-overlay')).toHaveClass(/show/);

      await page.keyboard.press('Escape');

      await expect(page.locator('#voice-overlay')).not.toHaveClass(/show/);
      await expect(page.locator('.history-popover')).toHaveCount(0);
      await expect(page.locator('#nav-list-popover')).toHaveCount(0);
      await expect.poll(activeTask).toBeFalsy();
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });

  test('Esc in the composer leaves the history popover and the nav list open', async ({ browser }) => {
    const { ctx, page, pageErrors } = await openSession(browser);
    try {
      await page.click('#nav-counter');
      await expect(page.locator('#nav-list-popover')).toHaveCount(1);
      await page.evaluate(() => (/** @type {any} */ (window)).nz.test.toggleHistory());
      await expect(page.locator('.history-popover')).toHaveCount(1);
      // focus, not click: a click outside the nav list closes it.
      await page.focus('#msg-input');
      await expect(page.locator('#msg-input')).toBeFocused();

      await page.keyboard.press('Escape');

      await expect(page.locator('.history-popover')).toHaveCount(1);
      await expect(page.locator('#nav-list-popover')).toHaveCount(1);
      // The same key with focus elsewhere closes both, so the open state above
      // is the composer guard and not a listener that never ran.
      await page.evaluate(() => /** @type {HTMLElement} */ (document.activeElement)?.blur());
      await page.keyboard.press('Escape');
      await expect(page.locator('.history-popover')).toHaveCount(0);
      await expect(page.locator('#nav-list-popover')).toHaveCount(0);
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });

  test('Alt+N opens the new-session modal, except while typing in the composer', async ({ browser }) => {
    const { ctx, page, pageErrors } = await openSession(browser);
    // With projects on the node createNewSession opens the project palette;
    // with none, the plain modal. Both carry this dialog label.
    const newSession = page.locator('[role="dialog"][aria-label="新建会话"]');
    try {
      // A listener added after boot runs after auth_modal's, so it sees
      // whether that one acted on the key (it calls preventDefault when it does).
      await page.evaluate(() => document.addEventListener('keydown', (e) => {
        if (e.altKey && e.code === 'KeyN') (/** @type {any} */ (window)).__altNPrevented = e.defaultPrevented;
      }));
      const prevented = () => page.evaluate(() => (/** @type {any} */ (window)).__altNPrevented);
      await page.focus('#msg-input');
      await page.keyboard.press('Alt+n');
      expect(await prevented(), 'Alt+N in the composer is left to the composer').toBe(false);

      await page.evaluate(() => /** @type {HTMLElement} */ (document.activeElement)?.blur());
      await page.keyboard.press('Alt+n');
      expect(await prevented()).toBe(true);
      await expect(newSession).toHaveCount(1);
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });
});
