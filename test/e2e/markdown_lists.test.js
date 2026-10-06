// @ts-check
//
// Nested-list CSS for renderMd output: a list nested in a cron run's final
// reply keeps the compact indent. Needs computed styles, so it runs in the
// browser; renderMd's string-level list cases are in
// scripts/render-md-cases.test.mjs.
//
// 跑法：cd test/e2e && npx playwright test markdown_lists.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd list 渲染', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  /** @type {import('@playwright/test').Page} */
  let page;

  test.beforeAll(async ({ browser }) => {
    mock = await startMockServer();
    const ctx = await browser.newContext();
    page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => typeof (/** @type {any} */ (window)).renderMd === 'function');
  });
  test.afterAll(async () => {
    await page.context().close();
    mock.server.close();
  });

  test('CSS：嵌套 list 在 .ctr-final-body.md 容器下拿到紧凑 16px 缩进', async () => {
    const m = await page.evaluate(() => {
      const w = /** @type {any} */ (window);
      const html = w.renderMd('1. 父项\n   - 子项\n');
      // 与 cron_timeline.js 展开 run 详情时的结构一致。
      const host = document.createElement('div');
      host.className = 'ctr-detail';
      const finalEl = document.createElement('div');
      finalEl.className = 'ctr-final';
      const body = document.createElement('div');
      body.className = 'ctr-final-body md';
      body.innerHTML = html;
      finalEl.appendChild(body);
      host.appendChild(finalEl);
      document.body.appendChild(host);
      const innerUl = body.querySelector('.md-ol > li > .md-ul');
      const cs = innerUl ? getComputedStyle(innerUl) : null;
      const result = {
        found: !!innerUl,
        marginLeft: cs ? cs.marginLeft : null,
        listStyle: cs ? cs.listStyleType : null,
      };
      host.remove();
      return result;
    });
    expect(m.found).toBe(true);
    // 期望 split_view.css 的嵌套紧凑规则生效，不被 .ctr-final-body.md 下的 ul/ol 覆盖压过
    expect(m.marginLeft).toBe('16px');
    expect(m.listStyle).toBe('circle');
  });
});
