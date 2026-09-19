// @ts-check
//
// 「对话框已显示 working 但左边栏卡片还是旧状态」的去同步修复：send 成功后
// markSessionOptimisticRunning 必须在同一次调用里同时翻转主区（横幅/停止
// 按钮）与侧栏卡片的圆点+状态文字（patchSidebarCardState），而不是让侧栏等
// 服务端的 session_state 推送（CLI 冷启动时几百毫秒）或 5s poll。
//
// 本 spec 用默认 no-WS 的 mock：没有任何推送会来，REST 快照也一直说 ready，
// 所以两个界面能变 running 的唯一来源就是乐观翻转本身 —— 断言在点击后立即
// 成立（远快于 5s poll），去掉翻转的任何一半都会让对应断言超时。
//
// 原 static_working_state_sync_test.go 用 grep 钉 patchSidebarCardState 的
// 调用点；这里直接看两个界面是否同时翻。
//
// 跑法：cd test/e2e && npx playwright test working_state_sync.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test('send 成功后主区与侧栏卡片同帧翻到 running', async ({ browser }) => {
  // 首次加载之后的 sessions 响应全部扣住 30s：断言窗口内不可能有列表重绘，
  // 能翻转侧栏圆点的只剩 patchSidebarCardState 的原地补丁。
  const mock = await startMockServer({ sessionsDelayMs: 30000, sessionsDelayAfterCalls: 1 });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect(page.locator('#msg-input')).toBeVisible();

  const card = page.locator(`.session-card[data-key="${KEY}"]`);
  await expect(card.locator('.sc-dot')).toHaveClass(/dot-ready/);

  await page.fill('#msg-input', '触发乐观翻转');
  await page.click('#btn-send');

  // 两个界面一起翻。列表重绘被扣在 30s 外，这里的信号只能来自本地翻转。
  await expect(page.locator('#btn-stop')).toBeVisible();
  await expect(card.locator('.sc-dot')).toHaveClass(/dot-running/);
  await expect(card.locator('.sc-meta span').nth(1)).toHaveText('running');

  await ctx.close();
  mock.server.close();
});
