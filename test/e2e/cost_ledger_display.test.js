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
//     so (累计花费, not 近 30 天花费). The card repaints when the ledger
//     lands, and the ledger is asked once per 30 s.
//   - A cron job's drawer asks the ledger for that job alone and shows its
//     30-day figure. The ledger fetch repaints the timeline itself: the
//     drawer's open-time list refetch is held, so its repaint cannot.
//   - Each daemon card in the 系统 view asks the ledger for its own session
//     key (sys:<name>) and shows that figure; a daemon with no entries, or
//     whose fetch fails, shows no cost row. Its title calls the dropped count
//     ledger-wide (the server reports it whatever the key). Each key is asked
//     once per 30 s.
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
  switch (q.get('session_key')) {
    case 'sys:auto-titler': return { buckets: [{ unit: 'USD', amount: 0.42, entries: 7 }], dropped: 2, basis: { unknown: 1 } };
    case 'sys:say"hi': return { buckets: [{ unit: 'USD', amount: 0.05, entries: 1 }] };
    case 'sys:quiet': return { buckets: [] };
    case 'sys:broken': return null;
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
    expect(title).toContain('账本曾丢弃 2 条');
    expect(title, 'the whole-ledger title need not call the count ledger-wide').not.toContain('整个账本');

    const health = page.locator('.svc-health-line');
    await expect(health.filter({ hasText: '成本账本丢弃 2 条' })).toHaveCount(1);
    await expect(health.filter({ hasText: '1 条未知定价' })).toHaveCount(1);
    await expect(health.filter({ hasText: '1 个进程中断的轮次（按 CLI 实测单价估算）' })).toHaveCount(1);
    expect(mock.costSummaryCalls.some(c => c.group_by === 'unit')).toBe(true);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// renderSystemView repaints the card when refreshCostSummary resolves true
// (a new snapshot); the fetch itself never paints. Time is frozen with
// page.clock, so no poll can repaint the card, and the ledger response is held
// until the session-sum fallback has painted: the switch can only come from
// that repaint. Two more trips into the view at 14 s and 28 s stay inside the
// 30 s TTL and must not ask the ledger again; the trip at 31 s must.
test('the overview card repaints when the ledger lands, and the ledger is asked once per 30 s', async ({ browser }) => {
  const mock = await startMockServer({ costSummary: ledger });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  /** @type {() => void} */
  let release = () => {};
  const held = new Promise((r) => { release = () => r(undefined); });
  await page.route('**/api/cost/summary?group_by=unit*', async (route) => { await held; await route.continue(); });
  const unitCalls = () => mock.costSummaryCalls.filter((c) => c.group_by === 'unit').length;
  try {
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.clock.pauseAt(Date.now() + 1000);
    const ledgerCard = page.locator('.svc-stat-label', { hasText: '近 30 天花费' });
    await page.click('#abnav-system');
    await expect(page.locator('.svc-stat-label', { hasText: '累计花费' }), 'the session-sum fallback paints first').toHaveCount(1);
    release();
    await expect(ledgerCard, 'the fetch resolving must repaint the card').toHaveCount(1);
    for (let i = 0; i < 2; i++) {
      await page.click('#abnav-chat');
      await page.clock.runFor(14000);
      await page.click('#abnav-system');
      await expect(ledgerCard).toHaveCount(1);
    }
    // Issued after the re-entries: once it has landed at the mock, a ledger
    // fetch either re-entry started has landed too.
    await page.evaluate(() => fetch('/api/cost/summary?group_by=probe').then((r) => r.status));
    expect(unitCalls(), 're-entry inside the TTL reuses the snapshot').toBe(1);

    await page.click('#abnav-chat');
    await page.clock.runFor(3000);
    await page.click('#abnav-system');
    await expect.poll(unitCalls, { message: 'past the TTL the ledger is asked again' }).toBe(2);
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
    const row = page.locator('.cj-row[data-cron-id="cron-cost-1"]');
    await row.waitFor();
    // Hold every later list refetch past its 8 s timeout.
    await page.route((u) => u.pathname === '/api/cron' && u.searchParams.has('compact'), () => {});
    await row.click();
    await expect(page.locator('.ct-cost-ledger')).toHaveText('30 天 $3.25', { timeout: 5000 });
    // entries counts ledger records (runs plus session spend outside a run window), not runs.
    await expect(page.locator('.ct-cost-ledger')).toHaveAttribute('title', /^近 30 天账本合计：4 条账本记录（/);
    expect(mock.costSummaryCalls).toContainEqual({ group_by: 'job', job_id: 'cron-cost-1', session_key: '' });
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

const daemons = ['auto-titler', 'quiet', 'broken', 'say"hi'].map((name) => ({ name, enabled: true, tick: 30e9, runs_total: 3 }));

test('daemon cards show their own 30-day figure; no entries or a failed fetch shows none', async ({ browser }) => {
  const mock = await startMockServer({ costSummary: ledger, systemDaemons: daemons });
  const { ctx, page, pageErrors } = await open(browser, mock);
  try {
    await page.click('#abnav-system');
    const card = (/** @type {string} */ name) => page.locator('.sys-card').filter({ has: page.locator('.sys-name', { hasText: name }) });
    const cost = card('auto-titler').locator('.sys-cost');
    await expect(cost).toHaveText('近 30 天花费 $0.42', { timeout: 8000 });
    const title = await cost.getAttribute('title');
    expect(title).toContain('仅 auto-titler');
    expect(title).toContain('CLI 估算口径');
    expect(title).toContain('1 条未知定价');
    // The server's dropped count is ledger-wide whatever the session_key.
    expect(title).toContain('整个账本曾丢弃 2 条');
    expect(await card('say"hi').locator('.sys-cost').getAttribute('title'), 'a quote in the name stays inside the attribute').toContain('仅 say"hi');
    // The overview card keeps the whole-ledger figure: the keys do not share a snapshot.
    await expect(page.locator('.svc-stat-value').filter({ hasText: '$12.50' })).toHaveCount(1);
    await expect.poll(() => mock.costSummaryCalls.filter((c) => c.session_key).length).toBe(4);
    for (const name of ['auto-titler', 'quiet', 'broken', 'say"hi']) {
      expect(mock.costSummaryCalls).toContainEqual({ group_by: 'unit', job_id: '', session_key: 'sys:' + name });
    }
    await expect(card('quiet')).toHaveCount(1);
    await expect(card('quiet').locator('.sys-cost'), 'no entries, no cost row').toHaveCount(0);
    await expect(card('broken').locator('.sys-cost'), 'a failed fetch shows nothing').toHaveCount(0);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// Time is frozen and every daemon-list refetch after the load-time one is
// held, so neither the 5 s poll nor the view's open-time refetch can repaint:
// the cost row appearing is the ledger fetch's own repaint. Re-entries at 14 s
// and 28 s reuse the snapshot; the one at 31 s asks again.
test('a daemon card repaints when its ledger figure lands, and asks once per 30 s', async ({ browser }) => {
  const failed = { ...daemons[0], last_run: { state: 'failed' } };
  const mock = await startMockServer({ costSummary: ledger, systemDaemons: [failed] });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  const daemonCalls = () => mock.costSummaryCalls.filter((c) => c.session_key === 'sys:auto-titler').length;
  try {
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    // The badge lights once the load-time daemon list has landed.
    await expect(page.locator('#abnav-system-badge')).toBeVisible();
    await page.route('**/api/system/daemons', () => {});
    await page.clock.pauseAt(Date.now() + 1000);
    const cost = page.locator('.sys-card .sys-cost');
    await page.click('#abnav-system');
    await expect(cost, 'the fetch resolving must repaint the card').toHaveText('近 30 天花费 $0.42');
    for (let i = 0; i < 2; i++) {
      await page.click('#abnav-chat');
      await page.clock.runFor(14000);
      await page.click('#abnav-system');
      await expect(cost).toHaveCount(1);
    }
    await page.evaluate(() => fetch('/api/cost/summary?group_by=probe').then((r) => r.status));
    expect(daemonCalls(), 're-entry inside the TTL reuses the snapshot').toBe(1);

    await page.click('#abnav-chat');
    await page.clock.runFor(3000);
    await page.click('#abnav-system');
    await expect.poll(daemonCalls, { message: 'past the TTL the daemon key is asked again' }).toBe(2);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
