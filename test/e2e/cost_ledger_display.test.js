// @ts-check
//
// Where the cost ledger shows up in the dashboard (docs/rfc/cost-ledger.md §8),
// driven against a mock /api/cost/summary:
//
//   - The 系统 view's overview card shows the ledger's 30-day USD figure. It
//     shows credits on their own sub-line: the two units never sum. A ⚠ flag
//     and a title say when the figure deserves less trust (entries dropped,
//     unknown pricing), and the health strip spells out dropped, unknown and
//     partial turns.
//   - Without the ledger the card falls back to the session-list sum and says
//     so (累计花费, not 近 30 天花费).
//   - A cron job's drawer asks the ledger for that job alone and shows its
//     30-day figure.
//
// 跑法：cd test/e2e && npx playwright test cost_ledger_display.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

/** @param {URLSearchParams} q */
function ledger(q) {
  if (q.get('group_by') === 'job') {
    return { buckets: [{ unit: 'USD', amount: 3.25, entries: 4 }] };
  }
  return {
    buckets: [
      { unit: 'USD', amount: 12.5, entries: 9 },
      { unit: 'credits', amount: 4, entries: 2 },
    ],
    dropped: 2,
    basis: { unknown: 1 },
    kinds: { partial: 1 },
  };
}

/** @param {import('@playwright/test').Browser} browser @param {any} mock */
async function open(browser, mock) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  return { ctx, page, pageErrors };
}

test('overview card: ledger USD, credits on their own line, trust flags and health lines', async ({ browser }) => {
  const mock = await startMockServer({ costSummary: ledger });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.click('#abnav-system');
    const card = page.locator('.svc-stat').filter({ has: page.locator('.svc-stat-label', { hasText: '近 30 天花费' }) });
    await expect(card, 'the card must switch to the ledger figure once it loads').toHaveCount(1, { timeout: 8000 });
    await expect(card.locator('.svc-stat-value')).toContainText('$12.50');
    await expect(card.locator('.svc-stat-value')).not.toContainText('16.5');
    await expect(card.locator('.svc-stat-sub')).toHaveText('4.00 credits');
    await expect(card.locator('.svc-stat-flag')).toHaveCount(1);
    const title = await card.getAttribute('title');
    expect(title).toContain('CLI 估算口径');
    expect(title).toContain('1 条未知定价');
    expect(title).toContain('丢弃 2 条');

    const health = page.locator('.svc-health-line');
    await expect(health.filter({ hasText: '成本账本丢弃 2 条' })).toHaveCount(1);
    await expect(health.filter({ hasText: '1 条未知定价' })).toHaveCount(1);
    await expect(health.filter({ hasText: '1 个进程中断的轮次' })).toHaveCount(1);
    expect(mock.costSummaryCalls.some(c => c.group_by === 'unit')).toBe(true);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('overview card without a ledger falls back to the session-list sum', async ({ browser }) => {
  const mock = await startMockServer({ costSummary: () => null });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.click('#abnav-system');
    await expect.poll(() => mock.costSummaryCalls.length, { timeout: 5000 }).toBeGreaterThan(0);
    await expect(page.locator('.svc-stat-label', { hasText: '累计花费' })).toHaveCount(1);
    await expect(page.locator('.svc-stat-label', { hasText: '近 30 天花费' })).toHaveCount(0);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('cron drawer asks the ledger for its job and shows the 30-day figure', async ({ browser }) => {
  const now = Date.now();
  const mock = await startMockServer({
    costSummary: ledger,
    cronJobs: [{
      id: 'cron-cost-1', schedule: '0 6 * * *', prompt: 'nightly report', work_dir: '/home/user/workspace/myproject',
      paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: [], stats: { total: 4, succeeded: 4 },
    }],
  });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.click('#abnav-cron');
    await page.locator('.cj-row[data-cron-id="cron-cost-1"]').click();
    await expect(page.locator('.ct-cost-ledger')).toHaveText('30 天 $3.25', { timeout: 8000 });
    expect(mock.costSummaryCalls).toContainEqual({ group_by: 'job', job_id: 'cron-cost-1' });
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
