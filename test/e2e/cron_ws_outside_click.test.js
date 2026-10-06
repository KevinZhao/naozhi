// @ts-check
//
// The cron modal's workspace popover closes on a click outside it, and the
// outside-click listener re-arms every time the popover opens: a second open
// that skipped the wiring would leave the popover stuck open.
//
// 跑法：cd test/e2e && npx playwright test cron_ws_outside_click.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(() => mock.server.close());

test('the workspace popover closes on an outside click, every time it opens', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.click('.cron-new-btn');
  await page.waitForSelector('.cron-modal');
  const pop = page.locator('#cron-ws-popover');
  for (let i = 0; i < 2; i++) {
    await page.click('#cron-ws-dropdown');
    await expect(pop).toHaveClass(/\bopen\b/);
    await expect(page.locator('#cron-ws-dropdown')).toHaveAttribute('aria-expanded', 'true');
    await page.click('#cron-prompt');
    await expect(pop).not.toHaveClass(/\bopen\b/);
    await expect(page.locator('#cron-ws-dropdown')).toHaveAttribute('aria-expanded', 'false');
  }
});
