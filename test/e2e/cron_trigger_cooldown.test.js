// @ts-check
//
// The drawer's "▷ 立即执行" button is a small state machine (RFC §4.3.1):
// normal → sending (≤1s, spinner) → sent (1-3s, ✓) → quiet hold (3-10s) →
// normal, with paused and running taking precedence. The cooldown exists
// because an operator reflexively clicks again during the round trip, and
// without it a job double-fires.
//
// Time is driven with page.clock, so each phase is asserted at a known
// instant rather than raced:
//
//   - a click locks the button synchronously and sends exactly one POST, and
//     choosing 立即运行 from the row menu twice inside the window fires once;
//   - the label walks sending → sent → back to normal at 10s;
//   - a failed trigger releases the lock at once, so a retry does not wait
//     out the 10s;
//   - a run_started frame replaces the optimistic lock with the running state,
//     so a run that ends inside the window leaves the button ready;
//   - under prefers-reduced-motion the sending spinner does not animate.
//
// 跑法：cd test/e2e && npx playwright test cron_trigger_cooldown.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const JOB = 'cron-trig-1';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

function job() {
  const now = Date.now();
  return {
    id: JOB, schedule: '0 6 * * *', prompt: 'nightly report', work_dir: '/home/user/workspace/myproject',
    paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  };
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {object} overrides
 * @param {{ reducedMotion?: 'reduce' }} [opts]
 */
async function openDrawer(browser, overrides, opts = {}) {
  const mock = await startMockServer({ cronJobs: [job()], ...overrides });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 }, reducedMotion: opts.reducedMotion });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.clock.install();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.locator(`.cj-row[data-cron-id="${JOB}"]`).click();
  const btn = page.locator('#cron-detail-pane .cron-drawer-actions .cda-btn.primary');
  await expect(btn).toHaveText('▷ 立即执行');
  // Freeze time from here on; every phase below is reached by advancing it.
  await page.clock.pauseAt(Date.now() + 1000);
  return { mock, ctx, page, btn, pageErrors };
}

test('click locks at once, sends one POST, walks sending → sent → normal at 10s', async ({ browser }) => {
  const { mock, ctx, page, btn, pageErrors } = await openDrawer(browser, { cronTrigger: {} });
  try {
    await btn.click();
    await expect(btn).toHaveText('▷ 触发中…');
    await expect(btn).toHaveClass(/is-sending/);
    await expect(btn).toBeDisabled();
    await expect.poll(() => mock.cronTriggerCalls.length).toBe(1);
    expect(mock.cronTriggerCalls[0]).toEqual({ id: JOB });

    await page.clock.runFor(1200);
    await expect(btn).toHaveText('▷ 已派发 ✓');
    await expect(btn).toHaveClass(/is-sent/);
    await expect(btn).toBeDisabled();

    await page.clock.runFor(6000); // t ≈ 7.2s: still held
    await expect(btn).toBeDisabled();

    await page.clock.runFor(3200); // t ≈ 10.4s: released
    await expect(btn).toHaveText('▷ 立即执行');
    await expect(btn).toBeEnabled();
    expect(mock.cronTriggerCalls.length, 'the held button must not have sent again').toBe(1);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a failed trigger releases the lock immediately', async ({ browser }) => {
  const { mock, ctx, btn, pageErrors } = await openDrawer(browser, { cronTrigger: { status: 502 } });
  try {
    await btn.click();
    await expect.poll(() => mock.cronTriggerCalls.length).toBe(1);
    // No clock advance: the release comes from the error path, not the timer.
    await expect(btn).toHaveText('▷ 立即执行');
    await expect(btn).toBeEnabled();
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a run_started frame replaces the optimistic lock with the running state', async ({ browser }) => {
  const { mock, ctx, page, btn, pageErrors } = await openDrawer(browser, { cronTrigger: {}, ws: true });
  try {
    await btn.click();
    await expect(btn).toHaveClass(/is-sending/);
    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
    const now = await page.evaluate(() => Date.now());
    mock.wsConnections[mock.wsConnections.length - 1].send({
      type: 'run_started', subsystem: 'cron', owner_id: JOB, run_id: 'run-trig-1', started_at: now, trigger: 'manual',
    });
    await expect(btn).toHaveText('▷ 运行中…');
    await expect(btn).toHaveClass(/is-running/);
    await expect(btn).not.toHaveClass(/is-sending/);

    // Running outranks the cooldown while it lasts, so what shows that the
    // frame cleared the lock is what follows: a run that ends inside the
    // 10s window leaves the button ready, not back on 已派发 ✓.
    const settled = job();
    mock.setCronJobs([settled]);
    mock.wsConnections[mock.wsConnections.length - 1].send({
      type: 'run_ended', subsystem: 'cron', owner_id: JOB, run_id: 'run-trig-1', state: 'succeeded',
      started_at: now, ended_at: now + 500, duration_ms: 500, trigger: 'manual',
    });
    await expect(btn).toHaveText('▷ 立即执行');
    await expect(btn).toBeEnabled();
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('立即运行 from the ⋯ menu twice inside the cooldown fires once', async ({ browser }) => {
  // The row menu's 立即运行 item is not disabled by the cooldown, so reopening
  // the menu and choosing it again inside the window reaches cronTriggerNow a
  // second time; its in-flight guard is what drops that call.
  const mock = await startMockServer({ cronJobs: [job()], cronTrigger: {} });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    const row = page.locator(`.cj-row[data-cron-id="${JOB}"]`);
    await expect(row).toBeVisible();
    await page.clock.pauseAt(Date.now() + 1000);
    for (let i = 0; i < 2; i++) {
      await row.locator('[data-action="cron-menu-toggle"]').click();
      await page.locator('.cj-menu-item[data-menu-action="run"]').click();
    }
    await expect.poll(() => mock.cronTriggerCalls.length).toBe(1);
    // Give a second request that did slip out time to land before re-counting.
    await page.waitForTimeout(300);
    expect(mock.cronTriggerCalls.length).toBe(1);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const [label, reducedMotion, animates] of /** @type {const} */ ([
  ['control: the sending spinner animates by default', undefined, true],
  ['reduced motion: the sending spinner does not animate', 'reduce', false],
])) {
  test(label, async ({ browser }) => {
    const { mock, ctx, btn } = await openDrawer(browser, { cronTrigger: {} }, { reducedMotion });
    try {
      await btn.click();
      await expect(btn).toHaveClass(/is-sending/);
      const anim = await btn.evaluate((el) => getComputedStyle(el, '::before').animationName);
      if (animates) expect(anim).not.toBe('none');
      else expect(anim).toBe('none');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
