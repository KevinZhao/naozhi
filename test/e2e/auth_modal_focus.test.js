// @ts-check
// Regression: the empty state's quick-ask textarea autofocuses 50ms after it
// paints, and cold start paints it at the same moment the /api/sessions 401
// mounts the auth modal. The timer fired unconditionally, so focus jumped out
// of the modal — and the token the operator was typing landed in the quick-ask
// box, where Enter submits it as a session prompt (createQuickSession also
// unmounts the modal on its way out). The autofocus now re-checks for a mounted
// overlay at fire time.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

// recordFocus logs every focusin target id together with whether an overlay was
// mounted at that instant. Asserting on the final activeElement cannot see this
// bug: both timers end with the modal input focused either way. Recording the
// overlay state at focus time also keeps the legal ordering legal — quick-ask
// may take focus before the 401 mounts the modal.
async function recordFocus(page) {
  await page.addInitScript(() => {
    window.__focusLog = [];
    document.addEventListener('focusin', (e) => {
      const el = /** @type {Element} */ (e.target);
      window.__focusLog.push({
        id: el && el.id ? el.id : '',
        overlay: !!document.querySelector('.modal-overlay'),
      });
    });
  });
}

test.describe('auth modal keeps the keyboard', () => {
  test('quick-ask autofocus does not steal focus from the auth modal', async ({ browser }) => {
    const mock = await startMockServer({ requireAuth: true, authToken: 'secret-token' });
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await recordFocus(page);

    // Reloading is what makes this deterministic. Which timer wins the cold
    // start varies per load, and quick-ask taking focus BEFORE the 401 mounts
    // the modal is legal — so a single load only catches the bug ~7 times in 8.
    // The invariant has to hold on every load, so assert it across several.
    for (let i = 0; i < 5; i++) {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.modal-overlay');
      // Both deferred focus timers (quick-ask 50ms, modal input 100ms) have
      // fired well before this resolves.
      await expect(page.locator('#token-input')).toBeFocused();
      const stolen = await page.evaluate(() =>
        window.__focusLog.filter((e) => e.id === 'quick-ask-input' && e.overlay));
      expect(stolen, `load ${i}: quick-ask must not take focus while the token modal is mounted`).toEqual([]);
    }

    // The strongest statement of the same invariant: a token typed into the
    // modal must not be able to leave as a session prompt.
    await page.fill('#token-input', 'secret-token');
    await page.keyboard.press('Enter');
    await page.waitForSelector('.modal-overlay', { state: 'detached' });
    expect(mock.sendCalls, 'the typed token must not reach a session prompt').toHaveLength(0);
    expect(mock.loginCalls).toEqual([JSON.stringify({ token: 'secret-token' })]);

    await ctx.close();
    mock.server.close();
  });

  test('the modal focus timer tolerates an overlay that unmounts first', async ({ browser }) => {
    const mock = await startMockServer({ requireAuth: true, authToken: 'secret-token' });
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(e.message));
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.modal-overlay');

    // createQuickSession does exactly this — unmount every overlay — and it can
    // land inside the modal's 100ms focus window.
    await page.evaluate(() => {
      const o = document.querySelector('.modal-overlay');
      if (o) o.remove();
    });
    await page.waitForTimeout(300);
    expect(pageErrors, 'the deferred focus must not throw when its input is gone').toEqual([]);

    await ctx.close();
    mock.server.close();
  });

  test('with no modal in the way cold start still autofocuses quick-ask', async ({ browser }) => {
    const mock = await startMockServer({ sessions: [] });
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await recordFocus(page);
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('#quick-ask-input');

    // The guard must not cost the feature: "open the page, start typing" is
    // the reason the autofocus exists.
    await expect(page.locator('#quick-ask-input')).toBeFocused();

    await ctx.close();
    mock.server.close();
  });
});
