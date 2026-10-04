// @ts-check
// The markdown export pager decides "history complete" from X-Events-Has-More
// on every `before=` page (#3181), like the load-earlier button (#3029). The
// server fails the header open ("1") when its disk read errors or the request
// is cancelled, so a page that brings nothing new but says has-more is a cut
// export the toast must warn about. has-more=0 ends the walk without an extra
// empty request; only without the header (an older server) does the pager
// fall back to the full-page heuristic.
//
// 跑法：cd test/e2e && npx playwright test export_has_more.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const fs = require('fs');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const BASE = 1750000000000;
const TRUNCATED = '已截断';

/** n events, one per millisecond; the detail carries the index. @param {number} n */
function history(n) {
  return Array.from({ length: n }, (_, i) => ({ type: 'text', detail: `[e${i}]`, time: BASE + i, uuid: 'xh-' + i }));
}

/**
 * Opens the session, clicks export and returns the downloaded markdown, the
 * toast text and how many `before=` requests the export sent.
 * @param {import('@playwright/test').Browser} browser
 * @param {number} n
 * @param {object} [opts] extra startMockServer overrides
 * @param {(page: import('@playwright/test').Page) => Promise<void>} [setup] runs before the click
 */
async function exportSession(browser, n, opts = {}, setup) {
  const mock = await startMockServer({ eventsByKey: { [KEY]: history(n) }, eventsRingSize: 500, ...opts });
  const ctx = await browser.newContext({ ...desktop, acceptDownloads: true });
  try {
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${KEY}"]`);
    await page.waitForSelector('#events-scroll .event');
    const beforeCalls = () => mock.eventsCalls.filter(q => new URLSearchParams(q).has('before')).length;
    const callsAtClick = beforeCalls();
    if (setup) await setup(page);
    const dl = page.waitForEvent('download');
    await page.click('.btn-download');
    const md = fs.readFileSync(await (await dl).path(), 'utf8');
    const toast = page.locator('#toast');
    await expect(toast).toContainText('已导出');
    return {
      exported: (md.match(/^## /gm) || []).length,
      md,
      toast: (await toast.textContent()) || '',
      exportBeforeCalls: beforeCalls() - callsAtClick,
    };
  } finally {
    await ctx.close();
    mock.server.close();
  }
}

/**
 * Answers the first `before=` request with `entries` and the given header.
 * @param {object[]} entries
 * @param {string|null} hasMore null omits X-Events-Has-More
 */
function routeFirstBeforePage(entries, hasMore) {
  return async (/** @type {import('@playwright/test').Page} */ page) => {
    let first = true;
    await page.route(u => u.pathname === '/api/sessions/events' && u.searchParams.has('before'), async route => {
      if (!first) return route.fallback();
      first = false;
      const headers = hasMore === null ? {} : { 'X-Events-Has-More': hasMore };
      await route.fulfill({ contentType: 'application/json', headers, body: JSON.stringify(entries) });
    });
  };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'pagination is viewport-independent; desktop-chrome only');
  }
});

test.describe('导出会话：以 X-Events-Has-More 判定历史是否完整', () => {
  test('降级读（空页 + has-more=1）：提示已截断，不冒充完整导出', async ({ browser }) => {
    const r = await exportSession(browser, 700, { eventsBeforeFailCount: 1 });
    expect(r.exported).toBe(500);
    expect(r.toast).toContain(TRUNCATED);
  });

  test('降级读（只回放游标毫秒的已持有事件 + has-more=1）：提示已截断', async ({ browser }) => {
    // The ring holds 200..699; a degraded read under `before=oldest+1` returns
    // only the held entry at the cursor millisecond.
    const r = await exportSession(browser, 700, {}, routeFirstBeforePage(history(700).slice(200, 201), '1'));
    expect(r.exported).toBe(500);
    expect(r.toast).toContain(TRUNCATED);
  });

  test('满页且 has-more=0：导出完整，不再多发一次空请求', async ({ browser }) => {
    // The ring holds 499..998; `before=oldest+1` reaches 0..499, exactly a page.
    const r = await exportSession(browser, 999);
    expect(r.exported).toBe(999);
    expect(r.md).toContain('[e0]');
    expect(r.toast).not.toContain(TRUNCATED);
    expect(r.exportBeforeCalls).toBe(1);
  });

  test('短会话（空页 + has-more=0）：导出完整，不提示截断', async ({ browser }) => {
    const r = await exportSession(browser, 30);
    expect(r.exported).toBe(30);
    expect(r.toast).not.toContain(TRUNCATED);
  });

  test('满页全是已持有事件但 has-more=0：导出完整，不提示截断', async ({ browser }) => {
    // A same-ms flood wider than a page is complete once the server says so.
    const r = await exportSession(browser, 700, {}, routeFirstBeforePage(history(700).slice(200), '0'));
    expect(r.exported).toBe(500);
    expect(r.toast).not.toContain(TRUNCATED);
  });

  test('短页但 has-more=1（部分页）：继续翻页直到完整', async ({ browser }) => {
    const r = await exportSession(browser, 700, {}, routeFirstBeforePage(history(700).slice(190, 200), '1'));
    expect(r.exported).toBe(700);
    expect(r.md).toContain('[e0]');
    expect(r.toast).not.toContain(TRUNCATED);
  });

  test('服务端不带 header（旧版本）：短页读作到头，导出完整', async ({ browser }) => {
    const r = await exportSession(browser, 700, { eventsBeforeLegacy: true });
    expect(r.exported).toBe(700);
    expect(r.toast).not.toContain(TRUNCATED);
    // 200 older entries, then the held cursor-ms entry alone: short, nothing new.
    expect(r.exportBeforeCalls).toBe(2);
  });

  test('服务端不带 header（旧版本）：满页全是已持有事件仍提示已截断', async ({ browser }) => {
    // Indistinguishable from a same-ms flood wider than a page without the header.
    const r = await exportSession(browser, 700, {}, routeFirstBeforePage(history(700).slice(200), null));
    expect(r.exported).toBe(500);
    expect(r.toast).toContain(TRUNCATED);
  });
});
