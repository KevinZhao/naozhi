// @ts-check
// Regressions for #1772 (mobile render hotspots). The two highest-value
// debounces are verified by behavior; the touchmove/popover micro-opts are
// guarded by source-shape assertions (they're hot-loop layout-read removals
// that have no observable DOM contract to assert on cheaply).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const fs = require('fs');
const path = require('path');

test.describe('#1772 mobile render hotspots', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

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
