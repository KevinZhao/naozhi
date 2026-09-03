// @ts-check
// 移动端布局 e2e：列表视图 / 选会话切聊天视图 / 返回按钮 / popstate /
// dismiss 免 hover / 弹层不超屏宽 / toast 置顶 / 桌面无 mobile class / manifest。
//
// #2472：本 spec 原先自带内联 mock，只吐 dashboard.html + manifest.json；
// dashboard 拆分出 nz_util.js / cron_view.js / files_view.js 等静态脚本后
// （#1874 fa001adf 等），内联 mock 对这些路径一律 404 → 页面脚本加载失败 →
// `.session-card` 永不渲染、`mobile-list-view` 永不落地。改用共享的
// mock-server.js（与其他 spec 一致，它按 STATIC_DIR 透传所有 .js）。
const { test, expect, devices } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const iPhone = devices['iPhone 13'];
const desktop = { viewport: { width: 1280, height: 800 } };
// dashboard.html @media(max-width:768px)：.sidebar 默认 translateX(0)，
// body.mobile-chat-view 时 translateX(-100%)；-100% 在 iPhone 13 上就是 -390px。
const SIDEBAR_SHOWN = 'matrix(1, 0, 0, 1, 0, 0)';
const SIDEBAR_HIDDEN = `matrix(1, 0, 0, 1, -${iPhone.viewport.width}, 0)`;

test.describe('Mobile dashboard', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  /** 新建独立 context 打开 dashboard，等首屏会话卡片渲染完。 */
  async function open(browser, ctxOpts) {
    const ctx = await browser.newContext({ ...ctxOpts });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    return { ctx, page };
  }

  test('on mobile: sidebar starts visible (list view)', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    const body = page.locator('body');
    await expect(body).toHaveClass(/mobile-list-view/);
    await expect(body).not.toHaveClass(/mobile-chat-view/);
    await expect(page.locator('.sidebar')).toHaveCSS('transform', SIDEBAR_SHOWN);

    await ctx.close();
  });

  test('on mobile: selecting a session switches to chat view', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    await page.locator('.session-card').first().click();

    const body = page.locator('body');
    await expect(body).toHaveClass(/mobile-chat-view/);
    await expect(body).not.toHaveClass(/mobile-list-view/);
    // toHaveCSS 轮询到 .25s transition 结束后的终值，不用 sleep。
    await expect(page.locator('.sidebar')).toHaveCSS('transform', SIDEBAR_HIDDEN);
    // 主区 header 的返回按钮在移动端可见
    await expect(page.locator('.main .btn-mobile-back')).toBeVisible();

    await ctx.close();
  });

  test('on mobile: back button returns to session list', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    await page.locator('.session-card').first().click();
    const back = page.locator('.main .btn-mobile-back');
    await expect(back).toBeVisible();
    await back.click();

    const body = page.locator('body');
    await expect(body).toHaveClass(/mobile-list-view/);
    await expect(body).not.toHaveClass(/mobile-chat-view/);
    await expect(page.locator('.sidebar')).toHaveCSS('transform', SIDEBAR_SHOWN);

    await ctx.close();
  });

  test('on mobile: browser back (popstate) returns to session list', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    await page.locator('.session-card').first().click();
    const body = page.locator('body');
    await expect(body).toHaveClass(/mobile-chat-view/);

    // mobileEnterChat 推了一条 {view:'chat'} history entry；浏览器后退 →
    // popstate → mobileShowList。
    await page.goBack();

    await expect(body).toHaveClass(/mobile-list-view/);
    await expect(body).not.toHaveClass(/mobile-chat-view/);

    await ctx.close();
  });

  test('on mobile: dismiss button is visible without hover', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    // 桌面端 .btn-dismiss 默认 opacity:0 只在 :hover 出现；≤768px 规则给到 .6/.75。
    const opacity = await page.locator('.session-card .btn-dismiss').first()
      .evaluate(el => parseFloat(getComputedStyle(el).opacity));
    expect(opacity).toBeGreaterThan(0);

    await ctx.close();
  });

  test('on mobile: new-session palette and custom-workspace modal fit within screen width', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);
    const viewportWidth = iPhone.viewport.width;

    // 有项目时「新建会话」走 command palette（openProjectPalette），不再是 .modal
    await page.locator('#btn-new-session').click();
    const palette = page.locator('.cmd-palette');
    await expect(palette).toBeVisible();
    const paletteBox = await palette.boundingBox();
    expect(paletteBox).not.toBeNull();
    expect(paletteBox.width).toBeLessThanOrEqual(viewportWidth);

    // 末行「打开自定义工作目录…」→ pickPaletteCustom 弹出 .modal
    await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();
    const modal = page.locator('.modal');
    await expect(modal).toBeVisible();
    const modalBox = await modal.boundingBox();
    expect(modalBox).not.toBeNull();
    expect(modalBox.width).toBeLessThanOrEqual(viewportWidth);

    await ctx.close();
  });

  test('on mobile: toast appears at top of screen', async ({ browser }) => {
    const { ctx, page } = await open(browser, iPhone);

    await page.evaluate(() => {
      const el = document.getElementById('toast');
      el.textContent = 'test message';
      el.classList.add('show');
    });

    const toast = page.locator('#toast.show');
    await expect(toast).toBeVisible();
    const box = await toast.boundingBox();
    expect(box).not.toBeNull();
    // ≤768px：.toast{bottom:auto;top:max(16px,safe-area)} → 落在上半屏
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.y + box.height).toBeLessThan(iPhone.viewport.height / 2);

    await ctx.close();
  });

  test('on desktop: no mobile classes, sidebar is always visible', async ({ browser }) => {
    const { ctx, page } = await open(browser, desktop);

    const body = page.locator('body');
    await expect(body).not.toHaveClass(/mobile-list-view/);
    await expect(body).not.toHaveClass(/mobile-chat-view/);

    // 选会话让 renderMainShell 渲染出 .btn-mobile-back；桌面端 display:none
    await page.locator('.session-card').first().click();
    const back = page.locator('.main .btn-mobile-back');
    await expect(back).toHaveCount(1);
    await expect(back).toBeHidden();
    await expect(page.locator('.sidebar')).toBeVisible();

    await expect(body).not.toHaveClass(/mobile-list-view/);
    await expect(body).not.toHaveClass(/mobile-chat-view/);

    await ctx.close();
  });

  test('manifest.json is served correctly', async ({ request }) => {
    const response = await request.get(mock.url + '/manifest.json');
    expect(response.status()).toBe(200);
    const body = await response.json();
    // 品牌前缀 >_naozhi：8a8e00a8 (#1982)
    expect(body.name).toBe('>_naozhi dashboard');
    expect(body.short_name).toBe('>_naozhi');
    expect(body.display).toBe('standalone');
    expect(body.start_url).toBe('/dashboard');
  });
});
