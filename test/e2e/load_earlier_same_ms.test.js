// @ts-check
// "加载更早的事件" across a page edge that falls inside one millisecond (#3030).
// `before=` is strict on the server (the mock mirrors it), so a cursor of the
// oldest held entry's ms used to skip that ms's siblings the page did not
// carry, for good. The dashboard now sends before=ms+1 and drops the entries
// it already holds at that ms, by uuid or, without one, by time|type|detail.
//
// 跑法：cd test/e2e && npx playwright test load_earlier_same_ms.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const BASE = 1750000000000;
const GROUP = 3; // entries per millisecond

/**
 * n text entries, GROUP to a millisecond; the detail carries the index so the
 * rendered pane can be read back. Indexes in `noUUID` carry no uuid.
 * @param {number} n @param {number[]} [noUUID]
 */
function sameMsHistory(n, noUUID = []) {
  return Array.from({ length: n }, (_, i) => {
    /** @type {{type: string, detail: string, time: number, uuid?: string}} */
    const e = { type: 'text', detail: `[e${i}]`, time: BASE + Math.floor(i / GROUP) };
    if (!noUUID.includes(i)) e.uuid = 'sm-' + i;
    return e;
  });
}

/** @param {import('@playwright/test').Page} page @returns {Promise<number[]>} */
async function renderedIndexes(page) {
  const texts = await page.locator('#events-scroll > .event').allTextContents();
  return texts.flatMap(t => [...t.matchAll(/\[e(\d+)\]/g)].map(m => Number(m[1])));
}

/** @param {number[]} got @param {number} from @param {number} to */
function expectEachOnce(got, from, to) {
  /** @type {Map<number, number>} */
  const counts = new Map();
  for (const i of got) counts.set(i, (counts.get(i) || 0) + 1);
  const bad = [];
  for (let i = from; i <= to; i++) if (counts.get(i) !== 1) bad.push(`e${i}x${counts.get(i) || 0}`);
  expect(bad, 'every entry renders exactly once').toEqual([]);
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {object[]} events
 */
async function openSession(browser, events) {
  const mock = await startMockServer({ eventsByKey: { [KEY]: events } });
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {URLSearchParams[]} */
  const earlier = [];
  page.on('request', r => {
    const u = new URL(r.url());
    if (u.pathname === '/api/sessions/events' && u.searchParams.get('before')) earlier.push(u.searchParams);
  });
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect(page.locator('#events-scroll > .event')).toHaveCount(100);
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  return { page, earlier, cleanup };
}

/**
 * 520 live bubbles through appendEvents (the poll path) take a 100-bubble pane
 * to 620, so trimEventsScroll evicts its 20 oldest.
 * @param {import('@playwright/test').Page} page
 */
async function pushLiveToTrim(page) {
  await page.evaluate((base) => {
    const w = /** @type {any} */ (window);
    w.appendEvents(Array.from({ length: 520 }, (_, i) => ({
      type: 'text', detail: `[live${i}]`, time: base + 1000 + i, uuid: 'live-' + i,
    })));
  }, BASE);
  await expect(page.locator('#events-scroll > .event')).toHaveCount(600);
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'pagination is viewport-independent; desktop-chrome only');
  }
});

test.describe('加载更早的事件：页边界切在同一毫秒内', () => {
  test('首页边界与翻页边界都切开同毫秒组：翻到底每条恰好一次', async ({ browser }) => {
    // 252 entries: the initial 100-entry page reaches back to e152, the last of
    // {e150,e151,e152}; the first earlier page then reaches back to e52, the
    // 2nd of {e51,e52,e53}. e152 and e53 are held at a cursor ms with no uuid.
    const N = 252;
    const { page, earlier, cleanup } = await openSession(browser, sameMsHistory(N, [152, 53]));
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(200);
      await btn.click();
      await expect(btn).toHaveText('没有更早的事件');
      expectEachOnce(await renderedIndexes(page), 0, N - 1);
      await expect(page.locator('#events-scroll > .event')).toHaveCount(N);
      // The cursor ms is re-admitted, and the page widened by the held keys.
      expect(earlier.map(p => [Number(p.get('before')) - BASE, Number(p.get('limit'))]))
        .toEqual([[51, 101], [18, 102]]);
    } finally {
      await cleanup();
    }
  });

  test('DOM 上限裁掉同毫秒组的前半：加载更早时被裁的兄弟恰好回来一次', async ({ browser }) => {
    // The initial page is e600..e699; the trim evicts e600..e619 and leaves
    // e620 alone at its ms (e618 and e619 share it). The next page must bring
    // both back, and not e620.
    const { page, cleanup } = await openSession(browser, sameMsHistory(700));
    try {
      await pushLiveToTrim(page);
      expect((await renderedIndexes(page))[0]).toBe(620);
      await page.locator('#earlier-events-btn').click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(700);
      expectEachOnce(await renderedIndexes(page), 520, 699);
    } finally {
      await cleanup();
    }
  });

  test('裁剪后的头部气泡没有 uuid：该毫秒整体视为已持有，不重复渲染', async ({ browser }) => {
    // Same trim as above, but e620 has no uuid, so the DOM cannot key it: the
    // cursor steps past its ms rather than risk painting it twice.
    const { page, earlier, cleanup } = await openSession(browser, sameMsHistory(700, [620]));
    try {
      await pushLiveToTrim(page);
      await page.locator('#earlier-events-btn').click();
      await expect(page.locator('#events-scroll > .event')).toHaveCount(700);
      const got = await renderedIndexes(page);
      expect(got.filter(i => i === 620)).toHaveLength(1);
      expect(Number(earlier[0].get('before')) - BASE, 'strict at e620\'s ms').toBe(Math.floor(620 / GROUP));
    } finally {
      await cleanup();
    }
  });

  test('同毫秒组超过一页上限：按钮不卡住，越过该毫秒继续翻到更早的消息', async ({ browser }) => {
    // e0..e4 at their own ms, then 600 entries at one ms. Each click widens the
    // page by the held keys until the 500 cap, where a page holds nothing new;
    // the cursor then steps past that ms (dropping the 100 entries no page can
    // reach) instead of re-requesting the same page.
    const events = [
      ...Array.from({ length: 5 }, (_, i) => ({ type: 'text', detail: `[e${i}]`, time: BASE + i, uuid: 'sm-' + i })),
      ...Array.from({ length: 600 }, (_, i) => ({ type: 'text', detail: `[e${5 + i}]`, time: BASE + 10, uuid: 'sm-' + (5 + i) })),
    ];
    const { page, earlier, cleanup } = await openSession(browser, events);
    try {
      const btn = page.locator('#earlier-events-btn');
      for (let shown = 200; shown <= 500; shown += 100) {
        await btn.click();
        await expect(page.locator('#events-scroll > .event')).toHaveCount(shown);
      }
      await btn.click();
      await expect(btn).toHaveText('没有更早的事件');
      const got = await renderedIndexes(page);
      expect(got.slice(0, 5)).toEqual([0, 1, 2, 3, 4]);
      expectEachOnce(got, 105, 604);
      expect(got).toHaveLength(505);
      expect(earlier.slice(-3).map(p => [Number(p.get('before')) - BASE, Number(p.get('limit'))]))
        .toEqual([[11, 500], [11, 500], [10, 100]]);
    } finally {
      await cleanup();
    }
  });

  test('服务端忽略 before：一次点击至多两次请求，不重复渲染', async ({ browser }) => {
    // A full page of nothing new also comes from a server that ignores the
    // cursor and keeps serving the tail. The step past the cursor ms happens
    // once a click; a second empty full page stops instead of walking back
    // one ms per request.
    const history = sameMsHistory(200);
    const { page, earlier, cleanup } = await openSession(browser, history);
    try {
      // The tail plus a few newer entries: a page at least as long as the request.
      const newer = Array.from({ length: 5 }, (_, i) => ({ type: 'text', detail: '[newer]', time: BASE + 999 + i, uuid: 'newer-' + i }));
      const tail = [...history.slice(-100), ...newer];
      await page.route(u => u.pathname === '/api/sessions/events' && u.searchParams.has('before'),
        route => route.fulfill({ contentType: 'application/json', body: JSON.stringify(tail) }));
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(btn).toHaveText('加载更早的事件');
      expect(earlier).toHaveLength(2);
      await expect(page.locator('#events-scroll > .event')).toHaveCount(100);
    } finally {
      await cleanup();
    }
  });
});
