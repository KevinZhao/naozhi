// @ts-check
// Regressions for #1772 (mobile render hotspots). The two highest-value
// debounces are verified by behavior; the touchmove/popover micro-opts are
// guarded by source-shape assertions (they're hot-loop layout-read removals
// that have no observable DOM contract to assert on cheaply).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const fs = require('fs');
const path = require('path');

const desktop = { viewport: { width: 1280, height: 800 } };

test.describe('#1772 mobile render hotspots', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('sidebar search re-render is debounced (not per-keystroke)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Open the sidebar search box.
    await page.click('#btn-sidebar-search');
    await page.waitForSelector('#sidebar-search-input', { state: 'visible' });

    // Instrument renderSidebar to count invocations.
    await page.evaluate(() => {
      // eslint-disable-next-line no-eval
      window.__renderCount = 0;
      // renderSidebar is a top-level function; wrap it via eval in page scope.
      // eslint-disable-next-line no-eval
      eval('var __origRenderSidebar = renderSidebar; renderSidebar = function(){ window.__renderCount++; return __origRenderSidebar.apply(this, arguments); };');
    });

    const input = page.locator('#sidebar-search-input');
    // Type a 5-char burst quickly.
    await input.type('hello', { delay: 10 });

    // Immediately after the burst, the debounced render must NOT have fired
    // once per keystroke. Allow at most 1 (in case the last timer just fired).
    const immediate = await page.evaluate(() => window.__renderCount);
    expect(immediate).toBeLessThanOrEqual(1);

    // After the debounce window, exactly one render should have landed.
    await page.waitForTimeout(250);
    const settled = await page.evaluate(() => window.__renderCount);
    expect(settled).toBeGreaterThanOrEqual(1);
    // A 5-char burst must collapse to far fewer than 5 renders.
    expect(settled).toBeLessThan(5);

    await ctx.close();
  });

  test('source: swipe-delete caches offsetWidth and popover dismiss is gated', async () => {
    // These are hot-loop layout-read removals with no cheap DOM contract to
    // assert at runtime; pin them at the source level so they can't regress.
    const js = fs.readFileSync(
      path.join(__dirname, '..', '..', 'internal/server/static/dashboard.js'),
      'utf8'
    );
    // touchmove must use the cached cardW, not a per-frame card.offsetWidth read.
    expect(js).toContain('-dx / cardW * 0.6');
    expect(js).not.toContain('-dx / card.offsetWidth * 0.6');
    // scroll handler must gate the popover dismiss on the open flag.
    expect(js).toContain('if (navPopoverOpen) navDismissPopover();');
  });

  test('source: asset_browser search input is debounced', async () => {
    const js = fs.readFileSync(
      path.join(__dirname, '..', '..', 'internal/server/static/asset_browser.js'),
      'utf8'
    );
    // The raw per-keystroke binding must be gone, replaced by a setTimeout debounce.
    expect(js).not.toContain("s.addEventListener('input', render)");
    expect(js).toContain('searchDebounce = setTimeout(');
    expect(js).toContain('150');
    // Tab switch must cancel a pending search debounce.
    expect(js).toContain('cancelSearchDebounce()');
  });
});
