// @ts-check
// The session-card context menu and the sidebar collapse preference, both in
// mobile_nav.js. The menu (long-press on a phone, right-click on desktop)
// renames, copies or deletes the card it was opened on: rename must select
// that card's session first (shell.selectSession), because renameSession
// acts on the current selection and repaints only the current header; delete
// goes through dismissSession. The collapse preference is read from
// localStorage once at boot and written by the `[` toggle.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const OPEN_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const MENU_KEY = 'dashboard:direct:2026-01-01-120002-3:myproject';

test.describe('mobile_nav actions', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeEach(async () => { mock = await startMockServer(); });
  test.afterEach(() => mock.server.close());

  async function open(browser, init) {
    const ctx = await browser.newContext({ ...desktop });
    if (init) await ctx.addInitScript(init);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    // attached, not visible: a collapsed sidebar hides the cards.
    await page.waitForSelector('.session-card', { state: 'attached' });
    return { ctx, page };
  }

  test('rename from the context menu selects that card before renaming it', async ({ browser }) => {
    const { ctx, page } = await open(browser);
    // Another session is open, so renaming the current selection would hit
    // the wrong one.
    await page.click(`.session-card[data-key="${OPEN_KEY}"]`);
    await expect.poll(() => page.evaluate(() => /** @type {any} */ (window).selectedKey)).toBe(OPEN_KEY);

    await page.click(`.session-card[data-key="${MENU_KEY}"]`, { button: 'right' });
    const items = page.locator('#session-ctx-menu .ctx-menu-item');
    await expect(items).toHaveCount(3);
    expect(await items.locator('.ctx-icon').allTextContents()).toEqual(['✎', '⎘', '🗑']);

    await items.filter({ hasText: '重命名' }).click();
    await expect.poll(() => page.evaluate(() => /** @type {any} */ (window).selectedKey)).toBe(MENU_KEY);
    await page.fill('.prompt-dialog .prompt-input', '菜单改名');
    await page.click('.prompt-dialog .prompt-ok');

    await expect(page.locator('#main .main-header h2')).toContainText('菜单改名');
    expect(mock.labelCalls.map((b) => JSON.parse(b).key)).toEqual([MENU_KEY]);
    await ctx.close();
  });

  test('delete from the context menu dismisses that card', async ({ browser }) => {
    const { ctx, page } = await open(browser);
    await page.click(`.session-card[data-key="${MENU_KEY}"]`, { button: 'right' });
    const del = page.waitForRequest((r) => r.method() === 'DELETE' && new URL(r.url()).pathname === '/api/sessions');
    await page.locator('#session-ctx-menu .ctx-menu-item').filter({ hasText: '删除' }).click();
    expect(JSON.parse((await del).postData() || '{}').key).toBe(MENU_KEY);
    // The refetch after the DELETE is the one that must not revive the card.
    await page.waitForResponse((r) => r.request().method() === 'GET' && new URL(r.url()).pathname === '/api/sessions');
    await expect(page.locator(`.session-card[data-key="${MENU_KEY}"]`)).toHaveCount(0);
    await expect(page.locator('#session-ctx-menu')).toHaveCount(0);
    await ctx.close();
  });

  test('a saved collapse is applied at boot and the [ toggle saves the new state', async ({ browser }) => {
    const { ctx, page } = await open(browser, () => localStorage.setItem('nz:sidebar_collapsed', '1'));
    const body = page.locator('body');
    await expect(body).toHaveClass(/sidebar-collapsed/);
    await expect(page.locator('#btn-sidebar-toggle')).toHaveAttribute('aria-expanded', 'false');

    await page.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
    await page.keyboard.press('[');
    await expect(body).not.toHaveClass(/sidebar-collapsed/);
    expect(await page.evaluate(() => localStorage.getItem('nz:sidebar_collapsed'))).toBe('0');
    await ctx.close();
  });
});
