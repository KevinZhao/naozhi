// @ts-check
//
// 窄 list-pane 下 cj-row 防压扁实测（#199 修复的回归保护）：narrow / single 模式
// 下 cj-main 列不应被三个 auto 列挤到个位数 px，cj-when + cj-stats 隐藏让 1fr
// 列回归实际可用空间。
//
// 阈值来源（#2472 重定视口）：
//   - cron_view.js setupCronLayoutObserver：按 .cron-list-pane 的 offsetWidth
//     分档 —— lpW ≥600 wide / ≥420 medium / ≥300 narrow / <300 single
//     （1035262a #231 从「body 宽 ≥1100/820/560」改为「list-pane 宽」）；
//     ResizeObserver 挂在 .cron-detail-body 上，body 尺寸变化时才重新分档。
//   - dashboard.html `.cron-detail-body.has-drawer > .cron-list-pane{flex:0 0 380px}`
//     及 `[data-cron-layout="narrow"].has-drawer > .cron-list-pane{flex:0 0 320px}`。
//   - dashboard.html `body.nz-view-cron .sidebar{display:none!important}`
//     （853f6834 #1834 定时任务升为顶层视图）：cron 视图不再有侧边栏，
//     .cron-detail-body ≈ viewport − activity-bar(56)，所以 1100 视口首绘就是
//     wide（旧断言假设 main 区 ~736 已失效）。
//
// 因此进入 narrow 的确定性路径是：wide 下先开 drawer（list-pane 被钉到 380），
// 再缩视口触发 ResizeObserver → 重新按 lpW=380 分档 → narrow → list-pane 320。
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

async function openCronWithDrawer(browser, viewport, mock) {
  const ctx = await browser.newContext({ viewport });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector(ROW);
  await page.click(ROW);
  await page.waitForSelector('.cron-detail-body.has-drawer .cron-detail-pane.is-open');
  return { ctx, page };
}

function measure(page) {
  return page.evaluate((rowSel) => {
    const body = document.querySelector('.cron-detail-body');
    const lp = document.querySelector('.cron-list-pane');
    const card = document.querySelector(rowSel);
    const main = card.querySelector('.cj-main');
    const when = card.querySelector('.cj-when');
    const stats = card.querySelector('.cj-stats');
    return {
      layout: body ? body.dataset.cronLayout : null,
      listPaneW: lp ? Math.round(lp.getBoundingClientRect().width) : 0,
      mainW: Math.round(main.getBoundingClientRect().width),
      whenDisplay: when ? getComputedStyle(when).display : 'absent',
      statsDisplay: stats ? getComputedStyle(stats).display : 'absent',
    };
  }, ROW);
}

test.describe('cj-row 窄屏不被压扁', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('drawer 打开后缩视口进入 narrow：list-pane 320、cj-main 列宽 ≥ 100px', async ({ browser }) => {
    const { ctx, page } = await openCronWithDrawer(browser, { width: 1100, height: 800 }, mock);

    // 前置：1100 视口首绘 wide（lpW = 1044 ≥ 600），drawer 打开后 list-pane 钉 380
    const before = await measure(page);
    expect(before.layout).toBe('wide');
    expect(before.listPaneW).toBe(380);

    // 缩视口 → .cron-detail-body 尺寸变化 → ResizeObserver 重新分档：
    // lpW=380 ∈ [300,420) → narrow
    await page.setViewportSize({ width: 1000, height: 800 });
    await expect(page.locator('.cron-detail-body')).toHaveAttribute('data-cron-layout', 'narrow');

    const m = await measure(page);
    expect(m.layout).toBe('narrow');
    expect(m.listPaneW).toBe(320);
    // cj-main 列必须 ≥ 100px（#199 修复前 ~0.4px）
    expect(m.mainW).toBeGreaterThan(100);
    // cj-when 和 cj-stats 在 narrow 模式被隐藏
    expect(['none', 'absent']).toContain(m.whenDisplay);
    expect(['none', 'absent']).toContain(m.statsDisplay);

    await ctx.close();
  });

  test('宽屏 wide 模式下 list-pane 380、cj-main 不受窄屏规则影响', async ({ browser }) => {
    const { ctx, page } = await openCronWithDrawer(browser, { width: 1600, height: 900 }, mock);

    const m = await measure(page);
    // 宽屏应进 wide 模式，list-pane 走 380 档
    expect(m.layout).toBe('wide');
    expect(m.listPaneW).toBe(380);
    // wide 下 cj-main 比 narrow(320 档) 更宽，标题可见
    expect(m.mainW).toBeGreaterThan(200);
    await expect(page.locator(ROW + ' .cj-title')).toBeVisible();
    // 1035262a (#231) cron-dashboard-redesign P0 follow-up：列表行只留 title +
    // schedule，`.cj-list .cj-row .cj-when/.cj-stats{display:none!important}` 在
    // 所有 layout 档都隐藏 —— 不再按 wide 断言 cj-when 可见。
    expect(['none', 'absent']).toContain(m.whenDisplay);

    await ctx.close();
  });
});
