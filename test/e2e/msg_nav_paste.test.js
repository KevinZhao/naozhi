// @ts-check
// msg_nav.js's composer paste handler. An image on the clipboard goes to
// composer_files' handleFiles (an import since S20g, an injected dep before),
// so it becomes a pending attachment instead of an <img> pasted into the
// contenteditable. No other spec pastes into the composer.
//
// 跑法：cd test/e2e && npx playwright test msg_nav_paste.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
// A 1x1 PNG, so the image path's decode and re-encode both succeed.
const PNG_1X1 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

test('pasting an image into the composer attaches it instead of embedding it', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  try {
    const page = await ctx.newPage();
    /** @type {string[]} */
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${KEY}"]`);
    await expect(page.locator('#msg-input')).toBeVisible();

    const notPrevented = await page.evaluate((b64) => {
      const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
      const dt = new DataTransfer();
      dt.items.add(new File([bytes], 'shot.png', { type: 'image/png' }));
      const input = /** @type {HTMLElement} */ (document.getElementById('msg-input'));
      input.focus();
      return input.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    }, PNG_1X1);

    expect(notPrevented, 'the browser must not also paste the image into the input').toBe(false);
    await expect.poll(() => page.evaluate(() => (/** @type {any} */ (window)).nz.test.pendingFiles.length)).toBe(1);
    await expect(page.locator('#file-preview .file-thumb')).toHaveCount(1);
    await expect(page.locator('#msg-input img')).toHaveCount(0);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
