// E2E: the "答应你的" group (promises.js, planner RFC §3.6, §10.1) renders
// GET /api/promises — earliest due first, overdue and due-today marked,
// undated last, the closed ones under a fold — and its 关闭 button closes a
// promise through POST /api/promises/close. The Planner page that hosts it
// lands separately, so the test mounts the group into a column of its own.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const later = new Date(now + 3 * 24 * 3600 * 1000);
later.setHours(10, 0, 0, 0);

function fixture() {
  return {
    open: [
      { id: 'pr_overdue', text: '周五前整理 JD 会前材料', due: iso(now - 3600000), state: 'open', created: iso(now - 86400000), source: 'planner' },
      { id: 'pr_later', text: '明早提醒你看压测结果', due: iso(later.getTime()), state: 'open', created: iso(now - 600000), source: 'user' },
      { id: 'pr_undated', text: '全部合并后告诉你', close_when: 'review issues 的 PR 全部合并', state: 'open', created: iso(now - 300000), source: 'planner' },
    ],
    closed: [
      { id: 'pr_done', text: 'CI 绿了告诉你', state: 'closed', created: iso(now - 7200000), closed_at: iso(now - 3600000), source: 'planner' },
    ],
  };
}

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`promises group (${name})`, () => {
    let mock;
    let promises;

    test.beforeAll(async () => {
      promises = fixture();
      mock = await startMockServer({ promises });
    });
    test.afterAll(async () => { await new Promise(r => mock.server.close(r)); });

    test('lists promises by due time and closes one', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.evaluate(async (mobile) => {
        const col = document.createElement('div');
        col.id = 'pm-test';
        col.style.cssText = 'position:fixed;top:0;left:0;bottom:0;z-index:999;overflow:auto;padding:12px;background:var(--nz-bg-0);width:' + (mobile ? '100%' : '280px');
        document.body.appendChild(col);
        const { mountPromises } = await import('/static/promises.js');
        await mountPromises(col);
      }, name === 'mobile');

      const group = page.locator('#pm-test');
      await expect(group.locator('.pm-head').first()).toHaveText('答应你的 · 3');
      const cards = group.locator(':scope > .pm-card');
      await expect(cards).toHaveCount(3);
      await expect(cards.locator('.pm-text')).toHaveText(['周五前整理 JD 会前材料', '明早提醒你看压测结果', '全部合并后告诉你']);
      await expect(cards.nth(0).locator('.pm-due')).toHaveClass(/overdue/);
      await expect(cards.nth(0).locator('.pm-due')).toContainText('已到期');
      await expect(cards.nth(1)).toContainText('你登记的');
      await expect(cards.nth(2)).toContainText('无到期时间');
      await expect(cards.nth(2)).toContainText('完成条件：review issues 的 PR 全部合并');
      await expect(group.locator('.pm-closed summary')).toHaveText('最近关闭 · 1');
      await expect(group.locator('.pm-closed .pm-close')).toHaveCount(0);
      await page.screenshot({ path: test.info().outputPath(`promises-${name}.png`) });

      await cards.nth(0).locator('.pm-close').click();
      await expect(group.locator('.pm-head').first()).toHaveText('答应你的 · 2');
      await expect(group.locator('.pm-closed summary')).toHaveText('最近关闭 · 2');
      expect(promises.closed[0].id).toBe('pr_overdue');

      await ctx.close();
    });
  });
}
