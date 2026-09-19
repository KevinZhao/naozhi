// @ts-check
//
// 分屏停靠契约（原 static_split_view_test.go 的行为化版本）：桌面上打开追问
// 抽屉必须把它停靠成右侧分屏而不是盖在对话流上 —— 对话流压缩进剩余宽度、
// 保持可见，可拖拽的分屏缝（#split-resizer）出现；关闭后复原。任何一环退化
// （回退成 overlay / 缝消失 / 对话流被盖住）对应断言就红。
//
// 触发走真实路径：hover 一条消息 → 「追问」按钮 → openScratch → showDrawer
// → nzSplitEnter 给 body 加 nz-split-open。
//
// 跑法：cd test/e2e && npx playwright test split_view_dock.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test('追问抽屉在桌面停靠为分屏，对话流保持可见，关闭后复原', async ({ browser }) => {
  // 追问按钮只在 >500 字符的 text 事件上渲染（与复制按钮共用同一道闸）。
  const mock = await startMockServer({
    events: [
      { type: 'user', detail: 'hello', time: Date.now() - 8000, uuid: 'ev-usr-1' },
      { type: 'text', detail: '这是一条足够长的回复。' + '内容填充，凑到追问按钮的长度闸以上。'.repeat(30), time: Date.now() - 5000, uuid: 'ev-txt-2' },
    ],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);

  const events = page.locator('#events-scroll');
  await expect(events).toBeVisible();
  const beforeBox = await events.boundingBox();
  if (!beforeBox) throw new Error('events-scroll has no box');

  // hover-only 的追问按钮：悬停到第一条带按钮的消息上再点。
  const askBtn = page.locator('.event-ask-btn').first();
  await askBtn.evaluate((el) => el.closest('.msg, .event, [data-msg-time]')?.scrollIntoView());
  await askBtn.hover({ force: true });
  await askBtn.click({ force: true });

  // 1. 停靠开关挂上，抽屉与分屏缝可见。
  await expect(page.locator('body')).toHaveClass(/nz-split-open/);
  const drawer = page.locator('#aside-drawer');
  await expect(drawer).toBeVisible();
  await expect(page.locator('#split-resizer')).toBeVisible();

  // 2. 对话流被压缩而不是被覆盖：仍可见、变窄，且与抽屉水平不重叠。
  const afterBox = await events.boundingBox();
  const drawerBox = await drawer.boundingBox();
  if (!afterBox || !drawerBox) throw new Error('split boxes missing');
  expect(afterBox.width, '对话流应压缩进剩余宽度').toBeLessThan(beforeBox.width);
  expect(afterBox.width, '对话流不能被挤没').toBeGreaterThan(200);
  expect(
    afterBox.x + afterBox.width,
    '抽屉必须停靠在对话流右侧（overlay 回归会让两者重叠）'
  ).toBeLessThanOrEqual(drawerBox.x + 1);

  // 3. 关闭后复原：开关摘掉，对话流宽度回到停靠前。
  await page.click('#ad-close');
  await expect(page.locator('body')).not.toHaveClass(/nz-split-open/);
  await expect.poll(async () => {
    const b = await events.boundingBox();
    return b ? Math.round(b.width) : 0;
  }).toBe(Math.round(beforeBox.width));

  await ctx.close();
  mock.server.close();
});
