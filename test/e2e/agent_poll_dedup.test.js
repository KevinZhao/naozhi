// @ts-check
// #2432 item 5: the agent-transcript HTTP poll fallback receives `Time >= after`
// — inclusive — so every page after a poll boundary replays the entries already
// rendered at the watermark millisecond. Transcript entries carry no uuid, so
// the dedup is by content key.
//
// Two things have to hold together, and only together:
//
//   1. the initial page seeds the watermark from what is on screen, so the
//      first poll tick (which replays that whole page) renders nothing;
//   2. a legitimate same-ms sibling — a second entry stamped with the same
//      millisecond, which a strict `>` cursor would lose — still renders.
//
// This replaced internal/server/static_agent_view_poll_dedup_test.go. That test
// was mostly sound: it extracted the dedupAgentPollBatch body between
// `@contract-begin` / `@contract-end` markers and executed the real function
// under node with a table of cases. What it could not do is check the two
// call sites, so it grepped for them instead — `var seed =
// dedupAgentPollBatch(events, 0, []);` must appear in fetchAgentEventsInitial,
// and startHttpPoll must not contain `state.pollAfterMS = 0` (#2547).
//
// Driving the real poll loop covers the function and both call sites at once,
// and needs no marker comments in production source.
//
// 跑法：cd test/e2e && npx playwright test agent_poll_dedup.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TASK_ID = 'task-dedup-1';
// The poll interval in agent_view.js's startHttpPoll.
const POLL_MS = 3000;

// Two entries share T_SAME: the same-ms sibling pair. Both are `text`, because
// the shared renderer drops `thinking` entries — a same-ms thinking/text pair
// would render as one bubble and the sibling assertion would be vacuous
// (measured: 2 bubbles, not 3).
const BASE = 1750000000000;
const T0 = BASE, T_SAME = BASE + 2000, T_LATER = BASE + 3000;

/**
 * Starts the HTTP-poll fallback the way production does: startHttpPoll is only
 * reached from onAgentSubscribeRejected('capacity'), so a mock with no WS never
 * polls at all. Without this the "nothing was replayed" assertion is vacuous —
 * measured: it passed while the poll had never run.
 * @param {import('@playwright/test').Page} page
 */
async function startPollFallback(page) {
  await page.evaluate((taskID) => {
    (/** @type {any} */ (window)).nz.views.agent.onAgentSubscribeRejected({
      task_id: taskID, reason: 'capacity',
    });
  }, TASK_ID);
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'dedup is viewport-independent; desktop-chrome only');
  }
});

test.describe('#2432 agent transcript poll dedup', () => {
  test('第一次 poll tick 重放整页时不重复渲染，同毫秒兄弟条目仍保留', async ({ browser }) => {
    const initial = [
      { time: T0, type: 'user', summary: 'go', detail: 'go' },
      { time: T_SAME, type: 'text', summary: 'answer-a', detail: 'answer-a' },
      { time: T_SAME, type: 'text', summary: 'answer-b', detail: 'answer-b' },
    ];
    const mock = await startMockServer({ agentEvents: { [TASK_ID]: initial } });
    const ctx = await browser.newContext({ ...desktop });
    try {
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
      await page.waitForSelector('#events-scroll');

      await page.evaluate((taskID) => {
        (/** @type {any} */ (window)).nz.views.agent.switchTo(taskID);
      }, TASK_ID);

      const rows = page.locator('#events-scroll > .event');
      // All three entries render, including BOTH same-ms siblings: a strict
      // cursor would have dropped one of them here already.
      await expect(rows).toHaveCount(3);
      await startPollFallback(page);
      const before = await rows.allInnerTexts();
      expect(before.join('\n')).toContain('answer-a');
      expect(before.join('\n')).toContain('answer-b');

      // Let one poll tick land. It re-requests with after=<seeded watermark>,
      // the mock answers inclusively (so all three come back), and nothing new
      // may appear on screen.
      await page.waitForTimeout(POLL_MS + 800);
      await expect(rows, 'the first poll tick replayed the page into the DOM')
        .toHaveCount(3);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('水位线之后的新条目照常渲染一次', async ({ browser }) => {
    // The mock's dataset is mutated between the initial fetch and a poll tick,
    // which is what a live agent does.
    const initial = [
      { time: T0, type: 'user', summary: 'go', detail: 'go' },
      { time: T_SAME, type: 'text', summary: 'answer-a', detail: 'answer-a' },
      { time: T_SAME, type: 'text', summary: 'answer-b', detail: 'answer-b' },
    ];
    const dataset = { [TASK_ID]: initial.slice() };
    const mock = await startMockServer({ agentEvents: dataset });
    const ctx = await browser.newContext({ ...desktop });
    try {
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
      await page.waitForSelector('#events-scroll');
      await page.evaluate((taskID) => {
        (/** @type {any} */ (window)).nz.views.agent.switchTo(taskID);
      }, TASK_ID);

      const rows = page.locator('#events-scroll > .event');
      await expect(rows).toHaveCount(3);
      await startPollFallback(page);

      // A newer entry appears server-side; the next tick must render exactly it.
      dataset[TASK_ID].push({ time: T_LATER, type: 'text', summary: 'newer', detail: 'newer' });
      await expect(rows).toHaveCount(4, { timeout: POLL_MS * 2 });
      expect((await rows.allInnerTexts()).join('\n')).toContain('newer');

      // And the tick after that must not render it twice.
      await page.waitForTimeout(POLL_MS + 800);
      await expect(rows).toHaveCount(4);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('同毫秒兄弟条目在后续页才到达时仍渲染', async ({ browser }) => {
    // The sharpest form of #2432 item 5, and the one a strict `>` cursor loses:
    // the first page carries one entry at T_SAME, and its sibling — same
    // millisecond, different content — only appears in a later poll page. A
    // cursor of `t <= afterMS` drops it forever; the content-key check keeps it.
    const initial = [
      { time: T0, type: 'user', summary: 'go', detail: 'go' },
      { time: T_SAME, type: 'text', summary: 'answer-a', detail: 'answer-a' },
    ];
    const dataset = { [TASK_ID]: initial.slice() };
    const mock = await startMockServer({ agentEvents: dataset });
    const ctx = await browser.newContext({ ...desktop });
    try {
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
      await page.waitForSelector('#events-scroll');
      await page.evaluate((taskID) => {
        (/** @type {any} */ (window)).nz.views.agent.switchTo(taskID);
      }, TASK_ID);

      const rows = page.locator('#events-scroll > .event');
      await expect(rows).toHaveCount(2);
      await startPollFallback(page);

      // The sibling lands at the watermark millisecond after the page was
      // already rendered and the watermark seeded from it.
      dataset[TASK_ID].push({ time: T_SAME, type: 'text', summary: 'answer-b', detail: 'answer-b' });
      await expect(rows).toHaveCount(3, { timeout: POLL_MS * 2 });
      expect((await rows.allInnerTexts()).join('\n')).toContain('answer-b');

      // Still exactly once on the following tick.
      await page.waitForTimeout(POLL_MS + 800);
      await expect(rows).toHaveCount(3);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});
