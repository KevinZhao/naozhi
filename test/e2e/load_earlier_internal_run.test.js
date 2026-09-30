// @ts-check
// "加载更早的事件" across a long internal-only stretch. A parallel agent team
// leaves thousands of consecutive task_progress / tool_use entries (measured:
// 3187 in one claude session's event log), so a `before=` page of
// EARLIER_PAGE_LIMIT can be entirely INTERNAL_EVENT_TYPES and render nothing.
//
// The bug: loadEarlierEvents took its cursor from the first rendered `.event`,
// which an all-internal page never moves — the click looked dead, and every
// further click re-fetched the same page. The cursor now follows the oldest
// fetched event, and one click walks all-internal pages (bounded by
// EARLIER_SKIP_MAX_PAGES) until a visible bubble lands.
//
// 跑法：cd test/e2e && npx playwright test load_earlier_internal_run.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PAGE = 100;      // EARLIER_PAGE_LIMIT / INITIAL_HISTORY_LIMIT
const SKIP_MAX = 10;   // EARLIER_SKIP_MAX_PAGES
const BASE = 1750000000000;

/** @param {number} n @param {number} from */
function internalRun(n, from) {
  return Array.from({ length: n }, (_, i) => ({
    type: 'task_progress',
    detail: 'progress ' + i,
    time: BASE + from + i,
    uuid: 'int-' + (from + i),
  }));
}

/** @param {number} n @param {number} from */
function visibleRun(n, from) {
  return Array.from({ length: n }, (_, i) => ({
    type: 'text',
    detail: 'recent ' + i,
    time: BASE + from + i,
    uuid: 'vis-' + (from + i),
  }));
}

/**
 * The oldest entry is the bubble the operator is paging toward; `internal`
 * entries sit between it and a first page of PAGE visible bubbles (so the
 * blank-page auto recovery never fires and only the click is under test).
 * @param {import('@playwright/test').Browser} browser
 * @param {number} internal
 */
async function openSession(browser, internal) {
  const events = [
    { type: 'text', detail: 'the old answer', time: BASE, uuid: 'old' },
    ...internalRun(internal, 10),
    ...visibleRun(PAGE, 10 + internal + 10),
  ];
  const mock = await startMockServer({ eventsByKey: { [KEY]: events } });
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {number[]} */
  const cursors = [];
  page.on('request', r => {
    const u = new URL(r.url());
    const b = u.searchParams.get('before');
    if (u.pathname === '/api/sessions/events' && b) cursors.push(Number(b));
  });
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect(page.locator('#events-scroll .event')).toHaveCount(PAGE);
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  return { page, cursors, cleanup };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'pagination is viewport-independent; desktop-chrome only');
  }
});

test.describe('加载更早的事件：跨越整页内部事件', () => {
  test('一次点击穿过全内部事件页，载入更早的可见气泡', async ({ browser }) => {
    const { page, cursors, cleanup } = await openSession(browser, PAGE * 3 + 50);
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      await expect(page.locator('#events-scroll')).toContainText('the old answer');
      await expect(page.locator('#events-scroll .event')).toHaveCount(PAGE + 1);
      // Four pages: three all-internal, then the one carrying the old answer
      // (short, so the history is exhausted).
      expect(cursors).toHaveLength(4);
      // Every request moved the cursor strictly backward — the old bug sent
      // the same `before=` on each click.
      for (let i = 1; i < cursors.length; i++) expect(cursors[i]).toBeLessThan(cursors[i - 1]);
      await expect(btn).toHaveText('没有更早的事件');
    } finally {
      await cleanup();
    }
  });

  test('单次点击的翻页数有上限，下一次点击从推进后的游标继续', async ({ browser }) => {
    const { page, cursors, cleanup } = await openSession(browser, PAGE * (SKIP_MAX + 5));
    try {
      const btn = page.locator('#earlier-events-btn');
      await btn.click();
      // The first click stops at the budget, leaving the button usable.
      await expect(btn).toHaveText('加载更早的事件');
      expect(cursors).toHaveLength(SKIP_MAX);
      await expect(page.locator('#events-scroll')).not.toContainText('the old answer');

      await btn.click();
      await expect(page.locator('#events-scroll')).toContainText('the old answer');
      expect(cursors[SKIP_MAX], 'the second click resumes below the first click\'s floor')
        .toBeLessThan(cursors[SKIP_MAX - 1]);
    } finally {
      await cleanup();
    }
  });
});
