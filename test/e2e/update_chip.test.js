// @ts-check
//
// 侧栏头部的自更新 chip（self_update.js）有两条互为反面的契约：
//
// 1. 冷启动默认隐藏。/api/system/update 没有答复（或答复 action:'none'）之前
//    chip 绝不能出现 —— 否则每次刷新都会先闪一个「有更新」再消失，操作员被
//    训练成无视它。
// 2. 服务端说有更新时 chip 出现，并带上目标版本号。
//
// 原 static_update_chip_test.go 只 grep HTML 里的 `hidden` 属性；这里两个
// 方向都行为化（同一份 markup，分别在无路由 / 有更新两种 mock 下渲染）。
//
// 跑法：cd test/e2e && npx playwright test update_chip.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test('无更新信息时 chip 保持冷启动隐藏', async ({ browser }) => {
  const mock = await startMockServer({}); // 无 systemUpdate 路由 → 404
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  // 首个 update poll 已经打出去也失败了之后再断言，否则只是在断言「还没轮到它」。
  await page.waitForTimeout(500);
  await expect(page.locator('#btn-update')).toBeHidden();
  await ctx.close();
  mock.server.close();
});

test('服务端报有更新时 chip 出现并带目标版本', async ({ browser }) => {
  const mock = await startMockServer({
    systemUpdate: {
      action: 'update',
      current: 'v0.1.12',
      latest: 'v0.1.13',
      phase: 'idle',
      can_apply: true,
      install_enabled: true,
    },
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  const chip = page.locator('#btn-update');
  await expect(chip).toBeVisible();
  await expect(page.locator('#update-tag')).toHaveText('v0.1.13');
  await expect(chip, '未在应用中，不该有 busy 态').not.toHaveClass(/is-busy/);
  await ctx.close();
  mock.server.close();
});
