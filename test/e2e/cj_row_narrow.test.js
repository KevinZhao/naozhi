// @ts-check
//
// The cron panel's layout tier (data-cron-layout on .cron-detail-body) and the
// list pane width it sets while a drawer is open.
//
// setupCronLayoutObserver gauges the tier off the body's own width (≥1100 wide
// / ≥820 medium / ≥560 narrow / below single) and cron.css pins the list pane
// to 380 / 360 / 320 px (or hides it in single) while a drawer is open. Opening
// the drawer does not change the body's width, so it does not change the tier:
// the list pane's width at a given window width no longer depends on whether
// the window was resized with the drawer open (#2824, where the tier was
// gauged off the list pane it itself sized).
//
// The cron view has no sidebar, so on desktop the body is the viewport minus
// the 56 px activity bar.
//
// 跑法：cd test/e2e && npx playwright test cj_row_narrow.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

const ROW = '.cj-row[data-cron-id="cron-001"]';

/** @param {import('@playwright/test').Browser} browser @param {{width: number, height: number}} viewport @param {any} mock */
async function openCronPanel(browser, viewport, mock) {
  const ctx = await browser.newContext({ viewport });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector(ROW);
  return { ctx, page };
}

/** @param {import('@playwright/test').Page} page */
async function openDrawer(page) {
  await page.click(ROW);
  await page.waitForSelector('.cron-detail-body.has-drawer .cron-detail-pane.is-open');
}

/** @param {import('@playwright/test').Page} page */
function measure(page) {
  return page.evaluate((rowSel) => {
    const body = /** @type {HTMLElement} */ (document.querySelector('.cron-detail-body'));
    const lp = document.querySelector('.cron-list-pane');
    const main = document.querySelector(rowSel + ' .cj-main');
    return {
      layout: body.dataset.cronLayout,
      listPaneW: lp ? Math.round(lp.getBoundingClientRect().width) : 0,
      listPaneShown: lp ? getComputedStyle(lp).display !== 'none' : false,
      mainW: main ? Math.round(main.getBoundingClientRect().width) : 0,
    };
  }, ROW);
}

test.describe('cron layout tier', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('opening and closing the drawer leaves the tier alone', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, { width: 1600, height: 900 }, mock);
    expect((await measure(page)).layout).toBe('wide');
    await openDrawer(page);
    // Give the observer a frame to (not) react to the list pane shrinking.
    await page.waitForTimeout(300);
    const open = await measure(page);
    expect(open.layout).toBe('wide');
    expect(open.listPaneW).toBe(380);

    await page.click('.cron-detail-pane.is-open [data-action="cron-detail-close"]');
    await expect(page.locator('.cron-detail-body')).not.toHaveClass(/has-drawer/);
    await page.waitForTimeout(300);
    expect((await measure(page)).layout).toBe('wide');
    await ctx.close();
  });

  test('the list pane width at a window width does not depend on history', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, { width: 1600, height: 900 }, mock);
    await openDrawer(page);
    // Resize with the drawer open, then back: the pane must return to the
    // width a fresh open at 1600 gives, not stay at the narrower tier.
    await page.setViewportSize({ width: 1100, height: 900 });
    await expect(page.locator('.cron-detail-body')).toHaveAttribute('data-cron-layout', 'medium');
    expect((await measure(page)).listPaneW).toBe(360);
    await page.setViewportSize({ width: 1600, height: 900 });
    await expect(page.locator('.cron-detail-body')).toHaveAttribute('data-cron-layout', 'wide');
    expect((await measure(page)).listPaneW).toBe(380);
    await ctx.close();
  });

  test('narrow windows get the narrow tier and the list title keeps its room', async ({ browser }) => {
    const { ctx, page } = await openCronPanel(browser, { width: 860, height: 800 }, mock);
    expect((await measure(page)).layout).toBe('narrow');
    await openDrawer(page);
    const m = await measure(page);
    expect(m.layout).toBe('narrow');
    expect(m.listPaneW).toBe(320);
    // #199: three auto columns once squeezed .cj-main to ~0.4px.
    expect(m.mainW).toBeGreaterThan(100);
    await ctx.close();
  });

  test('below 560px of body the drawer takes the whole panel', async ({ browser }) => {
    // Phone widths drop the activity bar, so the body is the full viewport.
    const { ctx, page } = await openCronPanel(browser, { width: 500, height: 800 }, mock);
    expect((await measure(page)).layout).toBe('single');
    await openDrawer(page);
    expect((await measure(page)).listPaneShown).toBe(false);
    await ctx.close();
  });
});
