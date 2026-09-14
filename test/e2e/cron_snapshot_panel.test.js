// @ts-check
// cron-live RFC §7.3: the input-snapshot panel is the replay preview. It shows
// the model, image version, a truncated prompt hash, the secret REF names
// (§5.1 — the server never sends values) and the prompt itself, and it is
// absent for runs with no snapshot (local runs answer available:false).
//
// This replaced internal/server/static_cron_snapshot_panel_test.go, which
// grepped cron_view.js for ten literals (`function cronSnapshotPanelHtml(snap)`,
// `if (!snap || !snap.available) return '';`, `snap.secret_refs`, `+ snapshotPanel;`
// …) and dashboard.html for three CSS class names (#2547). Matching those
// literals says the code is shaped a certain way. It does not say the panel
// renders, that available:false really suppresses it, that the hash is
// truncated, or — the one that matters — that a hostile prompt is escaped
// rather than executed. The prompt is server-supplied text rendered into
// innerHTML, so its escaping is the panel's only security-relevant property,
// and the source anchor did not check it at all.
//
// 跑法：cd test/e2e && npx playwright test cron_snapshot_panel.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const JOB = 'cron-001';
const RUN_WITH_SNAPSHOT = 'run-bbbb2222';
const RUN_WITHOUT_SNAPSHOT = 'run-cccc3333';
const LONG_HASH = 'a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00';
// The prompt arrives from the server as plain text and is written into
// innerHTML, so a payload here must come back out as characters.
const HOSTILE_PROMPT = 'summarise <img src=x onerror="window.__snapXSS=1"> and <script>window.__snapXSS=2</script>';

/**
 * @param {import('@playwright/test').Browser} browser
 * @returns {Promise<{page: import('@playwright/test').Page, cleanup: () => Promise<void>}>}
 */
async function openCronDrawer(browser) {
  const mock = await startMockServer({
    runSnapshots: {
      [RUN_WITH_SNAPSHOT]: {
        available: true,
        model: 'us.anthropic.claude-fable-5-1',
        image_version: 'naozhi-sandbox:2026-09-01',
        prompt_hash: LONG_HASH,
        secret_refs: ['GITHUB_TOKEN', 'SLACK_APP_TOKEN'],
        prompt: HOSTILE_PROMPT,
      },
      // RUN_WITHOUT_SNAPSHOT is deliberately absent: the mock answers
      // {available:false}, which is what a local run really returns.
    },
  });
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row');
  await page.click(`.cj-row[data-cron-id="${JOB}"]`);
  await page.waitForSelector('#cron-timeline-panel .ctr');
  return { page, cleanup };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'panel content is viewport-independent; desktop-chrome only');
  }
});

test.describe('cron §7.3 输入快照面板', () => {
  test('展开有快照的 run 时渲染模型/镜像/截断哈希/密钥引用名', async ({ browser }) => {
    const { page, cleanup } = await openCronDrawer(browser);
    try {
      await page.click(`#cron-timeline-panel .ctr[data-run-id="${RUN_WITH_SNAPSHOT}"]`);
      const panel = page.locator(`.ctr[data-run-id="${RUN_WITH_SNAPSHOT}"] details.ctr-snapshot`);
      await expect(panel).toHaveCount(1);
      await expect(panel.locator('summary')).toHaveText('输入快照（可重放）');

      // <details> starts collapsed; open it so the body is rendered visible.
      await panel.locator('summary').click();
      const body = panel.locator('.ctr-snap-body');
      await expect(body).toContainText('us.anthropic.claude-fable-5-1');
      await expect(body).toContainText('naozhi-sandbox:2026-09-01');

      // The hash is truncated to 16 chars plus an ellipsis: a full 64-char
      // hash in the UI is the regression this row exists to prevent.
      await expect(body).toContainText(LONG_HASH.slice(0, 16) + '…');
      await expect(body).not.toContainText(LONG_HASH);

      // §5.1: refs render as NAMES, each its own <code>.
      const refs = body.locator('code');
      await expect(refs).toHaveCount(2);
      await expect(refs.nth(0)).toHaveText('GITHUB_TOKEN');
      await expect(refs.nth(1)).toHaveText('SLACK_APP_TOKEN');
    } finally {
      await cleanup();
    }
  });

  test('快照里的提示词按文本转义，不执行也不建元素', async ({ browser }) => {
    const { page, cleanup } = await openCronDrawer(browser);
    try {
      await page.click(`#cron-timeline-panel .ctr[data-run-id="${RUN_WITH_SNAPSHOT}"]`);
      const panel = page.locator(`.ctr[data-run-id="${RUN_WITH_SNAPSHOT}"] details.ctr-snapshot`);
      await panel.locator('summary').click();

      const pre = panel.locator('pre.ctr-snap-pre');
      await expect(pre).toHaveCount(1);
      // The payload is present as characters …
      await expect(pre).toHaveText(HOSTILE_PROMPT);
      // … and produced no elements and no execution.
      await expect(pre.locator('img')).toHaveCount(0);
      await expect(pre.locator('script')).toHaveCount(0);
      expect(await page.evaluate(() => (/** @type {any} */ (window)).__snapXSS)).toBeUndefined();
    } finally {
      await cleanup();
    }
  });

  test('没有快照的 run 展开后不渲染面板', async ({ browser }) => {
    const { page, cleanup } = await openCronDrawer(browser);
    try {
      // The snapshot fetch is fire-and-forget, so asserting absence right after
      // the click would pass before the response even arrived. Wait for the
      // response and the repaint it triggers first: without this the test is
      // green against a panel that renders on available:false.
      const snapResponse = page.waitForResponse(r =>
        r.url().includes(`/${RUN_WITHOUT_SNAPSHOT}/snapshot`));
      await page.click(`#cron-timeline-panel .ctr[data-run-id="${RUN_WITHOUT_SNAPSHOT}"]`);
      const res = await snapResponse;
      expect(await res.json()).toEqual({ available: false });
      // The detail body must arrive — otherwise "no panel" would be vacuous.
      await expect(page.locator(`.ctr[data-run-id="${RUN_WITHOUT_SNAPSHOT}"] .ctr-detail`)).toHaveCount(1);
      // renderCronTimelinePanel runs synchronously at the end of the fetch, but
      // give the repaint a beat so a panel that does render has landed.
      await page.waitForTimeout(200);
      await expect(page.locator(`.ctr[data-run-id="${RUN_WITHOUT_SNAPSHOT}"] details.ctr-snapshot`)).toHaveCount(0);
    } finally {
      await cleanup();
    }
  });
});
