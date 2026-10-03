// @ts-check
// "加载更早的事件" decides "no more history" from the server's X-Events-Has-More
// header on every `before=` page (#3029). The server fails it open ("1") when
// its disk read errors or the request is cancelled, so an empty page carrying
// "1" is a failed read the operator can retry, not the end of history. Only
// without the header (an older server) is a short page read as exhausted.
//
// 跑法：cd test/e2e && npx playwright test load_earlier_has_more.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const BASE = 1750000000000;

/** n visible bubbles, one per millisecond; the detail carries the index. @param {number} n */
function history(n) {
  return Array.from({ length: n }, (_, i) => ({ type: 'text', detail: `[e${i}]`, time: BASE + i, uuid: 'hm-' + i }));
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {number} n
 * @param {object} [opts] extra startMockServer overrides
 */
async function openSession(browser, n, opts = {}) {
  const mock = await startMockServer({ eventsByKey: { [KEY]: history(n) }, ...opts });
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect(page.locator('#events-scroll > .event')).toHaveCount(100);
  const earlierCalls = () => mock.eventsCalls.filter(q => new URLSearchParams(q).has('before')).length;
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  return { page, mock, earlierCalls, cleanup };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'pagination is viewport-independent; desktop-chrome only');
  }
});

test.describe('加载更早的事件：以 X-Events-Has-More 判定到头', () => {
  test('读失败（空页 + has-more=1）显示重试，再点加载出真实的一页', async ({ browser }) => {
    const { page, earlierCalls, cleanup } = await openSession(browser, 150, { eventsBeforeFailCount: 1 });
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(btn).toHaveText('加载失败 — 点击重试');
      await expect(btn).toBeEnabled();
      expect(earlierCalls()).toBe(1);
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(150);
      await expect(btn).toHaveText('没有更早的事件');
      await expect(page.locator('#events-scroll > .event').first()).toContainText('[e0]');
      expect(earlierCalls()).toBe(2);
    } finally {
      await cleanup();
    }
  });

  test('恰好满页且 has-more=0：直接到头，不再多发一次空请求', async ({ browser }) => {
    const { page, earlierCalls, cleanup } = await openSession(browser, 200);
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(200);
      await expect(btn).toHaveText('没有更早的事件');
      await expect(btn).toBeDisabled();
      expect(earlierCalls()).toBe(1);
    } finally {
      await cleanup();
    }
  });

  test('短页但 has-more=1（降级读出的部分页）：按钮保持可点，继续翻到底', async ({ browser }) => {
    const { page, earlierCalls, cleanup } = await openSession(browser, 150);
    try {
      // The first page carries only the 10 entries just below the cursor.
      let first = true;
      await page.route(u => u.pathname === '/api/sessions/events' && u.searchParams.has('before'), async route => {
        if (!first) return route.fallback();
        first = false;
        const body = JSON.stringify(history(150).slice(40, 50));
        await route.fulfill({ contentType: 'application/json', headers: { 'X-Events-Has-More': '1' }, body });
      });
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(110);
      await expect(btn).toHaveText('加载更早的事件');
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(150);
      await expect(btn).toHaveText('没有更早的事件');
      expect(earlierCalls()).toBe(1); // the routed page never reached the mock
    } finally {
      await cleanup();
    }
  });

  test('服务端不带 header（旧版本）：仍按短页判定到头', async ({ browser }) => {
    const { page, earlierCalls, cleanup } = await openSession(browser, 200, { eventsBeforeLegacy: true });
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(200);
      await expect(btn).toHaveText('加载更早的事件');
      await btn.click();
      await expect(btn).toHaveText('没有更早的事件');
      await expect(page.locator('#events-scroll > .event')).toHaveCount(200);
      expect(earlierCalls()).toBe(2);
    } finally {
      await cleanup();
    }
  });
});
