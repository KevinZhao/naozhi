// @ts-check
//
// Today's spend against the cost.budget cap (GET /api/cost/budget), shown next
// to the figures the dashboard already has:
//
//   - The session header's run stats end with "今日 $x / $y" for the cap the
//     IM gate would check for that key; ⚠ from warn_ratio on, red once over,
//     and the title names the scope and the reset time. It shows even before
//     the session has a run.
//   - A cron job's timeline head shows the job's figure beside its 30-day
//     ledger total.
//   - No chip when no cap applies (limit 0) or the endpoint fails.
//
// 跑法：cd test/e2e && npx playwright test cost_budget_chip.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

const KEY_OVER = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_NONE = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const RESET = '2026-09-07T00:00:00+08:00';

/** @param {URLSearchParams} q */
function budget(q) {
  if (q.get('job_id') === 'cron-budget-1') {
    return { enabled: true, scope: 'job', subject: 'cron-budget-1', spent: 1.6, limit: 2, warn: true, over: false, blocked: false, day: '2026-09-06', reset_at: RESET };
  }
  if (q.get('session_key') === KEY_OVER) {
    return { enabled: true, scope: 'global', spent: 5.5, limit: 5, warn: true, over: true, blocked: true, day: '2026-09-06', reset_at: RESET };
  }
  return { enabled: true, spent: 0, limit: 0, warn: false, over: false, blocked: false };
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

const oneRun = {
  runs: [{ run_id: 'r1', subsystem: 'session', started_at: Date.now() - 60000, duration_ms: 1200, state: 'succeeded' }],
  stats: { count: 1, total_ms: 1200 },
};

test('session header: the budget chip follows the run stats, flagged and titled', async ({ browser }) => {
  const mock = await startMockServer({
    costBudget: budget,
    sessionRuns: {
      [KEY_OVER]: oneRun,
      [KEY_NONE]: oneRun,
    },
  });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.locator(`.session-card[data-key="${KEY_OVER}"]`).click();
    const chip = page.locator('#header-runstats .srp-stat', { hasText: '今日' });
    await expect(chip).toHaveText('⚠ 今日 $5.50 / $5.00', { timeout: 5000 });
    await expect(chip).toHaveClass(/\bbad\b/);
    const title = await chip.getAttribute('title');
    expect(title).toContain('整机今日费用预算');
    expect(title).toContain('重置');
    expect(title).toContain('已用尽');
    expect(title).toContain('dashboard 不受限');
    await expect(page.locator('#header-runstats')).toContainText('1 轮');
    expect(mock.costBudgetCalls).toContainEqual({ job_id: '', session_key: KEY_OVER });

    // limit 0: no cap covers this key, so the stats paint without a chip.
    await page.locator(`.session-card[data-key="${KEY_NONE}"]`).click();
    await expect.poll(() => mock.costBudgetCalls.some((c) => c.session_key === KEY_NONE)).toBe(true);
    await expect(page.locator('#header-runstats .srp-stat').first()).toHaveText('1 轮');
    await expect(page.locator('#header-runstats')).not.toContainText('今日');
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('session header: a session with no runs yet still shows its budget', async ({ browser }) => {
  const mock = await startMockServer({
    costBudget: budget,
    sessionRuns: { [KEY_OVER]: { runs: [], stats: { count: 0 } } },
  });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.locator(`.session-card[data-key="${KEY_OVER}"]`).click();
    await expect(page.locator('#header-runstats')).toHaveText('⚠ 今日 $5.50 / $5.00', { timeout: 5000 });
    await expect(page.locator('#session-runs-panel')).toBeHidden();
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('cron drawer: the job budget sits beside the 30-day ledger figure', async ({ browser }) => {
  const now = Date.now();
  const mock = await startMockServer({
    costBudget: budget,
    costSummary: () => ({ buckets: [{ unit: 'USD', amount: 3.25, entries: 4 }] }),
    cronJobs: [{
      id: 'cron-budget-1', schedule: '0 6 * * *', prompt: 'nightly report', work_dir: '/home/user/workspace/myproject',
      paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: [], stats: { total: 4, succeeded: 4 },
    }],
  });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.click('#abnav-cron');
    const row = page.locator('.cj-row[data-cron-id="cron-budget-1"]');
    await row.waitFor();
    // Hold every later list refetch past its 8 s timeout.
    await page.route((u) => u.pathname === '/api/cron' && u.searchParams.has('compact'), () => {});
    await row.click();
    const chips = page.locator('.ct-cost-ledger');
    await expect(chips).toHaveText(['30 天 $3.25', '⚠ 今日 $1.60 / $2.00'], { timeout: 5000 });
    await expect(chips.nth(1)).not.toHaveClass(/\bbad\b/);
    await expect(chips.nth(1)).toHaveAttribute('title', /^本任务今日费用预算（cost\.budget）已用 \$1\.60 \/ \$2\.00，.* 重置$/);
    expect(mock.costBudgetCalls).toContainEqual({ job_id: 'cron-budget-1', session_key: '' });
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
