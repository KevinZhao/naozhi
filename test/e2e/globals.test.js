// @ts-check
// The dashboard owns exactly one global, window.nz (#2948). Everything else a
// module shares goes through an import, so a new window.X or top-level global
// shows up here. Served without the e2e shim, which adds the suite's probes.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test('the dashboard defines no global but nz', async ({ browser }) => {
  const mock = await startMockServer({ shim: false });
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    const extra = await page.evaluate(() => {
      const frame = document.createElement('iframe');
      document.body.appendChild(frame);
      const base = new Set(Object.getOwnPropertyNames(/** @type {Window} */ (frame.contentWindow)));
      frame.remove();
      return Object.getOwnPropertyNames(window).filter((k) => !base.has(k)).sort();
    });
    expect(extra).toEqual(['nz']);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
