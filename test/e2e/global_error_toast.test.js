// @ts-check
//
// The dashboard's global error handler toasts uncaught exceptions and
// unhandled rejections. Element load errors (a 404 <img>, a <style> the CSP
// refuses) are not exceptions: they carry no error object, so a toast for
// them reads "[object Object]" and tells the operator to refresh a page that
// is working. markdown_csp_render.test.js covers the mermaid <style> case.
//
// 跑法：cd test/e2e && npx playwright test global_error_toast.test.js

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const PREFIX = '页面遇到异常';

/** @param {import('@playwright/test').Page} page */
const recordToasts = (page) => page.evaluate(() => {
  const w = /** @type {any} */ (window);
  const el = /** @type {HTMLElement} */ (document.getElementById('toast'));
  w.__toasts = [];
  new MutationObserver(() => w.__toasts.push(el.textContent)).observe(el, { childList: true, characterData: true, subtree: true });
});

/** @param {import('@playwright/test').Page} page */
const errorToasts = (page) => page.evaluate((p) => /** @type {string[]} */ (/** @type {any} */ (window).__toasts)
  .filter(t => t.includes(p)), PREFIX);

test('a failed <img> load does not toast; a thrown error and a rejection do', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await recordToasts(page);

    const imgFailed = page.evaluate(() => new Promise(resolve => {
      const img = document.createElement('img');
      img.addEventListener('error', () => resolve(true));
      img.src = '/static/no-such-image-3651.png';
      document.body.appendChild(img);
    }));
    expect(await imgFailed).toBe(true);
    // The handler runs synchronously in the same dispatch; a later frame
    // proves nothing was queued either.
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => r(null))));
    expect(await errorToasts(page)).toEqual([]);

    // Positive controls: real failures still surface.
    await page.evaluate(() => { setTimeout(() => { throw new Error('boom-3651'); }, 0); });
    await expect.poll(() => errorToasts(page)).toEqual([PREFIX + '，可能需要刷新：boom-3651']);
    await page.evaluate(() => { Promise.reject(new Error('reject-3651')); });
    await expect.poll(() => errorToasts(page)).toEqual([
      PREFIX + '，可能需要刷新：boom-3651',
      PREFIX + '，可能需要刷新：reject-3651',
    ]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
