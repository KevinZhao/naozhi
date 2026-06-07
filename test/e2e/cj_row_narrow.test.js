// @ts-check
//
// 窄屏 cj-row 防压扁实测：1100×800 viewport（main 区 ~736、list-pane 320，
// 触发 data-cron-layout="narrow"）下，cron 列表行的 cj-main 列不应被挤到
// 个位数 px。验证修复：narrow / single 模式下隐藏 cj-when + cj-stats 让
// 1fr 列回归实际可用空间。
//
// 跑法：cd test/e2e && npx playwright test cj_row_narrow.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test.describe('cj-row 窄屏不被压扁', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('1100×800 narrow 模式下 cj-main 列宽 ≥ 100px', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1100, height: 800 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    // 点开 drawer 让 has-drawer + ResizeObserver 启动 narrow layout
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForTimeout(300);

    const m = await page.evaluate(() => {
      const body = document.querySelector('.cron-detail-body');
      const card = document.querySelector('.cj-row[data-cron-id="cron-001"]');
      const main = card.querySelector('.cj-main');
      const cs = getComputedStyle(card);
      return {
        layout: body ? body.dataset.cronLayout : null,
        cardW: Math.round(card.getBoundingClientRect().width),
        gridCols: cs.gridTemplateColumns,
        mainW: Math.round(main.getBoundingClientRect().width),
        whenDisplay: card.querySelector('.cj-when') ? getComputedStyle(card.querySelector('.cj-when')).display : 'absent',
        statsDisplay: card.querySelector('.cj-stats') ? getComputedStyle(card.querySelector('.cj-stats')).display : 'absent',
      };
    });

    // 1100 viewport 应进入 narrow（不是 medium 也不是 single）
    expect(m.layout).toBe('narrow');
    // cj-main 列必须 ≥ 100px（修复前 ~0.4px）
    expect(m.mainW).toBeGreaterThan(100);
    // cj-when 和 cj-stats 在 narrow 模式被隐藏
    expect(['none', 'absent']).toContain(m.whenDisplay);
    expect(['none', 'absent']).toContain(m.statsDisplay);

    await ctx.close();
  });

  test('宽屏 wide 模式下 cj-when + cj-stats 仍可见（不影响桌面）', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.waitForSelector('.cj-row');
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForTimeout(300);

    const m = await page.evaluate(() => {
      const body = document.querySelector('.cron-detail-body');
      const card = document.querySelector('.cj-row[data-cron-id="cron-001"]');
      const when = card.querySelector('.cj-when');
      // stats 仅在 j.stats.total>0 时渲染；cron-001 mock 没填 stats，无 .cj-stats 节点
      return {
        layout: body ? body.dataset.cronLayout : null,
        whenDisplay: when ? getComputedStyle(when).display : 'absent',
      };
    });

    // 宽屏应进 wide 模式
    expect(m.layout).toBe('wide');
    // cj-when 应显示（not 'none'）
    expect(m.whenDisplay).not.toBe('none');

    await ctx.close();
  });
});
