// @ts-check
// R236-SEC-08 (#494) + follow-up: the cron list poll runs at 1 Hz, so it asks
// for ?compact=1 and gets prompts clipped to 256 bytes with prompt_truncated
// set. Anything that needs the whole prompt must re-fetch it first, and — the
// load-bearing half — if that re-fetch fails the editor must REFUSE to open,
// because Save on a 256-byte preview writes the truncation back to disk and
// destroys the user's prompt.
//
// This replaced internal/server/static_cron_compact_poll_test.go, whose eight
// checks were substrings of cron_view.js: `NZ_CONTRACT.API.cron + '?compact=1'`,
// `async function cronRefetchFullJob(`, `cronRefetchFullJob(id).then(`,
// `cronRefetchFullJob(jobId).then`, `return { ok: false, reason: 'fetch' }`,
// `return { ok: true, job: cached }`, `无法获取完整 prompt`, and
// `if (reason === 'fetch')` (#2547).
//
// Every one of those is a shape, and the thing that matters is an outcome: does
// the editor open, and with what in the textarea. A refactor that keeps all
// eight strings and drops the guard passes the anchor and loses data; a rename
// that keeps the guard fails it. So this drives the browser instead and watches
// the requests the page actually makes.
//
// 跑法：cd test/e2e && npx playwright test cron_compact_prompt.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const JOB = 'cron-001';
const LIMIT = 256;
// Long enough that the compact wire shape must clip it, and distinctive at both
// ends so a truncated body is unmistakable in an assertion.
const FULL_PROMPT = 'HEAD-' + 'x'.repeat(LIMIT * 2) + '-TAIL';

/** @param {Record<string, any>} extra */
function jobs(extra = {}) {
  return [Object.assign({
    id: JOB,
    schedule: '@every 1h',
    prompt: FULL_PROMPT,
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: Date.now() - 86400000,
    next_run: Date.now() + 3600000,
    stats: { total: 1, succeeded: 1, failed: 0, skipped: 0 },
  }, extra)];
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {Record<string, any>} overrides
 */
async function openCronPanel(browser, overrides = {}) {
  const mock = await startMockServer(Object.assign({
    cronJobs: jobs(),
    compactPromptLimit: LIMIT,
  }, overrides));
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const cronRequests = [];
  page.on('request', r => {
    const u = new URL(r.url());
    if (u.pathname === '/api/cron' && r.method() === 'GET') cronRequests.push(u.search);
  });
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector(`.cj-row[data-cron-id="${JOB}"]`);
  return { page, mock, cronRequests, cleanup };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'wire shape is viewport-independent; desktop-chrome only');
  }
});

test.describe('#494 cron compact poll 与完整 prompt 回取', () => {
  test('画面刷新用的 cron 列表请求都带 compact=1', async ({ browser }) => {
    const { cronRequests, cleanup } = await openCronPanel(browser);
    try {
      expect(cronRequests.length).toBeGreaterThan(0);
      // Every list GET made just to paint the panel must be the bounded shape.
      // A `compact=true` / missing param regresses to the full-prompt body per
      // job that this opt-in exists to stop. (fetchCronJobs is event-driven, not
      // on a timer — measured: zero /api/cron requests while a drawer is open —
      // so "the 1 Hz poll" in the retired anchor's comment was never accurate.)
      for (const search of cronRequests) {
        expect(search, 'a cron list poll went out without compact=1').toContain('compact=1');
      }
    } finally {
      await cleanup();
    }
  });

  test('编辑器打开前回取完整 prompt，textarea 里是全文而非 256 字节', async ({ browser }) => {
    const { page, mock, cleanup } = await openCronPanel(browser);
    try {
      expect(mock.fullCronListCalls, 'nothing should have asked for full prompts yet')
        .toHaveLength(0);

      await page.click(`.cj-row[data-cron-id="${JOB}"] [data-action="cron-edit"]`);
      await page.waitForSelector('.cron-modal #edit-cron-prompt');

      // A non-compact list GET happened: that is the refetch.
      expect(mock.fullCronListCalls.length).toBeGreaterThan(0);
      // And the editor holds the whole prompt. Checking the tail is what
      // separates "full" from "clipped": the head survives truncation.
      const value = await page.inputValue('.cron-modal #edit-cron-prompt');
      expect(value).toBe(FULL_PROMPT);
      expect(value.length).toBeGreaterThan(LIMIT);
    } finally {
      await cleanup();
    }
  });

  test('回取失败时编辑器拒绝打开并提示，Save 无从截断保存', async ({ browser }) => {
    const { page, cleanup } = await openCronPanel(browser, { fullCronListStatus: 503 });
    try {
      await page.click(`.cj-row[data-cron-id="${JOB}"] [data-action="cron-edit"]`);

      // The user is told why …
      await expect(page.locator('.toast')).toContainText('无法获取完整 prompt');
      // … and the modal never opened, so there is no Save button to press and
      // no 256-byte body to commit. This is the data-loss invariant.
      await expect(page.locator('.cron-modal')).toHaveCount(0);
      await expect(page.locator('#edit-cron-prompt')).toHaveCount(0);
    } finally {
      await cleanup();
    }
  });

  // Case 4 is why this file exists rather than just deleting the anchor. The
  // anchor asserted that openCronDetail calls cronRefetchFullJob "so the
  // drawer's 做什么 section shows the full prompt", and the call was there — but
  // a MutationObserver trace showed the full body living from 726 ms to 727 ms
  // and then reverting: entering the cron view kicks off a background
  // fetchCronJobs (compact), and when its response lands after the drawer's
  // re-fetch it replaces the whole cache and repaints the clipped copy. Nothing
  // re-fetches again, so the drawer stays clipped for as long as it is open.
  // keepRefetchedPrompts in cron_view.js fixes that.
  test('抽屉打开时回取，做什么一节显示完整 prompt', async ({ browser }) => {
    const { page, mock, cleanup } = await openCronPanel(browser);
    try {
      await page.click(`.cj-row[data-cron-id="${JOB}"]`);
      // The drawer paints from the compact cache first, so the section exists
      // immediately — with the clipped body.
      const body = page.locator('pre.css-prompt-body');
      await expect(body).toHaveCount(1);
      await expect(body).toContainText('HEAD-');

      // The refetch is fire-and-forget; wait for the request itself rather than
      // a timeout, otherwise this passes before it was ever made.
      await expect.poll(() => mock.fullCronListCalls.length, { timeout: 8000 })
        .toBeGreaterThan(0);
      // After it lands the section shows the tail, which the clipped copy does
      // not have. This is the repaint the drawer's refetch exists for.
      await expect(body).toContainText('-TAIL', { timeout: 8000 });

      // Still there a beat later. fetchCronJobs is event-driven, not periodic
      // (measured: zero /api/cron requests while a drawer is open), so this is
      // not a poll-storm assertion — it is the settled state after the view's
      // background refresh has landed.
      await page.waitForTimeout(1200);
      await expect(body).toContainText('-TAIL');
    } finally {
      await cleanup();
    }
  });

  test('抽屉合并过完整正文后，编辑器仍然重新回取', async ({ browser }) => {
    // The merge keeps prompt_truncated true on purpose. If it cleared the flag,
    // cronRefetchFullJob would early-return { ok: true, job: cached } and the
    // editor would open — and Save — from a body that may have changed since the
    // merge. That is exactly the data loss #494's follow-up guards against, so
    // the merge must not buy display convenience with a skipped re-fetch.
    // The compact response is delayed so no background refresh lands between the
    // drawer and the editor. Without that, keepRefetchedPrompts re-marks the row
    // prompt_truncated and the editor re-fetches for that reason instead — the
    // guard under test would go unexercised (measured: probe passes without it).
    const { page, mock, cleanup } = await openCronPanel(browser, { compactCronListDelayMs: 4000 });
    try {
      await page.click(`.cj-row[data-cron-id="${JOB}"]`);
      await expect(page.locator('pre.css-prompt-body')).toContainText('-TAIL', { timeout: 8000 });
      const afterDrawer = mock.fullCronListCalls.length;
      expect(afterDrawer).toBeGreaterThan(0);

      // The prompt is rewritten server-side; the cache still holds the old full
      // body merged in above.
      const edited = 'REWRITTEN-' + 'y'.repeat(LIMIT * 2) + '-NEWTAIL';
      mock.setCronPrompt(JOB, edited);

      await page.click(`.cj-row[data-cron-id="${JOB}"] [data-action="cron-edit"]`);
      await page.waitForSelector('.cron-modal #edit-cron-prompt');

      // A fresh non-compact fetch went out …
      expect(mock.fullCronListCalls.length).toBeGreaterThan(afterDrawer);
      // … and the editor holds the NEW body, not the merged stale one.
      const value = await page.inputValue('.cron-modal #edit-cron-prompt');
      expect(value).toBe(edited);
      expect(value).not.toContain('-TAIL');
    } finally {
      await cleanup();
    }
  });
});
