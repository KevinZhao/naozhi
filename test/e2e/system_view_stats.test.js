// @ts-check
//
// The 系统 view's per-run stat chips for attachment-gc: the dry-run Counts
// keys get Chinese labels, *_bytes renders as a size (0 included), and the
// dry_run flag reads as 是 rather than a bare 1; a live tick's buckets read
// 已回收 instead of 可回收.
//
// 跑法：cd test/e2e && npx playwright test system_view_stats.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'view rendering; desktop-chrome only');
  }
});

/** @param {Record<string, number>} stats */
function gcDaemon(stats) {
  return {
    name: 'attachment-gc', enabled: true, tick: 6 * 3600e9, runs_total: 1,
    last_run: { state: 'succeeded', trigger: 'tick', duration_ms: 40, ended_at: new Date().toISOString(), stats },
  };
}

/** @param {import('@playwright/test').Browser} browser @param {Record<string, number>} stats */
async function chips(browser, stats) {
  const mock = await startMockServer({ systemDaemons: [gcDaemon(stats)] });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-system');
    await expect(page.locator('#system-main .sys-stat').first()).toBeVisible();
    return await page.locator('#system-main .sys-stat').allInnerTexts();
  } finally {
    await ctx.close();
    mock.server.close();
  }
}

test('attachment-gc dry-run chips: labelled buckets, sized bytes, mode flag', async ({ browser }) => {
  const got = await chips(browser, {
    examined: 2, dry_run: 1, would_reap_legacy_no_meta: 3, would_reap_meta_no_refs: 4,
    would_reap_refs_expired: 5, would_reap_bytes: 3 * (1 << 20),
  });
  expect(got).toEqual([
    '检查 2', '演练模式 是', '可回收·无meta旧文件 3', '可回收·无引用(高风险) 4',
    '可回收·引用过期 5', '可回收体积 3 MiB',
  ]);
});

test('attachment-gc dry-run with nothing to reclaim shows 0 B', async ({ browser }) => {
  const got = await chips(browser, { examined: 1, dry_run: 1, would_reap_bytes: 0 });
  expect(got).toEqual(['检查 1', '演练模式 是', '可回收体积 0 B']);
});

test('attachment-gc live tick labels the same buckets as reclaimed', async ({ browser }) => {
  const got = await chips(browser, {
    examined: 1, acted: 2, would_reap_refs_expired: 2, would_reap_bytes: 2048,
  });
  expect(got).toEqual(['检查 1', '执行 2', '已回收·引用过期 2', '已回收体积 2 KiB']);
});
